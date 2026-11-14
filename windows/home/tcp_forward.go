//go:build windows

package homewindows

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"deskferry/internal/tunnel"
)

// tcpForwardFlag collects repeatable -tcp-forward values.
type tcpForwardFlag []string

func (f *tcpForwardFlag) Set(value string) error {
	if _, err := tunnel.ParseTCPForwardListeners(value); err != nil {
		return err
	}
	*f = append(*f, value)
	return nil
}

func (f *tcpForwardFlag) String() string {
	return strings.Join(*f, ";")
}

// tcpForwardListeners returns the loopback listeners for the selected
// destination, or the command-line override when one was given.
func (c config) tcpForwardListeners() ([]tunnel.TCPForwardListener, error) {
	value := c.TCPForwards
	if strings.TrimSpace(value) == "" {
		if selected := c.selectedProfile(); selected != nil {
			value = selected.TCPForwards
		}
	}
	return tunnel.ParseTCPForwardListeners(value)
}

// tcpForwardAddress is the loopback address a forward listener binds. Forward
// listeners never accept connections from other computers.
func tcpForwardAddress(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

type openTCPForward struct {
	forward  tunnel.TCPForwardListener
	listener net.Listener
}

// openTCPForwards binds every configured forward listener. A listener that
// cannot bind is reported and skipped so RDP and the other forwards still
// work.
func openTCPForwards(cfg config, logf func(string, ...any)) []openTCPForward {
	forwards, err := cfg.tcpForwardListeners()
	if err != nil {
		logf("TCP forwards are not started: %v", err)
		return nil
	}
	if len(forwards) == 0 {
		return nil
	}
	if cfg.roomProof() == "" {
		logf("TCP forwards are not started: they require a room password.")
		return nil
	}
	opened := make([]openTCPForward, 0, len(forwards))
	for _, forward := range forwards {
		listener, err := net.Listen("tcp", tcpForwardAddress(forward.LocalPort))
		if err != nil {
			logf("TCP forward %s could not listen on %s: %v", forward, tcpForwardAddress(forward.LocalPort), err)
			continue
		}
		logf("TCP forward listener started on %s for Work forward %d.", listener.Addr(), forward.ForwardPort)
		opened = append(opened, openTCPForward{forward: forward, listener: listener})
	}
	return opened
}

func serveTCPForwards(ctx context.Context, cfg config, forwards []openTCPForward, logf func(string, ...any)) {
	for _, forward := range forwards {
		go func(forward openTCPForward) {
			err := serveTCPForwardListener(ctx, cfg, forward, logf)
			if err != nil && ctx.Err() == nil {
				logf("TCP forward listener %s stopped: %v", forward.listener.Addr(), err)
			}
		}(forward)
	}
}

func serveTCPForwardListener(ctx context.Context, cfg config, forward openTCPForward, logf func(string, ...any)) error {
	var sessions tunnel.ConnGroup
	defer sessions.Close()
	go func() {
		<-ctx.Done()
		_ = forward.listener.Close()
	}()
	service := forward.forward.Service()
	for {
		conn, err := forward.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		connCtx, release := sessions.Begin(ctx, conn)
		go func() {
			defer release()
			handleLocalServiceConn(connCtx, cfg, conn, service, logf)
		}()
	}
}

func closeTCPForwards(forwards []openTCPForward, logf func(string, ...any)) {
	for _, forward := range forwards {
		_ = forward.listener.Close()
		logf("TCP forward listener %s stopped.", forward.listener.Addr())
	}
}

// requiresServiceEcho reports whether a relay must confirm the requested
// service. Older relays map unknown services to RDP, which must never happen
// for screen viewing or a TCP forward.
func requiresServiceEcho(service string) bool {
	if service == tunnel.ServiceScreen {
		return true
	}
	_, forward := tunnel.ParseTCPForwardService(service)
	return forward
}

// UI: the TCP forwards card edits the selected destination's listeners.

func tcpForwardListenerRows(value string) []string {
	forwards, err := tunnel.ParseTCPForwardListeners(value)
	if err != nil {
		return nil
	}
	rows := make([]string, 0, len(forwards))
	for _, forward := range forwards {
		rows = append(rows, fmt.Sprintf("%s  →  Work forward %d", tcpForwardAddress(forward.LocalPort), forward.ForwardPort))
	}
	return rows
}

func (a *clientApp) selectedForwards() []tunnel.TCPForwardListener {
	if a.selectedDestination < 0 || a.selectedDestination >= len(a.destinations) {
		return nil
	}
	forwards, _ := tunnel.ParseTCPForwardListeners(a.destinations[a.selectedDestination].TCPForwards)
	return forwards
}

func (a *clientApp) setSelectedForwards(forwards []tunnel.TCPForwardListener, selectIndex int) {
	if a.selectedDestination < 0 || a.selectedDestination >= len(a.destinations) {
		return
	}
	a.destinations[a.selectedDestination].TCPForwards = tunnel.FormatTCPForwardListeners(forwards)
	a.refreshForwardList(selectIndex)
}

func (a *clientApp) refreshForwardList(selectIndex int) {
	if a.forwardList == nil {
		return
	}
	value := ""
	if a.selectedDestination >= 0 && a.selectedDestination < len(a.destinations) {
		value = a.destinations[a.selectedDestination].TCPForwards
	}
	_ = a.forwardList.SetModel(tcpForwardListenerRows(value))
	_ = a.forwardList.SetCurrentIndex(selectIndex)
	a.updateForwardButtons()
}

func (a *clientApp) forwardSelectionChanged() {
	forwards := a.selectedForwards()
	index := a.forwardList.CurrentIndex()
	if index >= 0 && index < len(forwards) {
		_ = a.forwardLocal.SetText(strconv.Itoa(forwards[index].LocalPort))
		_ = a.forwardRemote.SetText(strconv.Itoa(forwards[index].ForwardPort))
	}
	a.updateForwardButtons()
}

func (a *clientApp) updateForwardButtons() {
	if a.forwardAdd == nil || a.forwardList == nil || a.forwardLocal == nil || a.forwardRemote == nil {
		return
	}
	filled := strings.TrimSpace(a.forwardLocal.Text()) != "" && strings.TrimSpace(a.forwardRemote.Text()) != ""
	index := a.forwardList.CurrentIndex()
	selected := index >= 0 && index < len(a.selectedForwards())
	a.forwardAdd.SetEnabled(filled)
	a.forwardUpdate.SetEnabled(filled && selected)
	a.forwardDelete.SetEnabled(selected)
}

// forwardFromEditor validates the editor; ignored is the row being replaced,
// or -1.
func (a *clientApp) forwardFromEditor(ignored int) (tunnel.TCPForwardListener, error) {
	parsed, err := tunnel.ParseTCPForwardListeners(strings.TrimSpace(a.forwardLocal.Text()) + "=" + strings.TrimSpace(a.forwardRemote.Text()))
	if err != nil {
		return tunnel.TCPForwardListener{}, err
	}
	forward := parsed[0]
	for i, existing := range a.selectedForwards() {
		if i != ignored && existing.LocalPort == forward.LocalPort {
			return forward, fmt.Errorf("local port %d is already listed", forward.LocalPort)
		}
	}
	return forward, nil
}

func (a *clientApp) addForward() {
	forward, err := a.forwardFromEditor(-1)
	if err != nil {
		a.showError(err)
		return
	}
	next := append(a.selectedForwards(), forward)
	a.setSelectedForwards(next, len(next)-1)
	a.appendLog("Added TCP forward %s. Save settings or reconnect to start it.", forward)
}

func (a *clientApp) updateForward() {
	forwards := a.selectedForwards()
	index := a.forwardList.CurrentIndex()
	if index < 0 || index >= len(forwards) {
		return
	}
	forward, err := a.forwardFromEditor(index)
	if err != nil {
		a.showError(err)
		return
	}
	forwards[index] = forward
	a.setSelectedForwards(forwards, index)
}

func (a *clientApp) deleteForward() {
	forwards := a.selectedForwards()
	index := a.forwardList.CurrentIndex()
	if index < 0 || index >= len(forwards) {
		return
	}
	forwards = append(forwards[:index], forwards[index+1:]...)
	a.setSelectedForwards(forwards, min(index, len(forwards)-1))
	_ = a.forwardLocal.SetText("")
	_ = a.forwardRemote.SetText("")
}
