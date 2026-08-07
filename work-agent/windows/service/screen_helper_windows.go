package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type helperConn struct {
	reader  *os.File
	writer  *os.File
	process windows.Handle
}

const screenResponseDrainTimeout = 30 * time.Second

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

func launchScreenCaptureHelper() (net.Conn, error) {
	token, sessionID, err := activeInteractiveUserToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	log.Printf("launching screen capture helper in interactive session=%d", sessionID)

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
		StdErr:    childOutput,
	}
	var environment *uint16
	if err := windows.CreateEnvironmentBlock(&environment, token, false); err != nil {
		return nil, err
	}
	defer windows.DestroyEnvironmentBlock(environment)
	var process windows.ProcessInformation
	if err := windows.CreateProcessAsUser(token, application, commandLine, nil, nil, true,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NEW_CONSOLE, environment, nil, &startup, &process); err != nil {
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
	return &helperConn{reader: reader, writer: writer, process: process.Process}, nil
}

func activeInteractiveUserToken() (windows.Token, uint32, error) {
	consoleSessionID := windows.WTSGetActiveConsoleSessionId()
	var sessionInfo *windows.WTS_SESSION_INFO
	var sessionCount uint32
	enumerateErr := windows.WTSEnumerateSessions(0, 0, 1, &sessionInfo, &sessionCount)
	var candidates []uint32
	if enumerateErr == nil {
		if sessionInfo != nil {
			defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessionInfo)))
		}
		candidates = orderedActiveSessionIDs(consoleSessionID, unsafe.Slice(sessionInfo, int(sessionCount)))
	} else if consoleSessionID != 0xffffffff {
		candidates = []uint32{consoleSessionID}
	}
	if len(candidates) == 0 {
		if enumerateErr != nil {
			return 0, 0, fmt.Errorf("enumerate interactive Windows sessions: %w", enumerateErr)
		}
		return 0, 0, errors.New("no active interactive Windows session is available")
	}
	var failures []error
	for _, sessionID := range candidates {
		var token windows.Token
		if err := windows.WTSQueryUserToken(sessionID, &token); err == nil {
			return token, sessionID, nil
		} else {
			failures = append(failures, fmt.Errorf("query user token for Windows session %d: %w", sessionID, err))
		}
	}
	return 0, 0, errors.Join(failures...)
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
