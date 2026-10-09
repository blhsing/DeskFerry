//go:build windows

package homewindows

import (
	"fmt"
	"strings"

	"deskferry/internal/tunnel"
	"deskferry/windows/uikit"
)

// overallStatus summarises the tunnel and Work agent tiles for the header chip.
func overallStatus(tunnelText, workText string) (string, uikit.Tone) {
	running := strings.EqualFold(strings.TrimSpace(tunnelText), "Running")
	work := strings.ToLower(strings.TrimSpace(workText))
	switch {
	case work == "offline":
		return "Work agent offline", uikit.ToneDanger
	case work == "check relay":
		return "Check relay", uikit.ToneWarning
	case work == "checking":
		return "Checking", uikit.ToneWarning
	case running && work == "online":
		return "Connected", uikit.ToneSuccess
	case running:
		return "Running", uikit.ToneSuccess
	case work == "online":
		return "Ready to connect", uikit.ToneInfo
	default:
		return "Stopped", uikit.ToneNeutral
	}
}

func (a *clientApp) updateOverallStatus() {
	if a.overallStatus == nil || a.tunnelStatus == nil || a.workStatus == nil {
		return
	}
	text, tone := overallStatus(a.tunnelStatus.Text(), a.workStatus.Text())
	a.overallStatus.SetStatus(text, tone)
}

// rdpSessionText uses the shared session vocabulary.
func rdpSessionText(running bool, active int) string {
	if !running || active <= 0 {
		return "No active sessions"
	}
	return fmt.Sprintf("%d active", active)
}

// workTargetDetail describes the room and relay count of a destination.
func workTargetDetail(cfg config) string {
	relays := cfg.relayAddresses()
	room := tunnel.RelayRoomToken(cfg.primaryRelayAddress(), "")
	noun := "relays"
	if len(relays) == 1 {
		noun = "relay"
	}
	if room == "" {
		return fmt.Sprintf("%d %s", len(relays), noun)
	}
	return fmt.Sprintf("room %s, %d %s", room, len(relays), noun)
}

func (a *clientApp) updateWorkTarget(cfg config) {
	if a.workTarget != nil {
		name := strings.TrimSpace(cfg.SelectedDestination)
		if name == "" {
			name = "Work PC"
		}
		_ = a.workTarget.SetText(name + " · " + workTargetDetail(cfg))
		_ = a.workTarget.SetToolTipText(name + " · " + workTargetDetail(cfg))
	}
}
