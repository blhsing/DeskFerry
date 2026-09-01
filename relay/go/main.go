package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"deskferry/internal/buildinfo"
	"deskferry/internal/tunnel"

	"nhooyr.io/websocket"
)

const (
	serviceName       = "DeskFerry.Relay"
	dashboardRole     = "dashboard"
	diagnosticLogRole = "diagnostic-log"
	resumeRole        = "resume"
	startMessage      = "start"
	resumeMessage     = "resume"
	started           = "started"
	agentUnavailable  = "agent-unavailable"
	clientUnavailable = "client-unavailable"
	serviceRDP        = "rdp"
	serviceWinRM      = "winrm"
	serviceSMB        = "smb"
	serviceScreen     = "screen"
	agentControlRole  = "agent-control"
	agentSessionRole  = "agent-session"
	protocolV2        = "2"
	sessionOfferTTL   = 8 * time.Second
	// Resumable tunnel data messages contain a 64 KiB payload plus framing.
	// Keep a bounded amount of headroom above that protocol maximum.
	relayWebSocketReadLimit = 1 << 20
)

var validRoles = map[string]bool{
	"agent":           true,
	agentControlRole:  true,
	agentSessionRole:  true,
	"client":          true,
	"home-agent":      true,
	"probe":           true,
	resumeRole:        true,
	dashboardRole:     true,
	diagnosticLogRole: true,
}

func main() {
	listen := flag.String("listen", envOrDefault("DESKFERRY_RELAY_LISTEN", "0.0.0.0:80"), "HTTP listen address")
	flag.Parse()

	srv := &http.Server{
		Addr:              *listen,
		Handler:           newServer(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("DeskFerry Go relay version=%s listening on %s", buildinfo.Version, *listen)
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
	streams := tunnel.NewHTTPStreamServer(func(ctx context.Context, conn tunnel.MessageConn, r *http.Request, room string) {
		serveRelayConn(ctx, conn, r, hub, room, "http-stream")
	})
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
		handleRelay(w, r, hub, streams)
	})
	return mux
}

func handleRelay(w http.ResponseWriter, r *http.Request, hub *RelayHub, streamServers ...*tunnel.HTTPStreamServer) {
	rest := strings.TrimPrefix(r.URL.Path, "/relay")
	if room, id, direction, ok := parseHTTPStreamPath(rest); ok {
		if len(streamServers) == 0 || streamServers[0] == nil {
			http.NotFound(w, r)
			return
		}
		streamServers[0].Serve(w, r, room, id, direction)
		return
	}
	switch {
	case rest == "" || rest == "/":
		writeHTML(w, dashboardHTML(""))
	case rest == "/health":
		writeJSON(w, map[string]any{
			"status":  "ok",
			"service": serviceName,
			"version": buildinfo.Version,
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

func parseHTTPStreamPath(rest string) (room, id, direction string, ok bool) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	switch {
	case len(parts) == 3 && parts[0] == "stream":
		id, direction = parts[1], parts[2]
	case len(parts) == 4 && parts[1] == "stream":
		room, id, direction = parts[0], parts[2], parts[3]
	default:
		return "", "", "", false
	}
	if direction != "up" && direction != "down" {
		return "", "", "", false
	}
	return room, id, direction, true
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

	serveRelayConn(r.Context(), c, r, hub, room, "websocket")
}

func serveRelayConn(ctx context.Context, c tunnel.MessageConn, r *http.Request, hub *RelayHub, room, transport string) {
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
		closeQuietly(c, websocket.StatusPolicyViolation, "missing relay role or bearer token")
		return
	}
	remote := remoteAddr(r)
	proof := readRoomProof(r)
	service := readService(r)
	if (role == "agent" || role == "client" || role == agentSessionRole || role == resumeRole) && service == "" {
		closeQuietly(c, websocket.StatusPolicyViolation, "unsupported service")
		return
	}
	log.Printf("relay transport connected transport=%s role=%s room=%s service=%s remote=%s user_agent=%q", transport, role, roomID(token), service, remote, r.UserAgent())
	switch role {
	case dashboardRole:
		hub.ServeDashboard(ctx, c, remote, room)
	case "agent":
		identity := readAgentIdentity(r)
		identity.Service = service
		hub.ServeAgent(ctx, token, c, remote, identity, readResumable(r), proof, service)
	case agentControlRole:
		hub.ServeAgentControl(ctx, token, c, remote, readAgentIdentity(r).Instance, readAgentServices(r), readConcurrency(r), proof)
	case agentSessionRole:
		hub.ServeAgentSession(ctx, token, c, remote, readAgentIdentity(r).Instance, r.Header.Get(tunnel.HeaderSessionID), readResumable(r), proof, service)
	case "client":
		if strings.TrimSpace(r.Header.Get(tunnel.HeaderProtocolVersion)) == protocolV2 {
			hub.ServeV2Client(ctx, token, c, remote, readResumable(r), readHeartbeat(r), proof, service)
		} else {
			hub.ServeClient(ctx, token, c, remote, readResumable(r), proof, service)
		}
	case resumeRole:
		hub.ServeResume(ctx, token, c, remote, r.Header.Get("X-DeskFerry-Session"), r.Header.Get("X-DeskFerry-Session-Side"), proof, service)
	case "home-agent":
		hub.ServeHomeAgent(ctx, token, c, remote, proof)
	case diagnosticLogRole:
		hub.ServeDiagnosticLog(ctx, token, c, remote, proof, r.Header.Get(tunnel.HeaderLogComponent), r.Header.Get(tunnel.HeaderLogInstance))
	case "probe":
		room := hub.roomFor(token)
		if !room.AuthorizeClient(proof) {
			closeQuietly(c, websocket.StatusPolicyViolation, "room authentication failed")
		} else {
			closeQuietly(c, websocket.StatusNormalClosure, "probe ok")
		}
	default:
		closeQuietly(c, websocket.StatusPolicyViolation, "unsupported role")
	}
}

type diagnosticLogBatch struct {
	Entries []string `json:"entries"`
}

func (h *RelayHub) ServeDiagnosticLog(ctx context.Context, token string, c tunnel.MessageConn, remote, proof, component, instance string) {
	room := h.roomFor(token)
	if !room.AuthorizeClient(proof) {
		closeQuietly(c, websocket.StatusPolicyViolation, "room authentication failed")
		return
	}
	component = logLabel(component, 64)
	instance = logLabel(instance, 128)
	for {
		typ, payload, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var batch diagnosticLogBatch
		if json.Unmarshal(payload, &batch) != nil || len(batch.Entries) == 0 || len(batch.Entries) > 100 {
			closeQuietly(c, websocket.StatusPolicyViolation, "invalid diagnostic log batch")
			return
		}
		for _, entry := range batch.Entries {
			entry = strings.ReplaceAll(strings.ReplaceAll(entry, "\r", " "), "\n", " ")
			if len(entry) > 8192 {
				entry = entry[:8192]
			}
			log.Printf("agent_log room=%s component=%s instance=%s remote=%s message=%q", room.ID, component, instance, remote, entry)
		}
		ack, _ := json.Marshal(map[string]int{"accepted": len(batch.Entries)})
		if err := c.Write(ctx, websocket.MessageText, ack); err != nil {
			return
		}
	}
}

func logLabel(value string, limit int) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r", ""), "\n", ""))
	if value == "" {
		return "unknown"
	}
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

func acceptWebSocket(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(relayWebSocketReadLimit)
	return c, nil
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

func readHeartbeat(r *http.Request) bool {
	value := strings.TrimSpace(r.Header.Get(tunnel.HeaderHeartbeat))
	return value == "1" || strings.EqualFold(value, "true")
}

func readAgentServices(r *http.Request) map[string]bool {
	services := make(map[string]bool)
	for _, value := range strings.Split(r.Header.Get(tunnel.HeaderAgentServices), ",") {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == serviceRDP || value == serviceWinRM || value == serviceSMB || value == serviceScreen {
			services[value] = true
		}
	}
	return services
}

func readConcurrency(r *http.Request) int {
	value, err := strconv.Atoi(strings.TrimSpace(r.Header.Get(tunnel.HeaderConcurrency)))
	if err != nil || value < 1 || value > 256 {
		return 4
	}
	return value
}

func readRoomProof(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("X-DeskFerry-Room-Proof"))
	if len(value) != 43 {
		return ""
	}
	for _, ch := range value {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
			return ""
		}
	}
	return value
}

func readService(r *http.Request) string {
	value := strings.ToLower(strings.TrimSpace(r.Header.Get("X-DeskFerry-Service")))
	if value == "" {
		return serviceRDP
	}
	if value == serviceRDP || value == serviceWinRM || value == serviceSMB || value == serviceScreen {
		return value
	}
	return ""
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
	completed  map[string]time.Time
	controls   map[string]*AgentControl
	pending    map[string]*PendingSession
}

func newRelayHub() *RelayHub {
	return &RelayHub{
		rooms:      make(map[string]*RelayRoom),
		dashboards: make(map[string]*DashboardClient),
		sessions:   make(map[string]*ResumeSession),
		completed:  make(map[string]time.Time),
		controls:   make(map[string]*AgentControl),
		pending:    make(map[string]*PendingSession),
	}
}

type AgentControl struct {
	Room        *RelayRoom
	Conn        tunnel.MessageConn
	Remote      string
	AgentID     string
	Services    map[string]bool
	Concurrency int
	SendMu      sync.Mutex
	Done        chan struct{}
	Active      atomic.Int32
	closed      atomic.Bool
}

func (a *AgentControl) TryReserve() bool {
	for !a.closed.Load() {
		current := a.Active.Load()
		if int(current) >= a.Concurrency {
			return false
		}
		if a.Active.CompareAndSwap(current, current+1) {
			return true
		}
	}
	return false
}

func (a *AgentControl) Release() {
	for {
		current := a.Active.Load()
		if current <= 0 || a.Active.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func (a *AgentControl) Send(message tunnel.ControlMessage) bool {
	a.SendMu.Lock()
	defer a.SendMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tunnel.WriteControlMessage(ctx, a.Conn, message); err != nil {
		return false
	}
	return true
}

func (a *AgentControl) Close(reason string) {
	if a.closed.CompareAndSwap(false, true) {
		close(a.Done)
		closeQuietly(a.Conn, websocket.StatusNormalClosure, reason)
	}
}

type AgentDataSocket struct {
	Conn      tunnel.MessageConn
	Remote    string
	Resumable bool
	Done      chan struct{}
}

type PendingSession struct {
	ID        string
	Room      *RelayRoom
	Control   *AgentControl
	Client    tunnel.MessageConn
	Remote    string
	Proof     string
	Service   string
	Resumable bool
	Heartbeat bool
	ExpiresAt time.Time
	Response  chan tunnel.ControlMessage
	Agent     chan *AgentDataSocket
}

func (h *RelayHub) ServeAgentControl(ctx context.Context, token string, c tunnel.MessageConn, remote, agentID string, services map[string]bool, concurrency int, proof string) {
	room := h.roomFor(token)
	if agentID == "" || len(services) == 0 {
		sendV2Result(c, tunnel.MessageInvalidRequest, "", "agent identity and services are required")
		closeQuietly(c, websocket.StatusPolicyViolation, "invalid agent control request")
		return
	}
	if !room.AuthorizeAgent(proof) {
		sendV2Result(c, tunnel.MessageAuthFailed, "", "room authentication failed")
		closeQuietly(c, websocket.StatusPolicyViolation, "room authentication failed")
		return
	}
	control := &AgentControl{Room: room, Conn: c, Remote: remote, AgentID: agentID, Services: services, Concurrency: concurrency, Done: make(chan struct{})}
	key := room.ID + "/" + agentID
	h.mu.Lock()
	previous := h.controls[key]
	h.controls[key] = control
	h.mu.Unlock()
	room.ControlConnected(agentID, remote)
	removedLegacy := room.RemoveLegacyAgents(agentID)
	if previous != nil {
		previous.Close("replaced by newer control connection")
	}
	defer func() {
		h.mu.Lock()
		if h.controls[key] == control {
			delete(h.controls, key)
		}
		for _, pending := range h.pending {
			if pending.Control == control {
				select {
				case pending.Response <- tunnel.ControlMessage{Type: tunnel.MessageNoAgent, SessionID: pending.ID, Reason: "work control disconnected"}:
				default:
				}
			}
		}
		h.mu.Unlock()
		control.Close("")
		room.ControlDisconnected(agentID, remote)
		h.NotifyDashboards()
		log.Printf("agent control disconnected room=%s agent=%s remote=%s", room.ID, agentID, remote)
	}()
	if !control.Send(tunnel.ControlMessage{Type: tunnel.MessageControlReady, AgentID: agentID, ProtocolVersion: tunnel.ProtocolVersion2}) {
		return
	}
	log.Printf("agent control connected room=%s agent=%s services=%v concurrency=%d remote=%s removed_legacy_slots=%d", room.ID, agentID, sortedServices(services), concurrency, remote, removedLegacy)
	h.NotifyDashboards()
	for {
		message, err := tunnel.ReadControlMessage(ctx, c)
		if err != nil {
			return
		}
		if cleanSessionValue(message.SessionID) == "" {
			continue
		}
		h.mu.Lock()
		pending := h.pending[room.ID+"/"+message.SessionID]
		session := h.sessions[room.ID+"/"+message.SessionID]
		h.mu.Unlock()
		if message.Type == tunnel.MessageSessionClosed {
			if session != nil {
				session.Finish()
			}
			continue
		}
		if pending == nil || pending.Control != control {
			continue
		}
		switch message.Type {
		case tunnel.MessageAccept, tunnel.MessageBusy, tunnel.MessageServiceDisabled, tunnel.MessageUnsupportedVersion:
			select {
			case pending.Response <- message:
			default:
			}
		}
	}
}

func (h *RelayHub) ServeV2Client(ctx context.Context, token string, c tunnel.MessageConn, remote string, resumable, heartbeat bool, proof, service string) {
	room := h.roomFor(token)
	if !room.AuthorizeClient(proof) {
		sendV2Result(c, tunnel.MessageAuthFailed, "", "room authentication failed")
		closeQuietly(c, websocket.StatusPolicyViolation, "room authentication failed")
		return
	}
	control, serviceControlExists := h.selectControl(room.ID, service)
	if control == nil {
		if room.HasLegacyAgent(service) {
			h.ServeClient(ctx, token, c, remote, resumable, proof, service)
			return
		}
		result, reason := tunnel.MessageNoAgent, "no work agent control connection"
		if serviceControlExists {
			result, reason = tunnel.MessageBusy, "work agent concurrency limit reached"
		}
		room.RecordRejection(result)
		sendV2Result(c, result, "", reason)
		closeQuietly(c, websocket.StatusNormalClosure, reason)
		return
	}
	h.serveOnDemandClient(ctx, room, c, remote, resumable, heartbeat, proof, service, control, true)
}

func (h *RelayHub) serveOnDemandClient(ctx context.Context, room *RelayRoom, c tunnel.MessageConn, remote string, resumable, heartbeat bool, proof, service string, control *AgentControl, typed bool) {
	defer control.Release()
	now := time.Now().UTC()
	pending := &PendingSession{
		ID: randomID(), Room: room, Control: control, Client: c, Remote: remote, Proof: proof, Service: service,
		Resumable: resumable, Heartbeat: heartbeat, ExpiresAt: now.Add(sessionOfferTTL), Response: make(chan tunnel.ControlMessage, 1), Agent: make(chan *AgentDataSocket, 1),
	}
	key := room.ID + "/" + pending.ID
	h.mu.Lock()
	if len(h.pending) >= 4096 {
		h.mu.Unlock()
		room.RecordRejection(tunnel.MessageBusy)
		rejectSessionClient(c, typed, tunnel.MessageBusy, "", "relay pending-session limit reached")
		return
	}
	h.pending[key] = pending
	h.mu.Unlock()
	room.PendingStarted(service)
	pendingOpen := true
	cleanupPending := func() {
		if !pendingOpen {
			return
		}
		pendingOpen = false
		h.mu.Lock()
		delete(h.pending, key)
		h.mu.Unlock()
		room.PendingEnded(service)
		h.NotifyDashboards()
	}
	defer cleanupPending()
	offer := tunnel.ControlMessage{Type: tunnel.MessageSessionOffer, SessionID: pending.ID, Room: room.ID, Service: service, AgentID: control.AgentID, CreatedAt: now, ExpiresAt: pending.ExpiresAt, ProtocolVersion: tunnel.ProtocolVersion2, Resumable: resumable, Heartbeat: heartbeat}
	if !control.Send(offer) {
		room.RecordRejection(tunnel.MessageNoAgent)
		rejectSessionClient(c, typed, tunnel.MessageNoAgent, pending.ID, "work control disconnected")
		return
	}
	h.NotifyDashboards()
	var response tunnel.ControlMessage
	timer := time.NewTimer(time.Until(pending.ExpiresAt))
	defer timer.Stop()
	select {
	case response = <-pending.Response:
	case <-timer.C:
		room.RecordRejection(tunnel.MessageTimeout)
		rejectSessionClient(c, typed, tunnel.MessageTimeout, pending.ID, "work agent did not answer the offer")
		return
	case <-ctx.Done():
		return
	}
	if response.Type != tunnel.MessageAccept {
		room.RecordRejection(response.Type)
		rejectSessionClient(c, typed, response.Type, pending.ID, response.Reason)
		return
	}
	var agent *AgentDataSocket
	select {
	case agent = <-pending.Agent:
	case <-timer.C:
		room.RecordRejection(tunnel.MessageTimeout)
		rejectSessionClient(c, typed, tunnel.MessageTimeout, pending.ID, "accepted work session did not connect")
		return
	case <-ctx.Done():
		return
	}
	cleanupPending()
	heartbeat = pending.Heartbeat && response.Heartbeat
	clientReady := false
	if typed {
		clientReady = sendV2SessionReady(c, pending.ID, service, heartbeat)
	} else {
		clientReady = sendControl(c, room.ID, remote, "legacy-client", startMessage+" "+pending.ID)
	}
	if !sendV2SessionReady(agent.Conn, pending.ID, service, heartbeat) || !clientReady {
		closeQuietly(agent.Conn, websocket.StatusNormalClosure, "peer unavailable")
		return
	}
	log.Printf("v2 pairing room=%s session=%s service=%s agent=%s client=%s", room.ID, pending.ID, service, agent.Remote, remote)
	room.ServiceSessionStarted(service)
	defer room.ServiceSessionEnded(service)
	if resumable && agent.Resumable {
		session := h.newResumeSessionWithID(pending.ID, room, agent.Remote, remote, proof, service)
		session.Run(agent.Conn, c, agent.Done, h.NotifyDashboards)
		return
	}
	room.Bridge(ctx, agent.Conn, c, agent.Remote, remote, agent.Done, h.NotifyDashboards)
}

func (h *RelayHub) ServeAgentSession(ctx context.Context, token string, c tunnel.MessageConn, remote, agentID, sessionID string, resumable bool, proof, service string) {
	room := roomID(token)
	sessionID = cleanSessionValue(sessionID)
	h.mu.Lock()
	pending := h.pending[room+"/"+sessionID]
	h.mu.Unlock()
	if pending == nil || pending.Control.AgentID != agentID || pending.Service != service || !proofEqual(pending.Proof, proof) || time.Now().After(pending.ExpiresAt) {
		sendV2Result(c, tunnel.MessageInvalidRequest, sessionID, "unknown or expired pending session")
		closeQuietly(c, websocket.StatusPolicyViolation, "unknown pending session")
		return
	}
	data := &AgentDataSocket{Conn: c, Remote: remote, Resumable: resumable, Done: make(chan struct{})}
	select {
	case pending.Agent <- data:
	case <-ctx.Done():
		return
	default:
		closeQuietly(c, websocket.StatusPolicyViolation, "duplicate agent session")
		return
	}
	select {
	case <-data.Done:
	case <-ctx.Done():
	}
}

func (h *RelayHub) selectControl(room, service string) (*AgentControl, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	controls := make([]*AgentControl, 0)
	for key, control := range h.controls {
		if !strings.HasPrefix(key, room+"/") || control.closed.Load() || !control.Services[service] {
			continue
		}
		controls = append(controls, control)
	}
	sort.Slice(controls, func(i, j int) bool { return controls[i].Active.Load() < controls[j].Active.Load() })
	for _, control := range controls {
		if control.TryReserve() {
			return control, true
		}
	}
	return nil, len(controls) > 0
}

func sortedServices(services map[string]bool) []string {
	out := make([]string, 0, len(services))
	for service := range services {
		out = append(out, service)
	}
	sort.Strings(out)
	return out
}

func sendV2Result(c tunnel.MessageConn, result, sessionID, reason string) bool {
	return sendV2ServiceResult(c, result, sessionID, "", reason)
}

func sendV2ServiceResult(c tunnel.MessageConn, result, sessionID, service, reason string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return tunnel.WriteControlMessage(ctx, c, tunnel.ControlMessage{Type: result, SessionID: sessionID, Service: service, ProtocolVersion: tunnel.ProtocolVersion2, Reason: reason}) == nil
}

func sendV2SessionReady(c tunnel.MessageConn, sessionID, service string, heartbeat bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return tunnel.WriteControlMessage(ctx, c, tunnel.ControlMessage{Type: tunnel.MessageSessionReady, SessionID: sessionID, Service: service, ProtocolVersion: tunnel.ProtocolVersion2, Heartbeat: heartbeat}) == nil
}

func rejectSessionClient(c tunnel.MessageConn, typed bool, result, sessionID, reason string) {
	if typed {
		sendV2Result(c, result, sessionID, reason)
	}
	closeQuietly(c, websocket.StatusTryAgainLater, reason)
}

func (h *RelayHub) ServeAgent(ctx context.Context, token string, c tunnel.MessageConn, remote string, identity AgentIdentity, resumable bool, proof, service string) {
	room := h.roomFor(token)
	if !room.AuthorizeAgent(proof) {
		log.Printf("agent rejected by room authentication room=%s service=%s remote=%s", room.ID, service, remote)
		closeQuietly(c, websocket.StatusPolicyViolation, "room authentication failed")
		return
	}
	waiting, replaced := room.EnqueueAgent(c, remote, identity, resumable, service)
	log.Printf("agent waiting room=%s service=%s remote=%s key=%s replaced=%d", room.ID, service, remote, identity.LogString(), replaced)
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
	log.Printf("pairing room=%s service=%s agent=%s client=%s", room.ID, service, remote, peer.Remote)
	if waiting.Resumable && peer.Resumable {
		session := h.newResumeSession(room, remote, peer.Remote, proof, service)
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

func (h *RelayHub) ServeClient(ctx context.Context, token string, c tunnel.MessageConn, remote string, resumable bool, proof, service string) {
	room := h.roomFor(token)
	if !room.AuthorizeClient(proof) {
		log.Printf("client rejected by room authentication room=%s service=%s remote=%s", room.ID, service, remote)
		closeQuietly(c, websocket.StatusPolicyViolation, "room authentication failed")
		return
	}
	control, serviceControlExists := h.selectControl(room.ID, service)
	if control != nil {
		h.serveOnDemandClient(ctx, room, c, remote, resumable, false, proof, service, control, false)
		return
	}
	if serviceControlExists {
		room.RecordRejection(tunnel.MessageBusy)
		closeQuietly(c, websocket.StatusTryAgainLater, "work agent concurrency limit reached")
		return
	}
	for {
		waiting := room.TryTakeAgent(service)
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

func (h *RelayHub) ServeResume(ctx context.Context, token string, c tunnel.MessageConn, remote, sessionID, side, proof, service string) {
	roomID := roomID(token)
	sessionID = cleanSessionValue(sessionID)
	side = strings.ToLower(strings.TrimSpace(side))
	if sessionID == "" || (side != "agent" && side != "client") {
		log.Printf("resume rejected room=%s session=%s side=%s remote=%s", roomID, sessionID, side, remote)
		closeQuietly(c, websocket.StatusPolicyViolation, "unknown resumable session")
		return
	}
	key := roomID + "/" + sessionID
	h.mu.Lock()
	session := h.sessions[key]
	completedUntil := h.completed[key]
	if !completedUntil.IsZero() && !time.Now().Before(completedUntil) {
		delete(h.completed, key)
		completedUntil = time.Time{}
	}
	h.mu.Unlock()
	if session == nil {
		if !completedUntil.IsZero() {
			log.Printf("resume rejected for completed session room=%s session=%s side=%s remote=%s", roomID, sessionID, side, remote)
			closeQuietly(c, websocket.StatusPolicyViolation, "unknown resumable session")
			return
		}
		room := h.existingRoom(roomID)
		if side == "agent" {
			if room == nil {
				room = h.roomFor(token)
			}
			if !room.AuthorizeAgent(proof) {
				log.Printf("resume authentication failed room=%s session=%s side=%s remote=%s", roomID, sessionID, side, remote)
				closeQuietly(c, websocket.StatusPolicyViolation, "room authentication failed")
				return
			}
		} else {
			if room == nil || !room.CredentialSet() {
				log.Printf("resume room not ready room=%s session=%s side=%s remote=%s", roomID, sessionID, side, remote)
				closeQuietly(c, websocket.StatusTryAgainLater, "resume room not ready")
				return
			}
			if !room.AuthorizeClient(proof) {
				log.Printf("resume authentication failed room=%s session=%s side=%s remote=%s", roomID, sessionID, side, remote)
				closeQuietly(c, websocket.StatusPolicyViolation, "room authentication failed")
				return
			}
		}

		created := false
		h.mu.Lock()
		session = h.sessions[key]
		if session == nil && len(h.sessions) < 4096 {
			agentRemote, clientRemote := "", ""
			if side == "agent" {
				agentRemote = remote
			} else {
				clientRemote = remote
			}
			session = NewResumeSession(sessionID, room, agentRemote, clientRemote, proof, service, func(completed *ResumeSession) {
				h.mu.Lock()
				if h.sessions[key] == completed {
					delete(h.sessions, key)
					h.completed[key] = time.Now().Add(5 * time.Minute)
				}
				h.pruneCompletedLocked()
				h.mu.Unlock()
			})
			h.sessions[key] = session
			created = true
		}
		h.mu.Unlock()
		if session == nil {
			closeQuietly(c, websocket.StatusTryAgainLater, "relay resumable-session limit reached")
			return
		}
		if created {
			log.Printf("reconstructed resumable session room=%s session=%s service=%s first_side=%s remote=%s", roomID, sessionID, service, side, remote)
			go session.RunRecovered(h.NotifyDashboards)
		}
	}
	if session.Service != service || !proofEqual(session.RoomProof, proof) {
		log.Printf("resume rejected room=%s session=%s side=%s remote=%s", roomID, sessionID, side, remote)
		closeQuietly(c, websocket.StatusPolicyViolation, "unknown resumable session")
		return
	}
	log.Printf("resume attachment waiting room=%s session=%s side=%s remote=%s", roomID, sessionID, side, remote)
	attached := session.Attach(ctx, side, c, remote)
	log.Printf("resume attachment released room=%s session=%s side=%s remote=%s attached=%t", roomID, sessionID, side, remote, attached)
	if !attached {
		closeQuietly(c, websocket.StatusTryAgainLater, "resumable session unavailable")
	}
}

func (h *RelayHub) newResumeSession(room *RelayRoom, agentRemote, clientRemote, proof, service string) *ResumeSession {
	return h.newResumeSessionWithID(randomID(), room, agentRemote, clientRemote, proof, service)
}

func (h *RelayHub) newResumeSessionWithID(id string, room *RelayRoom, agentRemote, clientRemote, proof, service string) *ResumeSession {
	key := room.ID + "/" + id
	session := NewResumeSession(id, room, agentRemote, clientRemote, proof, service, func(s *ResumeSession) {
		h.mu.Lock()
		delete(h.sessions, key)
		h.completed[key] = time.Now().Add(5 * time.Minute)
		h.pruneCompletedLocked()
		h.mu.Unlock()
	})
	h.mu.Lock()
	delete(h.completed, key)
	h.sessions[key] = session
	h.mu.Unlock()
	return session
}

func (h *RelayHub) pruneCompletedLocked() {
	if len(h.completed) <= 4096 {
		return
	}
	now := time.Now()
	for key, expiresAt := range h.completed {
		if !now.Before(expiresAt) {
			delete(h.completed, key)
		}
	}
}

func (h *RelayHub) ServeHomeAgent(ctx context.Context, token string, c tunnel.MessageConn, remote, proof string) {
	room := h.roomFor(token)
	if !room.AuthorizeClient(proof) {
		closeQuietly(c, websocket.StatusPolicyViolation, "room authentication failed")
		return
	}
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

func (h *RelayHub) ServeDashboard(ctx context.Context, c tunnel.MessageConn, remote, room string) {
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

func (h *RelayHub) existingRoom(id string) *RelayRoom {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rooms[id]
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
	credentialSet            bool
	roomProof                string
	activePairs              int
	controlConnections       int
	pendingRequests          int
	rejections               map[string]int64
	controlAgents            map[string]int
	pendingByService         map[string]int
	activeByService          map[string]int
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
	return &RelayRoom{ID: id, rejections: make(map[string]int64), controlAgents: make(map[string]int), pendingByService: make(map[string]int), activeByService: make(map[string]int)}
}

func (r *RelayRoom) AuthorizeAgent(proof string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneClosedAgentsLocked()
	if !r.credentialSet || (len(r.agents) == 0 && r.controlConnections == 0 && r.activePairs == 0) {
		r.credentialSet = true
		r.roomProof = proof
		return true
	}
	return proofEqual(r.roomProof, proof)
}

func (r *RelayRoom) ControlConnected(agentID, remote string) {
	now := time.Now().UTC()
	remoteCopy := remote
	r.mu.Lock()
	r.controlConnections++
	r.controlAgents[agentID]++
	r.lastAgentRemote = &remoteCopy
	r.lastAgentConnectedAt = &now
	r.mu.Unlock()
}

func (r *RelayRoom) ControlDisconnected(agentID, remote string) {
	now := time.Now().UTC()
	r.mu.Lock()
	if r.controlConnections > 0 {
		r.controlConnections--
	}
	if r.controlAgents[agentID] <= 1 {
		delete(r.controlAgents, agentID)
	} else {
		r.controlAgents[agentID]--
	}
	r.lastAgentDisconnectedAt = &now
	r.mu.Unlock()
}

func (r *RelayRoom) PendingStarted(service string) {
	r.mu.Lock()
	r.pendingRequests++
	r.pendingByService[service]++
	r.mu.Unlock()
}

func (r *RelayRoom) PendingEnded(service string) {
	r.mu.Lock()
	if r.pendingRequests > 0 {
		r.pendingRequests--
	}
	if r.pendingByService[service] > 0 {
		r.pendingByService[service]--
	}
	r.mu.Unlock()
}

func (r *RelayRoom) ServiceSessionStarted(service string) {
	r.mu.Lock()
	r.activeByService[service]++
	r.mu.Unlock()
}

func (r *RelayRoom) ServiceSessionEnded(service string) {
	r.mu.Lock()
	if r.activeByService[service] > 0 {
		r.activeByService[service]--
	}
	r.mu.Unlock()
}

func (r *RelayRoom) RecordRejection(kind string) {
	r.mu.Lock()
	r.rejections[kind]++
	r.mu.Unlock()
}

func (r *RelayRoom) AuthorizeClient(proof string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.credentialSet {
		return proof == ""
	}
	return proofEqual(r.roomProof, proof)
}

func (r *RelayRoom) CredentialSet() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.credentialSet
}

func proofEqual(expected, actual string) bool {
	if len(expected) != len(actual) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}

func (r *RelayRoom) EnqueueAgent(c tunnel.MessageConn, remote string, identity AgentIdentity, resumable bool, service string) (*WaitingAgent, int) {
	waiting := NewWaitingAgent(c, remote, identity, resumable, service)
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

func (r *RelayRoom) TryTakeAgent(service string) *WaitingAgent {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneClosedAgentsLocked()
	for i, waiting := range r.agents {
		if waiting.IsOpen() && waiting.Service == service {
			r.agents = append(r.agents[:i], r.agents[i+1:]...)
			return waiting
		}
	}
	return nil
}

func (r *RelayRoom) HasLegacyAgent(service string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneClosedAgentsLocked()
	for _, waiting := range r.agents {
		if waiting.IsOpen() && waiting.Service == service {
			return true
		}
	}
	return false
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

func (r *RelayRoom) RemoveLegacyAgents(instance string) int {
	r.mu.Lock()
	r.pruneClosedAgentsLocked()
	removed := make([]*WaitingAgent, 0)
	kept := r.agents[:0]
	for _, agent := range r.agents {
		if agent.Identity.Instance == instance && instance != "" {
			agent.Cancel()
			removed = append(removed, agent)
			continue
		}
		kept = append(kept, agent)
	}
	r.agents = kept
	r.mu.Unlock()
	for _, agent := range removed {
		closeQuietly(agent.Conn, websocket.StatusNormalClosure, "replaced by protocol v2 control connection")
	}
	return len(removed)
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

func (r *RelayRoom) Bridge(ctx context.Context, agent, client tunnel.MessageConn, agentRemote, clientRemote string, clientDone chan struct{}, stateChanged func()) {
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
		ControlConnections:       r.controlConnections,
		PendingRequests:          r.pendingRequests,
		BusyRejections:           r.rejections[tunnel.MessageBusy],
		NoAgentRejections:        r.rejections[tunnel.MessageNoAgent],
		ControlAgents:            sortedIntKeys(r.controlAgents),
		ProtocolVersion:          tunnel.ProtocolVersion2,
		PendingByService:         cloneIntMap(r.pendingByService),
		ActiveSessionsByService:  cloneIntMap(r.activeByService),
		Protected:                r.credentialSet && r.roomProof != "",
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

func sortedIntKeys(values map[string]int) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func cloneIntMap(values map[string]int) map[string]int {
	out := make(map[string]int, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
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
	Service  string
}

func (i AgentIdentity) Valid() bool {
	return i.Instance != "" && i.Slot != ""
}

func (i AgentIdentity) Equal(other AgentIdentity) bool {
	return i.Instance == other.Instance && i.Slot == other.Slot && i.Service == other.Service && i.Valid()
}

func (i AgentIdentity) LogString() string {
	if !i.Valid() {
		return "legacy"
	}
	return i.Instance + "/" + i.Service + "/" + i.Slot
}

type WaitingAgent struct {
	Conn      tunnel.MessageConn
	Remote    string
	Identity  AgentIdentity
	Resumable bool
	Service   string
	Paired    chan *HomePeer
	Done      chan struct{}

	closed atomic.Bool
	paired atomic.Bool
	once   sync.Once
}

func NewWaitingAgent(c tunnel.MessageConn, remote string, identity AgentIdentity, resumable bool, service string) *WaitingAgent {
	return &WaitingAgent{
		Conn:      c,
		Remote:    remote,
		Identity:  identity,
		Resumable: resumable,
		Service:   service,
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
	Conn      tunnel.MessageConn
	Remote    string
	Resumable bool
	Done      chan struct{}
	Started   chan string

	doneOnce    sync.Once
	startedOnce sync.Once
}

func NewHomePeer(c tunnel.MessageConn, remote string, resumable bool) *HomePeer {
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
	Conn   tunnel.MessageConn
	Remote string
	Done   chan struct{}
}

type ResumeSession struct {
	ID           string
	Room         *RelayRoom
	AgentRemote  string
	ClientRemote string
	RoomProof    string
	Service      string

	agent    chan *ResumeAttachment
	client   chan *ResumeAttachment
	done     chan struct{}
	onFinish func(*ResumeSession)
	once     sync.Once
}

func NewResumeSession(id string, room *RelayRoom, agentRemote, clientRemote, proof, service string, onFinish func(*ResumeSession)) *ResumeSession {
	return &ResumeSession{
		ID:           id,
		Room:         room,
		AgentRemote:  agentRemote,
		ClientRemote: clientRemote,
		RoomProof:    proof,
		Service:      service,
		agent:        make(chan *ResumeAttachment, 2),
		client:       make(chan *ResumeAttachment, 2),
		done:         make(chan struct{}),
		onFinish:     onFinish,
	}
}

func (s *ResumeSession) Attach(ctx context.Context, side string, conn tunnel.MessageConn, remote string) bool {
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

func (s *ResumeSession) Run(agent, client tunnel.MessageConn, clientDone chan struct{}, stateChanged func()) {
	s.run(agent, client, clientDone, stateChanged, nil, nil)
}

func (s *ResumeSession) RunRecovered(stateChanged func()) {
	defer s.Finish()
	for {
		agentAttachment, clientAttachment, ok := s.waitForAttachments()
		if !ok {
			return
		}
		if !sendControl(agentAttachment.Conn, s.Room.ID, agentAttachment.Remote, "agent", resumeMessage+" "+s.ID) ||
			!sendControl(clientAttachment.Conn, s.Room.ID, clientAttachment.Remote, "client", resumeMessage+" "+s.ID) {
			closeQuietly(agentAttachment.Conn, websocket.StatusServiceRestart, "retry resume")
			closeQuietly(clientAttachment.Conn, websocket.StatusServiceRestart, "retry resume")
			closeOnce(agentAttachment.Done)
			closeOnce(clientAttachment.Done)
			continue
		}
		s.AgentRemote = agentAttachment.Remote
		s.ClientRemote = clientAttachment.Remote
		s.Room.ServiceSessionStarted(s.Service)
		defer s.Room.ServiceSessionEnded(s.Service)
		log.Printf("reconstructed resumable bridge ready room=%s session=%s service=%s agent=%s client=%s", s.Room.ID, s.ID, s.Service, s.AgentRemote, s.ClientRemote)
		s.run(agentAttachment.Conn, clientAttachment.Conn, nil, stateChanged, agentAttachment, clientAttachment)
		return
	}
}

func (s *ResumeSession) run(agent, client tunnel.MessageConn, clientDone chan struct{}, stateChanged func(), agentAttachment, clientAttachment *ResumeAttachment) {
	startedAt := time.Now()
	pairID := s.Room.PairStarted(s.ClientRemote)
	stateChanged()
	defer func() {
		s.Room.PairEnded()
		if clientDone != nil {
			closeOnce(clientDone)
		}
		s.Finish()
		stateChanged()
		log.Printf("resumable bridge closed room=%s pair=%d session=%s agent=%s client=%s duration=%s", s.Room.ID, pairID, s.ID, s.AgentRemote, s.ClientRemote, time.Since(startedAt).Round(time.Millisecond))
	}()

	for {
		first, second := bridgeSockets(agent, client)
		// bridgeSockets cancels the opposite pump after the first one ends.
		// A close observed by that canceled pump did not initiate shutdown and
		// must not complete an otherwise resumable session.
		if isSessionClose(first.Err) {
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

func isSessionClose(err error) bool {
	var closeErr websocket.CloseError
	return errors.As(err, &closeErr) && closeErr.Code == websocket.StatusNormalClosure && closeErr.Reason == "session closed"
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

func bridgeSockets(agent, client tunnel.MessageConn) (pumpResult, pumpResult) {
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
	Conn   tunnel.MessageConn
	RoomID *string
	SendMu sync.Mutex
}

type StatusSnapshot struct {
	Service string         `json:"service"`
	Time    time.Time      `json:"time"`
	Rooms   []RoomSnapshot `json:"rooms"`
}

type RoomSnapshot struct {
	ID                       string         `json:"id"`
	WaitingAgents            int            `json:"waiting_agents"`
	ControlConnections       int            `json:"control_connections"`
	PendingRequests          int            `json:"pending_requests"`
	BusyRejections           int64          `json:"busy_rejections"`
	NoAgentRejections        int64          `json:"no_agent_rejections"`
	ControlAgents            []string       `json:"control_agents"`
	ProtocolVersion          int            `json:"protocol_version"`
	PendingByService         map[string]int `json:"pending_by_service"`
	ActiveSessionsByService  map[string]int `json:"active_sessions_by_service"`
	Protected                bool           `json:"protected"`
	ActivePairs              int            `json:"active_pairs"`
	TotalPairs               int64          `json:"total_pairs"`
	LastAgentRemote          *string        `json:"last_agent_remote"`
	LastAgentConnectedAt     *time.Time     `json:"last_agent_connected_at"`
	LastAgentDisconnectedAt  *time.Time     `json:"last_agent_disconnected_at"`
	HomeAgentConnected       bool           `json:"home_agent_connected"`
	HomeAgentRemote          *string        `json:"home_agent_remote"`
	HomeAgentConnectedAt     *time.Time     `json:"home_agent_connected_at"`
	HomeAgentDisconnectedAt  *time.Time     `json:"home_agent_disconnected_at"`
	LastClientRemote         *string        `json:"last_client_remote"`
	LastClientConnectedAt    *time.Time     `json:"last_client_connected_at"`
	LastClientDisconnectedAt *time.Time     `json:"last_client_disconnected_at"`
}

func sendStart(c tunnel.MessageConn, room, remote, side string) bool {
	return sendControl(c, room, remote, side, startMessage)
}

func sendControl(c tunnel.MessageConn, room, remote, side, message string) bool {
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

func pumpBinary(ctx context.Context, source, destination tunnel.MessageConn, direction string, done chan<- pumpResult) {
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

func drainUntilClose(ctx context.Context, c tunnel.MessageConn) error {
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return err
		}
	}
}

func closeQuietly(c tunnel.MessageConn, status websocket.StatusCode, reason string) {
	if c == nil {
		return
	}
	_ = c.Close(status, reason)
}

// Resume peers must not wait for a close handshake from a transport that may
// already have vanished behind a proxy. Replacement sockets carry the
// protocol-level continuation, so discard the obsolete transport immediately.
func abortQuietly(c tunnel.MessageConn) {
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
        <div class="subtle">DeskFerry Relay v` + buildinfo.Version + ` · Go WebSocket relay at <code>/relay/ws</code>. Status updates stream live over WebSocket.</div>
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
      const rooms=data.rooms||[],controls=rooms.reduce((s,r)=>s+(r.control_connections||0),0),waitingAgents=rooms.reduce((s,r)=>s+(r.waiting_agents||0),0),activePairs=rooms.reduce((s,r)=>s+(r.active_pairs||0),0),homeAgents=rooms.filter(r=>r.home_agent_connected).length,homeActiveRooms=rooms.filter(r=>r.home_agent_connected||(r.active_pairs||0)>0).length;
      setValue(workStatus,controls+waitingAgents+activePairs>0?"Connected":"Waiting",controls+waitingAgents+activePairs>0?"ok":"warn");
      workDetail.textContent=controls+' control connections, '+activePairs+' active sessions.';
      setValue(homeStatus,homeActiveRooms>0?"Active":"Waiting",homeActiveRooms>0?"ok":"warn");
      homeDetail.textContent=homeAgents+' presence socket'+(homeAgents===1?'':'s')+', '+activePairs+' active RDP stream'+(activePairs===1?'':'s')+'.';
      streamStatus.textContent=activePairs.toString();
      streamDetail.textContent=activePairs===0?'No active RDP streams.':activePairs+' RDP stream'+(activePairs===1?'':'s')+' bridged.';
      if(rooms.length===0){roomsBody.innerHTML='<tr><td colspan="5" class="subtle">No rooms have connected yet.</td></tr>';return}
      roomsBody.innerHTML=rooms.map(r=>{
        const workConnected=(r.control_connections||0)+(r.waiting_agents||0)+(r.active_pairs||0)>0,homePresence=!!r.home_agent_connected,streamActive=(r.active_pairs||0)>0,homeState=homePresence?'presence':(streamActive?'active stream':'waiting'),homeInfo=homePresence?esc(r.home_agent_remote||'')+'<br>'+esc(fmt(r.home_agent_connected_at)):(r.active_pairs||0)+' active<br>'+esc(fmt(r.last_client_connected_at));
        return '<tr><td><code>'+esc(r.id)+'</code></td><td>'+pill(workConnected,workConnected?'connected':'waiting')+'<br><span class="subtle">'+(r.control_connections||0)+' controls<br>'+esc(fmt(r.last_agent_connected_at))+'</span></td><td>'+pill(homePresence||streamActive,homeState)+'<br><span class="subtle">'+homeInfo+'</span></td><td>'+(r.active_pairs||0)+'<br><span class="subtle">'+(r.total_pairs||0)+' total</span></td><td><span class="subtle">'+esc(r.last_client_remote||'')+'<br>'+esc(fmt(r.last_client_connected_at))+'</span></td></tr>';
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
