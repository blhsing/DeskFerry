//go:build windows

package homewindows

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"
)

const (
	rdpDecodingPolicyKey   = `SOFTWARE\Policies\Microsoft\Windows NT\Terminal Services\Client`
	rdpDecodingPolicyValue = "EnableHardwareMode"
)

type rdpDecodingMode string

const (
	rdpDecodingAutomatic rdpDecodingMode = "automatic"
	rdpDecodingHardware  rdpDecodingMode = "hardware"
	rdpDecodingSoftware  rdpDecodingMode = "software"
)

var rdpDecodingModeLabels = []string{
	"Automatic (Windows default)",
	"Hardware decoding",
	"Software decoding (stability)",
}

type rdpGraphicsDiagnostics struct {
	SocketAbortCount int    `json:"socket_abort_count"`
	GPUName          string `json:"gpu_name"`
	DriverDateUTC    string `json:"driver_date_utc"`
}

type rdpDecodingRecommendation struct {
	Mode   rdpDecodingMode
	Reason string
}

func parseRDPDecodingMode(value string) (rdpDecodingMode, error) {
	switch rdpDecodingMode(strings.ToLower(strings.TrimSpace(value))) {
	case rdpDecodingAutomatic:
		return rdpDecodingAutomatic, nil
	case rdpDecodingHardware:
		return rdpDecodingHardware, nil
	case rdpDecodingSoftware:
		return rdpDecodingSoftware, nil
	default:
		return "", fmt.Errorf("invalid RDP decoding mode %q", value)
	}
}

func rdpDecodingModeIndex(mode rdpDecodingMode) int {
	switch mode {
	case rdpDecodingHardware:
		return 1
	case rdpDecodingSoftware:
		return 2
	default:
		return 0
	}
}

func rdpDecodingModeAt(index int) (rdpDecodingMode, error) {
	switch index {
	case 0:
		return rdpDecodingAutomatic, nil
	case 1:
		return rdpDecodingHardware, nil
	case 2:
		return rdpDecodingSoftware, nil
	default:
		return "", fmt.Errorf("invalid RDP decoding selection %d", index)
	}
}

func rdpDecodingModeDescription(mode rdpDecodingMode) string {
	switch mode {
	case rdpDecodingHardware:
		return "Hardware decoding is forced"
	case rdpDecodingSoftware:
		return "Software decoding is forced"
	default:
		return "Windows default (policy not configured)"
	}
}

func rdpDecodingModeTitle(mode rdpDecodingMode) string {
	switch mode {
	case rdpDecodingHardware:
		return "Hardware"
	case rdpDecodingSoftware:
		return "Software"
	default:
		return "Automatic"
	}
}

func readRDPDecodingMode() (rdpDecodingMode, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, rdpDecodingPolicyKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return rdpDecodingAutomatic, nil
	}
	if err != nil {
		return "", fmt.Errorf("open RDP client policy: %w", err)
	}
	defer key.Close()
	value, _, err := key.GetIntegerValue(rdpDecodingPolicyValue)
	if errors.Is(err, registry.ErrNotExist) {
		return rdpDecodingAutomatic, nil
	}
	if err != nil {
		return "", fmt.Errorf("read RDP decoding policy: %w", err)
	}
	switch value {
	case 0:
		return rdpDecodingSoftware, nil
	case 1:
		return rdpDecodingHardware, nil
	default:
		return "", fmt.Errorf("RDP decoding policy has unsupported value %d", value)
	}
}

func setRDPDecodingMode(mode rdpDecodingMode) error {
	if mode == rdpDecodingAutomatic {
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, rdpDecodingPolicyKey, registry.SET_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("open RDP client policy: %w", err)
		}
		defer key.Close()
		if err := key.DeleteValue(rdpDecodingPolicyValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("remove RDP decoding policy: %w", err)
		}
		return nil
	}
	value := uint32(1)
	if mode == rdpDecodingSoftware {
		value = 0
	} else if mode != rdpDecodingHardware {
		return fmt.Errorf("cannot apply invalid RDP decoding mode %q", mode)
	}
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, rdpDecodingPolicyKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create RDP client policy: %w", err)
	}
	defer key.Close()
	if err := key.SetDWordValue(rdpDecodingPolicyValue, value); err != nil {
		return fmt.Errorf("write RDP decoding policy: %w", err)
	}
	return nil
}

func requestRDPDecodingMode(mode rdpDecodingMode) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate DeskFerry executable: %w", err)
	}
	params := "-set-rdp-decoding-mode " + syscall.EscapeArg(string(mode))
	if err := shellExecute("runas", executable, params, ""); err != nil {
		return fmt.Errorf("request administrator approval: %w", err)
	}
	return nil
}

func collectRDPGraphicsDiagnostics() (rdpGraphicsDiagnostics, error) {
	const script = `$ErrorActionPreference='SilentlyContinue';` +
		`$cutoff=(Get-Date).AddDays(-7);` +
		`$events=@(Get-WinEvent -FilterHashtable @{LogName='Microsoft-Windows-TerminalServices-RDPClient/Operational';Id=1026;StartTime=$cutoff} | Where-Object {$_.Properties.Count -gt 1 -and [int]$_.Properties[1].Value -eq 2308});` +
		`$gpu=Get-CimInstance Win32_VideoController | Where-Object {$_.Name -notmatch 'Remote|Basic Display'} | Sort-Object DriverDate | Select-Object -First 1;` +
		`$driverDate=''; if($gpu -and $gpu.DriverDate){$driverDate=$gpu.DriverDate.ToUniversalTime().ToString('o')};` +
		`[pscustomobject]@{socket_abort_count=$events.Count;gpu_name=[string]$gpu.Name;driver_date_utc=$driverDate}|ConvertTo-Json -Compress`
	out, err := hiddenCommand("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script).Output()
	if err != nil {
		return rdpGraphicsDiagnostics{}, fmt.Errorf("query RDP graphics diagnostics: %w", err)
	}
	var diagnostics rdpGraphicsDiagnostics
	if err := json.Unmarshal(out, &diagnostics); err != nil {
		return diagnostics, fmt.Errorf("decode RDP graphics diagnostics: %w", err)
	}
	return diagnostics, nil
}

func recommendRDPDecoding(now time.Time, diagnostics rdpGraphicsDiagnostics) rdpDecodingRecommendation {
	driverDate, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(diagnostics.DriverDateUTC))
	oldDriver := err == nil && driverDate.Before(now.AddDate(-3, 0, 0))
	if diagnostics.SocketAbortCount >= 2 && oldDriver {
		name := strings.TrimSpace(diagnostics.GPUName)
		if name == "" {
			name = "display adapter"
		}
		return rdpDecodingRecommendation{
			Mode: rdpDecodingSoftware,
			Reason: fmt.Sprintf("%d RDP socket-abort disconnects in 7 days; %s driver is dated %s",
				diagnostics.SocketAbortCount, name, driverDate.Local().Format("2006-01-02")),
		}
	}
	if diagnostics.SocketAbortCount >= 2 {
		return rdpDecodingRecommendation{
			Mode:   rdpDecodingAutomatic,
			Reason: strconv.Itoa(diagnostics.SocketAbortCount) + " recent socket-abort disconnects, but no old display driver was detected",
		}
	}
	return rdpDecodingRecommendation{
		Mode:   rdpDecodingAutomatic,
		Reason: "no repeated RDP socket-abort and old-driver combination was detected",
	}
}

func (a *clientApp) applyRDPDecodingSelection() {
	mode, err := rdpDecodingModeAt(a.rdpDecodingMode.CurrentIndex())
	if err != nil {
		a.showError(err)
		return
	}
	current, readErr := readRDPDecodingMode()
	if readErr == nil && current == mode {
		a.appendLog("RDP graphics decoding is already set to %s.", mode)
		return
	}
	if err := requestRDPDecodingMode(mode); err != nil {
		a.showError(err)
		return
	}
	a.appendLog("Approve the elevation request to set RDP graphics decoding to %s.", mode)
	a.mu.Lock()
	active := a.activeLocal
	a.mu.Unlock()
	if active > 0 {
		a.appendLog("The %d active RDP connection(s) will not be interrupted; the decoding change applies after Remote Desktop is restarted.", active)
	}
	go a.waitForRDPDecodingMode(mode)
}

func (a *clientApp) waitForRDPDecodingMode(want rdpDecodingMode) {
	for range 30 {
		time.Sleep(500 * time.Millisecond)
		mode, err := readRDPDecodingMode()
		if err != nil || mode != want {
			continue
		}
		a.onUI(func() {
			_ = a.rdpDecodingMode.SetCurrentIndex(rdpDecodingModeIndex(mode))
		})
		a.refreshRDPDecodingRecommendation()
		a.appendLog("RDP graphics decoding policy applied: %s. Restart Remote Desktop when convenient; active sessions were left running.", mode)
		return
	}
	a.onUI(func() {
		mode, err := readRDPDecodingMode()
		if err == nil {
			_ = a.rdpDecodingMode.SetCurrentIndex(rdpDecodingModeIndex(mode))
		}
	})
	a.refreshRDPDecodingRecommendation()
	a.appendLog("RDP graphics decoding policy was not changed; administrator approval may have been canceled.")
}

func rdpDecodingAppliedText(mode rdpDecodingMode, err error) string {
	if err != nil {
		return "Applied mode unavailable: " + err.Error()
	}
	return "Applied: " + rdpDecodingModeDescription(mode)
}

func (a *clientApp) refreshRDPDecodingRecommendation() {
	go func() {
		mode, modeErr := readRDPDecodingMode()
		applied := rdpDecodingAppliedText(mode, modeErr)
		diagnostics, err := collectRDPGraphicsDiagnostics()
		if err != nil {
			a.onUI(func() {
				_ = a.rdpDecodingAdvice.SetText(applied + ". Recommendation unavailable: " + err.Error())
			})
			return
		}
		recommendation := recommendRDPDecoding(time.Now(), diagnostics)
		text := fmt.Sprintf("%s. Recommendation: %s — %s", applied, rdpDecodingModeTitle(recommendation.Mode), recommendation.Reason)
		a.onUI(func() {
			_ = a.rdpDecodingAdvice.SetText(text)
		})
	}()
}
