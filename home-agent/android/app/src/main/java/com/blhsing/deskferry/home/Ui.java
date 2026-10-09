package com.blhsing.deskferry.home;

import android.app.Activity;
import android.content.Context;
import android.content.res.ColorStateList;
import android.content.res.Configuration;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.ColorFilter;
import android.graphics.Paint;
import android.graphics.Path;
import android.graphics.PixelFormat;
import android.graphics.Rect;
import android.graphics.Typeface;
import android.graphics.drawable.Drawable;
import android.graphics.drawable.GradientDrawable;
import android.graphics.drawable.LayerDrawable;
import android.graphics.drawable.RippleDrawable;
import android.graphics.drawable.StateListDrawable;
import android.os.Build;
import android.text.TextUtils;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.Window;
import android.view.WindowInsets;
import android.view.WindowInsetsController;
import android.widget.ArrayAdapter;
import android.widget.Button;
import android.widget.EditText;
import android.widget.ImageButton;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.Spinner;
import android.widget.TextView;

import java.util.List;

/**
 * Small platform-widget UI kit implementing docs/design-system.md: palette, type scale,
 * cards, status chips, badges, buttons, fields, and system-bar handling. No AndroidX.
 */
final class Ui {
    enum Tone { SUCCESS, WARNING, DANGER, NEUTRAL, INFO }

    enum Type { DISPLAY, TITLE, BODY, BODY_STRONG, LABEL, CAPTION, METRIC, MONO }

    private static final int[] STATE_DISABLED = {-android.R.attr.state_enabled};
    private static final int[] STATE_FOCUSED = {android.R.attr.state_focused};
    private static final int[] STATE_PRESSED = {android.R.attr.state_pressed};
    private static final int[] STATE_DEFAULT = {};

    final Context context;
    final boolean dark;
    private final float density;

    final int bg;
    final int surface;
    final int surfaceSubtle;
    final int border;
    final int borderStrong;
    final int text;
    final int textSecondary;
    final int textMuted;
    final int primary;
    final int primaryPressed;
    final int primarySoft;
    final int onPrimary;

    private final Typeface medium = Typeface.create("sans-serif-medium", Typeface.NORMAL);
    private final Typeface regular = Typeface.create("sans-serif", Typeface.NORMAL);

    private Ui(Context context, boolean dark) {
        this.context = context;
        this.dark = dark;
        this.density = context.getResources().getDisplayMetrics().density;
        if (dark) {
            bg = rgb("#0B1220");
            surface = rgb("#111A2E");
            surfaceSubtle = rgb("#0F1729");
            border = rgb("#1F2A44");
            borderStrong = rgb("#2C3B5E");
            text = rgb("#F2F4F7");
            textSecondary = rgb("#C2C9D6");
            textMuted = rgb("#8A94A6");
            primary = rgb("#4F8BFF");
            primaryPressed = rgb("#3B76F0");
            primarySoft = rgb("#16254A");
            onPrimary = Color.WHITE;
        } else {
            bg = rgb("#F5F7FB");
            surface = Color.WHITE;
            surfaceSubtle = rgb("#F9FAFC");
            border = rgb("#E3E8EF");
            borderStrong = rgb("#CDD5DF");
            text = rgb("#101828");
            textSecondary = rgb("#475467");
            textMuted = rgb("#667085");
            primary = rgb("#2563EB");
            primaryPressed = rgb("#1D4ED8");
            primarySoft = rgb("#EEF4FF");
            onPrimary = Color.WHITE;
        }
    }

    /** Follows the system light/dark setting. */
    static Ui forSystem(Context context) {
        int mode = context.getResources().getConfiguration().uiMode & Configuration.UI_MODE_NIGHT_MASK;
        return new Ui(context, mode == Configuration.UI_MODE_NIGHT_YES);
    }

    /** Always dark, for the screen viewer. */
    static Ui darkTheme(Context context) {
        return new Ui(context, true);
    }

    // ---- Units and colours ------------------------------------------------------------

    int dp(float value) {
        return (int) (value * density + 0.5f);
    }

    static int rgb(String hex) {
        return Color.parseColor(hex);
    }

    static int alpha(int color, float fraction) {
        return Color.argb(Math.round(255 * fraction), Color.red(color), Color.green(color), Color.blue(color));
    }

    int toneDot(Tone tone) {
        switch (tone) {
            case SUCCESS: return rgb("#17B26A");
            case WARNING: return rgb("#F79009");
            case DANGER: return rgb("#F04438");
            case INFO: return rgb("#2E90FA");
            default: return rgb("#98A2B3");
        }
    }

    int toneForeground(Tone tone) {
        if (dark) {
            return tone == Tone.NEUTRAL ? textSecondary : toneDot(tone);
        }
        switch (tone) {
            case SUCCESS: return rgb("#067647");
            case WARNING: return rgb("#B54708");
            case DANGER: return rgb("#B42318");
            case INFO: return rgb("#1849A9");
            default: return rgb("#344054");
        }
    }

    int toneBackground(Tone tone) {
        if (dark) {
            return alpha(toneDot(tone), 0.14f);
        }
        switch (tone) {
            case SUCCESS: return rgb("#ECFDF3");
            case WARNING: return rgb("#FFFAEB");
            case DANGER: return rgb("#FEF3F2");
            case INFO: return rgb("#EEF4FF");
            default: return rgb("#F2F4F7");
        }
    }

    static ColorStateList enabledColors(int enabled, int disabled) {
        return new ColorStateList(new int[][]{STATE_DISABLED, STATE_DEFAULT}, new int[]{disabled, enabled});
    }

    // ---- Shapes -----------------------------------------------------------------------

    GradientDrawable shape(int fill, int stroke, float radiusDp) {
        return shape(fill, stroke, radiusDp, 1);
    }

    GradientDrawable shape(int fill, int stroke, float radiusDp, float strokeDp) {
        GradientDrawable drawable = new GradientDrawable();
        drawable.setShape(GradientDrawable.RECTANGLE);
        drawable.setColor(fill);
        drawable.setCornerRadius(dp(radiusDp));
        if (Color.alpha(stroke) != 0) {
            drawable.setStroke(Math.max(1, dp(strokeDp)), stroke);
        }
        return drawable;
    }

    GradientDrawable pill(int fill) {
        return shape(fill, Color.TRANSPARENT, 999);
    }

    GradientDrawable dot(int color, float sizeDp) {
        GradientDrawable drawable = new GradientDrawable();
        drawable.setShape(GradientDrawable.OVAL);
        drawable.setColor(color);
        drawable.setSize(dp(sizeDp), dp(sizeDp));
        return drawable;
    }

    private RippleDrawable ripple(int rippleColor, Drawable content, float radiusDp) {
        return new RippleDrawable(ColorStateList.valueOf(rippleColor), content,
                shape(Color.WHITE, Color.TRANSPARENT, radiusDp));
    }

    /** A ripple-backed state list: pressed/disabled fills on a rounded rectangle. */
    private Drawable buttonBackground(int fill, int stroke, int disabledFill, int disabledStroke, int rippleColor, float radiusDp) {
        StateListDrawable states = new StateListDrawable();
        states.addState(STATE_DISABLED, shape(disabledFill, disabledStroke, radiusDp));
        states.addState(STATE_DEFAULT, shape(fill, stroke, radiusDp));
        return ripple(rippleColor, states, radiusDp);
    }

    Drawable rowBackground(float radiusDp) {
        return ripple(alpha(primary, dark ? 0.18f : 0.10f), shape(Color.TRANSPARENT, Color.TRANSPARENT, radiusDp), radiusDp);
    }

    // ---- Text -------------------------------------------------------------------------

    TextView text(String value, Type type) {
        TextView view = new TextView(context);
        view.setText(value);
        styleText(view, type);
        return view;
    }

    void styleText(TextView view, Type type) {
        switch (type) {
            case DISPLAY:
                view.setTextSize(22);
                view.setTypeface(medium);
                view.setTextColor(text);
                break;
            case TITLE:
                view.setTextSize(16);
                view.setTypeface(medium);
                view.setTextColor(text);
                break;
            case BODY_STRONG:
                view.setTextSize(14);
                view.setTypeface(medium);
                view.setTextColor(text);
                break;
            case LABEL:
                view.setTextSize(13);
                view.setTypeface(medium);
                view.setTextColor(textSecondary);
                break;
            case CAPTION:
                view.setTextSize(12);
                view.setTypeface(medium);
                view.setTextColor(textMuted);
                break;
            case METRIC:
                view.setTextSize(19);
                view.setTypeface(medium);
                view.setTextColor(text);
                break;
            case MONO:
                view.setTextSize(12.5f);
                view.setTypeface(Typeface.MONOSPACE);
                view.setTextColor(textSecondary);
                view.setLineSpacing(0, 1.12f);
                break;
            case BODY:
            default:
                view.setTextSize(14);
                view.setTypeface(regular);
                view.setTextColor(text);
                break;
        }
    }

    /** Card header: title plus optional one-line description in text-muted. */
    View sectionHeader(String title, String description) {
        LinearLayout header = vertical();
        header.addView(text(title, Type.TITLE));
        if (description != null && !description.isEmpty()) {
            TextView desc = text(description, Type.CAPTION);
            desc.setTextSize(13);
            desc.setTypeface(regular);
            desc.setPadding(0, dp(2), 0, 0);
            header.addView(desc);
        }
        header.setPadding(0, 0, 0, dp(14));
        return header;
    }

    // ---- Layout -----------------------------------------------------------------------

    LinearLayout vertical() {
        LinearLayout layout = new LinearLayout(context);
        layout.setOrientation(LinearLayout.VERTICAL);
        return layout;
    }

    LinearLayout horizontal() {
        LinearLayout layout = new LinearLayout(context);
        layout.setOrientation(LinearLayout.HORIZONTAL);
        layout.setGravity(Gravity.CENTER_VERTICAL);
        layout.setBaselineAligned(false);
        return layout;
    }

    LinearLayout card(String title, String description) {
        LinearLayout card = vertical();
        card.setBackground(shape(surface, border, 12));
        card.setPadding(dp(16), dp(16), dp(16), dp(16));
        if (title != null) {
            card.addView(sectionHeader(title, description), matchWrap(0));
        }
        return card;
    }

    View divider() {
        View view = new View(context);
        view.setBackgroundColor(border);
        return view;
    }

    LinearLayout.LayoutParams matchWrap(int bottomMarginDp) {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        params.bottomMargin = dp(bottomMarginDp);
        return params;
    }

    LinearLayout.LayoutParams weighted(int heightPx, float weight, int startMarginDp) {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(0, heightPx, weight);
        params.setMarginStart(dp(startMarginDp));
        return params;
    }

    LinearLayout.LayoutParams fixed(int widthDp, int heightDp, int startMarginDp) {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(
                widthDp < 0 ? ViewGroup.LayoutParams.WRAP_CONTENT : dp(widthDp),
                heightDp < 0 ? ViewGroup.LayoutParams.WRAP_CONTENT : dp(heightDp));
        params.setMarginStart(dp(startMarginDp));
        return params;
    }

    // ---- Chips and badges -------------------------------------------------------------

    TextView chip(Tone tone, String value) {
        TextView chip = new TextView(context);
        chip.setTextSize(12);
        chip.setTypeface(medium);
        chip.setSingleLine(true);
        chip.setEllipsize(TextUtils.TruncateAt.END);
        chip.setGravity(Gravity.CENTER_VERTICAL);
        chip.setPadding(dp(10), dp(4), dp(10), dp(4));
        chip.setCompoundDrawablePadding(dp(6));
        setChip(chip, tone, value);
        return chip;
    }

    void setChip(TextView chip, Tone tone, String value) {
        chip.setText(value);
        chip.setTextColor(toneForeground(tone));
        chip.setBackground(pill(toneBackground(tone)));
        chip.setCompoundDrawablesRelativeWithIntrinsicBounds(dot(toneDot(tone), 8), null, null, null);
        chip.setContentDescription(value);
    }

    TextView badge(Tone tone, String value) {
        TextView badge = new TextView(context);
        badge.setTextSize(12);
        badge.setTypeface(medium);
        badge.setSingleLine(true);
        badge.setGravity(Gravity.CENTER);
        badge.setPadding(dp(8), dp(2), dp(8), dp(2));
        badge.setText(value);
        badge.setTextColor(toneForeground(tone));
        badge.setBackground(pill(toneBackground(tone)));
        return badge;
    }

    // ---- Buttons ----------------------------------------------------------------------

    Button button(String label, int iconRes) {
        Button button = new Button(context);
        button.setAllCaps(false);
        button.setText(label);
        button.setTextSize(14);
        button.setTypeface(medium);
        button.setSingleLine(true);
        button.setEllipsize(TextUtils.TruncateAt.END);
        button.setGravity(Gravity.CENTER);
        button.setMinHeight(0);
        button.setMinimumHeight(0);
        button.setMinWidth(0);
        button.setMinimumWidth(0);
        button.setPadding(dp(14), 0, dp(14), 0);
        button.setStateListAnimator(null);
        button.setElevation(0);
        if (iconRes != 0) {
            setButtonIcon(button, iconRes);
        }
        return button;
    }

    void setButtonIcon(Button button, int iconRes) {
        Drawable icon = context.getDrawable(iconRes);
        if (icon == null) {
            return;
        }
        icon = icon.mutate();
        icon.setBounds(0, 0, dp(18), dp(18));
        button.setCompoundDrawablesRelative(icon, null, null, null);
        button.setCompoundDrawablePadding(dp(8));
        button.setCompoundDrawableTintList(button.getTextColors());
        if (button.getTag(R.id.df_icon_centering) == null) {
            button.setTag(R.id.df_icon_centering, Boolean.TRUE);
            // Keep the start icon next to the centred label instead of pinned to the edge.
            button.addOnLayoutChangeListener((v, left, top, right, bottom, oldLeft, oldTop, oldRight, oldBottom) -> {
                Button b = (Button) v;
                Drawable start = b.getCompoundDrawablesRelative()[0];
                if (start == null) {
                    return;
                }
                float textWidth = b.getPaint().measureText(b.getText().toString());
                int content = Math.round(start.getBounds().width() + b.getCompoundDrawablePadding() + textWidth);
                int pad = Math.max(dp(8), (right - left - content) / 2);
                if (pad != b.getPaddingStart() || pad != b.getPaddingEnd()) {
                    b.setPaddingRelative(pad, b.getPaddingTop(), pad, b.getPaddingBottom());
                }
            });
        }
    }

    Button primaryButton(String label, int iconRes) {
        Button button = button(label, iconRes);
        stylePrimary(button);
        return button;
    }

    Button secondaryButton(String label, int iconRes) {
        Button button = button(label, iconRes);
        styleSecondary(button);
        return button;
    }

    Button quietButton(String label, int iconRes) {
        Button button = button(label, iconRes);
        styleQuiet(button);
        return button;
    }

    Button dangerButton(String label, int iconRes) {
        Button button = button(label, iconRes);
        button.setBackground(buttonBackground(Color.TRANSPARENT, Color.TRANSPARENT, Color.TRANSPARENT,
                Color.TRANSPARENT, alpha(toneDot(Tone.DANGER), 0.16f), 8));
        applyTextColors(button, enabledColors(toneForeground(Tone.DANGER), textMuted));
        return button;
    }

    void stylePrimary(Button button) {
        button.setBackground(buttonBackground(primary, Color.TRANSPARENT, dark ? border : rgb("#D0D5DD"),
                Color.TRANSPARENT, alpha(Color.WHITE, 0.28f), 8));
        applyTextColors(button, enabledColors(onPrimary, dark ? textMuted : Color.WHITE));
    }

    void styleSecondary(Button button) {
        button.setBackground(buttonBackground(surface, dark ? borderStrong : borderStrong, surfaceSubtle, border,
                alpha(primary, dark ? 0.20f : 0.10f), 8));
        applyTextColors(button, enabledColors(text, textMuted));
    }

    void styleQuiet(Button button) {
        button.setBackground(buttonBackground(Color.TRANSPARENT, Color.TRANSPARENT, Color.TRANSPARENT,
                Color.TRANSPARENT, alpha(primary, dark ? 0.20f : 0.10f), 8));
        applyTextColors(button, enabledColors(primary, textMuted));
    }

    private void applyTextColors(Button button, ColorStateList colors) {
        button.setTextColor(colors);
        button.setCompoundDrawableTintList(colors);
    }

    /** Square icon-only button. Outlined variants sit beside fields; plain ones live inside list rows. */
    ImageButton iconButton(int iconRes, String description, boolean outlined) {
        ImageButton button = new ImageButton(context);
        Drawable icon = context.getDrawable(iconRes);
        if (icon != null) {
            button.setImageDrawable(icon.mutate());
        }
        button.setScaleType(ImageView.ScaleType.CENTER);
        button.setPadding(0, 0, 0, 0);
        button.setContentDescription(description);
        button.setStateListAnimator(null);
        button.setImageTintList(enabledColors(textSecondary, alpha(textMuted, 0.45f)));
        if (outlined) {
            button.setBackground(buttonBackground(surface, borderStrong, surfaceSubtle, border,
                    alpha(primary, dark ? 0.20f : 0.10f), 8));
        } else {
            button.setBackground(buttonBackground(Color.TRANSPARENT, Color.TRANSPARENT, Color.TRANSPARENT,
                    Color.TRANSPARENT, alpha(primary, dark ? 0.22f : 0.12f), 8));
        }
        if (Build.VERSION.SDK_INT >= 26) {
            button.setTooltipText(description);
        }
        return button;
    }

    // ---- Fields -----------------------------------------------------------------------

    Drawable fieldBackground() {
        StateListDrawable states = new StateListDrawable();
        states.addState(STATE_DISABLED, shape(surfaceSubtle, border, 8));
        states.addState(STATE_FOCUSED, shape(surface, primary, 8, 1.5f));
        states.addState(STATE_DEFAULT, shape(surface, dark ? borderStrong : borderStrong, 8));
        return states;
    }

    EditText field(String hint) {
        EditText edit = new EditText(context);
        edit.setSingleLine(true);
        edit.setTextSize(14);
        edit.setTypeface(regular);
        edit.setHint(hint);
        edit.setTextColor(enabledColors(text, textMuted));
        edit.setHintTextColor(textMuted);
        edit.setBackground(fieldBackground());
        edit.setPadding(dp(12), 0, dp(12), 0);
        edit.setGravity(Gravity.CENTER_VERTICAL);
        edit.setMinHeight(0);
        edit.setMinimumHeight(0);
        return edit;
    }

    /** Label above, full-width control, helper text below. */
    LinearLayout fieldGroup(String label, View control, int controlHeightDp, String helper) {
        LinearLayout group = vertical();
        if (label != null) {
            TextView title = text(label, Type.LABEL);
            title.setPadding(0, 0, 0, dp(6));
            group.addView(title);
        }
        group.addView(control, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT,
                controlHeightDp < 0 ? ViewGroup.LayoutParams.WRAP_CONTENT : dp(controlHeightDp)));
        if (helper != null) {
            TextView help = text(helper, Type.CAPTION);
            help.setPadding(0, dp(5), 0, 0);
            group.addView(help);
        }
        return group;
    }

    Spinner spinner() {
        Spinner spinner = new Spinner(context, Spinner.MODE_DROPDOWN);
        LayerDrawable layers = new LayerDrawable(new Drawable[]{fieldBackground(), new Chevron(textMuted, dp(1.75f))});
        layers.setLayerGravity(1, Gravity.CENTER_VERTICAL | Gravity.END);
        layers.setLayerSize(1, dp(12), dp(12));
        layers.setLayerInsetEnd(1, dp(14));
        spinner.setBackground(layers);
        spinner.setPadding(dp(12), 0, dp(36), 0);
        spinner.setPopupBackgroundDrawable(shape(surface, border, 8));
        spinner.setDropDownVerticalOffset(dp(4));
        return spinner;
    }

    ArrayAdapter<String> spinnerAdapter(List<String> items) {
        ArrayAdapter<String> adapter = new ArrayAdapter<String>(context, android.R.layout.simple_spinner_item, items) {
            @Override
            public View getView(int position, View convertView, ViewGroup parent) {
                TextView view = (TextView) super.getView(position, convertView, parent);
                view.setTextSize(14);
                view.setTypeface(medium);
                view.setTextColor(enabledColors(text, textMuted));
                view.setDuplicateParentStateEnabled(true);
                view.setSingleLine(true);
                view.setEllipsize(TextUtils.TruncateAt.END);
                view.setPadding(0, 0, 0, 0);
                return view;
            }

            @Override
            public View getDropDownView(int position, View convertView, ViewGroup parent) {
                TextView view = (TextView) super.getDropDownView(position, convertView, parent);
                view.setTextSize(14);
                view.setTypeface(regular);
                view.setTextColor(text);
                view.setMinHeight(dp(44));
                view.setGravity(Gravity.CENTER_VERTICAL);
                view.setPadding(dp(16), 0, dp(16), 0);
                return view;
            }
        };
        adapter.setDropDownViewResource(android.R.layout.simple_spinner_dropdown_item);
        return adapter;
    }

    // ---- Window -----------------------------------------------------------------------

    /**
     * Colours the system bars to match the page and pads {@code root} by the system-bar and IME
     * insets, which matters on Android 15+ where targetSdk 35 apps are drawn edge to edge.
     */
    void applySystemBars(Activity activity, View root, int statusBarColor) {
        applySystemBars(activity, root, root, statusBarColor, bg, true);
    }

    /**
     * Colours the system bars and pads for their insets, which matters on Android 15+ where
     * targetSdk 35 apps are drawn edge to edge. {@code root} takes the top and side insets;
     * {@code bottom} (a bottom bar, or {@code root} itself) takes the bottom inset so its
     * background extends under the navigation bar. The IME inset is included only when the
     * window should shrink for the keyboard rather than pan.
     */
    void applySystemBars(Activity activity, View root, View bottomView, int statusBarColor,
                         int navigationBarColor, boolean includeIme) {
        Window window = activity.getWindow();
        window.setStatusBarColor(statusBarColor);
        window.setNavigationBarColor(navigationBarColor);
        boolean light = !dark;
        if (Build.VERSION.SDK_INT >= 30) {
            WindowInsetsController controller = window.getInsetsController();
            if (controller != null) {
                int mask = WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS
                        | WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS;
                controller.setSystemBarsAppearance(light ? mask : 0, mask);
            }
        } else {
            View decor = window.getDecorView();
            int flags = decor.getSystemUiVisibility();
            int lightFlags = View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR;
            if (Build.VERSION.SDK_INT >= 26) {
                lightFlags |= View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR;
            }
            decor.setSystemUiVisibility(light ? (flags | lightFlags) : (flags & ~lightFlags));
        }
        final int left = root.getPaddingLeft();
        final int top = root.getPaddingTop();
        final int right = root.getPaddingRight();
        final int bottom = root.getPaddingBottom();
        final int barLeft = bottomView.getPaddingLeft();
        final int barTop = bottomView.getPaddingTop();
        final int barRight = bottomView.getPaddingRight();
        final int barBottom = bottomView.getPaddingBottom();
        root.setOnApplyWindowInsetsListener((view, insets) -> {
            int l;
            int t;
            int r;
            int b;
            if (Build.VERSION.SDK_INT >= 30) {
                int types = WindowInsets.Type.systemBars() | WindowInsets.Type.displayCutout();
                if (includeIme) {
                    types |= WindowInsets.Type.ime();
                }
                android.graphics.Insets bars = insets.getInsets(types);
                l = bars.left;
                t = bars.top;
                r = bars.right;
                b = bars.bottom;
            } else {
                l = insets.getSystemWindowInsetLeft();
                t = insets.getSystemWindowInsetTop();
                r = insets.getSystemWindowInsetRight();
                b = insets.getSystemWindowInsetBottom();
            }
            if (bottomView == view) {
                view.setPadding(left + l, top + t, right + r, bottom + b);
            } else {
                view.setPadding(left + l, top + t, right + r, bottom);
                bottomView.setPadding(barLeft, barTop, barRight, barBottom + b);
            }
            return insets;
        });
        root.requestApplyInsets();
    }

    /** Down chevron used as the spinner affordance. */
    private static final class Chevron extends Drawable {
        private final Paint paint = new Paint(Paint.ANTI_ALIAS_FLAG);
        private final Path path = new Path();

        Chevron(int color, float strokeWidth) {
            paint.setColor(color);
            paint.setStyle(Paint.Style.STROKE);
            paint.setStrokeWidth(strokeWidth);
            paint.setStrokeCap(Paint.Cap.ROUND);
            paint.setStrokeJoin(Paint.Join.ROUND);
        }

        @Override
        public void draw(Canvas canvas) {
            Rect b = getBounds();
            float w = b.width();
            float h = b.height();
            path.reset();
            path.moveTo(b.left + w * 0.15f, b.top + h * 0.35f);
            path.lineTo(b.left + w * 0.5f, b.top + h * 0.68f);
            path.lineTo(b.left + w * 0.85f, b.top + h * 0.35f);
            canvas.drawPath(path, paint);
        }

        @Override
        public void setAlpha(int alpha) {
            paint.setAlpha(alpha);
        }

        @Override
        public void setColorFilter(ColorFilter colorFilter) {
            paint.setColorFilter(colorFilter);
        }

        @Override
        public int getOpacity() {
            return PixelFormat.TRANSLUCENT;
        }
    }
}
