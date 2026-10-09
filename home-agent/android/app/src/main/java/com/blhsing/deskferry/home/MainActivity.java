package com.blhsing.deskferry.home;

import android.annotation.SuppressLint;
import android.Manifest;
import android.app.Activity;
import android.app.AlertDialog;
import android.content.BroadcastReceiver;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.PackageManager;
import android.graphics.Outline;
import android.graphics.Typeface;
import android.graphics.drawable.Drawable;
import android.graphics.drawable.GradientDrawable;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.text.Editable;
import android.text.InputType;
import android.text.TextUtils;
import android.text.TextWatcher;
import android.view.DragEvent;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.ViewOutlineProvider;
import android.widget.Button;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.ImageButton;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.Spinner;
import android.widget.TextView;
import android.widget.Toast;

import java.net.URISyntaxException;
import java.util.ArrayList;
import java.util.List;

public class MainActivity extends Activity {
    private static final String STATE_TAB = "selected_tab";
    private static final int TAB_HOME = 0;
    private static final int TAB_SETTINGS = 1;
    private static final int TAB_ACTIVITY = 2;
    private static final int RELAY_ROWS_WITHOUT_SCROLL = 3;

    private final BroadcastReceiver stateReceiver = new BroadcastReceiver() {
        @Override
        public void onReceive(Context context, Intent intent) {
            renderState(TunnelService.snapshot());
        }
    };

    private final ArrayList<String> relayUrls = new ArrayList<>();
    private final ArrayList<HomePrefs.Destination> destinations = new ArrayList<>();
    private Ui ui;
    private Spinner destinationSpinner;
    private ImageButton destinationAddButton;
    private ImageButton destinationRenameButton;
    private ImageButton destinationDeleteButton;
    private int selectedDestination;
    private boolean updatingDestinationSpinner;
    private ScrollView relayScroll;
    private LinearLayout relayUrlList;
    private EditText relayUrlAddField;
    private EditText roomNameField;
    private Button relayAddButton;
    private EditText localPortField;
    private EditText localSMBPortField;
    private EditText roomPasswordField;
    private ImageButton clearRoomPasswordButton;
    private EditText proxyField;
    private EditText logRetentionDaysField;
    private TextView settingsLockNote;
    private TextView overallChip;
    private TextView statusSentence;
    private TextView tunnelStatus;
    private TextView tunnelDetail;
    private TextView workStatus;
    private TextView workDetail;
    private TextView homeStatus;
    private TextView homeDetail;
    private TextView activeStatus;
    private TextView activeDetail;
    private TextView rdpAddress;
    private TextView smbAddress;
    private TextView messageView;
    private TextView logView;
    private Button startButton;
    private Boolean startButtonRunning;
    private final View[] tabPages = new View[3];
    private final LinearLayout[] tabButtons = new LinearLayout[3];
    private int selectedTab = TAB_HOME;
    private final ArrayList<View> relayUpButtons = new ArrayList<>();
    private final ArrayList<View> relayDownButtons = new ArrayList<>();
    private String latestRdpAddress = RelayUrls.rdpAddress(HomePrefs.DEFAULT_LOCAL_PORT);
    private String latestSMBAddress = RelayUrls.rdpAddress(HomePrefs.DEFAULT_LOCAL_SMB_PORT);
    private int draggedRelayIndex = -1;
    private boolean relayRowsEnabled = true;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        maybeRequestNotificationPermission();
        if (savedInstanceState != null) {
            selectedTab = savedInstanceState.getInt(STATE_TAB, TAB_HOME);
        }
        buildUi();
        loadPreferences();
        renderState(TunnelService.snapshot());
    }

    @Override
    protected void onSaveInstanceState(Bundle outState) {
        super.onSaveInstanceState(outState);
        outState.putInt(STATE_TAB, selectedTab);
    }

    @Override
    @SuppressLint("UnspecifiedRegisterReceiverFlag")
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

    // ---- Layout: three non-scrolling tab pages above a bottom tab bar ---------------------

    private void buildUi() {
        ui = Ui.forSystem(this);

        LinearLayout root = ui.vertical();
        root.setBackgroundColor(ui.bg);

        FrameLayout pages = new FrameLayout(this);
        root.addView(pages, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));
        tabPages[TAB_HOME] = buildHomePage();
        tabPages[TAB_SETTINGS] = buildSettingsPage();
        tabPages[TAB_ACTIVITY] = buildActivityPage();
        for (View page : tabPages) {
            pages.addView(page, new FrameLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        }

        LinearLayout tabBar = buildTabBar();
        root.addView(tabBar, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        selectTab(selectedTab);

        // The window decor (and its insets controller) exists only after setContentView.
        setContentView(root);
        // The page takes the status-bar inset; the tab bar extends under the navigation bar.
        // The soft keyboard pans the window (manifest adjustPan) so pages never need to scroll.
        ui.applySystemBars(this, root, tabBar, ui.bg, ui.surface, false);
    }

    private LinearLayout page() {
        LinearLayout page = ui.vertical();
        page.setPadding(dp(16), dp(8), dp(16), dp(8));
        return page;
    }

    private LinearLayout buildTabBar() {
        LinearLayout bar = ui.vertical();
        bar.setBackgroundColor(ui.surface);
        bar.addView(ui.divider(), new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, Math.max(1, dp(1))));
        LinearLayout tabs = ui.horizontal();
        tabs.setPadding(dp(8), dp(6), dp(8), dp(6));
        bar.addView(tabs, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, dp(64)));
        tabButtons[TAB_HOME] = tabButton(tabs, TAB_HOME, R.drawable.ic_df_home, "Home");
        tabButtons[TAB_SETTINGS] = tabButton(tabs, TAB_SETTINGS, R.drawable.ic_df_settings, "Settings");
        tabButtons[TAB_ACTIVITY] = tabButton(tabs, TAB_ACTIVITY, R.drawable.ic_df_activity, "Activity");
        return bar;
    }

    private LinearLayout tabButton(LinearLayout parent, int index, int iconRes, String label) {
        LinearLayout tab = ui.vertical();
        tab.setGravity(Gravity.CENTER);
        tab.setBackground(ui.rowBackground(12));
        tab.setClickable(true);
        tab.setFocusable(true);
        tab.setContentDescription(label + " tab");
        tab.setOnClickListener(v -> selectTab(index));

        ImageView icon = new ImageView(this);
        icon.setImageDrawable(getDrawable(iconRes).mutate());
        icon.setScaleType(ImageView.ScaleType.CENTER);
        tab.addView(icon, new LinearLayout.LayoutParams(dp(56), dp(28)));

        TextView text = ui.text(label, Ui.Type.CAPTION);
        text.setGravity(Gravity.CENTER);
        text.setPadding(0, dp(3), 0, 0);
        tab.addView(text, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        parent.addView(tab, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.MATCH_PARENT, 1f));
        return tab;
    }

    private void selectTab(int index) {
        if (index < TAB_HOME || index > TAB_ACTIVITY) {
            index = TAB_HOME;
        }
        selectedTab = index;
        View focused = getCurrentFocus();
        if (focused != null && tabPages[index] != null && !isDescendant(tabPages[index], focused)) {
            focused.clearFocus();
            android.view.inputmethod.InputMethodManager imm =
                    (android.view.inputmethod.InputMethodManager) getSystemService(INPUT_METHOD_SERVICE);
            if (imm != null) {
                imm.hideSoftInputFromWindow(focused.getWindowToken(), 0);
            }
        }
        for (int i = 0; i < tabPages.length; i++) {
            boolean active = i == index;
            tabPages[i].setVisibility(active ? View.VISIBLE : View.GONE);
            LinearLayout tab = tabButtons[i];
            if (tab == null) {
                continue;
            }
            tab.setSelected(active);
            ImageView icon = (ImageView) tab.getChildAt(0);
            TextView label = (TextView) tab.getChildAt(1);
            int color = active ? ui.primary : ui.textMuted;
            icon.getDrawable().setTint(color);
            icon.setBackground(active ? ui.pill(ui.primarySoft) : null);
            label.setTextColor(color);
        }
    }

    private static boolean isDescendant(View ancestor, View view) {
        for (Object v = view; v instanceof View; v = ((View) v).getParent()) {
            if (v == ancestor) {
                return true;
            }
        }
        return false;
    }

    // ---- Home tab ---------------------------------------------------------------------

    private View buildHomePage() {
        LinearLayout page = page();
        buildHeader(page);
        buildStatusTiles(page);
        buildConnectCard(page);
        buildQuickActions(page);
        return page;
    }

    private void buildHeader(LinearLayout root) {
        LinearLayout header = ui.horizontal();
        root.addView(header, ui.matchWrap(0));

        header.addView(appMark(), ui.fixed(40, 40, 0));

        LinearLayout titles = ui.vertical();
        TextView title = ui.text("DeskFerry Home", Ui.Type.DISPLAY);
        title.setTextSize(21);
        title.setSingleLine(true);
        title.setEllipsize(TextUtils.TruncateAt.END);
        titles.addView(title);
        TextView version = ui.text("Version " + BuildConfig.VERSION_NAME + " \u00b7 Home agent", Ui.Type.CAPTION);
        version.setSingleLine(true);
        version.setEllipsize(TextUtils.TruncateAt.END);
        titles.addView(version);
        header.addView(titles, ui.weighted(ViewGroup.LayoutParams.WRAP_CONTENT, 1f, 10));

        overallChip = ui.chip(Ui.Tone.NEUTRAL, "Stopped");
        header.addView(overallChip, ui.fixed(-1, -1, 8));

        statusSentence = ui.text("", Ui.Type.BODY);
        statusSentence.setTextColor(ui.textSecondary);
        statusSentence.setMaxLines(2);
        statusSentence.setEllipsize(TextUtils.TruncateAt.END);
        statusSentence.setPadding(0, dp(10), 0, dp(12));
        root.addView(statusSentence, ui.matchWrap(0));
    }

    /** The DeskFerry mark: the adaptive launcher icon's layers on a rounded tile. */
    private View appMark() {
        ImageView mark = new ImageView(this);
        GradientDrawable tile = ui.shape(Ui.rgb("#1F5C70"), 0, 10);
        mark.setBackground(tile);
        Drawable foreground = getDrawable(R.drawable.ic_launcher_foreground);
        mark.setImageDrawable(foreground);
        mark.setScaleType(ImageView.ScaleType.FIT_XY);
        mark.setOutlineProvider(new ViewOutlineProvider() {
            @Override
            public void getOutline(View view, Outline outline) {
                outline.setRoundRect(0, 0, view.getWidth(), view.getHeight(), dp(10));
            }
        });
        mark.setClipToOutline(true);
        mark.setContentDescription("DeskFerry");
        return mark;
    }

    private void buildStatusTiles(LinearLayout root) {
        LinearLayout row1 = ui.horizontal();
        row1.setGravity(Gravity.NO_GRAVITY);
        root.addView(row1, ui.matchWrap(10));
        LinearLayout row2 = ui.horizontal();
        row2.setGravity(Gravity.NO_GRAVITY);
        root.addView(row2, ui.matchWrap(12));

        TextView[] tunnel = addStatusTile(row1, "Tunnel", 0);
        tunnelStatus = tunnel[0];
        tunnelDetail = tunnel[1];
        TextView[] work = addStatusTile(row1, "Work agent", 10);
        workStatus = work[0];
        workDetail = work[1];
        TextView[] home = addStatusTile(row2, "Home presence", 0);
        homeStatus = home[0];
        homeDetail = home[1];
        TextView[] sessions = addStatusTile(row2, "Sessions", 10);
        activeStatus = sessions[0];
        activeDetail = sessions[1];
    }

    private TextView[] addStatusTile(LinearLayout row, String caption, int startMarginDp) {
        LinearLayout tile = ui.vertical();
        tile.setBackground(ui.shape(ui.surface, ui.border, 12));
        tile.setPadding(dp(14), dp(12), dp(12), dp(12));

        TextView title = ui.text(caption, Ui.Type.CAPTION);
        title.setSingleLine(true);
        title.setEllipsize(TextUtils.TruncateAt.END);
        tile.addView(title);

        TextView chip = ui.chip(Ui.Tone.NEUTRAL, "Unknown");
        LinearLayout.LayoutParams chipParams = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        chipParams.topMargin = dp(8);
        tile.addView(chip, chipParams);

        TextView detail = ui.text("", Ui.Type.CAPTION);
        detail.setTypeface(Typeface.create("sans-serif", Typeface.NORMAL));
        detail.setSingleLine(true);
        detail.setEllipsize(TextUtils.TruncateAt.END);
        detail.setPadding(0, dp(6), 0, 0);
        tile.addView(detail);

        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.MATCH_PARENT, 1f);
        params.setMarginStart(dp(startMarginDp));
        row.addView(tile, params);
        return new TextView[]{chip, detail};
    }

    private void buildConnectCard(LinearLayout root) {
        LinearLayout card = ui.card(null, null);
        card.setPadding(dp(16), dp(14), dp(16), dp(16));
        root.addView(card, ui.matchWrap(12));

        card.addView(ui.text("Remote Desktop address", Ui.Type.LABEL));
        LinearLayout addressRow = ui.horizontal();
        rdpAddress = ui.text(RelayUrls.rdpAddress(HomePrefs.DEFAULT_LOCAL_PORT), Ui.Type.METRIC);
        rdpAddress.setSingleLine(true);
        rdpAddress.setEllipsize(TextUtils.TruncateAt.END);
        rdpAddress.setTextIsSelectable(true);
        addressRow.addView(rdpAddress, ui.weighted(ViewGroup.LayoutParams.WRAP_CONTENT, 1f, 0));
        Button copy = ui.secondaryButton("Copy", R.drawable.ic_df_copy);
        copy.setContentDescription("Copy RDP target");
        copy.setOnClickListener(v -> copyRdpTarget());
        addressRow.addView(copy, ui.fixed(-1, 40, 8));
        LinearLayout.LayoutParams addressParams = ui.matchWrap(0);
        addressParams.topMargin = dp(2);
        card.addView(addressRow, addressParams);

        smbAddress = ui.text("SMB " + RelayUrls.rdpAddress(HomePrefs.DEFAULT_LOCAL_SMB_PORT), Ui.Type.CAPTION);
        smbAddress.setTypeface(Typeface.create("sans-serif", Typeface.NORMAL));
        smbAddress.setSingleLine(true);
        smbAddress.setEllipsize(TextUtils.TruncateAt.END);
        card.addView(smbAddress, ui.matchWrap(0));

        startButton = ui.primaryButton("Start tunnel", R.drawable.ic_df_play);
        startButton.setTextSize(15);
        startButton.setOnClickListener(v -> toggleTunnel());
        LinearLayout.LayoutParams startParams = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, dp(50));
        startParams.topMargin = dp(12);
        card.addView(startButton, startParams);
    }

    private void buildQuickActions(LinearLayout root) {
        LinearLayout row1 = ui.horizontal();
        root.addView(row1, ui.matchWrap(10));
        LinearLayout row2 = ui.horizontal();
        root.addView(row2, ui.matchWrap(0));
        quickAction(row1, R.drawable.ic_df_desktop, "Open RDP app", 0, v -> openRdpApp());
        quickAction(row1, R.drawable.ic_df_screen, "Screen viewer", 10, v -> openScreenViewer());
        quickAction(row2, R.drawable.ic_df_dashboard, "Dashboard", 0, v -> openDashboard());
        quickAction(row2, R.drawable.ic_df_folder, "Copy SMB target", 10, v -> copySMBTarget());
    }

    private void quickAction(LinearLayout row, int iconRes, String label, int startMarginDp, View.OnClickListener listener) {
        Button button = ui.secondaryButton(label, iconRes);
        button.setTextSize(13);
        button.setPadding(dp(10), 0, dp(10), 0);
        button.setCompoundDrawablePadding(dp(6));
        button.setOnClickListener(listener);
        row.addView(button, ui.weighted(dp(48), 1f, startMarginDp));
    }

    private Drawable tinted(int res, int color) {
        Drawable drawable = getDrawable(res);
        if (drawable == null) {
            return null;
        }
        drawable = drawable.mutate();
        drawable.setTint(color);
        return drawable;
    }

    // ---- Settings tab -----------------------------------------------------------------

    private View buildSettingsPage() {
        LinearLayout page = page();
        buildDestinationCard(page);
        buildRelayCard(page);
        buildConnectionCard(page);
        return page;
    }

    private LinearLayout compactCard() {
        LinearLayout card = ui.card(null, null);
        card.setPadding(dp(14), dp(12), dp(14), dp(12));
        return card;
    }

    /** Label above a 44dp control, no helper text, so Settings fits without scrolling. */
    private LinearLayout compactField(String label, View control) {
        LinearLayout group = ui.vertical();
        TextView title = ui.text(label, Ui.Type.LABEL);
        title.setSingleLine(true);
        title.setEllipsize(TextUtils.TruncateAt.END);
        title.setPadding(0, 0, 0, dp(4));
        group.addView(title);
        group.addView(control, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, dp(44)));
        return group;
    }

    private LinearLayout twoColumns(View left, View right) {
        LinearLayout row = ui.horizontal();
        row.setGravity(Gravity.TOP);
        row.addView(left, ui.weighted(ViewGroup.LayoutParams.WRAP_CONTENT, 1f, 0));
        row.addView(right, ui.weighted(ViewGroup.LayoutParams.WRAP_CONTENT, 1f, 12));
        return row;
    }

    private void buildDestinationCard(LinearLayout root) {
        LinearLayout card = compactCard();
        root.addView(card, ui.matchWrap(8));

        LinearLayout titleRow = ui.horizontal();
        titleRow.addView(ui.text("Destination", Ui.Type.TITLE), ui.weighted(ViewGroup.LayoutParams.WRAP_CONTENT, 1f, 0));
        settingsLockNote = ui.chip(Ui.Tone.WARNING, "Stop the tunnel to edit");
        settingsLockNote.setVisibility(View.GONE);
        titleRow.addView(settingsLockNote, ui.fixed(-1, -1, 8));
        card.addView(titleRow, ui.matchWrap(6));

        LinearLayout destinationRow = ui.horizontal();
        destinationSpinner = ui.spinner();
        destinationSpinner.setContentDescription("Destination profile");
        destinationSpinner.setOnItemSelectedListener(new android.widget.AdapterView.OnItemSelectedListener() {
            @Override
            public void onItemSelected(android.widget.AdapterView<?> parent, View view, int position, long id) {
                if (!updatingDestinationSpinner) {
                    selectDestination(position);
                }
            }

            @Override
            public void onNothingSelected(android.widget.AdapterView<?> parent) {
            }
        });
        destinationRow.addView(destinationSpinner, ui.weighted(dp(44), 1f, 0));
        destinationAddButton = ui.iconButton(R.drawable.ic_df_add, "Add destination", true);
        destinationAddButton.setOnClickListener(v -> promptAddDestination());
        destinationRow.addView(destinationAddButton, ui.fixed(44, 44, 8));
        destinationRenameButton = ui.iconButton(R.drawable.ic_df_edit, "Rename destination", true);
        destinationRenameButton.setOnClickListener(v -> promptRenameDestination());
        destinationRow.addView(destinationRenameButton, ui.fixed(44, 44, 8));
        destinationDeleteButton = ui.iconButton(R.drawable.ic_df_delete, "Delete destination", true);
        destinationDeleteButton.setOnClickListener(v -> confirmDeleteDestination());
        destinationRow.addView(destinationDeleteButton, ui.fixed(44, 44, 8));
        card.addView(destinationRow, ui.matchWrap(10));

        roomNameField = ui.field(RelayUrls.DEFAULT_ROOM);

        LinearLayout passwordRow = ui.horizontal();
        roomPasswordField = ui.field("Unchanged");
        roomPasswordField.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_PASSWORD);
        // Password input types switch the typeface to monospace; keep the hint in the UI font.
        roomPasswordField.setTypeface(Typeface.create("sans-serif", Typeface.NORMAL));
        passwordRow.addView(roomPasswordField, ui.weighted(dp(44), 1f, 0));
        clearRoomPasswordButton = ui.iconButton(R.drawable.ic_df_close, "Clear saved room credential", true);
        clearRoomPasswordButton.setOnClickListener(v -> {
            if (!destinations.isEmpty()) {
                destinations.get(selectedDestination).roomProof = "";
                roomPasswordField.setText("");
                HomePrefs.saveDestinations(this, destinations, selectedDestination,
                        parsePortOrDefault(localPortField.getText().toString()));
                Toast.makeText(this, "Saved room credential cleared.", Toast.LENGTH_SHORT).show();
            }
        });
        passwordRow.addView(clearRoomPasswordButton, ui.fixed(40, 44, 6));
        card.addView(twoColumns(compactField("Room", roomNameField),
                compactField("Room password", passwordRow)), ui.matchWrap(0));
    }

    private void buildRelayCard(LinearLayout root) {
        LinearLayout card = compactCard();
        root.addView(card, ui.matchWrap(8));

        LinearLayout titleRow = ui.horizontal();
        titleRow.addView(ui.text("Relay services", Ui.Type.TITLE));
        TextView hint = ui.text("Tried in order \u00b7 hold \u2261 to drag", Ui.Type.CAPTION);
        hint.setTypeface(Typeface.create("sans-serif", Typeface.NORMAL));
        hint.setSingleLine(true);
        hint.setEllipsize(TextUtils.TruncateAt.END);
        hint.setGravity(Gravity.END);
        titleRow.addView(hint, ui.weighted(ViewGroup.LayoutParams.WRAP_CONTENT, 1f, 8));
        card.addView(titleRow, ui.matchWrap(6));

        relayUrlList = ui.vertical();
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
        relayScroll = new ScrollView(this);
        relayScroll.setVerticalScrollBarEnabled(true);
        relayScroll.addView(relayUrlList, new ScrollView.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        card.addView(relayScroll, ui.matchWrap(2));

        LinearLayout addRelayRow = ui.horizontal();
        relayUrlAddField = ui.field("Add relay base URL");
        relayUrlAddField.setContentDescription("New relay base URL");
        relayUrlAddField.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_URI);
        addRelayRow.addView(relayUrlAddField, ui.weighted(dp(44), 1f, 0));
        relayAddButton = ui.secondaryButton("Add", R.drawable.ic_df_add);
        relayAddButton.setContentDescription("Add relay service");
        relayAddButton.setOnClickListener(v -> addRelayUrlFromField());
        addRelayRow.addView(relayAddButton, ui.fixed(-1, 44, 8));
        card.addView(addRelayRow, ui.matchWrap(0));
    }

    private void buildConnectionCard(LinearLayout root) {
        LinearLayout card = compactCard();
        root.addView(card, ui.matchWrap(0));
        card.addView(ui.text("Connection", Ui.Type.TITLE), ui.matchWrap(6));

        localPortField = ui.field(String.valueOf(HomePrefs.DEFAULT_LOCAL_PORT));
        localPortField.setInputType(InputType.TYPE_CLASS_NUMBER);
        localSMBPortField = ui.field(String.valueOf(HomePrefs.DEFAULT_LOCAL_SMB_PORT));
        localSMBPortField.setInputType(InputType.TYPE_CLASS_NUMBER);
        card.addView(twoColumns(compactField("Local RDP port", localPortField),
                compactField("Local SMB port", localSMBPortField)), ui.matchWrap(10));

        proxyField = ui.field("system, direct, or URL");
        proxyField.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_URI);
        logRetentionDaysField = ui.field(String.valueOf(HomePrefs.DEFAULT_LOG_RETENTION_DAYS));
        logRetentionDaysField.setInputType(InputType.TYPE_CLASS_NUMBER);
        card.addView(twoColumns(compactField("Proxy", proxyField),
                compactField("Log retention (days)", logRetentionDaysField)), ui.matchWrap(0));
    }

    // ---- Activity tab -----------------------------------------------------------------

    private View buildActivityPage() {
        LinearLayout page = page();
        LinearLayout card = ui.card("Activity", "Latest status and the diagnostic log, newest first.");
        page.addView(card, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));

        messageView = ui.text("Ready.", Ui.Type.BODY_STRONG);
        messageView.setMaxLines(3);
        messageView.setEllipsize(TextUtils.TruncateAt.END);
        messageView.setPadding(0, 0, 0, dp(12));
        card.addView(messageView, ui.matchWrap(0));

        ScrollView logScroll = new ScrollView(this);
        logScroll.setBackground(ui.shape(ui.surfaceSubtle, ui.border, 8));
        logScroll.setPadding(dp(12), dp(10), dp(12), dp(10));
        logScroll.setClipToPadding(false);
        logView = ui.text("", Ui.Type.MONO);
        logView.setTextIsSelectable(true);
        logScroll.addView(logView, new ScrollView.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        card.addView(logScroll, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));
        return page;
    }

    private void loadPreferences() {
        destinations.clear();
        destinations.addAll(HomePrefs.loadDestinations(this));
        selectedDestination = HomePrefs.loadSelectedDestination(this, destinations.size());
        refreshDestinationSpinner();
        List<String> relayUrls;
        try {
            relayUrls = RelayUrls.normalizeRelayBaseUrls(destinations.get(selectedDestination).relayBases);
        } catch (URISyntaxException ex) {
            relayUrls = RelayUrls.DEFAULT_RELAY_BASE_URLS;
        }
        setRelayUrls(relayUrls);
        roomNameField.setText(destinations.get(selectedDestination).room);
        roomPasswordField.setText("");
        localPortField.setText(String.valueOf(HomePrefs.loadLocalPort(this)));
		localSMBPortField.setText(String.valueOf(HomePrefs.loadLocalSMBPort(this)));
        proxyField.setText(HomePrefs.loadProxy(this));
        logRetentionDaysField.setText(String.valueOf(HomePrefs.loadLogRetentionDays(this)));
    }

    private void savePreferences(String relayUrl, int port, String proxy) {
        if (!destinations.isEmpty()) {
            HomePrefs.Destination destination = destinations.get(selectedDestination);
            String room = roomNameField.getText().toString().trim();
            if (!destination.room.equalsIgnoreCase(room)) destination.roomProof = "";
            destination.relayBases = RelayUrls.joinRelayUrls(relayUrls);
            destination.room = room;
            String password = roomPasswordField.getText().toString();
            if (!password.isEmpty()) {
                destinations.get(selectedDestination).roomProof = RelayUrls.roomPasswordProof(
                        RelayUrls.primaryRelayUrl(relayUrl), password);
                roomPasswordField.setText("");
            }
        }
		HomePrefs.saveDestinations(this, destinations, selectedDestination, port, proxy,
				parseLogRetentionDays(logRetentionDaysField.getText().toString()),
				parseSMBPort(localSMBPortField.getText().toString()));
    }

    private void refreshDestinationSpinner() {
        ArrayList<String> names = new ArrayList<>();
        for (HomePrefs.Destination destination : destinations) {
            names.add(destination.name);
        }
        updatingDestinationSpinner = true;
        destinationSpinner.setAdapter(ui.spinnerAdapter(names));
        if (!names.isEmpty()) {
            selectedDestination = Math.max(0, Math.min(selectedDestination, names.size() - 1));
            destinationSpinner.setSelection(selectedDestination);
        }
        updatingDestinationSpinner = false;
        destinationDeleteButton.setEnabled(relayRowsEnabled && destinations.size() > 1);
    }

    private void commitSelectedDestination() {
        if (!destinations.isEmpty() && selectedDestination >= 0 && selectedDestination < destinations.size()) {
            HomePrefs.Destination destination = destinations.get(selectedDestination);
            String room = roomNameField.getText().toString().trim();
            if (!destination.room.equalsIgnoreCase(room)) destination.roomProof = "";
            destination.relayBases = RelayUrls.joinRelayUrls(relayUrls);
            destination.room = room;
        }
    }

    private void selectDestination(int index) {
        if (index < 0 || index >= destinations.size() || index == selectedDestination) {
            return;
        }
        commitSelectedDestination();
        selectedDestination = index;
        roomPasswordField.setText("");
        roomNameField.setText(destinations.get(index).room);
        try {
            setRelayUrls(RelayUrls.normalizeRelayBaseUrls(destinations.get(index).relayBases));
        } catch (URISyntaxException ex) {
            setRelayUrls(RelayUrls.DEFAULT_RELAY_BASE_URLS);
        }
        HomePrefs.saveDestinations(this, destinations, selectedDestination,
                parsePortOrDefault(localPortField.getText().toString()));
    }

    private void promptAddDestination() {
        promptDestinationName("Add destination", "", name -> {
            commitSelectedDestination();
            String unique = uniqueDestinationName(name, -1);
            destinations.add(new HomePrefs.Destination(unique, RelayUrls.joinRelayUrls(RelayUrls.DEFAULT_RELAY_BASE_URLS), RelayUrls.DEFAULT_ROOM, ""));
            selectedDestination = destinations.size() - 1;
            roomPasswordField.setText("");
            roomNameField.setText(RelayUrls.DEFAULT_ROOM);
            setRelayUrls(RelayUrls.DEFAULT_RELAY_BASE_URLS);
            refreshDestinationSpinner();
            HomePrefs.saveDestinations(this, destinations, selectedDestination,
                    parsePortOrDefault(localPortField.getText().toString()));
        });
    }

    private void promptRenameDestination() {
        if (destinations.isEmpty()) {
            return;
        }
        promptDestinationName("Rename destination", destinations.get(selectedDestination).name, name -> {
            destinations.get(selectedDestination).name = uniqueDestinationName(name, selectedDestination);
            refreshDestinationSpinner();
            HomePrefs.saveDestinations(this, destinations, selectedDestination,
                    parsePortOrDefault(localPortField.getText().toString()));
        });
    }

    private interface NameHandler {
        void accept(String name);
    }

    private void promptDestinationName(String title, String initial, NameHandler handler) {
        EditText input = ui.field("Destination name");
        input.setText(initial);
        input.setSelection(input.getText().length());
        FrameLayout frame = new FrameLayout(this);
        frame.setPadding(dp(24), dp(8), dp(24), 0);
        frame.addView(input, new FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, dp(48)));
        new AlertDialog.Builder(this, dialogTheme())
                .setTitle(title)
                .setView(frame)
                .setPositiveButton("Save", (dialog, which) -> {
                    String name = input.getText().toString().trim();
                    if (name.isEmpty()) {
                        Toast.makeText(this, "Destination name is required.", Toast.LENGTH_LONG).show();
                        return;
                    }
                    handler.accept(name);
                })
                .setNegativeButton("Cancel", null)
                .show();
    }

    private int dialogTheme() {
        return ui.dark ? android.R.style.Theme_Material_Dialog_Alert : android.R.style.Theme_Material_Light_Dialog_Alert;
    }

    private void confirmDeleteDestination() {
        if (destinations.size() <= 1) {
            Toast.makeText(this, "Keep at least one destination.", Toast.LENGTH_SHORT).show();
            return;
        }
        AlertDialog dialog = new AlertDialog.Builder(this, dialogTheme())
                .setTitle("Delete destination?")
                .setMessage("\u201c" + destinations.get(selectedDestination).name
                        + "\u201d and its saved room credential will be removed from this phone.")
                .setPositiveButton("Delete", (d, which) -> deleteDestination())
                .setNegativeButton("Cancel", null)
                .create();
        dialog.setOnShowListener(d -> dialog.getButton(AlertDialog.BUTTON_POSITIVE)
                .setTextColor(ui.toneForeground(Ui.Tone.DANGER)));
        dialog.show();
    }

    private void deleteDestination() {
        if (destinations.size() <= 1) {
            Toast.makeText(this, "Keep at least one destination.", Toast.LENGTH_SHORT).show();
            return;
        }
        destinations.remove(selectedDestination);
        selectedDestination = Math.min(selectedDestination, destinations.size() - 1);
        roomPasswordField.setText("");
        roomNameField.setText(destinations.get(selectedDestination).room);
        try {
            setRelayUrls(RelayUrls.normalizeRelayBaseUrls(destinations.get(selectedDestination).relayBases));
        } catch (URISyntaxException ex) {
            setRelayUrls(RelayUrls.DEFAULT_RELAY_BASE_URLS);
        }
        refreshDestinationSpinner();
        HomePrefs.saveDestinations(this, destinations, selectedDestination,
                parsePortOrDefault(localPortField.getText().toString()));
    }

    private String uniqueDestinationName(String requested, int ignoredIndex) {
        String base = requested.trim();
        String candidate = base;
        int suffix = 2;
        while (destinationNameExists(candidate, ignoredIndex)) {
            candidate = base + " " + suffix++;
        }
        return candidate;
    }

    private boolean destinationNameExists(String name, int ignoredIndex) {
        for (int i = 0; i < destinations.size(); i++) {
            if (i != ignoredIndex && destinations.get(i).name.equalsIgnoreCase(name)) {
                return true;
            }
        }
        return false;
    }

    private int parsePortOrDefault(String value) {
        try {
            return parsePort(value);
        } catch (Exception ignored) {
            return HomePrefs.DEFAULT_LOCAL_PORT;
        }
    }

    private void toggleTunnel() {
        TunnelService.State state = TunnelService.snapshot();
        if (state.running) {
            stopService(new Intent(this, TunnelService.class).setAction(TunnelService.ACTION_STOP));
            return;
        }
        String relayUrl;
        List<String> normalizedRelayBases;
        int port;
        String proxy;
        int logRetentionDays;
		int smbPort;
        try {
            normalizedRelayBases = normalizedRelayUrlsFromRows();
            relayUrl = RelayUrls.joinRelayUrls(RelayUrls.relayRoomUrls(normalizedRelayBases, roomNameField.getText().toString()));
            port = parsePort(localPortField.getText().toString());
            proxy = ProxySettings.normalize(proxyField.getText().toString());
            logRetentionDays = parseLogRetentionDays(logRetentionDaysField.getText().toString());
			smbPort = parseSMBPort(localSMBPortField.getText().toString());
        } catch (Exception ex) {
            Toast.makeText(this, ex.getMessage(), Toast.LENGTH_LONG).show();
            return;
        }
        setRelayUrls(normalizedRelayBases);
        localPortField.setText(String.valueOf(port));
        proxyField.setText(proxy);
        logRetentionDaysField.setText(String.valueOf(logRetentionDays));
		localSMBPortField.setText(String.valueOf(smbPort));
        savePreferences(relayUrl, port, proxy);
        String roomProof = destinations.isEmpty() ? "" : destinations.get(selectedDestination).roomProof;

        Intent intent = new Intent(this, TunnelService.class)
                .setAction(TunnelService.ACTION_START)
                .putExtra(TunnelService.EXTRA_RELAY_URL, relayUrl)
                .putExtra(TunnelService.EXTRA_LOCAL_PORT, port)
				.putExtra(TunnelService.EXTRA_LOCAL_SMB_PORT, smbPort)
                .putExtra(TunnelService.EXTRA_PROXY, proxy)
                .putExtra(TunnelService.EXTRA_ROOM_PROOF, roomProof)
                .putExtra(TunnelService.EXTRA_LOG_RETENTION_DAYS, logRetentionDays);
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

    private int parseLogRetentionDays(String value) {
        int days = Integer.parseInt(value.trim());
        if (days < 1 || days > 3650) {
            throw new IllegalArgumentException("Log retention days must be 1-3650.");
        }
        return days;
    }

	private int parseSMBPort(String value) {
		int port = Integer.parseInt(value.trim());
		if (port < 1024 || port > 65535) {
			throw new IllegalArgumentException("Local SMB port must be 1024-65535 so it works without root.");
		}
		if (port == parsePortOrDefault(localPortField.getText().toString())) {
			throw new IllegalArgumentException("Local SMB and RDP ports must be different.");
		}
		return port;
	}


    private void renderState(TunnelService.State state) {
        latestRdpAddress = state.rdpAddress;
        latestSMBAddress = state.smbAddress;
        renderStatus(state);
        rdpAddress.setText(state.rdpAddress);
        smbAddress.setText("SMB " + state.smbAddress + (state.smbEnabled ? "" : " (save a room password to enable)"));
        messageView.setText(state.lastMessage);
        boolean hasLog = state.log != null && !state.log.trim().isEmpty();
        logView.setText(hasLog ? state.log : "Diagnostic messages appear here once the tunnel starts.");
        logView.setTextColor(hasLog ? ui.textSecondary : ui.textMuted);
        logView.setTypeface(hasLog ? Typeface.MONOSPACE : Typeface.create("sans-serif", Typeface.NORMAL));
        renderStartButton(state.running);
        setRelayRowsEnabled(!state.running);
        destinationSpinner.setEnabled(!state.running);
        destinationAddButton.setEnabled(!state.running);
        destinationRenameButton.setEnabled(!state.running);
        destinationDeleteButton.setEnabled(!state.running && destinations.size() > 1);
        localPortField.setEnabled(!state.running);
        localSMBPortField.setEnabled(!state.running);
        proxyField.setEnabled(!state.running);
        logRetentionDaysField.setEnabled(!state.running);
        roomPasswordField.setEnabled(!state.running);
        roomNameField.setEnabled(!state.running);
        clearRoomPasswordButton.setEnabled(!state.running);
        settingsLockNote.setVisibility(state.running ? View.VISIBLE : View.GONE);
    }

    private void renderStartButton(boolean running) {
        if (startButtonRunning != null && startButtonRunning == running) {
            return;
        }
        startButtonRunning = running;
        startButton.setText(running ? "Stop tunnel" : "Start tunnel");
        if (running) {
            ui.styleSecondary(startButton);
        } else {
            ui.stylePrimary(startButton);
        }
        ui.setButtonIcon(startButton, running ? R.drawable.ic_df_stop : R.drawable.ic_df_play);
    }

    /** Maps TunnelService status strings onto the design-system status vocabulary. */
    private void renderStatus(TunnelService.State state) {
        String tunnel = emptyAs(state.tunnelStatus, state.running ? "Running" : "Stopped");
        switch (tunnel) {
            case "Running":
                ui.setChip(tunnelStatus, Ui.Tone.SUCCESS, "Running");
                tunnelDetail.setText("Listening on this phone");
                break;
            case "Stopped":
                ui.setChip(tunnelStatus, Ui.Tone.NEUTRAL, "Stopped");
                tunnelDetail.setText("Not listening");
                break;
            case "Error":
                ui.setChip(tunnelStatus, Ui.Tone.DANGER, "Error");
                tunnelDetail.setText("See Activity below");
                break;
            default:
                ui.setChip(tunnelStatus, Ui.Tone.WARNING, tunnel);
                tunnelDetail.setText(state.running ? "Listening on this phone" : "Not listening");
                break;
        }

        String work = emptyAs(state.workStatus, "Unknown");
        switch (work) {
            case "Connected":
                ui.setChip(workStatus, Ui.Tone.SUCCESS, "Online");
                workDetail.setText("Ready for connections");
                break;
            case "Waiting":
                ui.setChip(workStatus, Ui.Tone.DANGER, "Offline");
                workDetail.setText("Not on the relay");
                break;
            case "Checking":
                ui.setChip(workStatus, Ui.Tone.WARNING, "Checking");
                workDetail.setText("Asking the relay");
                break;
            case "Check relay":
                ui.setChip(workStatus, Ui.Tone.DANGER, "Unreachable");
                workDetail.setText("Relay status unavailable");
                break;
            case "Unknown":
                ui.setChip(workStatus, Ui.Tone.NEUTRAL, "Unknown");
                workDetail.setText(state.running ? "Waiting for relay status" : "Start the tunnel to check");
                break;
            default:
                ui.setChip(workStatus, Ui.Tone.WARNING, work);
                workDetail.setText("Asking the relay");
                break;
        }

        String home = emptyAs(state.homeStatus, "Offline");
        switch (home) {
            case "Online":
                ui.setChip(homeStatus, Ui.Tone.SUCCESS, "Online");
                homeDetail.setText("Visible on the dashboard");
                break;
            case "Connecting":
                ui.setChip(homeStatus, Ui.Tone.WARNING, "Connecting");
                homeDetail.setText("Joining the relay");
                break;
            case "Reconnecting":
                ui.setChip(homeStatus, Ui.Tone.WARNING, "Reconnecting");
                homeDetail.setText("Retrying the relay");
                break;
            case "Offline":
                ui.setChip(homeStatus, state.running ? Ui.Tone.DANGER : Ui.Tone.NEUTRAL, "Offline");
                homeDetail.setText("Not announced");
                break;
            default:
                ui.setChip(homeStatus, Ui.Tone.WARNING, home);
                homeDetail.setText("Joining the relay");
                break;
        }

        int active = state.activeConnections;
        if (active > 0) {
            ui.setChip(activeStatus, Ui.Tone.SUCCESS, active + " active");
        } else {
            ui.setChip(activeStatus, Ui.Tone.NEUTRAL, "None active");
        }
        int total = state.totalConnections;
        activeDetail.setText(total == 1 ? "1 session so far" : total + " sessions so far");

        if (!state.running) {
            if ("Check relay".equals(work)) {
                ui.setChip(overallChip, Ui.Tone.DANGER, "Error");
                statusSentence.setText("The tunnel could not start. See Activity for details.");
            } else {
                ui.setChip(overallChip, Ui.Tone.NEUTRAL, "Stopped");
                statusSentence.setText("The tunnel is stopped. Start it to reach your Work PC.");
            }
        } else if (active > 0) {
            ui.setChip(overallChip, Ui.Tone.SUCCESS, "Connected");
            statusSentence.setText(active == 1
                    ? "Connected. 1 active session through the relay."
                    : "Connected. " + active + " active sessions through the relay.");
        } else if ("Connected".equals(work)) {
            ui.setChip(overallChip, Ui.Tone.SUCCESS, "Connected");
            statusSentence.setText("Ready. Point your Remote Desktop app at " + state.rdpAddress + ".");
        } else if ("Waiting".equals(work)) {
            ui.setChip(overallChip, Ui.Tone.DANGER, "Offline");
            statusSentence.setText("The tunnel is running, but the Work agent is offline.");
        } else if ("Check relay".equals(work)) {
            ui.setChip(overallChip, Ui.Tone.DANGER, "Unreachable");
            statusSentence.setText("Can't reach the relay. Check the relay services and proxy.");
        } else {
            ui.setChip(overallChip, Ui.Tone.WARNING, "Checking");
            statusSentence.setText("Checking the Work agent through the relay.");
        }
    }

    private static String emptyAs(String value, String fallback) {
        return value == null || value.trim().isEmpty() ? fallback : value.trim();
    }

    private void copyRdpTarget() {
        ClipboardManager clipboard = (ClipboardManager) getSystemService(CLIPBOARD_SERVICE);
        clipboard.setPrimaryClip(ClipData.newPlainText("DeskFerry RDP target", latestRdpAddress));
        Toast.makeText(this, "Copied " + latestRdpAddress, Toast.LENGTH_SHORT).show();
    }

	private void copySMBTarget() {
		ClipboardManager clipboard = (ClipboardManager) getSystemService(CLIPBOARD_SERVICE);
		clipboard.setPrimaryClip(ClipData.newPlainText("DeskFerry SMB target", latestSMBAddress));
		Toast.makeText(this, "Copied " + latestSMBAddress, Toast.LENGTH_SHORT).show();
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
        String relayUrl = destinations.isEmpty() ? RelayUrls.DEFAULT_RELAY_URL : destinations.get(selectedDestination).relayUrl();
        try {
            relayUrl = RelayUrls.normalizeRelayUrl(relayUrl);
        } catch (URISyntaxException ignored) {
        }
        startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse(RelayUrls.dashboardUrl(relayUrl))));
    }

	private void openScreenViewer() {
		try {
			List<String> normalizedRelayBases = normalizedRelayUrlsFromRows();
			String relayUrl = RelayUrls.joinRelayUrls(RelayUrls.relayRoomUrls(
					normalizedRelayBases, roomNameField.getText().toString()));
			String proxy = ProxySettings.normalize(proxyField.getText().toString());
			int port = parsePort(localPortField.getText().toString());
			setRelayUrls(normalizedRelayBases);
			proxyField.setText(proxy);
			savePreferences(relayUrl, port, proxy);
			String proof = destinations.get(selectedDestination).roomProof;
			if (proof == null || proof.isEmpty()) {
				throw new IllegalArgumentException("Save a room password for this destination before viewing its screen.");
			}
			Intent intent = new Intent(this, ScreenViewerActivity.class)
					.putExtra(ScreenViewerActivity.EXTRA_RELAY_URLS, relayUrl)
					.putExtra(ScreenViewerActivity.EXTRA_PROXY, proxy)
					.putExtra(ScreenViewerActivity.EXTRA_ROOM_PROOF, proof)
					.putExtra(ScreenViewerActivity.EXTRA_DESTINATION, destinations.get(selectedDestination).name);
			startActivity(intent);
		} catch (Exception ex) {
			Toast.makeText(this, ex.getMessage(), Toast.LENGTH_LONG).show();
		}
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
        List<String> normalized = RelayUrls.normalizeRelayBaseUrls(RelayUrls.joinRelayUrls(relayUrls));
        setRelayUrls(normalized);
        return normalized;
    }

    private void addRelayUrlFromField() {
        try {
            String relayUrl = RelayUrls.normalizeRelayBaseUrl(relayUrlAddField.getText().toString());
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
        relayUpButtons.clear();
        relayDownButtons.clear();
        if (relayUrls.isEmpty()) {
            TextView empty = ui.text("No relay services yet. Add one below; the first is tried first.", Ui.Type.CAPTION);
            empty.setTypeface(Typeface.create("sans-serif", Typeface.NORMAL));
            empty.setTextSize(13);
            empty.setPadding(0, dp(4), 0, dp(10));
            relayUrlList.addView(empty, ui.matchWrap(0));
        }
        for (int i = 0; i < relayUrls.size(); i++) {
            relayUrlList.addView(relayUrlRow(i), ui.matchWrap(4));
        }
        if (relayScroll != null) {
            // Up to three rows show in full; longer lists scroll inside a fixed box (2.7 rows tall
            // so the cut-off row hints at scrolling) and the page itself never scrolls.
            ViewGroup.LayoutParams params = relayScroll.getLayoutParams();
            int height = relayUrls.size() > RELAY_ROWS_WITHOUT_SCROLL
                    ? dp(2 * 44 + 30)
                    : ViewGroup.LayoutParams.WRAP_CONTENT;
            if (params != null && params.height != height) {
                params.height = height;
                relayScroll.setLayoutParams(params);
            }
        }
        setRelayRowsEnabled(relayRowsEnabled);
    }

    /** One compact line per relay: drag handle, role badge, URL field, and up/down/remove. */
    private View relayUrlRow(int index) {
        final int rowIndex = index;
        final LinearLayout row = ui.horizontal();
        row.setPadding(0, 0, dp(2), 0);
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

        ImageButton grip = ui.iconButton(R.drawable.ic_df_drag, "Drag to reorder (long-press)", false);
        grip.setOnLongClickListener(v -> startRelayDrag(v, row, rowIndex));
        row.addView(grip, ui.fixed(30, 40, 0));

        TextView role = rowIndex == 0
                ? ui.badge(Ui.Tone.INFO, "Primary")
                : ui.badge(Ui.Tone.NEUTRAL, "Fallback");
        role.setTextSize(11);
        role.setPadding(dp(6), dp(2), dp(6), dp(2));
        row.addView(role, ui.fixed(-1, -1, 0));

        EditText edit = ui.field("Relay service base URL");
        edit.setText(relayUrls.get(rowIndex));
        edit.setTextSize(13);
        edit.setPadding(dp(8), 0, dp(8), 0);
        edit.setContentDescription(rowIndex == 0 ? "Primary relay URL" : "Fallback relay URL " + rowIndex);
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
        row.addView(edit, ui.weighted(dp(32), 1f, 6));

        ImageButton up = ui.iconButton(R.drawable.ic_df_up, "Move up", false);
        up.setEnabled(relayRowsEnabled && rowIndex > 0);
        up.setOnClickListener(v -> moveRelayUrl(rowIndex, rowIndex - 1));
        row.addView(up, ui.fixed(32, 40, 2));
        relayUpButtons.add(up);

        ImageButton down = ui.iconButton(R.drawable.ic_df_down, "Move down", false);
        down.setEnabled(relayRowsEnabled && rowIndex < relayUrls.size() - 1);
        down.setOnClickListener(v -> moveRelayUrl(rowIndex, rowIndex + 1));
        row.addView(down, ui.fixed(32, 40, 0));
        relayDownButtons.add(down);

        ImageButton delete = ui.iconButton(R.drawable.ic_df_close, "Remove relay", false);
        delete.setOnClickListener(v -> {
            if (rowIndex >= 0 && rowIndex < relayUrls.size()) {
                relayUrls.remove(rowIndex);
                renderRelayRows();
            }
        });
        row.addView(delete, ui.fixed(32, 40, 0));

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
        if (enabled) {
            // Keep the first row's "up" and the last row's "down" disabled after re-enabling.
            if (!relayUpButtons.isEmpty()) {
                relayUpButtons.get(0).setEnabled(false);
            }
            if (!relayDownButtons.isEmpty()) {
                relayDownButtons.get(relayDownButtons.size() - 1).setEnabled(false);
            }
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

    private GradientDrawable relayRowBackground(boolean active) {
        return active
                ? ui.shape(ui.primarySoft, ui.primary, 10, 1.5f)
                : ui.shape(ui.surfaceSubtle, ui.border, 10);
    }

    private int dp(float value) {
        return (int) (value * getResources().getDisplayMetrics().density + 0.5f);
    }
}
