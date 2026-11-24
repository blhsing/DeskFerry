//go:build windows

package workservice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestPreviousExecutablePathAvoidsALockedCopy reproduces an update after an
// earlier one whose previous copy is still open (as when the desktop app
// keeps running from it): the running executable must still be movable.
func TestPreviousExecutablePathAvoidsALockedCopy(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "DeskFerry.exe")
	for _, path := range []string{target, target + ".previous", target + ".previous-20260101-000000"} {
		if err := os.WriteFile(path, []byte("exe"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	name, _ := windows.UTF16PtrFromString(target + ".previous")
	// Open the way a running image is: readable, but neither deletable nor
	// replaceable.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.Local)
	previous := previousExecutablePath(target, now)
	if !strings.HasSuffix(previous, ".previous-20261124-100000") {
		t.Fatalf("previous = %s, want a unique name while .previous is locked", previous)
	}
	if _, err := os.Stat(target + ".previous-20260101-000000"); !os.IsNotExist(err) {
		t.Fatalf("an older unique copy was not removed: %v", err)
	}
	if err := os.Rename(target, previous); err != nil {
		t.Fatalf("moving the running executable aside failed: %v", err)
	}
	windows.CloseHandle(handle)

	if err := os.WriteFile(target, []byte("exe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := previousExecutablePath(target, now); got != target+".previous" {
		t.Fatalf("previous = %s, want the usual name once nothing holds it", got)
	}
	if _, err := os.Stat(target + ".previous"); !os.IsNotExist(err) {
		t.Fatal("the unlocked .previous copy was not removed")
	}
}
