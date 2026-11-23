package screenagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"deskferry/internal/screenview"
)

type testSource struct {
	frame *image.RGBA
	once  sync.Once
}

func (s *testSource) Next(timeout time.Duration) (*image.RGBA, bool, error) {
	changed := false
	s.once.Do(func() { changed = true })
	if !changed {
		time.Sleep(min(timeout, 5*time.Millisecond))
	}
	return s.frame, changed, nil
}
func (s *testSource) Cursor() screenview.CursorState {
	return screenview.CursorState{X: 400, Y: 200, Visible: true}
}
func (s *testSource) Method() string { return "test" }
func (s *testSource) Close()         {}

type testHost struct {
	mu     sync.Mutex
	events []screenview.InputEvent
}

func (h *testHost) Inject(events []screenview.InputEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, events...)
	return nil
}
func (h *testHost) ReleaseAll()                                            {}
func (h *testHost) LastHumanInput() time.Time                              { return time.Time{} }
func (h *testHost) ClipboardGet() (string, error)                          { return "copied", nil }
func (h *testHost) ClipboardSet(string) error                              { return nil }
func (h *testHost) Desktop() (string, bool, string)                        { return "Default", false, "Editor" }
func (h *testHost) SAS() error                                             { return nil }
func (h *testHost) UIFind(screenview.Command) ([]screenview.UINode, error) { return nil, nil }
func (h *testHost) UITree(screenview.Command) ([]screenview.UINode, error) {
	return []screenview.UINode{{Name: "OK", Role: "Button", Enabled: true, Rect: [4]int{1000, 500, 200, 100}}}, nil
}

// newTestAgent serves a 3840x2160 screen so screenshots are scaled by 0.5.
func newTestAgent(t *testing.T) (*Agent, *testHost) {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, 3840, 2160))
	for i := range frame.Pix {
		frame.Pix[i] = 255
	}
	host := &testHost{}
	key := screenview.DeriveScreenKey("room", "secret")
	connect := func(ctx context.Context) (*screenview.LiveClient, error) {
		serverSide, clientSide := net.Pipe()
		go func() {
			defer serverSide.Close()
			reader := bufio.NewReader(serverSide)
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			request, err := screenview.ReadRequest(bytes.NewReader(line))
			if err != nil {
				return
			}
			_ = screenview.ServeLive(context.Background(), struct {
				io.Reader
				io.Writer
			}{reader, serverSide}, request, screenview.LiveServerOptions{ScreenKey: key, ControlAllowed: true, Source: &testSource{frame: frame}, Host: host, Logf: t.Logf})
		}()
		return screenview.StartLive(ctx, clientSide, screenview.LiveClientOptions{ScreenKey: key, Client: "agent", Name: "test"})
	}
	agent := New(connect, Options{SettleQuiet: 50 * time.Millisecond, SettleTimeout: 2 * time.Second})
	t.Cleanup(agent.Close)
	return agent, host
}

func TestAgentActionsUseScreenshotCoordinates(t *testing.T) {
	agent, host := newTestAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	shot, err := agent.Do(ctx, "screenshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(shot.PNG))
	if err != nil || decoded.Bounds().Dx() != 1920 || decoded.Bounds().Dy() != 1080 {
		t.Fatalf("screenshot = %v, %v", decoded.Bounds(), err)
	}
	if decoded.At(10, 10) != (color.RGBA{255, 255, 255, 255}) {
		t.Fatalf("screenshot pixel = %v", decoded.At(10, 10))
	}
	args := Args{"coordinate": json.RawMessage(`[100, 50]`), "text": json.RawMessage(`"ctrl"`)}
	if _, err := agent.Do(ctx, "left_click", args); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Do(ctx, "key", Args{"text": json.RawMessage(`"ctrl+s Return"`)}); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	events := append([]screenview.InputEvent(nil), host.events...)
	host.mu.Unlock()
	if len(events) < 4 || events[0].T != "key" || events[0].VK != 0x11 || !events[0].Down {
		t.Fatalf("click did not hold ctrl first: %+v", events)
	}
	x, y, _ := events[1].Point()
	if events[1].T != "move" || x != 200 || y != 100 {
		t.Fatalf("click moved to %d,%d; want native 200,100: %+v", x, y, events[1])
	}
	if len(events) != 5+6 {
		t.Fatalf("key events = %+v", events[5:])
	}
	tree, err := agent.Do(ctx, "ui_tree", nil)
	if err != nil || !strings.Contains(tree.Text, "center=[550, 275]") {
		t.Fatalf("ui_tree = %q, %v", tree.Text, err)
	}
}

func TestMCPServerListsAndCallsTools(t *testing.T) {
	agent, _ := newTestAgent(t)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"screenshot","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := ServeMCP(ctx, agent, strings.NewReader(input), &output, "test"); err != nil {
		t.Fatal(err)
	}
	responses := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatalf("bad response %q: %v", line, err)
		}
		id, _ := json.Marshal(message["id"])
		responses[string(id)] = message
	}
	if len(responses) != 4 {
		t.Fatalf("responses = %v", output.String())
	}
	tools := responses["2"]["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 20 {
		t.Fatalf("only %d tools", len(tools))
	}
	content := responses["3"]["result"].(map[string]any)["content"].([]any)
	image := content[0].(map[string]any)
	if image["type"] != "image" || image["mimeType"] != "image/png" {
		t.Fatalf("screenshot content = %v", content)
	}
	if _, err := base64.StdEncoding.DecodeString(image["data"].(string)); err != nil {
		t.Fatal(err)
	}
	if responses["4"]["error"] == nil {
		t.Fatal("unknown tool did not fail")
	}
}

func TestParseCombo(t *testing.T) {
	keys, err := parseCombo("ctrl+shift+Page_Down")
	if err != nil || len(keys) != 3 || keys[2].VK != 0x22 || !keys[2].Ext {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	if keys, err := parseCombo("ctrl++"); err != nil || len(keys) != 2 || keys[1].Char != "+" {
		t.Fatalf("ctrl++ = %+v, %v", keys, err)
	}
	if _, err := parseCombo("ctrl+bogus"); Code(err) != "bad_request" {
		t.Fatalf("bogus key = %v", err)
	}
}
