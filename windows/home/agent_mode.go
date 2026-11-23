//go:build windows

package homewindows

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"deskferry/internal/buildinfo"
	"deskferry/internal/screenagent"
	"deskferry/internal/screenview"
	"deskferry/internal/tunnel"
)

// runScreenAgent serves the selected destination's screen to an AI agent over
// standard input and output, as an MCP server or as JSON lines.
func runScreenAgent(ctx context.Context, cfg config, mcp bool, minFPS int, opts screenagent.Options) error {
	if minFPS < 0 || minFPS > screenview.MaxMinFPS {
		return fmt.Errorf("-agent-min-fps must be from 0 to %d", screenview.MaxMinFPS)
	}
	if cfg.screenKey() == "" {
		return errors.New("the selected destination has no saved room password: save it in DeskFerry first")
	}
	hostname, _ := os.Hostname()
	connect := func(ctx context.Context) (*screenview.LiveClient, error) {
		conn, route, err := dialRelayService(ctx, cfg, tunnel.ServiceScreen)
		if err != nil {
			return nil, err
		}
		log.Printf("agent screen session relay=%s proxy=%s protocol=%s", route.RelayAddr, route.Proxy, route.Protocol)
		client, err := screenview.StartLive(ctx, conn, screenview.LiveClientOptions{ScreenKey: cfg.screenKey(), MinFPS: &minFPS, Client: "agent", Name: "AI agent on " + hostname})
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		return client, nil
	}
	agent := screenagent.New(connect, opts)
	defer agent.Close()
	log.Printf("screen agent ready destination=%q mode=%s", cfg.SelectedDestination, map[bool]string{true: "mcp", false: "json"}[mcp])
	if mcp {
		return screenagent.ServeMCP(ctx, agent, os.Stdin, os.Stdout, buildinfo.Version)
	}
	return screenagent.ServeJSONLines(ctx, agent, os.Stdin, os.Stdout)
}
