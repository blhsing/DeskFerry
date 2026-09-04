//go:build windows

package workconfigurator

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestWorkActivityFollowerLoadsTailAndFollowsAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "work-agent-2026-09-04.log")
	initial := "first\r\nsecond\nthird\n"
	if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}
	follower := newWorkActivityFollower(dir)
	lines, err := follower.initial(2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"second", "third"}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("initial lines = %#v, want %#v", lines, want)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("fourth\npart"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	lines, err = follower.poll()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"fourth"}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("first poll = %#v, want %#v", lines, want)
	}
	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("ial\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	lines, err = follower.poll()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"partial"}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("second poll = %#v, want %#v", lines, want)
	}
}

func TestWorkActivityFollowerSwitchesToNewLog(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "work-agent-2026-09-03.log")
	newPath := filepath.Join(dir, "work-agent-2026-09-04.log")
	if err := os.WriteFile(oldPath, []byte("old\n"), 0644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	follower := newWorkActivityFollower(dir)
	if _, err := follower.initial(10); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new\n"), 0644); err != nil {
		t.Fatal(err)
	}
	lines, err := follower.poll()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"new"}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("rotated lines = %#v, want %#v", lines, want)
	}
}

func TestFormatWorkActivityLine(t *testing.T) {
	got := formatWorkActivityLine("2026/09/04 00:46:02.779887 session ready service=winrm")
	if want := "00:46:02  session ready service=winrm"; got != want {
		t.Fatalf("formatted line = %q, want %q", got, want)
	}
	if got := formatWorkActivityLine("plain message"); got != "plain message" {
		t.Fatalf("plain line changed to %q", got)
	}
}
