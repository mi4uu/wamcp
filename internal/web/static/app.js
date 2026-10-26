// wamcp web client: a WhatsApp Web–like client for the local archive.
// No framework: small DOM helpers, one state object, server-sent events.

const $ = (id) => document.getElementById(id);

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

const S = {
  session: null,
  chats: [],
  byJid: new Map(),
  filter: 'all',
  tag: '',
  showArchived: false,
  query: '',
  view: 'chats', // chats | starred | calls
  active: null,
  messages: [],
  hasMore: false,
  loadingOlder: false,
  replyTo: null,
  files: [],
  info: false,
  infoData: null,
  typing: new Map(), // chat -> {name, until}
  presence: new Map(), // jid -> {online, last_seen}
  searchResults: null,
  settings: loadSettings(),
};

function loadSettings() {
  const def = { notify: false, autoRead: false, online: true };
  try { return { ...def, ...JSON.parse(localStorage.getItem('wamcp.settings') || '{}') }; } catch { return def; }
}
function saveSettings() {
  try { localStorage.setItem('wamcp.settings', JSON.stringify(S.settings)); } catch {}
}

// ---------- API

async function api(path, opts = {}) {
  const init = { method: opts.method || 'GET', headers: {} };
  if (opts.json !== undefined) {
    init.method = opts.method || 'POST';
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(opts.json);
  } else if (opts.form) {
    init.method = 'POST';
    init.body = opts.form;
  }
  const res = await fetch(path, init);
  if (res.status === 401 && !path.startsWith('/api/login')) {
    showLogin();
    throw new Error('Signed out');
  }
  const ct = res.headers.get('Content-Type') || '';
  const body = ct.includes('json') ? await res.json() : await res.text();
  if (!res.ok) throw new Error(body && body.error ? body.error : `HTTP ${res.status}`);
  return body;
}
const enc = encodeURIComponent;

function toast(msg, ms = 3500) {
  const t = $('toast');
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => (t.hidden = true), ms);
}
async function attempt(fn, okMsg) {
  try {
    const r = await fn();
    if (okMsg) toast(okMsg);
    return r;
  } catch (e) {
    toast(e.message || String(e), 6000);
    return undefined;
  }
}

// ---------- formatting

const colors = ['#e542a3', '#1f7aec', '#dc5f00', '#008069', '#7f66ff', '#c4532d', '#02a698', '#b4876e', '#5e47de', '#d9365e'];
function colorFor(s) {
  let x = 0;
  for (const ch of s || '') x = (x * 31 + ch.charCodeAt(0)) >>> 0;
  return colors[x % colors.length];
}
function initials(name) {
  const w = (name || '?').replace(/[^\p{L}\p{N} ]/gu, '').trim().split(/\s+/);
  return ((w[0] || '?')[0] + (w.length > 1 ? w[w.length - 1][0] : '')).toUpperCase();
}
// Avatars load once per session and only when visible, so long chat lists
// do not flood the (6-connection) HTTP/1.1 pool shared by all tabs.
const avatarState = new Map(); // jid -> 'ok' | 'none' | 'loading'
const avatarMissAt = new Map(); // jid -> when the picture was missing (retry after an hour)
const avatarObserver = 'IntersectionObserver' in window
  ? new IntersectionObserver((entries) => {
    for (const e of entries) {
      if (!e.isIntersecting) continue;
      avatarObserver.unobserve(e.target);
      loadAvatar(e.target);
    }
  }, { rootMargin: '200px' })
  : null;
function showAvatarImg(el, jid) {
  const img = new Image();
  img.src = `/api/avatar/${enc(jid)}`;
  el.textContent = '';
  el.append(img);
}
function loadAvatar(el) {
  const jid = el.dataset.jid;
  const st = avatarState.get(jid);
  if (st === 'ok') return showAvatarImg(el, jid);
  if (st === 'none' || st === 'loading') return;
  avatarState.set(jid, 'loading');
  const img = new Image();
  img.onload = () => {
    avatarState.set(jid, 'ok');
    for (const other of document.querySelectorAll(`.avatar[data-jid="${CSS.escape(jid)}"]`)) {
      if (!other.querySelector('img')) showAvatarImg(other, jid);
    }
  };
  img.onerror = () => { avatarState.set(jid, 'none'); avatarMissAt.set(jid, Date.now()); };
  img.src = `/api/avatar/${enc(jid)}`;
}
function avatar(jid, name, cls = '') {
  if (avatarState.get(jid) === 'none' && Date.now() - (avatarMissAt.get(jid) || 0) > 3600e3) avatarState.delete(jid);
  const el = h('div', { class: `avatar ${cls}`, style: `background:${colorFor(jid)}`, dataset: { jid: jid || '' } }, initials(name));
  if (!jid || jid.endsWith('@broadcast') || avatarState.get(jid) === 'none') return el;
  if (avatarState.get(jid) === 'ok') showAvatarImg(el, jid);
  else if (avatarObserver) avatarObserver.observe(el);
  else loadAvatar(el);
  return el;
}
const pad = (n) => String(n).padStart(2, '0');
function sameDay(a, b) { return a.toDateString() === b.toDateString(); }
function timeOf(ts) { const d = new Date(ts * 1000); return `${pad(d.getHours())}:${pad(d.getMinutes())}`; }
function listTime(ts) {
  if (!ts) return '';
  const d = new Date(ts * 1000), now = new Date();
  if (sameDay(d, now)) return timeOf(ts);
  const y = new Date(now); y.setDate(now.getDate() - 1);
  if (sameDay(d, y)) return 'Yesterday';
  if (now - d < 6 * 864e5) return d.toLocaleDateString(undefined, { weekday: 'long' });
  return d.toLocaleDateString();
}
function dayLabel(ts) {
  const d = new Date(ts * 1000), now = new Date();
  if (sameDay(d, now)) return 'Today';
  const y = new Date(now); y.setDate(now.getDate() - 1);
  if (sameDay(d, y)) return 'Yesterday';
  return d.toLocaleDateString(undefined, { weekday: 'long', year: 'numeric', month: 'long', day: 'numeric' });
}
function fullTime(ts) { return ts ? new Date(ts * 1000).toLocaleString() : '—'; }
function size(n) {
  if (!n) return '';
  if (n > 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n > 1 << 10) return `${Math.round(n / 1024)} KB`;
  return `${n} B`;
}
function clock(s) { return `${Math.floor(s / 60)}:${pad(s % 60)}`; }
const kindLabel = {
  image: '📷 Photo', video: '🎥 Video', gif: 'GIF', voice: '🎤 Voice message', audio: '🎵 Audio',
  document: '📄 Document', sticker: 'Sticker', location: '📍 Location', live_location: '📍 Live location',
  contact: '👤 Contact', poll: '📊 Poll', event: '📅 Event', video_note: '🎥 Video message',
  group_invite: 'Group invite', revoked: '🚫 This message was deleted', product: '🛍 Product',
};
function previewText(p) {
  if (!p) return '';
  if (p.kind === 'revoked') return kindLabel.revoked;
  if (p.deleted) return '🚫 ' + previewText({ ...p, deleted: false });
  const label = p.kind !== 'text' && p.kind !== 'system' ? kindLabel[p.kind] || p.kind : '';
  const text = (p.text || '').split('\n')[0];
  return [label, text].filter(Boolean).join(label && text ? ': ' : '');
}
function ticks(ack) {
  if (!ack) return null;
  const read = ack >= 3;
  return h('span', { class: `ticks${read ? ' read' : ''}`, title: ['', 'Sent', 'Delivered', 'Read', 'Played'][ack] }, ack === 1 ? '✓' : '✓✓');
}
const prioNames = ['', 'lowest', 'low', 'normal', 'high', 'top'];
function prioBadge(p) {
  return p ? h('span', { class: `prio prio-${p}`, title: `Priority: ${prioNames[p]} (${p}/5)` }, `P${p}`) : null;
}

// WhatsApp-style formatting (*bold*, _italic_, ~strike~, ```mono```) and links,
// built as DOM nodes so message text can never inject HTML.
function richText(text, highlight) {
  const out = document.createDocumentFragment();
  const re = /(https?:\/\/[^\s<>"]+)|```([\s\S]+?)```|\*([^*\n]+)\*|_([^_\n]+)_|~([^~\n]+)~/g;
  let last = 0, m;
  const plain = (s) => {
    if (!highlight) { out.append(s); return; }
    const words = highlight.toLowerCase().split(/\s+/).filter((w) => w.length > 1);
    if (!words.length) { out.append(s); return; }
    const hre = new RegExp(`(${words.map((w) => w.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')})`, 'gi');
    for (const part of s.split(hre)) out.append(hre.test(part) ? h('mark', {}, part) : part), (hre.lastIndex = 0);
  };
  while ((m = re.exec(text))) {
    if (m.index > last) plain(text.slice(last, m.index));
    if (m[1]) out.append(h('a', { href: m[1], target: '_blank', rel: 'noopener noreferrer' }, m[1]));
    else if (m[2]) out.append(h('code', {}, m[2]));
    else if (m[3]) out.append(h('b', {}, m[3]));
    else if (m[4]) out.append(h('i', {}, m[4]));
    else if (m[5]) out.append(h('s', {}, m[5]));
    last = re.lastIndex;
  }
  if (last < text.length) plain(text.slice(last));
  return out;
}

// ---------- boot, pairing, login

let es = null;

async function boot() {
  try {
    S.session = await api('/api/session');
  } catch (e) {
    $('boot').hidden = false;
    setTimeout(boot, 3000);
    return;
  }
  $('boot').hidden = true;
  if (!S.session.paired) return showPair();
  if (!S.session.authenticated) return showLogin();
  showApp();
}

function hideScreens() {
  for (const id of ['boot', 'pair', 'login', 'app']) $(id).hidden = true;
}

let pairTimer = null;
function showPair() {
  hideScreens();
  $('pair').hidden = false;
  clearInterval(pairTimer);
  const refresh = async () => {
    let s;
    try { s = await api('/api/session'); } catch { return; }
    S.session = s;
    if (s.paired && s.state === 'connected') {
      clearInterval(pairTimer);
      return s.authenticated ? showApp() : showLogin();
    }
    const qr = $('qr');
    if (s.has_qr) {
      qr.src = `/qr.png?t=${Date.now()}`;
      qr.hidden = false;
      $('qr-wait').hidden = true;
      $('qr-retry').hidden = true;
    } else {
      qr.hidden = true;
      $('qr-wait').hidden = false;
      const paused = s.state === 'qr_expired' || s.state === 'logged_out';
      $('qr-wait').textContent = paused ? 'The code expired.' : s.state === 'connecting' ? 'Linked! Connecting…' : 'Generating code…';
      $('qr-retry').hidden = !paused;
    }
    if (s.pair_code) {
      $('pair-code').textContent = s.pair_code;
      $('pair-code').hidden = false;
    }
    $('pair-status').textContent = s.last_error ? `Last error: ${s.last_error}` : `State: ${s.state}`;
  };
  refresh();
  pairTimer = setInterval(refresh, 2500);
}
$('qr-retry').onclick = () => attempt(() => api('/api/pairing/restart', { json: {} }));
$('pair-phone-form').onsubmit = (e) => {
  e.preventDefault();
  attempt(() => api('/api/pairing/phone', { json: { phone: $('pair-phone').value } }), 'Requesting a pair code…');
};

function showLogin() {
  hideScreens();
  $('login').hidden = false;
  if (new URLSearchParams(location.search).get('login') === 'expired') {
    $('login-error').textContent = 'That sign-in link expired. Run `wamcp open` again.';
  }
  $('login-token').focus();
}
$('login-form').onsubmit = async (e) => {
  e.preventDefault();
  try {
    await api('/api/login', { json: { token: $('login-token').value } });
    history.replaceState(null, '', '/');
    boot();
  } catch (err) {
    $('login-error').textContent = err.message;
  }
};

async function showApp() {
  hideScreens();
  $('app').hidden = false;
  clearInterval(pairTimer);
  const me = S.session.account || '';
  const meAv = $('me-avatar');
  meAv.replaceWith(Object.assign(avatar(me, S.session.push_name || 'Me', 'sm'), { id: 'me-avatar', title: S.session.push_name || me }));
  renderFilters();
  await refreshChats();
  updateConn(S.session.state);
  connectEvents();
  setPresence(document.visibilityState === 'visible');
  loadEmptyStats();
  const jid = decodeURIComponent(location.hash.slice(1));
  if (jid) openChat(jid);
}

async function loadEmptyStats() {
  const r = await attempt(() => api('/api/status'));
  if (!r) return;
  const st = r.stats, d = r.daemon;
  let text = `${st.Messages.toLocaleString()} messages · ${st.Chats} chats · ${st.Groups} groups · ${st.Contacts} contacts`;
  if (d.history && d.history.batches) text += ` · history sync: ${d.history.batches} batches (${d.history.last_type} ${d.history.last_progress}%)`;
  if (d.catch_up && d.catch_up.offline_seconds > 60) {
    text += ` · last reconnect after ${Math.round(d.catch_up.offline_seconds / 60)} min offline: ${d.catch_up.received} messages recovered`;
  }
  $('empty-stats').textContent = text;
}

// ---------- live events

function connectEvents() {
  if (es) es.close();
  es = new EventSource(`/api/events?tab=${tabId}`);
  es.onmessage = (e) => {
    let ev;
    try { ev = JSON.parse(e.data); } catch { return; }
    onEvent(ev);
  };
  es.onopen = () => {
    // Re-assert presence after (re)connecting; the server may have dropped this tab.
    presenceState = null;
    setPresence(document.visibilityState === 'visible');
  };
  es.onerror = async () => {
    if (es.readyState !== EventSource.CLOSED) { updateConn('disconnected'); return; }
    // The browser gave up (e.g. HTTP 401 after the session expired): find out why.
    try {
      const s = await api('/api/session');
      if (!s.authenticated) return showLogin();
    } catch { /* daemon restarting */ }
    updateConn('disconnected');
    setTimeout(connectEvents, 3000);
  };
}

let chatsTimer = null;
function scheduleChats() {
  clearTimeout(chatsTimer);
  chatsTimer = setTimeout(refreshChats, 400);
}
let activeTimer = null;
function scheduleActive() {
  clearTimeout(activeTimer);
  activeTimer = setTimeout(refreshActive, 250);
}

function onEvent(ev) {
  switch (ev.type) {
    case 'status':
      updateConn(ev.data.state);
      if (['pairing', 'qr_expired', 'logged_out'].includes(ev.data.state) && !ev.data.account) showPair();
      if (ev.data.state === 'logged_out') showPair();
      break;
    case 'message':
    case 'receipt':
      scheduleChats();
      if (ev.chat === S.active) scheduleActive();
      break;
    case 'chat':
    case 'sync':
      scheduleChats();
      if (ev.chat && ev.chat === S.active && S.info) loadInfo();
      break;
    case 'call':
      if (S.view === 'calls') renderCalls();
      break;
    case 'typing':
      if (ev.data.state === 'composing') {
        const c = S.byJid.get(ev.chat);
        const name = c && c.kind === 'group' ? nameOf(ev.data.sender) : '';
        S.typing.set(ev.chat, { name, until: Date.now() + 6000, audio: ev.data.media === 'audio' });
      } else S.typing.delete(ev.chat);
      if (ev.chat === S.active) renderChatSub();
      renderListSoon();
      break;
    case 'presence':
      S.presence.set(ev.chat, ev.data);
      if (ev.chat === S.active) renderChatSub();
      break;
  }
}

function updateConn(state) {
  const el = $('conn');
  el.className = 'conn ' + (state === 'connected' ? 'ok' : state === 'disconnected' || state === 'connecting' ? '' : 'bad');
  el.title = `WhatsApp: ${state}`;
  const b = $('banner');
  const msgs = {
    disconnected: 'Reconnecting to WhatsApp… messages will catch up automatically.',
    connecting: 'Connecting to WhatsApp…',
    replaced: 'Another program is using this WhatsApp session. Stop it and restart wamcp.',
    temporarily_banned: 'WhatsApp temporarily banned this account from linked devices.',
    logged_out: 'This device was unlinked. Link it again to continue syncing.',
  };
  b.textContent = msgs[state] || '';
  b.hidden = !msgs[state];
}

function nameOf(jid) {
  const c = S.byJid.get(jid);
  if (c) return c.name;
  return jid ? '+' + jid.split('@')[0] : '';
}

// ---------- presence (online while the tab is in use, so the phone keeps notifications otherwise)

// Each tab reports its own visibility; the server keeps the account online
// while any tab is visible.
const tabId = Math.random().toString(36).slice(2) + Date.now().toString(36);
let presenceState = null;
function setPresence(on) {
  on = on && S.settings.online;
  if (presenceState === on) return;
  presenceState = on;
  api('/api/presence', { json: { available: on, tab: tabId } }).catch(() => {});
}
document.addEventListener('visibilitychange', () => setPresence(document.visibilityState === 'visible'));
window.addEventListener('pagehide', () => {
  navigator.sendBeacon && navigator.sendBeacon('/api/presence', new Blob([JSON.stringify({ available: false, tab: tabId })], { type: 'application/json' }));
});

// ---------- chat list

async function refreshChats() {
  let chats;
  try { chats = await api('/api/chats'); } catch { return; }
  notifyNew(chats);
  S.chats = chats;
  S.byJid = new Map(chats.map((c) => [c.jid, c]));
  renderFilters();
  renderList();
  if (S.active) renderChatHead();
}

function notifyNew(chats) {
  if (!S.settings.notify || !('Notification' in window) || Notification.permission !== 'granted' || !S.chats.length) return;
  for (const c of chats) {
    const old = S.byJid.get(c.jid);
    if ((old && c.last_ts <= old.last_ts) || !c.preview || c.preview.from_me) continue;
    if (c.muted_until === -1 || c.muted_until > Date.now() / 1000) continue;
    if (c.jid === S.active && document.visibilityState === 'visible') continue;
    const n = new Notification(c.name, { body: (c.preview.sender ? c.preview.sender + ': ' : '') + previewText(c.preview), tag: c.jid });
    n.onclick = () => { window.focus(); openChat(c.jid); };
  }
}

function renderFilters() {
  const tags = [...new Set(S.chats.flatMap((c) => c.meta.tags || []))].sort();
  const box = $('filters');
  box.textContent = '';
  const chip = (id, label) => h('button', {
    class: `chip${S.filter === id && S.view === 'chats' ? ' on' : ''}`,
    onclick: () => { S.filter = id; S.view = 'chats'; S.showArchived = false; renderFilters(); renderList(); },
  }, label);
  box.append(chip('all', 'All'), chip('unread', 'Unread'), chip('direct', 'Direct'), chip('groups', 'Groups'), chip('priority', 'Priority'));
  if (tags.length) {
    const sel = h('select', { class: 'chip', onchange: (e) => { S.tag = e.target.value; renderList(); } },
      h('option', { value: '' }, '# tag'), ...tags.map((t) => h('option', { value: t, selected: t === S.tag }, '#' + t)));
    box.append(sel);
  }
}

function chatVisible(c) {
  if (S.showArchived !== c.archived && !(S.query && S.filter === 'all')) return false;
  switch (S.filter) {
    case 'unread': if (!(c.unread > 0 || c.marked_unread)) return false; break;
    case 'direct': if (c.kind !== 'direct') return false; break;
    case 'groups': if (c.kind !== 'group') return false; break;
    case 'priority': if (!(c.meta.priority >= 4)) return false; break;
  }
  if (S.tag && !(c.meta.tags || []).includes(S.tag)) return false;
  if (S.query) {
    const q = fold(S.query);
    const hay = fold([c.name, c.wa_name, c.phone, c.jid, (c.meta.tags || []).join(' '), c.meta.description].join(' '));
    if (!hay.includes(q)) return false;
  }
  return true;
}
function fold(s) {
  return (s || '').toLowerCase().normalize('NFD').replace(/\p{M}/gu, '').replace(/ł/g, 'l');
}

let listRaf = 0;
function renderListSoon() { cancelAnimationFrame(listRaf); listRaf = requestAnimationFrame(renderList); }

function renderList() {
  if (S.view === 'starred') return renderStarred();
  if (S.view === 'calls') return renderCalls();
  const body = $('side-body');
  const scroll = body.scrollTop;
  body.textContent = '';
  const archivedCount = S.chats.filter((c) => c.archived).length;
  if (S.showArchived) {
    body.append(h('div', { class: 'item', onclick: () => { S.showArchived = false; renderList(); } },
      h('div', { class: 'call-icon' }, '←'), h('div', { class: 'item-body' }, h('b', {}, 'Archived'))));
  } else if (archivedCount && !S.query) {
    const unread = S.chats.filter((c) => c.archived && c.unread > 0).length;
    body.append(h('div', { class: 'item', onclick: () => { S.showArchived = true; renderList(); } },
      h('div', { class: 'call-icon' }, '🗄'),
      h('div', { class: 'item-body' }, h('div', { class: 'item-top' }, h('span', { class: 'item-name' }, 'Archived'),
        unread ? h('span', { class: 'badge' }, unread) : h('span', { class: 'muted small' }, archivedCount)))));
  }
  const list = S.chats.filter(chatVisible);
  list.sort((a, b) => (b.pinned - a.pinned) || (b.last_ts - a.last_ts));
  const frag = document.createDocumentFragment();
  for (const c of list.slice(0, 800)) frag.append(chatItem(c));
  body.append(frag);
  if (!list.length && !S.searchResults) body.append(h('p', { class: 'muted', style: 'padding:20px;text-align:center' }, S.query ? 'No chats match.' : 'No chats yet. History arrives shortly after linking.'));
  if (S.searchResults) {
    body.append(h('div', { class: 'section-title' }, `Messages (${S.searchResults.length})`));
    for (const m of S.searchResults) body.append(resultItem(m));
  }
  body.scrollTop = scroll;
}

function chatItem(c) {
  const typing = S.typing.get(c.jid);
  const muted = c.muted_until === -1 || c.muted_until > Date.now() / 1000;
  let preview;
  if (typing && typing.until > Date.now()) {
    preview = h('span', { class: 'item-preview', style: 'color:#25d366' }, (typing.name ? typing.name + ' is ' : '') + (typing.audio ? 'recording audio…' : 'typing…'));
  } else {
    const p = c.preview;
    preview = h('span', { class: 'item-preview' },
      p && p.from_me ? ticks(1) : null,
      p && p.sender ? p.sender + ': ' : '',
      previewText(p));
  }
  return h('div', { class: `item${c.jid === S.active ? ' active' : ''}`, onclick: () => openChat(c.jid), title: c.jid },
    avatar(c.jid, c.name),
    h('div', { class: 'item-body' },
      h('div', { class: 'item-top' },
        h('span', { class: 'item-name' }, c.name),
        prioBadge(c.meta.priority),
        h('span', { class: `item-time${c.unread ? ' unread' : ''}` }, listTime(c.last_ts))),
      h('div', { class: 'item-bottom' },
        preview,
        ...(c.meta.tags || []).slice(0, 2).map((t) => h('span', { class: 'tag' }, '#' + t)),
        muted ? h('span', { class: 'flag', title: 'Muted' }, '🔕') : null,
        c.pinned ? h('span', { class: 'flag', title: 'Pinned' }, '📌') : null,
        c.unread > 0 ? h('span', { class: `badge${muted ? ' muted-badge' : ''}` }, c.unread)
          : c.marked_unread ? h('span', { class: 'badge' }, ' ') : null)));
}

function resultItem(m) {
  return h('div', { class: 'item', onclick: () => openChat(m.chat, m.id) },
    h('div', { class: 'item-body' },
      h('div', { class: 'item-top' }, h('span', { class: 'result-chat item-name' }, m.chat_name || nameOf(m.chat)),
        h('span', { class: 'item-time' }, listTime(m.ts))),
      h('div', { class: 'item-bottom' }, h('span', { class: 'item-preview' },
        m.from_me ? 'You: ' : m.sender_name && m.chat.endsWith('@g.us') ? m.sender_name + ': ' : '',
        richText(previewText(m), S.query)))));
}

let searchTimer = null;
$('search').addEventListener('input', (e) => {
  S.query = e.target.value.trim();
  S.view = 'chats';
  renderList();
  clearTimeout(searchTimer);
  if (S.query.length < 2) { S.searchResults = null; renderList(); return; }
  searchTimer = setTimeout(async () => {
    try { S.searchResults = await api(`/api/search?q=${enc(S.query)}&limit=60`); } catch { S.searchResults = []; }
    renderList();
  }, 300);
});

async function renderStarred() {
  const body = $('side-body');
  body.textContent = '';
  body.append(h('div', { class: 'section-title' }, 'Starred messages'));
  const list = await attempt(() => api('/api/starred'));
  if (!list) return;
  if (!list.length) body.append(h('p', { class: 'muted', style: 'padding:16px' }, 'No starred messages.'));
  for (const m of list.reverse()) body.append(resultItem(m));
}

async function renderCalls() {
  const body = $('side-body');
  const calls = await attempt(() => api('/api/calls'));
  if (!calls) return;
  body.textContent = '';
  body.append(h('div', { class: 'section-title' }, 'Calls'));
  if (!calls.length) body.append(h('p', { class: 'muted', style: 'padding:16px' }, 'No calls archived yet.'));
  for (const c of calls) {
    const missed = !c.from_me && ['missed', 'rejected', 'ringing'].includes(c.result);
    body.append(h('div', { class: 'item', onclick: () => openChat(c.chat) },
      avatar(c.chat, c.name),
      h('div', { class: 'item-body' },
        h('div', { class: 'item-top' }, h('span', { class: `item-name${missed ? ' call-missed' : ''}` }, c.name), h('span', { class: 'item-time' }, listTime(c.ts))),
        h('div', { class: 'item-bottom' }, h('span', { class: 'item-preview' },
          `${c.from_me ? '↗ Outgoing' : '↙ Incoming'} ${c.group ? 'group ' : ''}${c.video ? 'video' : 'voice'} call · ${c.result}${c.duration ? ' · ' + clock(c.duration) : ''}`)))));
  }
}

$('btn-starred').onclick = () => { S.view = S.view === 'starred' ? 'chats' : 'starred'; renderFilters(); renderList(); };
$('btn-calls').onclick = () => { S.view = S.view === 'calls' ? 'chats' : 'calls'; renderFilters(); renderList(); };
$('btn-new').onclick = newChatDialog;
$('btn-settings').onclick = settingsDialog;

// ---------- conversation

async function openChat(jid, focusId) {
  if (S.active !== jid) {
    S.replyTo = null;
    S.files = [];
    renderReply();
    renderAttach();
  }
  S.active = jid;
  history.replaceState(null, '', '#' + enc(jid));
  $('app').classList.add('chat-open');
  $('empty').hidden = true;
  $('chat').hidden = false;
  renderChatHead();
  renderList();
  $('messages').textContent = '';
  const res = await attempt(() => api(`/api/chats/${enc(jid)}/messages?limit=80${focusId ? '&around=' + enc(focusId) : ''}`));
  if (!res || S.active !== jid) return;
  S.messages = res.messages;
  S.hasMore = focusId ? true : res.has_more;
  renderMessages(focusId ? 'focus' : 'bottom', focusId);
  if (S.info) loadInfo();
  const c = S.byJid.get(jid);
  if (c && c.kind === 'direct') api(`/api/chats/${enc(jid)}/presence`, { json: {} }).catch(() => {});
  if (c && (c.unread > 0 || c.marked_unread) && S.settings.autoRead) markRead();
  $('input').focus();
}

function renderChatHead() {
  const c = S.byJid.get(S.active) || { jid: S.active, name: nameOf(S.active), kind: S.active.endsWith('@g.us') ? 'group' : 'direct', meta: {} };
  $('chat-avatar').replaceWith(Object.assign(avatar(c.jid, c.name), { id: 'chat-avatar' }));
  const name = $('chat-name');
  name.textContent = '';
  name.append(c.name, ' ', prioBadge(c.meta && c.meta.priority) || '');
  renderChatSub();
  const readonly = c.kind === 'status' || c.kind === 'channel' || c.member === false;
  $('composer').hidden = readonly;
  $('readonly-bar').hidden = !readonly;
  $('readonly-bar').textContent = c.member === false ? 'You are no longer a member of this group.' : 'Read-only chat.';
}

function renderChatSub() {
  const c = S.byJid.get(S.active);
  const sub = $('chat-sub');
  const t = S.typing.get(S.active);
  if (t && t.until > Date.now()) {
    sub.textContent = (t.name ? t.name + ' is ' : '') + (t.audio ? 'recording audio…' : 'typing…');
    setTimeout(renderChatSub, t.until - Date.now() + 50);
    return;
  }
  const p = S.presence.get(S.active);
  if (p && p.online) sub.textContent = 'online';
  else if (p && p.last_seen > 0) sub.textContent = 'last seen ' + listTime(p.last_seen).toLowerCase() + ' ' + timeOf(p.last_seen);
  else if (c && c.kind === 'group') sub.textContent = 'click here for group info';
  else sub.textContent = c && c.phone ? c.phone : '';
}

function renderMessages(mode, focusId) {
  const box = $('messages');
  const prevHeight = box.scrollHeight, prevTop = box.scrollTop;
  box.textContent = '';
  if (S.hasMore) {
    box.append(h('button', { class: 'btn load-older', onclick: loadOlder }, 'Load older messages'));
  } else {
    box.append(h('button', { class: 'btn load-older', onclick: askPhoneForOlder, title: 'Asks your phone to send older history' },
      'Beginning of the archive · ask phone for older messages'));
  }
  const isGroup = S.active.endsWith('@g.us');
  let prev = null;
  const frag = document.createDocumentFragment();
  for (const m of S.messages) {
    if (!prev || !sameDay(new Date(prev.ts * 1000), new Date(m.ts * 1000))) {
      frag.append(h('div', { class: 'day' }, dayLabel(m.ts)));
      prev = null;
    }
    if (m.kind === 'system') {
      frag.append(h('div', { class: 'system', id: 'm-' + m.id }, m.text));
      prev = null;
      continue;
    }
    const first = !prev || prev.sender !== m.sender || prev.kind === 'system' || m.ts - prev.ts > 600;
    frag.append(messageEl(m, first, isGroup));
    prev = m;
  }
  box.append(frag);
  if (mode === 'bottom') box.scrollTop = box.scrollHeight;
  else if (mode === 'older') box.scrollTop = box.scrollHeight - prevHeight + prevTop;
  else if (mode === 'keep') box.scrollTop = prevTop;
  else if (mode === 'focus' && focusId) {
    const el = document.getElementById('m-' + focusId);
    if (el) { el.scrollIntoView({ block: 'center' }); el.classList.add('hl'); }
  }
}

function messageEl(m, first, isGroup) {
  const out = m.from_me;
  const bubble = h('div', { class: 'bubble' });
  if (m.kind === 'sticker' && !m.deleted) bubble.classList.add('sticker-bubble');
  if (isGroup && !out && first) bubble.append(h('div', { class: 'sender', style: `color:${colorFor(m.sender)}` }, m.sender_name));
  if (m.forwarded) bubble.append(h('div', { class: 'fwd' }, '↪ Forwarded'));
  if (m.quote) {
    bubble.append(h('div', { class: 'quote', onclick: () => jumpTo(m.quote.id) },
      h('div', { class: 'q-name' }, m.quote.sender || ''), m.quote.text || '…'));
  }
  const kept = m.deleted && (m.text || m.has_media || m.file_name) && m.kind !== 'revoked';
  if (m.deleted && !kept) {
    bubble.append(h('span', { class: 'deleted' }, '🚫 This message was deleted'));
  } else {
    if (kept) {
      bubble.classList.add('kept-deleted');
      bubble.append(h('div', { class: 'deleted-label', title: fullTime(m.deleted_at) },
        `🚫 Deleted by ${m.deleted_by || 'sender'} · ${timeOf(m.deleted_at)}`));
    }
    const media = mediaEl(m);
    if (media) bubble.append(media);
    const text = mediaText(m);
    if (text) bubble.append(h('span', { class: 'text' }, richText(text)));
    if (m.versions && m.versions.length) {
      bubble.append(h('details', { class: 'versions' },
        h('summary', {}, `✎ ${m.versions.length} earlier version${m.versions.length > 1 ? 's' : ''}`),
        ...m.versions.map((v) => h('div', { class: 'version' },
          h('div', { class: 'muted small' }, `until ${fullTime(v.replaced_at)}`), richText(v.text)))));
    }
  }
  bubble.append(h('span', { class: 'meta' },
    m.starred ? h('span', { class: 'star' }, '★') : null,
    m.edited ? 'edited ' : '',
    timeOf(m.ts), ' ',
    out ? ticks(m.ack || 1) : null));
  bubble.append(h('button', { class: 'msg-actions', title: 'Message options', onclick: (e) => { e.stopPropagation(); messageMenu(e, m); } }, '⌄'));
  bubble.addEventListener('contextmenu', (e) => { e.preventDefault(); messageMenu(e, m); });
  bubble.addEventListener('dblclick', () => { if (!m.deleted) setReply(m); });
  const wrap = h('div', { class: 'msg-wrap' }, bubble);
  if (m.reactions && m.reactions.length) {
    const groups = {};
    for (const r of m.reactions) (groups[r.emoji] = groups[r.emoji] || []).push(r.name);
    wrap.append(h('div', { class: 'reactions' }, ...Object.entries(groups).map(([emoji, names]) =>
      h('span', { class: 'reaction-pill', title: names.join(', ') }, emoji, names.length > 1 ? ' ' + names.length : ''))));
  }
  return h('div', { class: `msg ${out ? 'out' : 'in'}${first ? ' first' : ''}`, id: 'm-' + m.id }, wrap);
}

function mediaURL(m, download) {
  return `/api/media/${enc(m.chat)}/${enc(m.id)}${download ? '?download=1' : ''}`;
}

function mediaEl(m) {
  if (!m.has_media) {
    if (m.kind === 'location' || m.kind === 'live_location') {
      const coords = (m.text || '').match(/\((-?\d+\.\d+), (-?\d+\.\d+)\)/);
      if (coords) {
        return h('a', { href: `https://www.openstreetmap.org/?mlat=${coords[1]}&mlon=${coords[2]}#map=16/${coords[1]}/${coords[2]}`, target: '_blank', rel: 'noopener noreferrer' }, '📍 Open map');
      }
    }
    return null;
  }
  const url = mediaURL(m);
  const failed = (el, label) => {
    el.replaceWith(h('div', { class: 'media-missing', title: 'Retry', onclick: () => scheduleActive() },
      `${label} · not available (${m.media_error || 'media expired; ask the phone by opening this chat on it'})`));
  };
  switch (m.kind) {
    case 'image': {
      const img = h('img', { class: 'media-img', src: url, loading: 'lazy', alt: 'Photo' });
      img.onclick = () => lightbox(h('img', { src: url }));
      img.onerror = () => failed(img, '📷 Photo');
      return img;
    }
    case 'sticker': {
      const img = h('img', { class: 'sticker', src: url, loading: 'lazy', alt: 'Sticker' });
      img.onerror = () => failed(img, 'Sticker');
      return img;
    }
    case 'video': case 'gif': case 'video_note': {
      if (!m.downloaded && m.file_size > 16 << 20) {
        return h('div', { class: 'media-missing', onclick: (e) => e.currentTarget.replaceWith(h('video', { class: 'media', controls: true, src: url, autoplay: true })) },
          `▶ ${kindLabel[m.kind]} · ${size(m.file_size)}${m.duration ? ' · ' + clock(m.duration) : ''} — click to load`);
      }
      const v = h('video', { class: 'media', controls: true, preload: 'metadata', src: url, loop: m.kind === 'gif', muted: m.kind === 'gif' });
      v.onerror = () => failed(v, kindLabel[m.kind]);
      return v;
    }
    case 'voice': case 'audio': {
      const a = h('audio', { controls: true, preload: 'none', src: url });
      return h('div', {}, a, m.duration ? h('span', { class: 'muted small' }, (m.kind === 'voice' ? '🎤 ' : '🎵 ') + clock(m.duration)) : null);
    }
    default:
      return h('a', { class: 'doc', href: mediaURL(m, true), download: m.file_name || '' },
        h('span', { class: 'doc-icon' }, '📄'),
        h('div', {}, h('div', { class: 'doc-name' }, m.file_name || 'Document'),
          h('div', { class: 'muted small' }, [m.mime && m.mime.split('/')[1], size(m.file_size)].filter(Boolean).join(' · '))));
  }
}

function mediaText(m) {
  switch (m.kind) {
    case 'poll': return '📊 ' + m.text;
    case 'event': return '📅 ' + m.text;
    case 'contact': return '👤 ' + m.text;
    case 'location': case 'live_location': return '📍 ' + m.text;
    case 'group_invite': return '👥 ' + m.text;
    case 'unsupported': return `(unsupported message type ${m.text})`;
    case 'revoked': return '';
    default: return m.text;
  }
}

function lightbox(el) {
  const lb = $('lightbox');
  lb.textContent = '';
  lb.append(el);
  lb.hidden = false;
}
$('lightbox').onclick = () => { $('lightbox').hidden = true; $('lightbox').textContent = ''; };

async function loadOlder() {
  if (S.loadingOlder || !S.messages.length) return;
  S.loadingOlder = true;
  const first = S.messages[0], jid = S.active;
  const res = await attempt(() => api(`/api/chats/${enc(jid)}/messages?limit=80&before_ts=${first.ts}&before_pk=${first.pk}`));
  S.loadingOlder = false;
  if (!res || S.active !== jid) return;
  const known = new Set(S.messages.map((m) => m.id));
  S.messages = res.messages.filter((m) => !known.has(m.id)).concat(S.messages);
  S.hasMore = res.has_more;
  renderMessages('older');
}

async function askPhoneForOlder() {
  await attempt(() => api(`/api/chats/${enc(S.active)}/older`, { json: {} }), 'Asked your phone for older messages; they appear within a minute if it is online.');
}

async function refreshActive() {
  const jid = S.active;
  if (!jid) return;
  const res = await attempt(() => api(`/api/chats/${enc(jid)}/messages?limit=${Math.min(Math.max(S.messages.length, 80), 300)}`));
  if (!res || S.active !== jid) return;
  const box = $('messages');
  const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 80;
  const fresh = new Map(res.messages.map((m) => [m.id, m]));
  const oldest = res.messages.length ? res.messages[0] : null;
  const kept = S.messages.filter((m) => !fresh.has(m.id) && oldest && (m.ts < oldest.ts || (m.ts === oldest.ts && m.pk < oldest.pk)));
  S.messages = kept.concat(res.messages);
  if (!kept.length) S.hasMore = res.has_more;
  renderMessages(atBottom ? 'bottom' : 'keep');
  const c = S.byJid.get(jid);
  if (document.visibilityState === 'visible' && S.settings.autoRead && c && c.unread > 0) markRead();
}

async function jumpTo(id) {
  const el = document.getElementById('m-' + id);
  if (el) {
    el.scrollIntoView({ block: 'center', behavior: 'smooth' });
    el.classList.remove('hl'); void el.offsetWidth; el.classList.add('hl');
    return;
  }
  const res = await attempt(() => api(`/api/chats/${enc(S.active)}/messages?limit=80&around=${enc(id)}`));
  if (!res) return;
  S.messages = res.messages;
  S.hasMore = true;
  renderMessages('focus', id);
}

$('messages').addEventListener('scroll', () => {
  const box = $('messages');
  if (box.scrollTop < 60 && S.hasMore) loadOlder();
  $('to-bottom').hidden = box.scrollHeight - box.scrollTop - box.clientHeight < 300;
});
$('to-bottom').onclick = () => openChat(S.active);
$('btn-back').onclick = () => {
  $('app').classList.remove('chat-open');
  S.active = null;
  history.replaceState(null, '', '/');
  $('chat').hidden = true;
  $('empty').hidden = false;
  renderList();
};

// ---------- message menu

const quickEmoji = ['👍', '❤️', '😂', '😮', '😢', '🙏'];

function showMenu(e, items) {
  const menu = $('menu');
  menu.textContent = '';
  for (const it of items) if (it) menu.append(it);
  menu.hidden = false;
  const r = menu.getBoundingClientRect();
  menu.style.left = Math.min(e.clientX, innerWidth - r.width - 8) + 'px';
  menu.style.top = Math.min(e.clientY, innerHeight - r.height - 8) + 'px';
}
function menuItem(label, fn) {
  return h('button', { onclick: () => { $('menu').hidden = true; fn(); } }, label);
}
document.addEventListener('click', (e) => { if (!$('menu').contains(e.target)) $('menu').hidden = true; });
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  $('menu').hidden = true;
  $('lightbox').hidden = true;
  if (!$('modal').hidden) $('modal').hidden = true;
  else if (S.replyTo) { S.replyTo = null; renderReply(); }
});

function messageMenu(e, m) {
  const mine = m.from_me;
  const myReaction = (m.reactions || []).find((r) => r.me);
  const emojiRow = m.deleted || m.chat.endsWith('@newsletter') ? null : h('div', { class: 'emoji-row' }, ...quickEmoji.map((em) =>
    h('button', { onclick: () => { $('menu').hidden = true; react(m, myReaction && myReaction.emoji === em ? '' : em); } }, em)));
  showMenu(e, [
    emojiRow,
    m.deleted ? null : menuItem('Reply', () => setReply(m)),
    m.text ? menuItem('Copy text', () => navigator.clipboard.writeText(m.text).then(() => toast('Copied'))) : null,
    m.deleted ? null : menuItem(m.starred ? 'Unstar' : 'Star', () => attempt(() => api(`/api/chats/${enc(m.chat)}/messages/${enc(m.id)}/star`, { json: { starred: !m.starred } }))),
    mine && m.kind === 'text' && !m.deleted ? menuItem('Edit', () => editMessage(m)) : null,
    m.has_media ? menuItem('Download', () => { location.href = mediaURL(m, true); }) : null,
    m.deleted ? null : menuItem('Forward text', () => forwardDialog(m)),
    menuItem('Message info', () => infoDialog(m)),
    !m.deleted ? menuItem(mine ? 'Delete for everyone' : 'Delete for everyone (admin)', () => deleteMessage(m)) : null,
  ]);
}

async function react(m, emoji) {
  await attempt(() => api(`/api/chats/${enc(m.chat)}/messages/${enc(m.id)}/react`, { json: { emoji } }));
}

function editMessage(m) {
  const ta = h('textarea', { rows: 4, style: 'width:100%' });
  ta.value = m.text;
  modal('Edit message', [ta], [
    h('button', { class: 'btn', onclick: closeModal }, 'Cancel'),
    h('button', { class: 'btn primary', onclick: async () => {
      if (await attempt(() => api(`/api/chats/${enc(m.chat)}/messages/${enc(m.id)}/edit`, { json: { text: ta.value } }))) closeModal();
    } }, 'Save'),
  ]);
  ta.focus();
}

function deleteMessage(m) {
  modal('Delete message?', [h('p', {}, 'The message will be deleted for everyone in this chat.')], [
    h('button', { class: 'btn', onclick: closeModal }, 'Cancel'),
    h('button', { class: 'btn danger', onclick: async () => {
      if (await attempt(() => api(`/api/chats/${enc(m.chat)}/messages/${enc(m.id)}/delete`, { json: {} }))) closeModal();
    } }, 'Delete for everyone'),
  ]);
}

function infoDialog(m) {
  const rows = [['Sent', fullTime(m.ts)], ['From', m.sender_name + (m.sender ? ` (${m.sender})` : '')], ['Message ID', m.id]];
  if (m.deleted) rows.push(['Deleted', `${fullTime(m.deleted_at)} by ${m.deleted_by || 'sender'}`]);
  for (const v of m.versions || []) rows.push([`Until ${fullTime(v.replaced_at)}`, v.text]);
  if (m.from_me) rows.push(['Status', ['pending', 'sent', 'delivered', 'read', 'played'][m.ack || 1]]);
  if (m.file_name) rows.push(['File', m.file_name]);
  if (m.mime) rows.push(['Type', m.mime]);
  if (m.file_size) rows.push(['Size', size(m.file_size)]);
  if (m.reactions) rows.push(['Reactions', m.reactions.map((r) => `${r.emoji} ${r.name}`).join(', ')]);
  modal('Message info', rows.map(([k, v]) => h('div', { class: 'kv' }, h('span', { class: 'k' }, k), h('span', {}, v))),
    [h('button', { class: 'btn', onclick: closeModal }, 'Close')]);
}

function forwardDialog(m) {
  pickChat('Forward to…', async (jid) => {
    const text = m.text || '';
    if (!text) return toast('Only text can be forwarded from here.');
    if (await attempt(() => api(`/api/chats/${enc(jid)}/messages`, { json: { text } }), 'Forwarded')) openChat(jid);
  });
}

// ---------- reply / compose / attachments

function setReply(m) {
  S.replyTo = m;
  renderReply();
  $('input').focus();
}
function renderReply() {
  const bar = $('reply-bar');
  bar.textContent = '';
  if (!S.replyTo) { bar.hidden = true; return; }
  const m = S.replyTo;
  bar.append(h('div', { class: 'quote' }, h('div', { class: 'q-name' }, m.sender_name), previewText(m) || '…'),
    h('button', { class: 'icon', onclick: () => { S.replyTo = null; renderReply(); } }, '✕'));
  bar.hidden = false;
}

const input = $('input');
function autosize() { input.style.height = 'auto'; input.style.height = Math.min(input.scrollHeight, 140) + 'px'; }
let typingTimer = null, typingSent = false;
input.addEventListener('input', () => {
  autosize();
  if (!S.active) return;
  if (!typingSent) { typingSent = true; api(`/api/chats/${enc(S.active)}/typing`, { json: { typing: true } }).catch(() => {}); }
  clearTimeout(typingTimer);
  typingTimer = setTimeout(() => { typingSent = false; api(`/api/chats/${enc(S.active)}/typing`, { json: { typing: false } }).catch(() => {}); }, 3000);
});
input.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
});
input.addEventListener('paste', (e) => {
  const files = [...(e.clipboardData && e.clipboardData.files || [])];
  if (files.length) { e.preventDefault(); addFiles(files); }
});
$('btn-send').onclick = send;
$('btn-attach').onclick = () => $('file').click();
$('file').onchange = (e) => { addFiles([...e.target.files]); e.target.value = ''; };
$('main').addEventListener('dragover', (e) => { if (S.active) e.preventDefault(); });
$('main').addEventListener('drop', (e) => {
  if (!S.active) return;
  e.preventDefault();
  addFiles([...e.dataTransfer.files]);
});

function addFiles(files) {
  S.files = S.files.concat(files);
  renderAttach();
}
function renderAttach() {
  const bar = $('attach-bar');
  bar.textContent = '';
  if (!S.files.length) { bar.hidden = true; return; }
  const cap = h('input', { type: 'text', id: 'caption', placeholder: 'Caption (optional)' });
  const asFile = h('input', { type: 'checkbox', id: 'as-file' });
  bar.append(
    h('div', { class: 'files' }, ...S.files.map((f) => h('span', { class: 'tag' }, `${f.name} · ${size(f.size)}`))),
    cap,
    h('label', { class: 'small muted', title: 'Send photos and videos as documents (original quality)' }, asFile, ' as file'),
    h('button', { class: 'icon', title: 'Remove', onclick: () => { S.files = []; renderAttach(); } }, '✕'));
  bar.hidden = false;
  cap.focus();
  cap.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); send(); } });
}

let sending = false;
async function send() {
  if (sending || !S.active) return;
  const jid = S.active;
  const text = input.value;
  const replyTo = S.replyTo ? S.replyTo.id : '';
  if (!text.trim() && !S.files.length) return;
  sending = true;
  try {
    if (S.files.length) {
      const caption = ($('caption') && $('caption').value) || text;
      const asFile = $('as-file') && $('as-file').checked;
      for (const [i, f] of S.files.entries()) {
        const fd = new FormData();
        fd.append('file', f, f.name);
        if (i === 0 && caption) fd.append('caption', caption);
        if (i === 0 && replyTo) fd.append('reply_to', replyTo);
        if (asFile) fd.append('as_file', '1');
        toast(`Sending ${f.name}…`, 60000);
        await api(`/api/chats/${enc(jid)}/media`, { form: fd });
      }
      $('toast').hidden = true;
      S.files = [];
      renderAttach();
    } else {
      await api(`/api/chats/${enc(jid)}/messages`, { json: { text, reply_to: replyTo } });
    }
    input.value = '';
    autosize();
    S.replyTo = null;
    renderReply();
    typingSent = false;
    await refreshActive();
    $('messages').scrollTop = $('messages').scrollHeight;
  } catch (e) {
    toast(e.message, 6000);
  } finally {
    sending = false;
  }
}

// ---------- chat menu

$('btn-menu').onclick = (e) => {
  e.stopPropagation();
  const c = S.byJid.get(S.active) || {};
  const muted = c.muted_until === -1 || c.muted_until > Date.now() / 1000;
  const chatApi = (path, body, msg) => attempt(() => api(`/api/chats/${enc(S.active)}/${path}`, { json: body }), msg);
  showMenu(e, [
    menuItem('Chat info', toggleInfo),
    c.unread > 0 || c.marked_unread ? menuItem('Mark as read', markRead) : null,
    menuItem(c.pinned ? 'Unpin chat' : 'Pin chat', () => chatApi('pin', { pinned: !c.pinned })),
    menuItem(c.archived ? 'Unarchive chat' : 'Archive chat', () => chatApi('archive', { archived: !c.archived })),
    muted ? menuItem('Unmute', () => chatApi('mute', { duration: 'off' }))
      : menuItem('Mute for 8 hours', () => chatApi('mute', { duration: '8h' })),
    muted ? null : menuItem('Mute for 1 week', () => chatApi('mute', { duration: '1w' })),
    muted ? null : menuItem('Mute always', () => chatApi('mute', { duration: 'always' })),
    menuItem('Media, links and docs', () => { S.info = true; loadInfo('media'); }),
    menuItem('Ask phone for older messages', askPhoneForOlder),
  ]);
};
async function markRead() {
  await attempt(() => api(`/api/chats/${enc(S.active)}/read`, { json: {} }));
}
$('btn-chat-search').onclick = () => {
  const c = S.byJid.get(S.active);
  const q = h('input', { type: 'search', placeholder: `Search in ${c ? c.name : 'chat'}`, style: 'width:100%' });
  const results = h('div', {});
  let t = null;
  q.addEventListener('input', () => {
    clearTimeout(t);
    t = setTimeout(async () => {
      results.textContent = '';
      if (q.value.trim().length < 2) return;
      const list = await attempt(() => api(`/api/search?q=${enc(q.value)}&chat=${enc(S.active)}&limit=100`));
      if (!list) return;
      if (!list.length) results.append(h('p', { class: 'muted' }, 'No results.'));
      for (const m of list) {
        results.append(h('div', { class: 'item', onclick: () => { closeModal(); openChat(m.chat, m.id); } },
          h('div', { class: 'item-body' },
            h('div', { class: 'item-top' }, h('span', { class: 'item-name small' }, m.from_me ? 'You' : m.sender_name), h('span', { class: 'item-time' }, fullTime(m.ts))),
            h('div', { class: 'item-bottom' }, h('span', { class: 'item-preview' }, richText(previewText(m), q.value))))));
      }
    }, 250);
  });
  modal('Search messages', [q, results], [h('button', { class: 'btn', onclick: closeModal }, 'Close')]);
  q.focus();
};

// ---------- info panel with contact notes

$('btn-info').onclick = toggleInfo;
$('chat-title-box').onclick = toggleInfo;
function toggleInfo() {
  S.info = !S.info;
  if (S.info) loadInfo(); else $('info').hidden = true;
}

async function loadInfo(tab) {
  const jid = S.active;
  if (!jid) return;
  const data = await attempt(() => api(`/api/chats/${enc(jid)}`));
  if (!data || S.active !== jid) return;
  S.infoData = data;
  const panel = $('info');
  panel.textContent = '';
  panel.hidden = false;
  panel.append(h('div', { class: 'info-head' },
    h('button', { class: 'icon', onclick: () => { S.info = false; panel.hidden = true; } }, '✕'),
    h('span', {}, data.group ? 'Group info' : 'Contact info')));
  const c = data.chat || {};
  panel.append(h('div', { class: 'info-sec' },
    avatar(jid, data.name, 'lg'),
    h('div', { class: 'info-name' }, data.name),
    data.name !== data.wa_name ? h('div', { class: 'info-phone' }, `WhatsApp name: ${data.wa_name}`) : null,
    data.contact ? h('div', { class: 'info-phone' }, data.contact.phone) : null,
    data.group ? h('div', { class: 'info-phone' }, `Group · ${data.group.count} members`) : null));

  panel.append(notesEditor(jid, data.meta));

  if (tab === 'media') {
    const sec = h('div', { class: 'info-sec' }, h('h3', {}, 'Media, links and docs'));
    const grid = h('div', { class: 'gallery' });
    sec.append(grid);
    panel.prepend(panel.firstChild);
    panel.insertBefore(sec, panel.children[1]);
    const res = await attempt(() => api(`/api/chats/${enc(jid)}/messages?media=1&limit=120`));
    if (res) {
      if (!res.messages.length) grid.append(h('p', { class: 'muted' }, 'No media.'));
      for (const m of res.messages.reverse()) {
        if (m.deleted) continue;
        if (m.kind === 'image' || m.kind === 'sticker') {
          grid.append(h('img', { src: mediaURL(m), loading: 'lazy', onclick: () => openChat(jid, m.id) }));
        } else {
          grid.append(h('div', { class: 'g-doc', onclick: () => openChat(jid, m.id) }, (kindLabel[m.kind] || m.kind) + (m.file_name ? '\n' + m.file_name : '')));
        }
      }
    }
  }

  const about = h('div', { class: 'info-sec' }, h('h3', {}, 'About'));
  if (data.group) {
    if (data.group.topic) about.append(h('div', { class: 'text', style: 'margin-bottom:8px' }, richText(data.group.topic)));
    about.append(kv('Created', data.group.created ? fullTime(data.group.created) + (data.group.owner ? ' by ' + data.group.owner : '') : '—'));
    if (data.group.community) about.append(kv('Type', 'Community'));
    if (data.group.parent) about.append(kv('Community', data.group.parent_name));
    if (data.group.announce) about.append(kv('Messages', 'Only admins can send'));
    if (!data.group.member) about.append(kv('Membership', 'You left this group'));
  }
  if (data.contact) {
    const ct = data.contact;
    if (ct.full_name) about.append(kv('Address book', ct.full_name));
    if (ct.push_name) about.append(kv('Push name', ct.push_name));
    if (ct.business_name) about.append(kv('Business', ct.business_name));
    about.append(kv('JID', jid));
  }
  if (c.ephemeral) about.append(kv('Disappearing', `${Math.round(c.ephemeral / 86400)} days`));
  if (data.activity) {
    const a = data.activity;
    about.append(kv('Messages', `${a.messages.toLocaleString()} (${a.media} with media)`));
    about.append(kv('First / last', `${fullTime(a.first)} / ${fullTime(a.last)}`));
    if (a.senders.length) about.append(kv('Most active', a.senders.map((s) => `${s.me ? 'You' : s.name} (${s.count})`).join(', ')));
  }
  panel.append(about);

  if (data.group && data.group.members) {
    const sec = h('div', { class: 'info-sec' }, h('h3', {}, `${data.group.members.length} members`));
    for (const p of data.group.members) {
      sec.append(h('div', { class: 'member', onclick: () => { if (!p.me) openChat(p.jid); } },
        avatar(p.jid, p.name, 'sm'),
        h('div', { class: 'grow' }, h('div', {}, p.me ? 'You' : p.name, ' ', prioBadge(p.priority) || ''), h('div', { class: 'muted small' }, p.phone || '')),
        p.role !== 'member' ? h('span', { class: 'role' }, p.role === 'superadmin' ? 'Group owner' : 'Admin') : null));
    }
    panel.append(sec);
  }
  if (data.contact && data.contact.groups && data.contact.groups.length) {
    const sec = h('div', { class: 'info-sec' }, h('h3', {}, `${data.contact.groups.length} groups in common`));
    for (const g of data.contact.groups) sec.append(h('div', { class: 'member', onclick: () => openChat(g.jid) }, avatar(g.jid, g.name, 'sm'), h('div', { class: 'grow' }, g.name)));
    panel.append(sec);
  }
}

function kv(k, v) { return h('div', { class: 'kv' }, h('span', { class: 'k' }, k), h('span', {}, v)); }

function notesEditor(jid, meta) {
  const alias = h('input', { type: 'text', placeholder: 'Custom display name' });
  alias.value = meta.alias || '';
  const prio = h('select', {}, h('option', { value: '0' }, 'Not set'),
    ...[5, 4, 3, 2, 1].map((p) => h('option', { value: String(p), selected: meta.priority === p }, `${p} – ${prioNames[p]}`)));
  const tags = h('input', { type: 'text', placeholder: 'family, work, …' });
  tags.value = (meta.tags || []).join(', ');
  const desc = h('textarea', { placeholder: 'Who is this, context, things to remember…' });
  desc.value = meta.description || '';
  const fieldsBox = h('div', { class: 'form-grid' });
  const addField = (k = '', v = '') => {
    const kIn = h('input', { type: 'text', placeholder: 'Field (e.g. birthday)' });
    const vIn = h('input', { type: 'text', placeholder: 'Value' });
    kIn.value = k; vIn.value = v;
    const row = h('div', { class: 'field-row' }, kIn, vIn, h('button', { class: 'icon', title: 'Remove', onclick: () => row.remove() }, '✕'));
    fieldsBox.append(row);
  };
  for (const [k, v] of Object.entries(meta.fields || {})) addField(k, v);
  const save = async () => {
    const fields = {};
    for (const row of fieldsBox.children) {
      const [k, v] = row.querySelectorAll('input');
      if (k.value.trim() && v.value.trim()) fields[k.value.trim()] = v.value.trim();
    }
    const body = { alias: alias.value, priority: Number(prio.value), tags: tags.value.split(',').map((t) => t.trim()).filter(Boolean), description: desc.value, fields };
    if (await attempt(() => api(`/api/meta/${enc(jid)}`, { method: 'PUT', json: body }), 'Notes saved')) refreshChats();
  };
  return h('div', { class: 'info-sec' },
    h('h3', {}, 'Your notes (private, visible to your AI assistant via MCP)'),
    h('div', { class: 'form-grid' },
      h('label', {}, 'Alias'), alias,
      h('label', {}, 'Priority'), prio,
      h('label', {}, 'Tags (comma separated)'), tags,
      h('label', {}, 'Description'), desc,
      h('label', {}, 'Custom fields'), fieldsBox,
      h('div', { class: 'row' }, h('button', { class: 'btn', onclick: () => addField() }, '+ Field'), h('div', { class: 'grow' }),
        h('button', { class: 'btn primary', onclick: save }, 'Save notes'))));
}

// ---------- dialogs

function modal(title, body, actions) {
  const card = $('modal-card');
  card.textContent = '';
  card.append(h('h2', {}, title), ...body, h('div', { class: 'modal-actions' }, ...actions));
  $('modal').hidden = false;
}
function closeModal() { $('modal').hidden = true; }
$('modal').addEventListener('click', (e) => { if (e.target === $('modal')) closeModal(); });

function pickChat(title, onPick) {
  const q = h('input', { type: 'search', placeholder: 'Search name or number', style: 'width:100%' });
  const list = h('div', { style: 'margin-top:10px' });
  const render = () => {
    list.textContent = '';
    const f = fold(q.value);
    const chats = S.chats.filter((c) => c.kind === 'direct' || c.kind === 'group')
      .filter((c) => !f || fold(c.name + ' ' + c.phone + ' ' + c.jid).includes(f)).slice(0, 60);
    for (const c of chats) list.append(h('div', { class: 'item', onclick: () => { closeModal(); onPick(c.jid); } }, avatar(c.jid, c.name, 'sm'),
      h('div', { class: 'item-body' }, h('div', { class: 'item-name' }, c.name), h('div', { class: 'muted small' }, c.phone || c.kind))));
    const digits = q.value.replace(/[^\d]/g, '');
    if (digits.length >= 7) {
      list.prepend(h('div', { class: 'item', onclick: () => { closeModal(); onPick(digits + '@s.whatsapp.net'); } },
        h('div', { class: 'call-icon' }, '＋'), h('div', { class: 'item-body' }, `Message +${digits}`)));
    }
  };
  q.addEventListener('input', render);
  render();
  modal(title, [q, list], [h('button', { class: 'btn', onclick: closeModal }, 'Close')]);
  q.focus();
}

function newChatDialog() {
  pickChat('New chat', (jid) => openChat(jid));
}

function settingsDialog() {
  const toggle = (key, label, hint, onchange) => {
    const cb = h('input', { type: 'checkbox' });
    cb.checked = S.settings[key];
    cb.onchange = () => { S.settings[key] = cb.checked; saveSettings(); onchange && onchange(cb.checked); };
    return h('label', { class: 'setting' }, cb, h('div', {}, h('div', {}, label), h('div', { class: 'muted small' }, hint)));
  };
  const status = h('div', { class: 'muted small' }, 'Loading…');
  api('/api/status').then((r) => {
    const d = r.daemon, st = r.stats;
    status.textContent = '';
    status.append(
      kv('Account', `${d.account ? '+' + d.account.split('@')[0] : '—'} ${d.push_name || ''}`),
      kv('Connection', d.state + (d.connected_at ? ` since ${fullTime(Date.parse(d.connected_at) / 1000)}` : '')),
      kv('Archive', `${st.Messages.toLocaleString()} messages, ${st.Chats} chats, ${(st.DBBytes / 1048576).toFixed(1)} MB`),
      kv('History sync', d.history && d.history.batches ? `${d.history.batches} batches, ${d.history.messages} messages, ${d.history.last_type} ${d.history.last_progress}%` : 'waiting for the phone'),
      d.catch_up && d.catch_up.offline_seconds ? kv('Last catch-up', `${Math.round(d.catch_up.offline_seconds / 60)} min offline → ${d.catch_up.received} messages recovered${d.catch_up.done ? '' : ' (in progress)'}`) : '',
      d.last_error ? kv('Last error', d.last_error) : '');
  }).catch(() => {});
  modal('Settings', [
    status,
    h('hr', { style: 'border:0;border-top:1px solid var(--line);margin:14px 0' }),
    toggle('notify', 'Desktop notifications', 'Show a notification for new messages while this tab is open.', (on) => {
      if (on && 'Notification' in window) Notification.requestPermission();
    }),
    toggle('autoRead', 'Send read receipts when opening chats', 'Like the official client: opening a chat marks it read (blue ticks).'),
    toggle('online', 'Appear online while this tab is visible', 'Your phone gets no notifications while you appear online, exactly like WhatsApp Web.', (on) => { presenceState = null; setPresence(on && document.visibilityState === 'visible'); }),
  ], [
    h('button', { class: 'btn danger', onclick: () => {
      modal('Unlink this device?', [h('p', {}, 'wamcp stops syncing until you link it again. The archive stays on disk.')], [
        h('button', { class: 'btn', onclick: closeModal }, 'Cancel'),
        h('button', { class: 'btn danger', onclick: async () => { if (await attempt(() => api('/api/device/logout', { json: {} }))) { closeModal(); showPair(); } } }, 'Unlink'),
      ]);
    } }, 'Unlink device'),
    h('button', { class: 'btn', onclick: async () => { await api('/api/logout', { json: {} }); location.reload(); } }, 'Sign out'),
    h('button', { class: 'btn primary', onclick: closeModal }, 'Done'),
  ]);
}

window.addEventListener('hashchange', () => {
  const jid = decodeURIComponent(location.hash.slice(1));
  if (jid && jid !== S.active) openChat(jid);
});

boot();
