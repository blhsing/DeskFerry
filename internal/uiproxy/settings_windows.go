//go:build windows

// Package uiproxy reads the proxy selected in the merged Windows UI.
package uiproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

func Path(appData string) string {
	return filepath.Join(appData, "DeskFerry", "home-client.json")
}

// Read returns found=false until the UI has saved a proxy setting. The service
// command line remains the fallback for installations without a UI profile.
func Read(path string) (proxy string, found bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
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
	return os.Rename(file.Name(), path)
}
