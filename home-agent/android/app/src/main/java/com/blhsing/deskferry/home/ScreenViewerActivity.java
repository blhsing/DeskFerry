package com.blhsing.deskferry.home;

import android.Manifest;
import android.app.Activity;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.ContentValues;
import android.content.Context;
import android.content.pm.PackageManager;
import android.content.res.ColorStateList;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.Matrix;
import android.graphics.Paint;
import android.graphics.PorterDuff;
import android.graphics.PorterDuffXfermode;
import android.graphics.Rect;
import android.graphics.drawable.Drawable;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.os.Handler;
import android.os.Looper;
import android.os.SystemClock;
import android.provider.MediaStore;
import android.provider.Settings;
import android.text.InputType;
import android.text.TextUtils;
import android.util.Log;
import android.view.Gravity;
import android.view.HapticFeedbackConstants;
import android.view.KeyEvent;
import android.view.MotionEvent;
import android.view.ScaleGestureDetector;
import android.view.View;
import android.view.ViewConfiguration;
import android.view.ViewGroup;
import android.view.Window;
import android.view.WindowManager;
import android.view.inputmethod.BaseInputConnection;
import android.view.inputmethod.EditorInfo;
import android.view.inputmethod.InputConnection;
import android.view.inputmethod.InputMethodManager;
import android.widget.AdapterView;
import android.widget.Button;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.HorizontalScrollView;
import android.widget.ImageButton;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.PopupMenu;
import android.widget.Spinner;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.DataInputStream;
import java.io.EOFException;
import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.InterruptedIOException;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.concurrent.Callable;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.Semaphore;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicLong;

import okhttp3.OkHttpClient;
import okhttp3.Request;
import okhttp3.Response;
import okhttp3.WebSocket;
import okhttp3.WebSocketListener;
import okio.Buffer;
import okio.ByteString;

/**
 * VNC-like viewer for the Work PC's screen. With a stored screen key it runs the live screen
 * protocol (docs/screen-live-protocol.md) over the relay {@code screen} service and offers remote
 * control; otherwise, or when an older Work agent answers with the legacy stream, it shows a
 * read-only view.
 */
public class ScreenViewerActivity extends Activity {
    private static final String LOG_TAG = "DeskFerryScreen";
    private static final int MAX_RECOVERY_RETRIES = 2;
    private static final long RECOVERY_DELAY_MS = 600;
    private static final long PING_INTERVAL_MS = 10_000;
    private static final int LEGACY_INTERVAL_MS = 1000;
    private static final int MAX_PENDING_FRAMES = 2;
    static final String EXTRA_RELAY_URLS = "relay_urls";
    static final String EXTRA_PROXY = "proxy";
    static final String EXTRA_ROOM_PROOF = "room_proof";
    static final String EXTRA_SCREEN_KEY = "screen_key";
    static final String EXTRA_DESTINATION = "destination";

    static final String PASSWORD_MISMATCH_MESSAGE =
            "Room password mismatch — re-enter the room password for this destination";

    // Windows virtual-key codes.
    static final int VK_BACK = 0x08;
    static final int VK_TAB = 0x09;
    static final int VK_RETURN = 0x0D;
    static final int VK_CONTROL = 0x11;
    static final int VK_MENU = 0x12;
    static final int VK_ESCAPE = 0x1B;
    static final int VK_PRIOR = 0x21;
    static final int VK_NEXT = 0x22;
    static final int VK_END = 0x23;
    static final int VK_HOME = 0x24;
    static final int VK_LEFT = 0x25;
    static final int VK_UP = 0x26;
    static final int VK_RIGHT = 0x27;
    static final int VK_DOWN = 0x28;
    static final int VK_INSERT = 0x2D;
    static final int VK_DELETE = 0x2E;
    static final int VK_LWIN = 0x5B;
    static final int VK_F1 = 0x70;

    private static final int REQUEST_WRITE_IMAGES = 2001;
    private final Handler mainHandler = new Handler(Looper.getMainLooper());
    private final Map<Long, ReplyHandler> pendingReplies = new ConcurrentHashMap<>();
    private final AtomicLong nextCommandId = new AtomicLong();
    private final Paint srcPaint = new Paint();
    private final Paint scaledPaint = new Paint(Paint.FILTER_BITMAP_FLAG);
    private final Paint fillPaint = new Paint();
    private ExecutorService decoders;
    private ZoomImageView imageView;
    private TextView status;
    private Spinner zoomPreset;
    private EditText zoomInput;
    private TextView zoomValue;
    private Ui ui;
    private TextView statusChip;
    private TextView emptyView;
    private ImageButton fullscreenButton;
    private Button liveButton;
    private ImageButton controlButton;
    private ImageButton keyboardButton;
    private ImageButton cadButton;
    private ImageButton clipboardButton;
    private android.widget.Button minFpsButton;
    /** Frame rate below which the Work PC compresses changing areas; 0 asks for lossless only. */
    private int minFps = 30;
    static final int DEFAULT_MIN_FPS = 30;
    static final int MAX_MIN_FPS = 120;
    private HorizontalScrollView extraKeysScroll;
    private Button ctrlKey;
    private Button altKey;
    private Button winKey;
    private KeyCaptureView keyCapture;
    private OkHttpClient client;
    private volatile WebSocket socket;
    private volatile Session session;
    private Bitmap current;
    private Canvas currentCanvas;
    private List<String> relayUrls;
    private String proxy;
    private String roomProof;
    private String screenKey;
    private String deviceName;
    private int candidateIndex;
    private volatile boolean paired;
    private volatile boolean closing;
    private volatile int generation;
    private int recoveryAttempts;
    private boolean fullscreen;
    private boolean updatingZoomControls;

    // Session state, main thread only.
    private boolean running;
    private Phase phase = Phase.IDLE;
    private boolean controlAllowed;
    private boolean controlActive;
    private boolean locked;
    private String desktop = "";
    private int viewers;
    private boolean keyboardMode;
    private boolean stickyCtrl;
    private boolean stickyAlt;
    private boolean stickyWin;
    private String finalError;
    private final HashMap<Long, Bitmap> cursorShapes = new HashMap<>();
    // Statistics, main thread only.
    private long statWindowStart;
    private int statFrames;
    private long statReceivedStart;
    private double fps;
    private double kbps;
    // Pointer state, main thread only.
    private long lastTapAt;
    private float lastTapViewX;
    private float lastTapViewY;
    private int[] lastTapPoint;
    private float scrollRemainderX;
    private float scrollRemainderY;

    private enum Phase { IDLE, CONNECTING, LIVE, LEGACY, STOPPED, ERROR }

    interface ReplyHandler {
        void onReply(boolean ok, JSONObject result, String error);
    }

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        setTheme(R.style.ViewerTheme);
        requestWindowFeature(Window.FEATURE_NO_TITLE);
        getWindow().setSoftInputMode(WindowManager.LayoutParams.SOFT_INPUT_ADJUST_RESIZE
                | WindowManager.LayoutParams.SOFT_INPUT_STATE_HIDDEN);
        srcPaint.setXfermode(new PorterDuffXfermode(PorterDuff.Mode.SRC));
        scaledPaint.setXfermode(new PorterDuffXfermode(PorterDuff.Mode.SRC));
        fillPaint.setStyle(Paint.Style.FILL);
        fillPaint.setXfermode(new PorterDuffXfermode(PorterDuff.Mode.SRC));
        decoders = Executors.newFixedThreadPool(Math.max(1, Math.min(4, Runtime.getRuntime().availableProcessors())));
        deviceName = deviceName();
        buildUi();
        try {
            relayUrls = RelayUrls.normalizeRelayUrls(getIntent().getStringExtra(EXTRA_RELAY_URLS), false);
            proxy = ProxySettings.normalize(getIntent().getStringExtra(EXTRA_PROXY));
            roomProof = empty(getIntent().getStringExtra(EXTRA_ROOM_PROOF));
            screenKey = empty(getIntent().getStringExtra(EXTRA_SCREEN_KEY));
            if (!screenKey.isEmpty() && ScreenKeys.decodeScreenKey(screenKey) == null) screenKey = "";
            OkHttpClient.Builder builder = new OkHttpClient.Builder()
                    .connectTimeout(15, TimeUnit.SECONDS)
                    .readTimeout(0, TimeUnit.MILLISECONDS)
                    .pingInterval(20, TimeUnit.SECONDS);
            ProxySettings.apply(builder, proxy);
            client = builder.build();
            if (relayUrls.isEmpty() || roomProof.isEmpty()) {
                throw new IllegalArgumentException("Relay URLs and a saved room password are required.");
            }
            startLive();
        } catch (Exception ex) {
            showStatus(ex.getMessage(), Ui.Tone.DANGER, "Error");
        }
    }

    @Override
    protected void onPause() {
        releaseAllInput();
        super.onPause();
    }

    @Override
    protected void onStop() {
        releaseAllInput();
        super.onStop();
    }

    @Override
    protected void onDestroy() {
        closing = true;
        stopLive(false);
        mainHandler.removeCallbacksAndMessages(null);
        if (client != null) client.dispatcher().executorService().shutdown();
        if (decoders != null) decoders.shutdownNow();
        super.onDestroy();
    }

    // ---- UI -----------------------------------------------------------------------------

    private void buildUi() {
        ui = Ui.darkTheme(this);
        LinearLayout root = ui.vertical();
        root.setBackgroundColor(ui.surface);

        LinearLayout bar = ui.vertical();
        bar.setBackgroundColor(ui.surface);
        bar.setPadding(dp(12), dp(10), dp(12), dp(10));
        root.addView(bar, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        LinearLayout header = ui.horizontal();
        LinearLayout titles = ui.vertical();
        TextView title = ui.text("Screen viewer", Ui.Type.TITLE);
        title.setSingleLine(true);
        title.setEllipsize(TextUtils.TruncateAt.END);
        titles.addView(title);
        String destination = empty(getIntent().getStringExtra(EXTRA_DESTINATION));
        TextView version = ui.text((destination.isEmpty() ? "" : destination + " · ") + "v" + BuildConfig.VERSION_NAME, Ui.Type.CAPTION);
        version.setSingleLine(true);
        version.setEllipsize(TextUtils.TruncateAt.END);
        titles.addView(version);
        header.addView(titles, ui.weighted(ViewGroup.LayoutParams.WRAP_CONTENT, 1f, 0));
        statusChip = ui.chip(Ui.Tone.NEUTRAL, "Ready");
        header.addView(statusChip, ui.fixed(-1, -1, 8));
        fullscreenButton = ui.iconButton(R.drawable.ic_df_fullscreen, "Full screen", false);
        fullscreenButton.setOnClickListener(v -> toggleFullscreen());
        header.addView(fullscreenButton, ui.fixed(40, 40, 4));
        ImageButton save = ui.iconButton(R.drawable.ic_df_save, "Save PNG", false);
        save.setOnClickListener(v -> savePNG());
        header.addView(save, ui.fixed(40, 40, 0));
        bar.addView(header, ui.matchWrap(10));

        LinearLayout controls = ui.horizontal();
        liveButton = ui.primaryButton("Stop live", R.drawable.ic_df_stop);
        liveButton.setOnClickListener(v -> {
            if (running) stopLive(true); else startLive();
        });
        controls.addView(liveButton, ui.weighted(dp(44), 1f, 0));
        controlButton = ui.iconButton(R.drawable.ic_df_control, "Take control", true);
        controlButton.setOnClickListener(v -> toggleControl());
        controls.addView(controlButton, ui.fixed(44, 44, 8));
        keyboardButton = ui.iconButton(R.drawable.ic_df_keyboard, "Keyboard", true);
        keyboardButton.setOnClickListener(v -> toggleKeyboard());
        controls.addView(keyboardButton, ui.fixed(44, 44, 6));
        cadButton = ui.iconButton(R.drawable.ic_df_cad, "Send Ctrl+Alt+Del", true);
        cadButton.setOnClickListener(v -> sendSecureAttention());
        controls.addView(cadButton, ui.fixed(44, 44, 6));
        clipboardButton = ui.iconButton(R.drawable.ic_df_clipboard, "Clipboard", true);
        clipboardButton.setOnClickListener(this::showClipboardMenu);
        controls.addView(clipboardButton, ui.fixed(44, 44, 6));
        minFps = loadMinFps();
        minFpsButton = ui.quietButton(minFpsLabel(minFps), 0);
        minFpsButton.setContentDescription("Minimum frames per second");
        minFpsButton.setOnClickListener(v -> showMinFpsChooser());
        controls.addView(minFpsButton, ui.fixed(-1, 44, 6));
        bar.addView(controls, ui.matchWrap(8));

        LinearLayout zoomControls = ui.horizontal();
        zoomPreset = ui.spinner();
        zoomPreset.setContentDescription("Zoom preset");
        zoomPreset.setPadding(dp(10), 0, dp(30), 0);
        zoomPreset.setAdapter(ui.spinnerAdapter(java.util.Arrays.asList(
                "Auto Fit", "50%", "75%", "100%", "125%", "150%", "200%", "300%", "400%", "Custom")));
        zoomPreset.setSelection(0);
        zoomPreset.setOnItemSelectedListener(new AdapterView.OnItemSelectedListener() {
            @Override public void onItemSelected(AdapterView<?> parent, View view, int position, long id) {
                if (updatingZoomControls || imageView == null || position == 9) return;
                float[] values = new float[]{0, 50, 75, 100, 125, 150, 200, 300, 400};
                if (position == 0) imageView.setAutoFit(); else imageView.setZoomPercent(values[position]);
            }
            @Override public void onNothingSelected(AdapterView<?> parent) { }
        });
        zoomControls.addView(zoomPreset, ui.fixed(112, 40, 0));
        zoomInput = ui.field("10-1600%");
        zoomInput.setContentDescription("Custom zoom percent");
        zoomInput.setInputType(InputType.TYPE_CLASS_NUMBER | InputType.TYPE_NUMBER_FLAG_DECIMAL);
        zoomInput.setImeOptions(EditorInfo.IME_ACTION_DONE);
        zoomInput.setOnEditorActionListener((view, action, event) -> {
            if (action == EditorInfo.IME_ACTION_DONE) {
                applyCustomZoom();
                return true;
            }
            return false;
        });
        zoomControls.addView(zoomInput, ui.weighted(dp(40), 1f, 8));
        Button apply = ui.quietButton("Apply", 0);
        apply.setPadding(dp(10), 0, dp(10), 0);
        apply.setOnClickListener(v -> applyCustomZoom());
        zoomControls.addView(apply, ui.fixed(-1, 40, 4));
        zoomValue = ui.text("Auto Fit", Ui.Type.CAPTION);
        zoomValue.setTextColor(ui.textSecondary);
        zoomValue.setSingleLine(true);
        zoomValue.setGravity(Gravity.END | Gravity.CENTER_VERTICAL);
        zoomValue.setMinWidth(dp(52));
        zoomControls.addView(zoomValue, ui.fixed(-1, -1, 4));
        bar.addView(zoomControls, ui.matchWrap(0));

        root.addView(ui.divider(), new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, Math.max(1, dp(1))));

        status = ui.text("Ready", Ui.Type.BODY);
        status.setTextSize(13);
        status.setTextColor(ui.textSecondary);
        status.setMaxLines(2);
        status.setEllipsize(TextUtils.TruncateAt.END);
        status.setPadding(dp(14), dp(8), dp(14), dp(8));
        status.setBackgroundColor(ui.bg);
        root.addView(status, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        FrameLayout stage = new FrameLayout(this);
        stage.setBackgroundColor(Color.BLACK);
        keyCapture = new KeyCaptureView(this);
        stage.addView(keyCapture, new FrameLayout.LayoutParams(1, 1));
        imageView = new ZoomImageView(this);
        imageView.setZoomListener(this::showZoomValue);
        imageView.setRemoteInput(new PointerInput());
        imageView.setBackgroundColor(Color.BLACK);
        stage.addView(imageView, new FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        emptyView = ui.text("No screen yet. The Work PC's screen appears here once the live view connects.", Ui.Type.BODY);
        emptyView.setTextColor(ui.textMuted);
        emptyView.setGravity(Gravity.CENTER);
        emptyView.setPadding(dp(32), 0, dp(32), 0);
        stage.addView(emptyView, new FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.WRAP_CONTENT, Gravity.CENTER));
        root.addView(stage, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));

        extraKeysScroll = new HorizontalScrollView(this);
        extraKeysScroll.setHorizontalScrollBarEnabled(false);
        extraKeysScroll.setBackgroundColor(ui.surface);
        LinearLayout keys = ui.horizontal();
        keys.setPadding(dp(8), dp(6), dp(8), dp(6));
        addExtraKey(keys, "Esc", v -> sendKey(VK_ESCAPE, false));
        addExtraKey(keys, "Tab", v -> sendKey(VK_TAB, false));
        ctrlKey = addExtraKey(keys, "Ctrl", v -> { stickyCtrl = !stickyCtrl; updateModifierKeys(); });
        altKey = addExtraKey(keys, "Alt", v -> { stickyAlt = !stickyAlt; updateModifierKeys(); });
        winKey = addExtraKey(keys, "Win", v -> { stickyWin = !stickyWin; updateModifierKeys(); });
        addExtraKey(keys, "←", v -> sendKey(VK_LEFT, true));
        addExtraKey(keys, "↑", v -> sendKey(VK_UP, true));
        addExtraKey(keys, "↓", v -> sendKey(VK_DOWN, true));
        addExtraKey(keys, "→", v -> sendKey(VK_RIGHT, true));
        addExtraKey(keys, "Del", v -> sendKey(VK_DELETE, true));
        extraKeysScroll.addView(keys, new HorizontalScrollView.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        extraKeysScroll.setVisibility(View.GONE);
        root.addView(extraKeysScroll, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        // The window decor (and its insets controller) exists only after setContentView.
        setContentView(root);
        ui.applySystemBars(this, root, ui.surface);
        updateControls();
    }

    private Button addExtraKey(LinearLayout parent, String label, View.OnClickListener listener) {
        Button button = ui.secondaryButton(label, 0);
        button.setTextSize(13);
        button.setPadding(dp(12), 0, dp(12), 0);
        button.setMinWidth(dp(48));
        button.setOnClickListener(listener);
        parent.addView(button, ui.fixed(-1, 40, parent.getChildCount() == 0 ? 0 : 6));
        return button;
    }

    private void updateModifierKeys() {
        styleToggle(ctrlKey, stickyCtrl);
        styleToggle(altKey, stickyAlt);
        styleToggle(winKey, stickyWin);
    }

    private void styleToggle(Button button, boolean on) {
        if (button == null) return;
        if (on) ui.stylePrimary(button); else ui.styleSecondary(button);
    }

    /** Main thread: reflects session phase and control state on the toolbar. */
    private void updateControls() {
        if (liveButton == null) return;
        liveButton.setText(running ? "Stop live" : "Start live");
        ui.setButtonIcon(liveButton, running ? R.drawable.ic_df_stop : R.drawable.ic_df_live);
        if (running) ui.styleSecondary(liveButton); else ui.stylePrimary(liveButton);
        boolean canControl = phase == Phase.LIVE && controlAllowed;
        controlButton.setAlpha(canControl ? 1f : 0.45f);
        String controlLabel = controlActive ? "Stop control" : "Take control";
        controlButton.setContentDescription(canControl ? controlLabel : controlLabel + " (" + controlUnavailableReason() + ")");
        if (Build.VERSION.SDK_INT >= 26) controlButton.setTooltipText(controlButton.getContentDescription());
        if (controlActive) {
            controlButton.setBackground(ui.shape(ui.primarySoft, ui.primary, 8));
            controlButton.setImageTintList(ColorStateList.valueOf(ui.primary));
        } else {
            controlButton.setBackground(ui.shape(ui.surface, ui.borderStrong, 8));
            controlButton.setImageTintList(ColorStateList.valueOf(ui.textSecondary));
        }
        float controlAlpha = controlActive ? 1f : 0.45f;
        keyboardButton.setAlpha(controlAlpha);
        cadButton.setAlpha(controlAlpha);
        clipboardButton.setAlpha(phase == Phase.LIVE ? 1f : 0.45f);
        if (keyboardMode) {
            keyboardButton.setBackground(ui.shape(ui.primarySoft, ui.primary, 8));
            keyboardButton.setImageTintList(ColorStateList.valueOf(ui.primary));
        } else {
            keyboardButton.setBackground(ui.shape(ui.surface, ui.borderStrong, 8));
            keyboardButton.setImageTintList(ColorStateList.valueOf(ui.textSecondary));
        }
    }

    private String controlUnavailableReason() {
        if (phase == Phase.LEGACY) {
            return screenKey.isEmpty()
                    ? "save a room password for this destination to enable live control"
                    : "the Work PC runs an older DeskFerry that only offers a read-only view; upgrade it to enable control";
        }
        if (phase != Phase.LIVE) return "start the live view first";
        if (!controlAllowed) return "remote control is turned off in the Work PC's DeskFerry settings";
        return "";
    }

    private void applyCustomZoom() {
        String text = zoomInput.getText().toString().trim().replace("%", "");
        try {
            float value = Float.parseFloat(text);
            if (value < 10 || value > 1600) throw new NumberFormatException();
            imageView.setZoomPercent(value);
            zoomInput.clearFocus();
        } catch (NumberFormatException ex) {
            Toast.makeText(this, "Zoom must be from 10% through 1600%.", Toast.LENGTH_SHORT).show();
        }
    }

    private void showZoomValue(float percent, boolean autoFit) {
        runOnUiThread(() -> {
            if (zoomValue == null || zoomPreset == null) return;
            zoomValue.setText(autoFit ? "Auto Fit" : String.format(Locale.US, "%.1f%%", percent));
            int position = zoomPresetPosition(percent, autoFit);
            updatingZoomControls = true;
            zoomPreset.setSelection(position);
            updatingZoomControls = false;
        });
    }

    private int zoomPresetPosition(float percent, boolean autoFit) {
        if (autoFit) return 0;
        float[] values = new float[]{50, 75, 100, 125, 150, 200, 300, 400};
        for (int i = 0; i < values.length; i++) {
            if (Math.abs(percent - values[i]) < 0.05f) return i + 1;
        }
        return 9;
    }

    // ---- Transport ----------------------------------------------------------------------

    private void startLive() {
        stopLive(false);
        int run = ++generation;
        candidateIndex = 0;
        recoveryAttempts = 0;
        paired = false;
        closing = false;
        running = true;
        finalError = null;
        phase = Phase.CONNECTING;
        updateControls();
        showStatus("Connecting to the Work screen service...", Ui.Tone.WARNING, "Connecting");
        connectNext(run, null);
    }

    private void connectNext(int run, Throwable previous) {
        if (run != generation || closing) {
            return;
        }
        if (candidateIndex >= relayUrls.size()) {
            failFinal("Screen connection failed: " + (previous == null ? "all relay services failed" : message(previous)));
            return;
        }
        String relay = relayUrls.get(candidateIndex++);
        try {
            Request request = new Request.Builder()
                    .url(RelayUrls.webSocketEndpoint(relay))
                    .header("Authorization", "Bearer " + RelayUrls.roomToken(relay, ""))
                    .header("X-DeskFerry-Role", "client")
                    .header("X-TunnelDesktop-Role", "client")
                    .header("X-DeskFerry-Protocol", "2")
                    .header("X-DeskFerry-Service", "screen")
                    .header("X-DeskFerry-Room-Proof", roomProof)
                    .header("User-Agent", "DeskFerry-Android/" + BuildConfig.VERSION_NAME)
                    .build();
            socket = client.newWebSocket(request, new ScreenListener(relay, run));
        } catch (Exception ex) {
            connectNext(run, ex);
        }
    }

    private final class ScreenListener extends WebSocketListener {
        private final String relay;
        private final int run;
        private Session listenerSession;

        ScreenListener(String relay, int run) { this.relay = relay; this.run = run; }

        @Override
        public void onMessage(WebSocket webSocket, String text) {
            if (run != generation || webSocket != socket) return;
            try {
                JSONObject result = new JSONObject(text);
                if (!"session-ready".equals(result.optString("type"))) {
                    throw new IllegalStateException(result.optString("reason", result.optString("type", "relay rejected screen session")));
                }
                if (!"screen".equals(result.optString("service"))) {
                    throw new IllegalStateException("relay did not confirm screen-service support");
                }
                if (listenerSession != null) return;
                paired = true;
                Session created = new Session(run, webSocket, relay, screenKey);
                listenerSession = created;
                session = created;
                setStatus("Connected through " + relay + "; waiting for the Work PC...", Ui.Tone.WARNING, "Connecting");
                created.start();
            } catch (Exception ex) {
                if (!paired) {
                    webSocket.cancel();
                    connectNext(run, ex);
                } else {
                    recoverScreenSession(run, webSocket, "Screen request failed", ex);
                }
            }
        }

        @Override
        public void onMessage(WebSocket webSocket, ByteString bytes) {
            Session value = listenerSession;
            if (value != null) value.pipe.write(bytes);
        }

        @Override
        public void onFailure(WebSocket webSocket, Throwable error, Response response) {
            Session value = listenerSession;
            if (value != null) value.pipe.close(new IOException(message(error)));
            if (run != generation || closing || webSocket != socket) return;
            if (!paired) connectNext(run, error); else recoverScreenSession(run, webSocket, "Screen stream ended", error);
        }

        @Override
        public void onClosed(WebSocket webSocket, int code, String reason) {
            Session value = listenerSession;
            if (value != null) value.pipe.close(new EOFException("relay closed the screen session"));
            if (run == generation && webSocket == socket && !closing) {
                recoverScreenSession(run, webSocket, "Screen stream ended",
                        new IllegalStateException("relay closed code=" + code + (reason.isEmpty() ? "" : " reason=" + reason)));
            }
        }
    }

    private void recoverScreenSession(int run, WebSocket failedSocket, String label, Throwable failure) {
        if (Looper.myLooper() != Looper.getMainLooper()) {
            mainHandler.post(() -> recoverScreenSession(run, failedSocket, label, failure));
            return;
        }
        if (run != generation || closing || failedSocket != socket) return;
        String detail = message(failure);
        recoveryAttempts++;
        Log.w(LOG_TAG, label + ": " + detail + " recovery_attempt=" + recoveryAttempts, failure);
        socket = null;
        failedSocket.cancel();
        endSession();
        paired = false;
        if (!canRetryScreenSession(recoveryAttempts)) {
            failFinal(label + " after recovery attempts: " + detail);
            return;
        }
        candidateIndex = 0;
        phase = Phase.CONNECTING;
        updateControls();
        showStatus(label + ": " + detail + ". Retrying (" + recoveryAttempts + "/" + MAX_RECOVERY_RETRIES + ")...",
                Ui.Tone.WARNING, "Retrying");
        mainHandler.postDelayed(() -> {
            if (run == generation && !closing && socket == null) connectNext(run, failure);
        }, RECOVERY_DELAY_MS * recoveryAttempts);
    }

    static boolean canRetryScreenSession(int failedSessions) {
        return failedSessions <= MAX_RECOVERY_RETRIES;
    }

    /** Ends the current session without retrying and shows why. Safe from any thread. */
    private void failFinal(String text) {
        runOnUiThread(() -> {
            stopLive(false);
            finalError = text;
            phase = Phase.ERROR;
            updateControls();
            showStatus(text, Ui.Tone.DANGER, "Error");
        });
    }

    private void stopLive(boolean announce) {
        if (Looper.myLooper() != Looper.getMainLooper()) {
            mainHandler.post(() -> stopLive(announce));
            return;
        }
        releaseAllInput();
        generation++;
        closing = true;
        WebSocket value = socket;
        socket = null;
        endSession();
        if (value != null) value.cancel();
        running = false;
        if (announce) {
            phase = Phase.STOPPED;
            showStatus("Live view stopped.", Ui.Tone.NEUTRAL, "Stopped");
        }
        updateControls();
    }

    /** Main thread: forgets the current session and its control state. */
    private void endSession() {
        Session value = session;
        session = null;
        if (value != null) value.cancel();
        pendingReplies.clear();
        mainHandler.removeCallbacks(pingTask);
        mainHandler.removeCallbacks(statsTask);
        if (controlActive || keyboardMode) setKeyboardMode(false);
        controlActive = false;
        fps = 0;
        kbps = 0;
        imageView.setCursor(false, 0, 0, null, 0, 0);
        for (Bitmap shape : cursorShapes.values()) {
            if (shape != null && !shape.isRecycled()) shape.recycle();
        }
        cursorShapes.clear();
    }

    // ---- Session ------------------------------------------------------------------------

    /** Blocking byte stream fed by WebSocket binary messages. */
    static final class StreamPipe extends InputStream {
        private final Buffer buffer = new Buffer();
        private boolean closed;
        private IOException failure;
        private long received;

        synchronized void write(ByteString bytes) {
            if (closed) return;
            buffer.write(bytes);
            received += bytes.size();
            notifyAll();
        }

        synchronized void close(IOException error) {
            if (closed) return;
            closed = true;
            failure = error;
            notifyAll();
        }

        synchronized long received() {
            return received;
        }

        @Override
        public synchronized int read() throws IOException {
            awaitData();
            return buffer.readByte() & 0xFF;
        }

        @Override
        public synchronized int read(byte[] target, int offset, int length) throws IOException {
            if (length == 0) return 0;
            awaitData();
            return buffer.read(target, offset, length);
        }

        @Override
        public synchronized int available() {
            return (int) Math.min(Integer.MAX_VALUE, buffer.size());
        }

        @Override
        public void close() {
            close(new EOFException("screen stream closed"));
        }

        private void awaitData() throws IOException {
            while (buffer.size() == 0) {
                if (closed) {
                    buffer.clear();
                    throw failure != null ? failure : new EOFException("screen stream closed");
                }
                try {
                    wait();
                } catch (InterruptedException ex) {
                    Thread.currentThread().interrupt();
                    throw new InterruptedIOException("screen stream interrupted");
                }
            }
        }
    }

    /** One drawing operation of a frame: a decoded tile or a solid fill. */
    private static final class DrawOp {
        final int x;
        final int y;
        final int w;
        final int h;
        final Bitmap tile;
        final int color;

        DrawOp(int x, int y, int w, int h, Bitmap tile, int color) {
            this.x = x;
            this.y = y;
            this.w = w;
            this.h = h;
            this.tile = tile;
            this.color = color;
        }
    }

    /** A decoded frame waiting to be drawn on the main thread. */
    private static final class FrameBatch {
        final long seq;
        final int width;
        final int height;
        final boolean keyframe;
        final boolean legacy;
        final ArrayList<DrawOp> ops = new ArrayList<>();
        boolean lossless;

        FrameBatch(long seq, int width, int height, boolean keyframe, boolean legacy) {
            this.seq = seq;
            this.width = width;
            this.height = height;
            this.keyframe = keyframe;
            this.legacy = legacy;
        }

        void recycle() {
            for (DrawOp op : ops) {
                if (op.tile != null && !op.tile.isRecycled()) op.tile.recycle();
            }
            ops.clear();
        }
    }

    private static final class HelloError extends IOException {
        HelloError(String message) {
            super(message);
        }
    }

    private final class Session implements Runnable {
        final int run;
        final WebSocket webSocket;
        final String relay;
        final StreamPipe pipe = new StreamPipe();
        final Semaphore frameSlots = new Semaphore(MAX_PENDING_FRAMES);
        private final String key;
        private final Object sendLock = new Object();
        private LiveScreenProtocol.RecordCipher sendCipher;
        private volatile boolean cancelled;
        private volatile boolean live;
        private Thread thread;

        Session(int run, WebSocket webSocket, String relay, String key) {
            this.run = run;
            this.webSocket = webSocket;
            this.relay = relay;
            this.key = key;
        }

        void start() {
            thread = new Thread(this, "DeskFerryScreenSession");
            thread.setDaemon(true);
            thread.start();
        }

        void cancel() {
            cancelled = true;
            pipe.close(new EOFException("screen session closed"));
            frameSlots.release(MAX_PENDING_FRAMES);
        }

        boolean current() {
            return !cancelled && session == this && run == generation;
        }

        @Override
        public void run() {
            try {
                runSession();
            } catch (Throwable error) {
                if (cancelled) return;
                mainHandler.post(() -> onSessionFailed(this, error));
            }
        }

        private void runSession() throws Exception {
            DataInputStream in = new DataInputStream(pipe);
            LiveScreenProtocol.ClientHandshake handshake = null;
            JSONObject request = new JSONObject()
                    .put("mode", "stream")
                    .put("interval_ms", LEGACY_INTERVAL_MS)
                    .put("tile_size", 64);
            byte[] psk = ScreenKeys.decodeScreenKey(key);
            if (psk != null) {
                handshake = new LiveScreenProtocol.ClientHandshake(psk);
                request.put("protocol", LiveScreenProtocol.PROTOCOL)
                        .put("client", "viewer")
                        .put("name", deviceName)
                        .put("client_hello", ScreenKeys.base64Url(handshake.hello))
                        .put("features", new org.json.JSONArray().put(LiveScreenProtocol.FEATURE_SCALED_JPEG))
                        .put("min_fps", minFps);
            }
            if (!webSocket.send(ByteString.encodeUtf8(request.toString() + "\n"))) {
                throw new IOException("could not send the screen request");
            }
            JSONObject first = readLegacyMetadata(in);
            String type = first.optString("type");
            if ("hello".equals(type)) {
                JSONObject hello = first.optJSONObject("live");
                if (handshake == null || hello == null) throw new IOException("unexpected live screen hello");
                byte[] serverHello = ScreenKeys.base64UrlDecode(hello.optString("server_hello"));
                if (serverHello == null) throw new IOException("invalid live screen hello");
                LiveScreenProtocol.SessionKeys keys = handshake.finish(serverHello);
                LiveScreenProtocol.RecordCipher receive = new LiveScreenProtocol.RecordCipher(keys.s2c);
                synchronized (sendLock) {
                    sendCipher = new LiveScreenProtocol.RecordCipher(keys.c2s);
                }
                sendRecord(LiveScreenProtocol.REC_FINISH, keys.clientFinish);
                live = true;
                int width = hello.optInt("width");
                int height = hello.optInt("height");
                boolean allowed = hello.optBoolean("control_allowed");
                String version = hello.optString("version");
                mainHandler.post(() -> onLiveStarted(this, width, height, allowed, version));
                liveLoop(in, receive);
            } else if ("error".equals(type)) {
                throw new HelloError(first.optString("error", "screen session failed"));
            } else {
                mainHandler.post(() -> onLegacyStarted(this));
                while (true) {
                    FrameBatch batch = readLegacyFrame(in, first);
                    submitFrame(batch);
                    first = readLegacyMetadata(in);
                }
            }
        }

        private JSONObject readLegacyMetadata(DataInputStream in) throws Exception {
            int size = in.readInt();
            if (size <= 0 || size > 1024 * 1024) throw new IOException("invalid frame metadata size");
            byte[] metadata = new byte[size];
            in.readFully(metadata);
            return new JSONObject(new String(metadata, StandardCharsets.UTF_8));
        }

        private FrameBatch readLegacyFrame(DataInputStream in, JSONObject frame) throws Exception {
            String type = frame.optString("type");
            if ("error".equals(type)) throw new IOException(frame.optString("error", "screen capture failed"));
            int width = frame.getInt("width");
            int height = frame.getInt("height");
            if (width <= 0 || height <= 0 || width > 16384 || height > 16384) {
                throw new IOException("invalid screen dimensions");
            }
            boolean full = "full".equals(type);
            if (!full && !"delta".equals(type)) throw new IOException("unexpected screen frame " + type);
            FrameBatch batch = new FrameBatch(frame.optLong("seq"), width, height, full, true);
            batch.lossless = true;
            try {
                JSONArray rects = frame.optJSONArray("rects");
                int count = rects == null ? 0 : rects.length();
                long total = 0;
                for (int i = 0; i < count; i++) {
                    JSONObject rect = rects.getJSONObject(i);
                    int length = rect.getInt("length");
                    int x = rect.getInt("x");
                    int y = rect.getInt("y");
                    int w = rect.getInt("width");
                    int h = rect.getInt("height");
                    total += length;
                    if (length <= 0 || total > 256L * 1024 * 1024) throw new IOException("invalid screen tile length");
                    if (x < 0 || y < 0 || w <= 0 || h <= 0 || x + (long) w > width || y + (long) h > height) {
                        throw new IOException("invalid screen tile dimensions");
                    }
                    byte[] payload = new byte[length];
                    in.readFully(payload);
                    batch.ops.add(new DrawOp(x, y, w, h, decodeTile(payload, 0, length, w, h, false), 0));
                }
            } catch (Exception ex) {
                batch.recycle();
                throw ex;
            }
            return batch;
        }

        private void liveLoop(DataInputStream in, LiveScreenProtocol.RecordCipher receive) throws Exception {
            FrameBatch batch = null;
            try {
                while (!cancelled) {
                    byte[] plain = receive.read(in);
                    int length = plain.length - 1;
                    switch (plain[0]) {
                        case LiveScreenProtocol.REC_FRAME_START: {
                            LiveScreenProtocol.FrameStart start = LiveScreenProtocol.parseFrameStart(plain, 1, length);
                            if (batch != null) batch.recycle();
                            batch = new FrameBatch(start.seq, start.width, start.height, start.keyframe(), false);
                            break;
                        }
                        case LiveScreenProtocol.REC_RECTS: {
                            if (batch == null) throw new IOException("screen rects outside a frame");
                            decodeRects(batch, LiveScreenProtocol.parseRects(plain, 1, length));
                            break;
                        }
                        case LiveScreenProtocol.REC_FRAME_END: {
                            LiveScreenProtocol.FrameEnd end = LiveScreenProtocol.parseFrameEnd(plain, 1, length);
                            if (batch == null || end.seq != batch.seq) throw new IOException("invalid screen frame end");
                            batch.lossless = end.lossless();
                            FrameBatch ready = batch;
                            batch = null;
                            submitFrame(ready);
                            break;
                        }
                        case LiveScreenProtocol.REC_CURSOR: {
                            LiveScreenProtocol.Cursor cursor;
                            try {
                                cursor = LiveScreenProtocol.parseCursor(plain, 1, length);
                            } catch (IOException ignored) {
                                break;
                            }
                            Bitmap shape = null;
                            if (cursor.png.length > 0) {
                                shape = BitmapFactory.decodeByteArray(cursor.png, 0, cursor.png.length);
                                if (shape != null) shape.setDensity(Bitmap.DENSITY_NONE);
                            }
                            Bitmap decodedShape = shape;
                            mainHandler.post(() -> onCursor(this, cursor, decodedShape));
                            break;
                        }
                        case LiveScreenProtocol.REC_JSON: {
                            JSONObject message;
                            try {
                                message = new JSONObject(new String(plain, 1, length, StandardCharsets.UTF_8));
                            } catch (Exception ignored) {
                                break;
                            }
                            mainHandler.post(() -> onMessage(this, message));
                            break;
                        }
                        case LiveScreenProtocol.REC_PING:
                            sendRecord(LiveScreenProtocol.REC_PONG, java.util.Arrays.copyOfRange(plain, 1, plain.length));
                            break;
                        default:
                            break;
                    }
                }
            } finally {
                if (batch != null) batch.recycle();
            }
        }

        private void decodeRects(FrameBatch batch, List<LiveScreenProtocol.Rect> rects) throws Exception {
            if (rects.size() <= 1 || decoders == null) {
                for (LiveScreenProtocol.Rect rect : rects) batch.ops.add(decodeRect(rect));
                return;
            }
            ArrayList<Callable<DrawOp>> tasks = new ArrayList<>(rects.size());
            for (LiveScreenProtocol.Rect rect : rects) tasks.add(() -> decodeRect(rect));
            List<Future<DrawOp>> results = decoders.invokeAll(tasks);
            Exception failure = null;
            for (Future<DrawOp> result : results) {
                try {
                    batch.ops.add(result.get());
                } catch (Exception ex) {
                    if (failure == null) failure = ex;
                }
            }
            if (failure != null) throw failure;
        }

        private DrawOp decodeRect(LiveScreenProtocol.Rect rect) throws IOException {
            if (rect.enc == LiveScreenProtocol.ENC_SOLID) {
                return new DrawOp(rect.x, rect.y, rect.w, rect.h, null, rect.solidArgb());
            }
            return new DrawOp(rect.x, rect.y, rect.w, rect.h, decodeTile(rect.data, rect.dataOffset, rect.dataLength, rect.w, rect.h,
                    rect.enc == LiveScreenProtocol.ENC_JPEG_SCALED), 0);
        }

        private Bitmap decodeTile(byte[] data, int offset, int length, int width, int height, boolean scaled) throws IOException {
            BitmapFactory.Options options = new BitmapFactory.Options();
            options.inPreferredConfig = Bitmap.Config.ARGB_8888;
            Bitmap tile = BitmapFactory.decodeByteArray(data, offset, length, options);
            if (tile == null) throw new IOException("invalid screen tile image");
            boolean fits = scaled
                    ? tile.getWidth() > 0 && tile.getHeight() > 0 && tile.getWidth() <= width && tile.getHeight() <= height
                    : tile.getWidth() == width && tile.getHeight() == height;
            if (!fits) {
                tile.recycle();
                throw new IOException("screen tile does not match its size");
            }
            return tile;
        }

        /** Hands a decoded frame to the main thread, waiting while too many are queued. */
        private void submitFrame(FrameBatch batch) throws InterruptedException {
            frameSlots.acquire();
            if (cancelled) {
                batch.recycle();
                return;
            }
            mainHandler.post(() -> {
                try {
                    applyFrame(this, batch);
                } finally {
                    frameSlots.release();
                }
            });
        }

        void sendRecord(byte type, byte[] payload) throws IOException {
            synchronized (sendLock) {
                if (sendCipher == null) throw new IOException("live screen session is not ready");
                byte[] record = sendCipher.seal(type, payload);
                if (!webSocket.send(ByteString.of(record))) throw new IOException("screen session is closed");
            }
        }

        /** Sends a JSON command; never throws. */
        boolean sendJson(JSONObject command) {
            if (!live || cancelled) return false;
            try {
                sendRecord(LiveScreenProtocol.REC_JSON, command.toString().getBytes(StandardCharsets.UTF_8));
                return true;
            } catch (IOException ex) {
                Log.w(LOG_TAG, "screen command failed: " + ex.getMessage());
                return false;
            }
        }
    }

    private final Runnable pingTask = new Runnable() {
        @Override public void run() {
            Session value = session;
            if (value == null || !value.live) return;
            try {
                value.sendRecord(LiveScreenProtocol.REC_PING, LiveScreenProtocol.encodeSeq(System.nanoTime()));
            } catch (IOException ex) {
                Log.w(LOG_TAG, "screen ping failed: " + ex.getMessage());
            }
            mainHandler.postDelayed(this, PING_INTERVAL_MS);
        }
    };

    private final Runnable statsTask = new Runnable() {
        @Override public void run() {
            if (session == null) return;
            updateStats(false);
            mainHandler.postDelayed(this, 1000);
        }
    };

    private void onSessionFailed(Session failed, Throwable error) {
        if (!failed.current()) return;
        if (error instanceof LiveScreenProtocol.PasswordMismatchException) {
            failFinal(PASSWORD_MISMATCH_MESSAGE);
            return;
        }
        if (error instanceof HelloError) {
            failFinal("The Work PC refused the screen session: " + error.getMessage());
            return;
        }
        recoverScreenSession(failed.run, failed.webSocket, "Screen stream ended", error);
    }

    private void onLiveStarted(Session started, int width, int height, boolean allowed, String version) {
        if (!started.current()) return;
        phase = Phase.LIVE;
        controlAllowed = allowed;
        controlActive = false;
        locked = false;
        recoveryAttempts = 0;
        startStats();
        mainHandler.removeCallbacks(pingTask);
        mainHandler.postDelayed(pingTask, PING_INTERVAL_MS);
        updateControls();
        refreshStatusLine();
        Log.i(LOG_TAG, "live screen session through " + started.relay + " size=" + width + "x" + height
                + " control_allowed=" + allowed + " work_version=" + version);
    }

    private void onLegacyStarted(Session started) {
        if (!started.current()) return;
        phase = Phase.LEGACY;
        controlAllowed = false;
        controlActive = false;
        recoveryAttempts = 0;
        startStats();
        updateControls();
        refreshStatusLine();
    }

    private void startStats() {
        statWindowStart = SystemClock.elapsedRealtime();
        statFrames = 0;
        Session value = session;
        statReceivedStart = value == null ? 0 : value.pipe.received();
        mainHandler.removeCallbacks(statsTask);
        mainHandler.postDelayed(statsTask, 1000);
    }

    /** Main thread: draws a decoded frame into the framebuffer, presents it, and acks it. */
    private void applyFrame(Session from, FrameBatch batch) {
        if (!from.current() || isDestroyed()) {
            batch.recycle();
            return;
        }
        try {
            boolean resized = current == null || current.getWidth() != batch.width || current.getHeight() != batch.height;
            if (resized) {
                // Like the Go client, a live frame of a new size starts a new framebuffer; a legacy
                // delta needs its full frame first.
                if (batch.legacy && !batch.keyframe) {
                    throw new IllegalStateException("received a screen delta before its full frame");
                }
                Bitmap previous = current;
                current = Bitmap.createBitmap(batch.width, batch.height, Bitmap.Config.ARGB_8888);
                current.setDensity(Bitmap.DENSITY_NONE);
                current.eraseColor(Color.BLACK);
                currentCanvas = new Canvas(current);
                imageView.setImageBitmap(current);
                if (previous != null && !previous.isRecycled()) previous.recycle();
            } else if (batch.keyframe) {
                current.eraseColor(Color.BLACK);
            }
            for (DrawOp op : batch.ops) {
                if (op.x < 0 || op.y < 0 || op.x + op.w > batch.width || op.y + op.h > batch.height) {
                    throw new IllegalStateException("screen rect outside the framebuffer");
                }
                if (op.tile != null) {
                    if (op.tile.getWidth() == op.w && op.tile.getHeight() == op.h) {
                        currentCanvas.drawBitmap(op.tile, op.x, op.y, srcPaint);
                    } else {
                        currentCanvas.drawBitmap(op.tile, null, new Rect(op.x, op.y, op.x + op.w, op.y + op.h), scaledPaint);
                    }
                } else {
                    fillPaint.setColor(op.color);
                    currentCanvas.drawRect(op.x, op.y, op.x + op.w, op.y + op.h, fillPaint);
                }
            }
            imageView.invalidate();
            if (emptyView != null) emptyView.setVisibility(View.GONE);
        } catch (Exception ex) {
            batch.recycle();
            recoverScreenSession(from.run, from.webSocket, "Screen frame failed", ex);
            return;
        }
        batch.recycle();
        if (!batch.legacy) {
            try {
                from.sendRecord(LiveScreenProtocol.REC_ACK, LiveScreenProtocol.encodeSeq(batch.seq));
            } catch (IOException ex) {
                Log.w(LOG_TAG, "screen ack failed: " + ex.getMessage());
            }
        }
        updateStats(true);
    }

    /** Main thread: frame rate and received bit rate (all screen bytes) over about a second. */
    private void updateStats(boolean frame) {
        if (frame) statFrames++;
        long now = SystemClock.elapsedRealtime();
        long elapsed = now - statWindowStart;
        if (elapsed >= 1000) {
            Session value = session;
            long received = value == null ? statReceivedStart : value.pipe.received();
            fps = statFrames * 1000.0 / elapsed;
            kbps = Math.max(0, received - statReceivedStart) * 8.0 / elapsed;
            statFrames = 0;
            statReceivedStart = received;
            statWindowStart = now;
            refreshStatusLine();
        }
    }

    private void onCursor(Session from, LiveScreenProtocol.Cursor cursor, Bitmap shape) {
        if (!from.current()) {
            if (shape != null) shape.recycle();
            return;
        }
        if (shape != null) {
            Bitmap old = cursorShapes.put(cursor.shapeId, shape);
            if (old != null && old != shape && !old.isRecycled()) old.recycle();
        }
        imageView.setCursor(cursor.visible, cursor.x, cursor.y, cursorShapes.get(cursor.shapeId), cursor.hotX, cursor.hotY);
    }

    private void onMessage(Session from, JSONObject message) {
        if (!from.current()) return;
        String type = message.optString("type");
        if ("reply".equals(type)) {
            ReplyHandler handler = pendingReplies.remove(message.optLong("id"));
            if (handler != null) {
                JSONObject result = message.optJSONObject("result");
                handler.onReply(message.optBoolean("ok"), result == null ? new JSONObject() : result, message.optString("error"));
            }
        } else if ("status".equals(type)) {
            controlAllowed = message.optBoolean("control_allowed", controlAllowed);
            boolean active = message.optBoolean("control_active", controlActive);
            if (controlActive && !active && keyboardMode) setKeyboardMode(false);
            controlActive = active;
            locked = message.optBoolean("locked", false);
            desktop = message.optString("desktop", "");
            viewers = message.optInt("viewers", 0);
            updateControls();
            refreshStatusLine();
        }
    }

    /** Main thread: the status line with live statistics and the Work desktop state. */
    private void refreshStatusLine() {
        if (phase != Phase.LIVE && phase != Phase.LEGACY) return;
        StringBuilder text = new StringBuilder();
        String size = current == null ? "" : current.getWidth() + "×" + current.getHeight();
        String rates = String.format(Locale.US, "%.0f fps · %.0f kbps", fps, kbps);
        Ui.Tone tone;
        String chip;
        if (phase == Phase.LIVE) {
            text.append("Live · ").append(rates);
            if (!size.isEmpty()) text.append(" · ").append(size);
            if (controlActive) text.append(" · You are in control");
            if (viewers > 1) text.append(" · ").append(viewers).append(" viewers");
            if (locked) {
                text.append(" · Work PC is locked");
                tone = Ui.Tone.WARNING;
                chip = "Locked";
            } else {
                if (!desktop.isEmpty() && !"Default".equalsIgnoreCase(desktop)) {
                    text.append(" · Secure desktop (").append(desktop).append(")");
                }
                tone = Ui.Tone.SUCCESS;
                chip = controlActive ? "Controlling" : "Live";
            }
        } else {
            text.append("Read-only view · ").append(rates);
            if (!size.isEmpty()) text.append(" · ").append(size);
            text.append(screenKey.isEmpty()
                    ? " · Save a room password for this destination to enable live control."
                    : " · The Work PC runs an older DeskFerry; upgrade it to enable live control.");
            tone = Ui.Tone.INFO;
            chip = "Read-only";
        }
        showStatus(text.toString(), tone, chip);
    }

    // ---- Commands -----------------------------------------------------------------------

    /** Main thread: sends a command, optionally with an id whose reply goes to {@code handler}. */
    private boolean sendCommand(JSONObject command, ReplyHandler handler) {
        Session value = session;
        if (value == null || !value.live) return false;
        long id = 0;
        try {
            if (handler != null) {
                id = nextCommandId.incrementAndGet();
                command.put("id", id);
                pendingReplies.put(id, handler);
            }
        } catch (Exception ignored) {
            return false;
        }
        if (!value.sendJson(command)) {
            if (id != 0) pendingReplies.remove(id);
            return false;
        }
        return true;
    }

    private void sendInput(JSONObject... events) {
        if (!controlActive) return;
        try {
            JSONArray array = new JSONArray();
            for (JSONObject event : events) array.put(event);
            sendCommand(new JSONObject().put("cmd", "input").put("events", array), null);
        } catch (Exception ex) {
            Log.w(LOG_TAG, "screen input failed: " + ex.getMessage());
        }
    }

    private void releaseAllInput() {
        Session value = session;
        if (value == null || !value.live || !controlActive) return;
        try {
            JSONArray events = new JSONArray().put(new JSONObject().put("t", "release_all"));
            sendCommand(new JSONObject().put("cmd", "input").put("events", events), null);
        } catch (Exception ignored) {
        }
        stickyCtrl = stickyAlt = stickyWin = false;
        updateModifierKeys();
    }

    private void toggleControl() {
        if (phase != Phase.LIVE || !controlAllowed) {
            Toast.makeText(this, "Control is unavailable: " + controlUnavailableReason() + ".", Toast.LENGTH_LONG).show();
            return;
        }
        boolean enable = !controlActive;
        if (!enable) releaseAllInput();
        try {
            boolean sent = sendCommand(new JSONObject().put("cmd", "control").put("enable", enable), (ok, result, error) -> {
                if (ok) {
                    controlActive = result.optBoolean("control_active", enable);
                    if (!controlActive) setKeyboardMode(false);
                    Toast.makeText(this, controlActive
                            ? "You are in control. Tap to click, long-press to right-click, two fingers to scroll."
                            : "Control returned to the Work PC.", Toast.LENGTH_SHORT).show();
                } else {
                    Toast.makeText(this, describeError(error), Toast.LENGTH_LONG).show();
                }
                updateControls();
                refreshStatusLine();
            });
            if (!sent) Toast.makeText(this, "The screen session is not connected.", Toast.LENGTH_SHORT).show();
        } catch (Exception ex) {
            Toast.makeText(this, ex.getMessage(), Toast.LENGTH_SHORT).show();
        }
    }

    static String minFpsLabel(int value) {
        return value == 0 ? "Min fps: lossless" : "Min fps: " + value;
    }

    private String minFpsPreferenceKey() {
        return "min_fps:" + empty(getIntent().getStringExtra(EXTRA_DESTINATION)).toLowerCase(java.util.Locale.ROOT);
    }

    private int loadMinFps() {
        int value = getSharedPreferences("screen_viewer", MODE_PRIVATE).getInt(minFpsPreferenceKey(), DEFAULT_MIN_FPS);
        return value < 0 || value > MAX_MIN_FPS ? DEFAULT_MIN_FPS : value;
    }

    private void showMinFpsChooser() {
        final int[] values = {0, 15, 24, 30, 45, 60};
        String[] labels = {"0 (lossless: never compress)", "15", "24", "30", "45", "60", "Custom…"};
        new android.app.AlertDialog.Builder(this)
                .setTitle("Minimum frames per second")
                .setItems(labels, (dialog, which) -> {
                    if (which < values.length) {
                        setMinFps(values[which]);
                        return;
                    }
                    android.widget.EditText input = new android.widget.EditText(this);
                    input.setInputType(android.text.InputType.TYPE_CLASS_NUMBER);
                    input.setText(String.valueOf(minFps));
                    new android.app.AlertDialog.Builder(this)
                            .setTitle("Minimum frames per second (0 to " + MAX_MIN_FPS + ")")
                            .setView(input)
                            .setPositiveButton("Apply", (inner, button) -> {
                                try {
                                    int value = Integer.parseInt(input.getText().toString().trim());
                                    if (value < 0 || value > MAX_MIN_FPS) throw new NumberFormatException();
                                    setMinFps(value);
                                } catch (NumberFormatException ex) {
                                    Toast.makeText(this, "Enter a whole number from 0 to " + MAX_MIN_FPS + ".", Toast.LENGTH_SHORT).show();
                                }
                            })
                            .setNegativeButton("Cancel", null)
                            .show();
                })
                .show();
    }

    private void setMinFps(int value) {
        minFps = value;
        minFpsButton.setText(minFpsLabel(value));
        getSharedPreferences("screen_viewer", MODE_PRIVATE).edit().putInt(minFpsPreferenceKey(), value).apply();
        try {
            sendCommand(new JSONObject().put("cmd", "quality").put("min_fps", value), (ok, result, error) -> {
                if (!ok) Toast.makeText(this, describeError(error), Toast.LENGTH_SHORT).show();
            });
        } catch (Exception ex) {
            Toast.makeText(this, ex.getMessage(), Toast.LENGTH_SHORT).show();
        }
    }

    private void sendSecureAttention() {
        if (!requireControl()) return;
        try {
            sendCommand(new JSONObject().put("cmd", "sas"), (ok, result, error) -> Toast.makeText(this,
                    ok ? "Ctrl+Alt+Del sent." : describeError(error), ok ? Toast.LENGTH_SHORT : Toast.LENGTH_LONG).show());
        } catch (Exception ex) {
            Toast.makeText(this, ex.getMessage(), Toast.LENGTH_SHORT).show();
        }
    }

    private void showClipboardMenu(View anchor) {
        if (phase != Phase.LIVE) {
            Toast.makeText(this, "Clipboard sharing needs the live view: " + controlUnavailableReason() + ".", Toast.LENGTH_LONG).show();
            return;
        }
        PopupMenu menu = new PopupMenu(this, anchor);
        menu.getMenu().add(0, 1, 0, "Send clipboard to Work PC");
        menu.getMenu().add(0, 2, 1, "Get Work PC clipboard");
        menu.setOnMenuItemClickListener(item -> {
            if (item.getItemId() == 1) sendClipboard(); else fetchClipboard();
            return true;
        });
        menu.show();
    }

    private void sendClipboard() {
        ClipboardManager clipboard = (ClipboardManager) getSystemService(Context.CLIPBOARD_SERVICE);
        CharSequence text = null;
        if (clipboard != null && clipboard.hasPrimaryClip() && clipboard.getPrimaryClip() != null
                && clipboard.getPrimaryClip().getItemCount() > 0) {
            text = clipboard.getPrimaryClip().getItemAt(0).coerceToText(this);
        }
        if (text == null || text.length() == 0) {
            Toast.makeText(this, "The Android clipboard has no text to send.", Toast.LENGTH_SHORT).show();
            return;
        }
        try {
            sendCommand(new JSONObject().put("cmd", "clipboard_set").put("text", text.toString()), (ok, result, error) ->
                    Toast.makeText(this, ok ? "Clipboard sent to the Work PC." : describeError(error), Toast.LENGTH_SHORT).show());
        } catch (Exception ex) {
            Toast.makeText(this, ex.getMessage(), Toast.LENGTH_SHORT).show();
        }
    }

    private void fetchClipboard() {
        try {
            sendCommand(new JSONObject().put("cmd", "clipboard_get"), (ok, result, error) -> {
                if (!ok) {
                    Toast.makeText(this, describeError(error), Toast.LENGTH_LONG).show();
                    return;
                }
                String text = result.optString("text", "");
                ClipboardManager clipboard = (ClipboardManager) getSystemService(Context.CLIPBOARD_SERVICE);
                if (clipboard != null) clipboard.setPrimaryClip(ClipData.newPlainText("Work PC clipboard", text));
                Toast.makeText(this, text.isEmpty() ? "The Work PC clipboard is empty." : "Copied the Work PC clipboard.",
                        Toast.LENGTH_SHORT).show();
            });
        } catch (Exception ex) {
            Toast.makeText(this, ex.getMessage(), Toast.LENGTH_SHORT).show();
        }
    }

    private boolean requireControl() {
        if (controlActive) return true;
        Toast.makeText(this, phase == Phase.LIVE && controlAllowed
                ? "Take control first."
                : "Control is unavailable: " + controlUnavailableReason() + ".", Toast.LENGTH_LONG).show();
        return false;
    }

    static String describeError(String error) {
        String value = error == null ? "" : error.trim();
        int colon = value.indexOf(':');
        String code = colon < 0 ? value : value.substring(0, colon).trim();
        switch (code) {
            case "control_not_allowed": return "Remote control is turned off on the Work PC.";
            case "control_not_enabled": return "Take control first.";
            case "human_active": return "Someone is using the Work PC right now.";
            case "screen_locked": return "The Work PC is locked.";
            case "timeout": return "The Work PC did not answer in time.";
            case "unsupported": return "The Work PC does not support this.";
            default: return value.isEmpty() ? "The Work PC rejected the request." : value;
        }
    }

    // ---- Pointer input ------------------------------------------------------------------

    private static JSONObject event(String type) throws Exception {
        return new JSONObject().put("t", type);
    }

    private static JSONObject move(int[] point) throws Exception {
        return event("move").put("x", point[0]).put("y", point[1]);
    }

    private static JSONObject button(String name, boolean down, int[] point) throws Exception {
        return event("button").put("b", name).put("down", down).put("x", point[0]).put("y", point[1]);
    }

    private final class PointerInput implements ZoomImageView.RemoteInput {
        @Override public boolean controlling() {
            return controlActive && current != null;
        }

        @Override public void onTap(float x, float y) {
            int[] point = imageView.toFramebuffer(x, y);
            if (point == null) return;
            long now = SystemClock.uptimeMillis();
            float slop = ViewConfiguration.get(ScreenViewerActivity.this).getScaledDoubleTapSlop();
            if (lastTapPoint != null && now - lastTapAt <= ViewConfiguration.getDoubleTapTimeout()
                    && Math.hypot(x - lastTapViewX, y - lastTapViewY) <= slop) {
                point = lastTapPoint; // Same spot so Windows sees a double click.
            }
            lastTapAt = now;
            lastTapViewX = x;
            lastTapViewY = y;
            lastTapPoint = point;
            click("left", point);
        }

        @Override public void onRightClick(float x, float y) {
            int[] point = imageView.toFramebuffer(x, y);
            if (point != null) click("right", point);
            lastTapPoint = null;
        }

        @Override public void onPointer(float x, float y) {
            int[] point = imageView.toFramebuffer(x, y);
            if (point == null) return;
            imageView.showTouch(point[0], point[1]);
            try {
                sendInput(move(point));
            } catch (Exception ignored) {
            }
        }

        @Override public void onDragStart(float x, float y) {
            int[] point = imageView.toFramebuffer(x, y);
            if (point == null) return;
            try {
                sendInput(move(point), button("left", true, point));
            } catch (Exception ignored) {
            }
        }

        @Override public void onDragMove(float x, float y) {
            int[] point = imageView.toFramebuffer(x, y);
            if (point == null) return;
            try {
                sendInput(move(point));
            } catch (Exception ignored) {
            }
        }

        @Override public void onDragEnd(float x, float y) {
            int[] point = imageView.toFramebuffer(x, y);
            if (point == null) return;
            try {
                sendInput(move(point), button("left", false, point));
            } catch (Exception ignored) {
            }
        }

        @Override public void onScroll(float x, float y, float dx, float dy) {
            int[] point = imageView.toFramebuffer(x, y);
            if (point == null) return;
            float notchPx = dp(32);
            // Natural scrolling: moving the fingers down scrolls the content up (positive dy).
            scrollRemainderY += dy / notchPx * 120f;
            scrollRemainderX -= dx / notchPx * 120f;
            int wheelY = (int) scrollRemainderY;
            int wheelX = (int) scrollRemainderX;
            if (Math.abs(wheelY) < 30 && Math.abs(wheelX) < 30) return;
            scrollRemainderY -= wheelY;
            scrollRemainderX -= wheelX;
            try {
                JSONObject wheel = event("wheel").put("x", point[0]).put("y", point[1]);
                if (wheelX != 0) wheel.put("dx", wheelX);
                if (wheelY != 0) wheel.put("dy", wheelY);
                sendInput(wheel);
            } catch (Exception ignored) {
            }
        }

        @Override public void onScrollEnd() {
            scrollRemainderX = 0;
            scrollRemainderY = 0;
        }

        private void click(String name, int[] point) {
            imageView.showTouch(point[0], point[1]);
            try {
                sendInput(move(point), button(name, true, point), button(name, false, point));
            } catch (Exception ignored) {
            }
        }
    }

    // ---- Keyboard -----------------------------------------------------------------------

    private void toggleKeyboard() {
        if (!keyboardMode && !requireControl()) return;
        setKeyboardMode(!keyboardMode);
    }

    private void setKeyboardMode(boolean enabled) {
        keyboardMode = enabled;
        if (extraKeysScroll != null) extraKeysScroll.setVisibility(enabled ? View.VISIBLE : View.GONE);
        InputMethodManager imm = (InputMethodManager) getSystemService(Context.INPUT_METHOD_SERVICE);
        if (keyCapture != null && imm != null) {
            if (enabled) {
                keyCapture.setFocusableInTouchMode(true);
                keyCapture.requestFocus();
                imm.restartInput(keyCapture);
                imm.showSoftInput(keyCapture, InputMethodManager.SHOW_IMPLICIT);
            } else {
                imm.hideSoftInputFromWindow(keyCapture.getWindowToken(), 0);
                keyCapture.clearFocus();
            }
        }
        if (!enabled) {
            stickyCtrl = stickyAlt = stickyWin = false;
            updateModifierKeys();
        }
        updateControls();
    }

    /** Sends one key press, wrapped in any sticky or held modifiers. */
    private void sendKey(int vk, boolean extended) {
        sendKey(vk, extended, false, false);
    }

    private void sendKey(int vk, boolean extended, boolean heldCtrl, boolean heldAlt) {
        if (!requireControl()) return;
        try {
            ArrayList<JSONObject> events = new ArrayList<>();
            List<int[]> modifiers = activeModifiers(heldCtrl, heldAlt);
            for (int[] modifier : modifiers) events.add(key(modifier[0], modifier[1] != 0, true));
            events.add(key(vk, extended, true));
            events.add(key(vk, extended, false));
            for (int i = modifiers.size() - 1; i >= 0; i--) events.add(key(modifiers.get(i)[0], modifiers.get(i)[1] != 0, false));
            sendInput(events.toArray(new JSONObject[0]));
        } catch (Exception ex) {
            Log.w(LOG_TAG, "key failed: " + ex.getMessage());
        }
        clearStickyModifiers();
    }

    /** Sends typed text; with modifiers active the first character becomes a key chord. */
    private void sendText(String text, boolean heldCtrl, boolean heldAlt) {
        if (text == null || text.isEmpty() || !requireControl()) return;
        try {
            StringBuilder plain = new StringBuilder();
            int index = 0;
            while (index < text.length()) {
                int codePoint = text.codePointAt(index);
                index += Character.charCount(codePoint);
                if (codePoint == '\n') {
                    flushText(plain);
                    sendKey(VK_RETURN, false, heldCtrl, heldAlt);
                    continue;
                }
                List<int[]> modifiers = activeModifiers(heldCtrl, heldAlt);
                if (!modifiers.isEmpty()) {
                    flushText(plain);
                    ArrayList<JSONObject> events = new ArrayList<>();
                    for (int[] modifier : modifiers) events.add(key(modifier[0], modifier[1] != 0, true));
                    String c = new String(Character.toChars(codePoint));
                    events.add(event("char_key").put("c", c).put("down", true));
                    events.add(event("char_key").put("c", c).put("down", false));
                    for (int i = modifiers.size() - 1; i >= 0; i--) events.add(key(modifiers.get(i)[0], modifiers.get(i)[1] != 0, false));
                    sendInput(events.toArray(new JSONObject[0]));
                    clearStickyModifiers();
                } else {
                    plain.appendCodePoint(codePoint);
                }
            }
            flushText(plain);
        } catch (Exception ex) {
            Log.w(LOG_TAG, "text failed: " + ex.getMessage());
        }
    }

    private void flushText(StringBuilder plain) throws Exception {
        if (plain.length() == 0) return;
        sendInput(event("text").put("s", plain.toString()));
        plain.setLength(0);
    }

    private List<int[]> activeModifiers(boolean heldCtrl, boolean heldAlt) {
        ArrayList<int[]> modifiers = new ArrayList<>();
        if (stickyCtrl || heldCtrl) modifiers.add(new int[]{VK_CONTROL, 0});
        if (stickyAlt || heldAlt) modifiers.add(new int[]{VK_MENU, 0});
        if (stickyWin) modifiers.add(new int[]{VK_LWIN, 1});
        return modifiers;
    }

    private void clearStickyModifiers() {
        if (!stickyCtrl && !stickyAlt && !stickyWin) return;
        stickyCtrl = stickyAlt = stickyWin = false;
        updateModifierKeys();
    }

    private static JSONObject key(int vk, boolean extended, boolean down) throws Exception {
        JSONObject key = event("key").put("vk", vk).put("down", down);
        if (extended) key.put("ext", true);
        return key;
    }

    /** Maps an Android key code to {Windows virtual-key code, extended flag}, or null. */
    static int[] windowsVirtualKey(int keyCode) {
        switch (keyCode) {
            case KeyEvent.KEYCODE_DEL: return new int[]{VK_BACK, 0};
            case KeyEvent.KEYCODE_FORWARD_DEL: return new int[]{VK_DELETE, 1};
            case KeyEvent.KEYCODE_ENTER:
            case KeyEvent.KEYCODE_NUMPAD_ENTER: return new int[]{VK_RETURN, 0};
            case KeyEvent.KEYCODE_TAB: return new int[]{VK_TAB, 0};
            case KeyEvent.KEYCODE_ESCAPE: return new int[]{VK_ESCAPE, 0};
            case KeyEvent.KEYCODE_DPAD_LEFT: return new int[]{VK_LEFT, 1};
            case KeyEvent.KEYCODE_DPAD_UP: return new int[]{VK_UP, 1};
            case KeyEvent.KEYCODE_DPAD_RIGHT: return new int[]{VK_RIGHT, 1};
            case KeyEvent.KEYCODE_DPAD_DOWN: return new int[]{VK_DOWN, 1};
            case KeyEvent.KEYCODE_MOVE_HOME: return new int[]{VK_HOME, 1};
            case KeyEvent.KEYCODE_MOVE_END: return new int[]{VK_END, 1};
            case KeyEvent.KEYCODE_PAGE_UP: return new int[]{VK_PRIOR, 1};
            case KeyEvent.KEYCODE_PAGE_DOWN: return new int[]{VK_NEXT, 1};
            case KeyEvent.KEYCODE_INSERT: return new int[]{VK_INSERT, 1};
            default:
                if (keyCode >= KeyEvent.KEYCODE_F1 && keyCode <= KeyEvent.KEYCODE_F12) {
                    return new int[]{VK_F1 + keyCode - KeyEvent.KEYCODE_F1, 0};
                }
                return null;
        }
    }

    /** Main thread: handles a key from the soft keyboard or a hardware keyboard. */
    private boolean handleKeyDown(int keyCode, KeyEvent event) {
        if (!controlActive) return false;
        int[] vk = windowsVirtualKey(keyCode);
        boolean ctrl = event != null && event.isCtrlPressed();
        boolean alt = event != null && event.isAltPressed();
        if (vk != null) {
            sendKey(vk[0], vk[1] != 0, ctrl, alt);
            return true;
        }
        if (event != null) {
            int unicode = event.getUnicodeChar(event.getMetaState() & ~(KeyEvent.META_CTRL_MASK | KeyEvent.META_ALT_MASK));
            if (unicode > 0) {
                sendText(new String(Character.toChars(unicode)), ctrl, alt);
                return true;
            }
        }
        return false;
    }

    /** Invisible focus target whose input connection forwards typing to the Work PC. */
    private final class KeyCaptureView extends View {
        KeyCaptureView(Context context) {
            super(context);
            setFocusable(true);
            setFocusableInTouchMode(true);
            setAlpha(0f);
        }

        @Override public boolean onCheckIsTextEditor() {
            return true;
        }

        @Override public InputConnection onCreateInputConnection(EditorInfo outAttrs) {
            outAttrs.inputType = InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_VISIBLE_PASSWORD
                    | InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS;
            outAttrs.imeOptions = EditorInfo.IME_FLAG_NO_EXTRACT_UI | EditorInfo.IME_FLAG_NO_FULLSCREEN
                    | EditorInfo.IME_ACTION_NONE;
            return new RemoteInputConnection(this);
        }

        @Override public boolean onKeyDown(int keyCode, KeyEvent event) {
            return handleKeyDown(keyCode, event) || super.onKeyDown(keyCode, event);
        }

        @Override public boolean onKeyUp(int keyCode, KeyEvent event) {
            return (controlActive && (windowsVirtualKey(keyCode) != null || event.getUnicodeChar() > 0))
                    || super.onKeyUp(keyCode, event);
        }
    }

    /**
     * Forwards IME edits as remote typing. Composing text is mirrored on the Work PC as it changes,
     * erasing with Backspace where the IME revises it, so commit only sends what is new.
     */
    private final class RemoteInputConnection extends BaseInputConnection {
        private String composing = "";

        RemoteInputConnection(View view) {
            super(view, false);
        }

        @Override public boolean commitText(CharSequence text, int newCursorPosition) {
            replaceComposing(text == null ? "" : text.toString());
            composing = "";
            return true;
        }

        @Override public boolean setComposingText(CharSequence text, int newCursorPosition) {
            replaceComposing(text == null ? "" : text.toString());
            return true;
        }

        @Override public boolean finishComposingText() {
            composing = "";
            return true;
        }

        @Override public boolean deleteSurroundingText(int beforeLength, int afterLength) {
            for (int i = 0; i < beforeLength; i++) sendKey(VK_BACK, false);
            for (int i = 0; i < afterLength; i++) sendKey(VK_DELETE, true);
            return true;
        }

        @Override public boolean sendKeyEvent(KeyEvent event) {
            if (event.getAction() == KeyEvent.ACTION_DOWN) handleKeyDown(event.getKeyCode(), event);
            return true;
        }

        @Override public boolean performEditorAction(int actionCode) {
            sendKey(VK_RETURN, false);
            return true;
        }

        private void replaceComposing(String next) {
            int common = 0;
            int limit = Math.min(composing.length(), next.length());
            while (common < limit && composing.charAt(common) == next.charAt(common)) common++;
            int erase = composing.codePointCount(common, composing.length());
            for (int i = 0; i < erase; i++) sendKey(VK_BACK, false);
            if (common < next.length()) sendText(next.substring(common), false, false);
            composing = next;
        }
    }

    // ---- Misc ---------------------------------------------------------------------------

    private void toggleFullscreen() {
        fullscreen = !fullscreen;
        getWindow().getDecorView().setSystemUiVisibility(fullscreen
                ? View.SYSTEM_UI_FLAG_FULLSCREEN | View.SYSTEM_UI_FLAG_HIDE_NAVIGATION | View.SYSTEM_UI_FLAG_IMMERSIVE_STICKY
                : View.SYSTEM_UI_FLAG_VISIBLE);
        if (fullscreenButton != null) {
            fullscreenButton.setImageResource(fullscreen ? R.drawable.ic_df_fullscreen_exit : R.drawable.ic_df_fullscreen);
            fullscreenButton.setContentDescription(fullscreen ? "Exit full screen" : "Full screen");
        }
    }

    private void savePNG() {
        if (current == null) {
            Toast.makeText(this, "Wait for the screen to appear before saving.", Toast.LENGTH_SHORT).show();
            return;
        }
        if (Build.VERSION.SDK_INT < 29 && checkSelfPermission(Manifest.permission.WRITE_EXTERNAL_STORAGE) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.WRITE_EXTERNAL_STORAGE}, REQUEST_WRITE_IMAGES);
            return;
        }
        Bitmap bitmap = current.copy(Bitmap.Config.ARGB_8888, false);
        if (bitmap == null) {
            Toast.makeText(this, "Could not copy the screenshot for saving.", Toast.LENGTH_SHORT).show();
            return;
        }
        String name = "DeskFerry-" + new java.text.SimpleDateFormat("yyyyMMdd-HHmmss", Locale.US).format(new java.util.Date()) + ".png";
        try {
            OutputStream output;
            String location;
            if (Build.VERSION.SDK_INT >= 29) {
                ContentValues values = new ContentValues();
                values.put(MediaStore.Images.Media.DISPLAY_NAME, name);
                values.put(MediaStore.Images.Media.MIME_TYPE, "image/png");
                values.put(MediaStore.Images.Media.RELATIVE_PATH, Environment.DIRECTORY_PICTURES + "/DeskFerry");
                Uri uri = getContentResolver().insert(MediaStore.Images.Media.EXTERNAL_CONTENT_URI, values);
                if (uri == null) throw new IllegalStateException("could not create the image");
                output = getContentResolver().openOutputStream(uri);
                location = "Pictures/DeskFerry/" + name;
            } else {
                File directory = new File(Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_PICTURES), "DeskFerry");
                if (!directory.exists() && !directory.mkdirs()) throw new IllegalStateException("could not create the Pictures/DeskFerry folder");
                File file = new File(directory, name);
                output = new FileOutputStream(file);
                location = file.getAbsolutePath();
            }
            try (OutputStream stream = output) {
                if (stream == null || !bitmap.compress(Bitmap.CompressFormat.PNG, 100, stream)) throw new IllegalStateException("PNG encoding failed");
            }
            Toast.makeText(this, "Saved " + location, Toast.LENGTH_LONG).show();
        } catch (Exception ex) {
            Toast.makeText(this, ex.getMessage(), Toast.LENGTH_LONG).show();
        } finally {
            bitmap.recycle();
        }
    }

    private void setStatus(String value, Ui.Tone tone, String chip) {
        runOnUiThread(() -> showStatus(value, tone, chip));
    }

    /** Shows the detailed status line and a short design-system status chip. Main thread only. */
    private void showStatus(String value, Ui.Tone tone, String chip) {
        status.setText(value == null ? "Unknown screen error" : value);
        if (statusChip != null) ui.setChip(statusChip, tone, chip);
    }

    private String deviceName() {
        String name = null;
        if (Build.VERSION.SDK_INT >= 25) {
            try {
                name = Settings.Global.getString(getContentResolver(), Settings.Global.DEVICE_NAME);
            } catch (Exception ignored) {
            }
        }
        if (name == null || name.trim().isEmpty()) {
            String model = empty(Build.MODEL);
            String maker = empty(Build.MANUFACTURER);
            name = model.toLowerCase(Locale.ROOT).startsWith(maker.toLowerCase(Locale.ROOT)) ? model : (maker + " " + model).trim();
        }
        name = name.trim();
        return name.isEmpty() ? "Android" : name;
    }

    private static String message(Throwable error) {
        String value = error == null ? "" : empty(error.getMessage());
        return value.isEmpty() ? "unknown screen error" : value;
    }

    /** Zoomable framebuffer view with the remote cursor overlay and remote-control gestures. */
    static final class ZoomImageView extends ImageView {
        interface ZoomListener { void onZoomChanged(float percent, boolean autoFit); }

        interface RemoteInput {
            boolean controlling();
            void onTap(float x, float y);
            void onRightClick(float x, float y);
            void onPointer(float x, float y);
            void onDragStart(float x, float y);
            void onDragMove(float x, float y);
            void onDragEnd(float x, float y);
            void onScroll(float x, float y, float dx, float dy);
            void onScrollEnd();
        }

        private enum Gesture { IDLE, PENDING, PAN, HELD, DRAG, TWO_PENDING, PINCH, SCROLL, DONE }

        private static final long HOLD_MS = 350;
        private static final long TWO_FINGER_TAP_MS = 300;

        private final Matrix transform = new Matrix();
        private final float[] matrixValues = new float[9];
        private final ScaleGestureDetector scaleDetector;
        private final float touchSlop;
        private final Paint cursorPaint = new Paint(Paint.FILTER_BITMAP_FLAG);
        private final Paint markerPaint = new Paint(Paint.ANTI_ALIAS_FLAG);
        private final float[] point = new float[2];
        private ZoomListener zoomListener;
        private RemoteInput remoteInput;
        private float scale = 1f;
        private float requestedScale;
        private float lastX;
        private float lastY;
        private boolean dragging;
        // Remote-control gesture state.
        private Gesture gesture = Gesture.IDLE;
        private float downX;
        private float downY;
        private float twoStartSpan;
        private float twoStartFocusX;
        private float twoStartFocusY;
        private float lastFocusX;
        private float lastFocusY;
        private long twoDownTime;
        private final Runnable holdTask = () -> {
            if (gesture == Gesture.PENDING && remoteInput != null) {
                gesture = Gesture.HELD;
                performHapticFeedback(HapticFeedbackConstants.LONG_PRESS);
                remoteInput.onPointer(downX, downY);
            }
        };
        // Remote cursor.
        private boolean cursorVisible;
        private int cursorX;
        private int cursorY;
        private int cursorHotX;
        private int cursorHotY;
        private Bitmap cursorShape;
        // Touch marker.
        private int markerX;
        private int markerY;
        private long markerUntil;

        ZoomImageView(Context context) {
            super(context);
            setScaleType(ScaleType.MATRIX);
            touchSlop = ViewConfiguration.get(context).getScaledTouchSlop();
            markerPaint.setStyle(Paint.Style.STROKE);
            scaleDetector = new ScaleGestureDetector(context, new ScaleGestureDetector.SimpleOnScaleGestureListener() {
                @Override public boolean onScaleBegin(ScaleGestureDetector detector) {
                    if (getDrawable() == null) return false;
                    if (requestedScale == 0) requestedScale = scale;
                    return true;
                }

                @Override public boolean onScale(ScaleGestureDetector detector) {
                    if (isControlling() && gesture != Gesture.PINCH) return false;
                    float next = clampScale(scale * detector.getScaleFactor());
                    if (Float.isNaN(next) || Float.isInfinite(next) || next == scale) return false;
                    float factor = next / scale;
                    transform.postScale(factor, factor, detector.getFocusX(), detector.getFocusY());
                    scale = next;
                    requestedScale = next;
                    clampTranslation();
                    setImageMatrix(transform);
                    notifyZoom();
                    return true;
                }
            });
        }

        void setZoomListener(ZoomListener listener) {
            zoomListener = listener;
        }

        void setRemoteInput(RemoteInput input) {
            remoteInput = input;
        }

        void setAutoFit() {
            requestedScale = 0;
            rebuildTransform();
        }

        void setZoomPercent(float percent) {
            requestedScale = clampScale(percent / 100f);
            rebuildTransform();
        }

        void setCursor(boolean visible, int x, int y, Bitmap shape, int hotX, int hotY) {
            cursorVisible = visible;
            cursorX = x;
            cursorY = y;
            cursorShape = shape;
            cursorHotX = hotX;
            cursorHotY = hotY;
            invalidate();
        }

        /** Briefly marks where a click lands, in framebuffer pixels. */
        void showTouch(int x, int y) {
            markerX = x;
            markerY = y;
            markerUntil = SystemClock.uptimeMillis() + 350;
            invalidate();
            postDelayed(this::invalidate, 360);
        }

        /** Maps a view point to framebuffer pixels, or null when no framebuffer is shown. */
        int[] toFramebuffer(float x, float y) {
            Drawable drawable = getDrawable();
            if (drawable == null || drawable.getIntrinsicWidth() <= 0 || drawable.getIntrinsicHeight() <= 0) return null;
            transform.getValues(matrixValues);
            return LiveScreenProtocol.viewToFramebuffer(x, y, matrixValues[Matrix.MSCALE_X],
                    matrixValues[Matrix.MTRANS_X], matrixValues[Matrix.MTRANS_Y],
                    drawable.getIntrinsicWidth(), drawable.getIntrinsicHeight());
        }

        @Override
        public void setImageBitmap(Bitmap bitmap) {
            Drawable old = getDrawable();
            int oldWidth = old == null ? 0 : old.getIntrinsicWidth();
            int oldHeight = old == null ? 0 : old.getIntrinsicHeight();
            super.setImageBitmap(bitmap);
            post(() -> {
                Drawable next = getDrawable();
                boolean dimensionsChanged = next == null || oldWidth != next.getIntrinsicWidth() || oldHeight != next.getIntrinsicHeight();
                if (requestedScale == 0 || dimensionsChanged) rebuildTransform();
                else {
                    clampTranslation();
                    setImageMatrix(transform);
                }
            });
        }

        @Override
        protected void onSizeChanged(int width, int height, int oldWidth, int oldHeight) {
            super.onSizeChanged(width, height, oldWidth, oldHeight);
            if (requestedScale == 0) rebuildTransform();
            else {
                clampTranslation();
                setImageMatrix(transform);
            }
        }

        @Override
        protected void onDraw(Canvas canvas) {
            super.onDraw(canvas);
            if (getDrawable() == null) return;
            if (cursorVisible) {
                canvas.save();
                canvas.concat(transform);
                if (cursorShape != null && !cursorShape.isRecycled()) {
                    canvas.drawBitmap(cursorShape, cursorX - cursorHotX, cursorY - cursorHotY, cursorPaint);
                } else {
                    float radius = 4f / Math.max(scale, 0.1f);
                    markerPaint.setStyle(Paint.Style.FILL);
                    markerPaint.setColor(Color.WHITE);
                    canvas.drawCircle(cursorX, cursorY, radius, markerPaint);
                    markerPaint.setStyle(Paint.Style.STROKE);
                    markerPaint.setStrokeWidth(1.5f / Math.max(scale, 0.1f));
                    markerPaint.setColor(Color.BLACK);
                    canvas.drawCircle(cursorX, cursorY, radius, markerPaint);
                }
                canvas.restore();
            }
            long now = SystemClock.uptimeMillis();
            if (now < markerUntil) {
                point[0] = markerX + 0.5f;
                point[1] = markerY + 0.5f;
                transform.mapPoints(point);
                float progress = 1f - (markerUntil - now) / 350f;
                float density = getResources().getDisplayMetrics().density;
                markerPaint.setStyle(Paint.Style.STROKE);
                markerPaint.setStrokeWidth(2f * density);
                markerPaint.setColor(Ui.alpha(Color.rgb(0x4F, 0x8B, 0xFF), 1f - progress));
                canvas.drawCircle(point[0], point[1], (10f + 14f * progress) * density, markerPaint);
                postInvalidateOnAnimation();
            }
        }

        private boolean isControlling() {
            return remoteInput != null && remoteInput.controlling();
        }

        @Override
        public boolean onTouchEvent(MotionEvent event) {
            if (getParent() != null) getParent().requestDisallowInterceptTouchEvent(true);
            scaleDetector.onTouchEvent(event);
            if (isControlling() || gesture != Gesture.IDLE) {
                handleControlTouch(event);
                return true;
            }
            switch (event.getActionMasked()) {
                case MotionEvent.ACTION_DOWN:
                    lastX = event.getX();
                    lastY = event.getY();
                    dragging = true;
                    break;
                case MotionEvent.ACTION_MOVE:
                    if (dragging && event.getPointerCount() == 1 && !scaleDetector.isInProgress()) {
                        pan(event.getX(), event.getY());
                    }
                    break;
                case MotionEvent.ACTION_UP:
                    performClick();
                    dragging = false;
                    break;
                case MotionEvent.ACTION_CANCEL:
                    dragging = false;
                    break;
                case MotionEvent.ACTION_POINTER_UP:
                    if (event.getPointerCount() > 1) {
                        int remaining = event.getActionIndex() == 0 ? 1 : 0;
                        lastX = event.getX(remaining);
                        lastY = event.getY(remaining);
                    }
                    break;
                default:
                    break;
            }
            return true;
        }

        private void pan(float x, float y) {
            transform.postTranslate(x - lastX, y - lastY);
            lastX = x;
            lastY = y;
            clampTranslation();
            setImageMatrix(transform);
        }

        /**
         * Remote-control gestures: tap = click, hold = right click, hold then drag = left drag,
         * two-finger tap = right click, two-finger drag = scroll, pinch = local zoom, and a quick
         * one-finger drag still pans the local view.
         */
        private void handleControlTouch(MotionEvent event) {
            switch (event.getActionMasked()) {
                case MotionEvent.ACTION_DOWN:
                    downX = lastX = event.getX();
                    downY = lastY = event.getY();
                    gesture = isControlling() ? Gesture.PENDING : Gesture.IDLE;
                    removeCallbacks(holdTask);
                    if (gesture == Gesture.PENDING) postDelayed(holdTask, HOLD_MS);
                    break;
                case MotionEvent.ACTION_POINTER_DOWN:
                    removeCallbacks(holdTask);
                    if (gesture == Gesture.PENDING || gesture == Gesture.HELD || gesture == Gesture.PAN) {
                        gesture = Gesture.TWO_PENDING;
                        twoDownTime = event.getEventTime();
                        twoStartSpan = span(event);
                        twoStartFocusX = lastFocusX = focusX(event);
                        twoStartFocusY = lastFocusY = focusY(event);
                    } else if (gesture == Gesture.DRAG) {
                        remoteInput.onDragEnd(lastX, lastY);
                        gesture = Gesture.DONE;
                    }
                    break;
                case MotionEvent.ACTION_MOVE: {
                    float x = event.getX();
                    float y = event.getY();
                    if (event.getPointerCount() == 1) {
                        boolean moved = Math.hypot(x - downX, y - downY) > touchSlop;
                        if (gesture == Gesture.PENDING && moved) {
                            removeCallbacks(holdTask);
                            gesture = Gesture.PAN;
                        } else if (gesture == Gesture.HELD && moved) {
                            gesture = Gesture.DRAG;
                            remoteInput.onDragStart(downX, downY);
                        }
                        if (gesture == Gesture.PAN) pan(x, y);
                        else if (gesture == Gesture.DRAG) remoteInput.onDragMove(x, y);
                        if (gesture != Gesture.PAN) {
                            lastX = x;
                            lastY = y;
                        }
                    } else {
                        float fx = focusX(event);
                        float fy = focusY(event);
                        if (gesture == Gesture.TWO_PENDING) {
                            float spanDelta = Math.abs(span(event) - twoStartSpan);
                            float focusDelta = (float) Math.hypot(fx - twoStartFocusX, fy - twoStartFocusY);
                            if (spanDelta > touchSlop * 2 && spanDelta > focusDelta) {
                                gesture = Gesture.PINCH;
                            } else if (focusDelta > touchSlop) {
                                gesture = Gesture.SCROLL;
                                lastFocusX = fx;
                                lastFocusY = fy;
                            }
                        } else if (gesture == Gesture.SCROLL) {
                            remoteInput.onScroll(fx, fy, fx - lastFocusX, fy - lastFocusY);
                            lastFocusX = fx;
                            lastFocusY = fy;
                        }
                    }
                    break;
                }
                case MotionEvent.ACTION_POINTER_UP:
                    if (gesture == Gesture.TWO_PENDING) {
                        if (event.getEventTime() - twoDownTime <= TWO_FINGER_TAP_MS) {
                            remoteInput.onRightClick(twoStartFocusX, twoStartFocusY);
                        }
                        gesture = Gesture.DONE;
                    } else if (gesture == Gesture.SCROLL || gesture == Gesture.PINCH) {
                        if (gesture == Gesture.SCROLL) remoteInput.onScrollEnd();
                        gesture = Gesture.DONE;
                    }
                    break;
                case MotionEvent.ACTION_UP:
                    removeCallbacks(holdTask);
                    if (gesture == Gesture.PENDING) {
                        remoteInput.onTap(event.getX(), event.getY());
                    } else if (gesture == Gesture.HELD) {
                        remoteInput.onRightClick(downX, downY);
                    } else if (gesture == Gesture.DRAG) {
                        remoteInput.onDragEnd(event.getX(), event.getY());
                    }
                    performClick();
                    gesture = Gesture.IDLE;
                    break;
                case MotionEvent.ACTION_CANCEL:
                    removeCallbacks(holdTask);
                    if (gesture == Gesture.DRAG) remoteInput.onDragEnd(lastX, lastY);
                    if (gesture == Gesture.SCROLL) remoteInput.onScrollEnd();
                    gesture = Gesture.IDLE;
                    break;
                default:
                    break;
            }
        }

        private static float span(MotionEvent event) {
            if (event.getPointerCount() < 2) return 0;
            return (float) Math.hypot(event.getX(0) - event.getX(1), event.getY(0) - event.getY(1));
        }

        private static float focusX(MotionEvent event) {
            float sum = 0;
            for (int i = 0; i < event.getPointerCount(); i++) sum += event.getX(i);
            return sum / Math.max(1, event.getPointerCount());
        }

        private static float focusY(MotionEvent event) {
            float sum = 0;
            for (int i = 0; i < event.getPointerCount(); i++) sum += event.getY(i);
            return sum / Math.max(1, event.getPointerCount());
        }

        @Override public boolean performClick() {
            super.performClick();
            return true;
        }

        private void rebuildTransform() {
            Drawable drawable = getDrawable();
            if (drawable == null || getWidth() <= 0 || getHeight() <= 0 || drawable.getIntrinsicWidth() <= 0 || drawable.getIntrinsicHeight() <= 0) return;
            float fit = Math.min((float) getWidth() / drawable.getIntrinsicWidth(), (float) getHeight() / drawable.getIntrinsicHeight());
            scale = requestedScale == 0 ? fit : requestedScale;
            transform.reset();
            transform.postScale(scale, scale);
            transform.postTranslate((getWidth() - drawable.getIntrinsicWidth() * scale) / 2f,
                    (getHeight() - drawable.getIntrinsicHeight() * scale) / 2f);
            setImageMatrix(transform);
            notifyZoom();
        }

        private void clampTranslation() {
            Drawable drawable = getDrawable();
            if (drawable == null) return;
            transform.getValues(matrixValues);
            float width = drawable.getIntrinsicWidth() * scale;
            float height = drawable.getIntrinsicHeight() * scale;
            float x = matrixValues[Matrix.MTRANS_X];
            float y = matrixValues[Matrix.MTRANS_Y];
            float wantedX = width <= getWidth() ? (getWidth() - width) / 2f : Math.max(getWidth() - width, Math.min(0, x));
            float wantedY = height <= getHeight() ? (getHeight() - height) / 2f : Math.max(getHeight() - height, Math.min(0, y));
            transform.postTranslate(wantedX - x, wantedY - y);
        }

        private void notifyZoom() {
            if (zoomListener != null) zoomListener.onZoomChanged(scale * 100f, requestedScale == 0);
        }

        private static float clampScale(float value) {
            return Math.max(.1f, Math.min(16f, value));
        }
    }

    private static String empty(String value) { return value == null ? "" : value.trim(); }
    private int dp(float value) { return (int) (value * getResources().getDisplayMetrics().density + 0.5f); }
}
