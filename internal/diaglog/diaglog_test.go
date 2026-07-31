package diaglog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDailyWriterRotatesBySize(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.Local)
	w, err := newDailyWriter(dir, "test", 7, 8, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.file.Close() }()
	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(w.path)
	previous, _ := os.ReadFile(w.path + ".old")
	if string(current) != "second\n" || string(previous) != "first\n" {
		t.Fatalf("current=%q previous=%q", current, previous)
	}
}

func TestDailyWriterPrunesExpiredLogsAndChangesDate(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "test-2026-07-24.log")
	keepPath := filepath.Join(dir, "test-2026-07-25.log")
	if err := os.WriteFile(oldPath, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keepPath, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.Local)
	w, err := newDailyWriter(dir, "test", 7, 1024, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.file.Close() }()
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expired log still exists: %v", err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("retained log missing: %v", err)
	}
	now = now.AddDate(0, 0, 1)
	if _, err := w.Write([]byte("next day\n")); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(w.path) != "test-2026-08-01.log" {
		t.Fatalf("path=%s", w.path)
	}
}
