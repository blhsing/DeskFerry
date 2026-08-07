//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	winrm "github.com/masterzen/winrm"

	"deskferry/internal/tunnel"
)

const macCredentialAccount = "DeskFerry Home"

type macWindowsCredential struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func macCredentialService(profile macProfile) string {
	room := strings.ToLower(strings.TrimSpace(profile.Room))
	if room == "" {
		room = "default"
	}
	return "DeskFerry/room/" + room
}

func saveMacWindowsCredential(profile macProfile, user, password string) error {
	user = strings.TrimSpace(user)
	if user == "" || password == "" {
		return errors.New("Windows username and password are required")
	}
	payload, err := json.Marshal(macWindowsCredential{User: user, Password: password})
	if err != nil {
		return err
	}
	output, err := exec.Command("security", "add-generic-password", "-U", "-a", macCredentialAccount, "-s", macCredentialService(profile), "-w", string(payload)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("save Windows login in macOS Keychain: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func readMacWindowsCredential(profile macProfile) (macWindowsCredential, error) {
	output, err := exec.Command("security", "find-generic-password", "-a", macCredentialAccount, "-s", macCredentialService(profile), "-w").Output()
	if err != nil {
		return macWindowsCredential{}, errors.New("no saved Windows login for this destination")
	}
	var credential macWindowsCredential
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &credential); err != nil {
		return macWindowsCredential{}, errors.New("the saved Windows login is invalid")
	}
	if strings.TrimSpace(credential.User) == "" || credential.Password == "" {
		return macWindowsCredential{}, errors.New("the saved Windows login is incomplete")
	}
	return credential, nil
}

func deleteMacWindowsCredential(profile macProfile) error {
	output, err := exec.Command("security", "delete-generic-password", "-a", macCredentialAccount, "-s", macCredentialService(profile)).CombinedOutput()
	if err != nil && !strings.Contains(strings.ToLower(string(output)), "could not be found") {
		return fmt.Errorf("delete Windows login from macOS Keychain: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func updateMacWindowsCredential(profile macProfile, password string, clear bool) error {
	if clear {
		return deleteMacWindowsCredential(profile)
	}
	if password != "" {
		return saveMacWindowsCredential(profile, profile.WindowsUser, password)
	}
	credential, err := readMacWindowsCredential(profile)
	if err != nil {
		return nil
	}
	if strings.TrimSpace(profile.WindowsUser) != "" && !strings.EqualFold(strings.TrimSpace(profile.WindowsUser), strings.TrimSpace(credential.User)) {
		return saveMacWindowsCredential(profile, profile.WindowsUser, credential.Password)
	}
	return nil
}

func executeMacWinRM(ctx context.Context, cfg config, profile macProfile, command string, output io.Writer) error {
	credential, err := readMacWindowsCredential(profile)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("open temporary local WinRM listener: %w", err)
	}
	listenerCtx, cancelListener := context.WithCancel(ctx)
	defer cancelListener()
	defer listener.Close()
	go func() {
		if err := serveMacWinRMListener(listenerCtx, cfg, listener); err != nil && listenerCtx.Err() == nil {
			log.Printf("temporary WinRM listener stopped: %v", err)
		}
	}()

	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return fmt.Errorf("parse temporary local WinRM listener: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return fmt.Errorf("parse temporary local WinRM port: %w", err)
	}
	timeout := 2 * time.Minute
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			return context.DeadlineExceeded
		}
	}
	endpoint := &winrm.Endpoint{Host: "127.0.0.1", Port: port, Timeout: timeout}
	client, err := winrm.NewClient(endpoint, credential.User, credential.Password)
	if err != nil {
		return fmt.Errorf("create WinRM client: %w", err)
	}
	started := time.Now()
	stdout, stderr, exitCode, err := client.RunPSWithContext(ctx, command)
	log.Printf("CLI WinRM command completed destination=%q success=%t exit_code=%d elapsed=%s", profile.Name, err == nil && exitCode == 0, exitCode, time.Since(started).Round(time.Millisecond))
	if stdout != "" {
		if _, writeErr := io.WriteString(output, stdout); writeErr != nil {
			return writeErr
		}
	}
	if stderr != "" {
		if stdout != "" && !strings.HasSuffix(stdout, "\n") {
			_, _ = io.WriteString(output, "\n")
		}
		if _, writeErr := io.WriteString(output, stderr); writeErr != nil {
			return writeErr
		}
	}
	if err != nil {
		return fmt.Errorf("execute WinRM command for destination %q: %w", profile.Name, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("WinRM command for destination %q exited with code %d", profile.Name, exitCode)
	}
	return nil
}

func serveMacWinRMListener(ctx context.Context, cfg config, listener net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		localConn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handleMacWinRMConn(ctx, cfg, localConn)
	}
}

func handleMacWinRMConn(ctx context.Context, cfg config, localConn net.Conn) {
	started := time.Now()
	relayConn, relayAddr, err := dialRelayService(ctx, cfg, tunnel.ServiceWinRM)
	if err != nil {
		log.Printf("WinRM session relay dial failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		_ = localConn.Close()
		return
	}
	log.Printf("WinRM session connected relay=%s", relayAddr)
	result := tunnel.PipeWithResult(localConn, relayConn)
	log.Printf("WinRM session relay=%s ended duration=%s local_to_relay_bytes=%d relay_to_local_bytes=%d local_error=%v relay_error=%v", relayAddr, result.Duration.Round(time.Millisecond), result.AToB.Bytes, result.BToA.Bytes, result.AToB.CopyErr, result.BToA.CopyErr)
}
