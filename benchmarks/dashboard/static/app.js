(() => {
  'use strict';

  const MAX_SAMPLES = 60;
  const SPARK_POINTS = 12;
  const SERIES = [
    { key: 'running', label: 'Running', cls: 's1' },
    { key: 'transitioning', label: 'Transitioning', cls: 's2' },
    { key: 'pending', label: 'Queue pending', cls: 's3' },
  ];
  const TRANSITIONING_STATES = new Set(['RESUMING', 'REVERTING', 'SUSPENDING', 'PAUSING', 'CREATING', 'PENDING', 'STARTING']);
  const DORMANT_STATES = new Set(['SUSPENDED', 'PAUSED']);

  const history = []; // { t: Date, running, transitioning, suspended, pending }
  let pollMs = 1000;
  let timer = null;
  let hoverIdx = null;
  let lastData = null;
  let workerGridSig = '';
  let stateOptionsSig = '';

  const $ = (id) => document.getElementById(id);
  const fmt = (n) => Number(n || 0).toLocaleString();
  const setText = (id, v) => { const n = $(id); if (n) n.textContent = String(v); };
  const SVG_NS = 'http://www.w3.org/2000/svg';
  const svgEl = (tag, attrs, cls) => {
    const e = document.createElementNS(SVG_NS, tag);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    if (cls) e.setAttribute('class', cls);
    return e;
  };
  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  };
  const timeLabel = (d) => d.toLocaleTimeString([], { hour12: false });

  // Round a maximum up to a clean axis number (1, 2, 5 × 10^k).
  function niceCeil(v) {
    if (v <= 0) return 10;
    const mag = Math.pow(10, Math.floor(Math.log10(v)));
    const r = v / mag;
    const step = r <= 1 ? 1 : r <= 2 ? 2 : r <= 5 ? 5 : 10;
    return step * mag;
  }

  function stateClass(state) {
    if (state === 'RUNNING') return 's1';
    if (TRANSITIONING_STATES.has(state)) return 's2';
    if (state === 'CRASHED') return 'crit';
    if (DORMANT_STATES.has(state)) return 'dim';
    return 'dim';
  }

  // ---- shared tooltip -------------------------------------------------------
  const tip = $('tip');
  function showTip(clientX, clientY, title, rows) {
    tip.replaceChildren();
    if (title) tip.appendChild(el('div', 'tip-title', title));
    for (const r of rows) {
      const row = el('div', 'tip-row');
      row.appendChild(el('span', 'key ' + (r.cls || 'idle')));
      row.appendChild(el('span', 'val', r.value));
      row.appendChild(el('span', 'lbl', r.label));
      tip.appendChild(row);
    }
    tip.hidden = false;
    const pad = 14;
    const rect = tip.getBoundingClientRect();
    let x = clientX + pad;
    let y = clientY + pad;
    if (x + rect.width > window.innerWidth - 8) x = clientX - rect.width - pad;
    if (y + rect.height > window.innerHeight - 8) y = clientY - rect.height - pad;
    tip.style.left = x + 'px';
    tip.style.top = y + 'px';
  }
  function hideTip() { tip.hidden = true; }

  // ---- live status ----------------------------------------------------------
  function setLive(state) {
    const pill = $('live-pill');
    pill.dataset.state = state;
    setText('live-text', { connecting: 'Connecting', live: 'Live', paused: 'Paused', error: 'Disconnected' }[state]);
  }

  // ---- hero + stacked state bar --------------------------------------------
  function renderHero(data) {
    const a = data.actors || {};
    const w = data.workers || {};
    const running = a.running || 0;
    const transitioning = (a.resuming || 0) + (a.creating || 0) + (a.suspending || 0) + (a.other || 0);
    const suspended = a.suspended || 0;
    const errored = a.errored || 0;
    const total = a.total || 0;

    setText('hero-total', fmt(total));
    setText('hero-workers', fmt(w.workerpool_ready || 0));
    const ratio = a.multiplex_ratio || 0;
    setText('hero-multiplex', (ratio >= 10 ? ratio.toFixed(0) : ratio.toFixed(1)) + '×');
    setText('stack-total', total ? fmt(total) + ' actors' : 'no actors');
    setText('leg-running', fmt(running));
    setText('leg-transitioning', fmt(transitioning));
    setText('leg-suspended', fmt(suspended));
    setText('leg-errored', fmt(errored));
    $('errored-row').hidden = errored === 0;

    const stack = $('state-stack');
    const parts = [
      { cls: 's1', label: 'Running', v: running },
      { cls: 's2', label: 'Transitioning', v: transitioning },
      { cls: 'dim', label: 'Suspended in GCS', v: suspended },
    ];
    const denom = Math.max(1, running + transitioning + suspended);
    let segs = Array.from(stack.children);
    if (segs.length !== parts.length) {
      stack.replaceChildren();
      segs = parts.map((p) => {
        const s = el('div', 'seg ' + p.cls);
        s.addEventListener('pointermove', (ev) => {
          showTip(ev.clientX, ev.clientY, 'Actor states', [{ cls: p.cls, value: s.dataset.value, label: p.label + ' · ' + s.dataset.pct + '%' }]);
        });
        s.addEventListener('pointerleave', hideTip);
        stack.appendChild(s);
        return s;
      });
    }
    parts.forEach((p, i) => {
      segs[i].style.flexBasis = ((p.v / denom) * 100).toFixed(2) + '%';
      segs[i].style.minWidth = p.v > 0 ? '3px' : '0'; // a non-zero sliver stays visible; the legend carries the exact count
      segs[i].dataset.value = fmt(p.v);
      segs[i].dataset.pct = String(Math.round((p.v / denom) * 100));
    });
  }

  // ---- stat tiles + sparklines ---------------------------------------------
  function renderTiles(data) {
    const a = data.actors || {};
    const w = data.workers || {};
    const q = data.queue || {};
    setText('kpi-running', fmt(a.running));
    setText('kpi-transitioning', fmt((a.resuming || 0) + (a.creating || 0) + (a.suspending || 0) + (a.other || 0)));
    setText('kpi-resuming', fmt((a.resuming || 0) + (a.creating || 0)));
    setText('kpi-suspending', fmt(a.suspending));
    setText('kpi-suspended', fmt(a.suspended));
    setText('kpi-pending', fmt(q.pending_count));
    setText('kpi-lag', fmt(q.lag));
    setText('kpi-consumers', fmt(q.consumers));
    setText('kpi-stream-len', fmt(q.stream_length));
    setText('kpi-ctrl-ready', fmt(w.controller_ready));
    setText('kpi-ctrl-desired', fmt(w.controller_desired));
    setText('kpi-claimed', fmt(w.workerpool_claimed));
    setText('kpi-idle', fmt(w.workerpool_idle));

    const util = Math.min(100, Math.max(0, w.worker_utilization_pct || 0));
    setText('kpi-util', util.toFixed(0) + '%');
    $('util-fill').style.width = util.toFixed(1) + '%';
    $('util-meter').setAttribute('aria-valuenow', util.toFixed(0));

    for (const svg of document.querySelectorAll('.spark')) renderSpark(svg, svg.dataset.spark);
  }

  function renderSpark(svg, key) {
    svg.replaceChildren();
    const pts = history.slice(-SPARK_POINTS);
    if (pts.length < 2) return;
    const W = 120, H = 32, padY = 4;
    const max = Math.max(1, ...pts.map((p) => p[key] || 0));
    const x = (i) => 2 + (W - 4) * (1 - (pts.length - 1 - i) / (SPARK_POINTS - 1)); // newest sample at the right edge
    const y = (v) => H - padY - ((v || 0) / max) * (H - 2 * padY);
    const d = pts.map((p, i) => (i ? 'L' : 'M') + x(i).toFixed(1) + ' ' + y(p[key]).toFixed(1)).join(' ');
    svg.appendChild(svgEl('path', { d }, 'spark-line'));
    const last = pts[pts.length - 1];
    svg.appendChild(svgEl('circle', { cx: x(pts.length - 1).toFixed(1), cy: y(last[key]).toFixed(1), r: 3 }, 'spark-dot'));
  }

  // ---- worker pool grid ----------------------------------------------------
  function renderWorkerGrid(data) {
    const w = data.workers || {};
    const list = data.actor_list || [];
    const ready = w.workerpool_ready || 0;
    const desired = Math.max(w.workerpool_desired || 0, ready);
    const claimed = Math.min(ready, w.workerpool_claimed || 0);
    setText('pool-ready', fmt(ready));
    setText('pool-desired', fmt(desired));

    const byPod = new Map();
    for (const it of list) {
      if (!it.worker_pod || DORMANT_STATES.has(it.state)) continue;
      if (!byPod.has(it.worker_pod)) byPod.set(it.worker_pod, { ip: it.worker_ip || '', actors: [] });
      byPod.get(it.worker_pod).actors.push(it);
    }
    const pods = Array.from(byPod.keys()).sort();
    const sig = [desired, ready, claimed, pods.join(',')].join('|');
    if (sig === workerGridSig) return;
    workerGridSig = sig;

    const grid = $('worker-grid');
    grid.replaceChildren();
    hideTip();
    if (desired === 0) {
      grid.appendChild(el('div', 'empty', 'No worker pool data yet.'));
      return;
    }
    const addTile = (cls, title, rows) => {
      const t = el('div', 'wtile ' + cls);
      t.tabIndex = 0;
      const show = (ev) => showTip(ev.clientX ?? t.getBoundingClientRect().left, ev.clientY ?? t.getBoundingClientRect().top, title, rows);
      t.addEventListener('pointermove', show);
      t.addEventListener('focus', show);
      t.addEventListener('pointerleave', hideTip);
      t.addEventListener('blur', hideTip);
      grid.appendChild(t);
    };
    let placed = 0;
    for (const pod of pods.slice(0, claimed)) {
      const info = byPod.get(pod);
      const rows = info.actors.slice(0, 4).map((it) => ({ cls: stateClass(it.state), value: it.state, label: it.name }));
      if (info.actors.length > 4) rows.push({ cls: 'idle', value: '+' + (info.actors.length - 4), label: 'more' });
      addTile('claimed', pod + (info.ip ? ' · ' + info.ip : ''), rows);
      placed++;
    }
    for (; placed < claimed; placed++) addTile('claimed', 'Warm sandbox', [{ cls: 's1', value: 'claimed', label: 'hosting an actor' }]);
    for (let i = 0; i < ready - claimed; i++) addTile('idle', 'Warm sandbox', [{ cls: 'idle', value: 'idle', label: 'ready for a resume' }]);
    for (let i = 0; i < desired - ready; i++) addTile('off', 'Sandbox', [{ cls: 'idle', value: 'not ready', label: 'starting or unavailable' }]);
  }

  // ---- activity chart --------------------------------------------------------
  const chart = {
    fig: $('activity-chart'),
    svg: $('activity-chart').querySelector('svg'),
    m: { l: 56, r: 76, t: 16, b: 30 },
    gGrid: null, gSeries: null, gHover: null, hit: null,
    w: 0, h: 300, n: 0, yMax: 10,
  };
  chart.gGrid = svgEl('g');
  chart.gSeries = svgEl('g');
  chart.gHover = svgEl('g');
  chart.hit = svgEl('rect', {}, 'hit');
  chart.svg.append(chart.gGrid, chart.gSeries, chart.gHover, chart.hit);

  const cx = (i) => chart.m.l + (chart.w - chart.m.l - chart.m.r) * (1 - (chart.n - 1 - i) / (MAX_SAMPLES - 1));
  const cy = (v) => chart.m.t + (chart.h - chart.m.t - chart.m.b) * (1 - Math.min(v, chart.yMax) / chart.yMax);

  function renderChart() {
    const { svg, m, gGrid, gSeries } = chart;
    chart.w = chart.fig.clientWidth || 900;
    chart.h = chart.fig.clientHeight || 300;
    chart.n = history.length;
    const { w, h, n } = chart;
    svg.setAttribute('viewBox', '0 0 ' + w + ' ' + h);
    const pw = w - m.l - m.r;
    const ph = h - m.t - m.b;
    chart.hit.setAttribute('x', m.l); chart.hit.setAttribute('y', m.t);
    chart.hit.setAttribute('width', Math.max(0, pw)); chart.hit.setAttribute('height', Math.max(0, ph));

    let max = 0;
    for (const p of history) max = Math.max(max, p.running, p.transitioning, p.pending);
    chart.yMax = 4 * niceCeil((max * 1.1) / 4); // four clean tick steps

    gGrid.replaceChildren();
    for (let k = 0; k <= 4; k++) {
      const v = (chart.yMax * k) / 4;
      const y = cy(v);
      gGrid.appendChild(svgEl('line', { x1: m.l, x2: w - m.r, y1: y, y2: y }, k === 0 ? 'baseline' : 'grid'));
      const t = svgEl('text', { x: m.l - 10, y: y + 4, 'text-anchor': 'end' }, 'axis');
      t.textContent = fmt(v);
      gGrid.appendChild(t);
    }
    if (n >= 2) {
      const idxs = Array.from(new Set([0, Math.floor((n - 1) / 3), Math.floor((2 * (n - 1)) / 3), n - 1]));
      let prev = '';
      for (const i of idxs) {
        const label = timeLabel(history[i].t);
        if (label === prev) continue;
        prev = label;
        const t = svgEl('text', { x: cx(i), y: h - 8, 'text-anchor': i === n - 1 ? 'end' : i === 0 ? 'start' : 'middle' }, 'axis');
        t.textContent = label;
        gGrid.appendChild(t);
      }
    }

    gSeries.replaceChildren();
    if (n < 2) {
      const t = svgEl('text', { x: m.l + pw / 2, y: m.t + ph / 2, 'text-anchor': 'middle' }, 'placeholder');
      t.textContent = 'Collecting samples…';
      gSeries.appendChild(t);
      renderHover();
      return;
    }
    for (const s of [...SERIES].reverse()) {
      const d = history.map((p, i) => (i ? 'L' : 'M') + cx(i).toFixed(1) + ' ' + cy(p[s.key]).toFixed(1)).join(' ');
      gSeries.appendChild(svgEl('path', { d }, 'series ' + s.cls));
    }
    // End markers with selective direct labels; nudge colliding labels and connect with leaders.
    const last = history[n - 1];
    const ends = SERIES.map((s) => ({ s, y: cy(last[s.key]), v: last[s.key] })).sort((a, b) => a.y - b.y);
    const ly = ends.map((e) => e.y);
    for (let i = 1; i < ly.length; i++) if (ly[i] - ly[i - 1] < 15) ly[i] = ly[i - 1] + 15;
    const over = ly[ly.length - 1] - (m.t + ph);
    if (over > 0) for (let i = 0; i < ly.length; i++) ly[i] -= over;
    const ex = cx(n - 1);
    ends.forEach((e, i) => {
      if (Math.abs(ly[i] - e.y) > 1) gSeries.appendChild(svgEl('line', { x1: ex + 6, y1: e.y, x2: ex + 14, y2: ly[i] }, 'leader'));
      gSeries.appendChild(svgEl('circle', { cx: ex, cy: e.y, r: 4.5 }, 'end ' + e.s.cls));
      const t = svgEl('text', { x: ex + 17, y: ly[i] + 4 }, 'end-label');
      t.textContent = fmt(e.v);
      gSeries.appendChild(t);
    });
    renderHover();
  }

  function renderHover() {
    const g = chart.gHover;
    g.replaceChildren();
    if (hoverIdx === null || hoverIdx >= chart.n || chart.n < 2) return;
    const x = cx(hoverIdx);
    g.appendChild(svgEl('line', { x1: x, x2: x, y1: chart.m.t, y2: chart.h - chart.m.b }, 'crosshair'));
    const p = history[hoverIdx];
    for (const s of SERIES) g.appendChild(svgEl('circle', { cx: x, cy: cy(p[s.key]), r: 4.5 }, 'end ' + s.cls));
  }

  function hoverAt(idx, clientX, clientY) {
    hoverIdx = Math.max(0, Math.min(chart.n - 1, idx));
    renderHover();
    const p = history[hoverIdx];
    if (!p) return;
    showTip(clientX, clientY, timeLabel(p.t), SERIES.map((s) => ({ cls: s.cls, value: fmt(p[s.key]), label: s.label })));
  }

  chart.hit.addEventListener('pointermove', (ev) => {
    if (chart.n < 2) return;
    const rect = chart.svg.getBoundingClientRect();
    const px = ((ev.clientX - rect.left) / rect.width) * chart.w;
    let best = 0, bd = Infinity;
    for (let i = 0; i < chart.n; i++) { const d = Math.abs(cx(i) - px); if (d < bd) { bd = d; best = i; } }
    hoverAt(best, ev.clientX, ev.clientY);
  });
  chart.hit.addEventListener('pointerleave', () => { hoverIdx = null; renderHover(); hideTip(); });
  chart.fig.addEventListener('keydown', (ev) => {
    if (chart.n < 2) return;
    if (ev.key === 'Escape') { hoverIdx = null; renderHover(); hideTip(); return; }
    if (ev.key !== 'ArrowLeft' && ev.key !== 'ArrowRight') return;
    ev.preventDefault();
    const idx = hoverIdx === null ? chart.n - 1 : hoverIdx + (ev.key === 'ArrowLeft' ? -1 : 1);
    const rect = chart.svg.getBoundingClientRect();
    const clamped = Math.max(0, Math.min(chart.n - 1, idx));
    hoverAt(clamped, rect.left + (cx(clamped) / chart.w) * rect.width, rect.top + chart.m.t);
  });
  chart.fig.addEventListener('blur', () => { hoverIdx = null; renderHover(); hideTip(); });
  new ResizeObserver(() => renderChart()).observe(chart.fig);

  function renderActivityTable() {
    const tbody = $('activity-rows');
    tbody.replaceChildren();
    for (let i = history.length - 1; i >= 0; i--) {
      const p = history[i];
      const tr = el('tr');
      tr.appendChild(el('td', null, timeLabel(p.t)));
      for (const s of SERIES) tr.appendChild(el('td', 'num', fmt(p[s.key])));
      tbody.appendChild(tr);
    }
  }

  $('table-toggle').addEventListener('click', (ev) => {
    const on = ev.currentTarget.getAttribute('aria-pressed') !== 'true';
    ev.currentTarget.setAttribute('aria-pressed', String(on));
    $('activity-table').hidden = !on;
    chart.fig.hidden = on;
    if (on) renderActivityTable(); else renderChart();
  });

  // ---- actor table -----------------------------------------------------------
  function renderActorTable(data) {
    const list = data.actor_list || [];
    const breakdown = (data.actors && data.actors.state_breakdown) || {};
    const sel = $('state-filter');
    const states = Object.keys(breakdown).sort();
    const sig = states.join(',');
    if (sig !== stateOptionsSig) {
      stateOptionsSig = sig;
      const keep = sel.value;
      sel.replaceChildren(el('option', null, 'All states'));
      sel.firstChild.value = '';
      for (const s of states) {
        const o = el('option', null, s + ' (' + fmt(breakdown[s]) + ')');
        o.value = s;
        sel.appendChild(o);
      }
      sel.value = states.includes(keep) ? keep : '';
    } else {
      for (const o of sel.options) if (o.value) o.textContent = o.value + ' (' + fmt(breakdown[o.value]) + ')';
    }

    const q = $('actor-filter').value.trim().toLowerCase();
    const st = sel.value;
    const rows = list.filter((it) => (!st || it.state === st) && (!q || (it.name || '').toLowerCase().includes(q) || (it.worker_pod || '').toLowerCase().includes(q)));

    const tbody = $('actor-rows');
    tbody.replaceChildren();
    if (rows.length === 0) {
      const tr = el('tr', 'empty');
      const td = el('td', null, list.length ? 'No actors match the current filter.' : 'No actors reported yet.');
      td.colSpan = 6;
      tr.appendChild(td);
      tbody.appendChild(tr);
    }
    for (const it of rows) {
      const tr = el('tr');
      const name = el('td'); name.appendChild(el('code', null, it.name || '')); tr.appendChild(name);
      const state = el('td');
      const badge = el('span', 'state');
      badge.appendChild(el('span', 'swatch ' + stateClass(it.state)));
      badge.appendChild(document.createTextNode(it.state || ''));
      if (it.has_error) badge.appendChild(document.createTextNode(' ⚠'));
      state.appendChild(badge); tr.appendChild(state);
      const pod = el('td'); if (it.worker_pod) pod.appendChild(el('code', null, it.worker_pod)); else pod.textContent = '—'; tr.appendChild(pod);
      tr.appendChild(el('td', null, it.worker_ip || '—'));
      tr.appendChild(el('td', 'num', fmt(it.version)));
      tr.appendChild(el('td', 'num', it.age || ''));
      tbody.appendChild(tr);
    }
    const total = (data.actors && data.actors.total) || 0;
    const note = list.length < total ? ' · the API returns the ' + fmt(list.length) + ' most relevant' : '';
    setText('actor-count', 'Showing ' + fmt(rows.length) + ' of ' + fmt(total) + ' actors' + note);
  }
  $('actor-filter').addEventListener('input', () => lastData && renderActorTable(lastData));
  $('state-filter').addEventListener('change', () => lastData && renderActorTable(lastData));

  // ---- polling -----------------------------------------------------------------
  async function fetchStats() {
    try {
      const resp = await fetch('/api/stats', { cache: 'no-store' });
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      const data = await resp.json();
      lastData = data;
      $('page').classList.remove('stale');
      setLive(pollMs > 0 ? 'live' : 'paused');

      setText('ns-badge', data.namespace || 'ate-ant');
      setText('last-updated', 'updated ' + timeLabel(new Date(data.timestamp)));
      setText('collect-ms', 'queried in ' + Math.round(data.collect_ms || 0) + ' ms');
      const warn = $('warnings');
      const warnings = data.warnings || [];
      warn.hidden = warnings.length === 0;
      warn.textContent = warnings.join(' · ');

      const a = data.actors || {};
      const q = data.queue || {};
      history.push({
        t: new Date(data.timestamp || Date.now()),
        running: a.running || 0,
        transitioning: (a.resuming || 0) + (a.creating || 0) + (a.suspending || 0) + (a.other || 0),
        suspended: a.suspended || 0,
        pending: (q.pending_count || 0) + (q.lag || 0),
      });
      if (history.length > MAX_SAMPLES) history.shift();
      if (hoverIdx !== null && history.length === MAX_SAMPLES) hoverIdx = Math.max(0, hoverIdx - 1);

      renderHero(data);
      renderTiles(data);
      renderWorkerGrid(data);
      renderActorTable(data);
      if (chart.fig.hidden) renderActivityTable(); else renderChart();
    } catch (_err) {
      $('page').classList.add('stale');
      setLive('error');
    }
  }

  function schedulePoll() {
    if (timer) clearInterval(timer);
    timer = pollMs > 0 ? setInterval(fetchStats, pollMs) : null;
  }

  $('poll-select').addEventListener('change', (ev) => {
    pollMs = parseInt(ev.target.value, 10) || 0;
    setLive(pollMs > 0 ? 'live' : 'paused');
    schedulePoll();
  });

  const themeSel = $('theme-select');
  const applyTheme = (v) => {
    if (v === 'dark' || v === 'light') document.documentElement.dataset.theme = v;
    else delete document.documentElement.dataset.theme;
    renderChart();
  };
  themeSel.value = localStorage.getItem('dashboard-theme') || 'dark';
  applyTheme(themeSel.value);
  themeSel.addEventListener('change', () => { localStorage.setItem('dashboard-theme', themeSel.value); applyTheme(themeSel.value); });

  fetchStats();
  schedulePoll();
})();
