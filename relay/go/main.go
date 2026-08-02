package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

const (
	serviceName       = "DeskFerry.Relay"
	dashboardRole     = "dashboard"
	resumeRole        = "resume"
	startMessage      = "start"
	resumeMessage     = "resume"
	started           = "started"
	agentUnavailable  = "agent-unavailable"
	clientUnavailable = "client-unavailable"
)

var validRoles = map[string]bool{
	"agent":       true,
	"client":      true,
	"home-agent":  true,
	"probe":       true,
	resumeRole:    true,
	dashboardRole: true,
}

func main() {
	listen := flag.String("listen", envOrDefault("DESKFERRY_RELAY_LISTEN", "0.0.0.0:80"), "HTTP listen address")
	flag.Parse()

	srv := &http.Server{
		Addr:              *listen,
		Handler:           newServer(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("DeskFerry Go relay listening on %s", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func newServer() http.Handler {
	hub := newRelayHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/relay/", http.StatusFound)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/relay") {
			http.NotFound(w, r)
			return
		}
		handleRelay(w, r, hub)
	})
	return mux
}

func handleRelay(w http.ResponseWriter, r *http.Request, hub *RelayHub) {
	rest := strings.TrimPrefix(r.URL.Path, "/relay")
	switch {
	case rest == "" || rest == "/":
		writeHTML(w, dashboardHTML(""))
	case rest == "/health":
		writeJSON(w, map[string]any{
			"status":  "ok",
			"service": serviceName,
			"time":    time.Now().UTC(),
		})
	case rest == "/icon.svg":
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		_, _ = w.Write([]byte(iconSVG()))
	case rest == "/status":
		writeJSON(w, hub.Snapshot(r.URL.Query().Get("room")))
	case rest == "/ws":
		handleWebSocket(w, r, hub, "")
	default:
		room, isWS, ok := parseRoomPath(rest)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if isWS {
			handleWebSocket(w, r, hub, room)
			return
		}
		writeHTML(w, dashboardHTML(room))
	}
}

func parseRoomPath(rest string) (room string, ws bool, ok bool) {
	path := strings.Trim(rest, "/")
	if path == "" {
		return "", false, true
	}
	if strings.HasSuffix(path, "/ws") {
		room = strings.TrimSuffix(path, "/ws")
		return room, true, room != ""
	}
	if strings.Contains(path, "/") {
		return "", false, false
	}
	if path == "health" || path == "status" || path == "icon.svg" || path == "ws" {
		return "", false, false
	}
	return path, false, true
}

func writeHTML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request, hub *RelayHub, room string) {
	role := readRole(r)
	token := room
	if token == "" {
		if role == dashboardRole {
			token = dashboardRole
		} else {
			token = readToken(r)
		}
	}
	if role == "" || token == "" {
		c, err := acceptWebSocket(w, r)
		if err == nil {
			closeQuietly(c, websocket.StatusPolicyViolation, "missing relay role or bearer token")
		}
		return
	}

	c, err := acceptWebSocket(w, r)
	if err != nil {
		log.Printf("websocket accept failed remote=%s: %v", remoteAddr(r), err)
		return
	}

	remote := remoteAddr(r)
	ctx := r.Context()
	log.Printf("websocket connected role=%s room=%s remote=%s user_agent=%q", role, roomID(token), remote, r.UserAgent())
	switch role {
	case dashboardRole:
		hub.ServeDashboard(ctx, c, remote, room)
	case "agent":
		hub.ServeAgent(ctx, token, c, remote, readAgentIdentity(r), readResumable(r))
	case "client":
		hub.ServeClient(ctx, token, c, remote, readResumable(r))
	case resumeRole:
		hub.ServeResume(ctx, token, c, remote, r.Header.Get("X-DeskFerry-Session"), r.Header.Get("X-DeskFerry-Session-Side"))
	case "home-agent":
		hub.ServeHomeAgent(ctx, token, c, remote)
	case "probe":
		closeQuietly(c, websocket.StatusNormalClosure, "probe ok")
	default:
		closeQuietly(c, websocket.StatusPolicyViolation, "unsupported role")
	}
}

func acceptWebSocket(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
}

func readRole(r *http.Request) string {
	value := r.Header.Get("X-DeskFerry-Role")
	if value == "" {
		value = r.Header.Get("X-TunnelDesktop-Role")
	}
	if value == "" {
		value = r.URL.Query().Get("role")
	}
	role := strings.ToLower(strings.TrimSpace(value))
	if validRoles[role] {
		return role
	}
	return ""
}

func readToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		if token := strings.TrimSpace(auth[7:]); token != "" {
			return token
		}
	}
	if token := strings.TrimSpace(r.URL.Query().Get("token")); token != "" {
		return token
	}
	if room := strings.TrimSpace(r.URL.Query().Get("room")); room != "" {
		return room
	}
	return "default"
}

func remoteAddr(r *http.Request) string {
	forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if forwarded != "" {
		return strings.TrimSpace(strings.SplitN(forwarded, ",", 2)[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

func readAgentIdentity(r *http.Request) AgentIdentity {
	return AgentIdentity{
		Instance: cleanAgentIdentity(r.Header.Get("X-DeskFerry-Agent-Instance")),
		Slot:     cleanAgentIdentity(r.Header.Get("X-DeskFerry-Agent-Slot")),
	}
}

func readResumable(r *http.Request) bool {
	value := strings.TrimSpace(r.Header.Get("X-DeskFerry-Resumable"))
	return value == "1" || strings.EqualFold(value, "true")
}

func cleanAgentIdentity(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if b.Len() >= 64 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func roomID(token string) string {
	raw := strings.Trim(strings.TrimSpace(token), "/")
	if raw == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range raw {
		if b.Len() >= 64 {
			break
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			value := b.String()
			if value == "" || value[len(value)-1] != '-' {
				b.WriteByte('-')
			}
		}
	}
	room := strings.Trim(b.String(), "-.")
	if room == "" {
		return "default"
	}
	return room
}

type RelayHub struct {
	mu         sync.Mutex
	rooms      map[string]*RelayRoom
	dashboards map[string]*DashboardClient
	sessions   map[string]*ResumeSession
}

func newRelayHub() *RelayHub {
	return &RelayHub{
		rooms:      make(map[string]*RelayRoom),
		dashboards: make(map[string]*DashboardClient),
		sessions:   make(map[string]*ResumeSession),
	}
}

func (h *RelayHub) ServeAgent(ctx context.Context, token string, c *websocket.Conn, remote string, identity AgentIdentity, resumable bool) {
	room := h.roomFor(token)
	waiting, replaced := room.EnqueueAgent(c, remote, identity, resumable)
	log.Printf("agent waiting room=%s remote=%s key=%s replaced=%d", room.ID, remote, identity.LogString(), replaced)
	h.NotifyDashboards()

	var peer *HomePeer
	havePeer := false
	defer func() {
		if havePeer {
			peer.SetStarted(agentUnavailable)
		}
		waiting.Cancel()
		room.RemoveWaiting(waiting)
		h.NotifyDashboards()
	}()

	for {
		select {
		case peer = <-waiting.Paired:
			havePeer = true
			goto paired
		case <-waiting.Done:
			return
		case <-ctx.Done():
			return
		}
	}

paired:
	log.Printf("pairing room=%s agent=%s client=%s", room.ID, remote, peer.Remote)
	if waiting.Resumable && peer.Resumable {
		session := h.newResumeSession(room, remote, peer.Remote)
		if !sendControl(c, room.ID, remote, "agent", startMessage+" "+session.ID) {
			peer.SetStarted(agentUnavailable)
			session.Finish()
			return
		}
		if !sendControl(peer.Conn, room.ID, peer.Remote, "client", startMessage+" "+session.ID) {
			peer.SetStarted(clientUnavailable)
			peer.SetDone()
			session.Finish()
			return
		}
		peer.SetStarted(started)
		session.Run(c, peer.Conn, peer.Done, h.NotifyDashboards)
		return
	}
	if !sendStart(c, room.ID, remote, "agent") {
		peer.SetStarted(agentUnavailable)
		return
	}
	if !sendStart(peer.Conn, room.ID, peer.Remote, "client") {
		peer.SetStarted(clientUnavailable)
		peer.SetDone()
		return
	}
	peer.SetStarted(started)
	room.Bridge(ctx, c, peer.Conn, remote, peer.Remote, peer.Done, h.NotifyDashboards)
}

func (h *RelayHub) ServeClient(ctx context.Context, token string, c *websocket.Conn, remote string, resumable bool) {
	room := h.roomFor(token)
	for {
		waiting := room.TryTakeAgent()
		if waiting == nil {
			log.Printf("client rejected without agent room=%s remote=%s", room.ID, remote)
			closeQuietly(c, websocket.StatusTryAgainLater, "no work agent connected")
			return
		}

		peer := NewHomePeer(c, remote, resumable)
		if !waiting.TryPair(peer) {
			peer.SetDone()
			continue
		}
		h.NotifyDashboards()

		select {
		case result := <-peer.Started:
			switch result {
			case started:
				<-peer.Done
				return
			case clientUnavailable:
				return
			default:
				log.Printf("skipped unavailable work agent room=%s agent=%s client=%s", room.ID, waiting.Remote, remote)
			}
		case <-ctx.Done():
			peer.SetDone()
			return
		}
	}
}

func (h *RelayHub) ServeResume(ctx context.Context, token string, c *websocket.Conn, remote, sessionID, side string) {
	room := roomID(token)
	sessionID = cleanSessionValue(sessionID)
	key := room + "/" + sessionID
	h.mu.Lock()
	session := h.sessions[key]
	h.mu.Unlock()
	if session == nil || (side != "agent" && side != "client") {
		log.Printf("resume rejected room=%s session=%s side=%s remote=%s", room, sessionID, side, remote)
		closeQuietly(c, websocket.StatusPolicyViolation, "unknown resumable session")
		return
	}
	log.Printf("resume attachment waiting room=%s session=%s side=%s remote=%s", room, sessionID, side, remote)
	attached := session.Attach(ctx, side, c, remote)
	log.Printf("resume attachment released room=%s session=%s side=%s remote=%s attached=%t", room, sessionID, side, remote, attached)
	if !attached {
		closeQuietly(c, websocket.StatusTryAgainLater, "resumable session unavailable")
	}
}

func (h *RelayHub) newResumeSession(room *RelayRoom, agentRemote, clientRemote string) *ResumeSession {
	session := NewResumeSession(randomID(), room, agentRemote, clientRemote, func(s *ResumeSession) {
		h.mu.Lock()
		delete(h.sessions, room.ID+"/"+s.ID)
		h.mu.Unlock()
	})
	h.mu.Lock()
	h.sessions[room.ID+"/"+session.ID] = session
	h.mu.Unlock()
	return session
}

func (h *RelayHub) ServeHomeAgent(ctx context.Context, token string, c *websocket.Conn, remote string) {
	room := h.roomFor(token)
	started := time.Now()
	room.HomeAgentConnected(remote)
	log.Printf("home app connected room=%s remote=%s", room.ID, remote)
	h.NotifyDashboards()
	defer func() {
		room.HomeAgentDisconnected(remote)
		h.NotifyDashboards()
		log.Printf("home app disconnected room=%s remote=%s duration=%s", room.ID, remote, time.Since(started).Round(time.Millisecond))
		closeQuietly(c, websocket.StatusNormalClosure, "")
	}()
	err := drainUntilClose(ctx, c)
	log.Printf("home app receive ended room=%s remote=%s error=%v close_status=%d context_error=%v", room.ID, remote, err, websocket.CloseStatus(err), ctx.Err())
}

func (h *RelayHub) ServeDashboard(ctx context.Context, c *websocket.Conn, remote, room string) {
	client := &DashboardClient{ID: randomID(), Conn: c}
	if room != "" {
		selected := roomID(room)
		client.RoomID = &selected
	}
	h.mu.Lock()
	h.dashboards[client.ID] = client
	h.mu.Unlock()
	log.Printf("dashboard connected remote=%s", remote)
	defer func() {
		h.removeDashboard(client.ID)
		closeQuietly(c, websocket.StatusNormalClosure, "")
		log.Printf("dashboard disconnected remote=%s", remote)
	}()
	h.sendDashboard(client)
	drainUntilClose(ctx, c)
}

func (h *RelayHub) Snapshot(room string) StatusSnapshot {
	selected := ""
	if strings.TrimSpace(room) != "" {
		selected = roomID(room)
	}

	h.mu.Lock()
	rooms := make([]*RelayRoom, 0, len(h.rooms))
	if selected == "" {
		for _, room := range h.rooms {
			rooms = append(rooms, room)
		}
	} else if room := h.rooms[selected]; room != nil {
		rooms = append(rooms, room)
	}
	h.mu.Unlock()

	sort.Slice(rooms, func(i, j int) bool { return rooms[i].ID < rooms[j].ID })
	out := make([]RoomSnapshot, 0, len(rooms))
	for _, room := range rooms {
		out = append(out, room.Snapshot())
	}
	return StatusSnapshot{
		Service: serviceName,
		Time:    time.Now().UTC(),
		Rooms:   out,
	}
}

func (h *RelayHub) NotifyDashboards() {
	h.mu.Lock()
	clients := make([]*DashboardClient, 0, len(h.dashboards))
	for _, client := range h.dashboards {
		clients = append(clients, client)
	}
	h.mu.Unlock()
	for _, client := range clients {
		go h.sendDashboard(client)
	}
}

func (h *RelayHub) roomFor(token string) *RelayRoom {
	id := roomID(token)
	h.mu.Lock()
	defer h.mu.Unlock()
	if room := h.rooms[id]; room != nil {
		return room
	}
	room := NewRelayRoom(id)
	h.rooms[id] = room
	return room
}

func (h *RelayHub) removeDashboard(id string) {
	h.mu.Lock()
	delete(h.dashboards, id)
	h.mu.Unlock()
}

func (h *RelayHub) sendDashboard(client *DashboardClient) {
	client.SendMu.Lock()
	defer client.SendMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	room := ""
	if client.RoomID != nil {
		room = *client.RoomID
	}
	payload, err := json.Marshal(h.Snapshot(room))
	if err != nil {
		h.removeDashboard(client.ID)
		return
	}
	if err := client.Conn.Write(ctx, websocket.MessageText, payload); err != nil {
		h.removeDashboard(client.ID)
		closeQuietly(client.Conn, websocket.StatusNormalClosure, "")
	}
}

type RelayRoom struct {
	ID string

	mu                       sync.Mutex
	agents                   []*WaitingAgent
	activePairs              int
	totalPairs               int64
	lastAgentRemote          *string
	lastAgentConnectedAt     *time.Time
	lastAgentDisconnectedAt  *time.Time
	homeAgentRemote          *string
	homeAgentConnectedAt     *time.Time
	homeAgentDisconnectedAt  *time.Time
	lastClientRemote         *string
	lastClientConnectedAt    *time.Time
	lastClientDisconnectedAt *time.Time
}

func NewRelayRoom(id string) *RelayRoom {
	return &RelayRoom{ID: id}
}

func (r *RelayRoom) EnqueueAgent(c *websocket.Conn, remote string, identity AgentIdentity, resumable bool) (*WaitingAgent, int) {
	waiting := NewWaitingAgent(c, remote, identity, resumable)
	now := time.Now().UTC()
	remoteCopy := remote
	r.mu.Lock()
	r.pruneClosedAgentsLocked()
	replaced := r.replaceAgentLocked(identity)
	r.agents = append(r.agents, waiting)
	r.lastAgentRemote = &remoteCopy
	r.lastAgentConnectedAt = &now
	r.mu.Unlock()
	for _, agent := range replaced {
		closeQuietly(agent.Conn, websocket.StatusNormalClosure, "replaced by newer agent socket")
	}
	return waiting, len(replaced)
}

func (r *RelayRoom) TryTakeAgent() *WaitingAgent {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneClosedAgentsLocked()
	for len(r.agents) > 0 {
		waiting := r.agents[0]
		r.agents = r.agents[1:]
		if waiting.IsOpen() {
			return waiting
		}
	}
	return nil
}

func (r *RelayRoom) RemoveWaiting(waiting *WaitingAgent) {
	now := time.Now().UTC()
	r.mu.Lock()
	kept := r.agents[:0]
	for _, agent := range r.agents {
		if agent != waiting {
			kept = append(kept, agent)
		}
	}
	r.agents = kept
	r.lastAgentDisconnectedAt = &now
	r.mu.Unlock()
}

func (r *RelayRoom) HomeAgentConnected(remote string) {
	now := time.Now().UTC()
	remoteCopy := remote
	r.mu.Lock()
	r.homeAgentRemote = &remoteCopy
	r.homeAgentConnectedAt = &now
	r.mu.Unlock()
}

func (r *RelayRoom) HomeAgentDisconnected(remote string) {
	now := time.Now().UTC()
	r.mu.Lock()
	if r.homeAgentRemote != nil && *r.homeAgentRemote == remote {
		r.homeAgentRemote = nil
		r.homeAgentConnectedAt = nil
		r.homeAgentDisconnectedAt = &now
	}
	r.mu.Unlock()
}

func (r *RelayRoom) Bridge(ctx context.Context, agent, client *websocket.Conn, agentRemote, clientRemote string, clientDone chan struct{}, stateChanged func()) {
	started := time.Now()
	pairID := r.PairStarted(clientRemote)
	stateChanged()

	bridgeCtx, cancel := context.WithCancel(ctx)
	done := make(chan pumpResult, 2)
	go pumpBinary(bridgeCtx, agent, client, "agent_to_client", done)
	go pumpBinary(bridgeCtx, client, agent, "client_to_agent", done)
	first := <-done
	cancel()
	second := <-done

	r.PairEnded()
	closeQuietly(agent, websocket.StatusNormalClosure, "")
	closeQuietly(client, websocket.StatusNormalClosure, "")
	closeOnce(clientDone)
	stateChanged()
	log.Printf("bridge closed room=%s pair=%d agent=%s client=%s duration=%s trigger_direction=%s trigger_bytes=%d trigger_messages=%d trigger_error=%v trigger_close_status=%d other_direction=%s other_bytes=%d other_messages=%d other_error=%v other_close_status=%d context_error=%v", r.ID, pairID, agentRemote, clientRemote, time.Since(started).Round(time.Millisecond), first.Direction, first.Bytes, first.Messages, first.Err, websocket.CloseStatus(first.Err), second.Direction, second.Bytes, second.Messages, second.Err, websocket.CloseStatus(second.Err), ctx.Err())
}

func (r *RelayRoom) PairStarted(clientRemote string) int64 {
	now := time.Now().UTC()
	clientRemoteCopy := clientRemote
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activePairs++
	r.totalPairs++
	r.lastClientRemote = &clientRemoteCopy
	r.lastClientConnectedAt = &now
	r.lastClientDisconnectedAt = nil
	return r.totalPairs
}

func (r *RelayRoom) PairEnded() {
	now := time.Now().UTC()
	r.mu.Lock()
	if r.activePairs > 0 {
		r.activePairs--
	}
	r.lastAgentDisconnectedAt = &now
	r.lastClientDisconnectedAt = &now
	r.mu.Unlock()
}

func (r *RelayRoom) Snapshot() RoomSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneClosedAgentsLocked()
	return RoomSnapshot{
		ID:                       r.ID,
		WaitingAgents:            len(r.agents),
		ActivePairs:              r.activePairs,
		TotalPairs:               r.totalPairs,
		LastAgentRemote:          r.lastAgentRemote,
		LastAgentConnectedAt:     r.lastAgentConnectedAt,
		LastAgentDisconnectedAt:  r.lastAgentDisconnectedAt,
		HomeAgentConnected:       r.homeAgentRemote != nil,
		HomeAgentRemote:          r.homeAgentRemote,
		HomeAgentConnectedAt:     r.homeAgentConnectedAt,
		HomeAgentDisconnectedAt:  r.homeAgentDisconnectedAt,
		LastClientRemote:         r.lastClientRemote,
		LastClientConnectedAt:    r.lastClientConnectedAt,
		LastClientDisconnectedAt: r.lastClientDisconnectedAt,
	}
}

func (r *RelayRoom) pruneClosedAgentsLocked() {
	kept := r.agents[:0]
	for _, agent := range r.agents {
		if agent.IsOpen() {
			kept = append(kept, agent)
		}
	}
	r.agents = kept
}

func (r *RelayRoom) replaceAgentLocked(identity AgentIdentity) []*WaitingAgent {
	if !identity.Valid() {
		return nil
	}
	replaced := make([]*WaitingAgent, 0, 1)
	kept := r.agents[:0]
	for _, agent := range r.agents {
		if agent.Identity.Equal(identity) {
			agent.Cancel()
			replaced = append(replaced, agent)
			continue
		}
		kept = append(kept, agent)
	}
	r.agents = kept
	return replaced
}

type AgentIdentity struct {
	Instance string
	Slot     string
}

func (i AgentIdentity) Valid() bool {
	return i.Instance != "" && i.Slot != ""
}

func (i AgentIdentity) Equal(other AgentIdentity) bool {
	return i.Instance == other.Instance && i.Slot == other.Slot && i.Valid()
}

func (i AgentIdentity) LogString() string {
	if !i.Valid() {
		return "legacy"
	}
	return i.Instance + "/" + i.Slot
}

type WaitingAgent struct {
	Conn      *websocket.Conn
	Remote    string
	Identity  AgentIdentity
	Resumable bool
	Paired    chan *HomePeer
	Done      chan struct{}

	closed atomic.Bool
	paired atomic.Bool
	once   sync.Once
}

func NewWaitingAgent(c *websocket.Conn, remote string, identity AgentIdentity, resumable bool) *WaitingAgent {
	return &WaitingAgent{
		Conn:      c,
		Remote:    remote,
		Identity:  identity,
		Resumable: resumable,
		Paired:    make(chan *HomePeer, 1),
		Done:      make(chan struct{}),
	}
}

func (w *WaitingAgent) IsOpen() bool {
	return !w.closed.Load() && !w.paired.Load()
}

func (w *WaitingAgent) TryPair(peer *HomePeer) bool {
	if w.closed.Load() || !w.paired.CompareAndSwap(false, true) {
		return false
	}
	w.Paired <- peer
	return true
}

func (w *WaitingAgent) Cancel() {
	if w.closed.CompareAndSwap(false, true) {
		w.once.Do(func() { close(w.Done) })
	}
}

type HomePeer struct {
	Conn      *websocket.Conn
	Remote    string
	Resumable bool
	Done      chan struct{}
	Started   chan string

	doneOnce    sync.Once
	startedOnce sync.Once
}

func NewHomePeer(c *websocket.Conn, remote string, resumable bool) *HomePeer {
	return &HomePeer{
		Conn:      c,
		Remote:    remote,
		Resumable: resumable,
		Done:      make(chan struct{}),
		Started:   make(chan string, 1),
	}
}

func (p *HomePeer) SetDone() {
	p.doneOnce.Do(func() { close(p.Done) })
}

func (p *HomePeer) SetStarted(value string) {
	p.startedOnce.Do(func() { p.Started <- value })
}

type ResumeAttachment struct {
	Conn   *websocket.Conn
	Remote string
	Done   chan struct{}
}

type ResumeSession struct {
	ID           string
	Room         *RelayRoom
	AgentRemote  string
	ClientRemote string

	agent    chan *ResumeAttachment
	client   chan *ResumeAttachment
	done     chan struct{}
	onFinish func(*ResumeSession)
	once     sync.Once
}

func NewResumeSession(id string, room *RelayRoom, agentRemote, clientRemote string, onFinish func(*ResumeSession)) *ResumeSession {
	return &ResumeSession{
		ID:           id,
		Room:         room,
		AgentRemote:  agentRemote,
		ClientRemote: clientRemote,
		agent:        make(chan *ResumeAttachment, 2),
		client:       make(chan *ResumeAttachment, 2),
		done:         make(chan struct{}),
		onFinish:     onFinish,
	}
}

func (s *ResumeSession) Attach(ctx context.Context, side string, conn *websocket.Conn, remote string) bool {
	attachment := &ResumeAttachment{Conn: conn, Remote: remote, Done: make(chan struct{})}
	queue := s.client
	if side == "agent" {
		queue = s.agent
	}
	select {
	case queue <- attachment:
	case <-s.done:
		return false
	case <-ctx.Done():
		return false
	}
	select {
	case <-attachment.Done:
		return true
	case <-s.done:
		return true
	}
}

func (s *ResumeSession) Run(agent, client *websocket.Conn, clientDone chan struct{}, stateChanged func()) {
	startedAt := time.Now()
	pairID := s.Room.PairStarted(s.ClientRemote)
	stateChanged()
	defer func() {
		s.Room.PairEnded()
		closeOnce(clientDone)
		s.Finish()
		stateChanged()
		log.Printf("resumable bridge closed room=%s pair=%d session=%s agent=%s client=%s duration=%s", s.Room.ID, pairID, s.ID, s.AgentRemote, s.ClientRemote, time.Since(startedAt).Round(time.Millisecond))
	}()

	var agentAttachment, clientAttachment *ResumeAttachment
	for {
		first, second := bridgeSockets(agent, client)
		if websocket.CloseStatus(first.Err) == websocket.StatusNormalClosure {
			closeQuietly(agent, websocket.StatusNormalClosure, "session closed")
			closeQuietly(client, websocket.StatusNormalClosure, "session closed")
			if agentAttachment != nil {
				closeOnce(agentAttachment.Done)
			}
			if clientAttachment != nil {
				closeOnce(clientAttachment.Done)
			}
			return
		}
		log.Printf("resumable bridge interrupted room=%s pair=%d session=%s trigger_direction=%s trigger_error=%v other_direction=%s other_error=%v", s.Room.ID, pairID, s.ID, first.Direction, first.Err, second.Direction, second.Err)
		abortQuietly(agent)
		abortQuietly(client)
		if agentAttachment != nil {
			closeOnce(agentAttachment.Done)
		}
		if clientAttachment != nil {
			closeOnce(clientAttachment.Done)
		}

		var ok bool
		agentAttachment, clientAttachment, ok = s.waitForAttachments()
		if !ok {
			return
		}
		agent = agentAttachment.Conn
		client = clientAttachment.Conn
		if !sendControl(agent, s.Room.ID, agentAttachment.Remote, "agent", resumeMessage+" "+s.ID) ||
			!sendControl(client, s.Room.ID, clientAttachment.Remote, "client", resumeMessage+" "+s.ID) {
			closeQuietly(agent, websocket.StatusServiceRestart, "retry resume")
			closeQuietly(client, websocket.StatusServiceRestart, "retry resume")
			closeOnce(agentAttachment.Done)
			closeOnce(clientAttachment.Done)
			continue
		}
		log.Printf("resumable bridge resumed room=%s pair=%d session=%s agent=%s client=%s", s.Room.ID, pairID, s.ID, agentAttachment.Remote, clientAttachment.Remote)
	}
}

func (s *ResumeSession) waitForAttachments() (*ResumeAttachment, *ResumeAttachment, bool) {
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	var agent, client *ResumeAttachment
	for agent == nil || client == nil {
		select {
		case next := <-s.agent:
			if agent != nil {
				closeQuietly(agent.Conn, websocket.StatusNormalClosure, "replaced resume socket")
				closeOnce(agent.Done)
			}
			agent = next
		case next := <-s.client:
			if client != nil {
				closeQuietly(client.Conn, websocket.StatusNormalClosure, "replaced resume socket")
				closeOnce(client.Done)
			}
			client = next
		case <-timer.C:
			if agent != nil {
				closeQuietly(agent.Conn, websocket.StatusGoingAway, "resume window expired")
				closeOnce(agent.Done)
			}
			if client != nil {
				closeQuietly(client.Conn, websocket.StatusGoingAway, "resume window expired")
				closeOnce(client.Done)
			}
			return nil, nil, false
		case <-s.done:
			return nil, nil, false
		}
	}
	return agent, client, true
}

func (s *ResumeSession) Finish() {
	s.once.Do(func() {
		close(s.done)
		if s.onFinish != nil {
			s.onFinish(s)
		}
	})
}

func bridgeSockets(agent, client *websocket.Conn) (pumpResult, pumpResult) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan pumpResult, 2)
	go pumpBinary(ctx, agent, client, "agent_to_client", done)
	go pumpBinary(ctx, client, agent, "client_to_agent", done)
	first := <-done
	cancel()
	second := <-done
	return first, second
}

type DashboardClient struct {
	ID     string
	Conn   *websocket.Conn
	RoomID *string
	SendMu sync.Mutex
}

type StatusSnapshot struct {
	Service string         `json:"service"`
	Time    time.Time      `json:"time"`
	Rooms   []RoomSnapshot `json:"rooms"`
}

type RoomSnapshot struct {
	ID                       string     `json:"id"`
	WaitingAgents            int        `json:"waiting_agents"`
	ActivePairs              int        `json:"active_pairs"`
	TotalPairs               int64      `json:"total_pairs"`
	LastAgentRemote          *string    `json:"last_agent_remote"`
	LastAgentConnectedAt     *time.Time `json:"last_agent_connected_at"`
	LastAgentDisconnectedAt  *time.Time `json:"last_agent_disconnected_at"`
	HomeAgentConnected       bool       `json:"home_agent_connected"`
	HomeAgentRemote          *string    `json:"home_agent_remote"`
	HomeAgentConnectedAt     *time.Time `json:"home_agent_connected_at"`
	HomeAgentDisconnectedAt  *time.Time `json:"home_agent_disconnected_at"`
	LastClientRemote         *string    `json:"last_client_remote"`
	LastClientConnectedAt    *time.Time `json:"last_client_connected_at"`
	LastClientDisconnectedAt *time.Time `json:"last_client_disconnected_at"`
}

func sendStart(c *websocket.Conn, room, remote, side string) bool {
	return sendControl(c, room, remote, side, startMessage)
}

func sendControl(c *websocket.Conn, room, remote, side, message string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(message)); err != nil {
		log.Printf("control frame failed room=%s side=%s remote=%s message=%q: %v", room, side, remote, message, err)
		closeQuietly(c, websocket.StatusNormalClosure, "")
		return false
	}
	return true
}

func cleanSessionValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) != 32 {
		return ""
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return ""
		}
	}
	return strings.ToLower(value)
}

type pumpResult struct {
	Direction string
	Bytes     int64
	Messages  int64
	Err       error
}

func pumpBinary(ctx context.Context, source, destination *websocket.Conn, direction string, done chan<- pumpResult) {
	result := pumpResult{Direction: direction}
	defer func() { done <- result }()
	for {
		typ, payload, err := source.Read(ctx)
		if err != nil {
			result.Err = fmt.Errorf("read: %w", err)
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		if err := destination.Write(ctx, websocket.MessageBinary, payload); err != nil {
			result.Err = fmt.Errorf("write: %w", err)
			return
		}
		result.Bytes += int64(len(payload))
		result.Messages++
	}
}

func drainUntilClose(ctx context.Context, c *websocket.Conn) error {
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return err
		}
	}
}

func closeQuietly(c *websocket.Conn, status websocket.StatusCode, reason string) {
	if c == nil {
		return
	}
	_ = c.Close(status, reason)
}

// Resume peers must not wait for a close handshake from a transport that may
// already have vanished behind a proxy. Replacement sockets carry the
// protocol-level continuation, so discard the obsolete transport immediately.
func abortQuietly(c *websocket.Conn) {
	if c == nil {
		return
	}
	_ = c.CloseNow()
}

func closeOnce(ch chan struct{}) {
	defer func() { _ = recover() }()
	close(ch)
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func iconSVG() string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 108 108">
  <defs>
    <linearGradient id="bg" x1="12" y1="12" x2="96" y2="96" gradientUnits="userSpaceOnUse">
      <stop stop-color="#13324d"/><stop offset="1" stop-color="#40b5ae"/>
    </linearGradient>
    <clipPath id="clip"><rect x="6" y="6" width="96" height="96" rx="22"/></clipPath>
  </defs>
  <rect x="6" y="6" width="96" height="96" rx="22" fill="url(#bg)"/>
  <g clip-path="url(#clip)">
    <path d="M6 34c22-17 61-14 97-24l3 12c-32 12-70 9-99 23z" fill="#fff" opacity=".08"/>
    <path d="M0 78q13-7 27 0t28 0t28 0q13 7 25-2v32H0z" fill="#69d2c7"/>
    <path d="M4 86q18-7 36 0t36 0q16-6 28-2v4q-13-2-28 3q-18 7-36 0q-18-7-36 0z" fill="#fff" opacity=".48"/>
  </g>
  <path d="M27 25q0-7 7-7h40q7 0 7 7v28q0 7-7 7H34q-7 0-7-7z" fill="#fff"/>
  <path d="M34 27q0-3 3-3h34q3 0 3 3v20q0 3-3 3H37q-3 0-3-3z" fill="#17324d"/>
  <path d="M49 59h10l3 8H46zM39 68q0-3 3-3h24q3 0 3 3v3H39z" fill="#fff"/>
  <path d="M20 64h68l-8 11q-9 7-42 4q-9-2-18-15z" fill="#e66d4f"/>
  <path d="M31 66h43q2 0 2 2t-2 2H31q-2 0-2-2t2-2z" fill="#fff" opacity=".76"/>
</svg>`
}

func dashboardHTML(room string) string {
	roomJSON, _ := json.Marshal(room)
	return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>DeskFerry Relay</title>
  <link rel="icon" href="/relay/icon.svg" type="image/svg+xml">
  <style>
    :root { color-scheme: light; --bg:#f5f7f8; --panel:#fff; --ink:#1f2933; --muted:#65717d; --line:#d7dee3; --accent:#2f6f73; --ok:#287d52; --warn:#9a6a12; --bad:#a94343; }
    * { box-sizing:border-box; }
    body { margin:0; font-family:"Segoe UI",system-ui,-apple-system,BlinkMacSystemFont,sans-serif; background:var(--bg); color:var(--ink); }
    header { padding:28px 24px 18px; border-bottom:1px solid var(--line); background:var(--panel); }
    main { width:min(1120px, calc(100% - 32px)); margin:22px auto 40px; }
    h1 { margin:0 0 6px; font-size:clamp(26px,4vw,38px); letter-spacing:0; }
    .brand { display:flex; align-items:center; gap:14px; }
    .brand-icon { width:58px; height:58px; flex:0 0 58px; border-radius:13px; }
    .brand-text { min-width:0; }
    .subtle { color:var(--muted); }
    .toolbar { display:flex; gap:10px; align-items:center; flex-wrap:wrap; margin-top:16px; }
    .toolbar input { flex:1 1 360px; min-width:0; height:40px; border:1px solid var(--line); border-radius:8px; padding:0 12px; color:var(--ink); background:#fbfcfd; font:13px ui-monospace,SFMono-Regular,Consolas,monospace; }
    .toolbar button { height:40px; border:1px solid var(--accent); border-radius:8px; padding:0 14px; color:var(--accent); background:#fff; font-weight:700; cursor:pointer; }
    .grid { display:grid; grid-template-columns:repeat(3,minmax(0,1fr)); gap:14px; margin-bottom:18px; }
    .card { background:var(--panel); border:1px solid var(--line); border-radius:8px; padding:16px; min-height:128px; }
    .label { color:var(--muted); font-size:13px; font-weight:700; text-transform:uppercase; }
    .value { margin-top:10px; font-size:28px; font-weight:700; line-height:1.1; }
    .ok { color:var(--ok); } .warn { color:var(--warn); } .bad { color:var(--bad); }
    table { width:100%; border-collapse:collapse; background:var(--panel); border:1px solid var(--line); border-radius:8px; overflow:hidden; }
    th,td { padding:12px 14px; text-align:left; border-bottom:1px solid var(--line); vertical-align:top; font-size:14px; }
    th { color:var(--muted); font-size:12px; text-transform:uppercase; background:#fbfcfd; }
    tr:last-child td { border-bottom:0; }
    code { font-family:ui-monospace,SFMono-Regular,Consolas,monospace; font-size:13px; }
    .pill { display:inline-block; padding:3px 8px; border-radius:999px; border:1px solid var(--line); font-size:12px; font-weight:700; background:#f9fafb; }
    .pill.ok { border-color:#bfe4cf; background:#edf8f1; } .pill.bad { border-color:#efc5c5; background:#fff0f0; }
    @media (max-width:760px) { .grid { grid-template-columns:1fr; } th:nth-child(5),td:nth-child(5){display:none;} .brand-icon{width:48px;height:48px;flex-basis:48px;} }
  </style>
</head>
<body>
  <header>
    <div class="brand">
      <img class="brand-icon" src="/relay/icon.svg" alt="">
      <div class="brand-text">
        <h1>DeskFerry Relay</h1>
        <div class="subtle">Go WebSocket relay at <code>/relay/ws</code>. Status updates stream live over WebSocket.</div>
      </div>
    </div>
    <div class="toolbar"><input id="roomUrl" readonly aria-label="Relay room URL"><button id="copyRoom" type="button">Copy</button></div>
  </header>
  <main>
    <section class="grid">
      <div class="card"><div class="label">Work agent</div><div id="workStatus" class="value warn">Checking</div><p id="workDetail" class="subtle">Waiting for status.</p></div>
      <div class="card"><div class="label">Home side</div><div id="homeStatus" class="value warn">Checking</div><p id="homeDetail" class="subtle">Waiting for status.</p></div>
      <div class="card"><div class="label">RDP streams</div><div id="streamStatus" class="value">0</div><p id="streamDetail" class="subtle">No active pairs.</p></div>
    </section>
    <table>
      <thead><tr><th>Room</th><th>Work Agent</th><th>Home Side</th><th>Active Pairs</th><th>Last Client</th></tr></thead>
      <tbody id="rooms"><tr><td colspan="5" class="subtle">Loading relay status...</td></tr></tbody>
    </table>
  </main>
  <script>
    const roomsBody=document.getElementById("rooms"),workStatus=document.getElementById("workStatus"),workDetail=document.getElementById("workDetail"),homeStatus=document.getElementById("homeStatus"),homeDetail=document.getElementById("homeDetail"),streamStatus=document.getElementById("streamStatus"),streamDetail=document.getElementById("streamDetail"),roomUrl=document.getElementById("roomUrl"),copyRoom=document.getElementById("copyRoom");
    const pageRoom=` + string(roomJSON) + `;
    function pill(ok,text){return '<span class="pill '+(ok?'ok':'bad')+'">'+text+'</span>'}
    function esc(value){return String(value??"").replace(/[&<>"']/g,char=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[char]))}
    function fmt(value){return value?new Date(value).toLocaleString():""}
    function setValue(node,text,cls){node.className="value "+cls;node.textContent=text}
    function relayRoomUrl(room){return room?location.origin+'/relay/'+encodeURIComponent(room):location.origin+'/relay/'}
    function render(data){
      const rooms=data.rooms||[],waitingAgents=rooms.reduce((s,r)=>s+(r.waiting_agents||0),0),activePairs=rooms.reduce((s,r)=>s+(r.active_pairs||0),0),homeAgents=rooms.filter(r=>r.home_agent_connected).length,homeActiveRooms=rooms.filter(r=>r.home_agent_connected||(r.active_pairs||0)>0).length;
      setValue(workStatus,waitingAgents+activePairs>0?"Connected":"Waiting",waitingAgents+activePairs>0?"ok":"warn");
      workDetail.textContent=waitingAgents+' idle work sockets, '+activePairs+' paired streams.';
      setValue(homeStatus,homeActiveRooms>0?"Active":"Waiting",homeActiveRooms>0?"ok":"warn");
      homeDetail.textContent=homeAgents+' presence socket'+(homeAgents===1?'':'s')+', '+activePairs+' active RDP stream'+(activePairs===1?'':'s')+'.';
      streamStatus.textContent=activePairs.toString();
      streamDetail.textContent=activePairs===0?'No active RDP streams.':activePairs+' RDP stream'+(activePairs===1?'':'s')+' bridged.';
      if(rooms.length===0){roomsBody.innerHTML='<tr><td colspan="5" class="subtle">No rooms have connected yet.</td></tr>';return}
      roomsBody.innerHTML=rooms.map(r=>{
        const workConnected=(r.waiting_agents||0)+(r.active_pairs||0)>0,homePresence=!!r.home_agent_connected,streamActive=(r.active_pairs||0)>0,homeState=homePresence?'presence':(streamActive?'active stream':'waiting'),homeInfo=homePresence?esc(r.home_agent_remote||'')+'<br>'+esc(fmt(r.home_agent_connected_at)):(r.active_pairs||0)+' active<br>'+esc(fmt(r.last_client_connected_at));
        return '<tr><td><code>'+esc(r.id)+'</code></td><td>'+pill(workConnected,workConnected?'connected':'waiting')+'<br><span class="subtle">'+(r.waiting_agents||0)+' idle<br>'+esc(fmt(r.last_agent_connected_at))+'</span></td><td>'+pill(homePresence||streamActive,homeState)+'<br><span class="subtle">'+homeInfo+'</span></td><td>'+(r.active_pairs||0)+'<br><span class="subtle">'+(r.total_pairs||0)+' total</span></td><td><span class="subtle">'+esc(r.last_client_remote||'')+'<br>'+esc(fmt(r.last_client_connected_at))+'</span></td></tr>';
      }).join("");
    }
    function connectDashboard(){
      const scheme=location.protocol==="https:"?"wss:":"ws:",roomPath=pageRoom?'/relay/'+encodeURIComponent(pageRoom)+'/ws':"/relay/ws",socket=new WebSocket(scheme+'//'+location.host+roomPath+'?role=dashboard');
      socket.onmessage=event=>render(JSON.parse(event.data));
      socket.onclose=()=>{setValue(workStatus,"Reconnecting","warn");setValue(homeStatus,"Reconnecting","warn");setTimeout(connectDashboard,1500)};
      socket.onerror=()=>socket.close();
    }
    roomUrl.value=relayRoomUrl(pageRoom);
    copyRoom.addEventListener("click",async()=>{roomUrl.select();await navigator.clipboard.writeText(roomUrl.value)});
    connectDashboard();
  </script>
</body>
</html>`
}
