'use strict';
const $ = id => document.getElementById(id);
const number = v => v == null ? '—' : Number(v).toLocaleString();
const duration = v => (v / 1000).toFixed(2) + ' s';
const date = v => new Date(v).toLocaleString();
let auth = '', identity = null, refreshing = false, pending = false, refreshScheduled = false;
let liveController, liveGeneration = 0, currentView = 'overview', lastProfileName;
let chartSeries = [], performanceData = null, renderCache = new Map();

function message(text) { $('message').textContent = text; $('message').hidden = !text; }
function connection(state) {
  $('connectionState').textContent = state.toUpperCase();
  $('statusDot').className = 'status-dot' + (state === 'live' ? ' live' : state === 'reconnecting' ? ' retry' : '');
}
function lock(text = '') {
  auth = ''; identity = null; liveGeneration++; liveController?.abort(); refreshScheduled = false;
  $('dashboard').hidden = true; $('logout').hidden = true; $('login').hidden = false;
  $('newKey').textContent = ''; $('userToken').textContent = ''; $('adminToken').value = '';
  $('secret').hidden = true; $('userSecret').hidden = true; renderCache.clear(); lastProfileName = undefined;
  for (const id of ['keyRows', 'userRows', 'usageRows', 'requestRows']) $(id).replaceChildren();
  $('keyFilter').replaceChildren(new Option('All keys', '')); $('userFilter').replaceChildren(new Option('All users', '')); $('modelFilter').value = ''; chartSeries = []; performanceData = null;
  connection('locked'); message(text);
}
async function api(path, body) {
  const response = await fetch('/admin/' + path, {
    method: body ? 'POST' : 'GET',
    headers: {Authorization: 'Bearer ' + auth, ...(body ? {'Content-Type': 'application/json'} : {})},
    body: body ? JSON.stringify(body) : undefined
  });
  const value = await response.json();
  if (response.status === 401) { lock('Your session is no longer authorized. Sign in again.'); throw Error('Sign-in token rejected.'); }
  if (!response.ok) throw Error(value.error?.message || 'Request failed');
  return value;
}
function cell(tr, text) { const td = document.createElement('td'); td.textContent = text; tr.append(td); return td; }
function row(id, values) { const tr = document.createElement('tr'); values.forEach(v => cell(tr, v)); $(id).append(tr); return tr; }
function detail(td, text) { const span = document.createElement('span'); span.className = 'subtext'; span.textContent = text; td.append(span); }
function empty(id, cols, text = 'No activity in this window. Awaiting signal.') {
  if (!$(id).children.length) { const tr = row(id, [text]); tr.firstChild.colSpan = cols; tr.firstChild.className = 'empty'; }
}
function changed(id, data) { const signature = JSON.stringify(data); if (renderCache.get(id) === signature) return false; renderCache.set(id, signature); $(id).replaceChildren(); return true; }
function selectOptions(id, items, label, first) {
  const selected = $(id).value, signature = JSON.stringify(items.map(x => [x.id, label(x)]));
  if (renderCache.get(id) === signature) return;
  renderCache.set(id, signature); $(id).replaceChildren(new Option(first, ''));
  items.forEach(item => $(id).add(new Option(label(item), item.id))); $(id).value = selected;
}
function switchView(view) {
  currentView = view;
  document.querySelectorAll('.view').forEach(el => el.hidden = el.id !== 'view-' + view);
  document.querySelectorAll('[data-view]').forEach(el => { const selected = el.dataset.view === view; el.classList.toggle('selected', selected); el.setAttribute('aria-pressed', selected); });
  $('filters').hidden = view === 'access';
}
function action(parent, label, handler, danger = false) {
  const button = document.createElement('button'); button.type = 'button'; button.className = danger ? 'danger' : 'secondary'; button.textContent = label;
  button.onclick = async () => { button.disabled = true; try { await handler(); await refresh(); } catch (e) { message(e.message); } finally { button.disabled = false; } };
  parent.append(button);
}
function actions(tr) { const td = cell(tr, ''); const div = document.createElement('div'); div.className = 'actions'; td.append(div); return div; }
function renderKeys(keys) {
  selectOptions('keyFilter', keys, k => k.name + (k.revoked ? ' (revoked)' : ''), 'All keys');
  if (!changed('keyRows', keys)) return;
  keys.forEach(k => {
    const tr = row('keyRows', [k.name, k.prefix + '…', date(k.created)]); detail(tr.firstChild, k.user_name || 'Administrator');
    const badge = document.createElement('span'); badge.className = 'badge' + (k.revoked ? ' off' : ''); badge.textContent = k.revoked ? 'Revoked' : 'Active'; cell(tr, '').append(badge);
    const target = actions(tr);
    action(target, 'Rename', async () => { const name = prompt('Key name', k.name); if (name) await api('key-update', {id: k.id, name}); });
    if (!k.revoked) action(target, 'Revoke', async () => { if (confirm('Revoke ' + k.name + '? New requests will be denied.')) await api('revoke', {id: k.id}); }, true);
  });
  empty('keyRows', 5, 'No keys yet. Create a key to connect your first client.');
}
function renderUsers(users) {
  for (const id of ['usersPanel', 'userFilterLabel', 'keyOwnerLabel']) $(id).hidden = !identity.admin;
  $('profilePanel').hidden = identity.admin;
  $('identityName').textContent = identity.admin ? 'ADMINISTRATOR' : identity.name;
  if (!identity.admin) {
    if (lastProfileName !== identity.name) { $('profileName').value = identity.name; lastProfileName = identity.name; }
    return;
  }
  selectOptions('userFilter', users, u => u.name, 'All users');
  selectOptions('keyOwner', users.filter(u => !u.disabled), u => u.name, 'Administrator');
  if (!changed('userRows', users)) return;
  users.forEach(u => {
    const tr = row('userRows', [u.name, u.disabled ? 'Disabled' : 'Active', date(u.created)]), target = actions(tr);
    action(target, 'Rename', async () => { const name = prompt('User name', u.name); if (name) await api('users/update', {id: u.id, name, disabled: !!u.disabled}); });
    action(target, u.disabled ? 'Enable' : 'Disable', async () => { if (confirm((u.disabled ? 'Enable ' : 'Disable ') + u.name + '?')) await api('users/update', {id: u.id, disabled: !u.disabled}); });
    action(target, 'Reset token', async () => { if (confirm('Invalidate the sign-in token for ' + u.name + '?')) { const value = await api('users/update', {id: u.id, rotate: true}); showUserToken(value.token); } });
  });
  empty('userRows', 4, 'No users yet. Add a user to issue their own keys.');
}
function renderUsage(usage) {
  const totals = {requests: 0, input_tokens: 0, output_tokens: 0, cached_tokens: 0, errors: 0, unmetered: 0, ms: 0};
  usage.forEach(u => { for (const key in totals) if (key !== 'ms') totals[key] += u[key] || 0; totals.ms += u.avg_ms * u.requests; });
  $('requests').textContent = number(totals.requests); $('input').textContent = number(totals.input_tokens); $('output').textContent = number(totals.output_tokens);
  $('duration').textContent = totals.requests ? duration(totals.ms / totals.requests) : '—';
  $('errors').textContent = number(totals.errors) + ' failed / interrupted'; $('cached').textContent = number(totals.cached_tokens) + ' cached input'; $('unmetered').textContent = number(totals.unmetered) + ' unmetered requests';
  if (!changed('usageRows', usage)) return;
  usage.forEach(u => { const tr = row('usageRows', [u.name, u.model || '—', number(u.requests), number(u.input_tokens), number(u.output_tokens), number(u.cached_tokens), number(u.unmetered), duration(u.avg_ms)]); detail(tr.firstChild, u.user_name || 'Administrator'); });
  empty('usageRows', 8);
}
function renderRequests(requests) {
  if (!changed('requestRows', requests)) return;
  requests.forEach(r => {
    const tr = row('requestRows', [date(r.time), r.key_name, r.method + ' ' + r.path, r.source, r.status, number(r.input_tokens) + ' / ' + number(r.output_tokens), duration(r.duration_ms), r.outcome]);
    detail(tr.children[1], r.model || '—'); tr.children[4].className = r.status >= 400 ? 'badge off' : 'badge';
  });
  empty('requestRows', 8);
}
function query() {
  const q = new URLSearchParams();
  for (const [param, id] of [['key', 'keyFilter'], ['user', 'userFilter'], ['model', 'modelFilter']]) if ($(id).value.trim()) q.set(param, $(id).value.trim());
  if (+$('period').value) q.set('since', new Date(Date.now() - $('period').value * 3600000).toISOString());
  return q;
}
async function refresh() {
  if (!auth) return;
  if (refreshing) { pending = true; return; }
  refreshing = true; const session = auth;
  try {
    do {
      pending = false; const q = query();
      const [me, keys, usage, requests, status, series, users] = await Promise.all([
        api('me'), api('keys'), api('usage?' + q), api('requests?' + q), api('status'), api('performance?' + q), identity.admin ? api('users') : Promise.resolve([])
      ]);
      if (auth !== session) return;
      identity = me; renderKeys(keys); renderUsers(users); renderUsage(usage); renderRequests(requests);
      performanceData = series; chartSeries = series.series; renderCharts(); $('updated').textContent = 'SYNC / ' + new Date().toLocaleTimeString();
      if (status.dropped_records) message('Accounting gap: ' + status.dropped_records + ' records dropped since service start.');
    } while (pending && auth === session);
  } finally { refreshing = false; }
}
function scheduleRefresh() {
  if (refreshScheduled) return;
  refreshScheduled = true;
  queueMicrotask(() => { refreshScheduled = false; refresh().catch(e => message(e.message)); });
}

// One authenticated fetch stream. Only changes trigger data reads; heartbeats
// maintain the connection and advance chart axes using already-loaded data.
async function startLive() {
  liveController?.abort();
  const generation = ++liveGeneration, session = auth; let delay = 500;
  while (generation === liveGeneration && auth === session && auth) {
    const controller = new AbortController(); liveController = controller; let watchdog;
    const touch = () => { clearTimeout(watchdog); watchdog = setTimeout(() => controller.abort(), 45000); };
    try {
      connection('connecting'); touch();
      const response = await fetch('/admin/events', {headers: {Authorization: 'Bearer ' + session}, signal: controller.signal});
      if (response.status === 401) { lock('Your session is no longer authorized. Sign in again.'); return; }
      if (!response.ok || !response.body) throw Error('Live connection unavailable');
      const reader = response.body.getReader(), decoder = new TextDecoder(); let buffer = '';
      for (;;) {
        const {value, done} = await reader.read(); if (done) break; touch(); buffer += decoder.decode(value, {stream: true});
        let end;
        while ((end = buffer.indexOf('\n\n')) !== -1) {
          const event = buffer.slice(0, end).split('\n').find(line => line.startsWith('event:'))?.slice(6).trim(); buffer = buffer.slice(end + 2);
          if (event === 'unauthorized') { lock('Your session is no longer authorized. Sign in again.'); return; }
          if (event === 'ready') { delay = 500; connection('live'); scheduleRefresh(); }
          if (event === 'change') { $('statusDot').classList.add('flash'); setTimeout(() => $('statusDot').classList.remove('flash'), 600); scheduleRefresh(); }
          if (event === 'heartbeat') renderCharts();
        }
      }
    } catch (_) { /* A disconnected stream reconnects and reloads a fresh view. */ }
    finally { clearTimeout(watchdog); controller.abort(); }
    if (generation !== liveGeneration || auth !== session || !auth) return;
    connection('reconnecting'); await new Promise(resolve => setTimeout(resolve, delay)); delay = Math.min(delay * 2, 10000);
  }
}
function showUserToken(value) { $('userToken').textContent = value; $('userSecret').hidden = false; }
function form(id, handler) {
  $(id).onsubmit = async event => { event.preventDefault(); const button = event.target.querySelector('button'); button.disabled = true; message(''); try { await handler(); } catch (e) { message(e.message); } finally { button.disabled = false; } };
}
form('loginForm', async () => {
  auth = $('adminToken').value.trim();
  try { identity = await api('me'); await refresh(); $('adminToken').value = ''; $('login').hidden = true; $('dashboard').hidden = false; $('logout').hidden = false; switchView('overview'); startLive(); }
  catch (e) { lock(e.message); }
});
form('createForm', async () => { const value = await api('keys', {name: $('keyName').value, user_id: $('keyOwner').value ? Number($('keyOwner').value) : null}); $('newKey').textContent = value.key; $('secret').hidden = false; $('keyName').value = ''; await refresh(); });
form('userForm', async () => { const value = await api('users', {name: $('userName').value}); showUserToken(value.token); $('userName').value = ''; await refresh(); });
form('profileForm', async () => { await api('profile', {name: $('profileName').value}); await refresh(); message('Profile saved.'); });
$('dismiss').onclick = () => { $('newKey').textContent = ''; $('secret').hidden = true; };
$('dismissUser').onclick = () => { $('userToken').textContent = ''; $('userSecret').hidden = true; };
$('logout').onclick = () => lock();
$('refresh').onclick = () => { message(''); scheduleRefresh(); };
['period', 'keyFilter', 'modelFilter', 'userFilter'].forEach(id => $(id).onchange = scheduleRefresh);
document.querySelectorAll('[data-view]').forEach(button => button.onclick = () => switchView(button.dataset.view));
$('baseURL').textContent = location.origin + '/v1';
window.addEventListener('offline', () => liveController?.abort());
window.addEventListener('online', () => { if (auth) startLive(); });
window.addEventListener('resize', () => { if (auth) requestAnimationFrame(renderCharts); });
document.addEventListener('visibilitychange', () => { if (!document.hidden && auth) scheduleRefresh(); });

function compact(value) {
  if (value == null) return '—';
  return new Intl.NumberFormat(undefined, {maximumFractionDigits: value < 10 ? 2 : 1, notation: value >= 10000 ? 'compact' : 'standard'}).format(value);
}
function windowLabel() { return $('period').selectedOptions[0].textContent; }
function bucketLabel() {
  const seconds = performanceData.bucket_seconds;
  return seconds < 60 ? seconds + 's' : seconds < 3600 ? (seconds / 60) + 'm' : seconds < 86400 ? (seconds / 3600) + 'h' : (seconds / 86400) + 'd';
}
function renderCharts() {
  if (!performanceData) return;
  document.querySelectorAll('.window-caption').forEach(el => el.textContent = windowLabel() + ' / ' + bucketLabel() + ' buckets');
  drawChart('requestChart', ['requests'], ['#b5ff55']);
  drawChart('tokenChart', ['input_tokens', 'output_tokens'], ['#b5ff55', '#b69aff']);
  drawChart('speedChart', ['output_tps'], ['#b69aff']);
  drawHistogram('latencyHistogram', performanceData.latency, '#b5ff55', 's', 'latencySummary');
  drawHistogram('speedHistogram', performanceData.output_rate, '#b69aff', 'tok/s', 'speedSummary');
}
function svgChart(id, height) {
  const NS = 'http://www.w3.org/2000/svg', svg = document.createElementNS(NS, 'svg');
  const width = window.innerWidth <= 600 ? 360 : 600;
  svg.setAttribute('viewBox', '0 0 ' + width + ' ' + height); svg.setAttribute('role', 'img');
  const labels = {requestChart: 'Requests per minute', tokenChart: 'Tokens per minute', speedChart: 'Mean end-to-end output tokens per second', latencyHistogram: 'Latency histogram', speedHistogram: 'End-to-end output speed histogram'};
  svg.setAttribute('aria-label', labels[id] + ' / ' + windowLabel());
  function el(tag, attrs, text) {
    const element = document.createElementNS(NS, tag); Object.entries(attrs).forEach(([key, value]) => element.setAttribute(key, value));
    if (text != null) element.textContent = text; svg.append(element); return element;
  }
  function title(element, text) { const t = document.createElementNS(NS, 'title'); t.textContent = text; element.append(t); }
  return {svg, width, el, title};
}
function drawChart(id, fields, colors) {
  const {svg, width, el, title} = svgChart(id, 190), right = width - 10, step = performanceData.bucket_seconds;
  const first = Math.floor(Date.parse(performanceData.start) / 1000 / step) * step;
  const last = Math.floor(Date.parse(performanceData.end) / 1000 / step) * step;
  const byTime = new Map(chartSeries.map(r => [Date.parse(r.time) / 1000, r]));
  const bins = Array.from({length: Math.max(1, Math.min(122, Math.round((last - first) / step) + 1))}, (_, i) => ({time: first + i * step, ...byTime.get(first + i * step)}));
  const rate = id === 'speedChart';
  const get = (bin, field) => rate ? (bin[field] ?? null) : (bin[field] || 0) * 60 / step;
  const values = bins.flatMap(bin => fields.map(field => get(bin, field))).filter(v => v != null);
  const largest = Math.max(0, ...values), max = largest > 0 ? largest * 1.1 : 1;
  for (let i = 0; i < 4; i++) {
    const y = 14 + i * 46;
    el('line', {x1: 48, x2: right, y1: y, y2: y, stroke: '#253129', 'stroke-dasharray': '2 5'});
    el('text', {x: 0, y: y + 4, fill: '#89968d', 'font-size': 10}, compact(max * (1 - i / 3)));
  }
  fields.forEach((field, j) => {
    let path = '', open = false;
    bins.forEach((bin, i) => {
      const value = get(bin, field), x = 48 + i * (right - 48) / Math.max(1, bins.length - 1);
      if (value == null) { open = false; return; }
      const y = 152 - value / max * 138;
      path += (open ? ' L ' : ' M ') + x + ',' + y; open = true;
      if (value > 0 || rate) {
        const circle = el('circle', {cx: x, cy: y, r: 3, fill: colors[j]});
        const stamp = typeof bin.time === 'string' ? bin.time : bin.time * 1000;
        title(circle, date(stamp) + ' / ' + compact(value) + (rate ? ' output tok/s / ' + bin.rate_samples + ' requests' : ' per minute / ' + bucketLabel() + ' bucket'));
      }
    });
    el('path', {d: path, fill: 'none', stroke: colors[j], 'stroke-width': 2, 'stroke-linejoin': 'round'});
  });
  if (!values.length || (!rate && !largest)) el('text', {x: (48 + right) / 2, y: 83, fill: '#89968d', 'font-size': 11, 'text-anchor': 'middle'}, 'NO SAMPLES IN WINDOW');
  const startDate = new Date(performanceData.start), endDate = new Date(performanceData.end);
  const format = date => (endDate - startDate) > 86400000 ? date.toLocaleDateString(undefined, {month: 'short', day: 'numeric'}) : date.toLocaleTimeString(undefined, {hour: '2-digit', minute: '2-digit'});
  el('text', {x: 48, y: 180, fill: '#89968d', 'font-size': 10}, format(startDate));
  el('text', {x: right, y: 180, fill: '#b5ff55', 'font-size': 10, 'text-anchor': 'end'}, format(endDate));
  $(id).replaceChildren(svg);
}
function drawHistogram(id, distribution, color, unit, summaryID) {
  const height = 280, {svg, width, el, title} = svgChart(id, height);
  const left = 86, right = width - 40, max = Math.max(1, ...distribution.bins.map(b => b.count));
  const rowHeight = 235 / distribution.bins.length;
  distribution.bins.forEach((bin, i) => {
    const y = 12 + i * rowHeight, label = bin.upper == null ? compact(bin.lower) + '+' : compact(bin.lower) + '–' + compact(bin.upper);
    el('text', {x: left - 10, y: y + rowHeight * .55, 'text-anchor': 'end', fill: '#b7c6bb', 'font-size': 10}, label);
    el('rect', {x: left, y, width: right - left, height: rowHeight - 5, fill: '#152019'});
    const bar = el('rect', {x: left, y, width: bin.count / max * (right - left), height: rowHeight - 5, fill: color, opacity: .75, class: 'histogram-bar', tabindex: bin.count ? 0 : -1, 'aria-label': label + ' ' + unit + ': ' + bin.count + ' requests'});
    title(bar, label + ' ' + unit + ' / ' + bin.count + ' requests / ' + (distribution.samples ? (100 * bin.count / distribution.samples).toFixed(1) : 0) + '%');
    el('text', {x: right + 7, y: y + rowHeight * .55, fill: '#89968d', 'font-size': 10}, compact(bin.count));
  });
  el('text', {x: left, y: 270, fill: '#89968d', 'font-size': 10}, 'REQUEST COUNT →');
  el('text', {x: 8, y: 270, fill: color, 'font-size': 10}, unit.toUpperCase());
  $(id).replaceChildren(svg);
  $(summaryID).textContent = number(distribution.samples) + ' samples / p50 ' + compact(distribution.p50) + ' ' + unit + ' / p95 ' + compact(distribution.p95) + ' ' + unit;
}
