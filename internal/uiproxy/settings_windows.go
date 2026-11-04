//go:build windows

// Package uiproxy reads the proxy selected in the merged Windows UI.
package uiproxy

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

func Path(appData string) string {
	return filepath.Join(appData, "DeskFerry", "home-client.json")
}

// Read returns found=false until the UI has saved a proxy setting. The service
// command line remains the fallback for installations without a UI profile.
func Read(path string) (proxy string, found bool, err error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return "", false, err
	}
	var settings struct {
		Proxy string `json:"proxy"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", false, err
	}
	proxy = strings.TrimSpace(settings.Proxy)
	return proxy, proxy != "", nil
}

func Current(fallback string) string {
	if proxy, found, err := Read(Path(os.Getenv("APPDATA"))); err == nil && found {
		return proxy
	}
	return fallback
}

// Write replaces a complete settings snapshot so a service notification never
// observes a partially written proxy (or the other saved UI settings).
func Write(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".home-client-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Windows readers and antivirus scanners can briefly deny replacement.
	// Keep the old snapshot intact and retry only transient sharing errors.
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := os.Rename(file.Name(), path)
		if err == nil {
			return nil
		}
		if (!errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED)) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
