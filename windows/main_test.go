//go:build windows

package main

import (
	"os"
	"strings"
	"testing"
)

func TestWorkServiceInvocation(t *testing.T) {
	for _, args := range [][]string{{"-service"}, {"-update-service", `C:\DeskFerry\DeskFerry.exe`}, {"-screen-capture-helper"}} {
		if !workServiceInvocation(args) {
			t.Fatalf("workServiceInvocation(%q) = false", args)
		}
	}
	if workServiceInvocation([]string{"-destination", "Work"}) {
		t.Fatal("Home destination invocation was classified as Work service")
	}
	if workServiceInvocation([]string{"-console"}) {
		t.Fatal("Home console invocation was classified as Work service")
	}
}

func TestConsumeWorkConsoleMode(t *testing.T) {
	saved := os.Args
	defer func() { os.Args = saved }()
	os.Args = []string{"DeskFerry.exe", "-work-console", "-relay-url", "https://relay/workdesk"}
	if !consumeMode(modeWorkConsole) {
		t.Fatal("-work-console was not recognized")
	}
	if got := strings.Join(os.Args[1:], " "); got != "-relay-url https://relay/workdesk" {
		t.Fatalf("remaining args = %q", got)
	}
	os.Args = []string{"DeskFerry.exe", "-console"}
	if consumeMode(modeWorkConsole) {
		t.Fatal("Home -console was taken as Work console mode")
	}
}
