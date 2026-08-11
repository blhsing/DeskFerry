package com.blhsing.deskferry.home;

import android.Manifest;
import android.app.Activity;
import android.content.ContentValues;
import android.content.pm.PackageManager;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.Matrix;
import android.graphics.drawable.Drawable;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.os.Handler;
import android.os.Looper;
import android.provider.MediaStore;
import android.text.InputType;
import android.util.Log;
import android.view.Gravity;
import android.view.MotionEvent;
import android.view.ScaleGestureDetector;
import android.view.View;
import android.view.ViewGroup;
import android.view.Window;
import android.view.inputmethod.EditorInfo;
import android.widget.AdapterView;
import android.widget.Button;
import android.widget.EditText;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.Spinner;
import android.widget.ArrayAdapter;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.File;
import java.io.FileOutputStream;
import java.io.OutputStream;
import java.net.URISyntaxException;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;

import okhttp3.OkHttpClient;
import okhttp3.Request;
import okhttp3.Response;
import okhttp3.WebSocket;
import okhttp3.WebSocketListener;
import okio.Buffer;
import okio.ByteString;

public class ScreenViewerActivity extends Activity {
    private static final String LOG_TAG = "DeskFerryScreen";
    private static final int MAX_RECOVERY_RETRIES = 2;
    private static final long RECOVERY_DELAY_MS = 600;
    static final String EXTRA_RELAY_URLS = "relay_urls";
    static final String EXTRA_PROXY = "proxy";
    static final String EXTRA_ROOM_PROOF = "room_proof";
    static final String EXTRA_DESTINATION = "destination";

    private static final int REQUEST_WRITE_IMAGES = 2001;
    private final Object lock = new Object();
    private final Buffer wire = new Buffer();
    private final Handler mainHandler = new Handler(Looper.getMainLooper());
    private ZoomImageView imageView;
    private TextView status;
    private Spinner interval;
    private Spinner zoomPreset;
    private EditText zoomInput;
    private TextView zoomValue;
    private OkHttpClient client;
    private WebSocket socket;
    private Bitmap current;
    private List<String> relayUrls;
    private String proxy;
    private String roomProof;
    private int candidateIndex;
    private String requestedMode;
    private int requestedInterval;
    private boolean paired;
    private boolean closing;
    private int metadataLength = -1;
    private JSONObject metadata;
    private int payloadLength;
    private boolean fullscreen;
    private int generation;
    private int recoveryAttempts;
    private boolean updatingZoomControls;

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        requestWindowFeature(Window.FEATURE_NO_TITLE);
        buildUi();
        try {
            relayUrls = RelayUrls.normalizeRelayUrls(getIntent().getStringExtra(EXTRA_RELAY_URLS), false);
            proxy = ProxySettings.normalize(getIntent().getStringExtra(EXTRA_PROXY));
            roomProof = empty(getIntent().getStringExtra(EXTRA_ROOM_PROOF));
            OkHttpClient.Builder builder = new OkHttpClient.Builder()
                    .connectTimeout(15, TimeUnit.SECONDS)
                    .readTimeout(0, TimeUnit.MILLISECONDS)
                    .pingInterval(20, TimeUnit.SECONDS);
            ProxySettings.apply(builder, proxy);
            client = builder.build();
            if (relayUrls.isEmpty() || roomProof.isEmpty()) {
                throw new IllegalArgumentException("Relay URLs and a saved room password are required.");
            }
            startCapture("single");
        } catch (Exception ex) {
            setStatus(ex.getMessage());
        }
    }

    @Override
    protected void onDestroy() {
        closing = true;
        stopCapture(false);
        if (client != null) client.dispatcher().executorService().shutdown();
        super.onDestroy();
    }

    private void buildUi() {
        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(Color.rgb(22, 25, 31));

        LinearLayout controls = new LinearLayout(this);
        controls.setOrientation(LinearLayout.HORIZONTAL);
        controls.setGravity(Gravity.CENTER_VERTICAL);
        controls.setPadding(dp(8), dp(8), dp(8), dp(8));
        controls.setBackgroundColor(Color.rgb(38, 43, 52));
        TextView version = new TextView(this);
        version.setText("v" + BuildConfig.VERSION_NAME);
        version.setTextColor(Color.WHITE);
        version.setPadding(0, 0, dp(6), 0);
        controls.addView(version);
        addButton(controls, "Capture", v -> startCapture("single"));
        addButton(controls, "Stream", v -> startCapture("stream"));
        addButton(controls, "Stop", v -> stopCapture(true));
        interval = new Spinner(this);
        interval.setAdapter(new ArrayAdapter<>(this, android.R.layout.simple_spinner_dropdown_item,
                new String[]{"0.5 s", "1 s", "2 s", "5 s"}));
        interval.setSelection(1);
        controls.addView(interval, new LinearLayout.LayoutParams(dp(90), dp(48)));
        addButton(controls, "Full", v -> toggleFullscreen());
        addButton(controls, "Save", v -> savePNG());
        root.addView(controls, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        LinearLayout zoomControls = new LinearLayout(this);
        zoomControls.setOrientation(LinearLayout.HORIZONTAL);
        zoomControls.setGravity(Gravity.CENTER_VERTICAL);
        zoomControls.setPadding(dp(8), 0, dp(8), dp(6));
        zoomControls.setBackgroundColor(Color.rgb(38, 43, 52));
        TextView zoomLabel = new TextView(this);
        zoomLabel.setText("Zoom ");
        zoomLabel.setTextColor(Color.WHITE);
        zoomControls.addView(zoomLabel);
        zoomPreset = new Spinner(this);
        zoomPreset.setAdapter(new ArrayAdapter<>(this, android.R.layout.simple_spinner_dropdown_item,
                new String[]{"Auto Fit", "50%", "75%", "100%", "125%", "150%", "200%", "300%", "400%", "Custom"}));
        zoomPreset.setSelection(0);
        zoomPreset.setOnItemSelectedListener(new AdapterView.OnItemSelectedListener() {
            @Override public void onItemSelected(AdapterView<?> parent, View view, int position, long id) {
                if (updatingZoomControls || imageView == null || position == 9) return;
                float[] values = new float[]{0, 50, 75, 100, 125, 150, 200, 300, 400};
                if (position == 0) imageView.setAutoFit(); else imageView.setZoomPercent(values[position]);
            }
            @Override public void onNothingSelected(AdapterView<?> parent) { }
        });
        zoomControls.addView(zoomPreset, new LinearLayout.LayoutParams(dp(112), dp(48)));
        zoomInput = new EditText(this);
        zoomInput.setSingleLine(true);
        zoomInput.setHint("10-1600%");
        zoomInput.setTextColor(Color.WHITE);
        zoomInput.setHintTextColor(Color.LTGRAY);
        zoomInput.setInputType(InputType.TYPE_CLASS_NUMBER | InputType.TYPE_NUMBER_FLAG_DECIMAL);
        zoomInput.setImeOptions(EditorInfo.IME_ACTION_DONE);
        zoomInput.setOnEditorActionListener((view, action, event) -> {
            if (action == EditorInfo.IME_ACTION_DONE) {
                applyCustomZoom();
                return true;
            }
            return false;
        });
        zoomControls.addView(zoomInput, new LinearLayout.LayoutParams(dp(104), dp(48)));
        addButton(zoomControls, "Apply", v -> applyCustomZoom());
        zoomValue = new TextView(this);
        zoomValue.setText("Auto Fit");
        zoomValue.setTextColor(Color.WHITE);
        zoomValue.setPadding(dp(10), 0, 0, 0);
        zoomControls.addView(zoomValue);
        root.addView(zoomControls, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        status = new TextView(this);
        status.setTextColor(Color.WHITE);
        status.setPadding(dp(10), dp(7), dp(10), dp(7));
        status.setText("Ready");
        root.addView(status, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        imageView = new ZoomImageView(this);
        imageView.setZoomListener(this::showZoomValue);
        imageView.setBackgroundColor(Color.BLACK);
        root.addView(imageView, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));
        setContentView(root);
    }

    private void addButton(LinearLayout parent, String text, View.OnClickListener listener) {
        Button button = new Button(this);
        button.setText(text);
        button.setAllCaps(false);
        button.setOnClickListener(listener);
        parent.addView(button, new LinearLayout.LayoutParams(0, dp(48), 1f));
    }

    private int selectedInterval() {
        return new int[]{500, 1000, 2000, 5000}[Math.max(0, Math.min(3, interval.getSelectedItemPosition()))];
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
            zoomValue.setText(autoFit ? "Auto Fit" : String.format(java.util.Locale.US, "%.1f%%", percent));
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

    private void startCapture(String mode) {
        stopCapture(false);
        int run = ++generation;
        requestedMode = mode;
        requestedInterval = selectedInterval();
        candidateIndex = 0;
        recoveryAttempts = 0;
        paired = false;
        closing = false;
        synchronized (lock) {
            wire.clear();
            metadataLength = -1;
            metadata = null;
            payloadLength = 0;
        }
        setStatus("Connecting to the Work screen service...");
        connectNext(run, null);
    }

    private void connectNext(int run, Throwable previous) {
        if (run != generation || closing) {
            return;
        }
        if (candidateIndex >= relayUrls.size()) {
            setStatus("Screen connection failed: " + (previous == null ? "all relay services failed" : previous.getMessage()));
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
                paired = true;
                JSONObject request = new JSONObject()
                        .put("mode", requestedMode)
                        .put("interval_ms", requestedInterval)
                        .put("tile_size", 64);
                if (!webSocket.send(ByteString.encodeUtf8(request.toString() + "\n"))) {
                    throw new IllegalStateException("could not send the screen capture request");
                }
                setStatus("Connected through " + relay + "; waiting for the first frame...");
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
            if (run != generation || webSocket != socket) return;
            try {
                synchronized (lock) {
                    wire.write(bytes);
                    parseFrames();
                }
            } catch (Exception ex) {
                recoverScreenSession(run, webSocket, "Screen frame failed", ex);
            }
        }

        @Override
        public void onFailure(WebSocket webSocket, Throwable error, Response response) {
            if (run != generation || closing || webSocket != socket) return;
            if (!paired) connectNext(run, error); else recoverScreenSession(run, webSocket, "Screen stream ended", error);
        }

        @Override
        public void onClosed(WebSocket webSocket, int code, String reason) {
            if (run == generation && webSocket == socket && !closing && !"single".equals(requestedMode)) {
                recoverScreenSession(run, webSocket, "Screen stream ended",
                        new IllegalStateException("relay closed code=" + code + (reason.isEmpty() ? "" : " reason=" + reason)));
            }
        }
    }

    private void recoverScreenSession(int run, WebSocket failedSocket, String label, Throwable failure) {
        if (run != generation || closing || failedSocket != socket) return;
        String detail = failure == null || empty(failure.getMessage()).isEmpty()
                ? "unknown screen error" : failure.getMessage();
        recoveryAttempts++;
        Log.w(LOG_TAG, label + ": " + detail + " recovery_attempt=" + recoveryAttempts, failure);
        socket = null;
        failedSocket.cancel();
        paired = false;
        synchronized (lock) {
            wire.clear();
            metadataLength = -1;
            metadata = null;
            payloadLength = 0;
        }
        if (!canRetryScreenSession(recoveryAttempts)) {
            setStatus(label + " after recovery attempts: " + detail);
            return;
        }
        candidateIndex = 0;
        setStatus(label + ": " + detail + ". Retrying (" + recoveryAttempts + "/" + MAX_RECOVERY_RETRIES + ")...");
        mainHandler.postDelayed(() -> {
            if (run == generation && !closing && socket == null) connectNext(run, failure);
        }, RECOVERY_DELAY_MS * recoveryAttempts);
    }

    static boolean canRetryScreenSession(int failedSessions) {
        return failedSessions <= MAX_RECOVERY_RETRIES;
    }

    private void parseFrames() throws Exception {
        while (true) {
            if (metadataLength < 0) {
                if (wire.size() < 4) return;
                metadataLength = wire.readInt();
                if (metadataLength <= 0 || metadataLength > 1024 * 1024) throw new IllegalStateException("invalid frame metadata size");
            }
            if (metadata == null) {
                if (wire.size() < metadataLength) return;
                metadata = new JSONObject(wire.readUtf8(metadataLength));
                long totalPayloadLength = 0;
                JSONArray rects = metadata.optJSONArray("rects");
                if (rects != null) {
                    for (int i = 0; i < rects.length(); i++) {
                        int length = rects.getJSONObject(i).getInt("length");
                        if (length <= 0) throw new IllegalStateException("invalid screen tile length");
                        totalPayloadLength += length;
                    }
                }
                if (totalPayloadLength > 256L * 1024 * 1024) throw new IllegalStateException("screen frame is too large");
                payloadLength = (int) totalPayloadLength;
            }
            if (wire.size() < payloadLength) return;
            JSONObject frame = metadata;
            metadataLength = -1;
            metadata = null;
            applyFrame(frame);
        }
    }

    private void applyFrame(JSONObject frame) throws Exception {
        String type = frame.optString("type");
        if ("error".equals(type)) throw new IllegalStateException(frame.optString("error", "screen capture failed"));
        int width = frame.getInt("width");
        int height = frame.getInt("height");
        if (width <= 0 || height <= 0 || (long) width * height > 64L * 1024 * 1024) {
            throw new IllegalStateException("invalid screen dimensions");
        }
        Bitmap next;
        if ("full".equals(type)) {
            next = Bitmap.createBitmap(width, height, Bitmap.Config.ARGB_8888);
        } else if ("delta".equals(type) && current != null && current.getWidth() == width && current.getHeight() == height) {
            next = current.copy(Bitmap.Config.ARGB_8888, true);
        } else {
            throw new IllegalStateException("received a screen delta before its full frame");
        }
        Canvas canvas = new Canvas(next);
        JSONArray rects = frame.optJSONArray("rects");
        int changed = rects == null ? 0 : rects.length();
        for (int i = 0; i < changed; i++) {
            JSONObject rect = rects.getJSONObject(i);
            int length = rect.getInt("length");
            int x = rect.getInt("x");
            int y = rect.getInt("y");
            int tileWidth = rect.getInt("width");
            int tileHeight = rect.getInt("height");
            if (x < 0 || y < 0 || tileWidth <= 0 || tileHeight <= 0 || x + (long) tileWidth > width || y + (long) tileHeight > height) {
                throw new IllegalStateException("invalid screen tile dimensions");
            }
            byte[] payload = wire.readByteArray(length);
            Bitmap tile = BitmapFactory.decodeByteArray(payload, 0, payload.length);
            if (tile == null) throw new IllegalStateException("invalid PNG screen tile");
            if (tile.getWidth() != tileWidth || tile.getHeight() != tileHeight) {
                tile.recycle();
                throw new IllegalStateException("screen tile does not match its metadata");
            }
            canvas.drawBitmap(tile, x, y, null);
            tile.recycle();
        }
        Bitmap previous = current;
        current = next;
        runOnUiThread(() -> {
            imageView.setImageBitmap(next);
            if (previous != null && previous != next && !previous.isRecycled()) previous.recycle();
            status.setText("stream".equals(requestedMode)
                    ? "Streaming frame " + frame.optLong("seq") + " (" + changed + " changed tiles)."
                    : "Screenshot captured.");
        });
        if ("single".equals(requestedMode) && socket != null) socket.close(1000, "capture complete");
    }

    private void stopCapture(boolean announce) {
        generation++;
        closing = true;
        WebSocket value = socket;
        socket = null;
        if (value != null) value.cancel();
        if (announce) setStatus("Screen stream stopped.");
    }

    private void toggleFullscreen() {
        fullscreen = !fullscreen;
        getWindow().getDecorView().setSystemUiVisibility(fullscreen
                ? View.SYSTEM_UI_FLAG_FULLSCREEN | View.SYSTEM_UI_FLAG_HIDE_NAVIGATION | View.SYSTEM_UI_FLAG_IMMERSIVE_STICKY
                : View.SYSTEM_UI_FLAG_VISIBLE);
    }

    private void savePNG() {
        if (current == null) {
            Toast.makeText(this, "Capture a screenshot before saving.", Toast.LENGTH_SHORT).show();
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
        String name = "DeskFerry-" + new java.text.SimpleDateFormat("yyyyMMdd-HHmmss", java.util.Locale.US).format(new java.util.Date()) + ".png";
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

    private void setStatus(String value) {
        runOnUiThread(() -> status.setText(value == null ? "Unknown screen error" : value));
    }

    private static final class ZoomImageView extends ImageView {
        interface ZoomListener { void onZoomChanged(float percent, boolean autoFit); }

        private final Matrix transform = new Matrix();
        private final float[] matrixValues = new float[9];
        private final ScaleGestureDetector scaleDetector;
        private ZoomListener zoomListener;
        private float scale = 1f;
        private float requestedScale;
        private float lastX;
        private float lastY;
        private boolean dragging;

        ZoomImageView(android.content.Context context) {
            super(context);
            setScaleType(ScaleType.MATRIX);
            scaleDetector = new ScaleGestureDetector(context, new ScaleGestureDetector.SimpleOnScaleGestureListener() {
                @Override public boolean onScaleBegin(ScaleGestureDetector detector) {
                    if (getDrawable() == null) return false;
                    if (requestedScale == 0) requestedScale = scale;
                    return true;
                }

                @Override public boolean onScale(ScaleGestureDetector detector) {
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

        void setAutoFit() {
            requestedScale = 0;
            rebuildTransform();
        }

        void setZoomPercent(float percent) {
            requestedScale = clampScale(percent / 100f);
            rebuildTransform();
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
        public boolean onTouchEvent(MotionEvent event) {
            if (getParent() != null) getParent().requestDisallowInterceptTouchEvent(true);
            scaleDetector.onTouchEvent(event);
            switch (event.getActionMasked()) {
                case MotionEvent.ACTION_DOWN:
                    lastX = event.getX();
                    lastY = event.getY();
                    dragging = true;
                    break;
                case MotionEvent.ACTION_MOVE:
                    if (dragging && event.getPointerCount() == 1 && !scaleDetector.isInProgress()) {
                        float x = event.getX();
                        float y = event.getY();
                        transform.postTranslate(x - lastX, y - lastY);
                        lastX = x;
                        lastY = y;
                        clampTranslation();
                        setImageMatrix(transform);
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
