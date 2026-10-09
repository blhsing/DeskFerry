'use strict';
// DeskFerry macOS Home control panel. All URLs are relative so the page keeps
// working under the random token path prefix.

const $ = id => document.getElementById(id);
const DEFAULT_RELAYS = ['https://test-officialwebsite.azurewebsites.net/relay', 'http://217.142.228.117/relay'];
const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);

let s = { profiles: [], selected: 0 };
let savedSnapshot = '';
let editingIndex = -1;
let dragIndex = -1;
let lastState = null;
let liveRelayURL = '';
let rawDetails = '';
let deleteArmTimer = 0;

// ---------- helpers ----------

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key === 'class') node.className = value;
    else if (key === 'text') node.textContent = value;
    else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value === true ? '' : value);
  }
  for (const child of children) if (child != null) node.append(child);
  return node;
}

function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('class', 'ico');
  svg.setAttribute('aria-hidden', 'true');
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', '#i-' + name);
  svg.append(use);
  return svg;
}

function iconButton(name, label, onclick, extra) {
  return el('button', { type: 'button', class: 'icon ' + (extra || ''), title: label, 'aria-label': label, onclick }, icon(name));
}

function setChip(node, cls, text) {
  node.className = node.className.split(' ').filter(c => !['success', 'warning', 'danger', 'neutral', 'info'].includes(c)).join(' ') + (cls ? ' ' + cls : '');
  node.textContent = text;
}

function toast(message, kind) {
  const node = el('div', { class: 'toast' + (kind === 'danger' ? ' danger' : ''), role: kind === 'danger' ? 'alert' : 'status', text: message });
  $('toasts').append(node);
  setTimeout(() => { node.classList.add('leaving'); setTimeout(() => node.remove(), 220); }, kind === 'danger' ? 6000 : 3200);
}

function inlineStatus(node, kind, message, ttl) {
  node.className = 'inline-status ' + (kind || '');
  node.textContent = message;
  clearTimeout(node._timer);
  if (ttl) node._timer = setTimeout(() => { node.textContent = ''; }, ttl);
}

async function busy(button, task) {
  button.classList.add('busy');
  button.disabled = true;
  try { return await task(); } finally { button.classList.remove('busy'); button.disabled = false; updateControls(); }
}

async function request(path, options) {
  const response = await fetch(path, options);
  const text = await response.text();
  if (!response.ok) throw new Error(text.trim() || response.status + ' ' + response.statusText);
  try { return JSON.parse(text); } catch (e) { return text; }
}

function postJSON(path, body) {
  return request(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch (e) {
    const area = el('textarea', { style: 'position:fixed;opacity:0' });
    area.value = text;
    document.body.append(area);
    area.select();
    document.execCommand('copy');
    area.remove();
  }
}

function trimSlash(value) { return String(value || '').trim().replace(/\/+$/, ''); }

function hostOf(value) {
  try { return new URL(value).host; } catch (e) { return ''; }
}

// ---------- settings ----------

function profile() { return s.profiles[s.selected]; }

function commit() {
  const p = profile();
  if (!p) return;
  p.name = $('name').value;
  p.room = $('room').value;
  p.windows_user = $('windowsUser').value;
  s.listen_addr = $('listen').value;
  s.proxy = $('proxy').value;
}

function snapshot() {
  return JSON.stringify({
    listen_addr: s.listen_addr, proxy: s.proxy, selected: s.selected,
    profiles: s.profiles.map(p => ({ name: p.name, room: p.room, relay_bases: p.relay_bases, windows_user: p.windows_user || '' }))
  });
}

function refreshDirty() {
  commit();
  const dirty = snapshot() !== savedSnapshot || $('password').value !== '' || $('clear').checked;
  $('dirtyChip').hidden = !dirty;
  $('revertBtn').hidden = !dirty;
}

async function load() {
  s = await request('api/settings');
  if (!Array.isArray(s.profiles)) s.profiles = [];
  s.profiles.forEach(p => { if (!Array.isArray(p.relay_bases)) p.relay_bases = []; });
  editingIndex = -1;
  render();
  savedSnapshot = snapshot();
  refreshDirty();
}

function render() {
  const select = $('profile');
  select.innerHTML = '';
  s.profiles.forEach((p, i) => select.add(new Option(p.name, i, i === s.selected, i === s.selected)));
  const p = profile();
  if (!p) return;
  $('name').value = p.name;
  $('room').value = p.room;
  $('windowsUser').value = p.windows_user || '';
  $('listen').value = s.listen_addr || '';
  $('proxy').value = s.proxy || '';
  setChip($('roomCredChip'), p.has_password ? 'success' : '', p.has_password ? 'Saved' : 'Not saved');
  setChip($('winCredChip'), p.has_windows_login ? 'success' : '', p.has_windows_login ? 'Saved in Keychain' : 'Not saved');
  disarmDelete();
  $('deleteProfileBtn').disabled = s.profiles.length <= 1;
  $('deleteProfileBtn').title = s.profiles.length <= 1 ? 'At least one profile is required' : 'Delete this profile';
  renderRelays();
  updateAddressTile();
}

function renderRelays() {
  const list = $('relays');
  const bases = profile().relay_bases;
  list.innerHTML = '';
  $('relayCount').textContent = (bases.length === 1 ? '1 relay' : bases.length + ' relays') + (bases.length > 1 ? ' · drag to reorder' : '');
  if (!bases.length) {
    list.append(el('li', { class: 'empty', text: 'No relay services yet. Add at least one relay URL below.' }));
    return;
  }
  const room = trimSlash(profile().room);
  bases.forEach((base, i) => {
    const row = el('li', { class: 'relay-row', 'data-index': i });
    const handle = el('span', { class: 'handle', title: 'Drag to reorder', 'aria-hidden': 'true' }, icon('grip'));
    handle.addEventListener('pointerdown', () => { row.draggable = true; });
    handle.addEventListener('pointerup', () => { row.draggable = false; });
    const badge = el('span', { class: 'badge ' + (i === 0 ? 'primary' : 'fallback'), text: i === 0 ? 'Primary' : 'Fallback' });
    row.append(handle);

    if (i === editingIndex) {
      row.classList.add('editing');
      const input = el('input', { type: 'text', 'aria-label': 'Relay service URL', spellcheck: 'false', autocomplete: 'off' });
      input.value = base;
      const save = () => {
        const value = input.value.trim();
        const error = validateRelay(value, i);
        if (error) { relayError(error); input.focus(); return; }
        relayError('');
        bases[i] = value;
        editingIndex = -1;
        renderRelays();
        refreshDirty();
      };
      const cancel = () => { editingIndex = -1; renderRelays(); };
      input.addEventListener('keydown', event => {
        if (event.key === 'Enter') { event.preventDefault(); save(); }
        if (event.key === 'Escape') { event.preventDefault(); cancel(); }
      });
      row.append(input, el('span', { class: 'relay-tools' }, iconButton('check', 'Save relay URL', save), iconButton('x', 'Cancel editing', cancel)));
      list.append(row);
      setTimeout(() => { input.focus(); input.select(); }, 0);
      return;
    }

    const live = liveRelayURL && room && trimSlash(liveRelayURL) === trimSlash(base) + '/' + room;
    const host = el('span', { class: 'host' }, badge, el('span', { class: 'host-name', text: hostOf(base) || 'Invalid URL' }));
    if (live) host.append(el('span', { class: 'live', text: 'In use' }));
    const url = el('span', { class: 'relay-url', ondblclick: () => startEdit(i) }, el('span', { class: 'url', title: base, text: base }), host);
    const up = iconButton('up', 'Move up', () => move(i, -1));
    const down = iconButton('down', 'Move down', () => move(i, 1));
    up.disabled = i === 0;
    down.disabled = i === bases.length - 1;
    const remove = iconButton('trash', 'Remove relay', () => { bases.splice(i, 1); renderRelays(); refreshDirty(); }, 'danger-hover');
    remove.disabled = bases.length <= 1;
    if (bases.length <= 1) remove.title = 'At least one relay is required';
    row.append(url, el('span', { class: 'relay-tools' }, iconButton('edit', 'Edit relay URL', () => startEdit(i)), up, down, remove));

    row.addEventListener('dragstart', event => {
      dragIndex = i;
      row.classList.add('dragging');
      event.dataTransfer.effectAllowed = 'move';
      event.dataTransfer.setData('text/plain', base);
    });
    row.addEventListener('dragend', () => {
      row.draggable = false;
      dragIndex = -1;
      list.querySelectorAll('.relay-row').forEach(r => r.classList.remove('dragging', 'drop-before', 'drop-after'));
    });
    row.addEventListener('dragover', event => {
      if (dragIndex < 0) return;
      event.preventDefault();
      event.dataTransfer.dropEffect = 'move';
      const rect = row.getBoundingClientRect();
      const after = event.clientY > rect.top + rect.height / 2;
      row.classList.toggle('drop-after', after);
      row.classList.toggle('drop-before', !after);
    });
    row.addEventListener('dragleave', () => row.classList.remove('drop-before', 'drop-after'));
    row.addEventListener('drop', event => {
      if (dragIndex < 0) return;
      event.preventDefault();
      const rect = row.getBoundingClientRect();
      let target = i + (event.clientY > rect.top + rect.height / 2 ? 1 : 0);
      const [item] = bases.splice(dragIndex, 1);
      if (dragIndex < target) target--;
      bases.splice(target, 0, item);
      dragIndex = -1;
      renderRelays();
      refreshDirty();
    });
    list.append(row);
  });
}

function relayError(message) {
  const node = $('relayError');
  node.textContent = message || '';
  node.hidden = !message;
  clearTimeout(node._timer);
  if (message) node._timer = setTimeout(() => { node.hidden = true; }, 6000);
}

function validateRelay(value, ignoreIndex) {
  if (!value) return 'Enter a relay service URL.';
  let parsed;
  try { parsed = new URL(value); } catch (e) { return 'Relay URLs look like https://host/relay.'; }
  if (!['http:', 'https:', 'ws:', 'wss:'].includes(parsed.protocol)) return 'Relay URLs must start with https:// or http://.';
  if (profile().relay_bases.some((b, i) => i !== ignoreIndex && trimSlash(b).toLowerCase() === trimSlash(value).toLowerCase())) return 'That relay is already in the list.';
  return '';
}

function startEdit(i) { editingIndex = i; renderRelays(); }

function move(i, delta) {
  const bases = profile().relay_bases;
  const j = i + delta;
  if (j < 0 || j >= bases.length) return;
  [bases[i], bases[j]] = [bases[j], bases[i]];
  renderRelays();
  refreshDirty();
  const buttons = $('relays').children[j]?.querySelectorAll('.relay-tools button');
  if (buttons) (delta < 0 ? buttons[1] : buttons[2])?.focus();
}

function addRelay(event) {
  event.preventDefault();
  const value = $('relayEdit').value.trim();
  const error = validateRelay(value, -1);
  if (error) { relayError(error); $('relayEdit').focus(); return; }
  relayError('');
  profile().relay_bases.push(value);
  $('relayEdit').value = '';
  renderRelays();
  refreshDirty();
}

function addProfile() {
  commit();
  const room = profile()?.room || 'workdesk';
  const bases = profile()?.relay_bases?.length ? profile().relay_bases.slice() : DEFAULT_RELAYS.slice();
  s.profiles.push({ name: 'Work ' + (s.profiles.length + 1), room, relay_bases: bases, has_password: false, windows_user: '', has_windows_login: false });
  s.selected = s.profiles.length - 1;
  render();
  refreshDirty();
  $('name').focus();
  $('name').select();
}

function disarmDelete() {
  clearTimeout(deleteArmTimer);
  const button = $('deleteProfileBtn');
  button.classList.remove('danger');
  button.querySelector('span').textContent = 'Delete';
  button.dataset.armed = '';
}

function deleteProfile() {
  const button = $('deleteProfileBtn');
  if (s.profiles.length <= 1) return;
  if (!button.dataset.armed) {
    button.dataset.armed = '1';
    button.classList.add('danger');
    button.querySelector('span').textContent = 'Confirm delete';
    deleteArmTimer = setTimeout(disarmDelete, 4000);
    return;
  }
  const name = profile().name;
  s.profiles.splice(s.selected, 1);
  s.selected = Math.max(0, s.selected - 1);
  render();
  refreshDirty();
  toast('Removed "' + name + '". Save to apply.');
}

async function save(saveWindowsLogin) {
  if (editingIndex >= 0) { relayError('Finish editing the relay URL first.'); return; }
  commit();
  const status = saveWindowsLogin ? $('winSaveStatus') : $('saveStatus');
  const button = saveWindowsLogin ? $('saveWinBtn') : $('saveBtn');
  const wasRunning = !!(lastState && lastState.running);
  const body = {
    settings: s,
    room_password: $('password').value,
    clear_password: $('clear').checked,
    windows_password: $('windowsPassword').value,
    save_windows_login: saveWindowsLogin,
    clear_windows_login: $('clearWindows').checked
  };
  await busy(button, async () => {
    try {
      await postJSON('api/settings', body);
      $('password').value = '';
      $('clear').checked = false;
      $('windowsPassword').value = '';
      $('clearWindows').checked = false;
      await load();
      inlineStatus(status, 'success', 'Saved', 4000);
      toast(saveWindowsLogin ? 'Windows login saved' : wasRunning ? 'Destination saved. The tunnel restarted with the new settings.' : 'Destination saved');
      state();
    } catch (error) {
      inlineStatus(status, 'danger', error.message);
      toast(error.message, 'danger');
    }
  });
}

// ---------- connection ----------

function tunnelState(x) {
  const text = String(x.tunnel_status || '');
  if (x.running) {
    if (/^connecting/i.test(text)) return { word: 'Starting', cls: 'warning' };
    return { word: 'Running', cls: 'success' };
  }
  if (/^stopped:/i.test(text)) return { word: 'Error', cls: 'danger' };
  return { word: 'Stopped', cls: '' };
}

function parseDetails(text) {
  const result = { raw: text || '', fields: [], map: {}, unavailable: false, empty: !text };
  if (!text) return result;
  if (/^relay status unavailable/i.test(text)) {
    result.unavailable = true;
    result.error = text.replace(/^relay status unavailable:\s*/i, '');
    return result;
  }
  for (const line of text.split('\n')) {
    const at = line.indexOf(': ');
    if (at <= 0) { result.fields.push(['', line]); continue; }
    const key = line.slice(0, at).trim();
    const value = line.slice(at + 2).trim();
    result.fields.push([key, value]);
    result.map[key.toLowerCase()] = value;
  }
  const work = result.map['work agent'] || '';
  result.workOnline = /^online/i.test(work);
  const waiting = /\((\d+) waiting/.exec(work);
  result.waiting = waiting ? +waiting[1] : null;
  const streams = /^(\d+)\s*\((\d+) total\)/.exec(result.map['active rdp streams'] || '');
  result.active = streams ? +streams[1] : 0;
  result.total = streams ? +streams[2] : 0;
  return result;
}

function updateAddressTile() {
  const value = (lastState && lastState.running && /^listening on /i.test(lastState.tunnel_status || ''))
    ? lastState.tunnel_status.replace(/^listening on /i, '')
    : ($('listen').value || s.listen_addr || '127.0.0.1:3390');
  $('tileAddr').textContent = value;
  $('tileAddr').title = value;
}

function renderDetails(d) {
  const box = $('relay');
  box.innerHTML = '';
  if (d.empty) {
    box.append(el('pre', { class: 'diag-raw', text: 'Checking the relay… details appear here once the first status check finishes.' }));
    return;
  }
  if (d.unavailable || !d.fields.some(([k]) => k)) {
    box.append(el('pre', { class: 'diag-raw', text: d.raw }));
    return;
  }
  const grid = el('dl', { class: 'diag-grid' });
  for (const [key, value] of d.fields) {
    if (/^checked$/i.test(key)) continue;
    const dd = el('dd');
    if (/^(work agent|home app)$/i.test(key)) {
      const online = /^online/i.test(value);
      dd.append(el('span', { class: 'chip ' + (online ? 'success' : 'danger'), text: online ? 'Online' : 'Offline' }));
      const rest = value.replace(/^(online|waiting)\s*/i, '').replace(/^\((.*)\)$/, '$1');
      if (rest) dd.append(el('span', { text: rest }));
    } else {
      // Allow URLs to wrap after separators without inserting characters.
      (value.match(/[^/.,:]+[/.,:]*|[/.,:]+/g) || []).forEach((part, n) => { if (n) dd.append(el('wbr')); dd.append(part); });
    }
    grid.append(el('dt', { text: key }), dd);
  }
  box.append(grid);
}

function applyState(x) {
  lastState = x;
  const t = tunnelState(x);
  const d = parseDetails(x.relay_details);
  liveRelayURL = d.map && d.map['relay url'] || '';
  rawDetails = d.raw;

  // Tunnel tile and connection card.
  $('tileTunnel').textContent = t.word;
  setChip($('tileTunnelChip'), t.cls, t.word === 'Running' ? 'Listening' : t.word);
  const tunnelText = String(x.tunnel_status || '');
  const errorText = tunnelText.replace(/^stopped:\s*/i, '');
  const tunnelNote = { Running: tunnelText, Starting: 'Opening the local listener', Error: errorText, Stopped: 'Not listening for Remote Desktop' }[t.word];
  $('tileTunnelNote').textContent = tunnelNote;
  $('tileTunnelBox').title = tunnelNote;
  $('connStatus').className = 'conn-status ' + t.cls;
  $('connHeadline').textContent = { Running: 'Tunnel running', Starting: 'Tunnel starting', Error: 'Tunnel stopped with an error', Stopped: 'Tunnel stopped' }[t.word];
  $('tunnel').textContent = t.word === 'Stopped' ? 'Select Connect to start listening for Remote Desktop.' : t.word === 'Error' ? errorText : tunnelText;

  // Work agent and relay.
  let work, relay;
  if (d.empty) { work = { word: 'Checking', cls: 'warning', note: 'Waiting for the first relay check' }; relay = { word: 'Connecting', cls: 'warning' }; }
  else if (d.unavailable) { work = { word: 'Unknown', cls: '', note: 'Relay unreachable' }; relay = { word: 'Unreachable', cls: 'danger' }; }
  else {
    relay = { word: 'Connected', cls: 'success' };
    work = d.workOnline
      ? { word: 'Online', cls: 'success', note: 'via ' + (hostOf(liveRelayURL) || 'relay') }
      : { word: 'Offline', cls: 'danger', note: 'No Work agent in room ' + (d.map.room || profile()?.room || '') };
  }
  $('tileWork').textContent = work.word;
  setChip($('tileWorkChip'), relay.cls, relay.word === 'Connected' ? 'Relay connected' : relay.word === 'Unreachable' ? 'Relay unreachable' : 'Checking');
  $('tileWorkNote').textContent = work.note;
  $('tileWorkBox').title = 'Work agent ' + work.word.toLowerCase() + ' - ' + work.note;
  setChip($('relayChip'), relay.cls, relay.word);
  $('diagDot').className = 'tab-dot ' + relay.cls;
  $('diagDot').title = 'Relay ' + relay.word.toLowerCase();
  $('diagSummary').textContent = d.unavailable ? d.error : (liveRelayURL ? 'Room ' + (d.map.room || '') + ' via ' + liveRelayURL : 'Room, relay and socket details, refreshed every few seconds.');
  $('diagChecked').textContent = d.map && d.map.checked ? 'Last checked ' + d.map.checked : '';

  // Sessions.
  const active = d.active || 0;
  $('tileSessions').textContent = String(active);
  setChip($('tileSessionsChip'), active ? 'success' : '', active ? active + ' active' : 'No active sessions');
  $('tileSessionsNote').textContent = d.total ? d.total + ' since the relay started' : 'Active Remote Desktop streams';
  $('tileSessionsBox').title = $('tileSessionsNote').textContent;

  updateAddressTile();

  // Overall chip and summary sentence.
  const addr = $('tileAddr').textContent;
  let overall, sentence;
  if (t.word === 'Error') { overall = ['danger', 'Error']; sentence = 'The tunnel stopped: ' + errorText; }
  else if (t.word === 'Stopped') { overall = ['', 'Stopped']; sentence = 'The tunnel is stopped. Select Connect to start it.'; }
  else if (t.word === 'Starting') { overall = ['warning', 'Connecting']; sentence = 'Starting the tunnel…'; }
  else if (d.unavailable) { overall = ['danger', 'Unreachable']; sentence = 'The relay could not be reached. Check the proxy and relay services.'; }
  else if (d.empty) { overall = ['warning', 'Checking']; sentence = 'Listening on ' + addr + '. Checking the Work agent…'; }
  else if (!d.workOnline) { overall = ['danger', 'Offline']; sentence = 'Listening on ' + addr + ', but the Work agent is offline.'; }
  else if (active) { overall = ['success', 'Connected']; sentence = active + (active === 1 ? ' Remote Desktop session is' : ' Remote Desktop sessions are') + ' active through ' + (hostOf(liveRelayURL) || 'the relay') + '.'; }
  else { overall = ['success', 'Connected']; sentence = 'Ready. Open Remote Desktop to connect to ' + addr + '.'; }
  setChip($('overallChip'), overall[0], overall[1]);
  $('summary').textContent = sentence;

  if (editingIndex < 0 && dragIndex < 0 && profile()) {
    const before = $('relays').querySelector('.live')?.closest('.relay-row')?.dataset.index;
    const room = trimSlash(profile().room);
    const now = profile().relay_bases.findIndex(b => liveRelayURL && trimSlash(liveRelayURL) === trimSlash(b) + '/' + room);
    if (String(before ?? -1) !== String(now)) renderRelays();
  }
  renderDetails(d);
  updateControls();
}

function updateControls() {
  const x = lastState;
  const running = !!(x && x.running);
  const button = $('connectBtn');
  if (!button.classList.contains('busy')) {
    button.className = 'lg block ' + (running ? 'stop' : 'primary');
    button.querySelector('use').setAttribute('href', running ? '#i-stop' : '#i-play');
    button.querySelector('span').textContent = running ? 'Stop tunnel' : 'Connect';
    button.setAttribute('aria-label', running ? 'Stop the tunnel' : 'Connect the tunnel');
  }
  const rdp = $('rdpBtn');
  if (!rdp.classList.contains('busy')) {
    rdp.disabled = !running;
    rdp.title = running ? 'Open Microsoft Remote Desktop with this destination' : 'Connect the tunnel first';
  }
}

let stateErrors = 0;
async function state() {
  try {
    applyState(await request('api/state'));
    stateErrors = 0;
  } catch (error) {
    if (++stateErrors === 2) {
      setChip($('overallChip'), 'danger', 'Offline');
      $('summary').textContent = 'The DeskFerry Home agent is not responding. Restart it to reopen this page.';
    }
  }
}

async function connectOrStop() {
  const running = !!(lastState && lastState.running);
  await busy($('connectBtn'), async () => {
    try {
      await postJSON(running ? 'api/stop' : 'api/connect');
      toast(running ? 'Tunnel stopped' : 'Tunnel started');
    } catch (error) { toast(error.message, 'danger'); }
    await state();
  });
}

async function openRDP() {
  await busy($('rdpBtn'), async () => {
    try { await postJSON('api/open-rdp'); toast('Opening Remote Desktop…'); }
    catch (error) { toast(error.message, 'danger'); }
  });
}

// ---------- WinRM ----------

async function runWinRM() {
  const command = $('winrmCommand').value;
  const out = $('winrmOutput');
  if (!command.trim()) { out.className = 'console-out error'; out.textContent = 'Enter a PowerShell command first.'; return; }
  await busy($('runBtn'), async () => {
    out.className = 'console-out muted';
    out.textContent = 'Running on the Work computer…';
    try {
      const result = await postJSON('api/winrm', { command });
      const text = typeof result === 'string' ? result : result.output;
      out.className = 'console-out' + (text ? '' : ' muted');
      out.textContent = text || '(The command finished with no output.)';
    } catch (error) {
      out.className = 'console-out error';
      out.textContent = error.message;
    }
  });
}

// ---------- wiring ----------

$('profile').addEventListener('change', () => {
  if (editingIndex >= 0) editingIndex = -1;
  commit();
  s.selected = +$('profile').value;
  render();
  refreshDirty();
});
['name', 'room', 'listen', 'proxy', 'password'].forEach(id => $(id).addEventListener('input', () => {
  refreshDirty();
  if (id === 'room') renderRelays();
  if (id === 'listen') updateAddressTile();
}));
$('clear').addEventListener('change', refreshDirty);
$('name').addEventListener('change', () => { const o = $('profile').options[s.selected]; if (o) o.text = $('name').value; });
$('relayAddForm').addEventListener('submit', addRelay);
$('addProfileBtn').addEventListener('click', addProfile);
$('deleteProfileBtn').addEventListener('click', deleteProfile);
$('saveBtn').addEventListener('click', () => save(false));
$('saveWinBtn').addEventListener('click', () => save(true));
$('revertBtn').addEventListener('click', async () => {
  $('password').value = '';
  $('clear').checked = false;
  await load();
  inlineStatus($('saveStatus'), 'muted', 'Changes discarded', 3000);
});
$('connectBtn').addEventListener('click', connectOrStop);
$('rdpBtn').addEventListener('click', openRDP);
$('screenBtn').addEventListener('click', () => window.open('screen', 'DeskFerryScreen', 'width=1100,height=760'));
$('runBtn').addEventListener('click', runWinRM);
$('clearOutputBtn').addEventListener('click', () => { const o = $('winrmOutput'); o.className = 'console-out muted'; o.textContent = 'Output from the Work computer appears here.'; });
$('winrmCommand').addEventListener('keydown', event => {
  if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) { event.preventDefault(); runWinRM(); }
});
$('copyAddr').addEventListener('click', async () => { await copyText($('tileAddr').textContent); toast('Copied ' + $('tileAddr').textContent); });
$('copyDiagBtn').addEventListener('click', async () => { await copyText(rawDetails || ''); toast('Relay details copied'); });
$('runKey').textContent = isMac ? '⌘' : 'Ctrl';
const tabs = [[$('tabWinrm'), $('winrmCard')], [$('tabDiag'), $('diagCard')]];
function selectTab(index, focus) {
  tabs.forEach(([tab, pane], i) => {
    const on = i === index;
    tab.setAttribute('aria-selected', on ? 'true' : 'false');
    tab.tabIndex = on ? 0 : -1;
    pane.hidden = !on;
  });
  if (focus) tabs[index][0].focus();
  try { localStorage.setItem('deskferry.tool', tabs[index][1].id); } catch (e) { /* ignore */ }
}
tabs.forEach(([tab], i) => {
  tab.addEventListener('click', () => selectTab(i));
  tab.addEventListener('keydown', event => {
    if (event.key === 'ArrowRight' || event.key === 'ArrowLeft') { event.preventDefault(); selectTab((i + 1) % tabs.length, true); }
  });
});
{
  // #console or #diagnostics in the URL picks the tool tab; otherwise reuse the last one.
  let wanted = { console: 'winrmCard', diagnostics: 'diagCard' }[location.hash.slice(1)] || '';
  if (!wanted) { try { wanted = localStorage.getItem('deskferry.tool') || ''; } catch (e) { wanted = ''; } }
  selectTab(Math.max(0, tabs.findIndex(([, pane]) => pane.id === wanted)));
}
window.addEventListener('beforeunload', event => {
  if (!$('dirtyChip').hidden) { event.preventDefault(); event.returnValue = ''; }
});

load().catch(error => toast('Could not load settings: ' + error.message, 'danger')).finally(state);
setInterval(state, 2000);
