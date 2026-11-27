//go:build windows

package sharebanner

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// The banner remembers on the Work PC whether the person there minimized it,
// so later sessions open as the compact pill. The screen helper normally runs
// as LocalSystem and keeps the choice in ProgramData next to its log; when it
// cannot write there, the choice is simply not remembered.

type bannerSettings struct {
	Minimized bool `json:"minimized"`
}

// settingsPath returns where the choice is kept; tests replace it.
var settingsPath = func() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "DeskFerry", "share-banner.json")
}

func loadMinimized() bool {
	data, err := os.ReadFile(settingsPath())
	if err != nil {
		return false
	}
	var settings bannerSettings
	return json.Unmarshal(data, &settings) == nil && settings.Minimized
}

func saveMinimized(minimized bool) {
	path := settingsPath()
	data, err := json.Marshal(bannerSettings{Minimized: minimized})
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	temp := path + ".tmp"
	if os.WriteFile(temp, data, 0o644) != nil {
		return
	}
	if os.Rename(temp, path) != nil {
		_ = os.Remove(temp)
	}
}
