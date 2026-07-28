package com.blhsing.deskferry.home;

import android.Manifest;
import android.app.Activity;
import android.content.BroadcastReceiver;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.PackageManager;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.text.Editable;
import android.text.InputType;
import android.text.TextWatcher;
import android.view.DragEvent;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.widget.Button;
import android.widget.EditText;
import android.widget.GridLayout;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import java.net.URISyntaxException;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Locale;

public class MainActivity extends Activity {
    private final BroadcastReceiver stateReceiver = new BroadcastReceiver() {
        @Override
        public void onReceive(Context context, Intent intent) {
            renderState(TunnelService.snapshot());
        }
    };

    private final ArrayList<String> relayUrls = new ArrayList<>();
    private LinearLayout relayUrlList;
    private EditText relayUrlAddField;
    private Button relayAddButton;
    private EditText localPortField;
    private EditText proxyField;
    private TextView tunnelStatus;
    private TextView workStatus;
    private TextView homeStatus;
    private TextView rdpAddress;
    private TextView activeStatus;
    private TextView messageView;
    private TextView logView;
    private Button startButton;
    private String latestRdpAddress = RelayUrls.rdpAddress(HomePrefs.DEFAULT_LOCAL_PORT);
    private int draggedRelayIndex = -1;
    private boolean relayRowsEnabled = true;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        maybeRequestNotificationPermission();
        buildUi();
        loadPreferences();
        renderState(TunnelService.snapshot());
    }

    @Override
    protected void onResume() {
        super.onResume();
        IntentFilter filter = new IntentFilter(TunnelService.ACTION_STATE);
        if (Build.VERSION.SDK_INT >= 33) {
            registerReceiver(stateReceiver, filter, Context.RECEIVER_NOT_EXPORTED);
        } else {
            registerReceiver(stateReceiver, filter);
        }
        renderState(TunnelService.snapshot());
    }

    @Override
    protected void onPause() {
        unregisterReceiver(stateReceiver);
        super.onPause();
    }

    private void buildUi() {
        ScrollView scroll = new ScrollView(this);
        scroll.setFillViewport(true);
        scroll.setBackgroundColor(color("#F5F7F8"));

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(dp(18), dp(18), dp(18), dp(24));
        scroll.addView(root, new ScrollView.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.WRAP_CONTENT));

        LinearLayout header = new LinearLayout(this);
        header.setOrientation(LinearLayout.VERTICAL);
        header.setPadding(0, 0, 0, dp(12));
        root.addView(header);

        TextView title = label("DeskFerry Home", 28, "#1F2933", true);
        title.setLetterSpacing(0);
        header.addView(title);
        TextView subtitle = label("Android RDP tunnel endpoint", 14, "#65717D", false);
        subtitle.setPadding(0, dp(3), 0, 0);
        header.addView(subtitle);

        GridLayout grid = new GridLayout(this);
        grid.setColumnCount(2);
        grid.setUseDefaultMargins(false);
        root.addView(grid, matchWrap());
        tunnelStatus = addStatusTile(grid, "Tunnel", "Stopped");
        workStatus = addStatusTile(grid, "Work Agent", "Unknown");
        homeStatus = addStatusTile(grid, "Home Presence", "Offline");
        activeStatus = addStatusTile(grid, "Streams", "0 active");

        LinearLayout configCard = card();
        configCard.setOrientation(LinearLayout.VERTICAL);
        configCard.setPadding(dp(14), dp(14), dp(14), dp(14));
        root.addView(configCard, cardParams());

        configCard.addView(sectionTitle("Connection"));
        relayUrlList = new LinearLayout(this);
        relayUrlList.setOrientation(LinearLayout.VERTICAL);
        relayUrlList.setOnDragListener((view, event) -> {
            if (event.getAction() == DragEvent.ACTION_DROP && draggedRelayIndex >= 0) {
                moveRelayUrl(draggedRelayIndex, relayUrls.size() - 1);
                draggedRelayIndex = -1;
                return true;
            }
            if (event.getAction() == DragEvent.ACTION_DRAG_ENDED) {
                draggedRelayIndex = -1;
            }
            return true;
        });
        configCard.addView(relayUrlList, matchWrap());

        LinearLayout addRelayRow = new LinearLayout(this);
        addRelayRow.setOrientation(LinearLayout.HORIZONTAL);
        relayUrlAddField = field("Relay room URL");
        relayUrlAddField.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_URI);
        addRelayRow.addView(relayUrlAddField, weightedField());
        relayAddButton = secondaryButton("Add");
        relayAddButton.setOnClickListener(v -> addRelayUrlFromField());
        addRelayRow.addView(relayAddButton, compactButtonParams());
        configCard.addView(addRelayRow, matchWrap());

        localPortField = field("Local RDP port");
        localPortField.setInputType(InputType.TYPE_CLASS_NUMBER);
        configCard.addView(localPortField, matchWrap());

        proxyField = field("Proxy: system, direct, or http(s)://host:port");
        proxyField.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_URI);
        configCard.addView(proxyField, matchWrap());

        rdpAddress = label(RelayUrls.rdpAddress(HomePrefs.DEFAULT_LOCAL_PORT), 20, "#1F2933", true);
        rdpAddress.setPadding(0, dp(10), 0, 0);
        configCard.addView(rdpAddress);

        LinearLayout actions = new LinearLayout(this);
        actions.setOrientation(LinearLayout.VERTICAL);
        actions.setPadding(0, dp(12), 0, 0);
        configCard.addView(actions);

        LinearLayout row1 = actionRow();
        startButton = primaryButton("Start Tunnel");
        startButton.setOnClickListener(v -> toggleTunnel());
        row1.addView(startButton, weightedButton());
        Button copy = secondaryButton("Copy RDP Target");
        copy.setOnClickListener(v -> copyRdpTarget());
        row1.addView(copy, weightedButton());
        actions.addView(row1);

        LinearLayout row2 = actionRow();
        Button openRdp = secondaryButton("Open RDP App");
        openRdp.setOnClickListener(v -> openRdpApp());
        row2.addView(openRdp, weightedButton());
        Button dashboard = secondaryButton("Dashboard");
        dashboard.setOnClickListener(v -> openDashboard());
        row2.addView(dashboard, weightedButton());
        actions.addView(row2);

        LinearLayout statusCard = card();
        statusCard.setOrientation(LinearLayout.VERTICAL);
        statusCard.setPadding(dp(14), dp(14), dp(14), dp(14));
        root.addView(statusCard, cardParams());
        statusCard.addView(sectionTitle("Activity"));
        messageView = label("Ready.", 15, "#1F2933", true);
        messageView.setPadding(0, dp(4), 0, dp(8));
        statusCard.addView(messageView);
        logView = label("", 13, "#65717D", false);
        logView.setTypeface(Typeface.MONOSPACE);
        logView.setLineSpacing(0, 1.08f);
        statusCard.addView(logView);

        setContentView(scroll);
    }

    private TextView addStatusTile(GridLayout grid, String title, String initial) {
        LinearLayout tile = card();
        tile.setOrientation(LinearLayout.VERTICAL);
        tile.setPadding(dp(12), dp(12), dp(12), dp(12));

        TextView label = label(title.toUpperCase(Locale.ROOT), 12, "#65717D", true);
        tile.addView(label);
        TextView value = label(initial, 22, "#1F2933", true);
        value.setPadding(0, dp(8), 0, 0);
        tile.addView(value);

        GridLayout.LayoutParams params = new GridLayout.LayoutParams();
        params.width = 0;
        params.height = ViewGroup.LayoutParams.WRAP_CONTENT;
        params.columnSpec = GridLayout.spec(GridLayout.UNDEFINED, 1f);
        params.setMargins(dp(4), dp(4), dp(4), dp(8));
        grid.addView(tile, params);
        return value;
    }

    private void loadPreferences() {
        List<String> relayUrls;
        try {
            relayUrls = RelayUrls.normalizeRelayUrls(HomePrefs.loadRelayUrl(this));
        } catch (URISyntaxException ex) {
            relayUrls = Collections.singletonList(RelayUrls.DEFAULT_RELAY_URL);
        }
        setRelayUrls(relayUrls);
        localPortField.setText(String.valueOf(HomePrefs.loadLocalPort(this)));
        proxyField.setText(HomePrefs.loadProxy(this));
    }

    private void savePreferences(String relayUrl, int port, String proxy) {
        HomePrefs.save(this, relayUrl, port, proxy);
    }

    private void toggleTunnel() {
        TunnelService.State state = TunnelService.snapshot();
        if (state.running) {
            stopService(new Intent(this, TunnelService.class).setAction(TunnelService.ACTION_STOP));
            return;
        }
        String relayUrl;
        int port;
        String proxy;
        try {
            List<String> normalizedRelayUrls = normalizedRelayUrlsFromRows();
            relayUrl = RelayUrls.joinRelayUrls(normalizedRelayUrls);
            port = parsePort(localPortField.getText().toString());
            proxy = ProxySettings.normalize(proxyField.getText().toString());
        } catch (Exception ex) {
            Toast.makeText(this, ex.getMessage(), Toast.LENGTH_LONG).show();
            return;
        }
        List<String> relayUrls;
        try {
            relayUrls = RelayUrls.normalizeRelayUrls(relayUrl);
        } catch (URISyntaxException ex) {
            relayUrls = Collections.singletonList(RelayUrls.DEFAULT_RELAY_URL);
        }
        setRelayUrls(relayUrls);
        localPortField.setText(String.valueOf(port));
        proxyField.setText(proxy);
        savePreferences(relayUrl, port, proxy);

        Intent intent = new Intent(this, TunnelService.class)
                .setAction(TunnelService.ACTION_START)
                .putExtra(TunnelService.EXTRA_RELAY_URL, relayUrl)
                .putExtra(TunnelService.EXTRA_LOCAL_PORT, port)
                .putExtra(TunnelService.EXTRA_PROXY, proxy);
        if (Build.VERSION.SDK_INT >= 26) {
            startForegroundService(intent);
        } else {
            startService(intent);
        }
    }

    private int parsePort(String value) {
        int port = Integer.parseInt(value.trim());
        if (port <= 0 || port > 65535) {
            throw new IllegalArgumentException("Local RDP port must be 1-65535.");
        }
        return port;
    }

    private void renderState(TunnelService.State state) {
        latestRdpAddress = state.rdpAddress;
        tunnelStatus.setText(state.tunnelStatus);
        workStatus.setText(state.workStatus);
        homeStatus.setText(state.homeStatus);
        activeStatus.setText(state.activeConnections + " active");
        rdpAddress.setText(state.rdpAddress);
        messageView.setText(state.lastMessage);
        logView.setText(state.log);
        startButton.setText(state.running ? "Stop Tunnel" : "Start Tunnel");
        setRelayRowsEnabled(!state.running);
        localPortField.setEnabled(!state.running);
        proxyField.setEnabled(!state.running);
    }

    private void copyRdpTarget() {
        ClipboardManager clipboard = (ClipboardManager) getSystemService(CLIPBOARD_SERVICE);
        clipboard.setPrimaryClip(ClipData.newPlainText("DeskFerry RDP target", latestRdpAddress));
        Toast.makeText(this, "Copied " + latestRdpAddress, Toast.LENGTH_SHORT).show();
    }

    private void openRdpApp() {
        Uri uri = Uri.parse("rdp://" + latestRdpAddress);
        Intent intent = new Intent(Intent.ACTION_VIEW, uri);
        try {
            startActivity(intent);
        } catch (Exception ex) {
            Intent share = new Intent(Intent.ACTION_SEND)
                    .setType("text/plain")
                    .putExtra(Intent.EXTRA_TEXT, latestRdpAddress);
            startActivity(Intent.createChooser(share, "RDP target"));
        }
    }

    private void openDashboard() {
        String relayUrl = relayUrls.isEmpty() ? RelayUrls.DEFAULT_RELAY_URL : relayUrls.get(0);
        try {
            relayUrl = RelayUrls.normalizeRelayUrl(relayUrl);
        } catch (URISyntaxException ignored) {
        }
        startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse(RelayUrls.dashboardUrl(relayUrl))));
    }

    private void setRelayUrls(List<String> values) {
        relayUrls.clear();
        if (values != null) {
            for (String value : values) {
                if (value != null && !value.trim().isEmpty()) {
                    relayUrls.add(value.trim());
                }
            }
        }
        renderRelayRows();
    }

    private List<String> normalizedRelayUrlsFromRows() throws URISyntaxException {
        List<String> normalized = RelayUrls.normalizeRelayUrls(RelayUrls.joinRelayUrls(relayUrls));
        setRelayUrls(normalized);
        return normalized;
    }

    private void addRelayUrlFromField() {
        try {
            String relayUrl = RelayUrls.normalizeRelayUrl(relayUrlAddField.getText().toString());
            for (String existing : relayUrls) {
                if (existing.equalsIgnoreCase(relayUrl)) {
                    relayUrlAddField.setText("");
                    return;
                }
            }
            relayUrls.add(relayUrl);
            relayUrlAddField.setText("");
            renderRelayRows();
        } catch (URISyntaxException ex) {
            Toast.makeText(this, ex.getMessage(), Toast.LENGTH_LONG).show();
        }
    }

    private void renderRelayRows() {
        if (relayUrlList == null) {
            return;
        }
        relayUrlList.removeAllViews();
        for (int i = 0; i < relayUrls.size(); i++) {
            relayUrlList.addView(relayUrlRow(i), relayRowParams());
        }
        setRelayRowsEnabled(relayRowsEnabled);
    }

    private View relayUrlRow(int index) {
        final int rowIndex = index;
        final LinearLayout row = new LinearLayout(this);
        row.setOrientation(LinearLayout.VERTICAL);
        row.setPadding(dp(10), dp(10), dp(10), dp(10));
        row.setBackground(relayRowBackground(false));
        row.setOnDragListener((view, event) -> {
            switch (event.getAction()) {
                case DragEvent.ACTION_DRAG_STARTED:
                    return draggedRelayIndex >= 0;
                case DragEvent.ACTION_DRAG_ENTERED:
                    row.setBackground(relayRowBackground(true));
                    return true;
                case DragEvent.ACTION_DRAG_EXITED:
                    row.setBackground(relayRowBackground(false));
                    return true;
                case DragEvent.ACTION_DROP:
                    moveRelayUrl(draggedRelayIndex, rowIndex);
                    draggedRelayIndex = -1;
                    return true;
                case DragEvent.ACTION_DRAG_ENDED:
                    row.setBackground(relayRowBackground(false));
                    return true;
                default:
                    return true;
            }
        });

        LinearLayout top = new LinearLayout(this);
        top.setOrientation(LinearLayout.HORIZONTAL);
        top.setGravity(Gravity.CENTER_VERTICAL);
        row.addView(top, matchNoMargin());

        TextView role = label(rowIndex == 0 ? "Primary" : "Fallback", 12, "#2F6F73", true);
        role.setGravity(Gravity.CENTER);
        role.setBackground(rounded("#E9F3F1", "#BFD7D3", 8));
        top.addView(role, roleParams());

        EditText edit = field("Relay room URL");
        edit.setText(relayUrls.get(rowIndex));
        edit.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_URI);
        edit.setEnabled(relayRowsEnabled);
        edit.addTextChangedListener(new TextWatcher() {
            @Override
            public void beforeTextChanged(CharSequence s, int start, int count, int after) {
            }

            @Override
            public void onTextChanged(CharSequence s, int start, int before, int count) {
            }

            @Override
            public void afterTextChanged(Editable s) {
                if (rowIndex >= 0 && rowIndex < relayUrls.size()) {
                    relayUrls.set(rowIndex, s.toString());
                }
            }
        });
        top.addView(edit, new LinearLayout.LayoutParams(0, dp(46), 1f));

        LinearLayout tools = new LinearLayout(this);
        tools.setOrientation(LinearLayout.HORIZONTAL);
        tools.setGravity(Gravity.RIGHT);
        tools.setPadding(0, dp(8), 0, 0);
        row.addView(tools, matchNoMargin());

        Button grip = compactButton("\u2261");
        grip.setOnLongClickListener(v -> startRelayDrag(v, row, rowIndex));
        tools.addView(grip, iconButtonParams());

        Button up = compactButton("\u2191");
        up.setEnabled(relayRowsEnabled && rowIndex > 0);
        up.setOnClickListener(v -> moveRelayUrl(rowIndex, rowIndex - 1));
        tools.addView(up, iconButtonParams());

        Button down = compactButton("\u2193");
        down.setEnabled(relayRowsEnabled && rowIndex < relayUrls.size() - 1);
        down.setOnClickListener(v -> moveRelayUrl(rowIndex, rowIndex + 1));
        tools.addView(down, iconButtonParams());

        Button delete = compactButton("\u00d7");
        delete.setOnClickListener(v -> {
            if (rowIndex >= 0 && rowIndex < relayUrls.size()) {
                relayUrls.remove(rowIndex);
                renderRelayRows();
            }
        });
        tools.addView(delete, iconButtonParams());

        return row;
    }

    private boolean startRelayDrag(View handle, View shadowSource, int index) {
        if (!relayRowsEnabled || index < 0 || index >= relayUrls.size()) {
            return false;
        }
        draggedRelayIndex = index;
        ClipData data = ClipData.newPlainText("DeskFerry relay URL", relayUrls.get(index));
        boolean started;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
            started = handle.startDragAndDrop(data, new View.DragShadowBuilder(shadowSource), null, 0);
        } else {
            started = handle.startDrag(data, new View.DragShadowBuilder(shadowSource), null, 0);
        }
        if (!started) {
            draggedRelayIndex = -1;
        }
        return started;
    }

    private void moveRelayUrl(int from, int to) {
        if (relayUrls.isEmpty()) {
            return;
        }
        if (from < 0 || from >= relayUrls.size()) {
            return;
        }
        if (to < 0) {
            to = 0;
        }
        if (to >= relayUrls.size()) {
            to = relayUrls.size() - 1;
        }
        if (from == to) {
            return;
        }
        String value = relayUrls.remove(from);
        if (to > relayUrls.size()) {
            to = relayUrls.size();
        }
        relayUrls.add(to, value);
        renderRelayRows();
    }

    private void setRelayRowsEnabled(boolean enabled) {
        relayRowsEnabled = enabled;
        if (relayUrlList != null) {
            setEnabledRecursive(relayUrlList, enabled);
        }
        if (relayUrlAddField != null) {
            relayUrlAddField.setEnabled(enabled);
        }
        if (relayAddButton != null) {
            relayAddButton.setEnabled(enabled);
        }
    }

    private void setEnabledRecursive(View view, boolean enabled) {
        view.setEnabled(enabled);
        if (view instanceof ViewGroup) {
            ViewGroup group = (ViewGroup) view;
            for (int i = 0; i < group.getChildCount(); i++) {
                setEnabledRecursive(group.getChildAt(i), enabled);
            }
        }
    }

    private void maybeRequestNotificationPermission() {
        if (Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, 701);
        }
    }

    private LinearLayout card() {
        LinearLayout view = new LinearLayout(this);
        view.setBackground(rounded("#FFFFFF", "#D7DEE3", 8));
        return view;
    }

    private EditText field(String hint) {
        EditText edit = new EditText(this);
        edit.setSingleLine(true);
        edit.setTextSize(15);
        edit.setHint(hint);
        edit.setPadding(dp(12), 0, dp(12), 0);
        edit.setMinHeight(dp(48));
        edit.setTextColor(color("#1F2933"));
        edit.setHintTextColor(color("#8A949E"));
        edit.setBackground(rounded("#FBFCFD", "#D7DEE3", 8));
        return edit;
    }

    private TextView sectionTitle(String text) {
        TextView view = label(text, 17, "#1F2933", true);
        view.setPadding(0, 0, 0, dp(10));
        return view;
    }

    private TextView label(String text, int sp, String color, boolean bold) {
        TextView view = new TextView(this);
        view.setText(text);
        view.setTextSize(sp);
        view.setTextColor(color(color));
        view.setIncludeFontPadding(true);
        if (bold) {
            view.setTypeface(Typeface.DEFAULT, Typeface.BOLD);
        }
        return view;
    }

    private Button primaryButton(String text) {
        Button button = button(text);
        button.setTextColor(Color.WHITE);
        button.setBackground(rounded("#2F6F73", "#2F6F73", 8));
        return button;
    }

    private Button secondaryButton(String text) {
        Button button = button(text);
        button.setTextColor(color("#2F6F73"));
        button.setBackground(rounded("#FFFFFF", "#9BC7C2", 8));
        return button;
    }

    private Button compactButton(String text) {
        Button button = secondaryButton(text);
        button.setTextSize(16);
        button.setMinWidth(0);
        button.setPadding(0, 0, 0, 0);
        return button;
    }

    private Button button(String text) {
        Button button = new Button(this);
        button.setAllCaps(false);
        button.setText(text);
        button.setTextSize(14);
        button.setGravity(Gravity.CENTER);
        button.setMinHeight(dp(44));
        return button;
    }

    private LinearLayout actionRow() {
        LinearLayout row = new LinearLayout(this);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setGravity(Gravity.CENTER);
        row.setPadding(0, 0, 0, dp(8));
        return row;
    }

    private LinearLayout.LayoutParams weightedButton() {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(0, dp(48), 1f);
        params.setMargins(dp(4), 0, dp(4), 0);
        return params;
    }

    private LinearLayout.LayoutParams weightedField() {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(0, dp(48), 1f);
        params.setMargins(0, 0, dp(8), 0);
        return params;
    }

    private LinearLayout.LayoutParams compactButtonParams() {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(dp(86), dp(48));
        params.setMargins(0, 0, 0, 0);
        return params;
    }

    private LinearLayout.LayoutParams iconButtonParams() {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(dp(44), dp(42));
        params.setMargins(dp(5), 0, 0, 0);
        return params;
    }

    private LinearLayout.LayoutParams roleParams() {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(dp(76), dp(42));
        params.setMargins(0, 0, dp(8), 0);
        return params;
    }

    private LinearLayout.LayoutParams cardParams() {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.WRAP_CONTENT);
        params.setMargins(0, dp(8), 0, dp(10));
        return params;
    }

    private LinearLayout.LayoutParams matchWrap() {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.WRAP_CONTENT);
        params.setMargins(0, 0, 0, dp(10));
        return params;
    }

    private LinearLayout.LayoutParams matchNoMargin() {
        return new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.WRAP_CONTENT);
    }

    private LinearLayout.LayoutParams relayRowParams() {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.WRAP_CONTENT);
        params.setMargins(0, 0, 0, dp(8));
        return params;
    }

    private GradientDrawable rounded(String fill, String stroke, int radiusDp) {
        GradientDrawable drawable = new GradientDrawable();
        drawable.setColor(color(fill));
        drawable.setCornerRadius(dp(radiusDp));
        drawable.setStroke(dp(1), color(stroke));
        return drawable;
    }

    private GradientDrawable relayRowBackground(boolean active) {
        return rounded(active ? "#EAF5F3" : "#FBFCFD", active ? "#2F6F73" : "#D7DEE3", 8);
    }

    private int color(String hex) {
        return Color.parseColor(hex);
    }

    private int dp(float value) {
        return (int) (value * getResources().getDisplayMetrics().density + 0.5f);
    }
}
