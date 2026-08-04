//go:build windows

package main

import "testing"

func TestValidSMBAlias(t *testing.T) {
	for _, value := range []string{"deskferry-work", "WORK10", "work-2"} {
		if !validSMBAlias(value) {
			t.Errorf("validSMBAlias(%q) = false", value)
		}
	}
	for _, value := range []string{"", "-work", "work-", `work\host`, "work host"} {
		if validSMBAlias(value) {
			t.Errorf("validSMBAlias(%q) = true", value)
		}
	}
}

func TestUniqueSMBAliasesPreservesUnrelatedValues(t *testing.T) {
	got := uniqueSMBAliases([]string{"files", "DeskFerry-Work", "FILES", "archive"})
	if len(got) != 3 || got[0] != "files" || got[1] != "DeskFerry-Work" || got[2] != "archive" {
		t.Fatalf("aliases = %#v", got)
	}
}
