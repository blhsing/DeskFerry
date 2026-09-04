//go:build windows

package main

import "testing"

func TestWorkServiceInvocation(t *testing.T) {
	for _, args := range [][]string{{"-service"}, {"-update-service", `C:\DeskFerry\DeskFerry.exe`}, {"-screen-capture-helper"}} {
		if !workServiceInvocation(args) {
			t.Fatalf("workServiceInvocation(%q) = false", args)
		}
	}
	if workServiceInvocation([]string{"-destination", "Work"}) {
		t.Fatal("Home destination invocation was classified as Work service")
	}
}
