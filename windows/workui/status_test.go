//go:build windows

package workconfigurator

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestServiceStatusTexts(t *testing.T) {
	text, chip := serviceStatusTexts(serviceInfo{Installed: true, State: windows.SERVICE_RUNNING, ProcessID: 42}, nil)
	if text != "Service running (pid 42)." || chip != "Running" {
		t.Fatalf("running = %q/%q", text, chip)
	}
	if _, chip := serviceStatusTexts(serviceInfo{}, nil); chip != "Not installed" {
		t.Fatalf("not installed chip = %q", chip)
	}
	if _, chip := serviceStatusTexts(serviceInfo{}, errors.New("access denied")); chip != "Error" {
		t.Fatalf("error chip = %q", chip)
	}
	if got := serviceChipText(windows.SERVICE_START_PENDING); got != "Starting" {
		t.Fatalf("start pending chip = %q", got)
	}
}
