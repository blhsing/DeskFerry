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
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
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

import okhttp3.ConnectionPool;
import okhttp3.Dispatcher;
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
    private static final long PRESENCE_CONFIRM_MILLIS = 2L * 1000L;
    static final int RESUMABLE_MAX_BUFFER = 8 * 1024 * 1024;
    static final int RESUMABLE_CHUNK_SIZE = 64 * 1024;
    static final long RESUMABLE_WINDOW_MS = 5L * 60L * 1000L;
    static final long HEARTBEAT_INTERVAL_MS = 5L * 1000L;
    static final long DIAGNOSTIC_STALL_MS = 3L * 1000L;
    static final long RDP_ACK_RECOVERY_MS = 5L * 1000L;
    // Proxy-routed HTTP-stream fallback has ~4.7s round trips and self-healing ack stalls of up to ~6s.
    static final long RDP_HTTP_STREAM_ACK_RECOVERY_MS = 8L * 1000L;
    static final long DEFAULT_ACK_RECOVERY_MS = 10L * 1000L;
    static final long HEARTBEAT_TIMEOUT_MS = 15L * 1000L;
    // Remote Desktop clients reset about 16s after data stops; notice a frozen path in time to resume.
    static final long RDP_HEARTBEAT_TIMEOUT_MS = 8L * 1000L;
    private static final long RESUME_ATTEMPT_TIMEOUT_MS = 8L * 1000L;
    private static final long FAILOVER_ATTEMPT_BASE_MS = 4L * 1000L;
    private static final long FAILOVER_ATTEMPT_STEP_MS = 3L * 1000L;
    private static final long FAILOVER_ATTEMPT_MAX_MS = 15L * 1000L;
    static final long FAILOVER_UNREACHABLE_ATTEMPT_MS = 12L * 1000L;
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
    private final Object powerLock = new Object();
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
    private PowerManager.WakeLock activeBridgeWakeLock;
    private int activeBridgeWakeLockUsers;
    private final Handler networkHandler = new Handler(Looper.getMainLooper());
    private Runnable pendingNetworkTransition;
    private volatile boolean running;
    private volatile String relayUrl = RelayUrls.DEFAULT_RELAY_URL;
    private volatile List<String> relayUrls = Collections.singletonList(RelayUrls.DEFAULT_RELAY_URL);
    private volatile int localPort = HomePrefs.DEFAULT_LOCAL_PORT;
	private volatile int localSMBPort = HomePrefs.DEFAULT_LOCAL_SMB_PORT;
    private volatile String roomProof = "";
    // Set when the relay closes the presence socket for a wrong room password.
    private volatile boolean roomAuthRejected;
    private volatile String lastRelayWorkStatus = "Checking";
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
                roomAuthRejected = false;
                logRetentionDays = HomePrefs.sanitizeLogRetentionDays(requestedLogRetentionDays);
                // Every relay socket shares this client. Over the HTTP-stream
                // fallback each socket keeps up to four GETs waiting and five
                // uploads in flight, so OkHttp's default of five concurrent
                // requests per host would queue new calls behind long polls.
                Dispatcher dispatcher = new Dispatcher();
                dispatcher.setMaxRequests(256);
                dispatcher.setMaxRequestsPerHost(128);
                OkHttpClient.Builder clientBuilder = new OkHttpClient.Builder()
                        .dispatcher(dispatcher)
                        .connectionPool(new ConnectionPool(64, 5, TimeUnit.MINUTES))
                        .readTimeout(30, TimeUnit.SECONDS)
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
        synchronized (networkLock) {
            if (pendingNetworkTransition != null) {
                networkHandler.removeCallbacks(pendingNetworkTransition);
                pendingNetworkTransition = null;
            }
        }
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
        // A handoff can deliver a burst of lost/available callbacks. Canceling
        // on each callback destroys the replacement sockets we just opened.
        // Socket failures still start recovery immediately; consolidate only
        // the proactive replacement triggered by connectivity notifications.
        synchronized (networkLock) {
            if (pendingNetworkTransition != null) {
                networkHandler.removeCallbacks(pendingNetworkTransition);
            }
            pendingNetworkTransition = () -> {
                synchronized (networkLock) {
                    pendingNetworkTransition = null;
                }
                replaceNetworkTransports(reason);
            };
            networkHandler.postDelayed(pendingNetworkTransition, 750);
        }
    }

    private void replaceNetworkTransports(String reason) {
        if (!running) {
            return;
        }
        append(reason + "; replacing relay transports after network callbacks settled.");
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

	private boolean acquireActiveBridgeWakeLock() {
		boolean firstUser = false;
		try {
			synchronized (powerLock) {
				if (activeBridgeWakeLock == null) {
					PowerManager powerManager = (PowerManager) getSystemService(Context.POWER_SERVICE);
					if (powerManager == null) {
						return false;
					}
					activeBridgeWakeLock = powerManager.newWakeLock(
							PowerManager.PARTIAL_WAKE_LOCK, "DeskFerry:ActiveBridge");
					activeBridgeWakeLock.setReferenceCounted(false);
				}
				firstUser = activeBridgeWakeLockUsers++ == 0;
				if (firstUser && !activeBridgeWakeLock.isHeld()) {
					activeBridgeWakeLock.acquire();
				}
			}
			if (firstUser) {
				append("Android partial wake lock acquired for active relay bridge recovery.");
			}
			return true;
		} catch (RuntimeException failure) {
			synchronized (powerLock) {
				if (firstUser && activeBridgeWakeLockUsers > 0) {
					activeBridgeWakeLockUsers--;
				}
			}
			append("Could not acquire Android partial wake lock: " + throwableText(failure) + ".");
			return false;
		}
	}

	private void releaseActiveBridgeWakeLock() {
		boolean lastUser = false;
		synchronized (powerLock) {
			if (activeBridgeWakeLockUsers <= 0) {
				return;
			}
			lastUser = --activeBridgeWakeLockUsers == 0;
			if (lastUser && activeBridgeWakeLock != null && activeBridgeWakeLock.isHeld()) {
				activeBridgeWakeLock.release();
			}
		}
		if (lastUser) {
			append("Android partial wake lock released; no active relay bridges remain.");
		}
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
                        WebSocket socket = newRelayWebSocket(request, new WebSocketListener() {
                            @Override
                            public void onOpen(WebSocket webSocket, Response response) {
                                presenceSocket = webSocket;
                                openedCandidate.set(true);
                                append("Home status connected to " + candidate + ".");
                            }

                            @Override
                            public void onClosed(WebSocket webSocket, int code, String reason) {
                                if (openedCandidate.get()) {
                                    append("Home status disconnected relay=" + candidate + " duration_ms=" + elapsedMillis(attemptStarted) + " close_code=" + code + " close_reason=" + quoted(reason) + ".");
                                }
                                if (running && isRoomAuthenticationFailure(code, reason)) {
                                    markRoomAuthRejected(candidate);
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
                        // The relay accepts the upgrade before it checks the
                        // room password, so a rejected socket opens and then
                        // closes at once. Report Online only once it stays up.
                        if (!closed.await(PRESENCE_CONFIRM_MILLIS, TimeUnit.MILLISECONDS) && openedCandidate.get()) {
                            boolean wasRejected = roomAuthRejected;
                            roomAuthRejected = false;
                            updateState(null, "Online", wasRejected ? lastRelayWorkStatus : null, null);
                        }
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
                        WebSocket socket = newRelayWebSocket(request, new WebSocketListener() {
                            @Override
                            public void onOpen(WebSocket webSocket, Response response) {
                                statusSocket = webSocket;
                                openedCandidate.set(true);
                                updateState(null, null, roomAuthRejected ? null : "Checking", "Relay status stream connected to " + candidate + ".");
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
            int controls = 0;
            int waiting = 0;
            int active = 0;
            if (rooms != null) {
                for (int i = 0; i < rooms.length(); i++) {
                    JSONObject room = rooms.getJSONObject(i);
                    controls += room.optInt("control_connections", 0);
                    waiting += room.optInt("waiting_agents", 0);
                    active += room.optInt("active_pairs", 0);
                }
            }
            String work = workAgentOnline(controls, waiting, active) ? "Connected" : "Waiting";
            lastRelayWorkStatus = work;
            if (roomAuthRejected) {
                // The dashboard stream is unauthenticated, so it reports the
                // Work agent even when this phone's room password is wrong.
                return;
            }
            String detail = controls + " work control connections, " + waiting + " legacy work sockets, " + active + " active streams.";
            updateState(null, null, work, detail);
        } catch (Exception ex) {
            updateState(null, null, "Check relay", "Relay status stream: " + ex.getMessage());
        }
    }

    /**
     * Protocol v2 Work agents hold agent-control sockets and open data sockets
     * on demand, so they appear only as control connections while idle.
     * Legacy agents park waiting sockets instead.
     */
    static boolean workAgentOnline(int controlConnections, int waitingAgents, int activePairs) {
        return controlConnections + waitingAgents + activePairs > 0;
    }

    static boolean isRoomAuthenticationFailure(int code, String reason) {
        return code == 1008 && reason != null && reason.contains("authentication failed");
    }

    private void markRoomAuthRejected(String relay) {
        if (!roomAuthRejected) {
            append("Relay rejected the room password relay=" + relay + ". Re-enter the room password in Settings.");
        }
        roomAuthRejected = true;
        updateState(null, "Rejected", "Rejected", "The relay rejected this room password.");
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

    private WebSocket newRelayWebSocket(Request request, WebSocketListener listener) {
        return new FallbackWebSocket(httpClient, request, listener);
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
                    socket = newRelayWebSocket(webSocketRequest(target, "diagnostic-log"), new WebSocketListener() {
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

    static long elapsedMillis(long started) {
        return Math.max(0, SystemClock.elapsedRealtime() - started);
    }

    static String throwableText(Throwable throwable) {
        if (throwable == null) {
            return "none";
        }
        return throwable.getClass().getSimpleName() + ": " + emptyAs(throwable.getMessage(), "no message");
    }

    static String responseStatus(Response response) {
        return response == null ? "none" : response.code() + " " + response.message();
    }

    static String quoted(String value) {
        return "\"" + emptyAs(value, "").replace("\r", " ").replace("\n", " ") + "\"";
    }

    static void sleepQuietly(long millis) {
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

    static String emptyAs(String value, String fallback) {
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

    /**
     * One local RDP or SMB connection carried over a resumable relay stream.
     * The stream does the relay work; this class adds the service's
     * bookkeeping (permits, wake lock, connection counts) and the local socket.
     */
    private final class BridgeSession implements Runnable, ResumableRelayStream.Host {
        private final Socket localSocket;
        private final String serviceLabel;
        private final Semaphore permits;
        private final long startedAt = SystemClock.elapsedRealtime();
        private final AtomicBoolean finished = new AtomicBoolean(false);
        private final ResumableRelayStream stream;

        BridgeSession(Socket localSocket, String service) {
            this.localSocket = localSocket;
            String normalized = "smb".equals(service) ? "smb" : "rdp";
            this.serviceLabel = normalized.toUpperCase(Locale.ROOT);
            this.permits = "smb".equals(normalized) ? smbBridgePermits : rdpBridgePermits;
            this.stream = new ResumableRelayStream(this, normalized, this::writeLocal, termination -> finish());
        }

        @Override
        public List<String> relayUrls() {
            return relayUrlsSnapshot();
        }

        @Override
        public Request request(String relayUrl, String role) throws Exception {
            return webSocketRequest(relayUrl, role);
        }

        @Override
        public WebSocket open(Request request, WebSocketListener listener) {
            return newRelayWebSocket(request, listener);
        }

        @Override
        public void log(String line) {
            append(line);
        }

        @Override
        public void run() {
            String remote = String.valueOf(localSocket.getRemoteSocketAddress());
            boolean permitAcquired = false;
            boolean wakeLockAcquired = acquireActiveBridgeWakeLock();
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
                if (stream.isClosed()) {
                    return;
                }
                String relay = stream.connect();
                append("Bridging local " + serviceLabel + " connection from " + remote + " through " + relay + ".");
                pipeLocalToRelay();
            } catch (Exception ex) {
                stream.recordTermination("bridge_error=" + throwableText(ex));
                if (!stream.isClosed()) {
                    append(serviceLabel + " bridge failed: " + ex.getMessage());
                }
            } finally {
                if (permitAcquired) {
                    permits.release();
                }
                try {
                    close();
                    append(serviceLabel + " session remote=" + remote + " relay=" + stream.selectedRelay() + " ended duration_ms="
                            + elapsedMillis(startedAt) + " termination=" + stream.termination() + " " + stream.stats() + ".");
                } finally {
                    if (wakeLockAcquired) {
                        releaseActiveBridgeWakeLock();
                    }
                }
            }
        }

        private void pipeLocalToRelay() throws IOException {
            InputStream input = localSocket.getInputStream();
            byte[] buffer = new byte[16 * 1024];
            int read;
            while (!stream.isClosed() && (read = input.read(buffer)) >= 0) {
                stream.write(Arrays.copyOf(buffer, read));
            }
            if (!stream.isClosed()) {
                stream.recordTermination("local_tcp_eof");
            }
        }

        private void writeLocal(byte[] payload) throws IOException {
            OutputStream output = localSocket.getOutputStream();
            synchronized (output) {
                output.write(payload);
                output.flush();
            }
        }

        void replaceTransportForNetworkChange(String reason) {
            stream.replaceTransportForNetworkChange(reason);
        }

        void close() {
            stream.close();
            finish();
        }

        /** Runs once, when the stream closed (by either side). */
        private void finish() {
            if (!finished.compareAndSet(false, true)) {
                return;
            }
            closeQuietly(localSocket);
            sessions.remove(this);
            activeConnections = Math.max(0, activeConnections - 1);
            updateState("Running", null, null, null);
        }
    }

	static long resumeAttemptWaitMillis(long remainingMillis) {
		return Math.max(1L, Math.min(RESUME_ATTEMPT_TIMEOUT_MS, remainingMillis));
	}

	// With alternate relays, an attempt covers connecting plus a bounded wait for
	// the Work agent, growing each round so the two sides eventually meet.
	static long failoverAttemptWaitMillis(int round, long remainingMillis) {
		long wait = Math.min(FAILOVER_ATTEMPT_MAX_MS, FAILOVER_ATTEMPT_BASE_MS + FAILOVER_ATTEMPT_STEP_MS * round);
		return Math.max(1L, Math.min(wait, remainingMillis));
	}

	static List<String> resumeRelayOrder(String current, List<String> configured) {
		List<String> order = new ArrayList<>();
		if (current != null && !current.trim().isEmpty() && !"none".equals(current)) {
			order.add(current.trim());
		}
		for (String relay : configured) {
			String trimmed = relay == null ? "" : relay.trim();
			boolean duplicate = trimmed.isEmpty();
			for (String existing : order) {
				if (existing.equalsIgnoreCase(trimmed)) {
					duplicate = true;
					break;
				}
			}
			if (!duplicate) {
				order.add(trimmed);
			}
		}
		if (order.isEmpty()) {
			order.add(RelayUrls.DEFAULT_RELAY_URL);
		}
		return order;
	}

	static boolean isLogicalSessionClose(int code, String reason) {
		return code == 1000 && "session closed".equals(reason);
	}

	static boolean isTerminalResumeClose(int code) {
		return code == 1000 || code == 1008;
	}
}
