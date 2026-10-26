// stradl dashboard. Plain JavaScript with no dependencies, so it works
// offline and under a strict Content-Security-Policy: no inline scripts or
// styles, and every piece of session text goes in through textContent.
'use strict';

(() => {
  const REFRESH_MS = 15000; // overview refresh while the page is visible
  const RELTIME_MS = 30000; // "2 min ago" redraw
  const SYNC_POLL_MS = 1500; // overview polling after Sync now
  const SYNC_TIMEOUT_MS = 90000; // stop showing "Syncing…" after this
  const PAGE = 100; // table rows before "Show all"
  const DAY_MS = 86400000;

  // short is the label narrow windows show in the sessions table.
  const STATES = {
    'conflict': { label: 'Conflict', cls: 'st-conflict', icon: 'i-alert', rank: 0 },
    'pending-upload': { label: 'Waiting to upload', short: 'To upload', cls: 'st-pending', icon: 'i-up', rank: 1 },
    'pending-download': { label: 'Waiting to download', short: 'To download', cls: 'st-pending', icon: 'i-down', rank: 2 },
    'merging': { label: 'Merging', cls: 'st-pending', icon: 'i-merge', rank: 3 },
    'held': { label: 'Open in Claude', short: 'Open', cls: 'st-held', icon: 'i-clock', rank: 4 },
    'not-synced': { label: 'Not synced', cls: 'st-not', icon: null, rank: 5 },
    'synced': { label: 'Synced', cls: 'st-synced', icon: 'i-check', rank: 6 },
  };

  const EXPLAIN = {
    'synced': 'In sync. This Mac and your storage have the same copy, so each of your other Macs has it after its next sync.',
    'pending-upload': 'Changed on this Mac since the last sync. The next sync uploads the new turns so your other Macs get them.',
    'pending-download': 'Changed on another Mac. The next sync brings the new turns to this Mac.',
    'merging': 'Changed on this Mac and on another one since the last sync. The next sync merges both, line by line, so no turn is lost.',
    'held': 'Open in a running Claude session on this Mac. What you add here keeps uploading, but changes from your other Macs wait until the session closes, so the conversation is never rewritten underneath you.',
    'conflict': 'Changed on two Macs in a way that could not be merged. Both versions are kept: the other one is saved next to it as a .conflict copy. Run stradl conflicts in Terminal to choose which one stays.',
    'not-synced': 'Not synced. It is outside what Stradl syncs, or it has not been uploaded yet.',
  };

  const KINDS = {
    prompt: { icon: 'i-prompt', label: 'Prompt' },
    reply: { icon: 'i-reply', label: 'Reply' },
    edit: { icon: 'i-edit', label: 'Edit' },
    write: { icon: 'i-write', label: 'New file' },
    command: { icon: 'i-command', label: 'Command' },
    compact: { icon: 'i-compact', label: 'Compacted' },
  };

  const PENDING_KINDS = [
    { key: 'upload', label: 'To upload', icon: 'i-up' },
    { key: 'download', label: 'To download', icon: 'i-down' },
    { key: 'merge', label: 'To merge', icon: 'i-merge' },
    { key: 'deferred', label: 'Held for Claude', icon: 'i-clock', cls: 'held' },
    { key: 'delete', label: 'To delete everywhere', icon: 'i-x' },
    { key: 'trash', label: 'To trash here', icon: 'i-archive' },
    { key: 'conflicts', label: 'Conflicts', icon: 'i-alert', cls: 'bad' },
  ];

  const ACTIONS = {
    upload: { label: 'Upload', cls: 'st-pending', icon: 'i-up' },
    download: { label: 'Download', cls: 'st-pending', icon: 'i-down' },
    merge: { label: 'Merge', cls: 'st-pending', icon: 'i-merge' },
    restore: { label: 'Restore', cls: 'st-pending', icon: 'i-up' },
    deferred: { label: 'Held', cls: 'st-held', icon: 'i-clock' },
    delete: { label: 'Delete', cls: 'st-pending', icon: 'i-x' },
    trash: { label: 'To trash', cls: 'st-pending', icon: 'i-archive' },
  };

  const S = {
    ov: null,
    sig: '',
    byId: new Map(),
    devIdx: new Map(),
    conn: 'ok', // ok | offline | expired | error
    connMsg: '',
    loading: false,
    loadedAt: 0,
    f: { q: '', project: '', mac: '', state: '', archived: false },
    sort: { key: 'last', dir: 'desc' },
    showAll: false,
    sync: null, // { started, prev, saw } while a Sync now is in flight
    detail: null, // { id, data, error }
    returnFocus: null,
  };

  // ---------- DOM helpers ----------

  const $ = (sel, root = document) => root.querySelector(sel);
  const SVGNS = 'http://www.w3.org/2000/svg';

  function h(tag, props, ...kids) {
    const el = document.createElement(tag);
    if (props) {
      for (const [k, v] of Object.entries(props)) {
        if (v == null || v === false) continue;
        if (k === 'class') el.className = v;
        else if (k === 'text') el.textContent = v;
        else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
        else el.setAttribute(k, v === true ? '' : String(v));
      }
    }
    put(el, kids);
    return el;
  }

  function put(el, kids) {
    for (const k of kids.flat(Infinity)) {
      if (k == null || k === false || k === '') continue;
      el.append(k.nodeType ? k : document.createTextNode(String(k)));
    }
    return el;
  }

  function fill(el, ...kids) {
    el.replaceChildren();
    return put(el, kids);
  }

  function icon(id, cls) {
    const svg = document.createElementNS(SVGNS, 'svg');
    svg.setAttribute('class', cls ? 'ico ' + cls : 'ico');
    svg.setAttribute('aria-hidden', 'true');
    const use = document.createElementNS(SVGNS, 'use');
    use.setAttribute('href', '#' + id);
    svg.append(use);
    return svg;
  }

  // ---------- formatting ----------

  const fmtTime = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' });
  const fmtShort = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric' });
  const fmtShortY = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
  const fmtDay = new Intl.DateTimeFormat(undefined, { weekday: 'short', month: 'short', day: 'numeric' });
  const fmtDayLong = new Intl.DateTimeFormat(undefined, { weekday: 'long', month: 'long', day: 'numeric' });
  const fmtWeekday = new Intl.DateTimeFormat(undefined, { weekday: 'short' });
  const fmtLong = new Intl.DateTimeFormat(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
  const fmtNum = new Intl.NumberFormat();

  const num = (n) => fmtNum.format(n || 0);
  const plural = (n, one, many) => num(n) + ' ' + (n === 1 ? one : (many || one + 's'));
  const ts = (iso) => (iso ? Date.parse(iso) : NaN);
  const startOfDay = (t) => { const d = new Date(t); d.setHours(0, 0, 0, 0); return d.getTime(); };

  function rel(iso) {
    const t = ts(iso);
    if (!t) return 'never';
    const d = Date.now() - t;
    if (d < 45000) return 'just now';
    const m = Math.round(d / 60000);
    if (m < 60) return m + ' min ago';
    const hr = Math.round(d / 3600000);
    if (hr < 24) return hr + ' hr ago';
    const days = Math.round((startOfDay(Date.now()) - startOfDay(t)) / DAY_MS);
    if (days <= 1) return 'yesterday';
    if (days < 7) return days + ' days ago';
    return dateShort(t);
  }

  function dateShort(t) {
    const d = new Date(t);
    return (d.getFullYear() === new Date().getFullYear() ? fmtShort : fmtShortY).format(d);
  }

  function longDate(iso) {
    const t = ts(iso);
    return t ? fmtLong.format(new Date(t)) : '—';
  }

  function timeRange(a, b) {
    const ta = ts(a);
    const tb = ts(b);
    if (!ta) return '';
    if (!tb || Math.abs(tb - ta) < 60000) return fmtTime.format(new Date(ta));
    try {
      return fmtTime.formatRange(new Date(ta), new Date(tb));
    } catch (e) {
      return fmtTime.format(new Date(ta)) + ' – ' + fmtTime.format(new Date(tb));
    }
  }

  function bytes(n) {
    if (!n) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0;
    while (n >= 1000 && i < units.length - 1) { n /= 1000; i++; }
    return (i === 0 || n >= 100 ? Math.round(n) : n.toFixed(1)) + ' ' + units[i];
  }

  function surfaceLabel(s) {
    switch (s) {
      case 'desktop': case 'claude-desktop': return 'Claude app';
      case 'cli': return 'Terminal';
      case 'claude-vscode': return 'VS Code';
      case 'sdk-ts': case 'sdk-py': case 'sdk-cli': return 'Agent SDK';
      case '': case undefined: case null: return '';
      default: return s;
    }
  }

  // relPath shows a file relative to the session's folder, or the home
  // folder as ~, which is how people think of their files.
  function relPath(p, root) {
    if (!p) return '';
    const r = (root || '').replace(/\/+$/, '');
    if (r && p.startsWith(r + '/')) return p.slice(r.length + 1);
    const m = /^\/Users\/[^/]+\//.exec(p);
    return m ? '~/' + p.slice(m[0].length) : p;
  }

  // midTrunc shortens a path in the middle, keeping the file name whole.
  function midTrunc(s, max) {
    if (!s || s.length <= max) return s || '';
    const slash = s.lastIndexOf('/');
    const tail = slash > 0 ? s.slice(slash) : '';
    if (!tail || tail.length >= max - 6) return '…' + s.slice(-(max - 1));
    return s.slice(0, max - tail.length - 1) + '…' + tail;
  }

  function shellQuote(s) {
    return /^[\w@%+=:,./~-]+$/.test(s) ? s : "'" + s.replace(/'/g, "'\\''") + "'";
  }

  function cmpVersion(a, b) {
    const pa = String(a || '').replace(/^v/, '').split(/[.-]/).map((x) => parseInt(x, 10) || 0);
    const pb = String(b || '').replace(/^v/, '').split(/[.-]/).map((x) => parseInt(x, 10) || 0);
    for (let i = 0; i < 3; i++) if ((pa[i] || 0) !== (pb[i] || 0)) return (pa[i] || 0) - (pb[i] || 0);
    return 0;
  }

  function listJoin(parts) {
    parts = parts.filter(Boolean);
    if (parts.length <= 1) return parts.join('');
    return parts.slice(0, -1).join(', ') + ' and ' + parts[parts.length - 1];
  }

  // ---------- small components ----------

  function devClass(name) {
    const i = S.devIdx.get(name);
    return i == null || i > 3 ? 'dev-x' : 'dev-' + i;
  }

  function chipDev(name) {
    const isThis = S.ov && name === S.ov.thisDevice;
    return h('span', { class: 'chip chip-dev ' + devClass(name), title: isThis ? name + ' (this Mac)' : name }, h('span', { text: name }));
  }

  function chipProject(project, path) {
    return h('span', { class: 'chip', title: path || project }, icon('i-folder'), h('span', { text: project || 'No folder' }));
  }

  function stateBadge(state, compact) {
    const st = STATES[state] || STATES['not-synced'];
    const label = compact && st.short
      ? [h('span', { class: 'lbl-long', text: st.label }), h('span', { class: 'lbl-short', 'aria-hidden': 'true', text: st.short })]
      : st.label;
    return h('span', { class: 'badge ' + st.cls, title: compact ? st.label : null }, st.icon ? icon(st.icon) : null, label);
  }

  function fileChip(path, root) {
    const shown = relPath(path, root);
    return h('span', { class: 'file', title: path }, icon('i-file'), h('span', { text: midTrunc(shown, 46) }));
  }

  function relTime(iso, cls) {
    return h('span', { class: cls || null, 'data-rel': iso || '', title: longDate(iso) }, rel(iso));
  }

  // ---------- data ----------

  async function api(path, opts = {}) {
    const res = await fetch(path, {
      credentials: 'same-origin',
      cache: 'no-store',
      // The page's policy is no-referrer, under which a POST would carry
      // "Origin: null"; same-origin lets the server see where it came from.
      referrerPolicy: 'same-origin',
      ...opts,
      headers: { Accept: 'application/json', ...(opts.headers || {}) },
    });
    let body = null;
    try { body = await res.json(); } catch (e) { body = null; }
    if (!res.ok) {
      const err = new Error((body && body.error) || res.statusText || 'request failed');
      err.status = res.status;
      throw err;
    }
    return body;
  }

  const arr = (v) => (Array.isArray(v) ? v : []);

  // normalize fills in anything a provider left out, so rendering never
  // has to check.
  function normalize(ov) {
    ov.agent = ov.agent || {};
    ov.devices = arr(ov.devices);
    ov.days = arr(ov.days);
    ov.sessions = arr(ov.sessions);
    ov.warnings = arr(ov.warnings);
    ov.totals = ov.totals || {};
    ov.pending = ov.pending || {};
    ov.pending.files = arr(ov.pending.files);
    for (const d of ov.devices) d.recentSessionIds = arr(d.recentSessionIds);
    for (const s of ov.sessions) { s.devices = arr(s.devices); s.filesTouched = arr(s.filesTouched); }
    for (const day of ov.days) {
      day.sessions = arr(day.sessions);
      for (const ds of day.sessions) { ds.prompts = arr(ds.prompts); ds.filesTouched = arr(ds.filesTouched); ds.devices = arr(ds.devices); }
    }
    return ov;
  }

  async function load() {
    if (S.loading) return;
    S.loading = true;
    try {
      const ov = normalize(await api('/api/overview'));
      S.loadedAt = Date.now();
      const sig = JSON.stringify(ov, (k, v) => (k === 'generatedAt' ? undefined : v));
      const wasDown = S.conn !== 'ok';
      S.ov = ov;
      S.conn = 'ok';
      if (sig !== S.sig || wasDown) {
        S.sig = sig;
        renderAll();
      } else {
        renderStatus();
        tick();
      }
      if (S.sync) checkSync();
    } catch (e) {
      S.conn = e.status === 401 ? 'expired' : e.status ? 'error' : 'offline';
      S.connMsg = e.message;
      renderStatus();
      renderBanners();
      if (!S.ov) renderUnavailable();
    } finally {
      S.loading = false;
    }
  }

  // ---------- rendering ----------

  function renderAll() {
    const ov = S.ov;
    const active = document.activeElement;
    const fk = active && active.dataset ? active.dataset.fk : null;

    S.byId = new Map(ov.sessions.map((s) => [s.id, s]));
    S.devIdx = new Map();
    ov.devices.forEach((d, i) => S.devIdx.set(d.name, i));

    renderStatus();
    renderBanners();
    renderSummary();
    renderMacs();
    renderWeek();
    renderFilters();
    renderTable();
    renderPending();
    renderFooter();
    if (S.detail && S.detail.data) renderDrawer();

    if (fk && !document.body.contains(active)) {
      const again = document.querySelector('[data-fk="' + CSS.escape(fk) + '"]');
      if (again) again.focus({ preventScroll: true });
    }
  }

  function renderUnavailable() {
    const sum = $('#summary');
    sum.setAttribute('aria-busy', 'false');
    fill(sum, h('div', { class: 'hero' },
      h('div', { class: 'hero-icon error' }, icon('i-alert')),
      h('div', {},
        h('h1', { text: S.conn === 'expired' ? 'This link has expired' : "Can't load the dashboard" }),
        h('p', { text: S.conn === 'expired'
          ? 'Open the dashboard again from the Stradl menu bar icon, or run stradl dashboard in Terminal.'
          : S.connMsg || 'Stradl is not answering on this Mac.' }))));
  }

  function statusInfo() {
    if (S.conn === 'offline') return ['pill-error', "Can't reach Stradl", 'The dashboard server is not answering. Retrying.'];
    if (S.conn === 'expired') return ['pill-error', 'Link expired', 'Reopen the dashboard from the Stradl menu.'];
    if (!S.ov) return ['pill-idle', 'Loading…', ''];
    const a = S.ov.agent;
    if (S.sync || a.syncing) return ['pill-sync', 'Syncing…', 'Syncing with your other Macs'];
    if (a.paused) return ['pill-warn', 'Paused', 'Syncing is paused. Resume it from the Stradl menu.'];
    if (a.lastError) {
      const when = a.lastAttemptAt ? 'Last attempt ' + rel(a.lastAttemptAt) + '. ' : '';
      return ['pill-error', a.lastErrorSummary || 'Last sync failed', when + a.lastError];
    }
    if (a.lastSyncAt) {
      const label = 'Synced ' + rel(a.lastSyncAt);
      return [a.running ? 'pill-ok' : 'pill-warn', a.running ? label : label + ' · agent off', longDate(a.lastSyncAt)];
    }
    if (!a.running) return ['pill-warn', 'Agent not running', 'Run stradl agent install in Terminal.'];
    return ['pill-idle', 'Not synced yet', ''];
  }

  function renderStatus() {
    const [cls, text, title] = statusInfo();
    const pill = $('#status');
    pill.className = 'pill ' + cls;
    pill.title = title || '';
    $('.pill-text', pill).textContent = text;

    const btn = $('#sync-btn');
    const busy = !!S.sync || !!(S.ov && S.ov.agent.syncing);
    btn.disabled = busy || S.conn !== 'ok' || !S.ov;
    btn.classList.toggle('is-syncing', busy);
    btn.setAttribute('aria-label', busy ? 'Syncing' : 'Sync now');
  }

  function notice(kind, iconId, ...kids) {
    return h('div', { class: 'notice ' + (kind || '') }, icon(iconId), h('div', {}, ...kids));
  }

  function renderBanners() {
    const el = $('#banners');
    const out = [];
    if (S.conn === 'offline') {
      out.push(notice('error', 'i-alert', h('strong', { text: "Can't reach Stradl on this Mac. " }),
        'If you quit the agent, open the dashboard again from the menu bar or run ', h('code', { text: 'stradl dashboard' }), '. Retrying every 15 seconds.'));
    } else if (S.conn === 'expired') {
      out.push(notice('error', 'i-lock', h('strong', { text: 'This dashboard link has expired. ' }),
        'Stradl makes a new one each time it starts. Choose Open Dashboard… in the Stradl menu, or run ', h('code', { text: 'stradl dashboard' }), '.'));
    } else if (S.conn === 'error') {
      out.push(notice('error', 'i-alert', h('strong', { text: "Couldn't refresh. " }), S.connMsg));
    }
    if (S.ov) {
      const a = S.ov.agent;
      const n = a.pendingDesktopRestart || 0;
      if (n > 0) {
        out.push(notice('', 'i-restart',
          h('strong', { text: plural(n, 'session') + ' from your other Macs ' }),
          (n === 1 ? 'appears' : 'appear') + ' in the Claude sidebar after Claude restarts (Stradl menu → Restart Claude).'));
      }
      if (!a.running && S.conn === 'ok') {
        out.push(notice('warn', 'i-pause', h('strong', { text: 'The background agent is not running, ' }),
          'so nothing syncs on its own. Run ', h('code', { text: 'stradl agent install' }), ' in Terminal to keep this Mac in sync.'));
      }
    }
    fill(el, out);
  }

  function countStates(sessions) {
    const c = { synced: 0, pending: 0, held: 0, conflict: 0, not: 0 };
    for (const s of sessions) {
      if (s.syncState === 'synced') c.synced++;
      else if (s.syncState === 'held') c.held++;
      else if (s.syncState === 'conflict') c.conflict++;
      else if (s.syncState === 'not-synced') c.not++;
      else c.pending++;
    }
    return c;
  }

  function weekStats() {
    const ids = new Set();
    const projects = new Set();
    const macs = new Set();
    let prompts = 0;
    let busiest = null;
    for (const day of S.ov.days) {
      let dayPrompts = 0;
      for (const ds of day.sessions) {
        ids.add(ds.id);
        if (ds.project) projects.add(ds.project);
        ds.devices.forEach((d) => macs.add(d));
        dayPrompts += ds.turns || 0;
      }
      prompts += dayPrompts;
      if (dayPrompts > 0 && (!busiest || dayPrompts > busiest.n)) busiest = { label: day.label, n: dayPrompts };
    }
    return { sessions: ids.size, projects: projects.size, macs: macs.size, prompts, busiest };
  }

  function renderSummary() {
    const ov = S.ov;
    const a = ov.agent;
    const p = ov.pending;
    const t = ov.totals;
    const total = ov.sessions.length;
    const c = countStates(ov.sessions);
    const up = p.upload || 0;
    const down = p.download || 0;
    const merge = p.merge || 0;
    const waiting = up + down + merge;

    let tone = 'ok';
    let ico = 'i-check';
    let title;
    let sub;
    if (a.paused) {
      tone = 'pending'; ico = 'i-pause'; title = 'Syncing is paused';
      sub = 'Nothing syncs until you resume from the Stradl menu or run stradl agent resume.';
    } else if (a.lastError) {
      tone = 'error'; ico = 'i-alert'; title = 'Sync needs attention';
      sub = (a.lastErrorSummary ? a.lastErrorSummary + '. ' : '') +
        (a.lastSyncAt ? 'The last good sync was ' + rel(a.lastSyncAt) + '.' : 'No sync has finished yet.');
    } else if (total === 0) {
      tone = 'neutral'; ico = 'i-sync'; title = 'No sessions yet';
      sub = 'Start a Claude Code session on this Mac, or press Sync now to bring in sessions from your other Macs.';
    } else if (c.conflict > 0) {
      tone = 'error'; ico = 'i-alert'; title = plural(c.conflict, 'session') + ' with a conflict';
      sub = 'Both versions are kept. Run stradl conflicts in Terminal to choose which one stays.';
    } else if (waiting + (p.conflicts || 0) === 0) {
      title = 'Everything is in sync';
      sub = (total === 1 ? 'Your session' : 'All ' + num(total) + ' sessions') + ' and ' + plural(t.syncedFiles || 0, 'file') +
        ' on this Mac match your storage, so your other Macs get them on their next sync.';
      if (p.deferred) sub += ' ' + plural(p.deferred, 'change') + ' from other Macs ' + (p.deferred === 1 ? 'waits' : 'wait') + ' for a session open in Claude to close.';
    } else {
      tone = 'pending'; ico = 'i-clock'; title = plural(waiting, 'file') + ' waiting to sync';
      sub = 'The next sync ' + listJoin([up && 'uploads ' + up, down && 'downloads ' + down, merge && 'merges ' + merge]) +
        '. It runs on its own within a minute, or press Sync now.';
    }

    const w = weekStats();
    const files = t.files || 0;
    const synced = t.syncedFiles || 0;
    const ratio = files ? Math.min(1, synced / files) : 0;
    const parts = [];
    if (c.held) parts.push(c.held + ' open in Claude');
    if (c.pending) parts.push(c.pending + ' waiting');
    if (c.conflict) parts.push(plural(c.conflict, 'conflict'));
    if (c.not) parts.push(c.not + ' not synced');

    const meter = h('div', { class: 'meter' + (files && ratio < 1 ? ' warn' : ''), role: 'progressbar', 'aria-label': 'Files in sync',
      'aria-valuemin': '0', 'aria-valuemax': String(files), 'aria-valuenow': String(synced) }, h('span'));
    meter.firstChild.style.width = (ratio * 100).toFixed(1) + '%';

    const sum = $('#summary');
    sum.setAttribute('aria-busy', 'false');
    fill(sum,
      h('div', { class: 'hero' },
        h('div', { class: 'hero-icon ' + (tone === 'ok' ? '' : tone) }, icon(ico)),
        h('div', {}, h('h1', { text: title }), h('p', { text: sub }))),
      h('div', { class: 'tiles' },
        tile('#all', 'Sessions in sync', [num(c.synced), h('span', { class: 'of', text: ' / ' + num(total) })],
          total === 0 ? 'None yet' : parts.length ? parts.join(' · ') : 'All of them'),
        tile('#pending', 'Files in sync', [num(synced), h('span', { class: 'of', text: ' / ' + num(files) })], bytes(t.bytes) + ' in all', meter),
        tile('#week', 'Worked on this week', [num(w.sessions), h('span', { class: 'of', text: w.sessions === 1 ? ' session' : ' sessions' })],
          w.sessions ? plural(w.projects, 'project') + ' · ' + plural(w.macs, 'Mac') : 'Nothing in the last 7 days'),
        tile('#week', 'Prompts this week', [num(w.prompts)],
          w.busiest ? 'Busiest: ' + w.busiest.label + ' (' + num(w.busiest.n) + ')' : 'No prompts yet')));
  }

  function tile(href, label, value, detail, extra) {
    return h('a', { class: 'tile', href },
      h('div', { class: 'tile-label', text: label }),
      h('div', { class: 'tile-value' }, value),
      extra || null,
      h('div', { class: 'tile-detail', text: detail, title: detail }));
  }

  function renderMacs() {
    const ov = S.ov;
    const now = Date.now();
    const newest = ov.devices.reduce((v, d) => (cmpVersion(d.version, v) > 0 ? d.version : v), '');
    const cards = ov.devices.map((d) => {
      const last = ts(d.lastSyncAt);
      const stale = !last || now - last > DAY_MS;
      const thisErr = d.this && ov.agent.lastError;
      let health = ['ok', 'Healthy'];
      if (stale) health = ['stale', last ? 'Stale' : 'Never synced'];
      else if (!d.healthy || thisErr) health = ['bad', 'Needs attention'];
      const behind = newest && d.version && cmpVersion(d.version, newest) < 0;
      const week = d.recentSessionIds.length;
      return h('article', { class: 'card device ' + devClass(d.name), 'aria-label': d.name + (d.this ? ', this Mac' : '') },
        h('div', { class: 'device-top' },
          h('div', { class: 'device-ico' }, icon('i-laptop')),
          h('div', { class: 'device-id' },
            h('div', { class: 'device-name', text: d.name, title: d.name }),
            h('div', { class: 'device-sub' }, d.this ? h('span', { class: 'badge badge-this', text: 'This Mac' }) : h('span', { text: 'Another Mac' }))),
          h('span', { class: 'health ' + health[0], title: stale && last ? 'No sync in over 24 hours' : '' }, health[1])),
        h('dl', { class: 'kv' },
          h('dt', { text: 'Last sync' }), h('dd', { class: stale ? 'warn' : null }, last ? relTime(d.lastSyncAt) : 'never'),
          h('dt', { text: 'Sessions' }), h('dd', { text: num(d.sessions) }),
          h('dt', { text: 'This week' }), h('dd', { text: week ? plural(week, 'session') : 'No changes' }),
          h('dt', { text: 'Version' }), h('dd', { class: behind ? 'warn' : null,
            title: behind ? 'Older than ' + newest + ' on your other Macs. Run stradl update on that Mac.' : null },
          d.version ? d.version + (behind ? ' · update' : '') : '—')));
    });
    if (ov.devices.length <= 1) {
      cards.push(h('article', { class: 'card device ghost' },
        h('div', { class: 'device-top' }, h('div', { class: 'device-ico dev-x' }, icon('i-laptop')),
          h('div', {}, h('div', { class: 'device-name', text: 'Your other Macs' }), h('div', { class: 'device-sub', text: 'Not syncing yet' }))),
        h('p', { class: 'muted small' }, 'Install Stradl on each of your Macs with the same storage and passphrase. They appear here after their first sync.')));
    }
    fill($('#macs-body'), cards);
    const n = ov.devices.length;
    $('#macs-sub').textContent = n > 1 ? plural(n, 'Mac') + ' syncing into your storage' : 'Every Mac syncing into your storage';
  }

  function renderWeek() {
    const days = S.ov.days;
    const w = weekStats();
    $('#week-sub').textContent = w.sessions
      ? plural(w.sessions, 'session') + ' across ' + plural(w.projects, 'project') + ' · ' + plural(w.prompts, 'prompt')
      : 'What you worked on, day by day';

    // Prompts per day, oldest on the left: one series, so no legend; the
    // busiest day is labeled and every bar has a tooltip.
    const chart = $('#week-chart');
    const counts = days.map((d) => d.sessions.reduce((n, s) => n + (s.turns || 0), 0));
    const max = Math.max(0, ...counts);
    const cols = [];
    for (let i = days.length - 1; i >= 0; i--) {
      const d = days[i];
      const n = counts[i];
      const date = parseDate(d.date);
      const tip = d.label + ': ' + (n ? plural(n, 'prompt') + ' in ' + plural(d.sessions.length, 'session') : 'nothing');
      const bar = h('span', { class: 'wc-bar' + (n ? '' : ' zero') });
      bar.style.height = n ? Math.max(4, Math.round((n / max) * 44)) + 'px' : '2px';
      cols.push(h('button', { type: 'button', class: 'wc-col' + (i === 0 ? ' today' : ''), 'data-tip': tip, 'aria-label': tip, 'data-fk': 'wc-' + d.date,
        onclick: () => scrollToDay(d.date) },
      h('span', { class: 'wc-val', text: n && n === max ? num(n) : '' }), bar,
      h('span', { class: 'wc-day', text: i === 0 ? 'Today' : date ? fmtWeekday.format(date) : '' })));
    }
    fill(chart, days.length ? h('div', { class: 'sr-only', text: 'Prompts per day' }) : null, cols);
    chart.setAttribute('role', 'group');
    chart.setAttribute('aria-label', 'Prompts per day, last 7 days');

    const body = $('#week-body');
    if (!w.sessions) {
      fill(body, h('div', { class: 'card empty' },
        h('h3', { text: 'Nothing in the last 7 days' }),
        h('p', { text: 'Sessions you work on from any of your Macs show up here, day by day, once they sync.' })));
      return;
    }
    const out = [];
    let run = [];
    const flush = () => {
      if (!run.length) return;
      const first = run[run.length - 1];
      const last = run[0];
      const label = run.length === 1 ? first.label : first.label + ' – ' + last.label;
      out.push(h('div', { class: 'day-empty', id: 'day-' + last.date, 'data-dates': run.map((d) => d.date).join(' ') },
        h('b', { text: label }), run.length === 1 ? 'No sessions' : 'No sessions on these days'));
      run = [];
    };
    for (const d of days) {
      if (!d.sessions.length) { run.push(d); continue; }
      flush();
      out.push(renderDay(d));
    }
    flush();
    fill(body, out);
  }

  function parseDate(s) {
    const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s || '');
    return m ? new Date(+m[1], +m[2] - 1, +m[3]) : null;
  }

  function scrollToDay(date) {
    const el = document.getElementById('day-' + date) ||
      Array.from(document.querySelectorAll('[data-dates]')).find((x) => x.dataset.dates.split(' ').includes(date));
    if (el) el.scrollIntoView({ behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth', block: 'start' });
  }

  function renderDay(d) {
    const date = parseDate(d.date);
    const prompts = d.sessions.reduce((n, s) => n + (s.turns || 0), 0);
    const isRel = d.label === 'Today' || d.label === 'Yesterday';
    return h('div', { class: 'day', id: 'day-' + d.date },
      h('div', { class: 'day-label' },
        h('h3', { text: d.label }),
        date && isRel ? h('div', { class: 'day-date', text: fmtDayLong.format(date) }) : null,
        h('div', { class: 'day-stats', text: plural(d.sessions.length, 'session') + ' · ' + plural(prompts, 'prompt') })),
      h('div', { class: 'day-list' }, d.sessions.map((ds) => renderDaySession(d, ds))));
  }

  function renderDaySession(day, ds) {
    const s = S.byId.get(ds.id);
    const root = s ? s.projectPath : '';
    const open = () => openSession(ds.id);
    const files = ds.filesTouched;
    const shown = files.slice(0, 4);
    return h('article', { class: 'card ws', onclick: (e) => { if (!e.target.closest('button, a') && !window.getSelection().toString()) open(); } },
      h('div', { class: 'ws-top' },
        h('button', { type: 'button', class: 'ws-title', 'data-fk': 'ws-' + day.date + '-' + ds.id, onclick: open, text: ds.title || 'Untitled session' }),
        h('span', { class: 'ws-time', text: timeRange(ds.firstAt, ds.lastAt) })),
      h('div', { class: 'ws-meta' },
        ds.project ? chipProject(ds.project, root) : null,
        ds.devices.map(chipDev),
        h('span', { class: 'ws-turns', text: plural(ds.turns || 0, 'prompt') }),
        s ? h('span', { class: 'ws-state' }, stateBadge(s.syncState)) : null),
      ds.prompts.length ? h('ul', { class: 'quotes', 'aria-label': 'Prompts' }, ds.prompts.slice(0, 3).map((p) => h('li', { text: p, title: p }))) : null,
      files.length ? h('div', { class: 'files', 'aria-label': 'Files touched' }, shown.map((f) => fileChip(f, root)),
        files.length > shown.length ? h('span', { class: 'more', text: '+' + (files.length - shown.length) + ' more' }) : null) : null);
  }

  // ---------- all sessions ----------

  const haystacks = new WeakMap();
  function hay(s) {
    let v = haystacks.get(s);
    if (!v) {
      v = [s.title, s.project, s.projectPath, s.branch, s.firstPrompt, s.lastPrompt, s.id, s.devices.join(' ')].join('\n').toLowerCase();
      haystacks.set(s, v);
    }
    return v;
  }

  function matchState(s, f) {
    if (!f) return true;
    if (f === 'pending') return s.syncState === 'pending-upload' || s.syncState === 'pending-download' || s.syncState === 'merging';
    return s.syncState === f;
  }

  function filtered() {
    const f = S.f;
    const words = f.q.toLowerCase().split(/\s+/).filter(Boolean);
    return S.ov.sessions.filter((s) =>
      (f.archived || !s.archived) &&
      (!f.project || s.project === f.project) &&
      (!f.mac || s.devices.includes(f.mac)) &&
      matchState(s, f.state) &&
      words.every((w) => hay(s).includes(w)));
  }

  function sorted(list) {
    const { key, dir } = S.sort;
    const sign = dir === 'asc' ? 1 : -1;
    const val = {
      title: (s) => (s.title || '').toLowerCase(),
      project: (s) => (s.project || '').toLowerCase(),
      last: (s) => ts(s.lastActivityAt) || 0,
      turns: (s) => s.userTurns || 0,
      state: (s) => (STATES[s.syncState] || STATES['not-synced']).rank,
    }[key];
    return list.slice().sort((a, b) => {
      const va = val(a);
      const vb = val(b);
      if (va < vb) return -sign;
      if (va > vb) return sign;
      return (ts(b.lastActivityAt) || 0) - (ts(a.lastActivityAt) || 0);
    });
  }

  function setOptions(select, options, current) {
    const keep = options.some((o) => o.value === current) ? current : '';
    fill(select, options.map((o) => h('option', { value: o.value, text: o.label })));
    select.value = keep;
    return keep;
  }

  function renderFilters() {
    const ov = S.ov;
    const projects = new Map();
    for (const s of ov.sessions) if (s.project) projects.set(s.project, (projects.get(s.project) || 0) + 1);
    S.f.project = setOptions($('#f-project'), [{ value: '', label: 'All projects' },
      ...Array.from(projects.keys()).sort((a, b) => a.localeCompare(b)).map((p) => ({ value: p, label: p + ' (' + projects.get(p) + ')' }))], S.f.project);
    const macs = new Set(ov.devices.map((d) => d.name));
    for (const s of ov.sessions) s.devices.forEach((d) => macs.add(d));
    S.f.mac = setOptions($('#f-mac'), [{ value: '', label: 'All Macs' },
      ...Array.from(macs).map((m) => ({ value: m, label: 'Changed on ' + m }))], S.f.mac);
  }

  function renderTable() {
    const all = S.ov.sessions;
    const list = sorted(filtered());
    const rows = S.showAll ? list : list.slice(0, PAGE);
    const tbody = $('#rows');

    for (const th of document.querySelectorAll('.sessions th[data-sort]')) {
      if (th.dataset.sort === S.sort.key) th.setAttribute('aria-sort', S.sort.dir === 'asc' ? 'ascending' : 'descending');
      else th.removeAttribute('aria-sort');
    }

    if (!all.length) {
      fill(tbody, h('tr', {}, h('td', { colspan: '5' }, h('div', { class: 'empty' },
        h('h3', { text: 'No sessions on this Mac yet' }),
        h('p', {}, 'Sessions from every Mac land here after they sync. Start one with ', h('code', { text: 'claude' }), ' in Terminal or the Code tab in the Claude app.')))));
    } else if (!list.length) {
      fill(tbody, h('tr', {}, h('td', { colspan: '5' }, h('div', { class: 'empty' },
        h('h3', { text: 'No sessions match' }),
        h('p', { text: 'Try a different search, or clear the filters.' }),
        h('button', { type: 'button', class: 'btn btn-sm', onclick: clearFilters, text: 'Clear filters' })))));
    } else {
      fill(tbody, rows.map(renderRow));
    }

    const hiddenArchived = S.f.archived ? 0 : all.filter((s) => s.archived).length;
    const count = $('#all-count');
    count.textContent = all.length
      ? (list.length === all.length - hiddenArchived ? plural(list.length, 'session') : num(list.length) + ' of ' + plural(all.length - hiddenArchived, 'session')) +
        (hiddenArchived ? ' · ' + num(hiddenArchived) + ' archived hidden' : '')
      : '';
    const foot = $('#table-foot');
    const more = $('.show-all', foot);
    if (more) more.remove();
    if (list.length > rows.length) {
      foot.append(h('button', { type: 'button', class: 'btn btn-sm show-all', 'data-fk': 'show-all', onclick: () => { S.showAll = true; renderTable(); },
        text: 'Show all ' + num(list.length) }));
    }
    foot.hidden = !count.textContent && !$('.show-all', foot);
  }

  function renderRow(s) {
    const open = () => openSession(s.id);
    const st = STATES[s.syncState] || STATES['not-synced'];
    const surface = surfaceLabel(s.surface);
    return h('tr', { class: 'row' + (S.detail && S.detail.id === s.id ? ' open' : ''), tabindex: '0', 'data-fk': 'row-' + s.id,
      'aria-label': (s.title || 'Untitled session') + ', ' + (s.project || 'no project') + ', ' + st.label + ', last active ' + rel(s.lastActivityAt),
      onclick: open,
      onkeydown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); } } },
    h('td', { class: 'c-title' },
      h('div', { class: 'r-title' },
        s.live ? h('span', { class: 'live-dot', title: 'Open in Claude now' }) : null,
        h('b', { text: s.title || 'Untitled session', title: s.title || '' }),
        s.archived ? h('span', { class: 'badge' }, icon('i-archive'), 'Archived') : null),
      h('div', { class: 'r-sub' },
        s.project ? h('span', { class: 'part only-narrow', text: s.project }) : null,
        s.branch ? h('span', { class: 'part part-branch', title: s.branch }, icon('i-branch'), h('span', { text: s.branch })) : null,
        surface ? h('span', { class: 'part', text: surface }) : null)),
    h('td', { class: 'c-project' }, h('span', { class: 'r-project', title: s.projectPath || '', text: s.project || '—' })),
    h('td', { class: 'c-last' }, relTime(s.lastActivityAt, 'r-time')),
    h('td', { class: 'c-turns', title: plural(s.userTurns || 0, 'prompt') + ' · ' + plural(s.assistantTurns || 0, 'reply', 'replies') }, num(s.userTurns)),
    h('td', { class: 'c-state' }, stateBadge(s.syncState, true)));
  }

  function clearFilters() {
    S.f = { q: '', project: '', mac: '', state: '', archived: S.f.archived };
    $('#f-q').value = '';
    $('#f-project').value = '';
    $('#f-mac').value = '';
    $('#f-state').value = '';
    S.showAll = false;
    renderTable();
    $('#f-q').focus();
  }

  // ---------- pending ----------

  function renderPending() {
    const p = S.ov.pending;
    const any = PENDING_KINDS.some((k) => p[k.key] > 0);
    const out = [];
    if (!any && p.known === false) {
      out.push(h('div', { class: 'card all-clear' },
        h('div', { class: 'hero-icon' }, icon('i-clock')),
        h('div', {}, h('strong', { text: 'Not known yet. ' }), "Storage didn't answer, so what's waiting to download is unknown; nothing here needs uploading.")));
    } else if (!any) {
      out.push(h('div', { class: 'card all-clear' },
        h('div', { class: 'hero-icon' }, icon('i-check')),
        h('div', {}, h('strong', { text: 'Nothing waiting. ' }), 'The next sync has nothing to upload, download or merge.')));
    } else {
      out.push(h('div', { class: 'pending-grid' }, PENDING_KINDS.map((k) => {
        const n = p[k.key] || 0;
        return h('div', { class: 'card pcount' + (n ? ' on ' + (k.cls || '') : '') },
          h('div', { class: 'pcount-ico' }, icon(k.icon)),
          h('div', {}, h('div', { class: 'pcount-n', text: num(n) }), h('div', { class: 'pcount-l', text: k.label })));
      })));
      if (p.deferred) {
        out.push(h('p', { class: 'muted small pending-note',
          text: 'Held files belong to sessions open in Claude on this Mac. They update after those sessions close.' }));
      }
    }
    if (p.files.length) {
      // Conflicts are .conflict copies already on disk, not files the
      // next sync touches, so they are not in the list.
      const total = (p.upload || 0) + (p.download || 0) + (p.merge || 0) + (p.deferred || 0) + (p.delete || 0) + (p.trash || 0);
      const det = h('details', { class: 'pfiles' },
        h('summary', { 'data-fk': 'pfiles' }, icon('i-chevron'), 'Show ' + plural(p.files.length, 'file')),
        h('ul', { class: 'plist card' }, p.files.map((f) => {
          const act = ACTIONS[f.action] || { label: f.action, cls: 'st-not', icon: null };
          return h('li', {},
            h('span', {}, h('span', { class: 'badge ' + act.cls }, act.icon ? icon(act.icon) : null, act.label)),
            h('span', { class: 'path', title: f.path, text: f.path }));
        })),
        total > p.files.length ? h('p', { class: 'muted small', text: 'Showing the first ' + num(p.files.length) + ' of ' + num(total) + '.' }) : null);
      const wasOpen = $('#pending-body details.pfiles');
      if (wasOpen && wasOpen.open) det.open = true;
      out.push(det);
    }
    if (S.ov.warnings.length) {
      out.push(h('ul', { class: 'warnings', 'aria-label': 'Warnings' },
        S.ov.warnings.map((w) => h('li', {}, notice('warn', 'i-alert', w)))));
    }
    fill($('#pending-body'), out);
  }

  function renderFooter() {
    const t = S.ov.totals;
    const item = (n, label) => h('span', {}, h('b', { text: n }), ' ' + label);
    fill($('#totals'),
      item(num(t.sessions), t.sessions === 1 ? 'session' : 'sessions'),
      item(num(t.projects), t.projects === 1 ? 'project' : 'projects'),
      item(num(t.messages), t.messages === 1 ? 'message' : 'messages'),
      item(num(t.files), t.files === 1 ? 'file' : 'files'),
      item(bytes(t.bytes), 'on disk'),
      S.ov.agent.version ? h('span', { text: 'stradl ' + S.ov.agent.version }) : null);
  }

  // ---------- session drawer ----------

  const drawerRoot = () => $('#drawer');

  function openSession(id) {
    const target = '#session/' + encodeURIComponent(id);
    if (location.hash !== target) location.hash = target; // hashchange shows it
    else showSession(id);
  }

  function closeSession() {
    if (location.hash.startsWith('#session/')) history.replaceState(null, '', location.pathname + location.search);
    hideDrawer();
  }

  function fromHash() {
    const m = /^#session\/(.+)$/.exec(location.hash);
    if (m) {
      let id = m[1];
      try { id = decodeURIComponent(id); } catch (e) { /* keep as is */ }
      showSession(id);
    } else if (!drawerRoot().hidden) {
      hideDrawer();
    }
  }

  async function showSession(id) {
    const root = drawerRoot();
    if (root.hidden) {
      S.returnFocus = document.activeElement;
      root.hidden = false;
      document.body.classList.add('locked');
      for (const el of document.querySelectorAll('.topbar, #banners, main, .foot')) el.inert = true;
      requestAnimationFrame(() => root.classList.add('open'));
    }
    S.detail = { id, data: null, error: null };
    markOpenRow();
    renderDrawer();
    $('.drawer', root).focus({ preventScroll: true });
    try {
      const d = await api('/api/sessions/' + encodeURIComponent(id));
      if (!S.detail || S.detail.id !== id) return;
      d.devices = arr(d.devices);
      d.filesTouched = arr(d.filesTouched);
      d.timeline = arr(d.timeline);
      S.detail.data = d;
    } catch (e) {
      if (!S.detail || S.detail.id !== id) return;
      S.detail.error = e.status === 404 ? 'This session is no longer on this Mac.' : e.message;
    }
    renderDrawer();
  }

  function hideDrawer() {
    const root = drawerRoot();
    if (root.hidden) return;
    root.classList.remove('open');
    for (const el of document.querySelectorAll('.topbar, #banners, main, .foot')) el.inert = false;
    document.body.classList.remove('locked');
    const back = S.returnFocus;
    const id = S.detail && S.detail.id;
    S.detail = null;
    markOpenRow();
    setTimeout(() => { if (!S.detail) root.hidden = true; }, 200);
    let el = back && document.body.contains(back) ? back : null;
    if (!el && id) el = document.querySelector('[data-fk="row-' + CSS.escape(id) + '"]');
    if (el) el.focus({ preventScroll: true });
  }

  function markOpenRow() {
    const id = S.detail && S.detail.id;
    for (const tr of document.querySelectorAll('#rows tr.row')) tr.classList.toggle('open', tr.dataset.fk === 'row-' + id);
  }

  function renderDrawer() {
    if (!S.detail) return;
    const body = $('#drawer-body');
    const { id, data, error } = S.detail;
    const s = data || S.byId.get(id);
    if (!s) {
      fill(body, error ? h('div', { class: 'empty' }, h('h3', { text: 'Session not found' }), h('p', { text: error }))
        : [h('div', { class: 'sk sk-title' }), h('div', { class: 'sk sk-line w80' }), h('div', { class: 'sk sk-line w60' })]);
      $('#drawer .drawer').setAttribute('aria-label', 'Session');
      return;
    }
    const st = STATES[s.syncState] || STATES['not-synced'];
    const surface = surfaceLabel(s.surface);
    const resume = (s.projectPath ? 'cd ' + shellQuote(s.projectPath) + ' && ' : '') + 'claude --resume ' + s.id;

    const sections = [
      h('div', { class: 'd-kicker' },
        s.project ? chipProject(s.project, s.projectPath) : null,
        surface ? h('span', { class: 'chip', text: surface }) : null,
        s.branch ? h('span', { class: 'chip' }, icon('i-branch'), h('span', { text: s.branch })) : null,
        s.live ? h('span', { class: 'badge st-held' }, h('span', { class: 'live-dot' }), 'Open now') : null,
        s.archived ? h('span', { class: 'badge' }, icon('i-archive'), 'Archived') : null),
      h('h2', { class: 'd-title', id: 'd-title', text: s.title || 'Untitled session' }),
      h('div', { class: 'd-state ' + st.cls }, stateBadge(s.syncState), h('p', { text: EXPLAIN[s.syncState] || EXPLAIN['not-synced'] })),
      h('section', { class: 'd-sec', 'aria-label': 'Details' },
        h('dl', { class: 'd-meta' },
          meta('Started', longDate(s.startedAt)),
          meta('Last activity', [longDate(s.lastActivityAt), ' · ', relTime(s.lastActivityAt)]),
          meta('Turns', plural(s.userTurns || 0, 'prompt') + ' · ' + plural(s.assistantTurns || 0, 'reply', 'replies')),
          meta('Size', bytes(s.sizeBytes)),
          meta('Claude sidebar', s.inSidebar ? 'Listed on this Mac' : 'Not listed (a Terminal session)'),
          meta('Changed on', s.devices.length ? h('span', { class: 'd-devs' }, s.devices.map(chipDev)) : 'No Mac in the last 7 days'),
          s.projectPath ? meta('Folder', h('span', { class: 'mono', text: s.projectPath }), 'wide') : null)),
      h('section', { class: 'd-sec', 'aria-labelledby': 'd-resume' },
        h('h3', { id: 'd-resume', text: 'Pick it up in Terminal' }),
        h('div', { class: 'copyline' }, h('code', { text: resume }),
          h('button', { type: 'button', class: 'btn btn-sm', onclick: (e) => copy(resume, e.currentTarget) }, icon('i-copy'), 'Copy'))),
    ];

    if (s.filesTouched.length) {
      const max = 40;
      sections.push(h('section', { class: 'd-sec', 'aria-labelledby': 'd-files' },
        h('h3', { id: 'd-files', text: 'Files touched · ' + num(s.filesTouched.length) }),
        h('ul', { class: 'd-files' }, s.filesTouched.slice(0, max).map((f) =>
          h('li', { title: f }, icon('i-file'), h('span', { text: relPath(f, s.projectPath) }))),
        s.filesTouched.length > max ? h('li', { class: 'muted' }, 'and ' + num(s.filesTouched.length - max) + ' more') : null)));
    }

    const tlSec = h('section', { class: 'd-sec', 'aria-labelledby': 'd-tl' }, h('h3', { id: 'd-tl', text: 'Timeline' }));
    if (!data && !error) {
      put(tlSec, [h('div', { class: 'sk sk-line' }), h('div', { class: 'sk sk-line w80' })]);
    } else if (error) {
      put(tlSec, [h('p', { class: 'muted', text: "Couldn't load the timeline: " + error })]);
    } else if (!data.timeline.length) {
      put(tlSec, [h('p', { class: 'muted', text: 'Nothing to show yet.' })]);
    } else {
      put(tlSec, [renderTimeline(data.timeline, s.projectPath)]);
    }
    sections.push(tlSec);

    const scroll = body.scrollTop;
    const sameSession = body.dataset.id === id;
    fill(body, sections);
    body.dataset.id = id;
    if (sameSession) body.scrollTop = scroll;
    else body.scrollTop = 0;
  }

  function meta(label, value, cls) {
    return h('div', { class: cls || null }, h('dt', { text: label }), h('dd', {}, value));
  }

  function renderTimeline(entries, root) {
    const out = [];
    let lastDay = '';
    const today = startOfDay(Date.now());
    entries.forEach((e, i) => {
      const t = ts(e.at);
      const day = t ? startOfDay(t) : 0;
      if (day && String(day) !== lastDay) {
        lastDay = String(day);
        const diff = Math.round((today - day) / DAY_MS);
        out.push(h('li', { class: 'tl-day', text: diff === 0 ? 'Today' : diff === 1 ? 'Yesterday' : fmtDay.format(new Date(t)) }));
      }
      const k = KINDS[e.kind] || { icon: 'i-chevron', label: e.kind };
      const next = entries[i + 1];
      const lastOfDay = !next || (ts(next.at) && startOfDay(ts(next.at)) !== day);
      let text = e.text || '';
      if ((e.kind === 'edit' || e.kind === 'write') && e.path) text = relPath(e.path, root);
      const long = (e.kind === 'prompt' || e.kind === 'reply') && text.length > 220;
      const textEl = h('div', { class: 'tl-text' + (long ? ' tl-clamp' : ''), title: e.path || null, text });
      if (long) {
        textEl.tabIndex = 0;
        textEl.setAttribute('role', 'button');
        textEl.setAttribute('aria-expanded', 'false');
        const toggle = () => {
          const open = textEl.classList.toggle('tl-clamp');
          textEl.setAttribute('aria-expanded', String(!open));
        };
        textEl.addEventListener('click', toggle);
        textEl.addEventListener('keydown', (ev) => { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); toggle(); } });
      }
      out.push(h('li', { class: 'tl-item k-' + e.kind + (lastOfDay ? ' last' : '') },
        h('span', { class: 'tl-ico', title: k.label }, icon(k.icon), h('span', { class: 'sr-only', text: k.label + ': ' })),
        textEl,
        h('time', { class: 'tl-time', datetime: e.at || '', text: t ? fmtTime.format(new Date(t)) : '' })));
    });
    return h('ol', { class: 'tl' }, out);
  }

  async function copy(text, btn) {
    try {
      await navigator.clipboard.writeText(text);
      toast('Copied to the clipboard');
      if (btn) {
        const label = btn.lastChild;
        label.textContent = 'Copied';
        setTimeout(() => { label.textContent = 'Copy'; }, 1600);
      }
    } catch (e) {
      toast("Couldn't copy; select the text instead");
    }
  }

  function trapFocus(e) {
    if (e.key !== 'Tab' || drawerRoot().hidden) return;
    const items = Array.from(drawerRoot().querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'))
      .filter((el) => !el.disabled && el.offsetParent !== null);
    if (!items.length) return;
    const first = items[0];
    const last = items[items.length - 1];
    if (e.shiftKey && (document.activeElement === first || document.activeElement === $('#drawer .drawer'))) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }

  // ---------- sync now ----------

  async function syncNow() {
    if (S.sync || !S.ov) return;
    const a = S.ov.agent;
    S.sync = { started: Date.now(), prev: a.lastAttemptAt || a.lastSyncAt || '', saw: false };
    renderStatus();
    try {
      await api('/api/sync', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
    } catch (e) {
      S.sync = null;
      renderStatus();
      toast(e.status === 401 ? 'This dashboard link has expired' : "Couldn't start a sync: " + e.message);
      return;
    }
    toast('Syncing with your other Macs…');
    while (S.sync) {
      await sleep(SYNC_POLL_MS);
      await load();
      if (S.sync && S.conn !== 'ok' && Date.now() - S.sync.started > 10000) S.sync = null;
    }
    renderStatus();
  }

  function checkSync() {
    const sy = S.sync;
    const a = S.ov.agent;
    if (a.syncing) sy.saw = true;
    const attempt = a.lastAttemptAt || a.lastSyncAt || '';
    const finished = (attempt && attempt !== sy.prev) || (sy.saw && !a.syncing);
    if (finished || Date.now() - sy.started > SYNC_TIMEOUT_MS) {
      S.sync = null;
      renderStatus();
      if (finished) toast(a.lastError ? 'Sync finished with a problem: ' + (a.lastErrorSummary || a.lastError) : 'Sync complete');
    }
  }

  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  let toastTimer = 0;
  function toast(msg) {
    const el = $('#toast');
    el.textContent = msg;
    el.hidden = false;
    requestAnimationFrame(() => el.classList.add('show'));
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => {
      el.classList.remove('show');
      setTimeout(() => { if (!el.classList.contains('show')) el.hidden = true; }, 200);
    }, 2600);
  }

  // tick redraws relative times without re-rendering anything else.
  function tick() {
    for (const el of document.querySelectorAll('[data-rel]')) {
      if (el.dataset.rel) el.textContent = rel(el.dataset.rel);
    }
    renderStatus();
  }

  // ---------- wiring ----------

  function init() {
    $('#sync-btn').addEventListener('click', syncNow);

    let qTimer = 0;
    $('#f-q').addEventListener('input', (e) => {
      clearTimeout(qTimer);
      qTimer = setTimeout(() => { S.f.q = e.target.value; S.showAll = false; if (S.ov) renderTable(); }, 80);
    });
    $('#f-q').addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && e.target.value) { e.stopPropagation(); e.target.value = ''; S.f.q = ''; if (S.ov) renderTable(); }
    });
    for (const [sel, key] of [['#f-project', 'project'], ['#f-mac', 'mac'], ['#f-state', 'state']]) {
      $(sel).addEventListener('change', (e) => { S.f[key] = e.target.value; S.showAll = false; if (S.ov) renderTable(); });
    }
    $('#f-archived').addEventListener('change', (e) => { S.f.archived = e.target.checked; if (S.ov) renderTable(); });

    for (const th of document.querySelectorAll('.sessions th[data-sort]')) {
      $('.th-btn', th).addEventListener('click', () => {
        const key = th.dataset.sort;
        if (S.sort.key === key) S.sort.dir = S.sort.dir === 'asc' ? 'desc' : 'asc';
        else S.sort = { key, dir: key === 'last' || key === 'turns' ? 'desc' : 'asc' };
        if (S.ov) renderTable();
      });
    }

    for (const el of document.querySelectorAll('#drawer [data-close]')) el.addEventListener('click', closeSession);
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && !drawerRoot().hidden) { e.preventDefault(); closeSession(); return; }
      trapFocus(e);
      const tag = (e.target.tagName || '').toLowerCase();
      if (e.key === '/' && !e.metaKey && !e.ctrlKey && !e.altKey && drawerRoot().hidden &&
          tag !== 'input' && tag !== 'select' && tag !== 'textarea' && !e.target.isContentEditable) {
        e.preventDefault();
        const q = $('#f-q');
        q.focus();
        q.select();
      }
    });
    window.addEventListener('hashchange', fromHash);

    setInterval(() => { if (!document.hidden && !S.sync) load(); }, REFRESH_MS);
    setInterval(() => { if (!document.hidden) tick(); }, RELTIME_MS);
    document.addEventListener('visibilitychange', () => {
      if (!document.hidden && Date.now() - S.loadedAt > REFRESH_MS / 2) load();
    });

    load().then(fromHash);
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
