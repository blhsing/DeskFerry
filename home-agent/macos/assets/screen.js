'use strict';
// DeskFerry live screen viewer.
//
// The page talks to the macOS Home agent over one local WebSocket,
// api/screen/ws (docs/screen-live-protocol.md, "macOS Home viewer socket").
// Every live update arrives on that socket; nothing is polled over HTTP.
// URLs are relative because the page is served under a random token path.
//
// The first half of this file is pure helpers (message parsing, key mapping,
// coordinate and wheel conversion); they are exported at the bottom for tests.

// ---------------------------------------------------------------- protocol

const MSG_FRAME_START = 0x01, MSG_RECT = 0x02, MSG_FRAME_END = 0x03;
const ENC_SOLID = 0, ENC_PNG = 1, ENC_JPEG = 2;
const MAX_CANVAS_SIDE = 16384;

// parseBinaryMessage decodes one binary socket message (big-endian) or
// returns null when it is malformed or of an unknown type.
function parseBinaryMessage(data) {
  const bytes = data instanceof Uint8Array ? data : new Uint8Array(data);
  if (bytes.length < 1) return null;
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  switch (bytes[0]) {
    case MSG_FRAME_START:
      if (bytes.length < 14) return null;
      return { kind: 'start', seq: view.getUint32(1), width: view.getUint32(5), height: view.getUint32(9), keyframe: bytes[13] !== 0 };
    case MSG_RECT:
      if (bytes.length < 10) return null;
      return { kind: 'rect', x: view.getUint16(1), y: view.getUint16(3), w: view.getUint16(5), h: view.getUint16(7), enc: bytes[9], data: bytes.subarray(10) };
    case MSG_FRAME_END:
      if (bytes.length < 6) return null;
      return { kind: 'end', seq: view.getUint32(1), flags: bytes[5], lossless: (bytes[5] & 1) !== 0 };
    default:
      return null;
  }
}

// solidColor turns a solid rect's R,G,B,A bytes into a canvas fill style.
function solidColor(data) {
  if (!data || data.length < 3) return null;
  const alpha = data.length > 3 ? data[3] : 255;
  if (alpha === 255) return 'rgb(' + data[0] + ',' + data[1] + ',' + data[2] + ')';
  return 'rgba(' + data[0] + ',' + data[1] + ',' + data[2] + ',' + Math.round(alpha / 255 * 1000) / 1000 + ')';
}

// Encoding 3 is a JPEG at reduced resolution; drawImage stretches it to the rect.
function rectMimeType(enc) { return enc === ENC_PNG ? 'image/png' : (enc === ENC_JPEG || enc === 3) ? 'image/jpeg' : ''; }

// Encoding 4 is one access unit of an H.264 (Annex B) stream for its rect;
// the first byte holds flags (1 = a new stream starts).
const ENC_H264 = 4;

// annexBNalTypes lists the NAL unit types in an Annex B byte stream.
function annexBNalTypes(bytes) {
  const types = [];
  for (let i = 0; i + 3 < bytes.length; i++) {
    if (bytes[i] === 0 && bytes[i + 1] === 0 && (bytes[i + 2] === 1 || (bytes[i + 2] === 0 && bytes[i + 3] === 1))) {
      const start = bytes[i + 2] === 1 ? i + 3 : i + 4;
      if (start < bytes.length) types.push({ type: bytes[start] & 0x1f, offset: start });
      i = start;
    }
  }
  return types;
}

// avcCodecString builds the WebCodecs codec string from the stream's SPS.
function avcCodecString(bytes) {
  const sps = annexBNalTypes(bytes).find(nal => nal.type === 7);
  if (!sps || sps.offset + 3 >= bytes.length) return '';
  const hex = value => value.toString(16).padStart(2, '0').toUpperCase();
  return 'avc1.' + hex(bytes[sps.offset + 1]) + hex(bytes[sps.offset + 2]) + hex(bytes[sps.offset + 3]);
}

// parseMinFps accepts "0 (lossless)", "30" or "30 fps"; it returns null when invalid.
function parseMinFps(text) {
  const match = /^\s*(\d+)/.exec(String(text));
  if (!match) return null;
  const value = Number(match[1]);
  return value >= 0 && value <= 120 ? value : null;
}

// backoffDelay is the reconnect wait: 0.5 s doubling to a 10 s ceiling.
function backoffDelay(attempt) { return Math.min(10000, 500 * Math.pow(2, Math.max(0, Math.min(attempt, 16)))); }

const ERROR_TEXT = {
  human_active: 'Someone is using the Work PC, so your input was not sent.',
  screen_locked: 'The Work PC is locked.',
  control_not_allowed: 'Control is turned off on the Work PC.',
  control_not_enabled: 'Control is not active. Turn Control on again.'
};

// errorCode extracts the "<code>" of a "<code>: <message>" error.
function errorCode(raw) {
  const match = /^([a-z][a-z0-9_]*):/.exec(String(raw || ''));
  return match ? match[1] : '';
}

// friendlyError turns "<code>: <message>" into one plain sentence.
function friendlyError(raw) {
  const text = String(raw || '').trim();
  if (!text) return 'The request failed.';
  const code = errorCode(text);
  if (ERROR_TEXT[code]) return ERROR_TEXT[code];
  const message = code ? text.slice(code.length + 1).trim() : text;
  if (!message) return 'The request failed (' + code.replace(/_/g, ' ') + ').';
  return message.charAt(0).toUpperCase() + message.slice(1);
}

// ---------------------------------------------------------------- keyboard

// KEY_TABLE maps KeyboardEvent.code (a physical key, by US-layout position)
// to a Windows virtual-key code and its extended-key flag.
const KEY_TABLE = (() => {
  const table = Object.create(null);
  const add = (code, vk, ext) => { table[code] = { vk, ext: ext === true }; };
  for (let i = 0; i < 26; i++) add('Key' + String.fromCharCode(65 + i), 0x41 + i);
  for (let i = 0; i < 10; i++) { add('Digit' + i, 0x30 + i); add('Numpad' + i, 0x60 + i); }
  for (let i = 1; i <= 24; i++) add('F' + i, 0x6F + i);

  add('Backspace', 0x08); add('Tab', 0x09); add('Enter', 0x0D); add('Escape', 0x1B); add('Space', 0x20);

  // Navigation cluster: all extended keys.
  add('PageUp', 0x21, true); add('PageDown', 0x22, true); add('End', 0x23, true); add('Home', 0x24, true);
  add('ArrowLeft', 0x25, true); add('ArrowUp', 0x26, true); add('ArrowRight', 0x27, true); add('ArrowDown', 0x28, true);
  add('Insert', 0x2D, true); add('Delete', 0x2E, true);
  add('Help', 0x2D, true); // The Help key of Apple keyboards sits where Insert is.

  // Numeric keypad.
  add('NumpadMultiply', 0x6A); add('NumpadAdd', 0x6B); add('NumpadSubtract', 0x6D); add('NumpadDecimal', 0x6E);
  add('NumpadDivide', 0x6F, true); add('NumpadEnter', 0x0D, true);
  add('NumpadEqual', 0xBB); add('NumpadComma', 0xBC);
  add('NumLock', 0x90, true); // Also the Clear key of Apple keypads.

  // Punctuation (OEM keys) by US-layout position.
  add('Semicolon', 0xBA); add('Equal', 0xBB); add('Comma', 0xBC); add('Minus', 0xBD); add('Period', 0xBE);
  add('Slash', 0xBF); add('Backquote', 0xC0); add('BracketLeft', 0xDB); add('Backslash', 0xDC);
  add('BracketRight', 0xDD); add('Quote', 0xDE);
  add('IntlBackslash', 0xE2); add('IntlRo', 0xC1); add('IntlYen', 0xDC);

  // Modifiers.
  add('ShiftLeft', 0xA0); add('ShiftRight', 0xA1);
  add('ControlLeft', 0xA2); add('ControlRight', 0xA3, true);
  add('AltLeft', 0xA4); add('AltRight', 0xA5, true);
  add('MetaLeft', 0x5B, true); add('MetaRight', 0x5C, true);
  add('OSLeft', 0x5B, true); add('OSRight', 0x5C, true);

  // Locks and system keys.
  add('CapsLock', 0x14); add('ScrollLock', 0x91); add('Pause', 0x13);
  add('PrintScreen', 0x2C, true); add('ContextMenu', 0x5D, true);

  // Input-method keys.
  add('KanaMode', 0x15); add('Lang1', 0x15); add('Lang2', 0x19); add('Convert', 0x1C); add('NonConvert', 0x1D);

  // Browser and media keys.
  add('BrowserBack', 0xA6, true); add('BrowserForward', 0xA7, true); add('BrowserRefresh', 0xA8, true);
  add('BrowserStop', 0xA9, true); add('BrowserSearch', 0xAA, true); add('BrowserFavorites', 0xAB, true);
  add('BrowserHome', 0xAC, true);
  add('AudioVolumeMute', 0xAD, true); add('AudioVolumeDown', 0xAE, true); add('AudioVolumeUp', 0xAF, true);
  add('MediaTrackNext', 0xB0, true); add('MediaTrackPrevious', 0xB1, true); add('MediaStop', 0xB2, true);
  add('MediaPlayPause', 0xB3, true);
  return table;
})();

const VK_CONTROL = 0x11;
const MODIFIER_VKS = new Set([0x10, 0x11, 0x12, 0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0x5B, 0x5C]);
const CONTROL_VKS = new Set([0x11, 0xA2, 0xA3]);

function isMetaCode(code) { return code === 'MetaLeft' || code === 'MetaRight' || code === 'OSLeft' || code === 'OSRight'; }

// codeToKey returns { vk, ext } for a KeyboardEvent.code, or null when the
// key has no Windows equivalent. With metaAsCtrl, the Command (Meta) keys
// become Control instead of the Windows keys.
function codeToKey(code, metaAsCtrl) {
  if (metaAsCtrl && isMetaCode(code)) return { vk: VK_CONTROL, ext: false };
  const entry = KEY_TABLE[code];
  return entry ? { vk: entry.vk, ext: entry.ext } : null;
}

// keyEvent builds the protocol input event for a resolved key.
function keyEvent(entry, down) {
  if (entry.c !== undefined) return { t: 'char_key', c: entry.c, down };
  const event = { t: 'key', vk: entry.vk, down };
  if (entry.ext) event.ext = true;
  return event;
}

// isControlToggle recognizes Ctrl+Alt+Shift+C, the one combination that is
// never forwarded: it takes or releases control.
function isControlToggle(event) {
  return event.code === 'KeyC' && event.ctrlKey === true && event.altKey === true && event.shiftKey === true && !event.metaKey;
}

// ------------------------------------------------------------------- mouse

const BUTTON_NAMES = ['left', 'middle', 'right', 'x1', 'x2'];       // by MouseEvent.button
const BUTTON_BITS = [1, 4, 2, 8, 16];                                // matching MouseEvent.buttons bits

function buttonName(button) { return BUTTON_NAMES[button] || ''; }

// mapPoint converts a pointer position in CSS client pixels to framebuffer
// pixels. rect is the canvas's bounding client rect, which already reflects
// zoom, scrolling and the canvas's offset in the page.
function mapPoint(clientX, clientY, rect, fbWidth, fbHeight) {
  if (!rect || !(rect.width > 0) || !(rect.height > 0) || !(fbWidth > 0) || !(fbHeight > 0)) return null;
  const x = Math.floor((clientX - rect.left) * fbWidth / rect.width);
  const y = Math.floor((clientY - rect.top) * fbHeight / rect.height);
  return { x: Math.max(0, Math.min(fbWidth - 1, x)), y: Math.max(0, Math.min(fbHeight - 1, y)) };
}

const WHEEL_UNITS_PER_NOTCH = 120, WHEEL_PIXELS_PER_NOTCH = 100, WHEEL_LINES_PER_NOTCH = 3, WHEEL_MAX_UNITS = 120 * 40;

// wheelToNotchUnits converts one wheel delta to 1/120 notches, keeping the
// browser's sign (positive = down or right).
function wheelToNotchUnits(delta, deltaMode) {
  if (!Number.isFinite(delta) || delta === 0) return 0;
  const notches = deltaMode === 1 ? delta / WHEEL_LINES_PER_NOTCH : deltaMode === 2 ? delta : delta / WHEEL_PIXELS_PER_NOTCH;
  return Math.max(-WHEEL_MAX_UNITS, Math.min(WHEEL_MAX_UNITS, notches * WHEEL_UNITS_PER_NOTCH));
}

// wheelDelta converts a wheel event to protocol units. Positive dy scrolls
// up, so the browser's deltaY is negated; dx keeps its direction.
function wheelDelta(event) {
  return { dx: wheelToNotchUnits(event.deltaX, event.deltaMode), dy: -wheelToNotchUnits(event.deltaY, event.deltaMode) || 0 };
}

// drainWheel takes the whole units out of an accumulator and leaves the
// fraction for the next event, so slow trackpad scrolling is not lost.
function drainWheel(acc) {
  const dx = Math.trunc(acc.dx) || 0, dy = Math.trunc(acc.dy) || 0;
  acc.dx -= dx; acc.dy -= dy;
  return { dx, dy };
}

// -------------------------------------------------------------------- zoom

const ZOOM_MIN = .1, ZOOM_MAX = 16;
const ZOOM_STEPS = [.1, .25, .33, .5, .67, .75, .9, 1, 1.1, 1.25, 1.5, 1.75, 2, 2.5, 3, 4, 6, 8, 12, 16];

function clampZoom(value) { return Math.max(ZOOM_MIN, Math.min(ZOOM_MAX, value)); }
function formatZoom(value) { return value === 0 ? 'Auto Fit' : (Math.round(value * 1000) / 10).toString().replace(/\.0$/, '') + '%'; }

// parseZoomText returns 0 for Auto Fit, a scale for 10% through 1600%, or
// null when the text is neither.
function parseZoomText(text) {
  const value = String(text || '').trim();
  if (/^(auto( fit)?|fit)$/i.test(value)) return 0;
  if (!/^\d+(\.\d+)?\s*%?$/.test(value)) return null;
  const percent = Number(value.replace('%', '').trim());
  return Number.isFinite(percent) && percent >= 10 && percent <= 1600 ? percent / 100 : null;
}

function fitScale(viewWidth, viewHeight, width, height) {
  if (!(viewWidth > 0) || !(viewHeight > 0) || !(width > 0) || !(height > 0)) return 1;
  return Math.min(viewWidth / width, viewHeight / height);
}

function nextZoomStep(current, direction) {
  const next = direction > 0 ? ZOOM_STEPS.find(z => z > current * 1.01) : [...ZOOM_STEPS].reverse().find(z => z < current * .99);
  return next || clampZoom(current);
}

// ------------------------------------------------------------------- stats

function formatRate(kbps) {
  if (!Number.isFinite(kbps) || kbps < 0) return '— kbps';
  if (kbps >= 1000) return (kbps / 1000).toFixed(kbps >= 10000 ? 0 : 1) + ' Mbps';
  return Math.round(kbps) + ' kbps';
}

// =================================================================== viewer

function initViewer() {
  const $ = id => document.getElementById(id);
  const view = $('view'), viewport = $('viewport'), screenBox = $('screen'), canvas = $('canvas');
  const cursorImg = $('cursor'), zoomInput = $('zoom'), toasts = $('toasts'), kbdMenu = $('kbdMenu');
  const ctx = canvas.getContext('2d', { alpha: false });

  const isMac = /Mac|iPhone|iPad/i.test(navigator.platform || navigator.userAgent || '');
  const TOGGLE_KEYS = isMac ? '⌃⌥⇧C' : 'Ctrl+Alt+Shift+C';
  const META_SETTING = 'deskferry.screen.metaAsCtrl';
  const SHAPE_PREFIX = 'data:image/png;base64,';

  // Socket and session.
  let ws = null, socketUp = false, everConnected = false, generation = 0, attempt = 0;
  let state = null, stateAt = 0, startPending = false, startTimer = 0, wantLive = true;
  const control = { want: false, confirmed: false };

  // Frames.
  let frame = null, chain = Promise.resolve(), hasFrame = false, lastLossless = false, refreshAt = 0;
  let framesShown = 0, bytesIn = 0, localFps = 0, localKbps = 0;

  // View.
  let zoom = 0, shownScale = 1, gestureBase = 1, drag = null, keyboardLocked = false;

  // Input.
  const held = new Map();          // KeyboardEvent.code -> resolved key that is down on the Work PC
  const buttonsDown = new Set();   // button names that are down on the Work PC
  const wheelAcc = { dx: 0, dy: 0 };
  let pendingMove = null, wheelPoint = null, flushQueued = false, lastLocalMove = 0;
  let metaAsCtrl = true;
  try { metaAsCtrl = localStorage.getItem(META_SETTING) !== '0'; } catch (error) { /* storage is unavailable */ }

  // Remote cursor.
  const cursor = { x: 0, y: 0, hotX: 0, hotY: 0, visible: false, shapeId: null, shapes: new Map() };
  let clipboardWait = null;

  // ------------------------------------------------------------- notices

  const toastTimers = new WeakMap();
  function dismissToast(toast) {
    clearTimeout(toastTimers.get(toast));
    toast.classList.add('leaving');
    setTimeout(() => toast.remove(), 220);
  }
  // notice shows a transient message over the top of the viewport.
  function notice(text, kind) {
    const message = String(text || '').trim();
    if (!message) return;
    let toast = [...toasts.children].find(item => item.dataset.text === message && !item.classList.contains('leaving'));
    if (!toast) {
      toast = document.createElement('div');
      toast.className = 'toast ' + (kind || 'info');
      toast.dataset.text = message;
      toast.textContent = message;
      toasts.appendChild(toast);
      while (toasts.children.length > 3) toasts.firstElementChild.remove();
    }
    // Confirmations leave quickly; warnings and errors stay a little longer.
    clearTimeout(toastTimers.get(toast));
    toastTimers.set(toast, setTimeout(() => dismissToast(toast), kind === 'danger' || kind === 'warning' ? 5000 : 3000));
  }

  // -------------------------------------------------------------- socket

  function send(message) {
    if (!ws || ws.readyState !== 1) return false;
    ws.send(JSON.stringify(message));
    return true;
  }

  function connect() {
    const url = new URL('api/screen/ws', location.href);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    let socket;
    try { socket = new WebSocket(url.href); } catch (error) { scheduleReconnect(); return; }
    socket.binaryType = 'arraybuffer';
    ws = socket;
    socket.onopen = () => {
      if (ws !== socket) return;
      socketUp = true; everConnected = true; attempt = 0;
      generation++; frame = null; state = null;
      if (wantLive) startLive();
      render();
    };
    socket.onmessage = event => {
      if (ws !== socket) return;
      if (typeof event.data === 'string') onText(event.data); else onBinary(event.data);
    };
    socket.onclose = () => {
      if (ws !== socket) return;
      ws = null; socketUp = false;
      generation++; frame = null;
      clearStartPending();
      clearInput();
      failClipboard('The connection to DeskFerry Home was lost.');
      scheduleReconnect();
      render();
    };
    socket.onerror = () => { /* a close event follows */ };
  }

  // Reconnect with bounded exponential backoff (0.5 s doubling to 10 s).
  function scheduleReconnect() { setTimeout(connect, backoffDelay(attempt++)); }

  // Min fps: the frame rate below which the Work PC compresses changing areas;
  // 0 asks for lossless only. Remembered in this browser.
  const MIN_FPS_SETTING = 'deskferry.minFps';
  let minFps = 30;
  try {
    const saved = parseMinFps(localStorage.getItem(MIN_FPS_SETTING) || '');
    if (saved !== null) minFps = saved;
  } catch (error) { /* storage is unavailable */ }
  const minFpsInput = document.getElementById('minFps');
  function formatMinFps(value) { return value === 0 ? '0 (lossless)' : String(value); }
  if (minFpsInput) {
    minFpsInput.value = formatMinFps(minFps);
    minFpsInput.addEventListener('focus', () => minFpsInput.select());
    minFpsInput.addEventListener('keydown', event => { if (event.key === 'Enter') minFpsInput.blur(); });
    minFpsInput.addEventListener('change', () => {
      const value = parseMinFps(minFpsInput.value);
      if (value === null) {
        minFpsInput.value = formatMinFps(minFps);
        return;
      }
      minFps = value;
      minFpsInput.value = formatMinFps(minFps);
      try { localStorage.setItem(MIN_FPS_SETTING, String(minFps)); } catch (error) { /* storage is unavailable */ }
      send({ type: 'min_fps', value: minFps });
    });
  }

  function startLive() {
    wantLive = true;
    if (!send({ type: 'start', min_fps: minFps, video: typeof VideoDecoder === 'function' && typeof EncodedVideoChunk === 'function' })) return;
    startPending = true;
    clearTimeout(startTimer);
    startTimer = setTimeout(() => { startPending = false; render(); }, 6000);
  }
  function clearStartPending() { startPending = false; clearTimeout(startTimer); }

  function stopLive() {
    wantLive = false;
    clearStartPending();
    setControl(false);
    send({ type: 'stop' });
    render();
  }

  function phaseNow() {
    if (!socketUp) return 'offline';
    const phase = (state && state.phase) || 'idle';
    return startPending && (phase === 'idle' || phase === 'stopped' || phase === 'error') ? 'connecting' : phase;
  }
  function liveNow() { const phase = phaseNow(); return phase === 'live' || phase === 'legacy'; }

  function onText(text) {
    let message;
    try { message = JSON.parse(text); } catch (error) { return; }
    if (!message || typeof message !== 'object') return;
    switch (message.type) {
      case 'state': onState(message); break;
      case 'cursor': onCursor(message); break;
      case 'clipboard': onClipboard(message); break;
      case 'error': onError(message); break;
    }
  }

  function onState(next) {
    const previous = state;
    state = next; stateAt = performance.now();
    const phase = next.phase;
    if (phase === 'connecting' || phase === 'live' || phase === 'legacy' || phase === 'error') clearStartPending();

    if (control.want) {
      if (phase === 'legacy') {
        dropControl('Read-only: the Work PC runs an older DeskFerry.');
      } else if (phase === 'live' && next.control_allowed !== true) {
        dropControl('Control is turned off on the Work PC.');
      } else if (phase === 'live') {
        if (next.control_active) control.confirmed = true;
        else if (control.confirmed && previous && previous.phase === 'live') dropControl('Control was ended by the Work PC.');
        else if (!previous || previous.phase !== 'live') { control.confirmed = false; send({ type: 'control', enable: true }); }
      } else {
        // The session is reconnecting or stopped: nothing is held any more.
        control.confirmed = false;
        clearInput();
      }
    }
    render();
  }

  function onError(message) {
    const raw = String(message.error || '');
    const code = errorCode(raw);
    const inputOnly = code === 'human_active' || code === 'screen_locked';
    if (control.want && (code === 'control_not_allowed' || code === 'control_not_enabled' || (!control.confirmed && !inputOnly))) {
      control.want = false; control.confirmed = false;
      clearInput();
    }
    if (code !== 'human_active') failClipboard('');
    notice(friendlyError(raw), 'danger');
    render();
  }

  // -------------------------------------------------------------- frames

  function onBinary(data) {
    const message = parseBinaryMessage(data);
    if (!message) return;
    bytesIn += data.byteLength;
    if (message.kind === 'start') {
      frame = { seq: message.seq, width: message.width, height: message.height, keyframe: message.keyframe, rects: [] };
    } else if (message.kind === 'rect') {
      if (frame) frame.rects.push(prepareRect(message));
    } else {
      const done = frame && frame.seq === message.seq ? frame : null;
      const socketGeneration = generation;
      frame = null;
      // Frames are presented strictly in arrival order, one after another.
      chain = chain.then(() => present(done, message, socketGeneration)).catch(error => console.error('screen frame', error));
    }
  }

  // prepareRect starts decoding an image rect immediately; the decoded
  // bitmap is applied later, in order, when its frame is presented.
  function prepareRect(message) {
    const rect = { x: message.x, y: message.y, w: message.w, h: message.h, enc: message.enc, color: null, image: null };
    if (message.enc === ENC_H264) {
      rect.video = new Uint8Array(message.data);
    } else if (message.enc === ENC_SOLID) {
      rect.color = solidColor(message.data);
    } else {
      const type = rectMimeType(message.enc);
      if (type && typeof createImageBitmap === 'function') {
        rect.image = createImageBitmap(new Blob([message.data], { type })).catch(() => null);
      }
    }
    return rect;
  }

  // present draws every rect of one frame in a single task, so the frame
  // appears at once, and only then acknowledges it.
  async function present(done, end, socketGeneration) {
    let failed = 0;
    if (done) {
      const images = await Promise.all(done.rects.map(rect => rect.image));
      const usable = done.width > 0 && done.height > 0 && done.width <= MAX_CANVAS_SIDE && done.height <= MAX_CANVAS_SIDE;
      if (socketGeneration === generation && usable) {
        const resized = !hasFrame || canvas.width !== done.width || canvas.height !== done.height;
        if (resized) {
          canvas.width = done.width; canvas.height = done.height;   // also clears to black
          ctx.imageSmoothingEnabled = false;
        } else if (done.keyframe) {
          ctx.fillStyle = '#000';
          ctx.fillRect(0, 0, canvas.width, canvas.height);
        }
        done.rects.forEach((rect, index) => {
          const image = images[index];
          if (rect.enc === ENC_H264) {
            if (!feedVideo(rect)) failed++;
          } else if (rect.enc === ENC_SOLID && rect.color) {
            ctx.fillStyle = rect.color;
            ctx.fillRect(rect.x, rect.y, rect.w, rect.h);
          } else if (image) {
            ctx.drawImage(image, rect.x, rect.y, rect.w || image.width, rect.h || image.height);
          } else {
            failed++;
          }
        });
        lastLossless = end.lossless;
        framesShown++;
        if (!hasFrame || resized) { hasFrame = true; screenBox.hidden = false; applyZoom(); render(); }
      }
      for (const image of images) if (image && image.close) image.close();
    }
    if (socketGeneration !== generation) return;
    send({ type: 'ack', seq: end.seq });
    if (failed) requestRefresh();
  }

  // Video: one WebCodecs decoder per stream; decoded pictures are drawn into
  // the rect of the access unit they came from.
  let videoDecoder = null;
  let videoRect = null;
  let videoTimestamp = 0;

  function closeVideo() {
    if (videoDecoder) {
      try { videoDecoder.close(); } catch (error) { /* already closed */ }
    }
    videoDecoder = null;
  }

  // feedVideo queues one access unit; it reports false when the picture
  // cannot be shown (no decoder support, or the stream start was missed).
  function feedVideo(rect) {
    if (typeof VideoDecoder !== 'function' || !rect.video || rect.video.length < 2) return false;
    const start = (rect.video[0] & 1) !== 0;
    const payload = rect.video.subarray(1);
    if (start) closeVideo();
    if (!videoDecoder) {
      if (!start) return true; // wait for the next stream; refinement fills the area
      const decoder = new VideoDecoder({
        output: picture => {
          if (videoRect && decoder === videoDecoder) {
            ctx.drawImage(picture, 0, 0, videoRect.w, videoRect.h, videoRect.x, videoRect.y, videoRect.w, videoRect.h);
          }
          picture.close();
        },
        error: () => { if (decoder === videoDecoder) videoDecoder = null; requestRefresh(); },
      });
      try {
        decoder.configure({ codec: avcCodecString(payload) || 'avc1.42E01F', optimizeForLatency: true });
      } catch (error) {
        return false;
      }
      videoDecoder = decoder;
    }
    videoRect = { x: rect.x, y: rect.y, w: rect.w, h: rect.h };
    const key = annexBNalTypes(payload).some(nal => nal.type === 5);
    try {
      videoDecoder.decode(new EncodedVideoChunk({ type: key ? 'key' : 'delta', timestamp: (videoTimestamp += 33333), data: payload }));
    } catch (error) {
      closeVideo();
      return false;
    }
    return true;
  }

  // requestRefresh asks for a full redraw, at most once every two seconds.
  function requestRefresh() {
    const now = performance.now();
    if (now - refreshAt < 2000) return false;
    refreshAt = now;
    return send({ type: 'refresh' });
  }

  // -------------------------------------------------------------- cursor

  function onCursor(message) {
    const id = message.shape_id;
    if (typeof message.shape === 'string' && message.shape.startsWith(SHAPE_PREFIX)) {
      cursor.shapes.delete(id);
      cursor.shapes.set(id, message.shape);
      if (cursorImg.dataset.shape === String(id)) delete cursorImg.dataset.shape;
      if (cursor.shapes.size > 64) cursor.shapes.delete(cursor.shapes.keys().next().value);
    }
    cursor.shapeId = id;
    cursor.visible = message.visible === true || message.visible === 1;
    cursor.hotX = Number(message.hot_x) || 0;
    cursor.hotY = Number(message.hot_y) || 0;
    // While you are moving the pointer yourself, the overlay follows your
    // hand; the Work PC's (slightly older) positions would make it jitter.
    if (!(isControlling() && performance.now() - lastLocalMove < 300)) {
      cursor.x = Number(message.x) || 0;
      cursor.y = Number(message.y) || 0;
    }
    renderCursor();
  }

  function renderCursor() {
    const shape = cursor.shapes.get(cursor.shapeId);
    const show = hasFrame && liveNow() && cursor.visible && !!shape;
    if (show) {
      const key = String(cursor.shapeId);
      if (cursorImg.dataset.shape !== key) { cursorImg.dataset.shape = key; cursorImg.src = shape; }
      cursorImg.style.transform = 'translate(' + (cursor.x - cursor.hotX) * shownScale + 'px,' + (cursor.y - cursor.hotY) * shownScale + 'px) scale(' + shownScale + ')';
    }
    cursorImg.hidden = !show;
    // With the Work PC's cursor drawn under the pointer, hide the Mac's.
    screenBox.classList.toggle('hide-pointer', show && isControlling());
  }
  cursorImg.addEventListener('error', () => {
    cursor.shapes.delete(cursor.shapeId);
    delete cursorImg.dataset.shape;
    renderCursor();
  });

  // ---------------------------------------------------------------- zoom

  function currentScale() { return zoom || fitScale(view.clientWidth, view.clientHeight, canvas.width, canvas.height); }

  function applyZoom() {
    if (!hasFrame) return;
    view.classList.toggle('fit', zoom === 0);
    const scale = currentScale();
    const width = Math.max(1, Math.floor(canvas.width * scale)), height = Math.max(1, Math.floor(canvas.height * scale));
    screenBox.style.width = width + 'px';
    screenBox.style.height = height + 'px';
    screenBox.classList.toggle('pixelated', scale >= 2);
    shownScale = width / canvas.width;
    if (document.activeElement !== zoomInput) zoomInput.value = formatZoom(zoom);
    updatePannable();
    renderCursor();
  }

  function canPan() { return view.scrollWidth > view.clientWidth || view.scrollHeight > view.clientHeight; }
  function updatePannable() { view.classList.toggle('pannable', !isControlling() && canPan()); }

  // setZoom changes the zoom and keeps the anchored point (the pointer, or
  // the middle of the view) over the same pixel of the Work screen.
  function setZoom(value, anchor) {
    if (!hasFrame) { zoom = value === 0 ? 0 : clampZoom(value); zoomInput.value = formatZoom(zoom); return; }
    const box = view.getBoundingClientRect(), before = screenBox.getBoundingClientRect();
    const ax = anchor ? anchor.x : box.left + view.clientWidth / 2, ay = anchor ? anchor.y : box.top + view.clientHeight / 2;
    const rx = before.width ? (ax - before.left) / before.width : .5, ry = before.height ? (ay - before.top) / before.height : .5;
    zoom = value === 0 ? 0 : clampZoom(value);
    applyZoom();
    if (zoom !== 0) {
      const after = screenBox.getBoundingClientRect();
      view.scrollLeft += after.left + rx * after.width - ax;
      view.scrollTop += after.top + ry * after.height - ay;
    }
  }

  function stepZoom(direction) { setZoom(nextZoomStep(currentScale(), direction)); }

  function commitZoomInput() {
    const value = parseZoomText(zoomInput.value);
    if (value === null) { zoomInput.value = formatZoom(zoom); notice('Zoom must be Auto Fit or 10% through 1600%.', 'warning'); return; }
    setZoom(value);
    zoomInput.value = formatZoom(zoom);
  }

  zoomInput.addEventListener('change', commitZoomInput);
  zoomInput.addEventListener('focus', () => zoomInput.select());
  zoomInput.addEventListener('keydown', event => { if (event.key === 'Enter') { commitZoomInput(); zoomInput.blur(); } });
  if (typeof ResizeObserver === 'function') new ResizeObserver(applyZoom).observe(viewport);
  else window.addEventListener('resize', applyZoom);

  function fullscreenElement() { return document.fullscreenElement || document.webkitFullscreenElement || null; }
  async function toggleFullscreen() {
    try {
      if (fullscreenElement()) await (document.exitFullscreen || document.webkitExitFullscreen).call(document);
      else { const root = document.documentElement; await (root.requestFullscreen || root.webkitRequestFullscreen).call(root); }
    } catch (error) { notice('Full screen is not available in this window.', 'warning'); }
  }
  function onFullscreenChange() { $('fullBtn').setAttribute('aria-pressed', fullscreenElement() ? 'true' : 'false'); updateKeyboardLock(); }
  document.addEventListener('fullscreenchange', onFullscreenChange);
  document.addEventListener('webkitfullscreenchange', onFullscreenChange);

  // In full screen, browsers that support the Keyboard Lock API also hand
  // over Escape and other reserved keys while you control the Work PC.
  function updateKeyboardLock() {
    const keyboard = navigator.keyboard;
    if (!keyboard || typeof keyboard.lock !== 'function') return;
    const wanted = !!fullscreenElement() && isControlling();
    if (wanted === keyboardLocked) return;
    keyboardLocked = wanted;
    if (wanted) keyboard.lock().catch(() => { keyboardLocked = false; }); else keyboard.unlock();
  }

  // ------------------------------------------------------------- control

  function isControlling() {
    return control.want && socketUp && !!state && state.phase === 'live' && state.control_allowed === true;
  }

  function controlBlockReason() {
    if (!socketUp) return 'Not connected to DeskFerry Home.';
    const phase = phaseNow();
    if (phase === 'legacy') return 'Read-only: the Work PC runs an older DeskFerry.';
    if (phase !== 'live') return 'Control is available while the live view is running.';
    if (state.control_allowed !== true) return 'Control is turned off on the Work PC.';
    return '';
  }

  function setControl(on) {
    if (on === control.want) return;
    if (on) {
      const reason = controlBlockReason();
      if (reason) { notice(reason, 'warning'); return; }
    } else if (isControlling()) {
      releaseAll();
    }
    control.want = on; control.confirmed = false;
    clearInput();
    send({ type: 'control', enable: on });
    render();
    if (on) view.focus({ preventScroll: true });
  }

  // dropControl ends control locally after the Work side refused or ended it.
  function dropControl(message) {
    control.want = false; control.confirmed = false;
    clearInput();
    notice(message, 'warning');
  }

  function sendInput(events) { return send({ type: 'input', events }); }

  // pushInput sends an event at once, after any pointer move that is still
  // waiting for the next animation frame, so events keep their order.
  function pushInput(event) {
    if (!isControlling()) return;
    if (pendingMove) {
      const move = pendingMove; pendingMove = null;
      sendInput([{ t: 'move', x: move.x, y: move.y }, event]);
    } else {
      sendInput([event]);
    }
  }

  function clearInput() {
    held.clear(); buttonsDown.clear();
    pendingMove = null; wheelPoint = null; wheelAcc.dx = 0; wheelAcc.dy = 0;
  }

  // releaseAll tells the Work PC to release every key and button we hold.
  function releaseAll() {
    clearInput();
    sendInput([{ t: 'release_all' }]);
  }

  // Pointer moves and wheel deltas are coalesced to one message per frame.
  function queueFlush() {
    if (flushQueued) return;
    flushQueued = true;
    requestAnimationFrame(() => {
      flushQueued = false;
      if (!isControlling()) { pendingMove = null; wheelAcc.dx = 0; wheelAcc.dy = 0; return; }
      const events = [];
      if (pendingMove) { events.push({ t: 'move', x: pendingMove.x, y: pendingMove.y }); pendingMove = null; }
      const wheel = drainWheel(wheelAcc);
      if (wheel.dx || wheel.dy) {
        const event = { t: 'wheel', dx: wheel.dx, dy: wheel.dy };
        if (wheelPoint) { event.x = wheelPoint.x; event.y = wheelPoint.y; }
        events.push(event);
      }
      if (events.length) sendInput(events);
    });
  }

  function pointOf(event) { return mapPoint(event.clientX, event.clientY, canvas.getBoundingClientRect(), canvas.width, canvas.height); }

  function trackLocalPointer(point) {
    lastLocalMove = performance.now();
    cursor.x = point.x; cursor.y = point.y;
    renderCursor();
  }

  function sendButton(name, down, point) {
    if (!name || down === buttonsDown.has(name)) return;
    if (down) buttonsDown.add(name); else buttonsDown.delete(name);
    pushInput({ t: 'button', b: name, down, x: point.x, y: point.y });
  }

  screenBox.addEventListener('pointerdown', event => {
    if (!isControlling()) return;
    event.preventDefault();
    view.focus({ preventScroll: true });
    const point = pointOf(event);
    if (!point) return;
    try { screenBox.setPointerCapture(event.pointerId); } catch (error) { /* the pointer is already gone */ }
    trackLocalPointer(point);
    sendButton(buttonName(event.button), true, point);
  });
  screenBox.addEventListener('pointermove', event => {
    if (!isControlling()) return;
    const point = pointOf(event);
    if (!point) return;
    trackLocalPointer(point);
    if (event.button >= 0) {
      // A second button changed while another is held (a chord).
      sendButton(buttonName(event.button), (event.buttons & BUTTON_BITS[event.button]) !== 0, point);
    } else {
      pendingMove = point;
      queueFlush();
    }
  });
  screenBox.addEventListener('pointerup', event => {
    if (!isControlling()) return;
    event.preventDefault();
    const point = pointOf(event);
    if (point) sendButton(buttonName(event.button), false, point);
    try { screenBox.releasePointerCapture(event.pointerId); } catch (error) { /* not captured */ }
  });
  function releaseButtons(event) {
    if (!buttonsDown.size || !isControlling()) { buttonsDown.clear(); return; }
    const point = pointOf(event) || { x: cursor.x, y: cursor.y };
    for (const name of [...buttonsDown]) sendButton(name, false, point);
  }
  screenBox.addEventListener('pointercancel', releaseButtons);
  screenBox.addEventListener('lostpointercapture', releaseButtons);
  // The canvas never shows the browser's context menu, and while you are
  // controlling, clicks must not trigger browser actions (back, autoscroll).
  screenBox.addEventListener('contextmenu', event => event.preventDefault());
  for (const type of ['mousedown', 'mouseup', 'click', 'auxclick', 'dblclick']) {
    screenBox.addEventListener(type, event => { if (isControlling()) event.preventDefault(); });
  }

  // Wheel: zooms the view, or scrolls the Work PC while you control it. A
  // trackpad pinch arrives as a wheel event with ctrlKey set although no
  // Control key is down; that still zooms the view.
  function controlKeyHeld() { for (const entry of held.values()) if (CONTROL_VKS.has(entry.vk)) return true; return false; }
  view.addEventListener('wheel', event => {
    event.preventDefault();
    const pinch = event.ctrlKey && !controlKeyHeld();
    if (isControlling() && !pinch) {
      if (!screenBox.contains(event.target)) return;
      const delta = wheelDelta(event);
      wheelAcc.dx += delta.dx; wheelAcc.dy += delta.dy;
      wheelPoint = pointOf(event);
      queueFlush();
      return;
    }
    if (!hasFrame) return;
    const notches = wheelToNotchUnits(event.deltaY, event.deltaMode) / WHEEL_UNITS_PER_NOTCH;
    setZoom(currentScale() * Math.exp(-notches * (event.ctrlKey ? 1 : .0953)), { x: event.clientX, y: event.clientY });
  }, { passive: false });
  view.addEventListener('gesturestart', event => { event.preventDefault(); gestureBase = currentScale(); }, { passive: false });
  view.addEventListener('gesturechange', event => { event.preventDefault(); if (hasFrame) setZoom(gestureBase * event.scale, { x: event.clientX, y: event.clientY }); }, { passive: false });

  // Drag to pan when the screen is larger than the view and you are not
  // controlling (while controlling, drags belong to the Work PC).
  view.addEventListener('pointerdown', event => {
    if (isControlling() || event.button !== 0 || !canPan()) return;
    drag = { x: event.clientX, y: event.clientY, left: view.scrollLeft, top: view.scrollTop };
    try { view.setPointerCapture(event.pointerId); } catch (error) { /* the pointer is already gone */ }
    view.classList.add('dragging');
  });
  view.addEventListener('pointermove', event => {
    if (!drag) return;
    view.scrollLeft = drag.left - (event.clientX - drag.x);
    view.scrollTop = drag.top - (event.clientY - drag.y);
  });
  function endDrag() { drag = null; view.classList.remove('dragging'); }
  view.addEventListener('pointerup', endDrag);
  view.addEventListener('pointercancel', endDrag);

  // ------------------------------------------------------------ keyboard

  // Keys go to the Work PC while the viewer (or nothing) has focus; a
  // focused toolbar button or field keeps its own keys.
  function keysTargetViewer() {
    const active = document.activeElement;
    return !active || active === document.body || active === document.documentElement || active === view || view.contains(active);
  }

  function tapKey(entry) {
    if (!isControlling()) return;
    sendInput([keyEvent(entry, true), keyEvent(entry, false)]);
  }

  function forwardKeyDown(event) {
    const entry = codeToKey(event.code, metaAsCtrl);
    if (!entry) {
      // No physical key code (some virtual keyboards): let the Work PC
      // resolve the character for its own layout.
      if (!event.code && event.key && [...event.key].length === 1) tapKey({ c: event.key });
      return;
    }
    // macOS reports Caps Lock as a key-down when it turns on and a key-up
    // when it turns off; either one is a full press for the Work PC.
    if (event.code === 'CapsLock' && isMac) { tapKey(entry); return; }
    held.set(event.code, entry);
    pushInput(keyEvent(entry, true));
  }

  function forwardKeyUp(event) {
    if (event.code === 'CapsLock' && isMac) {
      if (isControlling() && keysTargetViewer()) tapKey(KEY_TABLE.CapsLock);
      return;
    }
    const entry = held.get(event.code);
    if (!entry) return;
    held.delete(event.code);
    pushInput(keyEvent(entry, false));
    // macOS delivers no key-up for keys released while Command is down, so
    // release every non-modifier key together with Command.
    if (isMetaCode(event.code)) {
      for (const [code, other] of [...held]) {
        if (MODIFIER_VKS.has(other.vk)) continue;
        held.delete(code);
        pushInput(keyEvent(other, false));
      }
    }
  }

  document.addEventListener('keydown', event => {
    if (isControlToggle(event)) {
      event.preventDefault(); event.stopPropagation();
      if (!event.repeat) setControl(!control.want);
      return;
    }
    if (isControlling()) {
      if (keysTargetViewer()) { event.preventDefault(); forwardKeyDown(event); }
      return;
    }
    if (event.metaKey || event.ctrlKey || event.altKey) return;
    if (event.target instanceof Element && event.target.closest('input, select, textarea, summary, [contenteditable]')) return;
    if (event.key === '+' || event.key === '=') stepZoom(1);
    else if (event.key === '-') stepZoom(-1);
    else if (event.key === '0') setZoom(0);
    else if (event.key.toLowerCase() === 'f') toggleFullscreen();
  }, true);
  document.addEventListener('keyup', event => {
    if (!isControlling()) return;
    if (held.has(event.code) || event.code === 'CapsLock') { event.preventDefault(); forwardKeyUp(event); }
  }, true);

  // Nothing may stay pressed on the Work PC once this window stops
  // receiving the matching key-up or button-up events.
  window.addEventListener('blur', () => { if (isControlling()) releaseAll(); });
  window.addEventListener('pagehide', () => { if (isControlling()) releaseAll(); });
  document.addEventListener('visibilitychange', () => { if (document.hidden && isControlling()) releaseAll(); });
  view.addEventListener('blur', () => { if (isControlling() && (held.size || buttonsDown.size)) releaseAll(); });
  document.addEventListener('focusin', renderFocus);
  document.addEventListener('focusout', () => setTimeout(renderFocus, 0));
  // Closing the window by accident (Command+W meant for the Work PC) asks first.
  window.addEventListener('beforeunload', event => { if (isControlling()) { event.preventDefault(); event.returnValue = ''; } });

  // ----------------------------------------------------------- clipboard

  async function sendClipboard() {
    let text;
    try { text = await navigator.clipboard.readText(); }
    catch (error) { notice('This page could not read the clipboard. Allow clipboard access and try again.', 'danger'); return; }
    if (!text) { notice('The clipboard has no text to send.', 'warning'); return; }
    if (new TextEncoder().encode(text).length > 900000) { notice('The clipboard text is too large to send.', 'danger'); return; }
    if (send({ type: 'clipboard_set', text })) notice('Clipboard sent to the Work PC (' + text.length.toLocaleString('en-US') + ' characters).', 'success');
    else notice('Not connected to DeskFerry Home.', 'danger');
  }

  function failClipboard(message) {
    if (!clipboardWait) return;
    const wait = clipboardWait; clipboardWait = null;
    clearTimeout(wait.timer);
    wait.failure = message || '';    // '' = the failure was already shown
    wait.reject(new Error(message || 'clipboard request failed'));
  }

  function onClipboard(message) {
    if (!clipboardWait) return;
    const text = typeof message.text === 'string' ? message.text : '';
    if (!text) { failClipboard('The Work PC clipboard has no text.'); return; }
    const wait = clipboardWait; clipboardWait = null;
    clearTimeout(wait.timer);
    wait.text = text;
    wait.resolve(text);
  }

  // getClipboard asks for the Work PC's clipboard text. The clipboard write
  // starts inside the click (with a promise for the text) because Safari
  // refuses writes that begin after the click has finished.
  function getClipboard() {
    if (clipboardWait) return;
    const wait = { text: null, failure: null };
    const reply = new Promise((resolve, reject) => { wait.resolve = resolve; wait.reject = reject; });
    const blob = reply.then(text => new Blob([text], { type: 'text/plain' }));
    blob.catch(() => { /* reported below */ });
    wait.timer = setTimeout(() => failClipboard('The Work PC did not return its clipboard.'), 10000);
    clipboardWait = wait;
    if (!send({ type: 'clipboard_get' })) failClipboard('Not connected to DeskFerry Home.');

    const clipboard = navigator.clipboard;
    let write = null;
    if (clipboard && clipboard.write && typeof ClipboardItem === 'function') {
      try {
        write = clipboard.write([new ClipboardItem({ 'text/plain': blob })])
          .catch(error => (wait.text !== null && clipboard.writeText ? clipboard.writeText(wait.text) : Promise.reject(error)));
      } catch (error) { write = null; }
    }
    if (!write) write = reply.then(text => { if (!clipboard || !clipboard.writeText) throw new Error('no clipboard'); return clipboard.writeText(text); });
    write.then(
      () => notice('Work PC clipboard copied to this Mac (' + wait.text.length.toLocaleString('en-US') + ' characters).', 'success'),
      () => {
        if (wait.failure === '') return;
        notice(wait.failure || 'This page could not write to the clipboard. Allow clipboard access and try again.', 'danger');
      });
  }

  // -------------------------------------------------------------- render

  const CHIPS = {
    offline: ['warning', 'Reconnecting'], idle: ['', 'Ready'], connecting: ['warning', 'Connecting'],
    live: ['success live', 'Live'], legacy: ['info', 'Read-only'], error: ['danger', 'Error'], stopped: ['', 'Stopped']
  };
  const DEFAULT_STATUS = {
    idle: 'Ready', connecting: 'Connecting to the Work screen…', live: 'Live', legacy: 'Read-only view',
    error: 'The live view failed.', stopped: 'Live view stopped'
  };

  function renderFocus() { document.body.classList.toggle('keys-captured', keysTargetViewer()); renderHint(); }

  function renderHint() {
    const hint = $('hint'), phase = phaseNow();
    let text = '', muted = false;
    if (isControlling()) {
      text = keysTargetViewer() ? 'You are controlling the Work PC · ' + TOGGLE_KEYS + ' to release' : 'Controlling · click the screen to send keys';
    } else if (phase === 'live' && state.control_allowed !== true) {
      text = 'View only: control is turned off on the Work PC'; muted = true;
    }
    hint.hidden = !text;
    hint.textContent = text;
    hint.className = 'hint' + (muted ? ' muted' : '');
  }

  function renderStats() {
    const live = liveNow();
    const stats = (state && state.stats) || {};
    // The agent's figures are used while they are fresh; otherwise the
    // page's own count of frames shown and bytes received stands in.
    const fresh = performance.now() - stateAt < 2500;
    const pick = (value, local) => (fresh && Number.isFinite(value) ? value : local);
    const quality = Number(stats.quality);
    $('statSize').textContent = hasFrame ? canvas.width + ' × ' + canvas.height : '—';
    $('statFps').textContent = live ? Math.round(pick(Number(stats.fps), localFps)) + ' fps' : '— fps';
    $('statRate').textContent = live ? formatRate(pick(Number(stats.kbps), localKbps)) : '— kbps';
    $('statQuality').textContent = !live ? '—' : lastLossless ? 'Lossless' : quality > 0 ? 'Quality ' + Math.round(quality) : '—';
  }

  function render() {
    const info = state || {}, phase = phaseNow(), controlling = isControlling();
    const liveOn = phase === 'connecting' || phase === 'live' || phase === 'legacy';
    document.body.classList.toggle('controlling', controlling);

    const chip = controlling ? ['info live', 'Controlling']
      : phase === 'offline' && !everConnected ? CHIPS.connecting : CHIPS[phase] || CHIPS.idle;
    $('statusChip').className = 'chip ' + chip[0];
    $('statusWord').textContent = chip[1];

    let status;
    if (!socketUp) {
      status = attempt > 4 ? 'DeskFerry Home is not responding. Reopen the screen viewer from the control panel if this continues.'
        : !everConnected ? 'Connecting to DeskFerry Home…'
          : 'Lost the connection to DeskFerry Home. Reconnecting…';
    } else {
      status = (phase === (info.phase || 'idle') && info.status) || DEFAULT_STATUS[phase] || 'Ready';
    }
    $('status').textContent = status;
    $('status').title = status;
    const title = (phase === 'live' || phase === 'legacy') && info.foreground_title ? String(info.foreground_title) : '';
    $('foreground').hidden = !title;
    $('foreground').textContent = title;
    $('foreground').title = title ? 'Active window on the Work PC: ' + title : '';
    $('statusChip').title = status;

    const liveBtn = $('liveBtn');
    liveBtn.setAttribute('aria-pressed', liveOn ? 'true' : 'false');
    liveBtn.title = liveOn ? 'Stop live viewing' : 'Start live viewing';
    $('liveIcon').setAttribute('href', liveOn ? '#i-stop' : '#i-play');

    const controlBtn = $('controlBtn'), reason = control.want ? '' : controlBlockReason();
    controlBtn.setAttribute('aria-pressed', control.want ? 'true' : 'false');
    controlBtn.setAttribute('aria-disabled', reason ? 'true' : 'false');
    controlBtn.title = reason || (control.want ? 'Release control of the Work PC (' + TOGGLE_KEYS + ')' : 'Control the Work PC mouse and keyboard (' + TOGGLE_KEYS + ')');
    $('controlTools').hidden = !controlling;
    $('save').setAttribute('aria-disabled', hasFrame ? 'false' : 'true');

    // Work desktop state.
    const desktop = String(info.desktop || ''), secure = desktop !== '' && desktop.toLowerCase() !== 'default';
    const showLock = (phase === 'live' || phase === 'legacy') && (info.locked === true || secure);
    $('lockStrip').hidden = !showLock;
    if (showLock) {
      $('lockTitle').textContent = info.locked === true ? 'Work PC is locked' : 'Work PC is showing a secure desktop';
      const where = secure ? 'Secure desktop (' + desktop + ').' : 'Secure desktop.';
      $('lockDetail').textContent = info.locked === true
        ? where + (controlling ? ' Use Ctrl+Alt+Del in the toolbar to sign in.' : '')
        : where + ' Input may be limited until it closes.';
    }
    $('legacyStrip').hidden = phase !== 'legacy';

    // Overlay over the viewport while there is nothing live to show.
    const empty = $('empty'), covered = !hasFrame || !(phase === 'live' || phase === 'legacy');
    empty.hidden = !covered;
    if (covered) {
      const waiting = phase === 'offline' || phase === 'connecting' || phase === 'live' || phase === 'legacy';
      empty.className = 'empty' + (hasFrame ? ' scrim' : '') + (waiting ? '' : ' quiet') + (phase === 'error' ? ' error' : '');
      $('emptyText').textContent =
        phase === 'offline' ? status
          : phase === 'error' ? status.replace(/[.\s]+$/, '') + '. Select Live to try again.'
            : phase === 'stopped' || phase === 'idle' ? 'Live view is stopped. Select Live to start it.'
              : phase === 'connecting' ? 'Connecting to the Work computer screen…'
                : 'Waiting for the first frame…';
    }

    renderFocus();
    renderStats();
    renderCursor();
    updatePannable();
    updateKeyboardLock();
  }

  // ------------------------------------------------------------- toolbar

  $('controlKbd').textContent = TOGGLE_KEYS;
  $('menuKbd').textContent = TOGGLE_KEYS;
  $('metaAsCtrl').checked = metaAsCtrl;
  $('metaAsCtrl').addEventListener('change', event => {
    metaAsCtrl = event.target.checked;
    try { localStorage.setItem(META_SETTING, metaAsCtrl ? '1' : '0'); } catch (error) { /* storage is unavailable */ }
  });

  $('liveBtn').addEventListener('click', () => {
    const phase = phaseNow();
    if (phase === 'offline') { notice('Not connected to DeskFerry Home.', 'warning'); return; }
    if (phase === 'connecting' || phase === 'live' || phase === 'legacy') stopLive();
    else { startLive(); render(); }
  });
  $('controlBtn').addEventListener('click', () => setControl(!control.want));
  $('sasBtn').addEventListener('click', () => { if (send({ type: 'sas' })) notice('Ctrl+Alt+Del sent to the Work PC.', 'success'); });
  $('clipSendBtn').addEventListener('click', sendClipboard);
  $('clipGetBtn').addEventListener('click', getClipboard);
  $('zoomIn').addEventListener('click', () => stepZoom(1));
  $('zoomOut').addEventListener('click', () => stepZoom(-1));
  $('fitBtn').addEventListener('click', () => setZoom(0));
  $('refreshBtn').addEventListener('click', () => { if (!liveNow()) notice('Start the live view to refresh the screen.', 'warning'); else requestRefresh(); });
  $('fullBtn').addEventListener('click', toggleFullscreen);
  $('save').addEventListener('click', event => { if (!hasFrame) { event.preventDefault(); notice('There is no screen image to save yet.', 'warning'); } });

  // After a mouse click on a toolbar button, keys go back to the Work PC.
  $('toolbar').addEventListener('click', event => {
    if (event.detail > 0 && isControlling() && event.target.closest('button, a') && !event.target.closest('details')) view.focus({ preventScroll: true });
  });

  // The keyboard popover closes on Escape or a click elsewhere.
  document.addEventListener('pointerdown', event => { if (kbdMenu.open && !kbdMenu.contains(event.target)) kbdMenu.open = false; });
  kbdMenu.addEventListener('keydown', event => {
    if (event.key === 'Escape' && kbdMenu.open) { kbdMenu.open = false; kbdMenu.querySelector('summary').focus(); event.stopPropagation(); }
  });

  // A purely local clock for the page's own fallback statistics.
  setInterval(() => {
    localFps = framesShown; framesShown = 0;
    localKbps = bytesIn * 8 / 1000; bytesIn = 0;
    renderStats();
  }, 1000);

  render();
  connect();
}

if (typeof document !== 'undefined' && typeof window !== 'undefined') {
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', initViewer); else initViewer();
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    parseBinaryMessage, solidColor, rectMimeType, backoffDelay, errorCode, friendlyError,
    KEY_TABLE, codeToKey, keyEvent, isControlToggle, isMetaCode,
    buttonName, mapPoint, wheelToNotchUnits, wheelDelta, drainWheel,
    clampZoom, formatZoom, parseZoomText, fitScale, nextZoomStep, formatRate
  };
}
