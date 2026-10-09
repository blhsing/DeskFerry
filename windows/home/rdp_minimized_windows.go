//go:build windows

package homewindows

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// Windows' Remote Desktop client tells the remote session to stop drawing its
// desktop while the client window is minimized. The Work PC then has no image
// to capture, so DeskFerry's screen viewer fails exactly when a user minimizes
// Remote Desktop to watch the screen viewer instead. A value of 2 keeps the
// remote session drawing. mstsc reads the value when a connection starts.
const (
	rdpClientSettingsKey     = `Software\Microsoft\Terminal Server Client`
	rdpSuppressWhenMinimized = "RemoteDesktop_SuppressWhenMinimized"
	rdpKeepDrawingMinimized  = 2
)

// ensureRDPDrawsWhenMinimized sets the current user's Remote Desktop client
// to keep the remote session drawing while minimized. It reports whether it
// changed the setting.
func ensureRDPDrawsWhenMinimized() (bool, error) {
	return ensureRDPDrawsWhenMinimizedAt(registry.CURRENT_USER, rdpClientSettingsKey)
}

func ensureRDPDrawsWhenMinimizedAt(root registry.Key, path string) (bool, error) {
	key, _, err := registry.CreateKey(root, path, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, fmt.Errorf("open Remote Desktop client settings: %w", err)
	}
	defer key.Close()
	current, _, err := key.GetIntegerValue(rdpSuppressWhenMinimized)
	if err == nil && current == rdpKeepDrawingMinimized {
		return false, nil
	}
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", rdpSuppressWhenMinimized, err)
	}
	if err := key.SetDWordValue(rdpSuppressWhenMinimized, rdpKeepDrawingMinimized); err != nil {
		return false, fmt.Errorf("set %s: %w", rdpSuppressWhenMinimized, err)
	}
	return true, nil
}
