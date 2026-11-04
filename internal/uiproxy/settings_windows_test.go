//go:build windows

package uiproxy

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWriteWaitsForReaderToReleaseSettings(t *testing.T) {
	path := t.TempDir() + `\home-client.json`
	if err := os.WriteFile(path, []byte(`{"proxy":"env"}`), 0600); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Write(path, []byte(`{"proxy":"direct"}`)) }()
	select {
	case err := <-done:
		windows.CloseHandle(handle)
		t.Fatalf("write finished while replacement was blocked: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	windows.CloseHandle(handle)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if proxy, found, err := Read(path); err != nil || !found || proxy != "direct" {
		t.Fatalf("saved settings = %q, %t, %v", proxy, found, err)
	}
}

func TestCurrentUsesSavedUIProxy(t *testing.T) {
	base := t.TempDir()
	t.Setenv("APPDATA", base)
	if got := Current("env"); got != "env" {
		t.Fatalf("missing settings proxy = %q", got)
	}
	if err := os.MkdirAll(base+`\DeskFerry`, 0700); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"http://192.9.200.22:9090", "direct", "env"} {
		if err := os.WriteFile(Path(base), []byte(`{"proxy":"`+value+`","destinations":[{"proxy":"http://old:3128"}]}`), 0600); err != nil {
			t.Fatal(err)
		}
		if got := Current("http://old:3128"); got != value {
			t.Fatalf("saved %q, got %q", value, got)
		}
	}
	if err := os.WriteFile(Path(base), []byte(`{"proxy":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(Path(base)); err == nil {
		t.Fatal("invalid settings accepted")
	}
}
