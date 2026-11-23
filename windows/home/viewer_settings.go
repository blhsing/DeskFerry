//go:build windows

package homewindows

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"deskferry/internal/screenview"
)

// viewerSettings are Screen Viewer choices remembered per destination.
type viewerSettings struct {
	MinFPS map[string]int `json:"min_fps,omitempty"`
}

func viewerSettingsPath() (string, error) {
	path, err := settingsPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "screen-viewer.json"), nil
}

func loadViewerSettings() viewerSettings {
	var settings viewerSettings
	path, err := viewerSettingsPath()
	if err != nil {
		return settings
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &settings)
	}
	return settings
}

// loadViewerMinFPS returns the destination's Min fps, or the default.
func loadViewerMinFPS(destination string) int {
	settings := loadViewerSettings()
	if value, ok := settings.MinFPS[strings.ToLower(destination)]; ok && value >= 0 && value <= screenview.MaxMinFPS {
		return value
	}
	return screenview.DefaultMinFPS
}

func saveViewerMinFPS(destination string, value int) error {
	if strings.TrimSpace(destination) == "" {
		return errors.New("no destination is selected")
	}
	path, err := viewerSettingsPath()
	if err != nil {
		return err
	}
	settings := loadViewerSettings()
	if settings.MinFPS == nil {
		settings.MinFPS = map[string]int{}
	}
	settings.MinFPS[strings.ToLower(destination)] = value
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
