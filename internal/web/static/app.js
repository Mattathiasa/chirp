// Chirp web UI. Vanilla ES modules, no build step, no dependencies.
// Security note: every piece of peer-controlled text (names, messages) is
// inserted with textContent, never innerHTML. The CSP forbids inline script.

const $app = document.getElementById('app');

// ---------- tiny DOM helper ----------
function h(tag, props = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === false || v == null) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'text') el.textContent = v;
    else if (k === 'style') el.style.cssText = v; // CSSOM, allowed under a strict style-src
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const k of kids.flat()) if (k != null && k !== false) el.append(k.nodeType ? k : document.createTextNode(String(k)));
  return el;
}

// Icons are static constants (no user data), so innerHTML on them is safe.
const ICONS = {
  send: '<path d="M12 19V5M5 12l7-7 7 7"/>',
  back: '<path d="M15 5l-7 7 7 7"/>',
  shield: '<path d="M12 3l8 3v6c0 4.5-3.2 8-8 9-4.8-1-8-4.5-8-9V6z"/><path d="M8.5 12l2.5 2.5L16 9.5"/>',
  lock: '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  check: '<path d="M5 12.5l4.5 4.5L19 7.5"/>',
  checks: '<path d="M2 12.5l4.5 4.5L16 7.5M11 16.5l1 .5L23 7.5"/>',
  people: '<circle cx="9" cy="8" r="3.5"/><path d="M2.5 20c.5-3.5 3-5.5 6.5-5.5s6 2 6.5 5.5"/><path d="M16 4.7a3.5 3.5 0 0 1 0 6.6M18 14.8c2 .6 3.3 2.4 3.5 5.2"/>',
  wifi: '<path d="M2 9a15 15 0 0 1 20 0M5 12.5a10.5 10.5 0 0 1 14 0M8.5 16a5.5 5.5 0 0 1 7 0"/><circle cx="12" cy="19.5" r="1" fill="currentColor"/>',
  gear: '<circle cx="12" cy="12" r="3"/><path d="M12 2v3M12 19v3M4.2 4.2l2.1 2.1M17.7 17.7l2.1 2.1M2 12h3M19 12h3M4.2 19.8l2.1-2.1M17.7 6.3l2.1-2.1"/>',
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8v.01"/>',
  key: '<circle cx="8" cy="15" r="4"/><path d="M11 12l9-9M16 7l3 3"/>',
  warn: '<path d="M12 3l10 18H2z"/><path d="M12 10v5M12 18v.01"/>',
  x: '<path d="M6 6l12 12M18 6L6 18"/>',
  refresh: '<path d="M20 12a8 8 0 1 1-2.3-5.7M20 4v5h-5"/>',
  search: '<circle cx="11" cy="11" r="7"/><path d="M20 20l-4-4"/>',
  trash: '<path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13"/>',
  spin: '<circle cx="12" cy="12" r="9" stroke-opacity=".25"/><path d="M12 3a9 9 0 0 1 9 9"/>',
};
function icon(name, size = 20, cls = '') {
  const s = h('span', { class: cls, style: `display:inline-flex;width:${size}px;height:${size}px;flex:none` });
  s.innerHTML = `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name]}</svg>`;
  return s;
}

// ---------- API ----------
async function api(method, path, body) {
  const r = await fetch(path, {
    method,
    headers: { 'X-Chirp': '1', ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}) },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (!r.ok) {
    let msg = r.statusText;
    try { msg = (await r.json()).error || msg; } catch { /* not JSON */ }
    const e = new Error(msg); e.status = r.status; throw e;
  }
  return r;
}
const getJSON = async (p) => (await api('GET', p)).json();
const enc = encodeURIComponent;

// ---------- state ----------
const S = {
  setup: null, me: null, peers: [], cur: null, msgs: [], view: 'chat', panel: false,
  settings: { retentionDays: 0, notify: true, previews: false },
  outbox: [], diag: null, q: '', connected: true, unread: {}, modal: null, draft: {},
};
const peerByName = (n) => S.peers.find((p) => p.name.toLowerCase() === (n || '').toLowerCase());
const curPeer = () => peerByName(S.cur);

// ---------- formatting & visuals ----------
const pad = (n) => String(n).padStart(2, '0');
function fmtTime(ts) {
  const d = new Date(ts);
  const t = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  return d.toDateString() === new Date().toDateString() ? t : `${d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })} ${t}`;
}
// Avatar fills come from a fixed editorial palette so every key gets a bold, on-brand colour.
const TINTS = ['#5EE2A4', '#FFD84A', '#FF9EC7', '#C7B8FF', '#FF5A36', '#8FA0FF'];
const hue = (seed) => (seed && seed.length > 5 ? TINTS[seed[5] % TINTS.length] : TINTS[1]);
function initials(name) {
  const p = name.trim().split(/\s+/);
  return ((p[0]?.[0] || '?') + (p.length > 1 ? p[p.length - 1][0] : '')).toUpperCase();
}
function b64(s) { // Go encodes []byte as base64
  if (!s) return null;
  try { return Uint8Array.from(atob(s), (c) => c.charCodeAt(0)); } catch { return null; }
}
function avatar(p, size = 46, pres = true) {
  const seed = b64(p.identicon);
  const hh = hue(seed);
  const el = h('span', {
    class: 'avatar',
    style: `width:${size}px;height:${size}px;border-radius:${Math.round(size * 0.34)}px;background:${hh};color:#0C0C14;font-size:${Math.round(size * 0.36)}px`,
    'aria-hidden': 'true',
  }, initials(p.name));
  if (pres) el.append(h('span', { class: 'pres ' + (p.online ? 'on' : p.nearby ? 'wait' : '') }));
  return el;
}
function identicon(seedB64, size = 96) {
  const seed = b64(seedB64) || new Uint8Array(8);
  const hh = hue(seed), ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', '0 0 5 5'); svg.setAttribute('width', size); svg.setAttribute('height', size);
  svg.setAttribute('class', 'identicon'); svg.setAttribute('role', 'img'); svg.setAttribute('aria-label', 'Identicon derived from the key');
  const bg = document.createElementNS(ns, 'rect');
  bg.setAttribute('width', 5); bg.setAttribute('height', 5); bg.setAttribute('fill', hh);
  svg.append(bg);
  for (let y = 0; y < 5; y++) for (let x = 0; x < 3; x++) {
    if (((seed[y] >> x) & 1) === 0) continue;
    for (const xx of new Set([x, 4 - x])) {
      const r = document.createElementNS(ns, 'rect');
      r.setAttribute('x', xx); r.setAttribute('y', y); r.setAttribute('width', 1); r.setAttribute('height', 1);
      r.setAttribute('fill', '#0C0C14'); svg.append(r);
    }
  }
  return svg;
}
function trustChip(p) {
  if (p.trust === 'verified') return h('span', { class: 'chip ok' }, icon('shield', 13), 'Verified');
  if (p.trust === 'changed') return h('span', { class: 'chip bad' }, icon('warn', 13), 'Key changed');
  if (p.trust === 'unknown') return h('span', { class: 'chip' }, 'Nearby');
  return h('span', { class: 'chip warn' }, 'Unverified');
}

// ---------- toasts & modals ----------
let toastTimer;
function toast(msg, bad = false) {
  document.querySelector('.toast')?.remove();
  const t = h('div', { class: 'toast' + (bad ? ' bad' : ''), role: 'status' }, msg);
  document.body.append(t);
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.remove(), 3500);
}
function modal(build) {
  const bg = h('div', { class: 'modal-bg', onclick: (e) => { if (e.target === bg) close(); } });
  const close = () => { bg.remove(); document.removeEventListener('keydown', esc); };
  const esc = (e) => { if (e.key === 'Escape') close(); };
  document.addEventListener('keydown', esc);
  const box = h('div', { class: 'modal', role: 'dialog', 'aria-modal': 'true' });
  build(box, close);
  bg.append(box);
  document.body.append(bg);
  box.querySelector('input,button,textarea')?.focus();
}
function confirmBox(title, body, action, okLabel = 'Confirm', danger = true) {
  modal((box, close) => {
    box.append(h('h1', {}, title), h('p', { class: 'muted' }, body),
      h('div', { style: 'display:flex;gap:10px;justify-content:flex-end' },
        h('button', { class: 'btn', onclick: close }, 'Cancel'),
        h('button', { class: 'btn ' + (danger ? 'danger' : 'primary'), onclick: async () => { close(); try { await action(); } catch (e) { toast(e.message, true); } } }, okLabel)));
  });
}

// ---------- data loading ----------
let peersTimer;
function refreshPeersSoon() { clearTimeout(peersTimer); peersTimer = setTimeout(loadPeers, 80); }
async function loadPeers() {
  try {
    const next = await getJSON('/api/peers');
    const sig = JSON.stringify(next);
    if (sig === S.peersSig) return; // nothing changed: do not rebuild the DOM under the user's cursor
    S.peersSig = sig; S.peers = next;
    if (S.cur && !curPeer()) { S.cur = null; S.msgs = []; }
    renderSide(); renderMain(); renderPanel();
  } catch { /* SSE status handles connectivity */ }
}
async function loadMessages() {
  if (!S.cur) return;
  try { S.msgs = await getJSON(`/api/peers/${enc(S.cur)}/messages`); renderMain(true); } catch { /* ignore */ }
}
async function loadOutbox() { try { S.outbox = await getJSON('/api/outbox'); } catch { /* ignore */ } }
async function loadDiag() { try { S.diag = await getJSON('/api/diagnostics'); } catch { /* ignore */ } }
async function loadSettings() { try { S.settings = await getJSON('/api/settings'); } catch { /* ignore */ } }

// ---------- live events ----------
let es;
function connectEvents() {
  es = new EventSource('/api/events');
  es.onopen = () => { S.connected = true; renderMain(); loadPeers(); loadMessages(); };
  es.onerror = () => { S.connected = false; renderMain(); };
  es.onmessage = (ev) => {
    let e; try { e = JSON.parse(ev.data); } catch { return; }
    if ((e.type === 'message' || e.type === 'status') && e.message) {
      const m = e.message;
      const isCur = S.cur && e.peer && e.peer.toLowerCase() === S.cur.toLowerCase();
      if (isCur) {
        const i = S.msgs.findIndex((x) => x.id === m.id);
        if (i >= 0) S.msgs[i] = m; else S.msgs.push(m);
        renderMain(true);
      }
      if (e.type === 'message' && m.dir === 'in') {
        if (!isCur || document.hidden) S.unread[e.peer.toLowerCase()] = (S.unread[e.peer.toLowerCase()] || 0) + 1;
        notify(e.peer, m.body);
      }
      if (S.view !== 'chat') { if (S.view === 'network') refreshNetworkSoon(); }
    }
    if (e.type === 'outbox' && S.view === 'network') refreshNetworkSoon();
    refreshPeersSoon();
  };
}
function notify(peer, body) {
  if (!document.hidden || !S.settings.notify || !('Notification' in window) || Notification.permission !== 'granted') return;
  new Notification(peer, { body: S.settings.previews ? body : 'New message', tag: 'chirp-' + peer });
}
let netTimer;
function refreshNetworkSoon() { clearTimeout(netTimer); netTimer = setTimeout(async () => { await Promise.all([loadOutbox(), loadDiag()]); if (S.view === 'network') renderMain(); }, 200); }

// ---------- layout ----------
let $shell, $rail, $nav, $side, $main, $panel, composer, $ta, $sendBtn, $search;

function mountShell() {
  $app.replaceChildren();
  $rail = h('nav', { class: 'rail', 'aria-label': 'Sections' });
  $nav = h('aside', { class: 'nav pane', 'aria-label': 'Navigation' });
  $side = h('aside', { class: 'side pane', 'aria-label': 'Conversations' });
  $main = h('main', { class: 'main pane' });
  $panel = h('aside', { class: 'panel', 'aria-label': 'Trust details' });
  $search = h('input', { type: 'search', class: 'search', placeholder: 'Search', 'aria-label': 'Search people', autocomplete: 'off' });
  $search.addEventListener('input', () => { S.q = $search.value; renderSide(); });
  $shell = h('div', { class: 'stage' }, $rail, h('div', { class: 'window' }, $nav, $side, $main, $panel));
  $app.append($shell);
  buildComposer();
  renderRail(); renderNav(); renderSide(); renderMain(); renderPanel();
  setInterval(loadPeers, 10000);
}

function buildComposer() {
  $ta = h('textarea', { rows: 1, 'aria-label': 'Message', placeholder: 'Message', maxlength: 4000 });
  $sendBtn = h('button', { class: 'send', 'aria-label': 'Send message', type: 'button' }, icon('send', 22));
  const grow = () => { $ta.style.height = 'auto'; $ta.style.height = Math.min($ta.scrollHeight, 140) + 'px'; };
  $ta.addEventListener('input', () => { grow(); if (S.cur) S.draft[S.cur] = $ta.value; syncSend(); });
  $ta.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); sendMsg(); } });
  $sendBtn.addEventListener('click', sendMsg);
  composer = h('div', { class: 'composer' }, $ta, $sendBtn);
}
function syncSend() { $sendBtn.disabled = $ta.disabled || !$ta.value.trim(); }

async function sendMsg() {
  const body = $ta.value.trim();
  if (!body || !S.cur || $ta.disabled) return;
  $ta.value = ''; S.draft[S.cur] = ''; $ta.style.height = 'auto'; syncSend();
  try {
    const m = await (await api('POST', `/api/peers/${enc(S.cur)}/messages`, { body })).json();
    if (!S.msgs.find((x) => x.id === m.id)) S.msgs.push(m);
    renderMain(true);
  } catch (e) { $ta.value = body; syncSend(); toast(e.message, true); }
}

const queuedCount = () => S.peers.reduce((a, p) => a + (p.queued || 0), 0);
const unreadCount = () => Object.values(S.unread).reduce((a, n) => a + n, 0);

// ---------- floating icon rail (far left) ----------
function renderRail() {
  if (!$rail) return;
  const q = queuedCount();
  const btn = (ic, label, on, fn, badge) => h('button', { class: 'rbtn', 'aria-label': label, title: label, 'aria-pressed': String(on), onclick: fn }, icon(ic, 22), badge ? h('span', { class: 'rdot' }, badge) : null);
  $rail.replaceChildren(
    h('span', { class: 'logo' }, logoSvg()),
    btn('people', 'Chats', S.view === 'chat', () => setView('chat'), unreadCount() || null),
    btn('wifi', 'Network and outbox', S.view === 'network', () => setView('network'), q || null),
    btn('gear', 'Settings', S.view === 'settings', () => setView('settings')),
    h('span', { class: 'grow' }),
    S.me ? h('button', { class: 'rbtn', 'aria-label': 'My identity', title: 'My identity', onclick: showIdentity }, avatar({ name: S.me.name, identicon: S.me.identicon }, 36, false)) : null);
}

// ---------- left pane: profile + navigation ----------
function renderNav() {
  if (!$nav) return;
  const q = queuedCount(), un = unreadCount();
  const item = (ic, label, on, fn, badge) => h('button', { class: 'nitem', 'aria-current': String(on), onclick: fn },
    icon(ic, 20), h('span', { class: 'grow' }, label), badge ? h('span', { class: 'nbadge' }, badge) : null);
  $nav.replaceChildren(
    S.me ? h('div', { class: 'prof' },
      avatar({ name: S.me.name, identicon: S.me.identicon }, 44, false),
      h('button', { class: 'pname', onclick: showIdentity, 'aria-label': 'My identity' }, h('b', {}, S.me.name), h('small', {}, S.me.fp.slice(0, 8) + ' … ' + S.me.fp.slice(-4))),
      h('button', { class: 'iconbtn sm', 'aria-label': 'Settings', onclick: () => setView('settings') }, icon('gear', 18))) : null,
    h('div', { class: 'nlist' },
      item('people', 'Chats', S.view === 'chat', () => setView('chat'), un || null),
      item('wifi', 'Network', S.view === 'network', () => setView('network'), q || null),
      item('gear', 'Settings', S.view === 'settings', () => setView('settings'))),
    h('div', { class: 'nsec' }, 'Identity'),
    h('div', { class: 'nlist' },
      item('info', 'My fingerprint', false, showIdentity),
      item('key', 'Back up key', false, showBackup)),
    h('div', { class: 'grow' }),
    h('div', { class: 'brand' }, h('span', { class: 'logo sm' }, logoSvg()), 'Chirp', h('span', { class: 'muted ver' }, 'no server')));
}

// ---------- middle pane: search, online strip, conversations ----------
function renderSide() {
  renderRail(); renderNav();
  if (!$side) return;
  const needle = (S.q || '').trim().toLowerCase();
  const match = (p) => !needle || p.name.toLowerCase().includes(needle);
  const people = S.peers.filter((p) => p.trust !== 'unknown' && match(p));
  const nearby = S.peers.filter((p) => p.trust === 'unknown' && match(p));
  const online = S.peers.filter((p) => p.online && match(p));
  const rowFor = (p) => {
    const un = S.unread[p.name.toLowerCase()] || 0;
    const preview = p.trust === 'changed' ? 'Key changed. Review before chatting.'
      : p.last ? (p.last.dir === 'out' ? 'You: ' : '') + (S.settings.previews || p.last.dir === 'out' ? p.last.body : 'New message')
      : p.trust === 'unknown' ? 'Connecting securely…' : 'No messages yet';
    return h('button', { class: 'row', 'aria-current': S.view === 'chat' && S.cur === p.name ? 'true' : 'false', onclick: () => openPeer(p.name) },
      avatar(p),
      h('span', { class: 'grow' },
        h('span', { class: 'name' }, p.name, p.trust === 'verified' ? h('span', { style: 'color:var(--ok);display:inline-flex' }, icon('shield', 15)) : null),
        h('span', { class: 'sub' + (p.trust === 'changed' ? ' warn' : '') }, preview)),
      h('span', { class: 'meta' }, p.last ? fmtTime(p.last.ts) : '', un ? h('span', { class: 'badge' }, un) : p.queued ? h('span', { class: 'chip warn' }, icon('clock', 12), p.queued) : null));
  };
  const strip = online.length ? h('div', { class: 'strip', 'aria-label': 'Online now' }, online.map((p) =>
    h('button', { class: 'sitem', onclick: () => openPeer(p.name), 'aria-label': `Open chat with ${p.name}` }, avatar(p, 52), h('span', {}, p.name.split(' ')[0])))) : null;

  $side.replaceChildren(
    h('div', { class: 'sh' }, h('h1', {}, 'Chats'),
      h('span', { class: 'chip' }, `${S.peers.filter((p) => p.online).length} online`),
      h('span', { class: 'grow' }),
      h('span', { class: 'narrow-only' },
        h('button', { class: 'iconbtn sm', 'aria-label': 'Network and outbox', onclick: () => setView('network') }, icon('wifi', 18)),
        h('button', { class: 'iconbtn sm', 'aria-label': 'Settings', onclick: () => setView('settings') }, icon('gear', 18)),
        S.me ? h('button', { class: 'iconbtn sm', 'aria-label': 'My identity', onclick: showIdentity }, icon('key', 18)) : null)),
    h('div', { class: 'searchwrap' }, icon('search', 18), $search),
    strip,
    h('div', { class: 'list' },
      people.length ? [h('div', { class: 'section' }, 'Your people'), ...people.map(rowFor)] : null,
      nearby.length ? [h('div', { class: 'section' }, 'Nearby'), ...nearby.map(rowFor)] : null,
      !S.peers.length ? h('div', { class: 'empty' },
        h('div', { class: 'art' }, h('span', { style: 'color:var(--accent)' }, icon('wifi', 44))),
        h('h3', {}, 'Looking for people'),
        h('p', {}, 'Open Chirp on another device on this Wi-Fi. Anyone nearby appears here within a few seconds.')) : null,
      S.peers.length && !people.length && !nearby.length ? h('div', { class: 'empty' }, h('p', {}, `No one matches "${S.q}".`)) : null));
  $shell.classList.toggle('chat-open', S.view !== 'chat' || !!S.cur);
}
function logoSvg() {
  const s = h('span', { style: 'display:inline-flex' });
  s.innerHTML = '<svg width="22" height="22" viewBox="0 0 32 32" fill="none" stroke="#0C0C14" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"><path d="M9 20c0-5 3-9 8-9 3 0 5 2 5 4s-1 3-3 3c-1 0-2-.6-2-1.6M22 15l3-1-2 3"/></svg>';
  return s;
}

async function setView(v) {
  S.view = v;
  if (v === 'network') await Promise.all([loadOutbox(), loadDiag()]);
  if (v === 'settings') await loadSettings();
  renderSide(); renderMain(); renderPanel();
}
async function openPeer(name) {
  S.cur = name; S.view = 'chat'; S.unread[name.toLowerCase()] = 0; S.msgs = [];
  $ta.value = S.draft[name] || '';
  renderSide(); renderMain(); renderPanel();
  await loadMessages();
  $ta.focus();
}

// ---------- main pane ----------
function renderMain(keepScroll) {
  if (!$main) return;
  const prev = $main.querySelector('.msgs');
  const atBottom = !prev || prev.scrollHeight - prev.scrollTop - prev.clientHeight < 80;
  const banner = !S.connected ? h('div', { class: 'banner conn', role: 'alert' }, icon('spin', 18, 'spin'),
    h('div', { class: 'grow' }, 'Lost contact with the Chirp daemon. Retrying… Your messages are safe on disk.')) : null;

  if (S.view === 'network') return $main.replaceChildren(...[banner, viewNetwork()].filter(Boolean));
  if (S.view === 'settings') return $main.replaceChildren(...[banner, viewSettings()].filter(Boolean));

  const p = curPeer();
  if (!p) {
    return $main.replaceChildren(...[banner, h('div', { class: 'empty' },
      h('div', { class: 'art' }, h('span', { style: 'color:var(--accent)' }, icon('lock', 44))),
      h('h3', {}, 'Pick someone to talk to'),
      h('p', {}, 'Messages travel directly between devices on this network, encrypted end to end. There is no server.'))].filter(Boolean));
  }

  const status = p.online ? 'Online' : p.nearby ? 'Nearby · connecting' : 'Offline';
  const head = h('header', { class: 'head' },
    h('button', { class: 'back', 'aria-label': 'Back to chats', onclick: () => { S.cur = null; S.panel = false; renderSide(); renderMain(); renderPanel(); } }, icon('back', 24)),
    avatar(p, 56),
    h('div', { class: 'grow' }, h('h2', {}, p.name), h('div', { class: 'st' + (p.online ? ' on' : '') }, status), h('div', { style: 'margin-top:6px' }, trustChip(p))),
    h('div', { class: 'pill' },
      h('button', { class: 'pbtn', 'aria-label': 'Trust and fingerprint', title: 'Trust and fingerprint', 'aria-pressed': String(S.panel), onclick: () => { S.panel = !S.panel; renderPanel(); } }, icon('shield', 20)),
      h('button', { class: 'pbtn', 'aria-label': 'Retry queued messages', title: 'Retry queued', onclick: async () => { await api('POST', `/api/peers/${enc(p.name)}/retry`); toast('Retrying'); } }, icon('refresh', 20)),
      h('button', { class: 'pbtn red', 'aria-label': 'Forget this person', title: 'Forget this person', onclick: () => confirmBox(`Forget ${p.name}?`, 'Deletes the pinned key and every message with them on this device. They keep their own copy.', async () => { await api('DELETE', `/api/peers/${enc(p.name)}`); S.cur = null; S.panel = false; await loadPeers(); }, 'Forget') }, icon('trash', 20))));

  let notice = null;
  if (p.trust === 'changed') {
    notice = h('div', { class: 'banner bad', role: 'alert' }, icon('warn', 22),
      h('div', { class: 'grow' }, h('b', {}, `${p.name}'s key changed.`), ' Someone else may be using this name, or they reinstalled Chirp. Sending is blocked until you review it.'),
      h('button', { class: 'btn', onclick: () => { S.panel = true; renderPanel(); } }, 'Review'));
  } else if (p.trust === 'new') {
    notice = h('div', { class: 'banner warn' }, icon('info', 22),
      h('div', { class: 'grow' }, h('b', {}, 'First contact.'), ` Chirp trusted ${p.name}'s key on first sight. Compare fingerprints in person to be sure who this is.`),
      h('button', { class: 'btn', onclick: () => { S.panel = true; renderPanel(); } }, 'Verify'));
  }

  const list = h('div', { class: 'msgs', role: 'log', 'aria-label': `Conversation with ${p.name}` },
    h('div', { class: 'notice' }, icon('lock', 14), 'End-to-end encrypted · Noise XX'),
    S.msgs.length === 0 && p.trust !== 'unknown' ? h('div', { class: 'notice' }, 'No messages yet. Say hello.') : null,
    ...S.msgs.map((m) => msgEl(m, p)));

  const locked = p.trust === 'changed' || p.trust === 'unknown';
  $ta.disabled = locked;
  $ta.placeholder = p.trust === 'changed' ? 'Review the changed key to continue' : p.trust === 'unknown' ? 'Establishing a secure session…' : p.online ? 'Message' : `Message ${p.name} (queued until they are back)`;
  $ta.value = $ta.value; syncSend();

  $main.replaceChildren(...[banner, head, notice, list, composer].filter(Boolean));
  const l = $main.querySelector('.msgs');
  if (l && (atBottom || !keepScroll)) l.scrollTop = l.scrollHeight; else if (l && prev) l.scrollTop = prev.scrollTop;
}

function msgEl(m, p) {
  const mine = m.dir === 'out';
  let st = null;
  if (mine) {
    if (m.status === 'delivered') st = h('div', { class: 'stat ok' }, icon('checks', 16), `${fmtTime(m.ts)} · Delivered`);
    else st = h('div', { class: 'stat q', title: `Attempts: ${m.attempts}` }, icon('clock', 13), p.online ? 'Sending…' : `Queued · sends when ${p.name} is back`);
  } else st = h('div', { class: 'stat' }, fmtTime(m.ts));
  return h('div', { class: 'm ' + (mine ? 'me' : 'them') }, h('div', { class: 'bubble' }, m.body), st);
}

// ---------- trust panel ----------
function renderPanel() {
  if (!$panel) return;
  const p = curPeer();
  const show = S.view === 'chat' && S.panel && p;
  $panel.style.display = show ? '' : 'none';
  if (!show) return $panel.replaceChildren();
  const groups = (g) => h('div', { class: 'fp' }, (g || []).map((x) => h('span', {}, x)));
  const words = (w) => h('div', { class: 'words' }, (w || []).map((x) => h('span', {}, x)));
  const kids = [
    h('div', { style: 'display:flex;justify-content:space-between;align-items:center' }, h('h1', { style: 'font-size:20px' }, 'Trust'),
      h('button', { class: 'iconbtn', 'aria-label': 'Close panel', onclick: () => { S.panel = false; renderPanel(); } }, icon('x', 20))),
    h('div', { style: 'display:flex;gap:14px;align-items:center' }, identicon(p.identicon, 72), h('div', {}, h('div', { style: 'font-size:18px;font-weight:600' }, p.name), trustChip(p))),
  ];
  if (p.trust === 'changed') {
    kids.push(
      h('div', { class: 'card', style: 'border-color:var(--bad-line);background:var(--bad-bg)' },
        h('h3', { class: 'sec', style: 'color:var(--bad-ink)' }, 'New key claiming this name'), groups(p.newFpGroups), h('div', { style: 'height:10px' }), words(p.newWords)),
      h('div', { class: 'card' }, h('h3', { class: 'sec' }, 'Key you pinned before'), groups(p.fpGroups)),
      h('p', { class: 'muted' }, 'Ask them in person which fingerprint is theirs. Only accept if the new one matches. You will need to verify again afterwards.'),
      h('button', { class: 'btn primary block', onclick: () => confirmBox('Trust the new key?', `Chirp will pin ${p.name} to the new fingerprint and mark them unverified.`, async () => { await api('POST', `/api/peers/${enc(p.name)}/accept-key`); toast('New key pinned'); await loadPeers(); }, 'Trust new key', false) }, 'Trust the new key'));
  } else {
    kids.push(
      h('div', { class: 'card' }, h('h3', { class: 'sec' }, 'Fingerprint'), groups(p.fpGroups)),
      h('div', { class: 'card' }, h('h3', { class: 'sec' }, 'Verify with words'), words(p.words),
        h('p', { class: 'muted', style: 'margin-top:10px;font-size:13px' }, `On ${p.name}'s device, open My identity. Their words about themselves must match these. Six words are a convenience; comparing the full fingerprint is stronger.`)),
      p.trust === 'verified'
        ? h('button', { class: 'btn block', onclick: async () => { await api('POST', `/api/peers/${enc(p.name)}/verify`, { verified: false }); loadPeers(); } }, 'Remove verified mark')
        : h('button', { class: 'btn primary block', onclick: async () => { await api('POST', `/api/peers/${enc(p.name)}/verify`, { verified: true }); toast('Marked as verified'); loadPeers(); } }, icon('shield', 18), 'They match. Mark verified'));
  }
  kids.push(h('button', { class: 'btn danger block', onclick: () => confirmBox(`Forget ${p.name}?`, 'Deletes the pinned key and every message with them on this device. They keep their own copy.', async () => { await api('DELETE', `/api/peers/${enc(p.name)}`); S.cur = null; S.panel = false; await loadPeers(); }, 'Forget') }, 'Forget this person'));
  $panel.replaceChildren(h('div', { class: 'pad' }, ...kids));
}

// ---------- identity dialog ----------
function showIdentity() {
  modal((box, close) => {
    box.append(
      h('div', { style: 'display:flex;justify-content:space-between;align-items:center' }, h('h1', {}, 'My identity'), h('button', { class: 'iconbtn', 'aria-label': 'Close', onclick: close }, icon('x', 20))),
      h('div', { style: 'display:flex;gap:14px;align-items:center' }, identicon(S.me.identicon, 72), h('div', {}, h('div', { style: 'font-size:18px;font-weight:600' }, S.me.name), h('div', { class: 'muted', style: 'font-size:13px' }, `Listening on port ${S.me.port}`))),
      h('div', { class: 'card', style: 'box-shadow:none' }, h('h3', { class: 'sec' }, 'Fingerprint'), h('div', { class: 'fp' }, S.me.fpGroups.map((x) => h('span', {}, x)))),
      h('div', { class: 'card', style: 'box-shadow:none' }, h('h3', { class: 'sec' }, 'My words'), h('div', { class: 'words' }, S.me.words.map((x) => h('span', {}, x)))),
      h('p', { class: 'muted', style: 'font-size:13px' }, 'Show this to someone in person. If it matches what they see for you, nobody is impersonating you.'),
      h('button', { class: 'btn block', onclick: async () => { try { await navigator.clipboard.writeText(S.me.fp); toast('Fingerprint copied'); } catch { toast('Copy failed', true); } } }, 'Copy full fingerprint'));
  });
}

// ---------- network & outbox ----------
function viewNetwork() {
  const d = S.diag;
  const back = h('button', { class: 'back', onclick: () => setView('chat'), 'aria-label': 'Back' }, icon('back', 24));
  const ob = S.outbox.length ? S.outbox.map((m) => h('div', { class: 'ob' },
    h('div', { class: 'grow' }, h('div', { class: 't' }, m.body), h('div', { class: 'n' }, `To ${m.peer} · ${m.attempts} attempt${m.attempts === 1 ? '' : 's'} · written ${fmtTime(m.ts)}`)),
    h('button', { class: 'iconbtn', 'aria-label': 'Retry now', title: 'Retry now', onclick: async () => { await api('POST', `/api/peers/${enc(m.peer)}/retry`); toast('Retrying'); } }, icon('refresh', 18)),
    h('button', { class: 'iconbtn', 'aria-label': 'Delete from outbox', title: 'Delete', onclick: () => confirmBox('Delete this unsent message?', 'It will never be delivered.', async () => { await api('DELETE', `/api/outbox/${m.id}`); await loadOutbox(); renderMain(); renderSide(); }, 'Delete') }, icon('trash', 18))))
    : [h('p', { class: 'muted' }, 'Nothing waiting. Undelivered messages queue here and retry on their own.')];
  return h('div', { class: 'page' }, h('div', { class: 'col' },
    h('div', { style: 'display:flex;gap:8px;align-items:center' }, back, h('h1', {}, 'Network and outbox')),
    h('div', { class: 'card' }, h('h3', { class: 'sec' }, 'This device'),
      d ? h('dl', { class: 'kv' },
        h('dt', {}, 'Listening'), h('dd', {}, d.listen), h('dt', {}, 'Uptime'), h('dd', {}, `${d.uptimeSec}s`),
        h('dt', {}, 'Nearby peers'), h('dd', {}, String(d.nearby)),
        h('dt', {}, 'Live sessions'), h('dd', {}, d.sessions?.length ? d.sessions.map((s) => `${s.peer} @ ${s.addr}`).join('\n') : 'none')) : 'Loading…',
      h('p', { class: 'muted', style: 'margin-top:12px;font-size:13px' }, 'If nobody shows up: the devices must share a Wi-Fi network, guest or "client isolation" networks block multicast, and your firewall must allow UDP 5353 and this TCP port.')),
    h('div', { class: 'card' }, h('h3', { class: 'sec' }, `Outbox (${S.outbox.length})`), ob),
    h('div', { class: 'card' }, h('h3', { class: 'sec' }, 'Discovery log'),
      h('div', { class: 'log', role: 'log' }, (d?.log || []).slice(-80).map((l) => h('div', { class: /REFUSED|failed/.test(l.msg) ? 'bad' : '' }, h('time', {}, new Date(l.at).toLocaleTimeString()), l.msg))))));
}

// ---------- settings ----------
function viewSettings() {
  const st = S.settings;
  const save = async (patch) => { S.settings = { ...st, ...patch }; try { await api('PUT', '/api/settings', S.settings); } catch (e) { toast(e.message, true); await loadSettings(); } renderMain(); renderSide(); };
  const sw = (title, note, key, extra) => h('div', { class: 'toggle' },
    h('div', { class: 'grow' }, h('div', { class: 't' }, title), h('div', { class: 'n' }, note)),
    h('button', { class: 'switch', role: 'switch', 'aria-checked': String(!!st[key]), 'aria-label': title, onclick: async () => { if (extra) await extra(!st[key]); save({ [key]: !st[key] }); } }));
  const back = h('button', { class: 'back', onclick: () => setView('chat'), 'aria-label': 'Back' }, icon('back', 24));
  const opts = [[0, 'Forever'], [1, '1 day'], [7, '7 days'], [30, '30 days']];
  return h('div', { class: 'page' }, h('div', { class: 'col' },
    h('div', { style: 'display:flex;gap:8px;align-items:center' }, back, h('h1', {}, 'Settings')),
    h('div', { class: 'card', style: 'padding:6px 18px' },
      sw('Browser notifications', 'A quiet alert when this tab is in the background.', 'notify', async (on) => { if (on && 'Notification' in window && Notification.permission === 'default') await Notification.requestPermission(); }),
      sw('Show message text', 'Off: notifications and the people list say "New message".', 'previews')),
    h('div', { class: 'card' }, h('h3', { class: 'sec' }, 'Keep messages for'),
      h('p', { class: 'muted', style: 'font-size:13px;margin-bottom:10px' }, 'Older delivered messages are deleted from this device. Unsent messages are never deleted. Others keep their own copy.'),
      h('div', { class: 'seg' }, opts.map(([v, l]) => h('button', { 'aria-pressed': String(st.retentionDays === v), onclick: () => save({ retentionDays: v }) }, l)))),
    h('div', { class: 'card', style: 'padding:6px 18px' },
      h('div', { class: 'toggle' }, h('div', { class: 'grow' }, h('div', { class: 't' }, 'Back up your key'), h('div', { class: 'n' }, 'Your key is your identity. There is no account to recover.')), h('button', { class: 'btn', onclick: showBackup }, icon('key', 18), 'Back up')),
      h('div', { class: 'toggle' }, h('div', { class: 'grow' }, h('div', { class: 't', style: 'color:var(--bad)' }, 'Delete all messages'), h('div', { class: 'n' }, 'Keeps your key and pinned people.')),
        h('button', { class: 'btn danger', onclick: () => confirmBox('Delete every message?', 'This cannot be undone. Unsent messages are deleted too.', async () => { await api('POST', '/api/messages/clear'); S.msgs = []; toast('All messages deleted'); await loadPeers(); }, 'Delete all') }, 'Delete'))),
    h('p', { class: 'muted', style: 'font-size:13px;padding:0 6px' }, 'Chirp has no server, so nothing here is stored anywhere except your devices.')));
}

function strength(p) {
  let s = 0;
  if (p.length >= 8) s++; if (p.length >= 12) s++;
  if (/[A-Z]/.test(p) && /[a-z]/.test(p)) s++;
  if (/[0-9]/.test(p) && /[^A-Za-z0-9]/.test(p)) s++;
  return s;
}
function showBackup() {
  modal((box, close) => {
    const input = h('input', { id: 'bp', type: 'password', autocomplete: 'new-password', placeholder: 'At least 12 characters' });
    const bars = h('div', { class: 'bars' }, [0, 1, 2, 3].map(() => h('i')));
    const label = h('div', { class: 'muted', style: 'font-size:13px' }, 'Choose a passphrase you will remember');
    const go = h('button', { class: 'btn primary', disabled: true }, 'Export encrypted backup');
    input.addEventListener('input', () => {
      const s = strength(input.value), cols = ['#E5323B', '#FF9B1A', '#9BD14A', '#1FBF7A'];
      [...bars.children].forEach((b, i) => { b.style.background = i < s ? cols[s - 1] : ''; });
      label.textContent = input.value ? ['Too short', 'Weak', 'Okay', 'Good', 'Strong'][s] : 'Choose a passphrase you will remember';
      go.disabled = input.value.length < 12;
    });
    go.addEventListener('click', async () => {
      try {
        const r = await api('POST', '/api/backup', { passphrase: input.value });
        const url = URL.createObjectURL(await r.blob());
        const a = h('a', { href: url, download: 'chirp-key-backup.json' }); document.body.append(a); a.click(); a.remove(); URL.revokeObjectURL(url);
        close(); toast('Backup saved. Keep the passphrase safe.');
      } catch (e) { toast(e.message, true); }
    });
    box.append(h('h1', {}, 'Back up your key'),
      h('div', { class: 'banner warn', style: 'border-radius:18px' }, icon('key', 22), h('div', { class: 'grow' }, 'If you lose this device without a backup, everyone will see a new key and must verify you again. Chirp cannot recover a forgotten passphrase.')),
      h('div', { class: 'field' }, h('label', { for: 'bp' }, 'Backup passphrase'), input), bars, label,
      h('div', { style: 'display:flex;gap:10px;justify-content:flex-end' }, h('button', { class: 'btn', onclick: close }, 'Cancel'), go));
  });
}

// ---------- first run ----------
function viewWelcome() {
  const name = h('input', { id: 'nm', maxlength: 32, autocomplete: 'nickname', placeholder: 'e.g. Alex Rivera' });
  const err = h('div', { class: 'err' });
  const go = h('button', { class: 'btn primary block', disabled: true }, 'Create my key and start');
  name.addEventListener('input', () => { go.disabled = !name.value.trim(); err.textContent = ''; });
  const start = async () => {
    try { await api('POST', '/api/setup', { name: name.value }); await boot(); } catch (e) { err.textContent = e.message; }
  };
  go.addEventListener('click', start);
  name.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !go.disabled) start(); });
  const file = h('input', { type: 'file', accept: 'application/json,.json', 'aria-label': 'Backup file', style: 'height:auto;padding:10px' });
  const pass = h('input', { type: 'password', placeholder: 'Backup passphrase', 'aria-label': 'Backup passphrase' });
  const rerr = h('div', { class: 'err' });
  const restore = h('button', { class: 'btn block', onclick: async () => {
    try { const f = file.files[0]; if (!f) throw new Error('Choose a backup file'); await api('POST', '/api/restore', { backup: await f.text(), passphrase: pass.value }); await boot(); } catch (e) { rerr.textContent = e.message; }
  } }, 'Restore from backup');
  $app.replaceChildren(h('div', { class: 'welcome' }, h('div', { class: 'card' },
    h('div', { style: 'display:flex;align-items:center;gap:10px;font-weight:600;font-size:20px' }, h('span', { class: 'logo' }, logoSvg()), 'Chirp'),
    h('h1', {}, 'Talk to people on your Wi-Fi. No server.'),
    h('div', { class: 'steps' },
      h('div', {}, h('span', { class: 'ic' }, icon('key', 14)), h('span', {}, 'A key is created on this device. It is your identity and it never leaves.')),
      h('div', {}, h('span', { class: 'ic' }, icon('wifi', 14)), h('span', {}, 'Nearby Chirp users appear automatically over mDNS.')),
      h('div', {}, h('span', { class: 'ic' }, icon('lock', 14)), h('span', {}, 'Messages go device to device, encrypted with Noise.'))),
    h('div', { class: 'field' }, h('label', { for: 'nm' }, 'Your name'), name, h('span', { class: 'hint' }, 'Nearby people see this name.'), err), go,
    h('details', {}, h('summary', { style: 'cursor:pointer;font-weight:500' }, 'I have a backup'),
      h('div', { style: 'display:flex;flex-direction:column;gap:10px;margin-top:12px' }, file, pass, rerr, restore)))));
  name.focus();
}

// ---------- boot ----------
async function boot() {
  const st = await getJSON('/api/state');
  if (st.setup) { viewWelcome(); return; }
  S.me = st.me;
  await Promise.all([loadSettings(), loadPeers()]);
  mountShell();
  connectEvents();
}
boot().catch(() => { $app.replaceChildren(h('div', { class: 'empty', style: 'margin-top:20vh' }, h('h3', {}, 'Cannot reach the Chirp daemon'), h('p', {}, 'Is chirpd still running?'))); });
