package com.blhsing.deskferry.home;

import org.junit.Test;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;

import okhttp3.Request;
import okhttp3.WebSocket;
import okhttp3.WebSocketListener;
import okio.ByteString;

import static org.junit.Assert.assertArrayEquals;
import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;
import static org.junit.Assert.fail;

/** Plays the relay with fake WebSockets to check pairing, data, drops and resume. */
public class ResumableRelayStreamTest {
    private static final String RELAY = "https://relay.example/relay/p";

    static {
        // org.json is a stub on the JVM; read the flat test messages directly.
        ResumableRelayStream.readyParser = json -> {
            ResumableRelayStream.SessionReady ready = new ResumableRelayStream.SessionReady();
            ready.type = field(json, "type", "invalid-request");
            ready.service = field(json, "service", "");
            ready.sessionId = field(json, "session_id", "");
            ready.heartbeat = json.contains("\"heartbeat\":true");
            return ready;
        };
    }

    private static String field(String json, String name, String fallback) {
        java.util.regex.Matcher matcher = java.util.regex.Pattern.compile("\"" + name + "\":\"([^\"]*)\"").matcher(json);
        return matcher.find() ? matcher.group(1) : fallback;
    }

    private static final class FakeSocket implements WebSocket {
        final Request request;
        final List<ByteString> sent = Collections.synchronizedList(new ArrayList<>());
        volatile boolean dead;

        FakeSocket(Request request) {
            this.request = request;
        }

        @Override public Request request() { return request; }
        @Override public long queueSize() { return 0; }
        @Override public boolean send(String text) { return !dead; }

        @Override
        public boolean send(ByteString bytes) {
            if (dead) return false;
            sent.add(bytes);
            return true;
        }

        @Override public boolean close(int code, String reason) { dead = true; return true; }
        @Override public void cancel() { dead = true; }
    }

    private static final class Opened {
        final FakeSocket socket;
        final WebSocketListener listener;

        Opened(FakeSocket socket, WebSocketListener listener) {
            this.socket = socket;
            this.listener = listener;
        }
    }

    private static final class FakeHost implements ResumableRelayStream.Host {
        final BlockingQueue<Opened> opened = new LinkedBlockingQueue<>();

        @Override public List<String> relayUrls() { return Collections.singletonList(RELAY); }

        @Override
        public Request request(String relayUrl, String role) {
            return new Request.Builder().url("https://relay.example/relay/p/ws").header("X-DeskFerry-Role", role).build();
        }

        @Override
        public WebSocket open(Request request, WebSocketListener listener) {
            FakeSocket socket = new FakeSocket(request);
            opened.add(new Opened(socket, listener));
            return socket;
        }

        @Override public void log(String line) { }

        Opened next() throws InterruptedException {
            Opened value = opened.poll(5, TimeUnit.SECONDS);
            assertNotNull("the stream did not open a WebSocket", value);
            return value;
        }
    }

    private static final class Frame {
        final byte type;
        final long offset;
        final byte[] payload;

        Frame(ByteString bytes) {
            ByteBuffer buffer = ByteBuffer.wrap(bytes.toByteArray());
            type = buffer.get();
            offset = buffer.getLong();
            payload = new byte[buffer.remaining()];
            buffer.get(payload);
        }
    }

    private static ByteString frame(int type, long offset, String payload) {
        return ResumableRelayStream.frame((byte) type, offset, payload == null ? null : payload.getBytes(StandardCharsets.UTF_8));
    }

    private static Frame awaitFrame(FakeSocket socket, int index) throws InterruptedException {
        for (int i = 0; i < 500 && socket.sent.size() <= index; i++) Thread.sleep(10);
        assertTrue("frame " + index + " was not sent", socket.sent.size() > index);
        return new Frame(socket.sent.get(index));
    }

    private static String awaitDelivered(ByteArrayOutputStream delivered, int length) throws InterruptedException {
        for (int i = 0; i < 500; i++) {
            synchronized (delivered) {
                if (delivered.size() >= length) return new String(delivered.toByteArray(), StandardCharsets.UTF_8);
            }
            Thread.sleep(10);
        }
        synchronized (delivered) {
            return new String(delivered.toByteArray(), StandardCharsets.UTF_8);
        }
    }

    private static String connectInBackground(ResumableRelayStream stream, AtomicReference<Throwable> failure, CountDownLatch done) {
        new Thread(() -> {
            try {
                stream.connect();
            } catch (Throwable error) {
                failure.set(error);
            } finally {
                done.countDown();
            }
        }).start();
        return RELAY;
    }

    @Test
    public void screenSessionResumesAndReplaysAfterATransportDrop() throws Exception {
        FakeHost host = new FakeHost();
        ByteArrayOutputStream delivered = new ByteArrayOutputStream();
        AtomicInteger closes = new AtomicInteger();
        ResumableRelayStream stream = new ResumableRelayStream(host, "screen", data -> {
            synchronized (delivered) {
                delivered.write(data);
            }
        }, termination -> closes.incrementAndGet());
        AtomicReference<Throwable> failure = new AtomicReference<>();
        CountDownLatch connected = new CountDownLatch(1);
        connectInBackground(stream, failure, connected);

        Opened first = host.next();
        assertEquals("client", first.socket.request.header("X-DeskFerry-Role"));
        assertEquals("screen", first.socket.request.header("X-DeskFerry-Service"));
        assertEquals("1", first.socket.request.header("X-DeskFerry-Resumable"));
        first.listener.onMessage(first.socket, "{\"type\":\"session-ready\",\"service\":\"screen\",\"session_id\":\"s1\",\"heartbeat\":false}");
        assertTrue(connected.await(5, TimeUnit.SECONDS));
        if (failure.get() != null) throw new AssertionError(failure.get());
        Frame hello = awaitFrame(first.socket, 0);
        assertEquals(2, hello.type);
        assertEquals(0, hello.offset);

        stream.writeNow("hello".getBytes(StandardCharsets.UTF_8));
        Frame data = awaitFrame(first.socket, 1);
        assertEquals(1, data.type);
        assertEquals(0, data.offset);
        assertArrayEquals("hello".getBytes(StandardCharsets.UTF_8), data.payload);

        first.listener.onMessage(first.socket, frame(1, 0, "world"));
        assertEquals("world", awaitDelivered(delivered, 5));
        Frame ack = awaitFrame(first.socket, 2);
        assertEquals(2, ack.type);
        assertEquals(5, ack.offset);

        // A proxy cuts the connection; data written meanwhile is held.
        first.listener.onFailure(first.socket, new IOException("cut"), null);
        Opened resume = host.next();
        assertEquals("resume", resume.socket.request.header("X-DeskFerry-Role"));
        assertEquals("s1", resume.socket.request.header("X-DeskFerry-Session"));
        assertEquals("screen", resume.socket.request.header("X-DeskFerry-Service"));
        stream.writeNow("more".getBytes(StandardCharsets.UTF_8));
        resume.listener.onMessage(resume.socket, "resume s1");

        // The new transport acknowledges what arrived and replays what the
        // Work side has not acknowledged.
        Frame resumedAck = awaitFrame(resume.socket, 0);
        assertEquals(2, resumedAck.type);
        assertEquals(5, resumedAck.offset);
        Frame replay = awaitFrame(resume.socket, 1);
        assertEquals(1, replay.type);
        assertEquals(0, replay.offset);
        assertArrayEquals("hellomore".getBytes(StandardCharsets.UTF_8), replay.payload);

        // Data the Work side resends from before the drop is delivered once.
        resume.listener.onMessage(resume.socket, frame(1, 0, "world!!"));
        assertEquals("world!!", awaitDelivered(delivered, 7));

        stream.close();
        stream.close();
        assertEquals(1, closes.get());
    }

    @Test
    public void screenSessionRequiresTheRelayToConfirmTheService() throws Exception {
        FakeHost host = new FakeHost();
        ResumableRelayStream stream = new ResumableRelayStream(host, "screen", data -> { }, termination -> { });
        AtomicReference<Throwable> failure = new AtomicReference<>();
        CountDownLatch done = new CountDownLatch(1);
        connectInBackground(stream, failure, done);
        Opened first = host.next();
        first.listener.onMessage(first.socket, "{\"type\":\"session-ready\",\"session_id\":\"s1\"}");
        assertTrue(done.await(5, TimeUnit.SECONDS));
        if (failure.get() == null) fail("an older relay that does not echo the screen service was accepted");
        stream.close();
    }
}
