'use strict';
// DeskFerry screen viewer: capture or stream the Work screen, with wheel/pinch
// zoom, drag panning, full screen and PNG export. URLs are relative.

const $ = id => document.getElementById(id);
const view = $('view'), stage = $('stage'), image = $('image'), zoomInput = $('zoom');
const ZOOM_STEPS = [.1, .25, .33, .5, .67, .75, .9, 1, 1.1, 1.25, 1.5, 1.75, 2, 2.5, 3, 4, 6, 8, 12, 16];
let seq = 0, zoom = 0, gestureBase = 1, drag = null, mode = '', pollErrors = 0, holdUntil = 0;

function clamp(value) { return Math.max(.1, Math.min(16, value)); }
function effectiveZoom() {
  if (zoom) return zoom;
  if (!image.naturalWidth || !image.naturalHeight) return 1;
  return Math.min(view.clientWidth / image.naturalWidth, view.clientHeight / image.naturalHeight);
}
function formatZoom(value) { return value === 0 ? 'Auto Fit' : (Math.round(value * 1000) / 10).toString().replace(/\.0$/, '') + '%'; }

function applyZoom() {
  if (!image.naturalWidth || !image.naturalHeight) return;
  if (zoom === 0) {
    stage.classList.add('fit');
    stage.style.width = '100%'; stage.style.height = '100%';
    image.style.width = '100%'; image.style.height = '100%';
  } else {
    stage.classList.remove('fit');
    const w = Math.round(image.naturalWidth * zoom), h = Math.round(image.naturalHeight * zoom);
    stage.style.width = Math.max(view.clientWidth, w) + 'px';
    stage.style.height = Math.max(view.clientHeight, h) + 'px';
    image.style.width = w + 'px'; image.style.height = h + 'px';
  }
  zoomInput.value = formatZoom(zoom);
}

function setZoom(value, anchor) {
  const before = image.getBoundingClientRect(), rect = view.getBoundingClientRect();
  const oldWidth = before.width || 1, oldHeight = before.height || 1;
  const ax = anchor ? anchor.x - rect.left : view.clientWidth / 2, ay = anchor ? anchor.y - rect.top : view.clientHeight / 2;
  const rx = (view.scrollLeft + ax) / oldWidth, ry = (view.scrollTop + ay) / oldHeight;
  zoom = value === 0 ? 0 : clamp(value);
  applyZoom();
  if (zoom !== 0) {
    view.scrollLeft = rx * (image.getBoundingClientRect().width || 1) - ax;
    view.scrollTop = ry * (image.getBoundingClientRect().height || 1) - ay;
  }
}

function stepZoom(direction) {
  const current = effectiveZoom();
  const next = direction > 0 ? ZOOM_STEPS.find(z => z > current * 1.01) : [...ZOOM_STEPS].reverse().find(z => z < current * .99);
  setZoom(next || current);
}

function parseZoom() {
  const text = zoomInput.value.trim();
  if (/^(auto( fit)?|fit)$/i.test(text)) { setZoom(0); return; }
  const value = Number(text.replace('%', '').trim());
  if (Number.isFinite(value) && value >= 10 && value <= 1600) setZoom(value / 100);
  else { zoomInput.value = formatZoom(zoom); notice('Zoom must be Auto Fit or 10% through 1600%.'); }
}

zoomInput.addEventListener('change', parseZoom);
zoomInput.addEventListener('focus', () => zoomInput.select());
zoomInput.addEventListener('keydown', event => { if (event.key === 'Enter') { parseZoom(); zoomInput.blur(); } });
view.addEventListener('wheel', event => {
  event.preventDefault();
  const factor = event.ctrlKey ? Math.exp(-event.deltaY * .01) : (event.deltaY < 0 ? 1.1 : 1 / 1.1);
  setZoom(effectiveZoom() * factor, { x: event.clientX, y: event.clientY });
}, { passive: false });
view.addEventListener('gesturestart', event => { event.preventDefault(); gestureBase = effectiveZoom(); }, { passive: false });
view.addEventListener('gesturechange', event => { event.preventDefault(); setZoom(gestureBase * event.scale, { x: event.clientX, y: event.clientY }); }, { passive: false });
view.addEventListener('pointerdown', event => {
  if (view.scrollWidth <= view.clientWidth && view.scrollHeight <= view.clientHeight) return;
  drag = { x: event.clientX, y: event.clientY, left: view.scrollLeft, top: view.scrollTop };
  view.setPointerCapture(event.pointerId);
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
image.addEventListener('load', () => { $('empty').hidden = true; applyZoom(); });
window.addEventListener('resize', applyZoom);

async function toggleFullscreen() {
  if (document.fullscreenElement) await document.exitFullscreen();
  else await document.documentElement.requestFullscreen();
}

function notice(text) { holdUntil = Date.now() + 4000; setStatus(text); }

function setStatus(text) {
  const status = String(text || '');
  $('status').textContent = status;
  $('status').title = status;
  let cls = '', word = 'Ready';
  if (/^streaming/i.test(status)) { cls = 'success live'; word = 'Streaming'; }
  else if (/captured/i.test(status)) { cls = 'success'; word = 'Captured'; }
  else if (/^connected/i.test(status)) { cls = 'success'; word = 'Connected'; }
  else if (/^connecting/i.test(status)) { cls = 'warning'; word = 'Connecting'; }
  else if (/failed|ended|error|must|password/i.test(status)) { cls = 'danger'; word = 'Error'; }
  else if (/stopped/i.test(status)) { cls = ''; word = 'Stopped'; }
  $('statusChip').className = 'chip ' + cls;
  $('statusChip').title = status;
  $('statusWord').textContent = word;
  const empty = $('empty');
  if (!empty.hidden) {
    empty.className = 'empty' + (cls.startsWith('danger') ? ' error' : word === 'Stopped' ? ' idle' : '');
    $('emptyText').textContent = cls.startsWith('danger') ? status : word === 'Stopped' ? 'No screenshot yet. Select Capture once or Start stream.' : 'Capturing the Work computer screen…';
  }
  if (word === 'Stopped' || cls.startsWith('danger')) setMode('');
}

function setMode(value) {
  mode = value;
  $('streamBtn').classList.toggle('active', mode === 'stream');
  $('streamBtn').setAttribute('aria-pressed', mode === 'stream' ? 'true' : 'false');
}

async function start(next) {
  const response = await fetch('api/screen/start', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ mode: next, interval_ms: +$('interval').value })
  });
  if (!response.ok) { notice((await response.text()).trim()); return; }
  setMode(next);
}

async function stop() { await fetch('api/screen/stop', { method: 'POST' }); setMode(''); }

async function poll() {
  try {
    const s = await (await fetch('api/state')).json();
    pollErrors = 0;
    if (Date.now() >= holdUntil) setStatus(s.screen_status);
    if (s.screen_seq && s.screen_seq !== seq) { seq = s.screen_seq; image.src = 'api/screen/frame.png?seq=' + seq; }
  } catch (error) {
    if (++pollErrors === 3) setStatus('Connection to DeskFerry Home failed. Close this window and reopen it from the control panel.');
  }
}

$('captureBtn').addEventListener('click', () => start('single'));
$('streamBtn').addEventListener('click', () => start('stream'));
$('stopBtn').addEventListener('click', stop);
$('interval').addEventListener('change', () => { if (mode === 'stream') start('stream'); });
$('zoomIn').addEventListener('click', () => stepZoom(1));
$('zoomOut').addEventListener('click', () => stepZoom(-1));
$('fitBtn').addEventListener('click', () => setZoom(0));
$('fullBtn').addEventListener('click', toggleFullscreen);
$('save').addEventListener('click', event => { if (!seq) { event.preventDefault(); notice('Capture the screen before saving a PNG.'); } });
document.addEventListener('keydown', event => {
  if (event.target.matches('input, select, textarea')) return;
  if (event.key === '+' || event.key === '=') stepZoom(1);
  else if (event.key === '-') stepZoom(-1);
  else if (event.key === '0') setZoom(0);
  else if (event.key.toLowerCase() === 'f') toggleFullscreen();
});

setInterval(poll, 350);
poll();
start('single');
