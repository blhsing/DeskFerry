package com.blhsing.deskferry.home;

import java.io.EOFException;
import java.io.IOException;
import java.net.ProtocolException;
import java.security.SecureRandom;
import java.util.ArrayList;
import java.util.Iterator;
import java.util.List;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
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

    private static final class HTTPStreamSocket implements WebSocket {
        private static final int ACK = 0;
        private static final int TEXT = 1;
        private static final int BINARY = 2;
        private static final int CLOSE = 8;
        private static final int MAX_BUFFERED = 8 * 1024 * 1024;
        private static final long UPLOAD_PROBE_MILLIS = 1500;
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
        private final Object gate = new Object();
        private final List<Frame> outgoing = new ArrayList<>();
        private final AtomicBoolean opened = new AtomicBoolean();
        private volatile boolean stopped;
        private volatile Call upCall;
        private volatile Call downCall;
        private long nextSend = 1;
        private long nextReceive = 1;
        private int buffered;
        private long retryMillis = 250;

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
                    .header("X-DeskFerry-Stream-Secret", streamSecret);
        }

        private final Request.Builder requestBase;

        void start() {
            startDown();
            startUp();
        }

        private HttpUrl directionUrl(String direction) {
            return base.newBuilder().addPathSegment(direction).build();
        }

        private void startDown() {
            if (stopped) return;
            Request request = requestBase.url(directionUrl("down")).get().build();
            downCall = client.newCall(request);
            downCall.enqueue(new Callback() {
                @Override public void onFailure(Call call, IOException failure) { retryDown(failure, null); }

                @Override public void onResponse(Call call, Response response) {
                    if (!response.isSuccessful()) {
                        retryDown(new ProtocolException("HTTP stream GET failed: " + response.code()), response);
                        response.close();
                        return;
                    }
                    if (opened.compareAndSet(false, true)) {
                        retryMillis = 250;
                        listener.onOpen(HTTPStreamSocket.this, response);
                    }
                    try (Response ignored = response) {
                        BufferedSource source = response.body().source();
                        while (!stopped) {
                            Frame frame = readFrame(source);
                            applyDownstream(frame);
                        }
                    } catch (IOException failure) {
                        retryDown(failure, null);
                    }
                }
            });
        }

        private void startUp() {
            if (stopped) return;
            RequestBody body = new RequestBody() {
                @Override public MediaType contentType() { return OCTETS; }
                @Override public void writeTo(BufferedSink sink) throws IOException {
                    long lastSequence = 0;
                    long lastAck = -1;
                    long unacknowledgedSince = 0;
                    while (!stopped) {
                        List<Frame> frames = new ArrayList<>();
                        long ack;
                        synchronized (gate) {
                            for (Frame frame : outgoing) if (frame.sequence > lastSequence) frames.add(frame);
                            ack = nextReceive - 1;
                        }
                        if (ack != lastAck || frames.isEmpty()) {
                            writeFrame(sink, new Frame(ACK, ack, ByteString.EMPTY));
                            lastAck = ack;
                        }
                        for (Frame frame : frames) {
                            writeFrame(sink, frame);
                            lastSequence = frame.sequence;
                        }
                        sink.flush();
                        synchronized (gate) {
                            boolean acknowledged = lastSequence == 0 || outgoing.isEmpty() || outgoing.get(0).sequence > lastSequence;
                            long waitMillis = 5000;
                            if (acknowledged) {
                                unacknowledgedSince = 0;
                            } else {
                                long now = System.currentTimeMillis();
                                if (unacknowledgedSince == 0) unacknowledgedSince = now;
                                waitMillis = UPLOAD_PROBE_MILLIS - (now - unacknowledgedSince);
                                if (waitMillis <= 0) return;
                            }
                            try { gate.wait(waitMillis); } catch (InterruptedException interrupted) {
                                Thread.currentThread().interrupt();
                                throw new IOException("HTTP stream upload interrupted", interrupted);
                            }
                        }
                    }
                }
            };
            Request request = requestBase.url(directionUrl("up"))
                    .header("Expect", "100-continue")
                    .post(body).build();
            upCall = client.newCall(request);
            upCall.enqueue(new Callback() {
                @Override public void onFailure(Call call, IOException failure) { retryUp(failure); }
                @Override public void onResponse(Call call, Response response) {
                    boolean successful = response.isSuccessful();
                    response.close();
                    if (!stopped) {
                        if (successful) {
                            retryMillis = 250;
                            RETRIES.schedule(HTTPStreamSocket.this::startUp, 25, TimeUnit.MILLISECONDS);
                        } else {
                            retryUp(new EOFException("HTTP stream upload ended"));
                        }
                    }
                }
            });
        }

        private void retryDown(Throwable failure, Response response) {
            if (stopped) return;
            if (!opened.get() && response != null && (response.code() == 400 || response.code() == 401 || response.code() == 403 || response.code() == 404 || response.code() == 405)) {
                stopped = true;
                listener.onFailure(this, failure, response);
                return;
            }
            RETRIES.schedule(this::startDown, nextDelay(), TimeUnit.MILLISECONDS);
        }

        private void retryUp(Throwable ignored) {
            if (!stopped) RETRIES.schedule(this::startUp, nextDelay(), TimeUnit.MILLISECONDS);
        }

        private synchronized long nextDelay() {
            long value = retryMillis;
            retryMillis = Math.min(5000, retryMillis * 2);
            return value;
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
                    gate.notifyAll();
                }
                return;
            }
            synchronized (gate) {
                if (frame.sequence < nextReceive) return;
                if (frame.sequence != nextReceive) throw new ProtocolException("out-of-order HTTP stream record");
                nextReceive++;
                gate.notifyAll();
            }
            if (frame.kind == TEXT) {
                listener.onMessage(this, frame.payload.utf8());
            } else if (frame.kind == BINARY) {
                listener.onMessage(this, frame.payload);
            } else if (frame.kind == CLOSE) {
                int code = frame.payload.size() >= 2 ? ((frame.payload.getByte(0) & 0xff) << 8) | (frame.payload.getByte(1) & 0xff) : 1000;
                String reason = frame.payload.size() > 2 ? frame.payload.substring(2).utf8() : "";
                listener.onClosing(this, code, reason);
                stopped = true;
                listener.onClosed(this, code, reason);
            } else {
                throw new ProtocolException("invalid HTTP stream record type");
            }
        }

        private boolean queue(int kind, ByteString payload) {
            synchronized (gate) {
                if (stopped || buffered + payload.size() > MAX_BUFFERED) return false;
                outgoing.add(new Frame(kind, nextSend++, payload));
                buffered += payload.size();
                gate.notifyAll();
                return true;
            }
        }

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
            Call up = upCall;
            Call down = downCall;
            if (up != null) up.cancel();
            if (down != null) down.cancel();
            synchronized (gate) { gate.notifyAll(); }
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
