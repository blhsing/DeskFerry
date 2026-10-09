//go:build windows

package homewindows

import (
	"fmt"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func TestEnsureRDPDrawsWhenMinimizedSetsValueOnce(t *testing.T) {
	path := fmt.Sprintf(`Software\DeskFerryTest\rdp-minimized-%d`, time.Now().UnixNano())
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, path)
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\DeskFerryTest`)
	})

	changed, err := ensureRDPDrawsWhenMinimizedAt(registry.CURRENT_USER, path)
	if err != nil || !changed {
		t.Fatalf("first call: changed=%v err=%v, want a change", changed, err)
	}
	changed, err = ensureRDPDrawsWhenMinimizedAt(registry.CURRENT_USER, path)
	if err != nil || changed {
		t.Fatalf("second call: changed=%v err=%v, want no change", changed, err)
	}

	key, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	if value, _, err := key.GetIntegerValue(rdpSuppressWhenMinimized); err != nil || value != rdpKeepDrawingMinimized {
		t.Fatalf("value = %d, %v; want %d", value, err, rdpKeepDrawingMinimized)
	}
	// A user who chose a different value gets it corrected back to 2.
	if err := key.SetDWordValue(rdpSuppressWhenMinimized, 0); err != nil {
		t.Fatal(err)
	}
	if changed, err := ensureRDPDrawsWhenMinimizedAt(registry.CURRENT_USER, path); err != nil || !changed {
		t.Fatalf("after reset: changed=%v err=%v, want a change", changed, err)
	}
}
