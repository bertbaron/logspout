'use strict';

// All URLs are relative: the panel runs under an ingress path prefix.
// API data (log lines, rule names) is untrusted: it only goes into the page through textContent.

const $ = (id) => document.getElementById(id);

function h(tag, props, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === 'class') e.className = v;
    else e[k] = v;
  }
  e.append(...kids.filter((k) => k !== null && k !== undefined && k !== false));
  return e;
}

function clear(e) {
  e.replaceChildren();
  return e;
}

// api returns {status, data}; data is null when the body is not JSON. It throws on network errors.
async function api(path, method, body) {
  const opts = { method: method || 'GET', cache: 'no-store' };
  if (body !== undefined) {
    opts.headers = { 'Content-Type': 'application/json' };
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  let data = null;
  try {
    data = await res.json();
  } catch (e) {
    // not JSON
  }
  return { status: res.status, data, retryAfter: res.headers.get('Retry-After') };
}

function errText(r) {
  if (r.data && r.data.error) return `${r.status}: ${r.data.error}`;
  return `HTTP ${r.status}`;
}

function show(box, text, cls) {
  clear(box).append(h('p', { class: cls || '', textContent: text }));
}

// ---- views ----

const views = ['config', 'playground', 'live'];
let current = 'config';

function showView(name) {
  if (current === 'live' && name !== 'live') live.disconnect();
  current = name;
  for (const v of views) {
    const on = v === name;
    $('view-' + v).hidden = !on;
    $('tab-' + v).setAttribute('aria-selected', String(on));
  }
  if (name === 'config') refreshStatus();
}

for (const v of views) $('tab-' + v).addEventListener('click', () => showView(v));

// ---- issues (errors and warnings with line:column) ----

function jumpToLine(line, col) {
  const ta = $('cfg');
  const lines = ta.value.split('\n');
  let start = 0;
  for (let i = 0; i < line - 1 && i < lines.length; i++) start += lines[i].length + 1;
  const len = (lines[line - 1] || '').length;
  showView('config');
  ta.focus();
  ta.setSelectionRange(start + Math.min(Math.max(col - 1, 0), len), start + len);
  const lh = parseFloat(getComputedStyle(ta).lineHeight) || 18;
  ta.scrollTop = Math.max(0, (line - 3) * lh);
}

// link is false when the line numbers do not belong to the draft in the textarea.
function issueList(items, cls, link = true) {
  return h('ul', { class: 'issues' }, ...(items || []).map((it) => {
    const pos = it.line ? `line ${it.line}${it.column ? ':' + it.column : ''}: ` : '';
    if (!it.line || !link) return h('li', { class: cls, textContent: pos + it.message });
    const b = h('button', { type: 'button', class: cls, textContent: pos + it.message });
    b.addEventListener('click', () => jumpToLine(it.line, it.column));
    return h('li', {}, b);
  }));
}

function showIssues(box, head, headCls, data, link = true) {
  clear(box).append(h('p', { class: headCls, textContent: head }));
  if (data && data.errors && data.errors.length) box.append(issueList(data.errors, 'err', link));
  if (data && data.warnings && data.warnings.length) {
    box.append(h('p', { class: 'warn', textContent: 'Warnings:' }), issueList(data.warnings, 'warn', link));
  }
}

// ---- config ----

let saved = ''; // text as last loaded or saved
const cfg = $('cfg');

function dirty() {
  return cfg.value !== saved;
}

function updateDirty() {
  $('cfg-dirty').hidden = !dirty();
}

cfg.addEventListener('input', updateDirty);
window.addEventListener('beforeunload', (e) => {
  if (dirty()) {
    e.preventDefault();
    e.returnValue = '';
  }
});

async function loadConfig() {
  const box = $('cfg-result');
  try {
    const r = await api('api/config');
    if (r.status !== 200) {
      // Saving would overwrite a file that could not be read; Save is disabled until a load works.
      $('cfg-save').disabled = true;
      show(box, 'Cannot load the rule file. ' + errText(r), 'err');
      return;
    }
    $('cfg-save').disabled = false;
    $('cfg-path').textContent = r.data.path;
    saved = r.data.content;
    cfg.value = saved;
    updateDirty();
    clear(box);
  } catch (e) {
    $('cfg-save').disabled = true;
    show(box, 'Cannot reach the add-on: ' + e.message, 'err');
  }
}

async function checkDraft() {
  const box = $('cfg-result');
  try {
    const r = await api('api/validate', 'POST', { content: cfg.value });
    if (r.status !== 200) return show(box, errText(r), 'err');
    if (r.data.valid) showIssues(box, 'The rules are valid.', 'ok', r.data);
    else showIssues(box, 'The rules have errors.', 'err', r.data);
  } catch (e) {
    show(box, 'Request failed: ' + e.message, 'err');
  }
}

async function saveDraft() {
  const box = $('cfg-result');
  const content = cfg.value;
  try {
    const r = await api('api/config', 'PUT', { content });
    if (r.status === 200 || r.status === 409) {
      saved = content;
      updateDirty();
      if (r.status === 200) showIssues(box, 'Saved and applied. ' + (r.data.summary || ''), 'ok', r.data);
      else showIssues(box, 'Saved, but not applied: the file changed on disk during the save.', 'err', r.data);
      refreshStatus();
    } else if (r.status === 422) {
      showIssues(box, 'Not saved: the rules have errors.', 'err', r.data);
    } else {
      show(box, 'Not saved. ' + errText(r), 'err');
    }
  } catch (e) {
    show(box, 'Not saved: ' + e.message, 'err');
  }
}

$('cfg-validate').addEventListener('click', checkDraft);
$('cfg-save').addEventListener('click', saveDraft);
$('cfg-reload').addEventListener('click', () => {
  if (dirty() && !confirm('Discard the unsaved changes?')) return;
  loadConfig();
});

// ---- status and banner ----

let latest = '';

function row(k, ...v) {
  return h('tr', {}, h('th', { scope: 'row', textContent: k }), h('td', {}, ...v));
}

async function refreshStatus() {
  const box = $('status');
  try {
    const r = await api('api/status');
    if (r.status !== 200) return show(box, errText(r), 'err');
    renderStatus(box, r.data);
  } catch (e) {
    show(box, 'Cannot load the status: ' + e.message, 'err');
  }
}

function renderStatus(box, s) {
  const d = s.defaults;
  latest = d.latest;
  const banner = $('banner');
  banner.hidden = !d.newer_available;
  if (d.newer_available) {
    $('banner-text').textContent = `A newer default rule set (${d.latest}) is available; you use ${d.active}. ` +
      `Set "defaults: ${d.latest}" in the rule file (the file overrides the add-on option "default_rules").`;
    $('banner-switch').textContent = `Use ${d.latest} in the draft`;
  }

  const routes = h('table', {}, h('tr', {}, h('th', { textContent: 'Route' }), h('th', { textContent: 'Target rules' })));
  for (const rt of s.routes) {
    const name = h('td', { textContent: rt.name });
    if (rt.ambiguous) name.append(h('span', { class: 'warn', textContent: ' (ambiguous: add #name to the route)' }));
    routes.append(h('tr', {}, name, h('td', { textContent: String(rt.rules) })));
  }
  const file = s.file;
  const valid = file.valid ? h('span', { class: 'ok', textContent: 'valid' }) : h('span', { class: 'err', textContent: 'invalid, not applied' });
  clear(box).append(h('table', {},
    row('Default set', `${d.active} (latest: ${d.latest})`),
    row('Default rules', String(s.rules.defaults)),
    row('Global rules', String(s.rules.global)),
    row('Excluded containers', s.rules.excluded.join(', ') || 'none'),
    row('Rule file', file.exists ? 'exists, ' : 'does not exist, ', valid),
    row('Debug trace (DEBUG_PIPELINE)', s.debug_pipeline ? 'on' : 'off'),
    row('Summary', s.rules.summary || 'no rules active'),
  ));
  if (s.routes.length) box.append(routes);
  else box.append(h('p', { class: 'muted', textContent: 'No routes configured.' }));
  if (file.errors.length) box.append(h('p', { class: 'err', textContent: 'Problems in the active file:' }), issueList(file.errors, 'err', false));
}

// Replace or insert the top level `defaults:` line of the draft.
$('banner-switch').addEventListener('click', () => {
  const line = `defaults: ${latest}`;
  if (/^defaults:[^\n]*/m.test(cfg.value)) {
    cfg.value = cfg.value.replace(/^defaults:[^\n]*/m, line);
  } else {
    // Keep YAML directives and a document start marker first.
    const lines = cfg.value.split('\n');
    let i = 0;
    while (i < lines.length && /^(%.*|---\s*)$/.test(lines[i])) i++;
    lines.splice(i, 0, line);
    cfg.value = lines.join('\n');
  }
  updateDirty();
  showView('config');
  show($('cfg-result'), `The draft now has "${line}". Validate and Save to apply it.`, 'warn');
});

// ---- playground ----

let loaded = []; // messages from the recent-messages buffer
let results = [];
const pick = (m) => ({ container: m.container, image: m.image, source: m.source, time: m.time, data: m.data, level: m.level });

function updateCount() {
  $('pg-count').textContent = loaded.length ? `${loaded.length} recent messages loaded` : '';
}

$('pg-samples').addEventListener('click', async () => {
  try {
    const r = await api('api/samples');
    if (r.status !== 200) return show($('pg-msg'), errText(r), 'err');
    loaded = r.data.samples.map(pick);
    updateCount();
    show($('pg-msg'), loaded.length ? '' : 'No messages captured yet.', 'muted');
  } catch (e) {
    show($('pg-msg'), 'Request failed: ' + e.message, 'err');
  }
});

$('pg-clear').addEventListener('click', () => {
  loaded = [];
  results = [];
  $('pg-lines').value = '';
  updateCount();
  clear($('pg-results'));
  clear($('pg-msg'));
  $('pg-shown').textContent = '';
});

// post runs POST api/test and reports the HTTP errors into box. It returns the data or null.
async function postTest(body, box, issueHead) {
  try {
    const r = await api('api/test', 'POST', body);
    if (r.status === 200) return r.data;
    if (r.status === 422) showIssues(box, issueHead || 'The draft has errors (see the Config view).', 'err', r.data, !issueHead);
    else if (r.status === 413) show(box, 'Too much data: a test takes at most 1000 messages and 4 MiB. Load fewer messages or paste fewer lines.', 'err');
    else if (r.status === 429) show(box, 'The add-on is busy with another test. Try again in a few seconds.', 'warn');
    else show(box, errText(r), 'err');
  } catch (e) {
    show(box, 'Request failed: ' + e.message, 'err');
  }
  return null;
}

$('pg-run').addEventListener('click', async () => {
  const box = $('pg-msg');
  const text = $('pg-lines').value;
  const container = $('pg-container').value.trim();
  const pasted = text === '' ? [] : text.split('\n').filter((l) => l !== '').map((data) => ({ container, data }));
  const body = { content: cfg.value };
  // Without messages the server uses its own recent messages.
  if (loaded.length || pasted.length) body.messages = loaded.concat(pasted);
  show(box, 'Running...', 'muted');
  const data = await postTest(body, box);
  if (!data) return;
  results = data.results;
  clear(box);
  if (data.warnings && data.warnings.length) showIssues(box, 'Warnings:', 'warn', { warnings: data.warnings });
  renderResults();
});

const maxCards = 300;

function stageChanged(res, st) {
  return st.rules.some((r) => r.changed) || st.message !== res.input.data;
}

function isChanged(res) {
  return res.dropped || stageChanged(res, res.global) || res.targets.some((t) => stageChanged(res, t));
}

function stageView(st) {
  const box = h('div', {});
  const fired = st.rules.filter((r) => r.matched || r.error);
  if (!fired.length) box.append(h('div', { class: 'muted', textContent: 'no rule matched' }));
  const ul = h('ul', {});
  for (const r of fired) {
    const li = h('li', {}, h('strong', { textContent: (r.list ? r.list + ': ' : '') + r.name }));
    if (r.actions && r.actions.length) li.append(' [' + r.actions.join(', ') + ']');
    if (r.groups) li.append(h('pre', { textContent: Object.entries(r.groups).map(([k, v]) => `${k} = ${v}`).join('\n') }));
    if (r.error) li.append(h('div', { class: 'err', textContent: r.error }));
    ul.append(li);
  }
  box.append(ul);
  const by = st.dropped_by ? ` by rule "${st.dropped_by.name}"` : '';
  box.append(h('div', {}, `level: ${st.effective_level || st.level}`, st.dropped ? h('span', { class: 'err', textContent: ' dropped' + by }) : ''));
  if (!st.dropped) box.append(h('pre', { textContent: st.message }));
  const f = Object.entries(st.fields || {});
  if (f.length) box.append(h('div', { class: 'muted', textContent: 'fields: ' + f.map(([k, v]) => `${k}=${v}`).join(', ') }));
  return box;
}

function card(res) {
  const g = res.global;
  const c = h('div', { class: 'card' + (res.dropped ? ' dropped' : '') });
  c.append(h('div', { class: 'muted', textContent: `${res.input.container || '(no container)'} ${res.input.source}` }));
  c.append(h('pre', { textContent: res.input.data }));
  c.append(h('h3', { textContent: 'Global rules' + (g.excluded ? ' (container excluded)' : '') }), stageView(g));
  for (const t of res.targets) {
    const state = t.sent ? h('span', { class: 'ok', textContent: ' sent' }) : h('span', { class: 'err', textContent: ' dropped' });
    c.append(h('h3', { textContent: t.name }, state), g.dropped ? '' : stageView(t));
  }
  return c;
}

function renderResults() {
  const onlyChanged = $('pg-changed').checked;
  const onlyDropped = $('pg-dropped').checked;
  const list = results.filter((r) => (!onlyChanged || isChanged(r)) && (!onlyDropped || r.dropped));
  const box = clear($('pg-results'));
  box.append(...list.slice(0, maxCards).map(card));
  $('pg-shown').textContent = results.length ? `${list.length} of ${results.length} messages` + (list.length > maxCards ? `, showing the first ${maxCards}` : '') : '';
}

$('pg-changed').addEventListener('change', renderResults);
$('pg-dropped').addEventListener('change', renderResults);

// A JSON string is also a valid YAML double quoted string, except that YAML folds raw NEL, LS and PS
// and does not allow raw DEL, C1 controls and U+FFFE/U+FFFF: escape them.
const yamlString = (s) => JSON.stringify(s).replace(/[\u007f-\u009f\u2028\u2029\ufffe\uffff]/g, (c) => '\\u' + c.charCodeAt(0).toString(16).padStart(4, '0'));

// The tester sends a one-rule draft.
$('rx-run').addEventListener('click', async () => {
  const box = $('rx-result');
  const draft = `defaults: off\nrules:\n  - name: t\n    when: { match: ${yamlString($('rx-re').value)} }\n    stop: true\n`;
  const data = await postTest({ content: draft, messages: [{ data: $('rx-text').value }] }, box, 'The regex is not valid:');
  if (!data) return;
  const rule = data.results[0].global.rules.find((r) => r.name === 't');
  if (data.results[0].global.excluded) return show(box, 'No match: the empty container name is excluded by exclude_containers.', 'warn');
  if (!rule || !rule.matched) return show(box, 'No match.', 'warn');
  clear(box).append(h('p', { class: 'ok', textContent: 'Match.' }));
  const groups = Object.entries(rule.groups || {});
  if (!groups.length) box.append(h('p', { class: 'muted', textContent: 'The regex has no named groups.' }));
  else box.append(h('table', {}, ...groups.map(([k, v]) => h('tr', {}, h('th', { scope: 'row', textContent: k }), h('td', {}, h('pre', { textContent: v }))))));
});

// ---- live ----

const live = (() => {
  const maxRows = 500;
  const list = $('lv-list');
  const msg = $('lv-msg');
  let ws = null;
  let paused = false;
  let pending = [];
  let lost = 0;

  function addRow(res) {
    const g = res.global;
    const lvl = g.effective_level || g.level;
    const sent = res.targets.map((t) => `${t.name}: ${t.sent ? 'sent' : 'dropped'}`).join(', ');
    // Open a row for the full trace, the same view as in the Playground.
    const line = h('details', { class: 'line' + (res.dropped ? ' dropped' : ''), title: sent },
      h('summary', {},
        h('span', { class: 'lvl lvl-' + lvl, textContent: lvl }),
        h('span', { class: 'muted', textContent: (res.input.container || '-') + ' ' }),
        g.message));
    line.addEventListener('toggle', () => {
      if (line.open && line.childElementCount === 1) line.append(card(res));
    }, { once: true });
    list.append(line);
  }

  function flush(rows) {
    const stick = list.scrollTop + list.clientHeight >= list.scrollHeight - 20;
    rows.forEach(addRow);
    while (list.childElementCount > maxRows) list.firstElementChild.remove();
    if (stick) list.scrollTop = list.scrollHeight;
  }

  function onFrame(ev) {
    let f;
    try {
      f = JSON.parse(ev.data);
    } catch (e) {
      return;
    }
    if (f.type === 'overflow') {
      lost += f.dropped;
      show(msg, `${lost} messages were skipped because they arrived faster than they could be sent.`, 'warn');
    } else if (paused) {
      pending.push(f);
      if (pending.length > maxRows) pending.shift();
    } else {
      flush([f]);
    }
  }

  function setButtons() {
    $('lv-connect').textContent = ws ? 'Disconnect' : 'Connect';
    $('lv-pause').disabled = !ws;
    $('lv-pause').textContent = paused ? 'Resume' : 'Pause';
  }

  function connect() {
    const url = new URL('api/live', location.href);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    url.searchParams.set('container', $('lv-container').value.trim());
    if ($('lv-dropped').checked) url.searchParams.set('show_dropped', 'true');
    lost = 0;
    paused = false;
    pending = [];
    show(msg, 'Connecting...', 'muted');
    const sock = new WebSocket(url);
    let opened = false;
    ws = sock;
    sock.onopen = () => {
      opened = true;
      show(msg, 'Connected.', 'ok');
    };
    sock.onmessage = onFrame;
    sock.onclose = () => {
      if (ws !== sock) return; // closed by us
      ws = null;
      setButtons();
      // The browser does not expose the HTTP status of a refused upgrade.
      show(msg, opened ? 'Disconnected.' : 'Cannot connect. The container glob may be invalid, ' +
        'there may already be 8 live clients, or the add-on is not reachable.', opened ? 'warn' : 'err');
    };
    setButtons();
  }

  function disconnect() {
    const sock = ws;
    ws = null;
    if (sock) sock.close();
    setButtons();
    if (sock) show(msg, 'Disconnected.', 'muted');
  }

  $('lv-connect').addEventListener('click', () => (ws ? disconnect() : connect()));
  $('lv-pause').addEventListener('click', () => {
    paused = !paused;
    if (!paused) {
      flush(pending);
      pending = [];
    }
    setButtons();
  });
  $('lv-clear').addEventListener('click', () => clear(list));
  window.addEventListener('pagehide', disconnect);
  return { disconnect };
})();

// ---- start ----

loadConfig();
refreshStatus();
