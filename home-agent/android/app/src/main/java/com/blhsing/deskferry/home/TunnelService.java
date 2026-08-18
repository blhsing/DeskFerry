package com.blhsing.deskferry.home;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.net.ConnectivityManager;
import android.net.Network;
import android.net.NetworkCapabilities;
import android.net.NetworkRequest;
import android.os.Build;
import android.os.IBinder;
import android.os.SystemClock;
import android.provider.Settings;
import android.util.Log;

import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.net.URISyntaxException;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.text.SimpleDateFormat;
import java.util.Date;
import java.util.Calendar;
import java.util.Arrays;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Locale;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.Semaphore;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicReference;

import okhttp3.OkHttpClient;
import okhttp3.Request;
import okhttp3.Response;
import okhttp3.WebSocket;
import okhttp3.WebSocketListener;
import okio.ByteString;

import org.json.JSONArray;
import org.json.JSONObject;

public class TunnelService extends Service {
    static final String ACTION_START = "com.blhsing.deskferry.home.START";
    static final String ACTION_STOP = "com.blhsing.deskferry.home.STOP";
    static final String ACTION_STATE = "com.blhsing.deskferry.home.STATE";
    static final String EXTRA_RELAY_URL = "relay_url";
    static final String EXTRA_LOCAL_PORT = "local_port";
	static final String EXTRA_LOCAL_SMB_PORT = "local_smb_port";
    static final String EXTRA_PROXY = "proxy";
    static final String EXTRA_LOG_RETENTION_DAYS = "log_retention_days";
    static final String EXTRA_ROOM_PROOF = "room_proof";

    private static final String CHANNEL_ID = "deskferry_home";
    private static final int NOTIFICATION_ID = 7310;
    private static final String LOG_TAG = "DeskFerryHome";
    private static final long MAX_DIAGNOSTIC_LOG_BYTES = 8L * 1024L * 1024L;
    private static final int RESUMABLE_MAX_BUFFER = 8 * 1024 * 1024;
    private static final int RESUMABLE_CHUNK_SIZE = 64 * 1024;
    private static final long RESUMABLE_WINDOW_MS = 5L * 60L * 1000L;
	private static final long HEARTBEAT_INTERVAL_MS = 15L * 1000L;
	private static final long HEARTBEAT_TIMEOUT_MS = 45L * 1000L;
	private static final long RESUME_ATTEMPT_TIMEOUT_MS = 20L * 1000L;
	private static final int MAX_CONCURRENT_BRIDGES_PER_SERVICE = 2;
    private static final SimpleDateFormat TIME_FORMAT = new SimpleDateFormat("HH:mm:ss", Locale.ROOT);
    private static final SimpleDateFormat DIAGNOSTIC_TIME_FORMAT = new SimpleDateFormat("yyyy-MM-dd HH:mm:ss.SSS", Locale.ROOT);
    private static final SimpleDateFormat DIAGNOSTIC_DATE_FORMAT = new SimpleDateFormat("yyyy-MM-dd", Locale.ROOT);
    private static final Object DIAGNOSTIC_LOG_LOCK = new Object();
    private static final Object STATE_LOCK = new Object();
    private static final int MAX_REMOTE_LOG_LINES = 2000;
    private static final int MAX_REMOTE_LOG_BYTES = 1024 * 1024;
    private static State currentState = State.initial();

    private final Object lock = new Object();
    private final Set<BridgeSession> sessions = Collections.newSetFromMap(new ConcurrentHashMap<BridgeSession, Boolean>());
	private final Semaphore rdpBridgePermits = new Semaphore(MAX_CONCURRENT_BRIDGES_PER_SERVICE, true);
	private final Semaphore smbBridgePermits = new Semaphore(MAX_CONCURRENT_BRIDGES_PER_SERVICE, true);
    private final Object networkLock = new Object();
    private final Object remoteLogLock = new Object();
    private final ArrayDeque<RemoteLogLine> remoteLogLines = new ArrayDeque<>();
    private final List<DiagnosticUploader> diagnosticUploaders = new ArrayList<>();
    private long nextRemoteLogSequence = 1;
    private int remoteLogBytes;
    private OkHttpClient httpClient;
    private ServerSocket serverSocket;
	private ServerSocket smbServerSocket;
    private Thread acceptThread;
    private Thread presenceThread;
    private Thread statusThread;
    private volatile WebSocket presenceSocket;
    private volatile WebSocket statusSocket;
    private ConnectivityManager connectivityManager;
    private ConnectivityManager.NetworkCallback networkCallback;
    private Network activeNetwork;
    private boolean networkWasLost;
    private volatile boolean running;
    private volatile String relayUrl = RelayUrls.DEFAULT_RELAY_URL;
    private volatile List<String> relayUrls = Collections.singletonList(RelayUrls.DEFAULT_RELAY_URL);
    private volatile int localPort = HomePrefs.DEFAULT_LOCAL_PORT;
	private volatile int localSMBPort = HomePrefs.DEFAULT_LOCAL_SMB_PORT;
    private volatile String roomProof = "";
    private volatile int logRetentionDays = HomePrefs.DEFAULT_LOG_RETENTION_DAYS;
    private volatile String lastPrunedLogDate = "";
    private volatile int activeConnections;
    private volatile int totalConnections;

    public static State snapshot() {
        synchronized (STATE_LOCK) {
            return currentState.copy();
        }
    }

    @Override
    public void onCreate() {
        super.onCreate();
        createNotificationChannel();
        registerNetworkObserver();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        String action = intent == null ? ACTION_START : intent.getAction();
        if (ACTION_STOP.equals(action)) {
            stopTunnel();
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
                stopForeground(STOP_FOREGROUND_REMOVE);
            } else {
                stopForeground(true);
            }
            stopSelf();
            return START_NOT_STICKY;
        }

        String requestedRelay = intent != null && intent.hasExtra(EXTRA_RELAY_URL)
                ? intent.getStringExtra(EXTRA_RELAY_URL)
                : HomePrefs.loadRelayUrl(this);
        int requestedPort = intent != null && intent.hasExtra(EXTRA_LOCAL_PORT)
                ? intent.getIntExtra(EXTRA_LOCAL_PORT, HomePrefs.DEFAULT_LOCAL_PORT)
                : HomePrefs.loadLocalPort(this);
		int requestedSMBPort = intent != null && intent.hasExtra(EXTRA_LOCAL_SMB_PORT)
				? intent.getIntExtra(EXTRA_LOCAL_SMB_PORT, HomePrefs.DEFAULT_LOCAL_SMB_PORT)
				: HomePrefs.loadLocalSMBPort(this);
        String requestedProxy = intent != null && intent.hasExtra(EXTRA_PROXY)
                ? intent.getStringExtra(EXTRA_PROXY)
                : HomePrefs.loadProxy(this);
        int requestedLogRetentionDays = intent != null && intent.hasExtra(EXTRA_LOG_RETENTION_DAYS)
                ? intent.getIntExtra(EXTRA_LOG_RETENTION_DAYS, HomePrefs.DEFAULT_LOG_RETENTION_DAYS)
                : HomePrefs.loadLogRetentionDays(this);
        String requestedRoomProof = intent != null && intent.hasExtra(EXTRA_ROOM_PROOF)
                ? intent.getStringExtra(EXTRA_ROOM_PROOF)
                : HomePrefs.loadSelectedRoomProof(this);
        startForeground(NOTIFICATION_ID, buildNotification());
		startTunnel(requestedRelay, requestedPort, requestedSMBPort, requestedProxy, requestedLogRetentionDays, requestedRoomProof);
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        stopTunnel();
        unregisterNetworkObserver();
        if (httpClient != null) {
            httpClient.dispatcher().cancelAll();
        }
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

	private void startTunnel(String requestedRelay, int requestedPort, int requestedSMBPort, String requestedProxy, int requestedLogRetentionDays,
                             String requestedRoomProof) {
        synchronized (lock) {
            stopTunnelLocked();
            try {
                relayUrls = RelayUrls.normalizeRelayUrls(requestedRelay);
                relayUrl = RelayUrls.joinRelayUrls(relayUrls);
                localPort = sanitizePort(requestedPort);
				localSMBPort = HomePrefs.sanitizeSMBPort(requestedSMBPort);
				if (localSMBPort == localPort) {
					throw new IllegalArgumentException("local SMB and RDP ports must be different");
				}
                roomProof = requestedRoomProof == null ? "" : requestedRoomProof.trim();
                logRetentionDays = HomePrefs.sanitizeLogRetentionDays(requestedLogRetentionDays);
                OkHttpClient.Builder clientBuilder = new OkHttpClient.Builder()
                        .pingInterval(10, TimeUnit.SECONDS)
                        .retryOnConnectionFailure(true);
                ProxySettings.apply(clientBuilder, requestedProxy);
                httpClient = clientBuilder.build();
                serverSocket = new ServerSocket();
                serverSocket.setReuseAddress(true);
                serverSocket.bind(new InetSocketAddress(InetAddress.getByName("127.0.0.1"), localPort));
				if (!roomProof.isEmpty()) {
					smbServerSocket = new ServerSocket();
					smbServerSocket.setReuseAddress(true);
					smbServerSocket.bind(new InetSocketAddress(InetAddress.getByName("127.0.0.1"), localSMBPort));
				}
                running = true;
                activeConnections = 0;
                totalConnections = 0;
                updateState("Running", "Connecting", "Checking", null);
				append("DeskFerry Home Agent version=" + BuildConfig.VERSION_NAME + " platform=android.");
                append("Listening on " + RelayUrls.rdpAddress(localPort) + ".");
				if (smbServerSocket != null) {
					append("SMB forward listening on " + RelayUrls.rdpAddress(localSMBPort) + " for CX File Explorer.");
				} else {
					append("SMB forward disabled because this profile has no saved room password.");
				}
                append("Diagnostic log file: " + diagnosticLogFile().getAbsolutePath() + " retention_days=" + logRetentionDays + ".");
                append("Relay primary: " + relayUrls.get(0) + (relayUrls.size() > 1 ? " (" + (relayUrls.size() - 1) + " fallback)" : "") + ".");
                append("Proxy: " + ProxySettings.forLog(requestedProxy) + ".");
				startAcceptLoop(serverSocket, "rdp");
				if (smbServerSocket != null) {
					startAcceptLoop(smbServerSocket, "smb");
				}
                startDiagnosticUploaders();
                startPresenceLoop();
                startStatusLoop();
            } catch (Exception ex) {
                running = false;
                updateState("Stopped", "Offline", "Check relay", "Start failed: " + ex.getMessage());
                append("Start failed: " + ex.getMessage());
                stopTunnelLocked();
            }
        }
    }

    private void registerNetworkObserver() {
        connectivityManager = (ConnectivityManager) getSystemService(Context.CONNECTIVITY_SERVICE);
        if (connectivityManager == null) {
            return;
        }
        synchronized (networkLock) {
            activeNetwork = connectivityManager.getActiveNetwork();
            networkWasLost = activeNetwork == null;
        }
        networkCallback = new ConnectivityManager.NetworkCallback() {
            @Override
            public void onAvailable(Network network) {
                boolean changed;
                synchronized (networkLock) {
                    changed = networkWasLost || (activeNetwork != null && !activeNetwork.equals(network));
                    activeNetwork = network;
                    networkWasLost = false;
                }
                if (changed) {
                    handleNetworkTransition("Android network became available or changed");
                }
            }

            @Override
            public void onLost(Network network) {
                boolean changed = false;
                synchronized (networkLock) {
                    if (activeNetwork != null && activeNetwork.equals(network)) {
                        activeNetwork = null;
                        networkWasLost = true;
                        changed = true;
                    }
                }
                if (changed) {
                    handleNetworkTransition("Android network was lost");
                }
            }

            @Override
            public void onUnavailable() {
                boolean changed;
                synchronized (networkLock) {
                    changed = !networkWasLost;
                    activeNetwork = null;
                    networkWasLost = true;
                }
                if (changed) {
                    handleNetworkTransition("Android network is unavailable");
                }
            }
        };
        try {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
                connectivityManager.registerDefaultNetworkCallback(networkCallback);
            } else {
                NetworkRequest request = new NetworkRequest.Builder()
                        .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                        .build();
                connectivityManager.registerNetworkCallback(request, networkCallback);
            }
        } catch (RuntimeException ex) {
            Log.w(LOG_TAG, "Could not observe Android network changes", ex);
            networkCallback = null;
        }
    }

    private void unregisterNetworkObserver() {
        ConnectivityManager manager = connectivityManager;
        ConnectivityManager.NetworkCallback callback = networkCallback;
        networkCallback = null;
        if (manager != null && callback != null) {
            try {
                manager.unregisterNetworkCallback(callback);
            } catch (RuntimeException ignored) {
            }
        }
    }

    private void handleNetworkTransition(String reason) {
        if (!running) {
            return;
        }
        append(reason + "; replacing relay transports immediately.");
        cancelPresenceSocket();
        cancelStatusSocket();
        for (BridgeSession session : sessions) {
            session.replaceTransportForNetworkChange(reason);
        }
    }

    private void stopTunnel() {
        synchronized (lock) {
            stopTunnelLocked();
        }
        updateState("Stopped", "Offline", "Unknown", null);
        append("Tunnel stopped.");
    }

    private void stopTunnelLocked() {
        running = false;
        closePresenceSocket();
        closeStatusSocket();
        for (DiagnosticUploader uploader : diagnosticUploaders) {
            uploader.close();
        }
        diagnosticUploaders.clear();
        closeQuietly(serverSocket);
        serverSocket = null;
		closeQuietly(smbServerSocket);
		smbServerSocket = null;
        for (BridgeSession session : sessions) {
            session.close();
        }
        sessions.clear();
        activeConnections = 0;
    }

	private void startAcceptLoop(ServerSocket listener, String service) {
		Thread thread = new Thread(() -> {
            while (running) {
                try {
					Socket local = listener.accept();
                    local.setTcpNoDelay(true);
					BridgeSession session = new BridgeSession(local, service);
                    sessions.add(session);
					new Thread(session, "DeskFerry-Android-" + service.toUpperCase(Locale.ROOT) + "-Bridge").start();
                } catch (IOException ex) {
                    if (running) {
						append(service.toUpperCase(Locale.ROOT) + " local listener stopped: " + ex.getMessage());
                    }
                    return;
                }
            }
		}, "DeskFerry-Android-" + service.toUpperCase(Locale.ROOT) + "-Accept");
		if ("rdp".equals(service)) acceptThread = thread;
		thread.start();
    }

    private void startPresenceLoop() {
        presenceThread = new Thread(() -> {
            while (running) {
                boolean opened = false;
                String lastError = "";
                for (String candidate : relayUrlsSnapshot()) {
                    if (!running) {
                        break;
                    }
                    CountDownLatch closed = new CountDownLatch(1);
                    AtomicBoolean openedCandidate = new AtomicBoolean(false);
                    AtomicReference<String> failure = new AtomicReference<>("");
                    long attemptStarted = SystemClock.elapsedRealtime();
                    try {
                        Request request = webSocketRequest(candidate, "home-agent");
                        WebSocket socket = httpClient.newWebSocket(request, new WebSocketListener() {
                            @Override
                            public void onOpen(WebSocket webSocket, Response response) {
                                presenceSocket = webSocket;
                                openedCandidate.set(true);
                                updateState(null, "Online", null, null);
                                append("Home status connected to " + candidate + ".");
                            }

                            @Override
                            public void onClosed(WebSocket webSocket, int code, String reason) {
                                if (openedCandidate.get()) {
                                    append("Home status disconnected relay=" + candidate + " duration_ms=" + elapsedMillis(attemptStarted) + " close_code=" + code + " close_reason=" + quoted(reason) + ".");
                                }
                                closed.countDown();
                            }

                            @Override
                            public void onFailure(WebSocket webSocket, Throwable t, Response response) {
                                failure.set(t.getMessage());
                                append("Home status failure relay=" + candidate + " duration_ms=" + elapsedMillis(attemptStarted) + " error=" + throwableText(t) + " http_status=" + responseStatus(response) + ".");
                                if (running && openedCandidate.get()) {
                                    updateState(null, "Reconnecting", null, "Home status: " + t.getMessage());
                                }
                                closed.countDown();
                            }
                        });
                        presenceSocket = socket;
                        closed.await();
                    } catch (Exception ex) {
                        failure.set(ex.getMessage());
                    } finally {
                        closePresenceSocket();
                    }
                    if (openedCandidate.get()) {
                        opened = true;
                        break;
                    }
                    lastError = candidate + ": " + emptyAs(failure.get(), "connection failed");
                }
                if (running && !opened) {
                    updateState(null, "Reconnecting", null, "Home status: " + emptyAs(lastError, "all relay URLs failed"));
                }
                sleepQuietly(3000);
            }
        }, "DeskFerry-Android-Presence");
        presenceThread.start();
    }

    private void startStatusLoop() {
        statusThread = new Thread(() -> {
            while (running) {
                boolean opened = false;
                String lastError = "";
                for (String candidate : relayUrlsSnapshot()) {
                    if (!running) {
                        break;
                    }
                    CountDownLatch closed = new CountDownLatch(1);
                    AtomicBoolean openedCandidate = new AtomicBoolean(false);
                    AtomicReference<String> failure = new AtomicReference<>("");
                    long attemptStarted = SystemClock.elapsedRealtime();
                    try {
                        Request request = webSocketRequest(candidate, "dashboard");
                        WebSocket socket = httpClient.newWebSocket(request, new WebSocketListener() {
                            @Override
                            public void onOpen(WebSocket webSocket, Response response) {
                                statusSocket = webSocket;
                                openedCandidate.set(true);
                                updateState(null, null, "Checking", "Relay status stream connected to " + candidate + ".");
                            }

                            @Override
                            public void onMessage(WebSocket webSocket, String text) {
                                refreshRelayStatus(text);
                            }

                            @Override
                            public void onClosed(WebSocket webSocket, int code, String reason) {
                                if (openedCandidate.get()) {
                                    append("Relay status stream disconnected relay=" + candidate + " duration_ms=" + elapsedMillis(attemptStarted) + " close_code=" + code + " close_reason=" + quoted(reason) + ".");
                                }
                                closed.countDown();
                            }

                            @Override
                            public void onFailure(WebSocket webSocket, Throwable t, Response response) {
                                failure.set(t.getMessage());
                                append("Relay status stream failure relay=" + candidate + " duration_ms=" + elapsedMillis(attemptStarted) + " error=" + throwableText(t) + " http_status=" + responseStatus(response) + ".");
                                if (running && openedCandidate.get()) {
                                    updateState(null, null, "Check relay", "Relay status stream: " + t.getMessage());
                                }
                                closed.countDown();
                            }
                        });
                        statusSocket = socket;
                        closed.await();
                    } catch (Exception ex) {
                        failure.set(ex.getMessage());
                    } finally {
                        closeStatusSocket();
                    }
                    if (openedCandidate.get()) {
                        opened = true;
                        break;
                    }
                    lastError = candidate + ": " + emptyAs(failure.get(), "connection failed");
                }
                if (running && !opened) {
                    updateState(null, null, "Check relay", "Relay status stream: " + emptyAs(lastError, "all relay URLs failed"));
                }
                sleepQuietly(1500);
            }
        }, "DeskFerry-Android-Status");
        statusThread.start();
    }

    private void refreshRelayStatus(String payload) {
        try {
            JSONObject root = new JSONObject(payload);
            JSONArray rooms = root.optJSONArray("rooms");
            int waiting = 0;
            int active = 0;
            if (rooms != null) {
                for (int i = 0; i < rooms.length(); i++) {
                    JSONObject room = rooms.getJSONObject(i);
                    waiting += room.optInt("waiting_agents", 0);
                    active += room.optInt("active_pairs", 0);
                }
            }
            boolean online = waiting + active > 0;
            String detail = waiting + " waiting work sockets, " + active + " active streams.";
            updateState(null, null, online ? "Connected" : "Waiting", detail);
        } catch (Exception ex) {
            updateState(null, null, "Check relay", "Relay status stream: " + ex.getMessage());
        }
    }

    private Request webSocketRequest(String relayUrl, String role) throws URISyntaxException {
        String endpoint = RelayUrls.webSocketEndpoint(relayUrl);
        String token = RelayUrls.roomToken(relayUrl, "");
        Request.Builder request = new Request.Builder()
                .url(endpoint)
                .header("Authorization", "Bearer " + token)
                .header("X-DeskFerry-Role", role)
                .header("X-TunnelDesktop-Role", role)
				.header("User-Agent", "DeskFerry-Android/" + BuildConfig.VERSION_NAME);
        if (!roomProof.isEmpty() && !"dashboard".equals(role)) {
            request.header("X-DeskFerry-Room-Proof", roomProof);
        }
        if ("client".equals(role) || "resume".equals(role)) {
            request.header("X-DeskFerry-Service", "rdp");
        }
        if ("diagnostic-log".equals(role)) {
            request.header("X-DeskFerry-Log-Component", "home-agent-android");
            String instance = Settings.Secure.getString(getContentResolver(), Settings.Secure.ANDROID_ID);
            request.header("X-DeskFerry-Log-Instance", emptyAs(instance, Build.MODEL));
        }
        return request.build();
    }

    private List<String> relayUrlsSnapshot() {
        List<String> snapshot = relayUrls;
        return snapshot == null || snapshot.isEmpty()
                ? Collections.singletonList(RelayUrls.DEFAULT_RELAY_URL)
                : snapshot;
    }

    private void updateState(String tunnel, String home, String work, String message) {
        synchronized (STATE_LOCK) {
            State next = currentState.copy();
            next.running = running;
            next.relayUrl = relayUrl;
            next.rdpAddress = RelayUrls.rdpAddress(localPort);
			next.smbAddress = RelayUrls.rdpAddress(localSMBPort);
			next.smbEnabled = smbServerSocket != null;
            next.activeConnections = activeConnections;
            next.totalConnections = totalConnections;
            if (tunnel != null) {
                next.tunnelStatus = tunnel;
            }
            if (home != null) {
                next.homeStatus = home;
            }
            if (work != null) {
                next.workStatus = work;
            }
            if (message != null && !message.isEmpty()) {
                next.lastMessage = message;
            }
            currentState = next;
        }
        sendBroadcast(new Intent(ACTION_STATE).setPackage(getPackageName()));
        updateNotification();
    }

    private void append(String message) {
        String diagnosticLine = diagnosticNow() + " " + message;
        Log.i(LOG_TAG, message);
        writeDiagnosticLog(diagnosticLine + System.lineSeparator());
        queueRemoteLog(diagnosticLine);
        synchronized (STATE_LOCK) {
            State next = currentState.copy();
            next.lastMessage = message;
            next.log = now() + "  " + message + "\n" + trimLog(next.log);
            currentState = next;
        }
        sendBroadcast(new Intent(ACTION_STATE).setPackage(getPackageName()));
        updateNotification();
    }

    private void queueRemoteLog(String line) {
        if (line.length() > 8192) {
            line = line.substring(0, 8192);
        }
        synchronized (remoteLogLock) {
            remoteLogLines.addLast(new RemoteLogLine(nextRemoteLogSequence++, line));
            remoteLogBytes += line.length();
            while (remoteLogLines.size() > MAX_REMOTE_LOG_LINES || remoteLogBytes > MAX_REMOTE_LOG_BYTES) {
                remoteLogBytes -= remoteLogLines.removeFirst().text.length();
            }
            remoteLogLock.notifyAll();
        }
    }

    private void startDiagnosticUploaders() {
        for (String candidate : relayUrlsSnapshot()) {
            DiagnosticUploader uploader = new DiagnosticUploader(candidate);
            diagnosticUploaders.add(uploader);
            new Thread(uploader, "DeskFerry-Android-Logs").start();
        }
    }

    private static final class RemoteLogLine {
        final long sequence;
        final String text;
        RemoteLogLine(long sequence, String text) { this.sequence = sequence; this.text = text; }
    }

    private final class DiagnosticUploader implements Runnable {
        private final String target;
        private final LinkedBlockingQueue<Integer> acknowledgements = new LinkedBlockingQueue<>();
        private volatile WebSocket socket;
        private volatile boolean closed;
        private long cursor;

        DiagnosticUploader(String target) {
            this.target = target;
            synchronized (remoteLogLock) {
                cursor = remoteLogLines.isEmpty() ? nextRemoteLogSequence : remoteLogLines.getFirst().sequence;
            }
        }

        @Override public void run() {
            long backoff = 1000;
            while (running && !closed) {
                CountDownLatch opened = new CountDownLatch(1);
                CountDownLatch ended = new CountDownLatch(1);
                AtomicBoolean connected = new AtomicBoolean(false);
                try {
                    socket = httpClient.newWebSocket(webSocketRequest(target, "diagnostic-log"), new WebSocketListener() {
                        @Override public void onOpen(WebSocket webSocket, Response response) { connected.set(true); opened.countDown(); }
                        @Override public void onMessage(WebSocket webSocket, String text) {
                            try { acknowledgements.offer(new JSONObject(text).optInt("accepted", -1)); } catch (Exception ignored) { acknowledgements.offer(-1); }
                        }
                        @Override public void onClosed(WebSocket webSocket, int code, String reason) { opened.countDown(); ended.countDown(); }
                        @Override public void onFailure(WebSocket webSocket, Throwable t, Response response) { opened.countDown(); ended.countDown(); }
                    });
                    if (!opened.await(15, TimeUnit.SECONDS) || !connected.get()) throw new IOException("diagnostic WebSocket did not open");
                    backoff = 1000;
                    while (running && !closed && socket != null) {
                        List<RemoteLogLine> batch = remoteBatch(cursor);
                        if (batch.isEmpty()) {
                            synchronized (remoteLogLock) { remoteLogLock.wait(30000); }
                            continue;
                        }
                        JSONArray entries = new JSONArray();
                        for (RemoteLogLine item : batch) entries.put(item.text);
                        if (!socket.send(new JSONObject().put("entries", entries).toString())) break;
                        Integer accepted = acknowledgements.poll(15, TimeUnit.SECONDS);
                        if (accepted == null || accepted != batch.size()) break;
                        cursor = batch.get(batch.size() - 1).sequence + 1;
                    }
                } catch (Exception ignored) {
                } finally {
                    WebSocket current = socket;
                    socket = null;
                    if (current != null) current.cancel();
                }
                sleepQuietly(backoff);
                backoff = Math.min(30000, backoff * 2);
            }
        }

        private List<RemoteLogLine> remoteBatch(long from) {
            List<RemoteLogLine> batch = new ArrayList<>();
            synchronized (remoteLogLock) {
                if (!remoteLogLines.isEmpty() && from < remoteLogLines.getFirst().sequence) cursor = remoteLogLines.getFirst().sequence;
                for (RemoteLogLine item : remoteLogLines) {
                    if (item.sequence >= cursor) batch.add(item);
                    if (batch.size() == 100) break;
                }
            }
            return batch;
        }

        void close() {
            closed = true;
            WebSocket current = socket;
            if (current != null) current.cancel();
            synchronized (remoteLogLock) { remoteLogLock.notifyAll(); }
        }
    }

    private void updateNotification() {
        NotificationManager manager = (NotificationManager) getSystemService(Context.NOTIFICATION_SERVICE);
        if (manager != null) {
            manager.notify(NOTIFICATION_ID, buildNotification());
        }
    }

    private Notification buildNotification() {
        Intent openIntent = new Intent(this, MainActivity.class);
        PendingIntent pendingIntent = PendingIntent.getActivity(
                this,
                0,
                openIntent,
                PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);
        State state = snapshot();
        String title = state.running ? "DeskFerry Home is running" : "DeskFerry Home";
        String text = state.running ? state.rdpAddress + " - " + state.workStatus : "Tunnel stopped";
        Notification.Builder builder = Build.VERSION.SDK_INT >= 26
                ? new Notification.Builder(this, CHANNEL_ID)
                : new Notification.Builder(this);
        return builder
                .setSmallIcon(android.R.drawable.stat_sys_upload_done)
                .setContentTitle(title)
                .setContentText(text)
                .setContentIntent(pendingIntent)
                .setOngoing(state.running)
                .build();
    }

    private void createNotificationChannel() {
        if (Build.VERSION.SDK_INT < 26) {
            return;
        }
        NotificationChannel channel = new NotificationChannel(
                CHANNEL_ID,
                "DeskFerry Home",
                NotificationManager.IMPORTANCE_LOW);
        channel.setDescription("DeskFerry foreground tunnel status");
        NotificationManager manager = (NotificationManager) getSystemService(Context.NOTIFICATION_SERVICE);
        if (manager != null) {
            manager.createNotificationChannel(channel);
        }
    }

    private void closePresenceSocket() {
        WebSocket socket = presenceSocket;
        presenceSocket = null;
        if (socket != null) {
            socket.close(1000, "stopped");
        }
    }

    private void cancelPresenceSocket() {
        WebSocket socket = presenceSocket;
        presenceSocket = null;
        if (socket != null) {
            socket.cancel();
        }
    }

    private void closeStatusSocket() {
        WebSocket socket = statusSocket;
        statusSocket = null;
        if (socket != null) {
            socket.close(1000, "stopped");
        }
    }

    private void cancelStatusSocket() {
        WebSocket socket = statusSocket;
        statusSocket = null;
        if (socket != null) {
            socket.cancel();
        }
    }

    private static int sanitizePort(int port) {
        return HomePrefs.sanitizePort(port);
    }

    private static String trimLog(String log) {
        if (log == null) {
            return "";
        }
        return log.length() <= 6000 ? log : log.substring(0, 6000);
    }

    private static String now() {
        synchronized (TIME_FORMAT) {
            return TIME_FORMAT.format(new Date());
        }
    }

    private static String diagnosticNow() {
        synchronized (DIAGNOSTIC_TIME_FORMAT) {
            return DIAGNOSTIC_TIME_FORMAT.format(new Date());
        }
    }

    private File diagnosticLogFile() {
        File base = getExternalFilesDir(null);
        if (base == null) {
            base = getFilesDir();
        }
        return new File(new File(base, "logs"), "home-agent-" + diagnosticDate() + ".log");
    }

    private void writeDiagnosticLog(String line) {
        synchronized (DIAGNOSTIC_LOG_LOCK) {
            File file = diagnosticLogFile();
            File directory = file.getParentFile();
            if (directory == null || (!directory.isDirectory() && !directory.mkdirs())) {
                Log.e(LOG_TAG, "Could not create diagnostic log directory");
                return;
            }
            String today = diagnosticDate();
            if (!today.equals(lastPrunedLogDate)) {
                pruneDiagnosticLogs(directory);
                lastPrunedLogDate = today;
            }
            byte[] data = line.getBytes(StandardCharsets.UTF_8);
            if (file.length() > 0 && file.length() + data.length > MAX_DIAGNOSTIC_LOG_BYTES) {
                File old = new File(file.getPath() + ".old");
                if (old.exists() && !old.delete()) {
                    Log.w(LOG_TAG, "Could not remove previous diagnostic log");
                }
                if (!file.renameTo(old)) {
                    Log.w(LOG_TAG, "Could not rotate diagnostic log");
                }
            }
            try (FileOutputStream output = new FileOutputStream(file, true)) {
                output.write(data);
            } catch (IOException ex) {
                Log.e(LOG_TAG, "Could not write diagnostic log", ex);
            }
        }
    }

    private void pruneDiagnosticLogs(File directory) {
        Calendar cutoff = Calendar.getInstance();
        cutoff.set(Calendar.HOUR_OF_DAY, 0);
        cutoff.set(Calendar.MINUTE, 0);
        cutoff.set(Calendar.SECOND, 0);
        cutoff.set(Calendar.MILLISECOND, 0);
        cutoff.add(Calendar.DAY_OF_YEAR, -(logRetentionDays - 1));
        File[] files = directory.listFiles((dir, name) ->
                name.equals("home-agent.log") || name.equals("home-agent.log.old") ||
                        (name.startsWith("home-agent-") && (name.endsWith(".log") || name.endsWith(".log.old"))));
        if (files == null) {
            return;
        }
        for (File candidate : files) {
            if (candidate.lastModified() < cutoff.getTimeInMillis() && !candidate.delete()) {
                Log.w(LOG_TAG, "Could not remove expired diagnostic log " + candidate.getName());
            }
        }
    }

    private static String diagnosticDate() {
        synchronized (DIAGNOSTIC_DATE_FORMAT) {
            return DIAGNOSTIC_DATE_FORMAT.format(new Date());
        }
    }

    private static long elapsedMillis(long started) {
        return Math.max(0, SystemClock.elapsedRealtime() - started);
    }

    private static String throwableText(Throwable throwable) {
        if (throwable == null) {
            return "none";
        }
        return throwable.getClass().getSimpleName() + ": " + emptyAs(throwable.getMessage(), "no message");
    }

    private static String responseStatus(Response response) {
        return response == null ? "none" : response.code() + " " + response.message();
    }

    private static String quoted(String value) {
        return "\"" + emptyAs(value, "").replace("\r", " ").replace("\n", " ") + "\"";
    }

    private static void sleepQuietly(long millis) {
        try {
            Thread.sleep(millis);
        } catch (InterruptedException ex) {
            Thread.currentThread().interrupt();
        }
    }

    private static void closeQuietly(ServerSocket socket) {
        if (socket != null) {
            try {
                socket.close();
            } catch (IOException ignored) {
            }
        }
    }

    private static void closeQuietly(Socket socket) {
        if (socket != null) {
            try {
                socket.close();
            } catch (IOException ignored) {
            }
        }
    }

    private static String emptyAs(String value, String fallback) {
        return value == null || value.trim().isEmpty() ? fallback : value;
    }

    static final class State {
        boolean running;
        String relayUrl;
        String rdpAddress;
		String smbAddress;
		boolean smbEnabled;
        String tunnelStatus;
        String homeStatus;
        String workStatus;
        String lastMessage;
        String log;
        int activeConnections;
        int totalConnections;

        static State initial() {
            State state = new State();
            state.running = false;
            state.relayUrl = RelayUrls.DEFAULT_RELAY_URL;
            state.rdpAddress = RelayUrls.rdpAddress(HomePrefs.DEFAULT_LOCAL_PORT);
			state.smbAddress = RelayUrls.rdpAddress(HomePrefs.DEFAULT_LOCAL_SMB_PORT);
			state.smbEnabled = false;
            state.tunnelStatus = "Stopped";
            state.homeStatus = "Offline";
            state.workStatus = "Unknown";
            state.lastMessage = "Ready.";
            state.log = "";
            return state;
        }

        State copy() {
            State state = new State();
            state.running = running;
            state.relayUrl = relayUrl;
            state.rdpAddress = rdpAddress;
			state.smbAddress = smbAddress;
			state.smbEnabled = smbEnabled;
            state.tunnelStatus = tunnelStatus;
            state.homeStatus = homeStatus;
            state.workStatus = workStatus;
            state.lastMessage = lastMessage;
            state.log = log;
            state.activeConnections = activeConnections;
            state.totalConnections = totalConnections;
            return state;
        }
    }

    private final class BridgeSession implements Runnable {
        private final Socket localSocket;
		private final String service;
		private final String serviceLabel;
		private final Semaphore permits;
        private final AtomicBoolean closed = new AtomicBoolean(false);
        private final AtomicLong localToRelayBytes = new AtomicLong();
        private final AtomicLong localToRelayMessages = new AtomicLong();
        private final AtomicLong relayToLocalBytes = new AtomicLong();
        private final AtomicLong relayToLocalMessages = new AtomicLong();
        private final AtomicReference<String> termination = new AtomicReference<>("running");
        private final AtomicBoolean reconnecting = new AtomicBoolean(false);
        private final Object resumeLock = new Object();
        private final long startedAt = SystemClock.elapsedRealtime();
        private volatile WebSocket webSocket;
        private volatile WebSocket pendingResumeSocket;
		private volatile CountDownLatch pendingResumeReady;
        private volatile String selectedRelay = "none";
        private volatile String sessionId = "";
        private byte[] sendBuffer = new byte[0];
        private long sendBase;
        private long sendEnd;
        private long receiveOffset;
		private boolean heartbeatEnabled;
		private long transportGeneration;
		private long heartbeatNonce;
		private long heartbeatAcknowledged;

		BridgeSession(Socket localSocket, String service) {
            this.localSocket = localSocket;
			this.service = "smb".equals(service) ? "smb" : "rdp";
			this.serviceLabel = this.service.toUpperCase(Locale.ROOT);
			this.permits = "smb".equals(this.service) ? smbBridgePermits : rdpBridgePermits;
        }

        @Override
        public void run() {
            String remote = String.valueOf(localSocket.getRemoteSocketAddress());
            boolean permitAcquired = false;
            activeConnections++;
            totalConnections++;
            updateState("Running", null, null, null);
			append(serviceLabel + " connection from " + remote + ".");
            try {
				if (!permits.tryAcquire()) {
					append(serviceLabel + " connection from " + remote + " queued behind active/retry sockets.");
					permits.acquire();
                }
                permitAcquired = true;
                if (closed.get()) {
                    return;
                }
                boolean connected = false;
                String lastError = "";
                for (String candidate : relayUrlsSnapshot()) {
                    if (closed.get()) {
                        break;
                    }
                    try {
                        long dialStarted = SystemClock.elapsedRealtime();
                        connectRelay(candidate);
                        selectedRelay = candidate;
						append("Relay attempt selected relay=" + candidate + " service=" + service + " protocol_v2=true elapsed_ms=" + elapsedMillis(dialStarted) + ".");
                    } catch (Exception ex) {
                        lastError = candidate + ": " + ex.getMessage();
                        closeWebSocketOnly();
                        if (!closed.get()) {
							append("Relay attempt failed relay=" + candidate + " service=" + service + " result=" + relayAttemptResult(ex) + " error=" + ex.getMessage() + ".");
                        }
                        if (ex instanceof SessionRejectedException && ((SessionRejectedException) ex).terminal()) {
                            throw ex;
                        }
                        continue;
                    }
					append("Bridging local " + serviceLabel + " connection from " + remote + " through " + candidate + ".");
                    connected = true;
                    break;
                }
                if (connected) {
                    pipeLocalToRelay();
                }
                if (!connected && !closed.get()) {
                    recordTermination("all_relay_urls_failed error=" + emptyAs(lastError, "unknown"));
					append(serviceLabel + " bridge failed: " + emptyAs(lastError, "all relay URLs failed"));
                }
            } catch (Exception ex) {
                recordTermination("bridge_error=" + throwableText(ex));
                if (!closed.get()) {
					append(serviceLabel + " bridge failed: " + ex.getMessage());
                }
            } finally {
                if (permitAcquired) {
					permits.release();
                }
                close();
				append(serviceLabel + " session remote=" + remote + " relay=" + selectedRelay + " ended duration_ms=" + elapsedMillis(startedAt) + " termination=" + termination.get() + " local_to_relay_bytes=" + localToRelayBytes.get() + " local_to_relay_messages=" + localToRelayMessages.get() + " relay_to_local_bytes=" + relayToLocalBytes.get() + " relay_to_local_messages=" + relayToLocalMessages.get() + ".");
            }
        }

        private void connectRelay(String candidate) throws Exception {
            CountDownLatch paired = new CountDownLatch(1);
            AtomicBoolean started = new AtomicBoolean(false);
            AtomicReference<Throwable> failure = new AtomicReference<>();
            Request request = webSocketRequest(candidate, "client").newBuilder()
					.header("X-DeskFerry-Service", service)
                    .header("X-DeskFerry-Resumable", "1")
					.header("X-DeskFerry-Heartbeat", "1")
                    .header("X-DeskFerry-Protocol", "2")
                    .build();
            WebSocket socket = httpClient.newWebSocket(request, bridgeListener(paired, started, failure, true));
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

        private WebSocketListener bridgeListener(CountDownLatch ready, AtomicBoolean started, AtomicReference<Throwable> failure, boolean initial) {
            return new WebSocketListener() {
                @Override
                public void onMessage(WebSocket socket, String text) {
                    String value = text.trim();
                    if (initial && ("start".equals(value) || value.startsWith("start ") || value.startsWith("{"))) {
                        if (value.startsWith("{")) {
                            try {
                                JSONObject message = new JSONObject(value);
                                String result = message.optString("type", "invalid-request");
                                if (!"session-ready".equals(result)) {
                                    failure.set(new SessionRejectedException(result, message.optString("reason", "")));
                                    ready.countDown();
                                    return;
                                }
								String confirmedService = message.optString("service", "").trim();
								if (!confirmedService.isEmpty() && !service.equals(confirmedService)) {
									failure.set(new IOException("relay confirmed " + confirmedService + " instead of " + service));
									ready.countDown();
									return;
								}
                                sessionId = message.optString("session_id", "").trim();
								heartbeatEnabled = message.optBoolean("heartbeat", false);
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
                        recordTermination("relay_to_local_write_error=" + throwableText(ex));
                        close();
                    }
                }

                @Override
                public void onClosed(WebSocket socket, int code, String reason) {
                    if (!started.get()) {
                        if (failure.get() == null) {
                            IOException error = !initial && (code == 1000 || code == 1008)
                                    ? new TerminalResumeException("relay resume rejected code=" + code + " reason=" + reason)
                                    : new IOException("relay closed before pairing code=" + code + " reason=" + reason);
                            failure.set(error);
                        }
                        ready.countDown();
                        return;
                    }
                    if (!sessionId.isEmpty() && code != 1008 && !isLogicalSessionClose(code, reason) && !closed.get()) {
                        markTransportLost(socket, "websocket_closed code=" + code + " reason=" + quoted(reason));
                    } else {
                        recordTermination("websocket_closed code=" + code + " reason=" + quoted(reason));
                        close();
                    }
                }

                @Override
                public void onFailure(WebSocket socket, Throwable t, Response response) {
                    int status = response == null ? 0 : response.code();
                    Throwable reported = !initial && (status == 401 || status == 403)
                            ? new TerminalResumeException("relay resume authentication failed http_status=" + status, t)
                            : t;
                    failure.set(reported);
                    ready.countDown();
                    if (!started.get()) {
                        return;
                    }
                    if (!sessionId.isEmpty() && status != 401 && status != 403 && !closed.get()) {
                        markTransportLost(socket, "websocket_failure error=" + throwableText(t) + " http_status=" + responseStatus(response));
                    } else {
                        recordTermination("websocket_failure error=" + throwableText(t) + " http_status=" + responseStatus(response));
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

        private final class SessionRejectedException extends IOException {
            final String result;

            SessionRejectedException(String result, String reason) {
                super("relay session rejected: " + result + (reason.isEmpty() ? "" : " (" + reason + ")"));
                this.result = result;
            }

            boolean terminal() {
                return "authentication-failed".equals(result) || "service-disabled".equals(result) || "invalid-request".equals(result);
            }
        }

        private final class TerminalResumeException extends IOException {
            TerminalResumeException(String message) {
                super(message);
            }

            TerminalResumeException(String message, Throwable cause) {
                super(message, cause);
            }
        }

        private boolean attachTransport(WebSocket socket) {
			long generation;
            synchronized (resumeLock) {
                if (closed.get()) {
                    return false;
                }
                webSocket = socket;
				generation = ++transportGeneration;
                if (!socket.send(frame((byte) 2, receiveOffset, null))) {
                    webSocket = null;
                    return false;
                }
                int position = 0;
                long offset = sendBase;
                while (position < sendBuffer.length) {
                    int size = Math.min(RESUMABLE_CHUNK_SIZE, sendBuffer.length - position);
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
			if (heartbeatEnabled) {
				startHeartbeat(socket, generation);
			}
			return true;
        }

		private void startHeartbeat(WebSocket socket, long generation) {
			new Thread(() -> {
				while (!closed.get()) {
					sleepQuietly(HEARTBEAT_INTERVAL_MS);
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
					long deadline = SystemClock.elapsedRealtime() + HEARTBEAT_TIMEOUT_MS;
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
				}
			}, "DeskFerry-" + serviceLabel + "-Heartbeat").start();
		}

        private void markTransportLost(WebSocket socket, String reason) {
            synchronized (resumeLock) {
                if (webSocket != socket || closed.get()) {
                    return;
                }
                webSocket = null;
                resumeLock.notifyAll();
            }
			append(serviceLabel + " relay stream interrupted; retrying transparently: " + reason);
            startReconnectLoop();
        }

        private void replaceTransportForNetworkChange(String reason) {
            WebSocket active;
            WebSocket pending;
			CountDownLatch pendingReady;
            synchronized (resumeLock) {
                if (closed.get()) {
                    return;
                }
                active = webSocket;
                webSocket = null;
                pending = pendingResumeSocket;
                pendingResumeSocket = null;
				pendingReady = pendingResumeReady;
				pendingResumeReady = null;
                resumeLock.notifyAll();
            }
            if (active != null) {
                active.cancel();
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
                long deadline = SystemClock.elapsedRealtime() + RESUMABLE_WINDOW_MS;
                long backoff = 250;
                try {
                    while (!closed.get() && SystemClock.elapsedRealtime() < deadline) {
                        CountDownLatch ready = new CountDownLatch(1);
                        AtomicBoolean started = new AtomicBoolean(false);
                        AtomicReference<Throwable> failure = new AtomicReference<>();
						long attemptStarted = SystemClock.elapsedRealtime();
                        try {
                            Request request = webSocketRequest(selectedRelay, "resume").newBuilder()
									.header("X-DeskFerry-Service", service)
                                    .header("X-DeskFerry-Session", sessionId)
                                    .header("X-DeskFerry-Session-Side", "client")
                                    .build();
                            WebSocket candidate = httpClient.newWebSocket(request, bridgeListener(ready, started, failure, false));
							synchronized (resumeLock) {
								pendingResumeSocket = candidate;
								pendingResumeReady = ready;
							}
                            long remaining = Math.max(1, deadline - SystemClock.elapsedRealtime());
							long attemptWait = resumeAttemptWaitMillis(remaining);
							boolean signaled = ready.await(attemptWait, TimeUnit.MILLISECONDS);
							if (signaled && started.get() && failure.get() == null) {
								clearPendingResume(candidate, ready);
								append(serviceLabel + " relay stream resumed through " + selectedRelay + ".");
                                return;
                            }
							clearPendingResume(candidate, ready);
                            candidate.cancel();
                            Throwable resumeFailure = failure.get();
                            if (resumeFailure instanceof TerminalResumeException) {
                                recordTermination("resume_rejected error=" + throwableText(resumeFailure));
								append(serviceLabel + " relay session cannot resume; closing the stale local connection: " + throwableText(resumeFailure));
                                close();
                                return;
                            }
							if (!closed.get()) {
								String result = !signaled ? "attempt_timeout" : "transport_failure";
								append(serviceLabel + " relay resume attempt failed relay=" + selectedRelay
										+ " result=" + result + " elapsed_ms=" + elapsedMillis(attemptStarted)
										+ (resumeFailure == null ? "" : " error=" + throwableText(resumeFailure)) + ".");
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
							if (!closed.get()) {
								append(serviceLabel + " relay resume attempt failed relay=" + selectedRelay
										+ " result=exception elapsed_ms=" + elapsedMillis(attemptStarted)
										+ " error=" + throwableText(ex) + ".");
							}
                        }
                        sleepQuietly(backoff);
                        backoff = Math.min(5000, backoff * 2);
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
                writeLocal(payload);
                return;
            }
            if (payload.length < 9 || payload.length > 9 + RESUMABLE_CHUNK_SIZE) {
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
            synchronized (resumeLock) {
                long end = offset + data.length;
                if (offset > receiveOffset) {
                    markTransportLost(socket, "resumable sequence gap");
                    return;
                }
                if (end > receiveOffset) {
                    int trim = (int) (receiveOffset - offset);
                    byte[] fresh = Arrays.copyOfRange(data, trim, data.length);
                    writeLocal(fresh);
                    receiveOffset += fresh.length;
                }
                WebSocket current = webSocket;
                if (current != null && !current.send(frame((byte) 2, receiveOffset, null))) {
                    markTransportLost(current, "acknowledgement send failed");
                }
            }
        }

        private void writeLocal(byte[] payload) throws IOException {
            OutputStream output = localSocket.getOutputStream();
            synchronized (output) {
                output.write(payload);
                output.flush();
            }
            relayToLocalBytes.addAndGet(payload.length);
            relayToLocalMessages.incrementAndGet();
        }

        private void applyAcknowledgement(long offset) {
            synchronized (resumeLock) {
                if (offset < sendBase || offset > sendEnd) {
                    WebSocket current = webSocket;
                    if (current != null) {
                        markTransportLost(current, "invalid resumable acknowledgement");
                    }
                    return;
                }
                int drop = (int) (offset - sendBase);
                sendBuffer = Arrays.copyOfRange(sendBuffer, drop, sendBuffer.length);
                sendBase = offset;
                resumeLock.notifyAll();
            }
        }

        private ByteString frame(byte type, long offset, byte[] payload) {
            int length = payload == null ? 0 : payload.length;
            ByteBuffer buffer = ByteBuffer.allocate(9 + length);
            buffer.put(type);
            buffer.putLong(offset);
            if (payload != null) {
                buffer.put(payload);
            }
            return ByteString.of(buffer.array());
        }

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
            closeQuietly(localSocket);
            sessions.remove(this);
            activeConnections = Math.max(0, activeConnections - 1);
            updateState("Running", null, null, null);
        }

        private void closeWebSocketOnly() {
            WebSocket socket = webSocket;
            webSocket = null;
            if (socket != null) {
                socket.close(1000, "retrying");
            }
        }

        private void pipeLocalToRelay() throws IOException {
            InputStream input = localSocket.getInputStream();
            byte[] buffer = new byte[16 * 1024];
            int read;
            while (!closed.get() && (read = input.read(buffer)) >= 0) {
                if (sessionId.isEmpty()) {
                    WebSocket socket = webSocket;
                    if (socket == null || !socket.send(ByteString.of(buffer, 0, read))) {
                        recordTermination("local_to_relay_send_failed");
                        throw new IOException("relay WebSocket send failed");
                    }
                    while (!closed.get() && socket.queueSize() > 4L * 1024L * 1024L) {
                        sleepQuietly(10);
                    }
                } else {
                    sendReliable(Arrays.copyOf(buffer, read));
                }
                localToRelayBytes.addAndGet(read);
                localToRelayMessages.incrementAndGet();
            }
            if (!closed.get()) {
                recordTermination("local_tcp_eof");
            }
        }

        private void sendReliable(byte[] payload) throws IOException {
            long offset;
            synchronized (resumeLock) {
                while (!closed.get() && sendBuffer.length + payload.length > RESUMABLE_MAX_BUFFER) {
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

        private void recordTermination(String cause) {
            termination.compareAndSet("running", cause);
        }
    }

	static long resumeAttemptWaitMillis(long remainingMillis) {
		return Math.max(1L, Math.min(RESUME_ATTEMPT_TIMEOUT_MS, remainingMillis));
	}

	static boolean isLogicalSessionClose(int code, String reason) {
		return code == 1000 && "session closed".equals(reason);
	}
}
