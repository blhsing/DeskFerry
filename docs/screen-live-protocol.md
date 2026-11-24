# DeskFerry live screen protocol (screen protocol 2)

This document specifies the live screen session that runs inside the relay's
`screen` service. The relays only pair and pipe the bytes; everything below is
end-to-end between a Home client (viewer or agent) and the Work agent's screen
helper. The Go implementation is `internal/screenview` (`live_*.go`).

The session's byte stream is carried like RDP's: Home clients ask the relay
for a resumable session with heartbeats (`X-DeskFerry-Resumable`,
`X-DeskFerry-Heartbeat`), so when a proxy or relay drops the WebSocket both
sides resume the same session over a new one and replay what the other side
has not acknowledged. Everything below sees one unbroken stream. Only when
resuming fails (for example after the Work PC restarted) does the session end;
the Windows, macOS and Android viewers then open a new session, waiting 2, 4,
8, 16 and then 30 seconds between attempts until the user stops the view.

## Compatibility and negotiation

1. The Home client sends one JSON line (UTF-8, terminated by `\n`), the
   *request*. A live request keeps the legacy fields so an older Work agent
   still answers with the legacy tile-delta stream:

   ```json
   {"mode":"stream","interval_ms":250,"tile_size":64,
    "protocol":2,"client":"viewer","name":"HOME-PC",
    "client_hello":"<base64url, no padding>"}
   ```

   `client` is `viewer` or `agent`. `name` is shown on the Work PC's sharing
   banner. An optional `features` array lists extra rect encodings the client
   draws; `"scaled-jpeg"` allows encoding `3` and `"h264"` allows encoding `4`. An optional `min_fps` (0 to
   120, default 30) sets the frame rate compression aims for; `0` asks for
   lossless rects only.

2. The Work side answers with a *legacy-framed* message: a 4-byte big-endian
   length, then that many bytes of JSON metadata (the same framing as legacy
   `full`/`delta` frames, with no payload rectangles).
   - An older Work agent answers `{"type":"full",...}` followed by legacy
     deltas. The client then shows the legacy stream read-only.
   - A protocol 2 Work agent answers:

     ```json
     {"type":"hello","live":{"server_hello":"<base64url>","version":"0.15.0",
      "width":1920,"height":1080,"control_allowed":true,
      "capabilities":["control","clipboard","ui","sas","settle","cursor"]}}
     ```

   - On failure it answers `{"type":"error","error":"<code>: <message>"}`.

3. After `hello`, both directions carry only *secure records* (below). The
   client's first record must be `finish`. The Work side sends nothing else
   until it has verified `finish`.

## Keys and handshake

The pre-shared key is derived from the room proof, the credential every Home
app already saves for a room (the Work agent computes the same proof from its
stored room password), so viewers and agents never ask for anything extra:

```
proof = base64url(SHA-256("DeskFerry room credential v1\x00" + lower(trim(room)) + "\x00" + password))
PSK   = HKDF-SHA256(secret = UTF-8 proof,
                    salt   = "DeskFerry screen key v2\x00",
                    info   = "DeskFerry live screen PSK", length 32)
```

The relays see the proof when they pair a room, so they could derive the PSK
too. The encryption therefore keeps the screen and input private from anyone
who cannot authenticate to the room, such as other relay users and anyone
reading traffic to the HTTPS relay, but not from a relay operator or from
someone who can read the plain-HTTP traffic to the OCI relay.

ECDH uses P-256 with uncompressed points (65 bytes, `0x04 || X || Y`).

```
client_hello = C_pub(65) || C_nonce(32)                         97 bytes
server_hello = S_pub(65) || S_nonce(32) || S_mac(32)           129 bytes
TH     = SHA-256("DeskFerry live v2\x00" || client_hello || S_pub || S_nonce)
S_mac  = HMAC-SHA256(PSK, "server finished\x00" || TH)
C_mac  = HMAC-SHA256(PSK, "client finished\x00" || TH)
Z      = ECDH(C_priv, S_pub) = ECDH(S_priv, C_pub)              32 bytes
PRK    = HKDF-Extract(SHA-256, salt = TH, ikm = Z || PSK)
K_c2s  = HKDF-Expand(PRK, "DeskFerry c2s", 32)
K_s2c  = HKDF-Expand(PRK, "DeskFerry s2c", 32)
```

The client must verify `S_mac` before using the keys; a mismatch means the
room password differs, or one side runs DeskFerry 0.15.0 or 0.15.1, which
derived the key from the password itself (report `room password mismatch`).

## Secure records

```
record = length(4, big-endian) || AES-256-GCM(key, nonce, plaintext, aad = empty)
nonce  = 0x00000000 || sequence(8, big-endian)
```

Each direction has its own key and its own sequence, starting at 0 and
increasing by one per record. `length` counts the ciphertext including the
16-byte tag and must not exceed 2 MiB. Plaintext is `type(1) || payload` and
never exceeds 1 MiB. Any authentication failure ends the session.

All integers are big-endian and unsigned unless marked signed.

### Client to Work

| Type | Name | Payload |
|---|---|---|
| `0x01` | finish | `C_mac(32)` |
| `0x02` | ack | `seq(8)` of a completed frame |
| `0x20` | command | UTF-8 JSON command (below) |
| `0x7E` | ping | any bytes (echoed in pong) |
| `0x7F` | pong | echo of a ping payload |

### Work to client

| Type | Name | Payload |
|---|---|---|
| `0x10` | frame start | `seq(8) width(4) height(4) flags(1)`; flag `1` = keyframe (client resets its framebuffer to black at this size) |
| `0x11` | rects | `count(2)` then per rect `x(2) y(2) w(2) h(2) enc(1) len(4) data(len)` |
| `0x12` | frame end | `seq(8) flags(1)`; flag `1` = lossless (no lossy pixels remain), `2` = settled (no change pending) |
| `0x13` | cursor | `x(4, signed) y(4, signed) visible(1) shape_id(4) hot_x(2) hot_y(2) png_len(4) png` ; `png_len` is 0 when the shape is unchanged |
| `0x20` | message | UTF-8 JSON message (below) |
| `0x7E` | ping | any bytes |
| `0x7F` | pong | echo |

Rect encodings: `0` solid (`data` = R,G,B,A), `1` PNG, `2` JPEG, `3` JPEG at
reduced resolution (only when the client listed `"scaled-jpeg"`): the image is
smaller than the rect and the client stretches it to the rect. `4` is one
access unit of an H.264 stream (only when the client listed `"h264"` and the
Work side offers `h264` in its hello capabilities): `data` is one flags byte
(`1` = a new stream starts with this unit, so the client opens a new decoder)
followed by Annex B NAL units (start codes, SPS/PPS before each IDR,
Constrained Baseline profile, no B-frames). The coded picture is the rect
rounded up to multiples of 16 pixels; the client draws its top-left `w x h`
at the rect. A stream covers one rect until a unit with the start flag
begins the next stream. A client that cannot decode a unit (or missed the
stream start) skips the stream's units; the area is refined losslessly once
it stops changing, and the client may also send `refresh` with `keyframe`. Rects of one
frame are drawn in order; a client may draw them as they arrive but should
present the frame at `frame end`. The client acks each frame after it has drawn
it; the Work side uses acks for flow control, so a client that never acks
stops receiving frames. Coordinates are framebuffer pixels of the Work PC's
primary display; cursor coordinates are in the same space.

## JSON commands (client to Work)

Every command has `cmd` and an optional numeric `id`. A command with an `id`
gets exactly one `reply` message.

| `cmd` | Fields | Result |
|---|---|---|
| `control` | `enable` (bool), `yield_to_human` (bool) | `{"control_active":bool}` |
| `input` | `events` (array, below) | `{"human_active":bool}` |
| `refresh` | `keyframe` (bool) | sends lossless refinement now, or a full keyframe |
| `settle` | `quiet_ms` (default 400), `timeout_ms` (default 5000) | `{"seq":n,"settled":bool,"changed":[[x,y,w,h],...]}` — replied after the frame `seq` that contains every change so far |
| `status` | | status object (below) |
| `clipboard_get` | | `{"text":"..."}` |
| `clipboard_set` | `text` | `{}` |
| `ui_tree` | `depth` (default 6), `max_nodes` (default 400), `scope` (`foreground`/`focused`/`desktop`) | `{"nodes":[...]}` |
| `ui_find` | `text`, `max` (default 20) | `{"nodes":[...]}` |
| `sas` | | sends Ctrl+Alt+Del (secure attention sequence) |
| `quality` | `max_fps` (1-120), `min_fps` (0-120; 0 = lossless only), `jpeg_quality` (0 = adaptive, else 20-95) | `{}` |

UI nodes: `{"name","role","value","automation_id","class","enabled","focused",
"rect":[x,y,w,h],"depth"}` with `rect` in framebuffer pixels.

### Input events

Input is accepted only while control is enabled for this session and allowed
by the Work PC's settings.

| `t` | Fields |
|---|---|
| `move` | `x`, `y` |
| `button` | `b` (`left`/`right`/`middle`/`x1`/`x2`), `down` (bool), optional `x`, `y` |
| `wheel` | `dx`, `dy` (in 1/120 notches; positive `dy` scrolls up), optional `x`, `y` |
| `key` | `vk` (Windows virtual-key code), optional `scan`, `ext` (bool), `down` (bool) |
| `char_key` | `c` (one character), `down` (bool): the Work side resolves the key and Shift state for its keyboard layout |
| `text` | `s` (string): typed as Unicode characters regardless of layout |
| `release_all` | releases every key and button this session holds |

Web and Android clients map their key codes to Windows virtual-key codes.

## JSON messages (Work to client)

- `{"type":"reply","id":n,"ok":true,"result":{...}}` or
  `{"type":"reply","id":n,"ok":false,"error":"<code>: <message>"}`. Codes:
  `control_not_allowed`, `control_not_enabled`, `human_active`,
  `screen_locked`, `timeout`, `unsupported`, `failed`, `bad_request`.
- `{"type":"status", ...}` is pushed when it changes and returned by `status`:
  `width`, `height`, `control_allowed`, `control_active`, `yield_to_human`,
  `human_input_ms_ago` (-1 if none seen), `desktop` (input desktop name, for
  example `Default` or `Winlogon`), `locked` (bool), `foreground_title`,
  `cursor` (`[x,y]`), `capture` (`dxgi`/`gdi`), `viewers` (sessions sharing the
  screen).

## Flow control and quality

The Work side keeps capturing the latest screen and merges changes while it
waits, so a slow link lowers the frame rate instead of building a queue. It
keeps the unacknowledged bytes within a window derived from the measured
delivery rate and round-trip time.

Compression follows the delivered frame rate, measured against the viewer's
`min_fps` (30 unless the viewer sets it). With `min_fps` 0 every rect is
lossless (solid, PNG) at full resolution and nothing below applies. At full quality, text-like tiles
are lossless and photographic tiles are JPEG at quality 80. A tile is *in
motion* once it has kept changing, with no pause over 300 ms, for 300 ms. Every
second the Work side counts acknowledged frames; while changes have been
waiting for most of that second and fewer than `min_fps` frames arrived, it tries one
step more compression: lower JPEG quality, then tiles in motion as JPEG even
when text-like, then the region in motion as H.264 video (when both sides
support it), then tiles in motion at 1/2, 1/3 and 1/4 resolution. It keeps
a step only if the next second delivers clearly more frames (at least 15% and
one frame more), and otherwise reverts and waits before trying that step again
(10 s, doubling up to 2 minutes). With at least 10% more than `min_fps` frames a
second it tries one step less compression and keeps it if the rate stays at
`min_fps` or more. After 5
calm seconds it returns to full quality. Once the screen is still, it re-sends
every lossy region losslessly at full resolution and marks the frame
`lossless`. `status.compression` names the current level, and `status.perf` summarizes recent frame timings on the Work side.

## macOS Home viewer socket

The macOS Home agent runs the live client itself and serves the viewer page.
The page talks to it over one local WebSocket, `api/screen/ws` (relative to
the page, under the control panel's token path). No HTTP polling is used.

Agent to page:

- Text JSON `{"type":"state", ...}` whenever the session changes, with
  `status` (human-readable line), `phase` (`idle`, `connecting`, `live`,
  `legacy`, `error`, `stopped`), `control_allowed`, `control_active`,
  `width`, `height`, `desktop`, `locked`, `foreground_title`, and `stats`
  (`fps`, `kbps`, `quality` as numbers).
- Text JSON `{"type":"cursor","x","y","visible","hot_x","hot_y","shape_id",
  "shape"}`; `shape` is a `data:image/png;base64,...` URL sent only the first
  time a `shape_id` appears.
- Text JSON `{"type":"clipboard","text"}` answering `clipboard_get`, and
  `{"type":"error","error"}` for a failed page command.
- Binary frame messages, big-endian:
  - `0x01` frame start: `seq(4) width(4) height(4) keyframe(1)`; on a
    keyframe or size change the page resets its canvas to that size.
  - `0x02` rect: `x(2) y(2) w(2) h(2) enc(1) data`; `enc` 0 = solid
    (`data` is R,G,B,A), 1 = PNG, 2 = JPEG.
  - `0x03` frame end: `seq(4) flags(1)` (1 = lossless).

The page must draw rects in arrival order (decode asynchronously but apply in
sequence) and reply `{"type":"ack","seq":n}` after drawing a frame. The agent
stops forwarding while more than three frames are unacknowledged and then
sends a fresh keyframe once the page catches up.

Page to agent (text JSON): `{"type":"start"}`, `{"type":"stop"}`,
`{"type":"ack","seq"}`, `{"type":"control","enable":bool}`,
`{"type":"input","events":[...]}` (events as in *Input events*),
`{"type":"sas"}`, `{"type":"refresh"}`, `{"type":"clipboard_get"}`,
`{"type":"clipboard_set","text"}`.

`api/screen/frame.png` still returns the current framebuffer as a PNG for
saving.
