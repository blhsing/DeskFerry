package workservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"deskferry/internal/screenview"
	"deskferry/windows/sharebanner"
)

func runShareBanner(ctx context.Context, states <-chan screenview.SessionState, stopControl, stopSharing func()) {
	sharebanner.Run(ctx, states, sharebanner.Actions{StopControl: stopControl, StopSharing: stopSharing})
}

type helperConn struct {
	reader  *os.File
	writer  *os.File
	process windows.Handle
}

const screenResponseDrainTimeout = 30 * time.Second

const sessionReconnectTimeout = 15 * time.Second

const sessionDesktopReadyDelay = 500 * time.Millisecond

// screenRelayConn keeps the relay read side open after the capture helper has
// finished writing. The Home app closes its socket after consuming a single
// screenshot or an error frame. Closing the WebSocket immediately on helper
// EOF can cancel a just-written final frame before the relay forwards it.
type screenRelayConn struct {
	net.Conn
}

func (c *screenRelayConn) CloseWrite() error {
	return c.SetReadDeadline(time.Now().Add(screenResponseDrainTimeout))
}

func drainScreenResponse(conn net.Conn) net.Conn {
	return &screenRelayConn{Conn: conn}
}

// launchScreenCaptureHelper starts the capture helper in the active user's
// session and writes the preamble to it. The helper runs as LocalSystem when
// possible, so it can capture and control the lock screen, sign-in and UAC
// prompts; otherwise it runs as the signed-in user.
func launchScreenCaptureHelper(preamble screenview.HelperPreamble) (net.Conn, error) {
	userToken, sessionID, err := activeInteractiveUserToken()
	if err != nil {
		return nil, err
	}
	defer userToken.Close()
	token := userToken
	identity := "user"
	if systemToken, systemErr := systemTokenForSession(sessionID); systemErr == nil {
		defer systemToken.Close()
		token = systemToken
		identity = "LocalSystem"
	} else {
		log.Printf("screen capture helper will run as the signed-in user: %v", systemErr)
	}
	log.Printf("launching screen capture helper in interactive session=%d identity=%s control_allowed=%t", sessionID, identity, preamble.ControlAllowed)

	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	var childInput, parentInput windows.Handle
	if err := windows.CreatePipe(&childInput, &parentInput, &sa, 0); err != nil {
		return nil, err
	}
	defer func() {
		if childInput != 0 {
			windows.CloseHandle(childInput)
		}
	}()
	defer func() {
		if parentInput != 0 {
			windows.CloseHandle(parentInput)
		}
	}()
	if err := windows.SetHandleInformation(parentInput, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return nil, err
	}
	var parentOutput, childOutput windows.Handle
	if err := windows.CreatePipe(&parentOutput, &childOutput, &sa, 0); err != nil {
		return nil, err
	}
	defer func() {
		if childOutput != 0 {
			windows.CloseHandle(childOutput)
		}
	}()
	defer func() {
		if parentOutput != 0 {
			windows.CloseHandle(parentOutput)
		}
	}()
	if err := windows.SetHandleInformation(parentOutput, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return nil, err
	}
	stderrFile, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open screen helper stderr sink: %w", err)
	}
	defer stderrFile.Close()
	stderrHandle := windows.Handle(stderrFile.Fd())
	if err := windows.SetHandleInformation(stderrHandle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return nil, fmt.Errorf("make screen helper stderr sink inheritable: %w", err)
	}

	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	application, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return nil, err
	}
	commandLine, err := windows.UTF16PtrFromString(syscall.EscapeArg(executable) + " -screen-capture-helper")
	if err != nil {
		return nil, err
	}
	desktop, _ := windows.UTF16PtrFromString("winsta0\\default")
	startup := windows.StartupInfo{
		Cb:        uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Desktop:   desktop,
		Flags:     windows.STARTF_USESTDHANDLES,
		StdInput:  childInput,
		StdOutput: childOutput,
		StdErr:    stderrHandle,
	}
	var environment *uint16
	if err := windows.CreateEnvironmentBlock(&environment, userToken, false); err != nil {
		return nil, err
	}
	defer windows.DestroyEnvironmentBlock(environment)
	var process windows.ProcessInformation
	if err := windows.CreateProcessAsUser(token, application, commandLine, nil, nil, true,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW, environment, nil, &startup, &process); err != nil {
		return nil, err
	}
	windows.CloseHandle(process.Thread)
	windows.CloseHandle(childInput)
	childInput = 0
	windows.CloseHandle(childOutput)
	childOutput = 0
	reader := os.NewFile(uintptr(parentOutput), "DeskFerry screen helper output")
	writer := os.NewFile(uintptr(parentInput), "DeskFerry screen helper input")
	parentOutput = 0
	parentInput = 0
	conn := &helperConn{reader: reader, writer: writer, process: process.Process}
	line, err := json.Marshal(preamble)
	if err == nil {
		_, err = conn.Write(append(line, '\n'))
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send screen helper settings: %w", err)
	}
	return conn, nil
}

// systemTokenForSession returns a primary LocalSystem token moved into the
// given session. It fails unless this process runs as LocalSystem.
func systemTokenForSession(sessionID uint32) (windows.Token, error) {
	var current windows.Token
	access := uint32(windows.TOKEN_DUPLICATE | windows.TOKEN_QUERY | windows.TOKEN_ASSIGN_PRIMARY | windows.TOKEN_ADJUST_DEFAULT | windows.TOKEN_ADJUST_SESSIONID)
	if err := windows.OpenProcessToken(windows.CurrentProcess(), access, &current); err != nil {
		return 0, fmt.Errorf("open service token: %w", err)
	}
	defer current.Close()
	user, err := current.GetTokenUser()
	if err != nil {
		return 0, fmt.Errorf("query service token: %w", err)
	}
	if !user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return 0, errors.New("the Work agent is not running as LocalSystem")
	}
	var duplicate windows.Token
	if err := windows.DuplicateTokenEx(current, windows.MAXIMUM_ALLOWED, nil, windows.SecurityIdentification, windows.TokenPrimary, &duplicate); err != nil {
		return 0, fmt.Errorf("duplicate service token: %w", err)
	}
	if err := windows.SetTokenInformation(duplicate, windows.TokenSessionId, (*byte)(unsafe.Pointer(&sessionID)), uint32(unsafe.Sizeof(sessionID))); err != nil {
		duplicate.Close()
		return 0, fmt.Errorf("move service token to session %d: %w", sessionID, err)
	}
	return duplicate, nil
}

func activeInteractiveUserToken() (windows.Token, uint32, error) {
	consoleSessionID := windows.WTSGetActiveConsoleSessionId()
	var sessionInfo *windows.WTS_SESSION_INFO
	var sessionCount uint32
	enumerateErr := windows.WTSEnumerateSessions(0, 0, 1, &sessionInfo, &sessionCount)
	var activeCandidates []uint32
	var disconnectedCandidates []uint32
	if enumerateErr == nil {
		if sessionInfo != nil {
			defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessionInfo)))
		}
		sessions := unsafe.Slice(sessionInfo, int(sessionCount))
		activeCandidates = orderedActiveSessionIDs(consoleSessionID, sessions)
		disconnectedCandidates = orderedDisconnectedSessionIDs(consoleSessionID, sessions)
	} else if consoleSessionID != 0xffffffff {
		activeCandidates = []uint32{consoleSessionID}
	}
	if len(activeCandidates) == 0 && len(disconnectedCandidates) == 0 {
		if enumerateErr != nil {
			return 0, 0, fmt.Errorf("enumerate interactive Windows sessions: %w", enumerateErr)
		}
		return 0, 0, errors.New("no active interactive Windows session is available")
	}
	var failures []error
	for _, sessionID := range activeCandidates {
		var token windows.Token
		if err := windows.WTSQueryUserToken(sessionID, &token); err == nil {
			return token, sessionID, nil
		} else {
			failures = append(failures, fmt.Errorf("query user token for Windows session %d: %w", sessionID, err))
		}
	}
	for _, sessionID := range disconnectedCandidates {
		var token windows.Token
		if err := windows.WTSQueryUserToken(sessionID, &token); err != nil {
			failures = append(failures, fmt.Errorf("query disconnected user token for Windows session %d: %w", sessionID, err))
			continue
		}
		log.Printf("reattaching disconnected Windows session=%d to the console for screen capture", sessionID)
		if err := reconnectSessionToConsole(sessionID); err != nil {
			token.Close()
			failures = append(failures, fmt.Errorf("reattach Windows session %d to the console: %w", sessionID, err))
			continue
		}
		token.Close()
		freshToken, err := waitForActiveSessionToken(sessionID, sessionReconnectTimeout)
		if err != nil {
			failures = append(failures, fmt.Errorf("refresh user token after reattaching Windows session %d: %w", sessionID, err))
			continue
		}
		return freshToken, sessionID, nil
	}
	return 0, 0, errors.Join(failures...)
}

func waitForActiveSessionToken(sessionID uint32, timeout time.Duration) (windows.Token, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		state, found, err := windowsSessionState(sessionID)
		if err != nil {
			lastErr = err
		} else if found && state == windows.WTSActive {
			time.Sleep(sessionDesktopReadyDelay)
			var token windows.Token
			if err := windows.WTSQueryUserToken(sessionID, &token); err == nil {
				return token, nil
			} else {
				lastErr = err
			}
		} else if !found {
			lastErr = errors.New("session disappeared while reconnecting")
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = errors.New("session did not become active")
			}
			return 0, lastErr
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func windowsSessionState(sessionID uint32) (uint32, bool, error) {
	var sessionInfo *windows.WTS_SESSION_INFO
	var sessionCount uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessionInfo, &sessionCount); err != nil {
		return 0, false, err
	}
	if sessionInfo != nil {
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessionInfo)))
	}
	for _, session := range unsafe.Slice(sessionInfo, int(sessionCount)) {
		if session.SessionID == sessionID {
			return session.State, true, nil
		}
	}
	return 0, false, nil
}

func orderedActiveSessionIDs(consoleSessionID uint32, sessions []windows.WTS_SESSION_INFO) []uint32 {
	result := make([]uint32, 0, len(sessions))
	if consoleSessionID != 0xffffffff {
		for _, session := range sessions {
			if session.SessionID == consoleSessionID && session.State == windows.WTSActive {
				result = append(result, session.SessionID)
				break
			}
		}
	}
	for _, session := range sessions {
		if session.State == windows.WTSActive && session.SessionID != consoleSessionID {
			result = append(result, session.SessionID)
		}
	}
	return result
}

func orderedDisconnectedSessionIDs(consoleSessionID uint32, sessions []windows.WTS_SESSION_INFO) []uint32 {
	result := make([]uint32, 0, len(sessions))
	if consoleSessionID != 0xffffffff {
		for _, session := range sessions {
			if session.SessionID == consoleSessionID && session.State == windows.WTSDisconnected {
				result = append(result, session.SessionID)
				break
			}
		}
	}
	for _, session := range sessions {
		if session.State == windows.WTSDisconnected && session.SessionID != consoleSessionID {
			result = append(result, session.SessionID)
		}
	}
	return result
}

// reconnectSessionToConsole runs tscon as the Work service account. The normal
// LocalSystem service has permission to reconnect the existing user session,
// so DeskFerry does not need to store or receive the user's Windows password.
func reconnectSessionToConsole(sessionID uint32) error {
	systemRoot := strings.TrimSpace(os.Getenv("SystemRoot"))
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	executable := filepath.Join(systemRoot, "System32", "tscon.exe")
	ctx, cancel := context.WithTimeout(context.Background(), sessionReconnectTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, executable, strconv.FormatUint(uint64(sessionID), 10), "/dest:console")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("tscon did not finish within %s", sessionReconnectTimeout)
	}
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return fmt.Errorf("tscon failed: %w", err)
		}
		return fmt.Errorf("tscon failed: %w: %s", err, message)
	}
	return nil
}

func (c *helperConn) Read(buffer []byte) (int, error)  { return c.reader.Read(buffer) }
func (c *helperConn) Write(buffer []byte) (int, error) { return c.writer.Write(buffer) }
func (c *helperConn) LocalAddr() net.Addr              { return helperAddr("work-screen") }
func (c *helperConn) RemoteAddr() net.Addr             { return helperAddr("interactive-desktop") }
func (c *helperConn) SetDeadline(time.Time) error      { return nil }
func (c *helperConn) SetReadDeadline(time.Time) error  { return nil }
func (c *helperConn) SetWriteDeadline(time.Time) error { return nil }

func (c *helperConn) Close() error {
	if c.writer != nil {
		_ = c.writer.Close()
	}
	if c.reader != nil {
		_ = c.reader.Close()
	}
	if c.process != 0 {
		if event, _ := windows.WaitForSingleObject(c.process, 0); event == uint32(windows.WAIT_TIMEOUT) {
			_ = windows.TerminateProcess(c.process, 0)
		}
		_ = windows.CloseHandle(c.process)
		c.process = 0
	}
	return nil
}

type helperAddr string

func (a helperAddr) Network() string { return "pipe" }
func (a helperAddr) String() string  { return string(a) }
