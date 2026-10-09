//go:build windows

package homewindows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"deskferry/internal/tunnel"
)

var errWorkAgentOffline = errors.New("Work agent offline")

func checkWorkAgentOnline(ctx context.Context, cfg config) error {
	summary, err := queryRelaySummary(ctx, cfg)
	room := tunnel.RelayRoomToken(cfg.primaryRelayAddress(), "")
	if err != nil {
		return fmt.Errorf("Could not check the Work agent in room %q. Check your proxy and relay connection, then try again. Remote Desktop was not opened.\n\n%w", room, err)
	}
	if !summary.WorkOnline {
		return fmt.Errorf("The Work agent in room %q is not connected to any reachable relay. Check that DeskFerry Work services are running on that computer and can connect through its proxy. Remote Desktop was not opened.\n\n%w", room, errWorkAgentOffline)
	}
	return nil
}

// Keep the native window responsive while checking the destination, and gate
// both Connect and Open Remote Desktop on a fresh snapshot.
func (a *clientApp) startTunnelFromUI(openRDP bool) {
	if a.connectChecking {
		return
	}
	a.connectChecking = true
	cfg := a.currentConfig()
	a.connectButton.SetEnabled(false)
	_ = a.workStatus.SetText("Checking")
	a.appendLog("Checking Work agent before connecting to room %s.", tunnel.RelayRoomToken(cfg.primaryRelayAddress(), ""))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		err := checkWorkAgentOnline(ctx, cfg)
		cancel()
		a.onUI(func() {
			a.connectChecking = false
			a.connectButton.SetEnabled(true)
			a.mu.Lock()
			exiting := a.exiting
			a.mu.Unlock()
			if exiting {
				return
			}
			current := a.currentConfig()
			if current.Proxy != cfg.Proxy || current.SelectedDestination != cfg.SelectedDestination || current.roomProof() != cfg.roomProof() || !slicesEqualFold(current.relayAddresses(), cfg.relayAddresses()) {
				return
			}
			if err != nil {
				if errors.Is(err, errWorkAgentOffline) {
					_ = a.workStatus.SetText("Offline")
				} else {
					_ = a.workStatus.SetText("Check relay")
				}
				_ = a.details.SetText(err.Error())
				a.showError(err)
				a.appendLog("Connect failed: %v", err)
				return
			}
			_ = a.workStatus.SetText("Online")
			if a.isTunnelRunning() {
				if openRDP {
					err = launchMSTSC(cfg)
				}
			} else {
				err = a.startTunnel(openRDP)
			}
			if err != nil {
				a.showError(err)
				a.appendLog("Connect failed: %v", err)
			}
		})
	}()
}

type dashboardUpdate struct {
	relay   string
	summary relaySummary
	err     error
}

func followRelayDashboard(ctx context.Context, cfg config, updates chan<- dashboardUpdate) {
	_, room, err := relayStatusURL(cfg.RelayAddr)
	if err != nil {
		return
	}
	backoff := time.Second
	publish := func(summary relaySummary, err error) {
		select {
		case updates <- dashboardUpdate{cfg.RelayAddr, summary, err}:
		case <-ctx.Done():
		}
	}
	for ctx.Err() == nil {
		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := tunnel.DialMessageConn(dialCtx, cfg.RelayAddr, cfg.Proxy, tunnel.RoleDashboard, "")
		cancel()
		if err == nil {
			for {
				_, data, readErr := conn.Read(ctx)
				if readErr != nil {
					err = readErr
					break
				}
				var snapshot relaySnapshot
				if decodeErr := json.Unmarshal(data, &snapshot); decodeErr != nil {
					err = decodeErr
					break
				}
				publish(summarizeRelaySnapshot(snapshot, cfg.RelayAddr, room), nil)
				backoff = time.Second
			}
			tunnel.CloseMessageConn(conn)
		}
		if ctx.Err() != nil {
			return
		}
		publish(relaySummary{}, err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (a *clientApp) followRelayStatus(ctx context.Context, cfg config) {
	updates := make(chan dashboardUpdate, len(cfg.relayAddresses()))
	for _, relay := range cfg.relayAddresses() {
		go followRelayDashboard(ctx, cfg.withRelayAddress(relay), updates)
	}
	states := make(map[string]dashboardUpdate)
	for {
		select {
		case <-ctx.Done():
			return
		case update := <-updates:
			states[update.relay] = update
			combined := relaySummary{}
			available := 0
			for _, relay := range cfg.relayAddresses() {
				state, found := states[relay]
				if !found || state.err != nil {
					combined.RelayDetails = append(combined.RelayDetails, relay+": status unavailable")
					continue
				}
				available++
				s := state.summary
				combined.Room = s.Room
				combined.WorkOnline = combined.WorkOnline || s.WorkOnline
				combined.HomeOnline = combined.HomeOnline || s.HomeOnline
				combined.Waiting += s.Waiting
				combined.Active += s.Active
				combined.Total += s.Total
				combined.RelayDetails = append(combined.RelayDetails, fmt.Sprintf("%s: work %s, %d active streams", relay, onlineText(s.WorkOnline), s.Active))
				if s.CheckedAt.After(combined.CheckedAt) {
					combined.CheckedAt = s.CheckedAt
				}
			}
			a.onUI(func() {
				if ctx.Err() != nil {
					return
				}
				if available == 0 {
					_ = a.workStatus.SetText("Check relay")
					_ = a.details.SetText(strings.Join(combined.RelayDetails, "\r\n"))
					return
				}
				if combined.WorkOnline {
					_ = a.workStatus.SetText("Online")
				} else {
					_ = a.workStatus.SetText("Offline")
				}
				_ = a.details.SetText(formatRelayDetails(combined, cfg))
			})
		}
	}
}
