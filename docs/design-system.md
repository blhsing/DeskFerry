# DeskFerry Design System

Every DeskFerry surface (the Windows app and its windows, the macOS Home agent page, the Android Home app, and the relay dashboards) follows this guide so the product reads as one family: calm, friendly, modern, and quietly technical. Each platform keeps its native controls where restyling them is impractical. The palette, typography scale, spacing, shapes, and status language below are shared.

## Principles

- **Status first.** The first thing on every screen answers "is it working?" with a coloured status chip and one plain sentence.
- **Calm surfaces.** A soft neutral page background, white cards with a hairline border, generous padding, and one accent colour for the primary action.
- **One primary action per area.** Other actions are secondary or quiet. Destructive actions use the danger colour only on confirmation.
- **Progressive detail.** Diagnostics such as logs, room details, and raw relay JSON live in their own card or section, in a monospace font, below the everyday controls.
- **Plain language.** Sentence-case labels ("Relay services", not "RELAY SERVICE BASE URLS"), short helper text under fields, and no jargon in headings.

## Colour

### Light theme (default)

| Token | Hex | Use |
| --- | --- | --- |
| `bg` | `#F5F7FB` | Window and page background |
| `surface` | `#FFFFFF` | Cards, panels, inputs |
| `surface-subtle` | `#F9FAFC` | Inset areas, list rows, read-only fields |
| `border` | `#E3E8EF` | Card borders, dividers, input borders |
| `border-strong` | `#CDD5DF` | Hovered or focused input border |
| `text` | `#101828` | Primary text |
| `text-secondary` | `#475467` | Secondary text and labels |
| `text-muted` | `#667085` | Helper text, captions, timestamps |
| `primary` | `#2563EB` | Primary buttons, links, focus rings, selected items |
| `primary-hover` | `#1D4ED8` | Primary hover and pressed states |
| `primary-soft` | `#EEF4FF` | Selected rows, info backgrounds |
| `primary-on` | `#FFFFFF` | Text on primary |

### Status colours

| Status | Foreground | Background | Dot | Meaning |
| --- | --- | --- | --- | --- |
| `success` | `#067647` | `#ECFDF3` | `#17B26A` | Connected, running, online |
| `warning` | `#B54708` | `#FFFAEB` | `#F79009` | Connecting, checking, degraded, retrying |
| `danger` | `#B42318` | `#FEF3F2` | `#F04438` | Offline, failed, error |
| `neutral` | `#344054` | `#F2F4F7` | `#98A2B3` | Stopped, idle, unknown, not configured |
| `info` | `#1849A9` | `#EEF4FF` | `#2E90FA` | Informational |

### Dark theme (web dashboards and the macOS page)

Web surfaces follow `prefers-color-scheme`.

| Token | Hex |
| --- | --- |
| `bg` | `#0B1220` |
| `surface` | `#111A2E` |
| `surface-subtle` | `#0F1729` |
| `border` | `#1F2A44` |
| `text` | `#F2F4F7` |
| `text-secondary` | `#C2C9D6` |
| `text-muted` | `#8A94A6` |
| `primary` | `#4F8BFF` |
| `primary-soft` | `#16254A` |

In dark mode, status chips use the dot colour for text on a 14 % alpha tint of the same colour.

## Typography

| Platform | Family |
| --- | --- |
| Windows | Segoe UI Variable Display or Text (Windows 11), falling back to Segoe UI |
| Web and macOS | `-apple-system, BlinkMacSystemFont, "Segoe UI Variable Text", "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif` |
| Android | `sans-serif` (Roboto); `sans-serif-medium` for emphasis |
| Monospace | `"Cascadia Mono", "SF Mono", ui-monospace, Menlo, Consolas, monospace` (Windows: Cascadia Mono, then Consolas) |

| Style | Size | Weight | Use |
| --- | --- | --- | --- |
| Display | 22-24 px (Windows 16 pt) | Semibold 600 | Window or page title |
| Title | 16-17 px (Windows 11 pt) | Semibold 600 | Card titles |
| Body | 14 px (Windows 9.5-10 pt) | Regular 400 | Default text and controls |
| Label | 13 px | Medium 500 | Field labels |
| Caption | 12 px | Medium 500 | Helper text, tile captions, timestamps |
| Metric | 18-20 px | Semibold 600 | Status tile values |
| Mono | 12.5-13 px | Regular | Logs, room details, command output |

## Spacing and shape

- 4 px grid: 4, 8, 12, 16, 20, 24, 32.
- Page padding 24 px (16 px on phones); card padding 16-20 px; gap between cards 16 px; gap between fields 12 px.
- Radii: cards 12 px, inputs and buttons 8 px, chips and pills fully rounded.
- Cards: `surface` with a 1 px `border` and, on the web, shadow `0 1px 2px rgba(16,24,40,.05)`.
- Controls are at least 36 px tall (44-48 dp on Android).

## Components

- **App header**: the DeskFerry mark (the existing app icon or `icon.svg`), product name, a version caption in `text-muted`, and an overall status chip on the right.
- **Status chip**: a pill with a 8 px dot and a short word ("Connected", "Offline", "Checking", "Stopped"), coloured per the status table.
- **Status tile**: a small card with a caption on top, a metric value, and a status dot or chip. Tiles are laid out in a responsive row: four across on desktop, two across on phones.
- **Card**: title, optional one-line description in `text-muted`, then content.
- **Fields**: a label above the input, helper text below, full-width inputs, and related fields side by side on wide layouts.
- **Buttons**:
  - **Primary**: filled `primary`.
  - **Secondary**: `surface` with a `border`, text in `text`.
  - **Quiet**: text-only, in `primary`.
  - **Danger**: `danger` text, for confirmation steps only.
- **Relay list**: rows with a drag handle, a "Primary" or "Fallback" badge, and the URL. Edit, up/down, and remove actions sit at the right.
- **Log / diagnostics**: `surface-subtle` background, monospace text, timestamps in `text-muted`.
- **Empty states**: one muted sentence explaining what will appear and how to make it appear.

## Status vocabulary

Use these words consistently across platforms:

- **Tunnel**: Stopped (neutral), Starting (warning), Running (success), Error (danger).
- **Work agent**: Online (success), Checking (warning), Offline (danger), Unknown (neutral).
- **Relay**: Connected (success), Connecting (warning), Unreachable (danger).
- **Sessions**: "No active sessions" (neutral) or "N active" (success).

## Platform notes

- **Windows (lxn/walk)**: Native push buttons, edits, and list boxes remain native but sit on white card composites over the `bg` window colour. Use per-monitor DPI awareness, Segoe UI Variable fonts, coloured status chips drawn as custom widgets or coloured labels on tinted composites, and consistent margins from the spacing scale. Every DeskFerry window shows the app icon.
- **Web (relay dashboards and the macOS page)**: Self-contained HTML, CSS and JavaScript with no external fonts, CDNs, or build step. Use responsive CSS grid, light and dark themes, and accessible contrast. Keep the existing element IDs that tests or scripts depend on.
- **Android (platform widgets, no AndroidX)**: Use `GradientDrawable` cards and pills, `RippleDrawable` buttons, Roboto with `sans-serif-medium` emphasis, 16 dp page padding, a 2x2 tile grid, and status chips.
