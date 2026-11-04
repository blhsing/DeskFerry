//go:build windows

package homewindows

import (
	"bufio"
	"context"
	"os/exec"
	"testing"
	"time"
)

// A command waiting on an unresponsive worker (as during a relay dial to an
// offline Work agent) must not hold Close, which the UI thread calls.
func TestWinRMSessionCloseCancelsCommandInProgress(t *testing.T) {
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "Start-Sleep -Seconds 60")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	manager := newWinRMSessionManager(time.Minute)
	manager.cmd, manager.stdin, manager.stdout = cmd, stdin, bufio.NewReader(stdout)

	done := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), "Room", "user", "password", "hostname", "3391")
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)

	started := time.Now()
	manager.Close()
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("Close waited %s for the command in progress", elapsed)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Execute succeeded after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not return after Close")
	}
}
