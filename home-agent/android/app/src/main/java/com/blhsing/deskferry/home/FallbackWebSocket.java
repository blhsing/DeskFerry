package com.blhsing.deskferry.home;

import java.io.IOException;
import java.net.ProtocolException;
import java.security.SecureRandom;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Iterator;
import java.util.List;
import java.util.Set;
import java.util.TreeMap;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.ScheduledFuture;
import java.util.concurrent.ThreadFactory;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;

import okhttp3.Call;
import okhttp3.Callback;
import okhttp3.HttpUrl;
import okhttp3.MediaType;
import okhttp3.OkHttpClient;
import okhttp3.Request;
import okhttp3.RequestBody;
import okhttp3.Response;
import okhttp3.WebSocket;
import okhttp3.WebSocketListener;
import okio.BufferedSink;
import okio.BufferedSource;
import okio.ByteString;

/**
 * Prefers OkHttp's native WebSocket and replaces a failed proxy CONNECT with
 * DeskFerry's reliable streaming POST/GET transport.
 */
final class FallbackWebSocket implements WebSocket {
    private static final ScheduledExecutorService RETRIES = Executors.newScheduledThreadPool(4, new ThreadFactory() {
        private int next;
        @Override public synchronized Thread newThread(Runnable runnable) {
            Thread thread = new Thread(runnable, "DeskFerry-http-stream-" + (++next));
            thread.setDaemon(true);
            return thread;
        }
    });

    private final OkHttpClient client;
    private final Request request;
    private final WebSocketListener listener;
    private final Object lock = new Object();
    private volatile WebSocket active;
    private volatile boolean nativeOpened;
    private volatile boolean fallbackStarted;
    private volatile boolean canceled;

    String protocol() {
        return fallbackStarted ? "http-stream" : "websocket";
    }

    FallbackWebSocket(OkHttpClient client, Request request, WebSocketListener listener) {
        this.client = client;
        this.request = request;
        this.listener = listener;
        active = client.newWebSocket(request, new WebSocketListener() {
            @Override public void onOpen(WebSocket webSocket, Response response) {
                nativeOpened = true;
                active = webSocket;
                listener.onOpen(FallbackWebSocket.this, response);
            }

            @Override public void onMessage(WebSocket webSocket, String text) {
                listener.onMessage(FallbackWebSocket.this, text);
            }

            @Override public void onMessage(WebSocket webSocket, ByteString bytes) {
                listener.onMessage(FallbackWebSocket.this, bytes);
            }

            @Override public void onClosing(WebSocket webSocket, int code, String reason) {
                listener.onClosing(FallbackWebSocket.this, code, reason);
            }

            @Override public void onClosed(WebSocket webSocket, int code, String reason) {
                listener.onClosed(FallbackWebSocket.this, code, reason);
            }

            @Override public void onFailure(WebSocket webSocket, Throwable failure, Response response) {
                if (!nativeOpened && shouldFallback(response)) {
                    startFallback(failure);
                    return;
                }
                listener.onFailure(FallbackWebSocket.this, failure, response);
            }
        });
    }

    private static boolean shouldFallback(Response response) {
        if (response == null) return true;
        int code = response.code();
        return code != 401 && code != 403;
    }

    private void startFallback(Throwable webSocketFailure) {
        synchronized (lock) {
            if (fallbackStarted || canceled) return;
            fallbackStarted = true;
            HTTPStreamSocket stream;
            try {
                stream = new HTTPStreamSocket(client, request, new WebSocketListener() {
                    @Override public void onOpen(WebSocket webSocket, Response response) {
                        active = webSocket;
                        listener.onOpen(FallbackWebSocket.this, response);
                    }

                    @Override public void onMessage(WebSocket webSocket, String text) {
                        listener.onMessage(FallbackWebSocket.this, text);
                    }

                    @Override public void onMessage(WebSocket webSocket, ByteString bytes) {
                        listener.onMessage(FallbackWebSocket.this, bytes);
                    }

                    @Override public void onClosing(WebSocket webSocket, int code, String reason) {
                        listener.onClosing(FallbackWebSocket.this, code, reason);
                    }

                    @Override public void onClosed(WebSocket webSocket, int code, String reason) {
                        listener.onClosed(FallbackWebSocket.this, code, reason);
                    }

                    @Override public void onFailure(WebSocket webSocket, Throwable failure, Response response) {
                        failure.addSuppressed(webSocketFailure);
                        listener.onFailure(FallbackWebSocket.this, failure, response);
                    }
                });
            } catch (RuntimeException failure) {
                failure.addSuppressed(webSocketFailure);
                listener.onFailure(FallbackWebSocket.this, failure, null);
                return;
            }
            active = stream;
            stream.start();
        }
    }

    @Override public Request request() { return request; }
    @Override public long queueSize() { WebSocket socket = active; return socket == null ? 0 : socket.queueSize(); }
    @Override public boolean send(String text) { WebSocket socket = active; return socket != null && socket.send(text); }
    @Override public boolean send(ByteString bytes) { WebSocket socket = active; return socket != null && socket.send(bytes); }
    @Override public boolean close(int code, String reason) { WebSocket socket = active; return socket != null && socket.close(code, reason); }
    @Override public void cancel() { canceled = true; WebSocket socket = active; if (socket != null) socket.cancel(); }

    /**
     * Carries WebSocket-equivalent messages over finite POST and GET batches,
     * matching the Go client. Sequence numbers make every batch safe to replay,
     * so either direction can retry after a proxy drops a request. Once the
     * relay confirms pipelining, several uploads and two waiting GETs overlap so
     * data queued during one round trip does not wait for it to finish.
     */
    private static final class HTTPStreamSocket implements WebSocket {
        private static final int ACK = 0;
        private static final int TEXT = 1;
        private static final int BINARY = 2;
        private static final int CLOSE = 8;
        private static final int MAX_BUFFERED = 8 * 1024 * 1024;
        // Matches the Go clients: behind a buffering proxy each request holds
        // its slot for about a round trip plus travel time, so interactive
        // traffic needs about five uploads and four waiting GETs to stay at
        // one round trip.
        private static final int UPLOAD_PIPELINE = 5;
        private static final int DOWNLOAD_PIPELINE = 4;
        private static final int PIPELINE_BATCH_BYTES = 256 * 1024;
        private static final int REORDER_LIMIT = 4096;
        private static final long KEEPALIVE_MILLIS = 10_000;
        private static final long RESEND_AFTER_MILLIS = 3_000;
        private static final String PIPELINE_HEADER = "X-DeskFerry-Stream-Pipeline";
        private static final String ACK_HEADER = "X-DeskFerry-Stream-Ack";
        private static final MediaType OCTETS = MediaType.get("application/octet-stream");
        private static final SecureRandom RANDOM = new SecureRandom();

        private static final class Frame {
            final int kind;
            final long sequence;
            final ByteString payload;
            Frame(int kind, long sequence, ByteString payload) {
                this.kind = kind;
                this.sequence = sequence;
                this.payload = payload;
            }
        }

        private final OkHttpClient client;
        private final Request original;
        private final WebSocketListener listener;
        private final HttpUrl base;
        private final Request.Builder requestBase;
        private final Object gate = new Object();
        // Serializes listener delivery so records applied by concurrent GETs
        // still reach the listener in sequence order.
        private final Object deliverGate = new Object();
        private final Set<Call> calls = Collections.newSetFromMap(new ConcurrentHashMap<>());
        private final AtomicBoolean opened = new AtomicBoolean();
        private final AtomicBoolean downPipelineStarted = new AtomicBoolean();
        private volatile boolean stopped;
        private volatile boolean downAcks;
        private ScheduledFuture<?> uploadTicker;

        // Guarded by gate.
        private final List<Frame> outgoing = new ArrayList<>();
        private final TreeMap<Long, Frame> receiveAhead = new TreeMap<>();
        private long nextSend = 1;
        private long nextReceive = 1;
        private int buffered;
        private int receiveAheadBytes;
        private long nextClaim = 1;
        private long deliveredAck;
        private int uploadsInFlight;
        private boolean uploadPipelined;
        // Each new proxy connection may cost an authentication round trip, so
        // the upload connection opens with an ack-only batch at once and the
        // remaining pipeline connections as soon as the relay confirms them.
        private int uploadWarmups = 1;
        private long lastUploadStart;
        private long uploadRetryAt;
        private long uploadBackoff = 250;

        HTTPStreamSocket(OkHttpClient client, Request request, WebSocketListener listener) {
            this.client = client;
            this.original = request;
            this.listener = listener;
            HttpUrl webSocketUrl = request.url();
            String scheme = webSocketUrl.isHttps() ? "https" : "http";
            HttpUrl.Builder builder = webSocketUrl.newBuilder().scheme(scheme);
            if ("ws".equals(webSocketUrl.pathSegments().get(webSocketUrl.pathSize() - 1))) {
                builder.removePathSegment(webSocketUrl.pathSize() - 1);
            }
            byte[] id = new byte[16];
            byte[] secret = new byte[32];
            RANDOM.nextBytes(id);
            RANDOM.nextBytes(secret);
            String streamId = ByteString.of(id).base64Url().replace("=", "");
            String streamSecret = ByteString.of(secret).base64Url().replace("=", "");
            this.base = builder.addPathSegment("stream").addPathSegment(streamId).build();
            this.requestBase = request.newBuilder()
                    .removeHeader("Sec-WebSocket-Key")
                    .removeHeader("Sec-WebSocket-Version")
                    .removeHeader("Upgrade")
                    .removeHeader("Connection")
                    .header("X-DeskFerry-Stream-Secret", streamSecret)
                    .header("X-DeskFerry-Stream-Batch", "1");
        }

        void start() {
            synchronized (gate) {
                lastUploadStart = System.currentTimeMillis();
            }
            startDown(true, 250);
            pumpUploads();
            uploadTicker = RETRIES.scheduleWithFixedDelay(this::pumpUploads, 1, 1, TimeUnit.SECONDS);
        }

        private HttpUrl directionUrl(String direction) {
            return base.newBuilder().addPathSegment(direction).build();
        }

        private Call track(Request request) {
            Call call = client.newCall(request);
            calls.add(call);
            if (stopped) call.cancel();
            return call;
        }

        // Downloads ---------------------------------------------------------

        /**
         * Issues one downstream GET. The primary loop opens the socket and
         * reports permanent failures; a secondary loop only keeps a second GET
         * waiting at a relay that confirmed pipelined downloads.
         */
        private void startDown(boolean primary, long retryMillis) {
            if (stopped) return;
            long received;
            synchronized (gate) { received = nextReceive - 1; }
            Request request = requestBase.url(directionUrl("down"))
                    .header(ACK_HEADER, Long.toString(received))
                    .header(PIPELINE_HEADER, "1")
                    .get().build();
            Call call = track(request);
            call.enqueue(new Callback() {
                @Override public void onFailure(Call call, IOException failure) {
                    calls.remove(call);
                    retryDown(primary, retryMillis, failure, null);
                }

                @Override public void onResponse(Call call, Response response) {
                    calls.remove(call);
                    if (!response.isSuccessful()) {
                        retryDown(primary, retryMillis, new ProtocolException("HTTP stream GET failed: " + response.code()), response);
                        response.close();
                        return;
                    }
                    if (response.header(ACK_HEADER) != null) downAcks = true;
                    if (response.header(PIPELINE_HEADER) != null && downPipelineStarted.compareAndSet(false, true)) {
                        for (int i = 1; i < DOWNLOAD_PIPELINE; i++) startDown(false, 250);
                    }
                    if (primary && opened.compareAndSet(false, true)) {
                        listener.onOpen(HTTPStreamSocket.this, response);
                    }
                    int records = 0;
                    try (Response ignored = response) {
                        BufferedSource source = response.body().source();
                        while (!stopped) {
                            if (records > 0 && source.exhausted()) {
                                // A finished batch is the normal long-poll
                                // cycle; poll again at once.
                                startDown(primary, 250);
                                return;
                            }
                            applyDownstream(readFrame(source));
                            records++;
                        }
                    } catch (IOException failure) {
                        retryDown(primary, retryMillis, failure, null);
                    }
                }
            });
        }

        private void retryDown(boolean primary, long retryMillis, Throwable failure, Response response) {
            if (stopped) return;
            if (primary && !opened.get() && response != null && (response.code() == 400 || response.code() == 401 || response.code() == 403 || response.code() == 404 || response.code() == 405)) {
                stopped = true;
                listener.onFailure(this, failure, response);
                return;
            }
            RETRIES.schedule(() -> startDown(primary, Math.min(5000, retryMillis * 2)), retryMillis, TimeUnit.MILLISECONDS);
        }

        private void applyDownstream(Frame frame) throws IOException {
            if (frame.kind == ACK) {
                synchronized (gate) {
                    if (frame.sequence >= nextSend) throw new ProtocolException("invalid HTTP stream acknowledgement");
                    Iterator<Frame> iterator = outgoing.iterator();
                    while (iterator.hasNext()) {
                        Frame pending = iterator.next();
                        if (pending.sequence > frame.sequence) break;
                        buffered -= pending.payload.size();
                        iterator.remove();
                    }
                }
                return;
            }
            if (frame.kind != TEXT && frame.kind != BINARY && frame.kind != CLOSE) {
                throw new ProtocolException("invalid HTTP stream record type");
            }
            List<Frame> ready = new ArrayList<>();
            synchronized (deliverGate) {
                synchronized (gate) {
                    if (frame.sequence < nextReceive) return;
                    if (frame.sequence > nextReceive) {
                        if (receiveAhead.containsKey(frame.sequence)) return;
                        if (receiveAhead.size() >= REORDER_LIMIT || receiveAheadBytes + frame.payload.size() > MAX_BUFFERED) {
                            throw new ProtocolException("HTTP stream sequence too far ahead");
                        }
                        receiveAhead.put(frame.sequence, frame);
                        receiveAheadBytes += frame.payload.size();
                        return;
                    }
                    Frame next = frame;
                    while (next != null) {
                        ready.add(next);
                        nextReceive++;
                        next = receiveAhead.remove(nextReceive);
                        if (next != null) receiveAheadBytes -= next.payload.size();
                    }
                }
                for (Frame delivered : ready) deliver(delivered);
            }
            pumpUploads();
        }

        private void deliver(Frame frame) {
            if (stopped) return;
            if (frame.kind == TEXT) {
                listener.onMessage(this, frame.payload.utf8());
            } else if (frame.kind == BINARY) {
                listener.onMessage(this, frame.payload);
            } else {
                int code = frame.payload.size() >= 2 ? ((frame.payload.getByte(0) & 0xff) << 8) | (frame.payload.getByte(1) & 0xff) : 1000;
                String reason = frame.payload.size() > 2 ? frame.payload.substring(2).utf8() : "";
                listener.onClosing(this, code, reason);
                stopped = true;
                listener.onClosed(this, code, reason);
                cancelCalls();
            }
        }

        // Uploads -----------------------------------------------------------

        /** Starts as many upload batches as the window and pending data allow. */
        private void pumpUploads() {
            if (stopped) return;
            synchronized (gate) {
                long now = System.currentTimeMillis();
                // A batch the relay accepted but never acknowledged, for
                // example one a proxy truncated, is sent again once nothing
                // else is in flight.
                if (uploadsInFlight == 0 && now - lastUploadStart >= RESEND_AFTER_MILLIS
                        && !outgoing.isEmpty() && outgoing.get(0).sequence < nextClaim) {
                    nextClaim = outgoing.get(0).sequence;
                }
                int window = uploadPipelined ? UPLOAD_PIPELINE : 1;
                int limit = uploadPipelined ? PIPELINE_BATCH_BYTES : MAX_BUFFERED;
                while (uploadsInFlight < window && now >= uploadRetryAt) {
                    List<Frame> frames = new ArrayList<>();
                    int size = 0;
                    for (Frame frame : outgoing) {
                        if (frame.sequence < nextClaim) continue;
                        if (!frames.isEmpty() && size + frame.payload.size() > limit) break;
                        frames.add(frame);
                        size += frame.payload.size();
                    }
                    long ack = nextReceive - 1;
                    // When the relay takes acknowledgements from GETs, an
                    // ack-only POST would only occupy an upload slot.
                    boolean ackOnly = uploadsInFlight == 0 && ack > deliveredAck && !downAcks;
                    boolean keepaliveDue = uploadsInFlight == 0 && now - lastUploadStart >= KEEPALIVE_MILLIS;
                    if (frames.isEmpty() && !ackOnly && !keepaliveDue && uploadWarmups == 0) break;
                    if (frames.isEmpty() && uploadWarmups > 0) uploadWarmups--;
                    long first = 0;
                    if (!frames.isEmpty()) {
                        first = frames.get(0).sequence;
                        nextClaim = frames.get(frames.size() - 1).sequence + 1;
                    }
                    uploadsInFlight++;
                    lastUploadStart = now;
                    postUpload(first, ack, frames);
                }
            }
        }

        private void postUpload(long first, long ack, List<Frame> frames) {
            okio.Buffer body = new okio.Buffer();
            try {
                writeFrame(body, new Frame(ACK, ack, ByteString.EMPTY));
                for (Frame frame : frames) writeFrame(body, frame);
            } catch (IOException impossible) {
                throw new IllegalStateException(impossible);
            }
            Request request = requestBase.url(directionUrl("up"))
                    .header(PIPELINE_HEADER, "1")
                    .post(RequestBody.create(body.readByteString(), OCTETS))
                    .build();
            Call call = track(request);
            call.enqueue(new Callback() {
                @Override public void onFailure(Call call, IOException failure) {
                    calls.remove(call);
                    uploadFinished(first, ack, false, false);
                }

                @Override public void onResponse(Call call, Response response) {
                    calls.remove(call);
                    boolean successful = response.isSuccessful();
                    boolean pipelined = response.header(PIPELINE_HEADER) != null;
                    response.close();
                    uploadFinished(first, ack, successful, pipelined);
                }
            });
        }

        private void uploadFinished(long first, long ack, boolean successful, boolean pipelined) {
            long retryDelay = 0;
            synchronized (gate) {
                uploadsInFlight--;
                if (successful) {
                    uploadBackoff = 250;
                    if (ack > deliveredAck) deliveredAck = ack;
                    if (pipelined && !uploadPipelined) {
                        uploadPipelined = true;
                        uploadWarmups = UPLOAD_PIPELINE - 1;
                    }
                } else {
                    // Resend from the failed batch. Batches still in flight may
                    // overlap the resend; the relay discards duplicates.
                    if (first != 0 && first < nextClaim) nextClaim = first;
                    retryDelay = uploadBackoff;
                    uploadRetryAt = System.currentTimeMillis() + retryDelay;
                    uploadBackoff = Math.min(5000, uploadBackoff * 2);
                }
            }
            if (stopped) return;
            if (retryDelay > 0) {
                RETRIES.schedule(this::pumpUploads, retryDelay, TimeUnit.MILLISECONDS);
            } else {
                pumpUploads();
            }
        }

        private boolean queue(int kind, ByteString payload) {
            synchronized (gate) {
                if (stopped || buffered + payload.size() > MAX_BUFFERED) return false;
                outgoing.add(new Frame(kind, nextSend++, payload));
                buffered += payload.size();
            }
            pumpUploads();
            return true;
        }

        // WebSocket ---------------------------------------------------------

        @Override public Request request() { return original; }
        @Override public long queueSize() { synchronized (gate) { return buffered; } }
        @Override public boolean send(String text) { return queue(TEXT, ByteString.encodeUtf8(text)); }
        @Override public boolean send(ByteString bytes) { return queue(BINARY, bytes); }
        @Override public boolean close(int code, String reason) {
            byte[] data = new byte[2 + Math.min(123, ByteString.encodeUtf8(reason == null ? "" : reason).size())];
            data[0] = (byte) (code >>> 8);
            data[1] = (byte) code;
            ByteString reasonBytes = ByteString.encodeUtf8(reason == null ? "" : reason);
            byte[] raw = reasonBytes.toByteArray();
            System.arraycopy(raw, 0, data, 2, data.length - 2);
            boolean queued = queue(CLOSE, ByteString.of(data));
            RETRIES.schedule(this::cancel, 2500, TimeUnit.MILLISECONDS);
            return queued;
        }
        @Override public void cancel() {
            stopped = true;
            cancelCalls();
        }

        private void cancelCalls() {
            ScheduledFuture<?> ticker = uploadTicker;
            if (ticker != null) ticker.cancel(false);
            for (Call call : calls) call.cancel();
            calls.clear();
        }

        private static Frame readFrame(BufferedSource source) throws IOException {
            int kind = source.readByte() & 0xff;
            long sequence = source.readLong();
            long length = source.readInt() & 0xffffffffL;
            if (length > (1 << 20)) throw new ProtocolException("HTTP stream record exceeds limit");
            return new Frame(kind, sequence, source.readByteString(length));
        }

        private static void writeFrame(BufferedSink sink, Frame frame) throws IOException {
            sink.writeByte(frame.kind);
            sink.writeLong(frame.sequence);
            sink.writeInt(frame.payload.size());
            sink.write(frame.payload);
        }
    }
}
