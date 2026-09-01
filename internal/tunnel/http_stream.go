package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"deskferry/internal/buildinfo"

	"nhooyr.io/websocket"
)

const (
	HeaderHTTPStreamSecret = "X-DeskFerry-Stream-Secret"
	HeaderHTTPStreamBatch  = "X-DeskFerry-Stream-Batch"

	httpStreamRecordAck    = byte(0)
	httpStreamRecordText   = byte(1)
	httpStreamRecordBinary = byte(2)
	httpStreamRecordClose  = byte(8)
	httpStreamHeaderLen    = 13
	httpStreamMaxBuffered  = 8 * 1024 * 1024
	httpStreamKeepalive    = 10 * time.Second
	// Some managed HTTP front ends buffer a chunked request body until EOF.
	// Keep the preferred upload open while acknowledgements flow, but rotate it
	// when a sent frame remains unacknowledged so those front ends can forward
	// the completed batch.
	httpStreamUploadProbeTimeout   = 1500 * time.Millisecond
	httpStreamGracefulCloseTimeout = httpStreamUploadProbeTimeout + time.Second
	httpStreamStallTimeout         = 30 * time.Second
	httpStreamRetryWindow          = 5 * time.Minute
	httpStreamReadLimit            = 1 << 20
)

type httpStreamFrame struct {
	kind    byte
	seq     uint64
	payload []byte
}

// HTTPStreamConn carries WebSocket-equivalent text and binary messages over a
// reconnecting streaming POST/GET pair. Sequence acknowledgements make either
// half safe to replay after a proxy timeout without duplicating messages.
type HTTPStreamConn struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	cond        *sync.Cond
	wake        chan struct{}
	sendFrames  []httpStreamFrame
	sendBytes   int
	nextSend    uint64
	recvNext    uint64
	recvQueue   []httpStreamFrame
	closing     bool
	closed      bool
	terminalErr error
	lastActive  time.Time

	client     *http.Client
	endpoint   string
	headers    http.Header
	batch      bool
	streamID   string
	secret     string
	ready      chan error
	readyOnce  sync.Once
	upGen      uint64
	downGen    uint64
	downBatch  bool
	downPrimed bool
	started    sync.Once
	closeOnce  sync.Once
}

func newHTTPStreamConn(ctx context.Context) *HTTPStreamConn {
	child, cancel := context.WithCancel(ctx)
	c := &HTTPStreamConn{
		ctx:        child,
		cancel:     cancel,
		wake:       make(chan struct{}, 1),
		nextSend:   1,
		recvNext:   1,
		lastActive: time.Now(),
	}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// DialHTTPStreamWithHeaders opens the non-CONNECT fallback transport. The
// downstream GET is verified before the connection is returned; the upstream
// POST then remains open and reconnects independently for the connection's
// lifetime.
func DialHTTPStreamWithHeaders(ctx context.Context, relayAddr, proxySpec, role, token string, extraHeaders http.Header) (MessageConn, error) {
	if err := validateWebSocketRole(role); err != nil {
		return nil, err
	}
	id, err := randomHTTPStreamValue(16)
	if err != nil {
		return nil, fmt.Errorf("generate HTTP stream ID: %w", err)
	}
	secret, err := randomHTTPStreamValue(32)
	if err != nil {
		return nil, fmt.Errorf("generate HTTP stream secret: %w", err)
	}
	endpoint, err := httpStreamEndpoint(relayAddr, id)
	if err != nil {
		return nil, err
	}
	token = RelayRoomToken(relayAddr, token)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("X-DeskFerry-Role", role)
	headers.Set("X-TunnelDesktop-Role", role)
	headers.Set("User-Agent", "DeskFerry/"+buildinfo.Version)
	headers.Set(HeaderHTTPStreamSecret, secret)
	// Request finite downstream batches. A number of authenticated enterprise
	// proxies buffer an otherwise endless chunked response until it closes.
	// Sequence replay keeps these quick GET rotations lossless.
	headers.Set(HeaderHTTPStreamBatch, "1")
	for name, values := range extraHeaders {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				headers.Add(name, value)
			}
		}
	}
	// DialWebSocketWithHeaders uses ctx only to bound setup; callers commonly
	// cancel that short-lived context as soon as pairing succeeds. Match that
	// contract by detaching the established stream lifetime while preserving
	// context values. The select below still aborts setup if ctx is cancelled.
	c := newHTTPStreamConn(context.WithoutCancel(ctx))
	c.client = httpStreamHTTPClient(relayAddr, proxySpec)
	c.endpoint = endpoint
	c.headers = headers
	c.batch = true
	c.streamID = id
	c.secret = secret
	c.ready = make(chan error, 1)
	go c.clientDownLoop()
	go c.clientUpLoop()
	select {
	case err := <-c.ready:
		if err != nil {
			c.CloseNow()
			return nil, err
		}
		return c, nil
	case <-ctx.Done():
		c.CloseNow()
		return nil, ctx.Err()
	}
}

func httpStreamEndpoint(relayAddr, id string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(relayAddr))
	if err != nil {
		return "", fmt.Errorf("parse relay URL: %w", err)
	}
	if u.Host == "" {
		return "", errors.New("relay URL must include a host")
	}
	switch strings.ToLower(u.Scheme) {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported relay URL scheme %q", u.Scheme)
	}
	path := strings.TrimRight(u.Path, "/")
	path = strings.TrimSuffix(path, "/ws")
	if path == "" {
		path = "/relay"
	}
	u.Path = path + "/stream/" + url.PathEscape(id)
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func httpStreamHTTPClient(relayAddr, proxySpec string) *http.Client {
	return httpStreamHTTPClientWithAuth(relayAddr, proxySpec, newIntegratedProxyAuthenticator)
}

func httpStreamHTTPClientWithAuth(relayAddr, proxySpec string, authFactory integratedProxyAuthFactory) *http.Client {
	transport := &http.Transport{
		Proxy:                 proxyFunc(relayAddr, proxySpec),
		DialContext:           resilientDNSDialer.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, ServerName: HostFromRelayAddress(relayAddr)},
		ForceAttemptHTTP2:     false,
		DisableCompression:    true,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
	}
	// HTTPS requests still need CONNECT before their ordinary POST/GET traffic
	// can reach the relay. Use the same integrated-authentication tunnel dialer
	// as WebSocket so an NTLM proxy sees the current Windows identity instead
	// of an unauthenticated net/http CONNECT request.
	if target, err := url.Parse(strings.TrimSpace(relayAddr)); err == nil && target.Host != "" &&
		(strings.EqualFold(target.Scheme, "https") || strings.EqualFold(target.Scheme, "wss")) {
		if proxyURL, proxyErr := webSocketProxyURL(target.Host, proxySpec); proxyErr == nil && proxyURL != nil {
			transport.Proxy = nil
			transport.DialContext = proxyConnectDialContextWithAuth(proxyURL, authFactory)
		}
	}
	// A non-CONNECT proxy sees the POST and GET requests themselves. Go's
	// standard proxy support handles Basic credentials embedded in the URL but
	// does not negotiate connection-affine NTLM with the current Windows user.
	// Authenticate each forward-proxy TCP connection before handing it to the
	// standard transport so both persistent halves and every replacement
	// connection inherit the authenticated proxy session.
	if target, proxyURL, ok := httpStreamIntegratedProxy(relayAddr, proxySpec, authFactory); ok {
		transportProxy := *proxyURL
		// authenticatedForwardProxyDialContext performs TLS to an HTTPS proxy
		// itself. Present it as HTTP to net/http so it writes proxy-form requests
		// onto the already secured connection instead of wrapping TLS twice.
		transportProxy.Scheme = "http"
		transport.Proxy = http.ProxyURL(&transportProxy)
		transport.DialContext = authenticatedForwardProxyDialContext(proxyURL, target, authFactory)
	}
	return &http.Client{Transport: transport}
}

func httpStreamIntegratedProxy(relayAddr, proxySpec string, authFactory integratedProxyAuthFactory) (*url.URL, *url.URL, bool) {
	target, err := url.Parse(strings.TrimSpace(relayAddr))
	if err != nil || target.Host == "" {
		return nil, nil, false
	}
	switch strings.ToLower(target.Scheme) {
	case "ws":
		target.Scheme = "http"
	case "http":
	case "wss", "https":
		// HTTPS is configured above with the authenticated CONNECT dialer. This
		// helper is only for plain forward-proxy requests.
		return nil, nil, false
	default:
		return nil, nil, false
	}
	// Use the bounded health response for the authentication exchange. Some
	// enterprise proxies wait indefinitely while forwarding HEAD, redirects,
	// or dynamically generated room dashboards; the health GET always has an
	// explicit finite body that can be drained before reusing the connection.
	target.Path = "/relay/health"
	target.RawQuery = ""
	target.Fragment = ""

	var proxyURL *url.URL
	spec := strings.TrimSpace(proxySpec)
	if spec == "" || strings.EqualFold(spec, "direct") {
		return nil, nil, false
	}
	if strings.EqualFold(spec, "env") || strings.EqualFold(spec, "auto") {
		proxyURL, err = http.ProxyFromEnvironment(&http.Request{URL: target})
	} else {
		proxyURL, err = resolveProxyURL(target.Host, spec)
	}
	if err != nil || proxyURL == nil || proxyURL.User != nil || authFactory == nil {
		return nil, nil, false
	}
	// Avoid changing the request path on platforms without an integrated
	// authenticator. Each actual connection receives a fresh NTLM context.
	auth, err := authFactory()
	if err != nil || auth == nil {
		return nil, nil, false
	}
	_ = auth.Close()
	return target, proxyURL, true
}

func authenticatedForwardProxyDialContext(proxyURL, target *url.URL, authFactory integratedProxyAuthFactory) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		conn, err := resilientDNSDialer.DialContext(ctx, network, canonicalProxyAddr(proxyURL))
		if err != nil {
			return nil, err
		}
		if proxyURL.Scheme == "https" {
			tlsConn := tls.Client(conn, &tls.Config{
				MinVersion: tls.VersionTLS12,
				ServerName: proxyURL.Hostname(),
			})
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("TLS handshake with proxy %s failed: %w", proxyURLForLog(proxyURL), err)
			}
			conn = tlsConn
		}
		stopCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stopCancellation()
		authDeadline := time.Now().Add(20 * time.Second)
		if deadline, ok := ctx.Deadline(); ok && deadline.Before(authDeadline) {
			authDeadline = deadline
		}
		_ = conn.SetDeadline(authDeadline)

		auth, err := authFactory()
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("initialize Windows authentication for proxy %s: %w", proxyURLForLog(proxyURL), err)
		}
		if auth == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("integrated authentication is unavailable for proxy %s", proxyURLForLog(proxyURL))
		}
		defer auth.Close()

		reader := bufio.NewReader(conn)
		authorization := proxyAuthorization(auth.Scheme(), auth.InitialToken())
		for attempt := 0; attempt < 2; attempt++ {
			probe, err := writeForwardProxyProbe(conn, target, authorization)
			if err != nil {
				_ = conn.Close()
				return nil, err
			}
			resp, err := http.ReadResponse(reader, probe)
			if err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("read proxy authentication response from %s: %w", proxyURLForLog(proxyURL), err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusProxyAuthRequired {
				if resp.Close {
					_ = conn.Close()
					return nil, fmt.Errorf("proxy %s closed the authenticated connection", proxyURLForLog(proxyURL))
				}
				_ = conn.SetDeadline(time.Time{})
				return &bufferedConn{Conn: conn, reader: reader}, nil
			}
			if attempt > 0 {
				_ = conn.Close()
				return nil, fmt.Errorf("proxy %s rejected Windows authentication", proxyURLForLog(proxyURL))
			}
			challenge, ok := proxyAuthenticationChallenge(resp.Header, auth.Scheme())
			if !ok {
				_ = conn.Close()
				return nil, fmt.Errorf("proxy %s requested unsupported authentication", proxyURLForLog(proxyURL))
			}
			nextToken, err := auth.NextToken(challenge)
			if err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("authenticate proxy %s with Windows credentials: %w", proxyURLForLog(proxyURL), err)
			}
			authorization = proxyAuthorization(auth.Scheme(), nextToken)
		}
		_ = conn.Close()
		return nil, fmt.Errorf("proxy %s authentication did not complete", proxyURLForLog(proxyURL))
	}
}

func writeForwardProxyProbe(conn net.Conn, target *url.URL, authorization string) (*http.Request, error) {
	probeURL := *target
	probeURL.User = nil
	probeURL.Fragment = ""
	probe := &http.Request{
		Method: http.MethodGet,
		URL:    &probeURL,
		Host:   probeURL.Host,
		Header: http.Header{
			"User-Agent":       {"DeskFerry/" + buildinfo.Version},
			"Connection":       {"keep-alive"},
			"Proxy-Connection": {"Keep-Alive"},
		},
	}
	if authorization != "" {
		probe.Header.Set("Proxy-Authorization", authorization)
	}
	if err := probe.WriteProxy(conn); err != nil {
		return nil, fmt.Errorf("write proxy authentication request: %w", err)
	}
	return probe, nil
}

func (c *HTTPStreamConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			c.cond.Broadcast()
			c.mu.Unlock()
		case <-done:
		}
	}()
	defer close(done)
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.recvQueue) == 0 && !c.closed && ctx.Err() == nil {
		c.cond.Wait()
	}
	if ctx.Err() != nil {
		return 0, nil, ctx.Err()
	}
	if len(c.recvQueue) == 0 {
		if c.terminalErr != nil {
			return 0, nil, c.terminalErr
		}
		return 0, nil, netClosedError()
	}
	frame := c.recvQueue[0]
	c.recvQueue = c.recvQueue[1:]
	switch frame.kind {
	case httpStreamRecordText:
		return websocket.MessageText, frame.payload, nil
	case httpStreamRecordBinary:
		return websocket.MessageBinary, frame.payload, nil
	case httpStreamRecordClose:
		code, reason := decodeHTTPStreamClose(frame.payload)
		return 0, nil, websocket.CloseError{Code: code, Reason: reason}
	default:
		return 0, nil, errors.New("invalid HTTP stream message type")
	}
}

func (c *HTTPStreamConn) Write(ctx context.Context, typ websocket.MessageType, payload []byte) error {
	kind := byte(0)
	switch typ {
	case websocket.MessageText:
		kind = httpStreamRecordText
	case websocket.MessageBinary:
		kind = httpStreamRecordBinary
	default:
		return fmt.Errorf("unsupported HTTP stream message type %d", typ)
	}
	if len(payload) > httpStreamReadLimit {
		return fmt.Errorf("HTTP stream message exceeds %d bytes", httpStreamReadLimit)
	}
	return c.queueFrame(ctx, kind, append([]byte(nil), payload...))
}

func (c *HTTPStreamConn) queueFrame(ctx context.Context, kind byte, payload []byte) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			c.cond.Broadcast()
			c.mu.Unlock()
		case <-done:
		}
	}()
	defer close(done)
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.sendBytes+len(payload) > httpStreamMaxBuffered && !c.closed && ctx.Err() == nil {
		c.cond.Wait()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if c.closed || c.closing {
		return netClosedError()
	}
	c.sendFrames = append(c.sendFrames, httpStreamFrame{kind: kind, seq: c.nextSend, payload: payload})
	c.nextSend++
	c.sendBytes += len(payload)
	c.signalLocked()
	return nil
}

func (c *HTTPStreamConn) Close(code websocket.StatusCode, reason string) error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		closeSequence := uint64(0)
		if !c.closed {
			c.closing = true
			payload := encodeHTTPStreamClose(code, reason)
			closeSequence = c.nextSend
			c.sendFrames = append(c.sendFrames, httpStreamFrame{kind: httpStreamRecordClose, seq: closeSequence, payload: payload})
			c.nextSend++
			c.sendBytes += len(payload)
			c.signalLocked()
		}
		c.mu.Unlock()
		if closeSequence != 0 {
			deadline := time.Now().Add(httpStreamGracefulCloseTimeout)
			timer := time.AfterFunc(httpStreamGracefulCloseTimeout, func() {
				c.mu.Lock()
				c.cond.Broadcast()
				c.mu.Unlock()
			})
			c.mu.Lock()
			for !c.closed && !c.sendSequenceAcknowledgedLocked(closeSequence) && time.Now().Before(deadline) {
				c.cond.Wait()
			}
			c.mu.Unlock()
			timer.Stop()
		}
		c.closeTerminal(nil)
	})
	return nil
}

func (c *HTTPStreamConn) CloseNow() error {
	c.closeTerminal(netClosedError())
	return nil
}

func (c *HTTPStreamConn) closeTerminal(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if err != nil {
		c.terminalErr = err
	}
	c.cond.Broadcast()
	c.signalLocked()
	c.mu.Unlock()
	c.cancel()
}

func (c *HTTPStreamConn) applyRecord(frame httpStreamFrame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastActive = time.Now()
	if frame.kind == httpStreamRecordAck {
		if frame.seq >= c.nextSend {
			return errors.New("HTTP stream acknowledgement exceeds sent sequence")
		}
		for len(c.sendFrames) > 0 && c.sendFrames[0].seq <= frame.seq {
			c.sendBytes -= len(c.sendFrames[0].payload)
			c.sendFrames = c.sendFrames[1:]
		}
		c.cond.Broadcast()
		return nil
	}
	if frame.seq < c.recvNext {
		c.signalLocked()
		return nil
	}
	if frame.seq != c.recvNext {
		return fmt.Errorf("HTTP stream received sequence %d after %d", frame.seq, c.recvNext-1)
	}
	if frame.kind != httpStreamRecordText && frame.kind != httpStreamRecordBinary && frame.kind != httpStreamRecordClose {
		return fmt.Errorf("HTTP stream received invalid record type %d", frame.kind)
	}
	c.recvNext++
	c.recvQueue = append(c.recvQueue, frame)
	c.cond.Broadcast()
	c.signalLocked()
	return nil
}

func (c *HTTPStreamConn) signalLocked() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *HTTPStreamConn) snapshotAfter(lastSeq uint64) ([]httpStreamFrame, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	frames := make([]httpStreamFrame, 0, len(c.sendFrames))
	for _, frame := range c.sendFrames {
		if frame.seq > lastSeq {
			frames = append(frames, frame)
		}
	}
	ack := c.recvNext - 1
	return frames, ack
}

func (c *HTTPStreamConn) clientUpLoop() {
	backoff := 250 * time.Millisecond
	deliveredAck := uint64(0)
	deliveredSequence := uint64(0)
	for c.ctx.Err() == nil {
		attemptCtx, cancel := context.WithCancel(c.ctx)
		reader, writer := io.Pipe()
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.endpoint+"/up", reader)
		if err != nil {
			cancel()
			c.closeTerminal(err)
			return
		}
		req.Header = c.headers.Clone()
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Cache-Control", "no-store")
		req.Header.Set("Expect", "100-continue")
		result := make(chan error, 1)
		go func() {
			resp, err := c.client.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					err = fmt.Errorf("HTTP stream POST failed: HTTP %s", resp.Status)
				} else {
					err = io.EOF
				}
			}
			result <- err
		}()
		type uploadWriteResult struct {
			ack      uint64
			sequence uint64
			err      error
		}
		writeDone := make(chan uploadWriteResult, 1)
		go func() {
			ack, sequence, writeErr := c.writeUpload(attemptCtx, writer, deliveredAck, deliveredSequence)
			writeDone <- uploadWriteResult{ack: ack, sequence: sequence, err: writeErr}
		}()
		var attemptErr error
		normalRotation := false
		attemptAck := deliveredAck
		attemptSequence := deliveredSequence
		select {
		case attemptErr = <-result:
		case writeResult := <-writeDone:
			attemptAck = writeResult.ack
			attemptSequence = writeResult.sequence
			if writeResult.err != nil {
				_ = writer.CloseWithError(writeResult.err)
			}
			attemptErr = <-result
			normalRotation = writeResult.err == nil && errors.Is(attemptErr, io.EOF)
		case <-c.ctx.Done():
			cancel()
			_ = writer.CloseWithError(c.ctx.Err())
			return
		}
		cancel()
		_ = writer.CloseWithError(attemptErr)
		if c.ctx.Err() != nil {
			return
		}
		if normalRotation {
			if c.batch {
				deliveredAck = attemptAck
				deliveredSequence = attemptSequence
			}
			backoff = 250 * time.Millisecond
			if !sleepHTTPStream(c.ctx, 25*time.Millisecond) {
				return
			}
			continue
		}
		if !sleepHTTPStream(c.ctx, backoff) {
			return
		}
		backoff = nextHTTPStreamBackoff(backoff)
	}
}

func (c *HTTPStreamConn) writeUpload(ctx context.Context, writer *io.PipeWriter, deliveredAck, deliveredSequence uint64) (uint64, uint64, error) {
	if c.batch {
		return c.writeUploadBatch(ctx, writer, deliveredAck, deliveredSequence)
	}
	return deliveredAck, deliveredSequence, c.writePersistentUpload(ctx, writer)
}

func (c *HTTPStreamConn) writeUploadBatch(ctx context.Context, writer *io.PipeWriter, deliveredAck, deliveredSequence uint64) (uint64, uint64, error) {
	defer writer.Close()
	ticker := time.NewTicker(httpStreamKeepalive)
	defer ticker.Stop()
	for {
		frames, ack := c.snapshotAfter(deliveredSequence)
		if ack > deliveredAck || len(frames) > 0 {
			if err := writeHTTPStreamRecord(writer, httpStreamFrame{kind: httpStreamRecordAck, seq: ack}); err != nil {
				return deliveredAck, deliveredSequence, err
			}
			sequence := deliveredSequence
			for _, frame := range frames {
				if err := writeHTTPStreamRecord(writer, frame); err != nil {
					return deliveredAck, deliveredSequence, err
				}
				sequence = frame.seq
			}
			return ack, sequence, nil
		}
		select {
		case <-ctx.Done():
			return deliveredAck, deliveredSequence, ctx.Err()
		case <-c.wake:
		case <-ticker.C:
			if err := writeHTTPStreamRecord(writer, httpStreamFrame{kind: httpStreamRecordAck, seq: ack}); err != nil {
				return deliveredAck, deliveredSequence, err
			}
			return ack, deliveredSequence, nil
		}
	}
}

func (c *HTTPStreamConn) writePersistentUpload(ctx context.Context, writer *io.PipeWriter) error {
	defer writer.Close()
	lastSeq, lastAck := uint64(0), uint64(0)
	forceAck := true
	unacknowledgedSince := time.Time{}
	ticker := time.NewTicker(httpStreamKeepalive)
	defer ticker.Stop()
	for {
		frames, ack := c.snapshotAfter(lastSeq)
		if forceAck || ack > lastAck {
			if err := writeHTTPStreamRecord(writer, httpStreamFrame{kind: httpStreamRecordAck, seq: ack}); err != nil {
				return err
			}
			lastAck = ack
			forceAck = false
		}
		for _, frame := range frames {
			if err := writeHTTPStreamRecord(writer, frame); err != nil {
				return err
			}
			lastSeq = frame.seq
		}
		if lastSeq > 0 && !c.sendSequenceAcknowledged(lastSeq) {
			if unacknowledgedSince.IsZero() {
				unacknowledgedSince = time.Now()
			}
		} else {
			unacknowledgedSince = time.Time{}
		}
		var rotate <-chan time.Time
		var rotateTimer *time.Timer
		if !unacknowledgedSince.IsZero() {
			remaining := httpStreamUploadProbeTimeout - time.Since(unacknowledgedSince)
			if remaining <= 0 {
				return nil
			}
			rotateTimer = time.NewTimer(remaining)
			rotate = rotateTimer.C
		}
		select {
		case <-ctx.Done():
			if rotateTimer != nil {
				rotateTimer.Stop()
			}
			return ctx.Err()
		case <-c.wake:
		case <-ticker.C:
			forceAck = true
		case <-rotate:
			return nil
		}
		if rotateTimer != nil {
			rotateTimer.Stop()
		}
	}
}

func (c *HTTPStreamConn) sendSequenceAcknowledged(sequence uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendSequenceAcknowledgedLocked(sequence)
}

func (c *HTTPStreamConn) sendSequenceAcknowledgedLocked(sequence uint64) bool {
	return len(c.sendFrames) == 0 || c.sendFrames[0].seq > sequence
}

func (c *HTTPStreamConn) clientDownLoop() {
	backoff := 250 * time.Millisecond
	lostAt := time.Time{}
	first := true
	for c.ctx.Err() == nil {
		attemptCtx, cancel := context.WithCancel(c.ctx)
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, c.endpoint+"/down", nil)
		if err != nil {
			cancel()
			c.readyOnce.Do(func() { c.ready <- err })
			c.closeTerminal(err)
			return
		}
		req.Header = c.headers.Clone()
		req.Header.Set("Accept", "application/octet-stream")
		resp, err := c.client.Do(req)
		if err == nil && resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			err = fmt.Errorf("HTTP stream GET failed: HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		if err == nil {
			if first {
				c.readyOnce.Do(func() { c.ready <- nil })
				first = false
			}
			lostAt = time.Time{}
			backoff = 250 * time.Millisecond
			err = c.readDownload(attemptCtx, cancel, resp.Body)
			_ = resp.Body.Close()
		}
		cancel()
		if c.ctx.Err() != nil {
			return
		}
		if first && isPermanentHTTPStreamDialError(err) {
			c.readyOnce.Do(func() { c.ready <- err })
			c.closeTerminal(err)
			return
		}
		if lostAt.IsZero() {
			lostAt = time.Now()
		}
		if time.Since(lostAt) >= httpStreamRetryWindow {
			if first {
				c.readyOnce.Do(func() { c.ready <- err })
			}
			c.closeTerminal(fmt.Errorf("HTTP stream downstream could not recover within %s: %w", httpStreamRetryWindow, err))
			return
		}
		if !sleepHTTPStream(c.ctx, backoff) {
			return
		}
		backoff = nextHTTPStreamBackoff(backoff)
	}
}

func (c *HTTPStreamConn) readDownload(ctx context.Context, cancel context.CancelFunc, body io.Reader) error {
	activity := make(chan struct{}, 1)
	readErr := make(chan error, 1)
	go func() {
		for {
			frame, err := readHTTPStreamRecord(body)
			if err != nil {
				readErr <- err
				return
			}
			if err := c.applyRecord(frame); err != nil {
				readErr <- err
				return
			}
			select {
			case activity <- struct{}{}:
			default:
			}
		}
	}()
	timer := time.NewTimer(httpStreamStallTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(httpStreamStallTimeout)
		case <-timer.C:
			cancel()
			return errors.New("HTTP stream downstream stalled")
		}
	}
}

func isPermanentHTTPStreamDialError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "http 400") || strings.Contains(text, "http 401") || strings.Contains(text, "http 403") || strings.Contains(text, "http 404") || strings.Contains(text, "http 405") ||
		strings.Contains(text, "proxy authentication") || strings.Contains(text, "proxy requested unsupported authentication")
}

// HTTPStreamServer owns logical HTTP-stream connections and lets a relay use
// them through the same MessageConn interface as accepted WebSockets.
type HTTPStreamServer struct {
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	streams  map[string]*HTTPStreamConn
	onAccept func(context.Context, MessageConn, *http.Request, string)
}

func NewHTTPStreamServer(onAccept func(context.Context, MessageConn, *http.Request, string)) *HTTPStreamServer {
	ctx, cancel := context.WithCancel(context.Background())
	s := &HTTPStreamServer{ctx: ctx, cancel: cancel, streams: make(map[string]*HTTPStreamConn), onAccept: onAccept}
	go s.sweep()
	return s
}

func (s *HTTPStreamServer) Close() {
	s.cancel()
	s.mu.Lock()
	streams := make([]*HTTPStreamConn, 0, len(s.streams))
	for _, stream := range s.streams {
		streams = append(streams, stream)
	}
	s.streams = make(map[string]*HTTPStreamConn)
	s.mu.Unlock()
	for _, stream := range streams {
		stream.CloseNow()
	}
}

func (s *HTTPStreamServer) Serve(w http.ResponseWriter, r *http.Request, room, id, direction string) {
	if len(id) < 16 || len(id) > 128 || (direction != "up" && direction != "down") {
		http.Error(w, "invalid HTTP stream path", http.StatusBadRequest)
		return
	}
	secret := strings.TrimSpace(r.Header.Get(HeaderHTTPStreamSecret))
	if len(secret) < 24 || len(secret) > 256 {
		http.Error(w, "missing HTTP stream secret", http.StatusUnauthorized)
		return
	}
	key := strings.ToLower(room) + "/" + id
	s.mu.Lock()
	c := s.streams[key]
	created := false
	if c == nil {
		if len(s.streams) >= 4096 {
			s.mu.Unlock()
			http.Error(w, "HTTP stream capacity reached", http.StatusServiceUnavailable)
			return
		}
		c = newHTTPStreamConn(s.ctx)
		c.streamID = id
		c.secret = secret
		s.streams[key] = c
		created = true
	}
	s.mu.Unlock()
	if c.secret != secret {
		http.Error(w, "HTTP stream secret mismatch", http.StatusForbidden)
		return
	}
	if value := strings.TrimSpace(r.Header.Get(HeaderHTTPStreamBatch)); value == "1" || strings.EqualFold(value, "true") {
		c.mu.Lock()
		c.downBatch = true
		c.mu.Unlock()
	}
	if created {
		request := r.Clone(c.ctx)
		go s.onAccept(c.ctx, c, request, room)
	}
	if direction == "up" {
		c.serveUpload(w, r)
	} else {
		c.serveDownload(w, r)
	}
}

func (c *HTTPStreamConn) serveUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "HTTP stream upstream requires POST", http.StatusMethodNotAllowed)
		return
	}
	c.mu.Lock()
	c.upGen++
	generation := c.upGen
	c.lastActive = time.Now()
	c.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	for {
		frame, err := readHTTPStreamRecord(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		c.mu.Lock()
		current := c.upGen == generation && !c.closed
		c.mu.Unlock()
		if !current {
			return
		}
		if err := c.applyRecord(frame); err != nil {
			c.closeTerminal(err)
			return
		}
	}
}

func (c *HTTPStreamConn) serveDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "HTTP stream downstream requires GET", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming responses are unavailable", http.StatusInternalServerError)
		return
	}
	c.mu.Lock()
	c.downGen++
	generation := c.downGen
	c.lastActive = time.Now()
	batchMode := c.downBatch
	primeBatch := batchMode && !c.downPrimed
	if primeBatch {
		c.downPrimed = true
	}
	c.mu.Unlock()
	if batchMode {
		c.serveDownloadBatch(w, r, generation, primeBatch)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	lastSeq, lastAck := uint64(0), uint64(0)
	forceAck := true
	unacknowledgedSince := time.Time{}
	ticker := time.NewTicker(httpStreamKeepalive)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		current := c.downGen == generation && !c.closed
		c.mu.Unlock()
		if !current {
			return
		}
		frames, ack := c.snapshotAfter(lastSeq)
		if forceAck || ack > lastAck {
			if err := writeHTTPStreamRecord(w, httpStreamFrame{kind: httpStreamRecordAck, seq: ack}); err != nil {
				return
			}
			lastAck = ack
			forceAck = false
		}
		for _, frame := range frames {
			if err := writeHTTPStreamRecord(w, frame); err != nil {
				return
			}
			lastSeq = frame.seq
		}
		flusher.Flush()
		if lastSeq > 0 && !c.sendSequenceAcknowledged(lastSeq) {
			if unacknowledgedSince.IsZero() {
				unacknowledgedSince = time.Now()
			}
		} else {
			unacknowledgedSince = time.Time{}
		}
		var rotate <-chan time.Time
		var rotateTimer *time.Timer
		if !unacknowledgedSince.IsZero() {
			remaining := httpStreamUploadProbeTimeout - time.Since(unacknowledgedSince)
			if remaining <= 0 {
				c.mu.Lock()
				if c.downGen == generation && !c.closed {
					c.downBatch = true
				}
				c.mu.Unlock()
				return
			}
			rotateTimer = time.NewTimer(remaining)
			rotate = rotateTimer.C
		}
		select {
		case <-r.Context().Done():
			if rotateTimer != nil {
				rotateTimer.Stop()
			}
			return
		case <-c.ctx.Done():
			if rotateTimer != nil {
				rotateTimer.Stop()
			}
			return
		case <-c.wake:
		case <-ticker.C:
			forceAck = true
		case <-rotate:
			c.mu.Lock()
			if c.downGen == generation && !c.closed {
				c.downBatch = true
			}
			c.mu.Unlock()
			return
		}
		if rotateTimer != nil {
			rotateTimer.Stop()
		}
	}
}

func (c *HTTPStreamConn) serveDownloadBatch(w http.ResponseWriter, r *http.Request, generation uint64, prime bool) {
	timer := time.NewTimer(httpStreamKeepalive)
	defer timer.Stop()
	for {
		c.mu.Lock()
		current := c.downGen == generation && !c.closed
		c.mu.Unlock()
		if !current {
			return
		}
		frames, ack := c.snapshotAfter(0)
		if prime || len(frames) > 0 {
			c.writeDownloadBatch(w, ack, frames)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-c.ctx.Done():
			return
		case <-c.wake:
		case <-timer.C:
			c.writeDownloadBatch(w, ack, nil)
			return
		}
	}
}

func (c *HTTPStreamConn) writeDownloadBatch(w http.ResponseWriter, ack uint64, frames []httpStreamFrame) {
	var payload bytes.Buffer
	if err := writeHTTPStreamRecord(&payload, httpStreamFrame{kind: httpStreamRecordAck, seq: ack}); err != nil {
		return
	}
	for _, frame := range frames {
		if err := writeHTTPStreamRecord(&payload, frame); err != nil {
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("Content-Length", strconv.Itoa(payload.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload.Bytes())
}

func (s *HTTPStreamServer) sweep() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		cutoff := time.Now().Add(-httpStreamRetryWindow)
		s.mu.Lock()
		for key, stream := range s.streams {
			stream.mu.Lock()
			expired := stream.closed || stream.lastActive.Before(cutoff)
			stream.mu.Unlock()
			if expired {
				delete(s.streams, key)
				stream.CloseNow()
			}
		}
		s.mu.Unlock()
	}
}

func readHTTPStreamRecord(r io.Reader) (httpStreamFrame, error) {
	header := make([]byte, httpStreamHeaderLen)
	if _, err := io.ReadFull(r, header); err != nil {
		return httpStreamFrame{}, err
	}
	length := binary.BigEndian.Uint32(header[9:13])
	if length > httpStreamReadLimit {
		return httpStreamFrame{}, fmt.Errorf("HTTP stream record length %d exceeds limit", length)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return httpStreamFrame{}, err
	}
	return httpStreamFrame{kind: header[0], seq: binary.BigEndian.Uint64(header[1:9]), payload: payload}, nil
}

func writeHTTPStreamRecord(w io.Writer, frame httpStreamFrame) error {
	if len(frame.payload) > httpStreamReadLimit {
		return fmt.Errorf("HTTP stream record exceeds %d bytes", httpStreamReadLimit)
	}
	header := make([]byte, httpStreamHeaderLen)
	header[0] = frame.kind
	binary.BigEndian.PutUint64(header[1:9], frame.seq)
	binary.BigEndian.PutUint32(header[9:13], uint32(len(frame.payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(frame.payload) > 0 {
		_, err := w.Write(frame.payload)
		return err
	}
	return nil
}

func encodeHTTPStreamClose(code websocket.StatusCode, reason string) []byte {
	if len(reason) > 123 {
		reason = reason[:123]
	}
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], uint16(code))
	copy(payload[2:], reason)
	return payload
}

func decodeHTTPStreamClose(payload []byte) (websocket.StatusCode, string) {
	if len(payload) < 2 {
		return websocket.StatusNormalClosure, ""
	}
	return websocket.StatusCode(binary.BigEndian.Uint16(payload[:2])), string(payload[2:])
}

func randomHTTPStreamValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func sleepHTTPStream(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextHTTPStreamBackoff(current time.Duration) time.Duration {
	current *= 2
	if current > 5*time.Second {
		return 5 * time.Second
	}
	return current
}

func netClosedError() error { return net.ErrClosed }
