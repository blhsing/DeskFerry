//go:build windows

package shortcut

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Directory is the shared Start menu folder used by DeskFerry installers.
func Directory() string {
	base := strings.TrimSpace(os.Getenv("ProgramData"))
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "Microsoft", "Windows", "Start Menu", "Programs", "DeskFerry")
}

// Create adds a shortcut to the installed executable.
func Create(name, installDir, args, description string) error {
	dir := Directory()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	target := filepath.Join(installDir, "DeskFerry.exe")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	script := "$ErrorActionPreference='Stop';$w=New-Object -ComObject WScript.Shell;$s=$w.CreateShortcut(" + quote(filepath.Join(dir, name)) + ");$s.TargetPath=" + quote(target) + ";$s.Arguments=" + quote(args) + ";$s.WorkingDirectory=" + quote(installDir) + ";$s.Description=" + quote(description) + ";$s.IconLocation=" + quote(target+",0") + ";$s.Save()"
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("create Start menu shortcut: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
