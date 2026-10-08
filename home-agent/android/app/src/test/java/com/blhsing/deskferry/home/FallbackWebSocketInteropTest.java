package com.blhsing.deskferry.home;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;
import static org.junit.Assume.assumeTrue;

import java.net.InetSocketAddress;
import java.net.Proxy;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.UUID;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.TimeUnit;

import okhttp3.OkHttpClient;
import okhttp3.Request;
import okhttp3.Response;
import okhttp3.WebSocket;
import okhttp3.WebSocketListener;

import org.junit.Test;

/**
 * Exercises the HTTP-stream fallback against a real DeskFerry relay. Set
 * DESKFERRY_TEST_RELAY to a relay base such as http://127.0.0.1:18082/relay and
 * DESKFERRY_TEST_PROXY to host:port of a forward proxy that cannot upgrade
 * WebSockets, so the native WebSocket fails and the fallback carries traffic.
 */
public class FallbackWebSocketInteropTest {
    @Test
    public void controlPingsRoundTripOverHttpStream() throws Exception {
        String relay = System.getenv("DESKFERRY_TEST_RELAY");
        String proxy = System.getenv("DESKFERRY_TEST_PROXY");
        assumeTrue("DESKFERRY_TEST_RELAY and DESKFERRY_TEST_PROXY are not set", relay != null && proxy != null);
        String[] proxyParts = proxy.split(":");
        OkHttpClient client = new OkHttpClient.Builder()
                .proxy(new Proxy(Proxy.Type.HTTP, new InetSocketAddress(proxyParts[0], Integer.parseInt(proxyParts[1]))))
                .readTimeout(30, TimeUnit.SECONDS)
                .build();
        String room = "android-" + UUID.randomUUID().toString().substring(0, 8);
        Request request = new Request.Builder()
                .url(relay.replaceFirst("^http", "ws") + "/" + room + "/ws")
                .header("X-DeskFerry-Role", "agent-control")
                .header("X-DeskFerry-Protocol", "2")
                .header("X-DeskFerry-Agent-Instance", room)
                .header("X-DeskFerry-Agent-Services", "rdp")
                .header("X-DeskFerry-Concurrency", "1")
                .build();
        BlockingQueue<String> messages = new LinkedBlockingQueue<>();
        BlockingQueue<Throwable> failures = new LinkedBlockingQueue<>();
        FallbackWebSocket socket = new FallbackWebSocket(client, request, new WebSocketListener() {
            @Override public void onMessage(WebSocket webSocket, String text) { messages.add(text); }
            @Override public void onFailure(WebSocket webSocket, Throwable failure, Response response) { failures.add(failure); }
        });
        try {
            String ready = messages.poll(30, TimeUnit.SECONDS);
            assertNotNull("no control-ready: " + failures.peek(), ready);
            assertTrue(ready, ready.contains("control-ready"));
            assertEquals("http-stream", socket.protocol());

            List<Long> idle = new ArrayList<>();
            for (int i = 0; i < 15; i++) {
                Thread.sleep(100 + (i * 37) % 300);
                long started = System.nanoTime();
                assertTrue(socket.send("{\"type\":\"control-ping\"}"));
                awaitPong(messages);
                idle.add((System.nanoTime() - started) / 1_000_000);
            }

            int count = 120;
            long[] sent = new long[count];
            Thread sender = new Thread(() -> {
                for (int i = 0; i < count; i++) {
                    sent[i] = System.nanoTime();
                    socket.send("{\"type\":\"control-ping\"}");
                    try { Thread.sleep(10); } catch (InterruptedException ignored) { return; }
                }
            });
            sender.start();
            List<Long> sustained = new ArrayList<>();
            for (int i = 0; i < count; i++) {
                awaitPong(messages);
                sustained.add((System.nanoTime() - sent[i]) / 1_000_000);
            }
            sender.join();
            System.out.println("android http-stream idle " + summary(idle) + "  sustained " + summary(sustained));
            assertTrue("unexpected failure: " + failures.peek(), failures.isEmpty());
        } finally {
            socket.cancel();
        }
    }

    private static void awaitPong(BlockingQueue<String> messages) throws InterruptedException {
        while (true) {
            String message = messages.poll(20, TimeUnit.SECONDS);
            assertNotNull("pong timeout", message);
            if (message.contains("control-pong")) return;
        }
    }

    private static String summary(List<Long> samples) {
        List<Long> sorted = new ArrayList<>(samples);
        Collections.sort(sorted);
        return "p50=" + sorted.get(sorted.size() / 2) + "ms p90=" + sorted.get(sorted.size() * 9 / 10) + "ms max=" + sorted.get(sorted.size() - 1) + "ms";
    }
}
