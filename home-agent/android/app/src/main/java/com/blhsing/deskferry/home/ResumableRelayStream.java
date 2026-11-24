package com.blhsing.deskferry.home;

import android.os.SystemClock;

import org.json.JSONObject;

import java.io.IOException;
import java.nio.ByteBuffer;
import java.util.Arrays;
import java.util.List;
import java.util.Locale;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicReference;

import okhttp3.Request;
import okhttp3.Response;
import okhttp3.WebSocket;
import okhttp3.WebSocketListener;
import okio.ByteString;

/**
 * One relay session for a Home service (RDP, SMB or the live screen): pairs
 * through the first reachable relay, then hides transport drops by resuming
 * the same session over a new WebSocket, replaying unacknowledged data and
 * checking the path end to end with heartbeats. It is the Android counterpart
 * of the Go resumable WebSocket connection and uses the same frames: type 1
 * data, 2 acknowledgement, 3 heartbeat, 4 heartbeat response, each with a
 * 64-bit offset.
 */
final class ResumableRelayStream {
    /** Relay access: where to connect and how. */
    interface Host {
        List<String> relayUrls();

        /** A request for role "client" or "resume"; the stream sets the service and session headers. */
        Request request(String relayUrl, String role) throws Exception;

        WebSocket open(Request request, WebSocketListener listener);

        void log(String line);
    }

    /** Receives data from the Work side, in order, on the stream's delivery thread. */
    interface Sink {
        void deliver(byte[] data) throws IOException;
    }

    /** Learns, once, that the stream closed and why. */
    interface CloseListener {
        void closed(String termination);
    }

    /** The relay's session-ready result. */
    static final class SessionReady {
        String type = "invalid-request";
        String reason = "";
        String service = "";
        String sessionId = "";
        boolean heartbeat;
    }

    interface ReadyParser {
        SessionReady parse(String json) throws Exception;
    }

    /** Parses session-ready results; unit tests replace it, as org.json is a stub on the JVM. */
    static ReadyParser readyParser = json -> {
        JSONObject message = new JSONObject(json);
        SessionReady ready = new SessionReady();
        ready.type = message.optString("type", "invalid-request");
        ready.reason = message.optString("reason", "");
        ready.service = message.optString("service", "").trim();
        ready.sessionId = message.optString("session_id", "").trim();
        ready.heartbeat = message.optBoolean("heartbeat", false);
        return ready;
    };

    final String service;
    private final String serviceLabel;
    private final boolean requireServiceEcho;
    private final Host host;
    private final Sink sink;
    private final CloseListener closeListener;

    private final AtomicBoolean closed = new AtomicBoolean(false);
    private final AtomicLong localToRelayBytes = new AtomicLong();
    private final AtomicLong localToRelayMessages = new AtomicLong();
    private final AtomicLong relayToLocalBytes = new AtomicLong();
    private final AtomicLong relayToLocalMessages = new AtomicLong();
    private final AtomicLong pendingLocalBytes = new AtomicLong();
    private final AtomicReference<String> termination = new AtomicReference<>("running");
    private final AtomicBoolean reconnecting = new AtomicBoolean(false);
    private final Object resumeLock = new Object();
    private final Object receiveLock = new Object();
    private final LinkedBlockingQueue<ReceivedChunk> localWriteQueue = new LinkedBlockingQueue<>();
    private volatile WebSocket webSocket;
    private volatile WebSocket pendingResumeSocket;
    private volatile CountDownLatch pendingResumeReady;
    private volatile Thread localWriterThread;
    private volatile String selectedRelay = "none";
    private volatile String sessionId = "";
    private byte[] sendBuffer = new byte[0];
    private long sendBase;
    private long sendEnd;
    private long receiveOffset;
    private long receiveQueuedOffset;
    private boolean heartbeatEnabled;
    private long transportGeneration;
    private long heartbeatNonce;
    private long heartbeatAcknowledged;
    private long lastAckProgressMs;
    private boolean ackStallLogged;
    private long ackStallAtMs;
    private long lastTransportLostMs;

    ResumableRelayStream(Host host, String service, Sink sink, CloseListener closeListener) {
        this.host = host;
        this.service = service;
        this.serviceLabel = service.toUpperCase(Locale.ROOT);
        // Only RDP and SMB predate service echoes; any other service must be
        // confirmed so an older relay cannot map it to RDP.
        this.requireServiceEcho = !"rdp".equals(service) && !"smb".equals(service);
        this.sink = sink;
        this.closeListener = closeListener;
        startLocalWriter();
        startAckMonitor();
    }

    String selectedRelay() {
        return selectedRelay;
    }

    String sessionId() {
        return sessionId;
    }

    String termination() {
        return termination.get();
    }

    boolean isClosed() {
        return closed.get();
    }

    String stats() {
        return "local_to_relay_bytes=" + localToRelayBytes.get() + " local_to_relay_messages=" + localToRelayMessages.get()
                + " relay_to_local_bytes=" + relayToLocalBytes.get() + " relay_to_local_messages=" + relayToLocalMessages.get();
    }

    void recordTermination(String cause) {
        termination.compareAndSet("running", cause);
    }

    private void append(String line) {
        host.log(line);
    }

    private final class ReceivedChunk {
        final long offset;
        final byte[] data;
        final boolean resumable;

        ReceivedChunk(long offset, byte[] data, boolean resumable) {
            this.offset = offset;
            this.data = data;
            this.resumable = resumable;
        }
    }

    private void startAckMonitor() {
        new Thread(() -> {
            while (!closed.get()) {
                TunnelService.sleepQuietly(1000);
                long now = SystemClock.elapsedRealtime();
                long pending;
                long generation;
                long noProgress;
                WebSocket stalledSocket;
                boolean shouldLog;
                synchronized (resumeLock) {
                    pending = sendEnd - sendBase;
                    noProgress = now - lastAckProgressMs;
                    if (pending == 0 || webSocket == null || noProgress < TunnelService.DIAGNOSTIC_STALL_MS) {
                        continue;
                    }
                    stalledSocket = webSocket;
                    shouldLog = !ackStallLogged;
                    if (shouldLog) {
                        ackStallLogged = true;
                        ackStallAtMs = now;
                    }
                    generation = transportGeneration;
                }
                if (shouldLog) {
                    append(serviceLabel + " relay acknowledgements stalled session=" + sessionId + " relay=" + selectedRelay
                            + " generation=" + generation + " pending_bytes=" + pending + " no_progress_ms=" + noProgress + ".");
                }
                long recoveryDeadline = TunnelService.DEFAULT_ACK_RECOVERY_MS;
                if ("rdp".equals(service)) {
                    recoveryDeadline = "http-stream".equals(relayProtocol(stalledSocket))
                            ? TunnelService.RDP_HTTP_STREAM_ACK_RECOVERY_MS : TunnelService.RDP_ACK_RECOVERY_MS;
                }
                if (noProgress >= recoveryDeadline) {
                    markTransportLost(stalledSocket, "data acknowledgements made no progress for " + noProgress + "ms");
                }
            }
        }, "DeskFerry-" + serviceLabel + "-AckMonitor").start();
    }

    private void startLocalWriter() {
        Thread writer = new Thread(() -> {
            while (!closed.get()) {
                ReceivedChunk chunk;
                try {
                    chunk = localWriteQueue.take();
                } catch (InterruptedException interrupted) {
                    if (!closed.get()) {
                        Thread.currentThread().interrupt();
                    }
                    return;
                }
                try {
                    sink.deliver(chunk.data);
                    relayToLocalBytes.addAndGet(chunk.data.length);
                    relayToLocalMessages.incrementAndGet();
                    if (chunk.resumable) {
                        long acknowledgedOffset;
                        synchronized (receiveLock) {
                            if (chunk.offset != receiveOffset) {
                                throw new IOException("out-of-order local delivery");
                            }
                            receiveOffset += chunk.data.length;
                            acknowledgedOffset = receiveOffset;
                        }
                        acknowledgeLocalDelivery(acknowledgedOffset);
                    }
                } catch (IOException ex) {
                    if (!closed.get()) {
                        recordTermination("relay_to_local_write_error=" + TunnelService.throwableText(ex));
                        append(serviceLabel + " local delivery failed: " + TunnelService.throwableText(ex));
                        close();
                    }
                    return;
                } finally {
                    pendingLocalBytes.addAndGet(-chunk.data.length);
                }
            }
        }, "DeskFerry-" + serviceLabel + "-LocalWriter");
        localWriterThread = writer;
        writer.start();
    }

    /**
     * Pairs through the first relay that accepts the session and returns it.
     * Throws when every relay failed, or at once when a relay rejected the
     * session for good (wrong room password, service turned off).
     */
    String connect() throws Exception {
        String lastError = "";
        for (String candidate : host.relayUrls()) {
            if (closed.get()) {
                break;
            }
            try {
                long dialStarted = SystemClock.elapsedRealtime();
                connectRelay(candidate);
                selectedRelay = candidate;
                append("Relay attempt selected relay=" + candidate + " service=" + service + " protocol_v2=true elapsed_ms="
                        + TunnelService.elapsedMillis(dialStarted) + ".");
                return candidate;
            } catch (Exception ex) {
                lastError = candidate + ": " + ex.getMessage();
                closeWebSocketOnly();
                if (!closed.get()) {
                    append("Relay attempt failed relay=" + candidate + " service=" + service + " result=" + relayAttemptResult(ex)
                            + " error=" + ex.getMessage() + ".");
                }
                if (ex instanceof SessionRejectedException && ((SessionRejectedException) ex).terminal()) {
                    throw ex;
                }
            }
        }
        recordTermination("all_relay_urls_failed error=" + TunnelService.emptyAs(lastError, "unknown"));
        throw new IOException(TunnelService.emptyAs(lastError, "all relay URLs failed"));
    }

    private Request serviceRequest(String relayUrl, String role) throws Exception {
        Request.Builder builder = host.request(relayUrl, role).newBuilder().header("X-DeskFerry-Service", service);
        if ("client".equals(role)) {
            builder.header("X-DeskFerry-Resumable", "1")
                    .header("X-DeskFerry-Heartbeat", "1")
                    .header("X-DeskFerry-Protocol", "2");
        } else {
            builder.header("X-DeskFerry-Session", sessionId)
                    .header("X-DeskFerry-Session-Side", "client");
        }
        return builder.build();
    }

    private void connectRelay(String candidate) throws Exception {
        CountDownLatch paired = new CountDownLatch(1);
        AtomicBoolean started = new AtomicBoolean(false);
        AtomicReference<Throwable> failure = new AtomicReference<>();
        WebSocket socket = host.open(serviceRequest(candidate, "client"), relayListener(paired, started, failure, true));
        webSocket = socket;
        if (!paired.await(12, TimeUnit.SECONDS)) {
            throw new IOException("relay did not pair with a work agent");
        }
        Throwable err = failure.get();
        if (err != null) {
            if (err instanceof SessionRejectedException) {
                throw (SessionRejectedException) err;
            }
            throw new IOException("relay connection failed", err);
        }
        if (!started.get()) {
            throw new IOException("relay closed before pairing");
        }
    }

    private WebSocketListener relayListener(CountDownLatch ready, AtomicBoolean started, AtomicReference<Throwable> failure, boolean initial) {
        return new WebSocketListener() {
            private final AtomicBoolean closeHandled = new AtomicBoolean(false);

            @Override
            public void onMessage(WebSocket socket, String text) {
                String value = text.trim();
                if (initial && ("start".equals(value) || value.startsWith("start ") || value.startsWith("{"))) {
                    if (value.startsWith("{")) {
                        try {
                            SessionReady message = readyParser.parse(value);
                            if (!"session-ready".equals(message.type)) {
                                failure.set(new SessionRejectedException(message.type, message.reason));
                                ready.countDown();
                                return;
                            }
                            String confirmedService = message.service;
                            if ((requireServiceEcho || !confirmedService.isEmpty()) && !service.equals(confirmedService)) {
                                failure.set(new IOException(confirmedService.isEmpty()
                                        ? "relay did not confirm " + service + "-service support"
                                        : "relay confirmed " + confirmedService + " instead of " + service));
                                ready.countDown();
                                return;
                            }
                            sessionId = message.sessionId;
                            heartbeatEnabled = message.heartbeat;
                            if (sessionId.isEmpty()) {
                                failure.set(new IOException("relay session-ready result omitted the session ID"));
                                ready.countDown();
                                return;
                            }
                        } catch (Exception ex) {
                            failure.set(ex);
                            ready.countDown();
                            return;
                        }
                    } else if (requireServiceEcho) {
                        failure.set(new IOException("relay did not confirm " + service + "-service support"));
                        ready.countDown();
                        return;
                    } else if (value.startsWith("start ")) {
                        sessionId = value.substring("start ".length()).trim();
                    }
                    if (!sessionId.isEmpty()) {
                        if (!attachTransport(socket)) {
                            failure.set(new IOException("failed to initialize resumable relay stream"));
                        }
                    }
                    started.set(true);
                    ready.countDown();
                } else if (!initial && value.equals("resume " + sessionId)) {
                    if (!attachTransport(socket)) {
                        failure.set(new IOException("failed to replay resumable relay stream"));
                    }
                    started.set(true);
                    ready.countDown();
                }
            }

            @Override
            public void onMessage(WebSocket socket, ByteString bytes) {
                try {
                    handleRelayPayload(socket, bytes.toByteArray());
                } catch (IOException ex) {
                    failure.set(ex);
                    recordTermination("relay_to_local_write_error=" + TunnelService.throwableText(ex));
                    close();
                }
            }

            @Override
            public void onClosing(WebSocket socket, int code, String reason) {
                // OkHttp waits for the application to acknowledge a peer-initiated
                // close before invoking onClosed. Handle it here so a completed
                // session or rejected resume does not sit through repeated resume
                // timeouts while the stale local connection remains open.
                socket.close(code, reason);
                handleClose(socket, code, reason);
            }

            @Override
            public void onClosed(WebSocket socket, int code, String reason) {
                handleClose(socket, code, reason);
            }

            private void handleClose(WebSocket socket, int code, String reason) {
                if (!closeHandled.compareAndSet(false, true)) {
                    return;
                }
                if (!started.get()) {
                    if (failure.get() == null) {
                        IOException error = !initial && TunnelService.isTerminalResumeClose(code)
                                ? new TerminalResumeException("relay resume rejected code=" + code + " reason=" + reason)
                                : new IOException("relay closed before pairing code=" + code + " reason=" + reason);
                        failure.set(error);
                    }
                    ready.countDown();
                    return;
                }
                if (!sessionId.isEmpty() && code != 1008 && !TunnelService.isLogicalSessionClose(code, reason) && !closed.get()) {
                    markTransportLost(socket, "websocket_closed code=" + code + " reason=" + TunnelService.quoted(reason));
                } else {
                    recordTermination("websocket_closed code=" + code + " reason=" + TunnelService.quoted(reason));
                    close();
                }
            }

            @Override
            public void onFailure(WebSocket socket, Throwable t, Response response) {
                int status = response == null ? 0 : response.code();
                Throwable reported = !initial && (status == 401 || status == 403)
                        ? new TerminalResumeException("relay resume authentication failed http_status=" + status, t)
                        : t;
                failure.compareAndSet(null, reported);
                ready.countDown();
                if (closeHandled.get()) {
                    return;
                }
                if (!started.get()) {
                    return;
                }
                if (!sessionId.isEmpty() && status != 401 && status != 403 && !closed.get()) {
                    markTransportLost(socket, "websocket_failure error=" + TunnelService.throwableText(t) + " http_status=" + TunnelService.responseStatus(response));
                } else {
                    recordTermination("websocket_failure error=" + TunnelService.throwableText(t) + " http_status=" + TunnelService.responseStatus(response));
                    close();
                }
            }
        };
    }

    private String relayAttemptResult(Exception error) {
        if (error instanceof SessionRejectedException) {
            return ((SessionRejectedException) error).result;
        }
        String text = String.valueOf(error.getMessage()).toLowerCase(Locale.ROOT);
        return text.contains("did not pair") || text.contains("timeout") ? "timeout" : "transport-failure";
    }

    static final class SessionRejectedException extends IOException {
        final String result;

        SessionRejectedException(String result, String reason) {
            super("relay session rejected: " + result + (reason.isEmpty() ? "" : " (" + reason + ")"));
            this.result = result;
        }

        boolean terminal() {
            return "authentication-failed".equals(result) || "service-disabled".equals(result) || "invalid-request".equals(result);
        }
    }

    static final class TerminalResumeException extends IOException {
        TerminalResumeException(String message) {
            super(message);
        }

        TerminalResumeException(String message, Throwable cause) {
            super(message, cause);
        }
    }

    private boolean attachTransport(WebSocket socket) {
        long generation;
        int replayBytes;
        long lostDurationMs;
        long acknowledgedOffset;
        synchronized (receiveLock) {
            acknowledgedOffset = receiveOffset;
        }
        synchronized (resumeLock) {
            if (closed.get()) {
                return false;
            }
            webSocket = socket;
            generation = ++transportGeneration;
            replayBytes = sendBuffer.length;
            lostDurationMs = lastTransportLostMs == 0 ? 0 : SystemClock.elapsedRealtime() - lastTransportLostMs;
            if (!socket.send(frame((byte) 2, acknowledgedOffset, null))) {
                webSocket = null;
                return false;
            }
            int position = 0;
            long offset = sendBase;
            while (position < sendBuffer.length) {
                int size = Math.min(TunnelService.RESUMABLE_CHUNK_SIZE, sendBuffer.length - position);
                byte[] payload = Arrays.copyOfRange(sendBuffer, position, position + size);
                if (!socket.send(frame((byte) 1, offset, payload))) {
                    webSocket = null;
                    return false;
                }
                position += size;
                offset += size;
            }
            resumeLock.notifyAll();
        }
        if (generation > 1) {
            append(serviceLabel + " relay transport resumed session=" + sessionId + " relay=" + selectedRelay
                    + " protocol=" + relayProtocol(socket) + " generation=" + generation + " lost_ms=" + lostDurationMs + " replay_bytes=" + replayBytes + ".");
        }
        if (heartbeatEnabled) {
            startHeartbeat(socket, generation);
        }
        return true;
    }

    private void startHeartbeat(WebSocket socket, long generation) {
        new Thread(() -> {
            while (!closed.get()) {
                TunnelService.sleepQuietly(TunnelService.HEARTBEAT_INTERVAL_MS);
                long nonce;
                synchronized (resumeLock) {
                    if (closed.get() || webSocket != socket || transportGeneration != generation) {
                        return;
                    }
                    if (heartbeatNonce == Long.MAX_VALUE) {
                        heartbeatNonce = 0;
                        heartbeatAcknowledged = 0;
                    }
                    nonce = ++heartbeatNonce;
                }
                if (!socket.send(frame((byte) 3, nonce, null))) {
                    markTransportLost(socket, "heartbeat send failed");
                    return;
                }
                long pingAt = SystemClock.elapsedRealtime();
                long deadline = SystemClock.elapsedRealtime()
                        + ("rdp".equals(service) ? TunnelService.RDP_HEARTBEAT_TIMEOUT_MS : TunnelService.HEARTBEAT_TIMEOUT_MS);
                synchronized (resumeLock) {
                    while (!closed.get() && webSocket == socket && transportGeneration == generation
                            && heartbeatAcknowledged < nonce) {
                        long remaining = deadline - SystemClock.elapsedRealtime();
                        if (remaining <= 0) {
                            break;
                        }
                        try {
                            resumeLock.wait(remaining);
                        } catch (InterruptedException interrupted) {
                            Thread.currentThread().interrupt();
                            return;
                        }
                    }
                    if (closed.get() || webSocket != socket || transportGeneration != generation) {
                        return;
                    }
                    if (heartbeatAcknowledged < nonce) {
                        markTransportLost(socket, "end-to-end heartbeat timed out");
                        return;
                    }
                }
                long roundTripMs = SystemClock.elapsedRealtime() - pingAt;
                if (roundTripMs >= TunnelService.DIAGNOSTIC_STALL_MS) {
                    append(serviceLabel + " slow relay heartbeat session=" + sessionId + " relay=" + selectedRelay
                            + " generation=" + generation + " round_trip_ms=" + roundTripMs + ".");
                }
            }
        }, "DeskFerry-" + serviceLabel + "-Heartbeat").start();
    }

    private void markTransportLost(WebSocket socket, String reason) {
        long pending;
        long generation;
        synchronized (resumeLock) {
            if (webSocket != socket || closed.get()) {
                return;
            }
            webSocket = null;
            lastTransportLostMs = SystemClock.elapsedRealtime();
            pending = sendEnd - sendBase;
            generation = transportGeneration;
            resumeLock.notifyAll();
        }
        append(serviceLabel + " relay transport lost session=" + sessionId + " relay=" + selectedRelay
                + " protocol=" + relayProtocol(socket) + " generation=" + generation + " pending_bytes=" + pending + " reason=" + reason + ".");
        append(serviceLabel + " relay stream interrupted; retrying transparently: " + reason);
        startReconnectLoop();
    }

    /** Drops the current transport for one on the new network and resumes. */
    void replaceTransportForNetworkChange(String reason) {
        WebSocket active;
        WebSocket pending;
        CountDownLatch pendingReady;
        long pendingBytes;
        long generation;
        synchronized (resumeLock) {
            if (closed.get()) {
                return;
            }
            active = webSocket;
            webSocket = null;
            if (active != null) {
                lastTransportLostMs = SystemClock.elapsedRealtime();
            }
            pendingBytes = sendEnd - sendBase;
            generation = transportGeneration;
            pending = pendingResumeSocket;
            pendingResumeSocket = null;
            pendingReady = pendingResumeReady;
            pendingResumeReady = null;
            resumeLock.notifyAll();
        }
        if (active != null) {
            active.cancel();
            append(serviceLabel + " relay transport replaced after network change session=" + sessionId
                    + " relay=" + selectedRelay + " protocol=" + relayProtocol(active) + " generation=" + generation + " pending_bytes=" + pendingBytes
                    + " reason=" + reason + ".");
        }
        if (pending != null && pending != active) {
            pending.cancel();
        }
        // OkHttp may not deliver the cancellation callback until the obsolete
        // network finishes failing. Wake the resume worker immediately so its
        // next attempt is created on the newly available Android network.
        if (pendingReady != null) {
            pendingReady.countDown();
        }
        if (!sessionId.isEmpty()) {
            append(serviceLabel + " relay stream interrupted; retrying transparently: " + reason);
            startReconnectLoop();
        }
    }

    private void startReconnectLoop() {
        if (!reconnecting.compareAndSet(false, true)) {
            return;
        }
        new Thread(() -> {
            long deadline = SystemClock.elapsedRealtime() + TunnelService.RESUMABLE_WINDOW_MS;
            long backoff = 250;
            // Try the session's relay first, then the room's other relays in
            // the configured order, as the Work agent does; relays rebuild the
            // session from both sides' resume attachments.
            List<String> resumeRelays = TunnelService.resumeRelayOrder(selectedRelay, host.relayUrls());
            int relayIndex = 0;
            int waitRound = 0;
            // A phone that cannot reach the session's relay cannot return
            // there, so it waits longer at the others for the Work agent.
            boolean sessionRelayUnreachable = false;
            try {
                while (!closed.get() && SystemClock.elapsedRealtime() < deadline) {
                    boolean attemptTimedOut = false;
                    String relay = resumeRelays.get(relayIndex);
                    CountDownLatch ready = new CountDownLatch(1);
                    AtomicBoolean started = new AtomicBoolean(false);
                    AtomicReference<Throwable> failure = new AtomicReference<>();
                    long attemptStarted = SystemClock.elapsedRealtime();
                    try {
                        WebSocket candidate = host.open(serviceRequest(relay, "resume"), relayListener(ready, started, failure, false));
                        synchronized (resumeLock) {
                            pendingResumeSocket = candidate;
                            pendingResumeReady = ready;
                        }
                        long remaining = Math.max(1, deadline - SystemClock.elapsedRealtime());
                        long attemptWait = resumeRelays.size() > 1
                                ? TunnelService.failoverAttemptWaitMillis(waitRound, remaining)
                                : TunnelService.resumeAttemptWaitMillis(remaining);
                        if (sessionRelayUnreachable && relayIndex != 0) {
                            attemptWait = Math.max(attemptWait, Math.min(TunnelService.FAILOVER_UNREACHABLE_ATTEMPT_MS, remaining));
                        }
                        boolean signaled = ready.await(attemptWait, TimeUnit.MILLISECONDS);
                        if (signaled && started.get() && failure.get() == null) {
                            clearPendingResume(candidate, ready);
                            selectedRelay = relay;
                            append(serviceLabel + " relay stream resumed through " + relay + ".");
                            return;
                        }
                        clearPendingResume(candidate, ready);
                        candidate.cancel();
                        Throwable resumeFailure = failure.get();
                        if (resumeFailure instanceof TerminalResumeException) {
                            recordTermination("resume_rejected error=" + TunnelService.throwableText(resumeFailure));
                            append(serviceLabel + " relay session cannot resume; closing the stale local connection: " + TunnelService.throwableText(resumeFailure));
                            close();
                            return;
                        }
                        if (!closed.get()) {
                            attemptTimedOut = !signaled;
                            if (relayIndex == 0 && signaled) {
                                sessionRelayUnreachable = true;
                            }
                            String result = !signaled ? "attempt_timeout" : "transport_failure";
                            append(serviceLabel + " relay resume attempt failed relay=" + relay
                                    + " result=" + result + " elapsed_ms=" + TunnelService.elapsedMillis(attemptStarted)
                                    + (resumeFailure == null ? "" : " error=" + TunnelService.throwableText(resumeFailure)) + ".");
                        }
                    } catch (Exception ex) {
                        WebSocket candidate = pendingResumeSocket;
                        pendingResumeSocket = null;
                        CountDownLatch pendingReady = pendingResumeReady;
                        pendingResumeReady = null;
                        if (candidate != null) {
                            candidate.cancel();
                        }
                        if (pendingReady != null) {
                            pendingReady.countDown();
                        }
                        if (relayIndex == 0) {
                            sessionRelayUnreachable = true;
                        }
                        if (!closed.get()) {
                            append(serviceLabel + " relay resume attempt failed relay=" + relay
                                    + " result=exception elapsed_ms=" + TunnelService.elapsedMillis(attemptStarted)
                                    + " error=" + TunnelService.throwableText(ex) + ".");
                        }
                    }
                    if (attemptTimedOut) {
                        waitRound++;
                    }
                    relayIndex = (relayIndex + 1) % resumeRelays.size();
                    TunnelService.sleepQuietly(backoff);
                    // An attempt that ran out its whole wait has already waited.
                    if (!attemptTimedOut) {
                        backoff = Math.min(5000, backoff * 2);
                    }
                }
                if (!closed.get()) {
                    recordTermination("resume_window_expired");
                    close();
                }
            } finally {
                reconnecting.set(false);
                synchronized (resumeLock) {
                    resumeLock.notifyAll();
                }
            }
        }, "DeskFerry-" + serviceLabel + "-Resume").start();
    }

    private void clearPendingResume(WebSocket candidate, CountDownLatch ready) {
        synchronized (resumeLock) {
            if (pendingResumeSocket == candidate) {
                pendingResumeSocket = null;
            }
            if (pendingResumeReady == ready) {
                pendingResumeReady = null;
            }
        }
    }

    private void handleRelayPayload(WebSocket socket, byte[] payload) throws IOException {
        if (sessionId.isEmpty()) {
            enqueueLocalDelivery(0, payload, false);
            return;
        }
        if (payload.length < 9 || payload.length > 9 + TunnelService.RESUMABLE_CHUNK_SIZE) {
            markTransportLost(socket, "invalid resumable frame");
            return;
        }
        ByteBuffer frame = ByteBuffer.wrap(payload);
        byte type = frame.get();
        long offset = frame.getLong();
        if (offset < 0) {
            markTransportLost(socket, "invalid resumable offset");
            return;
        }
        if (type == 2) {
            if (frame.hasRemaining()) {
                markTransportLost(socket, "invalid resumable acknowledgement");
                return;
            }
            applyAcknowledgement(offset);
            return;
        }
        if (type == 3 || type == 4) {
            if (frame.hasRemaining()) {
                markTransportLost(socket, "invalid heartbeat frame");
                return;
            }
            if (type == 3) {
                if (!socket.send(frame((byte) 4, offset, null))) {
                    markTransportLost(socket, "heartbeat response failed");
                }
            } else {
                synchronized (resumeLock) {
                    heartbeatAcknowledged = Math.max(heartbeatAcknowledged, offset);
                    resumeLock.notifyAll();
                }
            }
            return;
        }
        if (type != 1) {
            markTransportLost(socket, "unknown resumable frame");
            return;
        }
        byte[] data = new byte[frame.remaining()];
        frame.get(data);
        boolean sequenceGap = false;
        synchronized (receiveLock) {
            long end = offset + data.length;
            if (offset > receiveQueuedOffset) {
                sequenceGap = true;
            } else if (end > receiveQueuedOffset) {
                int trim = (int) (receiveQueuedOffset - offset);
                byte[] fresh = Arrays.copyOfRange(data, trim, data.length);
                enqueueLocalDelivery(receiveQueuedOffset, fresh, true);
                receiveQueuedOffset += fresh.length;
            }
        }
        if (sequenceGap) {
            markTransportLost(socket, "resumable sequence gap");
        }
    }

    private void enqueueLocalDelivery(long offset, byte[] payload, boolean resumable) throws IOException {
        long queued = pendingLocalBytes.addAndGet(payload.length);
        if (queued > TunnelService.RESUMABLE_MAX_BUFFER) {
            pendingLocalBytes.addAndGet(-payload.length);
            throw new IOException("local consumer stalled with more than " + TunnelService.RESUMABLE_MAX_BUFFER + " queued bytes");
        }
        if (!localWriteQueue.offer(new ReceivedChunk(offset, payload, resumable))) {
            pendingLocalBytes.addAndGet(-payload.length);
            throw new IOException("local delivery queue rejected data");
        }
    }

    private void acknowledgeLocalDelivery(long offset) {
        WebSocket current;
        synchronized (resumeLock) {
            current = webSocket;
        }
        if (current != null && !current.send(frame((byte) 2, offset, null))) {
            markTransportLost(current, "acknowledgement send failed");
        }
    }

    private void applyAcknowledgement(long offset) {
        boolean recovered;
        long stalledMs;
        long pending;
        synchronized (resumeLock) {
            if (offset < sendBase || offset > sendEnd) {
                WebSocket current = webSocket;
                if (current != null) {
                    markTransportLost(current, "invalid resumable acknowledgement");
                }
                return;
            }
            int drop = (int) (offset - sendBase);
            recovered = drop > 0 && ackStallLogged;
            stalledMs = recovered ? SystemClock.elapsedRealtime() - ackStallAtMs : 0;
            sendBuffer = Arrays.copyOfRange(sendBuffer, drop, sendBuffer.length);
            sendBase = offset;
            if (drop > 0) {
                lastAckProgressMs = SystemClock.elapsedRealtime();
                ackStallLogged = false;
            }
            pending = sendEnd - sendBase;
            resumeLock.notifyAll();
        }
        if (recovered) {
            append(serviceLabel + " relay acknowledgement progress resumed session=" + sessionId + " relay=" + selectedRelay
                    + " stalled_ms=" + stalledMs + " pending_bytes=" + pending + ".");
        }
    }

    private static String relayProtocol(WebSocket socket) {
        return socket instanceof FallbackWebSocket ? ((FallbackWebSocket) socket).protocol() : "websocket";
    }

    static ByteString frame(byte type, long offset, byte[] payload) {
        int length = payload == null ? 0 : payload.length;
        ByteBuffer buffer = ByteBuffer.allocate(9 + length);
        buffer.put(type);
        buffer.putLong(offset);
        if (payload != null) {
            buffer.put(payload);
        }
        return ByteString.of(buffer.array());
    }

    /** Closes the stream and its transports; the close listener runs once. */
    void close() {
        recordTermination("closed_by_app");
        if (!closed.compareAndSet(false, true)) {
            return;
        }
        WebSocket socket = webSocket;
        WebSocket pending = pendingResumeSocket;
        CountDownLatch pendingReady = pendingResumeReady;
        pendingResumeSocket = null;
        pendingResumeReady = null;
        if (socket != null) {
            socket.close(1000, "session closed");
        }
        if (pending != null && pending != socket) {
            pending.cancel();
        }
        if (pendingReady != null) {
            pendingReady.countDown();
        }
        synchronized (resumeLock) {
            webSocket = null;
            resumeLock.notifyAll();
        }
        Thread writer = localWriterThread;
        if (writer != null && writer != Thread.currentThread()) {
            writer.interrupt();
        }
        if (closeListener != null) {
            closeListener.closed(termination.get());
        }
    }

    private void closeWebSocketOnly() {
        WebSocket socket = webSocket;
        webSocket = null;
        if (socket != null) {
            socket.close(1000, "retrying");
        }
    }

    /**
     * Sends data to the Work side, waiting while the replay buffer is full or
     * the transport is being resumed. For a local socket pump (RDP, SMB).
     */
    void write(byte[] payload) throws IOException {
        if (sessionId.isEmpty()) {
            WebSocket socket = webSocket;
            if (socket == null || !socket.send(ByteString.of(payload))) {
                recordTermination("local_to_relay_send_failed");
                throw new IOException("relay WebSocket send failed");
            }
            while (!closed.get() && socket.queueSize() > 4L * 1024L * 1024L) {
                TunnelService.sleepQuietly(10);
            }
        } else {
            sendReliable(payload);
        }
        localToRelayBytes.addAndGet(payload.length);
        localToRelayMessages.incrementAndGet();
    }

    /**
     * Queues data for the Work side without waiting: while the transport is
     * being resumed the data is held and replayed once it is back. For callers
     * on the UI thread (the screen viewer's input and commands).
     */
    void writeNow(byte[] payload) throws IOException {
        synchronized (resumeLock) {
            if (closed.get()) {
                throw new IOException(serviceLabel + " stream closed");
            }
            if (sessionId.isEmpty()) {
                WebSocket socket = webSocket;
                if (socket == null || !socket.send(ByteString.of(payload))) {
                    throw new IOException("relay WebSocket send failed");
                }
            } else {
                if (sendBuffer.length + payload.length > TunnelService.RESUMABLE_MAX_BUFFER) {
                    throw new IOException(serviceLabel + " stream send buffer is full");
                }
                long offset = sendEnd;
                if (sendEnd == sendBase) {
                    lastAckProgressMs = SystemClock.elapsedRealtime();
                }
                byte[] combined = Arrays.copyOf(sendBuffer, sendBuffer.length + payload.length);
                System.arraycopy(payload, 0, combined, sendBuffer.length, payload.length);
                sendBuffer = combined;
                sendEnd += payload.length;
                // Sent under the lock so frames leave in offset order; OkHttp
                // only queues them. Without a transport, the next attach
                // replays them.
                WebSocket socket = webSocket;
                if (socket != null) {
                    for (int position = 0; position < payload.length; position += TunnelService.RESUMABLE_CHUNK_SIZE) {
                        int size = Math.min(TunnelService.RESUMABLE_CHUNK_SIZE, payload.length - position);
                        if (!socket.send(frame((byte) 1, offset + position, Arrays.copyOfRange(payload, position, position + size)))) {
                            webSocket = null;
                            lastTransportLostMs = SystemClock.elapsedRealtime();
                            resumeLock.notifyAll();
                            new Thread(() -> {
                                append(serviceLabel + " relay stream interrupted; retrying transparently: data send failed");
                                startReconnectLoop();
                            }, "DeskFerry-" + serviceLabel + "-SendFailure").start();
                            break;
                        }
                    }
                }
            }
        }
        localToRelayBytes.addAndGet(payload.length);
        localToRelayMessages.incrementAndGet();
    }

    private void sendReliable(byte[] payload) throws IOException {
        long offset;
        synchronized (resumeLock) {
            while (!closed.get() && sendBuffer.length + payload.length > TunnelService.RESUMABLE_MAX_BUFFER) {
                try {
                    resumeLock.wait(1000);
                } catch (InterruptedException ex) {
                    Thread.currentThread().interrupt();
                    throw new IOException("interrupted while waiting for relay acknowledgement", ex);
                }
            }
            if (closed.get()) {
                throw new IOException(serviceLabel + " bridge closed");
            }
            offset = sendEnd;
            if (sendEnd == sendBase) {
                lastAckProgressMs = SystemClock.elapsedRealtime();
            }
            byte[] combined = Arrays.copyOf(sendBuffer, sendBuffer.length + payload.length);
            System.arraycopy(payload, 0, combined, sendBuffer.length, payload.length);
            sendBuffer = combined;
            sendEnd += payload.length;
        }
        ByteString framed = frame((byte) 1, offset, payload);
        while (!closed.get()) {
            WebSocket socket;
            synchronized (resumeLock) {
                while (!closed.get() && webSocket == null) {
                    try {
                        resumeLock.wait(1000);
                    } catch (InterruptedException ex) {
                        Thread.currentThread().interrupt();
                        throw new IOException("interrupted while resuming relay", ex);
                    }
                }
                socket = webSocket;
            }
            if (socket != null && socket.send(framed)) {
                return;
            }
            if (socket != null) {
                markTransportLost(socket, "data send failed");
            }
        }
        throw new IOException(serviceLabel + " bridge closed");
    }
}
