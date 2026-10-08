//go:build windows

package uiproxy

import (
	"os"
	"testing"
)

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
