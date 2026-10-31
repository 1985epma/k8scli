// The two panels that act on a single object: the log viewer (drawer) and the
// scale dialog (modal). Both mirror a CLI command and show it.

import { api, ApiError } from './api.js';
import { $, esc, icon, cliFor, showDrawer, hideDrawer, showModal, setModalHTML, hideModal, toast } from './ui.js';

/* ---------- logs ---------- */

const MAX_LINES = 5000;
let logs = null;

function logsCommand(S) {
  const l = logs;
  let cmd = `k8scli logs ${l.pod.name}`;
  if (l.pod.containers.length > 1) cmd += ` -c ${l.container}`;
  cmd += ` -n ${l.pod.namespace} -l ${l.tail === 'all' ? '-1' : l.tail}`;
  if (l.since) cmd += ` --since ${l.since}`;
  if (l.timestamps) cmd += ' --timestamps';
  if (l.previous) cmd += ' -p';
  if (l.follow) cmd += ' -f';
  return cliFor(S, cmd);
}

const option = (value, label, current) => `<option value="${esc(value)}"${current === value ? ' selected' : ''}>${esc(label)}</option>`;

export function openLogs(S, pod) {
  logs = { pod, ctx: S.ctx, container: pod.containers[0] || '', tail: '100', since: '', timestamps: false, previous: false, follow: false, abort: null, pinned: true };

  showDrawer(`
    <div class="drawer-head">
      <div class="grow"><div class="muted small">Logs · ${esc(pod.namespace)}</div><h2 class="mono trunc">${esc(pod.name)}</h2></div>
      <button class="btn btn-ghost btn-icon" data-act="close-panel" aria-label="Close logs">${icon('x')}</button>
    </div>
    <div class="toolbar">
      <label class="row g2 muted small">Container
        <select id="lg-container" class="field mono">${pod.containers.map((c) => option(c, c, logs.container)).join('')}</select></label>
      <label class="row g2 muted small">Lines
        <select id="lg-tail" class="field">${[['20', '20'], ['100', '100'], ['500', '500'], ['all', 'All']].map(([v, l]) => option(v, l, logs.tail)).join('')}</select></label>
      <label class="row g2 muted small">Since
        <select id="lg-since" class="field">${[['', 'The start'], ['5m', '5 minutes'], ['1h', '1 hour'], ['24h', '24 hours']].map(([v, l]) => option(v, l, logs.since)).join('')}</select></label>
      <label class="check"><input type="checkbox" id="lg-timestamps"> Timestamps</label>
      <label class="check"><input type="checkbox" id="lg-previous"> Previous container</label>
      <label class="check"><input type="checkbox" id="lg-follow"> Follow</label>
    </div>
    <div class="term" id="term" tabindex="0" role="log" aria-label="Log output"></div>
    <div class="cli-bar">
      <span class="muted small" id="lg-status">Loading…</span><span class="grow"></span>
      <code class="cli" id="lg-cmd"></code>
      <button class="btn btn-ghost btn-icon" data-act="copy-logs" aria-label="Copy command">${icon('copy')}</button>
    </div>`, stopLogs);

  $('#term').addEventListener('scroll', () => {
    const term = $('#term');
    logs.pinned = term.scrollHeight - term.scrollTop - term.clientHeight < 40;
  });
  startLogs(S);
}

export function logsControlChanged(S, id, target) {
  if (!logs) return false;
  const fields = { 'lg-container': 'container', 'lg-tail': 'tail', 'lg-since': 'since' };
  const flags = { 'lg-timestamps': 'timestamps', 'lg-previous': 'previous', 'lg-follow': 'follow' };
  if (fields[id]) logs[fields[id]] = target.value;
  else if (flags[id]) logs[flags[id]] = target.checked;
  else return false;
  startLogs(S);
  return true;
}

export const currentLogsCommand = (S) => (logs ? logsCommand(S) : '');

function stopLogs() {
  if (logs && logs.abort) logs.abort.abort();
  if (logs) logs.abort = null;
}

function setStatus(text) {
  const status = $('#lg-status');
  if (status) status.textContent = text;
}

async function startLogs(S) {
  stopLogs();
  const l = logs;
  l.abort = new AbortController();
  const signal = l.abort.signal;
  const term = $('#term');
  term.textContent = '';
  l.pinned = true;
  $('#lg-cmd').textContent = logsCommand(S);
  setStatus(l.follow ? 'Streaming…' : 'Loading…');

  try {
    const response = await api.logs({
      context: l.ctx, namespace: l.pod.namespace, pod: l.pod.name, container: l.container,
      tail: l.tail, since: l.since, timestamps: l.timestamps, previous: l.previous, follow: l.follow,
    }, signal);

    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let pending = '';
    let count = 0;
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      pending += decoder.decode(value, { stream: true });
      const lines = pending.split('\n');
      pending = lines.pop();
      count += appendLines(term, lines, l);
    }
    if (pending) count += appendLines(term, [pending], l);
    if (signal.aborted) return;
    if (!count) term.textContent = 'No log lines for these options.';
    setStatus(l.follow ? 'Stream closed' : `${count} lines`);
  } catch (error) {
    if (signal.aborted || error.name === 'AbortError') return;
    term.textContent = '';
    const line = document.createElement('div');
    line.className = 'line lv-err';
    line.textContent = error instanceof ApiError ? error.message : `Could not read the log stream: ${error.message}`;
    term.append(line);
    if (error.hint) {
      const hint = document.createElement('div');
      hint.className = 'line ts';
      hint.textContent = error.hint;
      term.append(hint);
    }
    setStatus('Failed');
  }
}

const LEVELS = [[/\b(error|err|fatal|panic)\b/i, 'lv-err'], [/\b(warn|warning)\b/i, 'lv-warn'], [/\binfo\b/i, 'lv-info']];

// Lines are built with textContent, never innerHTML: logs are untrusted input.
function appendLines(term, lines, l) {
  for (const text of lines) {
    const line = document.createElement('div');
    line.className = 'line';
    const level = LEVELS.find(([pattern]) => pattern.test(text));
    if (level) line.classList.add(level[1]);
    line.textContent = text;
    term.append(line);
  }
  while (term.childElementCount > MAX_LINES) term.firstElementChild.remove();
  if (l.pinned) term.scrollTop = term.scrollHeight;
  return lines.length;
}

/* ---------- scale ---------- */

let scale = null;

function scaleCommand(S) {
  const s = scale;
  let cmd = `k8scli scale ${s.dep.name} ${s.replicas} -n ${s.dep.namespace}`;
  cmd += s.dryRun ? ' --dry-run' : (s.production ? ' --yes' : '');
  return cliFor(S, cmd);
}

const scaleReady = () => scale.replicas >= 0 && scale.replicas !== scale.dep.desired && !scale.busy &&
  (scale.dryRun || !scale.production || scale.typed === scale.ctx);

export function openScale(S, dep, onDone) {
  const production = (S.app.contexts.find((c) => c.name === S.ctx) || {}).production === true;
  scale = { dep, ctx: S.ctx, production, readOnly: S.app.readOnly, replicas: dep.desired, dryRun: S.app.readOnly, typed: '', error: '', busy: false, onDone, S };
  showModal(scaleHTML(), () => { scale = null; });
}

function scaleHTML() {
  const s = scale;
  const d = s.dep;
  const quick = [2, 4, 6, 8].map((n) => `<button class="chip" data-act="scale-set" data-arg="${n}" aria-pressed="${s.replicas === n}">${n}</button>`).join('');
  const confirm = s.production && !s.dryRun ? `
    <div class="alert alert-err">${icon('warn', 'icon icon-lg')}<div class="grow">
      <strong>${esc(s.ctx)} looks like production</strong>
      <label class="block mt2">Type <span class="mono strong">${esc(s.ctx)}</span> to confirm
        <input id="scale-typed" class="field mono block mt1" value="${esc(s.typed)}" autocomplete="off" spellcheck="false"></label></div></div>` : '';

  return `
    <div class="modal-head">
      <div class="grow"><h2>Scale deployment</h2><p class="mono muted trunc">${esc(d.namespace)} / ${esc(d.name)} · ${esc(s.ctx)}</p></div>
      <button class="btn btn-ghost btn-icon" data-act="close-panel" aria-label="Close">${icon('x')}</button>
    </div>
    <div class="modal-body">
      <div class="row between g4">
        <div><div class="muted small">Current replicas</div><div class="kpi-value tnum">${d.desired}</div></div>
        <div class="row g2">
          <button class="btn btn-icon" data-act="scale-step" data-arg="-1" aria-label="One fewer">${icon('minus')}</button>
          <input id="scale-replicas" type="number" min="0" step="1" class="field tnum center-text narrow" value="${s.replicas}" aria-label="New number of replicas">
          <button class="btn btn-icon" data-act="scale-step" data-arg="1" aria-label="One more">${icon('plus')}</button>
        </div>
      </div>
      <div class="row g2 wrap"><span class="muted small">Quick scale</span>${quick}</div>
      <label class="check"><input type="checkbox" id="scale-dry"${s.dryRun ? ' checked' : ''}${s.readOnly ? ' disabled' : ''}> Only simulate (changes nothing)</label>
      ${s.readOnly ? '<p class="muted small">The console is read-only, so only simulations are allowed.</p>' : ''}
      ${confirm}
      ${s.replicas === d.desired ? `<p class="muted small">It already has ${d.desired} replicas. Pick a different number.</p>` : ''}
      ${s.error ? `<div class="alert alert-err">${icon('warn', 'icon icon-lg')}<div>${esc(s.error)}</div></div>` : ''}
      <div class="row g2"><code class="cli grow">${esc(scaleCommand(s.S))}</code>
        <button class="btn btn-ghost btn-icon" data-act="copy" data-arg="${esc(scaleCommand(s.S))}" aria-label="Copy command">${icon('copy')}</button></div>
    </div>
    <div class="modal-foot">
      <button class="btn" data-act="close-panel">Cancel</button>
      <button class="btn btn-primary" id="scale-go" data-act="scale-apply"${scaleReady() ? '' : ' disabled'}>${s.busy ? 'Working…' : (s.dryRun ? 'Simulate' : 'Apply')}</button>
    </div>`;
}

function redrawScale() {
  // Keep the caret where it was while the dialog is redrawn under the user's typing.
  const active = document.activeElement;
  const id = active && active.id;
  const caret = active && active.selectionStart;
  setModalHTML(scaleHTML());
  if (id) {
    const again = $('#' + id);
    if (again) {
      again.focus();
      if (typeof caret === 'number' && again.type !== 'number') again.setSelectionRange(caret, caret);
    }
  }
}

export function scaleInput(id, target) {
  if (!scale) return false;
  if (id === 'scale-replicas') scale.replicas = Math.max(0, parseInt(target.value || '0', 10) || 0);
  else if (id === 'scale-typed') scale.typed = target.value;
  else if (id === 'scale-dry') scale.dryRun = target.checked;
  else return false;
  scale.error = '';
  redrawScale();
  return true;
}

export function scaleClick(act, arg) {
  if (!scale) return false;
  if (act === 'scale-step') scale.replicas = Math.max(0, scale.replicas + Number(arg));
  else if (act === 'scale-set') scale.replicas = Number(arg);
  else if (act === 'scale-apply') { applyScale(); return true; }
  else return false;
  scale.error = '';
  redrawScale();
  return true;
}

export const currentScaleCommand = () => (scale ? scaleCommand(scale.S) : '');

export async function applyScale() {
  if (!scale || !scaleReady()) return;
  const s = scale;
  s.busy = true;
  s.error = '';
  redrawScale();
  try {
    const result = await api.scale({
      context: s.ctx, namespace: s.dep.namespace, name: s.dep.name,
      replicas: s.replicas, dryRun: s.dryRun, confirm: s.typed,
    });
    hideModal();
    const name = `<strong>${esc(result.name)}</strong>`;
    if (!result.changed && !result.dryRun) toast(`${name} already has ${result.to} replicas.`);
    else if (result.dryRun) toast(`Simulation: ${name} would go from ${result.from} to ${result.to} replicas. Nothing was changed.`);
    else toast(`${name} scaled from ${result.from} to ${result.to} replicas.`);
    if (s.onDone) s.onDone();
  } catch (error) {
    s.busy = false;
    s.error = error.hint ? `${error.message} ${error.hint}` : error.message;
    if (scale === s) redrawScale();
  }
}

export function closePanels() {
  hideModal();
  hideDrawer();
}
