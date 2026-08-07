package com.blhsing.deskferry.home;

import android.Manifest;
import android.app.Activity;
import android.content.ContentValues;
import android.content.pm.PackageManager;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.graphics.Canvas;
import android.graphics.Color;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.provider.MediaStore;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.Window;
import android.widget.Button;
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
    static final String EXTRA_RELAY_URLS = "relay_urls";
    static final String EXTRA_PROXY = "proxy";
    static final String EXTRA_ROOM_PROOF = "room_proof";
    static final String EXTRA_DESTINATION = "destination";

    private static final int REQUEST_WRITE_IMAGES = 2001;
    private final Object lock = new Object();
    private final Buffer wire = new Buffer();
    private ImageView imageView;
    private TextView status;
    private Spinner interval;
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

        status = new TextView(this);
        status.setTextColor(Color.WHITE);
        status.setPadding(dp(10), dp(7), dp(10), dp(7));
        status.setText("Ready");
        root.addView(status, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        imageView = new ImageView(this);
        imageView.setScaleType(ImageView.ScaleType.FIT_CENTER);
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

    private void startCapture(String mode) {
        stopCapture(false);
        int run = ++generation;
        requestedMode = mode;
        requestedInterval = selectedInterval();
        candidateIndex = 0;
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
                webSocket.send(ByteString.encodeUtf8(request.toString() + "\n"));
                setStatus("Connected through " + relay + "; waiting for the first frame...");
            } catch (Exception ex) {
                webSocket.cancel();
                if (!paired) connectNext(run, ex); else setStatus(ex.getMessage());
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
                webSocket.cancel();
                setStatus("Screen frame failed: " + ex.getMessage());
            }
        }

        @Override
        public void onFailure(WebSocket webSocket, Throwable error, Response response) {
            if (run != generation || closing || webSocket != socket) return;
            if (!paired) connectNext(run, error); else setStatus("Screen stream ended: " + error.getMessage());
        }

        @Override
        public void onClosed(WebSocket webSocket, int code, String reason) {
            if (run == generation && webSocket == socket && !closing && !"single".equals(requestedMode)) setStatus("Screen stream stopped.");
        }
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

    private static String empty(String value) { return value == null ? "" : value.trim(); }
    private int dp(float value) { return (int) (value * getResources().getDisplayMetrics().density + 0.5f); }
}
