(() => {
  'use strict';

  const MAX_SAMPLES = 90;
  // Live theme palette, read from the CSS custom properties in styles.css so
  // the canvases follow the active theme. DOM elements use var(--token) directly.
  const COLORS = {};
  const TOKENS = {
    green: '--green', amber: '--amber', sky: '--sky', violet: '--violet', rose: '--rose', slate: '--slate',
    grid: '--chart-grid', axis: '--chart-axis', cursor: '--chart-cursor', dotFill: '--chart-dot-fill',
    fillAlpha: '--chart-fill-alpha', glow: '--chart-glow',
  };
  function readPalette() {
    const cs = getComputedStyle(document.documentElement);
    for (const [k, v] of Object.entries(TOKENS)) COLORS[k] = cs.getPropertyValue(v).trim();
  }
  readPalette();
  // Display order and colors for actor states (server strips the ACTOR_STATE_ prefix).
  const STATES = [
    { key: 'RUNNING', label: 'Running', tone: 'green' },
    { key: 'RESUMING', label: 'Resuming', tone: 'sky' },
    { key: 'REVERTING', label: 'Reverting', tone: 'sky' },
    { key: 'SUSPENDING', label: 'Suspending', tone: 'amber' },
    { key: 'SUSPENDED', label: 'Suspended', tone: 'slate' },
    { key: 'CRASHED', label: 'Crashed', tone: 'rose' },
  ];
  const KNOWN_STATES = new Set(STATES.map((s) => s.key));

  const history = [];
  let pollMs = 1000;
  let timer = null;
  let inFlight = false;
  let hoverIdx = -1;
  let lastData = null;
  const tableFilter = { state: 'ALL', query: '' };

  const $ = (id) => document.getElementById(id);
  const fmt = new Intl.NumberFormat();
  const num = (v) => (Number.isFinite(v) ? v : 0);

  // ---------- Animated counters ----------
  const counters = new Map();
  function setCount(id, value, suffix = '') {
    const node = $(id);
    if (!node) return;
    const target = num(value);
    const prev = counters.get(id);
    const from = prev ? prev.current : target;
    const state = { from, to: target, start: performance.now(), current: from, suffix };
    counters.set(id, state);
    if (from === target || matchMedia('(prefers-reduced-motion: reduce)').matches) {
      state.current = target;
      node.textContent = fmt.format(target) + suffix;
      return;
    }
    const step = (now) => {
      if (counters.get(id) !== state) return;
      const t = Math.min(1, Math.max(0, (now - state.start) / 450));
      const eased = 1 - Math.pow(1 - t, 3);
      state.current = Math.round(from + (target - from) * eased);
      node.textContent = fmt.format(state.current) + suffix;
      if (t < 1) requestAnimationFrame(step);
    };
    requestAnimationFrame(step);
  }
  const setText = (id, text) => {
    const node = $(id);
    if (node) node.textContent = String(text);
  };

  // ---------- Canvas helpers ----------
  function fitCanvas(canvas) {
    const dpr = window.devicePixelRatio || 1;
    const rect = canvas.getBoundingClientRect();
    const w = Math.max(1, Math.round(rect.width * dpr));
    const h = Math.max(1, Math.round(rect.height * dpr));
    if (canvas.width !== w || canvas.height !== h) {
      canvas.width = w;
      canvas.height = h;
    }
    const ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    return { ctx, w: rect.width, h: rect.height };
  }

  function hexToRgba(hex, a) {
    const n = parseInt(hex.slice(1), 16);
    return 'rgba(' + ((n >> 16) & 255) + ',' + ((n >> 8) & 255) + ',' + (n & 255) + ',' + a + ')';
  }

  // Smooth path through points using monotone-ish cubic segments.
  function tracePath(ctx, pts) {
    ctx.moveTo(pts[0][0], pts[0][1]);
    for (let i = 1; i < pts.length; i++) {
      const [x0, y0] = pts[i - 1];
      const [x1, y1] = pts[i];
      const cx = (x0 + x1) / 2;
      ctx.bezierCurveTo(cx, y0, cx, y1, x1, y1);
    }
  }

  function niceMax(v) {
    if (v <= 5) return 5;
    const mag = Math.pow(10, Math.floor(Math.log10(v)));
    const n = v / mag;
    const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
    return step * mag;
  }

  // ---------- Sparklines ----------
  function drawSpark(id, getter, tone) {
    const color = COLORS[tone];
    const canvas = $(id);
    if (!canvas) return;
    const { ctx, w, h } = fitCanvas(canvas);
    ctx.clearRect(0, 0, w, h);
    if (history.length < 2) return;
    const vals = history.map(getter);
    const hi = Math.max(...vals);
    const lo = Math.min(...vals);
    // Scale to the window's range so slow-moving series still show a trend;
    // a flat series sits mid-height.
    const span = hi - lo;
    const pts = vals.map((v, i) => [
      (i / (MAX_SAMPLES - 1)) * w + (w - ((history.length - 1) / (MAX_SAMPLES - 1)) * w),
      span === 0 ? h / 2 : h - 3 - ((v - lo) / span) * (h - 6),
    ]);
    const grad = ctx.createLinearGradient(0, 0, 0, h);
    grad.addColorStop(0, hexToRgba(color, 0.35));
    grad.addColorStop(1, hexToRgba(color, 0));
    ctx.beginPath();
    tracePath(ctx, pts);
    ctx.lineTo(pts[pts.length - 1][0], h);
    ctx.lineTo(pts[0][0], h);
    ctx.closePath();
    ctx.fillStyle = grad;
    ctx.fill();
    ctx.beginPath();
    tracePath(ctx, pts);
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.75;
    ctx.stroke();
  }

  // ---------- Main activity chart ----------
  const SERIES = [
    { key: 'pending', label: 'Queue backlog', tone: 'sky' },
    { key: 'transitioning', label: 'Resuming / suspending', tone: 'amber' },
    { key: 'running', label: 'Running', tone: 'green' },
  ];
  const PAD = { l: 44, r: 12, t: 12, b: 26 };

  function chartGeometry(w, h) {
    return { plotW: w - PAD.l - PAD.r, plotH: h - PAD.t - PAD.b };
  }
  function xFor(i, plotW) {
    const offset = MAX_SAMPLES - history.length;
    return PAD.l + ((i + offset) / (MAX_SAMPLES - 1)) * plotW;
  }

  function renderChart() {
    const canvas = $('activity-chart');
    if (!canvas) return;
    const { ctx, w, h } = fitCanvas(canvas);
    const { plotW, plotH } = chartGeometry(w, h);
    ctx.clearRect(0, 0, w, h);

    let maxVal = 0;
    for (const p of history) maxVal = Math.max(maxVal, p.running, p.transitioning, p.pending);
    maxVal = niceMax(maxVal);

    ctx.font = '11px ui-monospace, "SF Mono", Menlo, monospace';
    ctx.textBaseline = 'middle';
    for (let i = 0; i <= 4; i++) {
      const y = PAD.t + (plotH * i) / 4;
      ctx.strokeStyle = COLORS.grid;
      ctx.lineWidth = 1;
      ctx.setLineDash(i === 4 ? [] : [3, 4]);
      ctx.beginPath();
      ctx.moveTo(PAD.l, Math.round(y) + 0.5);
      ctx.lineTo(w - PAD.r, Math.round(y) + 0.5);
      ctx.stroke();
      ctx.setLineDash([]);
      ctx.fillStyle = COLORS.axis;
      ctx.textAlign = 'right';
      ctx.fillText(fmt.format(Math.round(maxVal * (1 - i / 4))), PAD.l - 8, y);
    }

    // Time axis: label seconds-ago at a few ticks.
    ctx.textAlign = 'center';
    ctx.textBaseline = 'alphabetic';
    if (history.length > 1) {
      const newest = history[history.length - 1].t;
      [0, 0.25, 0.5, 0.75].forEach((f) => {
        const idx = Math.round((history.length - 1) * f);
        const ago = Math.round((newest - history[idx].t) / 1000);
        ctx.fillStyle = COLORS.axis;
        ctx.fillText(ago === 0 ? 'now' : '-' + ago + 's', xFor(idx, plotW), h - 6);
      });
      ctx.fillText('now', xFor(history.length - 1, plotW) - 10, h - 6);
    }

    if (history.length < 2) {
      ctx.fillStyle = COLORS.axis;
      ctx.textAlign = 'center';
      ctx.fillText('Collecting samples…', PAD.l + plotW / 2, PAD.t + plotH / 2);
      return;
    }

    const fillAlpha = parseFloat(COLORS.fillAlpha) || 0.2;
    const glow = parseFloat(COLORS.glow) || 0;
    for (const s of SERIES) {
      const color = COLORS[s.tone];
      const pts = history.map((p, i) => [xFor(i, plotW), PAD.t + plotH - (Math.min(p[s.key], maxVal) / maxVal) * plotH]);
      const grad = ctx.createLinearGradient(0, PAD.t, 0, PAD.t + plotH);
      grad.addColorStop(0, hexToRgba(color, fillAlpha));
      grad.addColorStop(1, hexToRgba(color, 0));
      ctx.beginPath();
      tracePath(ctx, pts);
      ctx.lineTo(pts[pts.length - 1][0], PAD.t + plotH);
      ctx.lineTo(pts[0][0], PAD.t + plotH);
      ctx.closePath();
      ctx.fillStyle = grad;
      ctx.fill();

      ctx.beginPath();
      tracePath(ctx, pts);
      ctx.strokeStyle = color;
      ctx.lineWidth = 2;
      ctx.shadowColor = hexToRgba(color, 0.5);
      ctx.shadowBlur = glow;
      ctx.stroke();
      ctx.shadowBlur = 0;

      // Endpoint marker.
      const last = pts[pts.length - 1];
      ctx.beginPath();
      ctx.arc(last[0], last[1], 3.5, 0, Math.PI * 2);
      ctx.fillStyle = color;
      ctx.fill();
    }

    if (hoverIdx >= 0 && hoverIdx < history.length) {
      const x = xFor(hoverIdx, plotW);
      ctx.strokeStyle = COLORS.cursor;
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(Math.round(x) + 0.5, PAD.t);
      ctx.lineTo(Math.round(x) + 0.5, PAD.t + plotH);
      ctx.stroke();
      for (const s of SERIES) {
        const v = history[hoverIdx][s.key];
        const y = PAD.t + plotH - (Math.min(v, maxVal) / maxVal) * plotH;
        ctx.beginPath();
        ctx.arc(x, y, 4.5, 0, Math.PI * 2);
        ctx.fillStyle = COLORS.dotFill;
        ctx.fill();
        ctx.lineWidth = 2;
        ctx.strokeStyle = COLORS[s.tone];
        ctx.stroke();
      }
    }
  }

  function updateTooltip() {
    const tip = $('chart-tooltip');
    const canvas = $('activity-chart');
    if (!tip || !canvas) return;
    if (hoverIdx < 0 || hoverIdx >= history.length) {
      tip.hidden = true;
      return;
    }
    const p = history[hoverIdx];
    tip.replaceChildren();
    const time = document.createElement('div');
    time.className = 'tt-time';
    time.textContent = new Date(p.t).toLocaleTimeString();
    tip.appendChild(time);
    for (const s of [...SERIES].reverse()) {
      const row = document.createElement('div');
      row.className = 'tt-row';
      const label = document.createElement('span');
      const sw = document.createElement('span');
      sw.className = 'swatch';
      sw.style.background = 'var(--' + s.tone + ')';
      label.append(sw, document.createTextNode(s.label));
      const val = document.createElement('strong');
      val.textContent = fmt.format(p[s.key]);
      row.append(label, val);
      tip.appendChild(row);
    }
    tip.hidden = false;
    const rect = canvas.getBoundingClientRect();
    const x = xFor(hoverIdx, chartGeometry(rect.width, rect.height).plotW);
    const tipW = tip.offsetWidth;
    const left = x + 14 + tipW > rect.width ? x - 14 - tipW : x + 14;
    tip.style.left = Math.max(0, left) + 'px';
  }

  function bindChartHover() {
    const canvas = $('activity-chart');
    if (!canvas) return;
    canvas.addEventListener('mousemove', (ev) => {
      if (history.length < 2) return;
      const rect = canvas.getBoundingClientRect();
      const { plotW } = chartGeometry(rect.width, rect.height);
      const offset = MAX_SAMPLES - history.length;
      const rel = ((ev.clientX - rect.left - PAD.l) / plotW) * (MAX_SAMPLES - 1) - offset;
      hoverIdx = Math.max(0, Math.min(history.length - 1, Math.round(rel)));
      renderChart();
      updateTooltip();
    });
    canvas.addEventListener('mouseleave', () => {
      hoverIdx = -1;
      renderChart();
      updateTooltip();
    });
  }

  // ---------- State breakdown ----------
  function stateMeta(key) {
    return STATES.find((s) => s.key === key) || { key, label: key.charAt(0) + key.slice(1).toLowerCase(), tone: 'violet' };
  }

  function renderStates(breakdown, total) {
    const stack = $('state-stack');
    const list = $('state-list');
    if (!stack || !list) return;
    setCount('state-total', total);

    const keys = Object.keys(breakdown || {}).filter((k) => breakdown[k] > 0);
    keys.sort((a, b) => {
      const ia = STATES.findIndex((s) => s.key === a);
      const ib = STATES.findIndex((s) => s.key === b);
      return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib) || a.localeCompare(b);
    });

    stack.replaceChildren();
    list.replaceChildren();
    if (keys.length === 0) {
      const li = document.createElement('li');
      li.className = 'none';
      li.textContent = 'No actors in this atespace yet.';
      list.appendChild(li);
      return;
    }
    for (const k of keys) {
      const meta = stateMeta(k);
      const count = breakdown[k];
      const seg = document.createElement('span');
      seg.style.flexGrow = String(count);
      seg.style.background = 'var(--' + meta.tone + ')';
      seg.title = meta.label + ': ' + count;
      stack.appendChild(seg);

      const li = document.createElement('li');
      const dot = document.createElement('span');
      dot.className = 'dot';
      dot.style.background = 'var(--' + meta.tone + ')';
      const name = document.createElement('span');
      name.textContent = meta.label;
      const c = document.createElement('span');
      c.className = 'count';
      c.textContent = fmt.format(count);
      const pct = document.createElement('span');
      pct.className = 'pct';
      pct.textContent = (total ? (count * 100) / total : 0).toFixed(total && count * 100 < total ? 1 : 0) + '%';
      li.append(dot, name, c, pct);
      list.appendChild(li);
    }
  }

  // ---------- Actor table ----------
  function badgeClass(state) {
    const k = state.toLowerCase();
    return KNOWN_STATES.has(state) || state === 'CREATING' ? 's-' + k : 's-other';
  }

  function renderFilters(actors) {
    const host = $('state-filters');
    if (!host) return;
    const counts = { ALL: actors.length };
    for (const a of actors) counts[a.state] = (counts[a.state] || 0) + 1;
    const keys = ['ALL', ...Object.keys(counts).filter((k) => k !== 'ALL').sort((a, b) => {
      const ia = STATES.findIndex((s) => s.key === a);
      const ib = STATES.findIndex((s) => s.key === b);
      return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib);
    })];
    if (tableFilter.state !== 'ALL' && !counts[tableFilter.state]) keys.push(tableFilter.state);

    host.replaceChildren();
    for (const k of keys) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'filter';
      b.setAttribute('aria-pressed', String(tableFilter.state === k));
      b.dataset.state = k;
      const label = document.createTextNode(k === 'ALL' ? 'All' : stateMeta(k).label);
      const n = document.createElement('span');
      n.className = 'n';
      n.textContent = fmt.format(counts[k] || 0);
      b.append(label, n);
      host.appendChild(b);
    }
  }

  function renderTable(actors) {
    const body = $('actor-rows');
    const empty = $('actor-empty');
    if (!body) return;
    const q = tableFilter.query;
    const rows = actors.filter((a) =>
      (tableFilter.state === 'ALL' || a.state === tableFilter.state) &&
      (!q || a.name.toLowerCase().includes(q) || (a.worker_pod || '').toLowerCase().includes(q)));

    const frag = document.createDocumentFragment();
    for (const a of rows) {
      const tr = document.createElement('tr');
      if (a.state === 'CRASHED' || a.has_error) tr.className = 'crashed';

      const name = document.createElement('td');
      name.className = 'name';
      name.textContent = a.name;
      name.title = a.atespace + '/' + a.name;

      const st = document.createElement('td');
      const badge = document.createElement('span');
      badge.className = 'badge ' + badgeClass(a.state);
      badge.textContent = stateMeta(a.state).label;
      st.appendChild(badge);

      const pod = document.createElement('td');
      pod.className = a.worker_pod ? 'pod' : 'pod none';
      pod.textContent = a.worker_pod || '—';

      const ver = document.createElement('td');
      ver.className = 'num';
      ver.textContent = fmt.format(a.version || 0);

      const age = document.createElement('td');
      age.className = 'num';
      age.textContent = a.age || '—';

      tr.append(name, st, pod, ver, age);
      frag.appendChild(tr);
    }
    body.replaceChildren(frag);
    setText('table-count', fmt.format(rows.length));
    if (empty) {
      empty.hidden = rows.length > 0;
      empty.textContent = actors.length === 0
        ? 'No actors yet. Start a session or run the load generator.'
        : 'No actors match the current filter.';
    }
  }

  function bindTableControls() {
    const host = $('state-filters');
    if (host) {
      host.addEventListener('click', (ev) => {
        const b = ev.target.closest('button[data-state]');
        if (!b) return;
        tableFilter.state = b.dataset.state;
        if (lastData) {
          renderFilters(lastData.actor_list || []);
          renderTable(lastData.actor_list || []);
        }
      });
    }
    const search = $('actor-search');
    if (search) {
      search.addEventListener('input', () => {
        tableFilter.query = search.value.trim().toLowerCase();
        if (lastData) renderTable(lastData.actor_list || []);
      });
    }
  }

  // ---------- Status / polling ----------
  function setStatus(state, text) {
    const pill = $('status-pill');
    if (pill) pill.dataset.state = state;
    setText('status-text', text);
  }

  function render(data) {
    lastData = data;
    const actors = data.actors || {};
    const workers = data.workers || {};
    const queue = data.queue || {};

    setText('ns-badge', data.namespace || 'ate-ant');
    setText('last-updated', new Date(data.timestamp).toLocaleTimeString());
    setText('collect-ms', 'collected in ' + num(data.collect_ms).toFixed(0) + ' ms');

    const transitioning = num(actors.resuming) + num(actors.creating) + num(actors.suspending);
    setCount('kpi-running', actors.running);
    setCount('kpi-resuming', num(actors.resuming) + num(actors.creating));
    setCount('kpi-suspending', actors.suspending);
    setText('kpi-active-tag', fmt.format(num(actors.active_total)) + ' active');

    setCount('kpi-total-actors', actors.total);
    setCount('kpi-suspended', actors.suspended);
    setCount('kpi-errored', actors.errored);
    setText('kpi-multiplex', num(actors.multiplex_ratio).toFixed(1) + '× / worker');

    const util = Math.min(100, Math.max(0, num(workers.worker_utilization_pct)));
    setCount('kpi-workers-claimed', workers.workerpool_claimed);
    setCount('kpi-workers-ready', workers.workerpool_ready);
    setCount('kpi-workers-idle', workers.workerpool_idle);
    setCount('kpi-workers-desired', workers.workerpool_desired);
    setText('kpi-worker-util', util.toFixed(0) + '%');
    const meter = $('util-meter');
    if (meter) {
      meter.style.width = util + '%';
      meter.className = 'meter-fill' + (util >= 95 ? ' full' : util >= 75 ? ' hot' : '');
    }

    const backlog = num(queue.pending_count) + num(queue.lag);
    setCount('kpi-queue-pending', queue.pending_count);
    setCount('kpi-queue-lag', queue.lag);
    setCount('kpi-ctrl-ready', workers.controller_ready);
    setCount('kpi-ctrl-desired', workers.controller_desired);
    setText('kpi-queue-tag', !queue.connected ? 'disconnected' : backlog > 0 ? 'draining' : 'idle');

    setCount('q-length', queue.stream_length);
    setCount('q-read', queue.entries_read);
    const qc = $('q-connected');
    if (qc) {
      qc.textContent = queue.connected ? 'Connected' : 'Unavailable';
      qc.className = queue.connected ? 'ok' : 'bad';
    }

    renderStates(actors.state_breakdown, num(actors.total));
    renderFilters(data.actor_list || []);
    renderTable(data.actor_list || []);

    const warnings = data.warnings || [];
    const banner = $('warnings');
    const wl = $('warnings-list');
    if (banner && wl) {
      wl.replaceChildren(...warnings.map((w) => {
        const li = document.createElement('li');
        li.textContent = w;
        return li;
      }));
      banner.hidden = warnings.length === 0;
    }

    history.push({
      t: Date.now(),
      running: num(actors.running),
      transitioning,
      pending: backlog,
      total: num(actors.total),
    });
    if (history.length > MAX_SAMPLES) history.shift();

    renderChart();
    updateTooltip();
    drawSpark('spark-running', (p) => p.running, 'green');
    drawSpark('spark-total', (p) => p.total, 'violet');
    drawSpark('spark-queue', (p) => p.pending, 'amber');
  }

  async function fetchStats() {
    if (inFlight) return;
    inFlight = true;
    try {
      const resp = await fetch('/api/stats', { cache: 'no-store' });
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      const data = await resp.json();
      setStatus(pollMs > 0 ? 'live' : 'paused', pollMs > 0 ? 'Live' : 'Paused');
      render(data);
    } catch (_err) {
      setStatus('error', 'Disconnected');
      setText('collect-ms', 'retrying…');
    } finally {
      inFlight = false;
    }
  }

  function schedule() {
    if (timer) clearInterval(timer);
    timer = pollMs > 0 ? setInterval(fetchStats, pollMs) : null;
  }

  function bindPollControl() {
    const group = $('poll-group');
    if (!group) return;
    group.addEventListener('click', (ev) => {
      const b = ev.target.closest('button[data-ms]');
      if (!b) return;
      for (const other of group.querySelectorAll('button')) other.setAttribute('aria-checked', String(other === b));
      pollMs = parseInt(b.dataset.ms, 10) || 0;
      setStatus(pollMs > 0 ? 'live' : 'paused', pollMs > 0 ? 'Live' : 'Paused');
      schedule();
      if (pollMs > 0) fetchStats();
    });
  }

  // ---------- Theme ----------
  const THEME_KEY = 'ate-dashboard-theme';
  const THEMES = {
    auto: { next: 'light', icon: '◐', label: 'Auto', title: 'Theme: follows your system' },
    light: { next: 'dark', icon: '☀', label: 'Light', title: 'Theme: light' },
    dark: { next: 'auto', icon: '☾', label: 'Dark', title: 'Theme: dark' },
  };
  let redrawAll = () => {};

  function storedTheme() {
    try {
      const t = localStorage.getItem(THEME_KEY);
      return THEMES[t] ? t : 'auto';
    } catch (_e) {
      return 'auto';
    }
  }

  function applyTheme(mode) {
    const root = document.documentElement;
    if (mode === 'auto') root.removeAttribute('data-theme');
    else root.dataset.theme = mode;
    const t = THEMES[mode];
    setText('theme-icon', t.icon);
    setText('theme-label', t.label);
    const btn = $('theme-toggle');
    if (btn) btn.title = t.title + ' (click to switch)';
    readPalette();
    redrawAll();
  }

  function bindThemeToggle() {
    let mode = storedTheme();
    applyTheme(mode);
    const btn = $('theme-toggle');
    if (btn) {
      btn.addEventListener('click', () => {
        mode = THEMES[mode].next;
        try {
          localStorage.setItem(THEME_KEY, mode);
        } catch (_e) { /* storage unavailable: theme lasts for this page only */ }
        applyTheme(mode);
      });
    }
    // In Auto, follow OS theme changes live.
    matchMedia('(prefers-color-scheme: light)').addEventListener('change', () => {
      if (mode === 'auto') applyTheme('auto');
    });
  }

  document.addEventListener('DOMContentLoaded', () => {
    setText('window-label', String(MAX_SAMPLES));
    bindPollControl();
    bindChartHover();
    bindTableControls();
    const redraw = () => {
      renderChart();
      drawSpark('spark-running', (p) => p.running, 'green');
      drawSpark('spark-total', (p) => p.total, 'violet');
      drawSpark('spark-queue', (p) => p.pending, 'amber');
    };
    redrawAll = redraw;
    bindThemeToggle();
    if ('ResizeObserver' in window) {
      new ResizeObserver(redraw).observe(document.querySelector('.container'));
    } else {
      window.addEventListener('resize', redraw);
    }
    document.addEventListener('visibilitychange', () => {
      if (document.hidden) {
        if (timer) clearInterval(timer);
        timer = null;
      } else {
        schedule();
        if (pollMs > 0) fetchStats();
      }
    });
    fetchStats();
    schedule();
  });
})();
