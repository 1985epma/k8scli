// Guided write forms (ADR 0003). Each form collects a few fields, validates them
// with the same bounds as the server, and hands over to confirmChange(), which
// previews the request with a server-side dry run before anything is applied.
//
// There is no free-form input here: no YAML, no manifests, no arbitrary patches.

import { api } from './api.js';
import { $, esc, icon, toast, showModal, setModalHTML, hideModal, modalOpen } from './ui.js';

/* ---------- parsing and validation (pure, also used by the tests) ---------- */

const LABEL = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const SUBDOMAIN = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;
const ENV_NAME = /^[-._a-zA-Z][-._a-zA-Z0-9]*$/;
const CONFIG_KEY = /^[-._a-zA-Z0-9]+$/;
const MAX_ENV = 50;
const MAX_VALUE = 256 * 1024;
const MAX_TOTAL = 1024 * 1024;

const isLabel = (value) => value.length > 0 && value.length <= 63 && LABEL.test(value);
const isSubdomain = (value) => value.length > 0 && value.length <= 253 && SUBDOMAIN.test(value);
const isImage = (value) => value.length > 0 && value.length <= 255 && !/\s/.test(value);
const isPort = (text) => /^\d+$/.test(text) && Number(text) >= 1 && Number(text) <= 65535;
const isInt = (text, min, max) => /^-?\d+$/.test(String(text).trim()) && Number(text) >= min && Number(text) <= max;

// One KEY=VALUE per line. Blank lines and lines starting with # are ignored.
export function parsePairs(text) {
  const pairs = [];
  const errors = [];
  String(text || '').split('\n').forEach((raw, index) => {
    const line = raw.replace(/\r$/, '');
    if (!line.trim() || line.trim().startsWith('#')) return;
    const at = line.indexOf('=');
    if (at < 1) {
      errors.push(`Line ${index + 1}: use KEY=VALUE.`);
      return;
    }
    pairs.push([line.slice(0, at).trim(), line.slice(at + 1)]);
  });
  return { pairs, errors };
}

const CPU_MILLI = (value) => {
  if (/^\d+(\.\d+)?m$/.test(value)) return parseFloat(value);
  return /^\d+(\.\d+)?$/.test(value) ? parseFloat(value) * 1000 : NaN;
};
const MEM_UNITS = { Ki: 1024, Mi: 1024 ** 2, Gi: 1024 ** 3, Ti: 1024 ** 4, k: 1e3, M: 1e6, G: 1e9, T: 1e12 };
const MEM_BYTES = (value) => {
  const match = /^(\d+(?:\.\d+)?)(Ki|Mi|Gi|Ti|k|M|G|T)?$/.exec(value);
  return match ? parseFloat(match[1]) * (MEM_UNITS[match[2]] || 1) : NaN;
};

function checkEnvPairs(pairs, errors) {
  if (pairs.length > MAX_ENV) errors.push(`At most ${MAX_ENV} environment variables.`);
  const seen = new Set();
  for (const [name, value] of pairs) {
    if (!ENV_NAME.test(name)) errors.push(`"${name}" is not a valid variable name.`);
    else if (seen.has(name)) errors.push(`"${name}" appears twice.`);
    seen.add(name);
    if (value.length > MAX_VALUE) errors.push(`The value of ${name} is too long.`);
  }
}

function checkQuantity(label, value, parse, example, errors) {
  if (value.trim() && Number.isNaN(parse(value.trim()))) errors.push(`${label}: use a value such as ${example}.`);
}

/* ---------- the review dialog ---------- */

let R = null; // the review in progress
let reviewToken = 0;

// Trying again only helps when the failure can pass by itself: network or server trouble, or a
// conflict. A refusal such as already_exists or a validation error returns the same answer.
const retryable = (error) => !error || !error.status || error.status >= 500 || error.status === 429 || error.code === 'conflict';

const isProduction = (S) => ((S.app.contexts || []).find((c) => c.name === S.ctx) || {}).production === true;

function cleanup() {
  R = null;
  reviewToken += 1;
  form = null;
  const modal = $('#modal');
  if (modal) modal.classList.remove('f-wide');
}

function widen() {
  $('#modal').classList.add('f-wide');
}

// Refusals that explain a rule instead of reporting a fault.
const SOFT_ERRORS = new Set(['hub_upgrade_unsupported', 'catalog_only']);

const ACTION_TONE = { created: 'ok', updated: 'accent', deleted: 'err', restarted: 'accent', unchanged: 'neutral' };

function reviewHTML() {
  const { S, title, phase } = R;
  const prod = isProduction(S);
  const preview = R.preview;
  let body = '';

  if (phase === 'loading') {
    body = '<span class="skel w3"></span><span class="skel w2"></span><span class="skel w3"></span>';
  } else if (phase === 'error') {
    // Some refusals are not faults: the server is explaining a rule. Show those as information.
    body = SOFT_ERRORS.has(R.error.code)
      ? `<div class="alert alert-info">${icon('info', 'icon icon-lg')}<div class="grow"><strong>Not available here</strong>
        <p>${esc(R.error.message)}</p>${R.error.hint ? `<p>${esc(R.error.hint)}</p>` : ''}</div></div>`
      : `<div class="alert alert-err">${icon('warn', 'icon icon-lg')}<div class="grow"><strong>The preview failed</strong>
      <code class="cli block">${esc(R.error.message)}</code>${R.error.hint ? `<p>${esc(R.error.hint)}</p>` : ''}</div></div>`;
  } else {
    const warnings = preview.changes.filter((line) => line.startsWith('warning:'));
    const changes = preview.changes.filter((line) => !line.startsWith('warning:'));
    const unchanged = preview.action === 'unchanged';
    body = `
      <div class="row g2 wrap"><span class="badge b-${ACTION_TONE[preview.action] || 'neutral'}">${esc(unchanged ? 'No change' : `Will be ${preview.action}`)}</span>
        <strong class="grow">${esc(preview.summary)}</strong></div>
      ${unchanged ? `<div class="alert alert-info">${icon('info', 'icon icon-lg')}<div>Nothing changes. The cluster already matches this request.</div></div>` : ''}
      ${changes.length ? `<ul class="f-changes mono">${changes.map((line) => `<li>${esc(line)}</li>`).join('')}</ul>` : ''}
      ${warnings.map((line) => `<div class="alert alert-warn">${icon('warn', 'icon icon-lg')}<div>${esc(line.replace(/^warning:\s*/, ''))}</div></div>`).join('')}
      ${preview.equivalent ? `<div class="f-equivalent"><div class="muted small">Equivalent command</div>
        <div class="row g2"><code class="cli grow">${esc(preview.equivalent)}</code>
        <button class="btn btn-ghost btn-icon" data-act="copy" data-arg="${esc(preview.equivalent)}" aria-label="Copy command">${icon('copy')}</button></div></div>` : ''}
      ${preview.digest ? `<div class="f-digest"><div class="muted small">Chart archive digest (sha256) shown in this review</div>
        <div class="row g2"><code class="f-digest-value mono grow">${esc(preview.digest)}</code>
        <button class="btn btn-ghost btn-icon" data-act="copy" data-arg="${esc(preview.digest)}" aria-label="Copy digest">${icon('copy')}</button></div>
        <div class="muted small">The apply only goes through if the archive still has this digest.</div></div>` : ''}
      ${S.app.readOnly ? `<div class="alert alert-info">${icon('eye', 'icon icon-lg')}<div>The console is read-only, so you can preview this change but not apply it.</div></div>` : ''}
      ${R.phrase && !S.app.readOnly && !unchanged ? `<div class="alert alert-warn">${icon('warn', 'icon icon-lg')}<div class="grow">
        <strong>${esc(R.phrase.label || 'Type the phrase below to confirm you reviewed this')}</strong>
        <div class="f-phrase mono strong mt1">${esc(R.phrase.phrase)}</div>
        <input id="f-phrase" class="field mono block mt1" value="${esc(R.typedPhrase)}" autocomplete="off" spellcheck="false" aria-label="Type ${esc(R.phrase.phrase)} to confirm"></div></div>` : ''}
      ${prod && !S.app.readOnly && !unchanged ? `<div class="alert alert-err">${icon('warn', 'icon icon-lg')}<div class="grow">
        <strong>${esc(S.ctx)} looks like production</strong>
        <label class="block mt2">Type <span class="mono strong">${esc(S.ctx)}</span> to confirm
          <input id="f-typed" class="field mono block mt1" value="${esc(R.typed)}" autocomplete="off" spellcheck="false"></label></div></div>` : ''}
      ${R.applyError ? `<div class="alert alert-err">${icon('warn', 'icon icon-lg')}<div>${esc(R.applyError)}</div></div>` : ''}`;
  }

  return `
    <div class="modal-head">
      <div class="grow"><h2>${esc(title)}</h2><p class="muted small">Review the changes on <span class="mono">${esc(S.ctx)}</span></p></div>
      <button class="btn btn-ghost btn-icon" data-act="close-panel" aria-label="Close">${icon('x')}</button>
    </div>
    <div class="modal-body">${body}</div>
    <div class="modal-foot">
      ${R.onBack ? '<button class="btn" data-act="f-back">Back</button>' : '<button class="btn" data-act="close-panel">Cancel</button>'}
      ${phase === 'error' ? (retryable(R.error) ? '<button class="btn btn-primary" data-act="f-review-retry">Try again</button>' : '')
        : (R.applyCode === 'digest_changed'
          ? '<button class="btn btn-primary" data-act="f-review-retry">Review again</button>'
          : `<button class="btn btn-primary" id="f-apply" data-act="f-apply"${canApply() ? '' : ' disabled'}>${R.busy ? 'Working…' : 'Apply'}</button>`)}
    </div>`;
}

function canApply() {
  if (!R || R.phase !== 'preview' || R.busy || R.S.app.readOnly) return false;
  if (R.preview.action === 'unchanged' || R.applyCode === 'digest_changed') return false;
  if (R.phrase && R.typedPhrase !== R.phrase.phrase) return false;
  return !isProduction(R.S) || R.typed === R.S.ctx;
}

function drawReview() {
  if (!R) return;
  const active = document.activeElement;
  const id = active && active.id;
  const caret = active && active.selectionStart;
  setModalHTML(reviewHTML());
  const again = id && $('#' + id);
  if (again) {
    again.focus();
    if (typeof caret === 'number') again.setSelectionRange(caret, caret);
  }
}

async function runPreview() {
  const token = ++reviewToken;
  // A new review after a changed digest is a new thing to confirm: the typed phrase does not carry over.
  if (R.applyCode === 'digest_changed') R.typedPhrase = '';
  R.phase = 'loading';
  R.error = null;
  R.applyError = '';
  R.applyCode = '';
  drawReview();
  try {
    const preview = await R.call({ context: R.S.ctx, dryRun: true, confirm: '' });
    if (token !== reviewToken || !R) return; // closed, or a newer preview started
    R.preview = { ...preview, changes: preview.changes || [] };
    R.phase = 'preview';
  } catch (error) {
    if (token !== reviewToken || !R) return;
    R.phase = 'error';
    R.error = error;
  }
  drawReview();
}

async function applyChange() {
  if (!canApply()) return;
  const review = R;
  review.busy = true;
  review.applyError = '';
  drawReview();
  try {
    // Only what the caller asked for is added, because the API refuses fields it does not know.
    const extra = {};
    if (review.phrase) extra.confirmPhrase = review.typedPhrase;
    if (review.preview && review.preview.digest) extra.digest = review.preview.digest;
    const result = await review.call({ context: review.S.ctx, dryRun: false, confirm: review.typed, ...extra });
    if (R !== review) return;
    const done = review.onDone;
    hideModal();
    toast(esc(result.summary || 'Done.'));
    if (done) done();
  } catch (error) {
    if (R !== review) return;
    review.busy = false;
    review.applyCode = error.code || '';
    review.applyError = error.hint ? `${error.message}${/[.!?]$/.test(error.message) ? '' : '.'} ${error.hint}` : error.message;
    drawReview();
  }
}

// Shows the "Review changes" dialog for one write action. `call` receives
// { context, dryRun, confirm } and returns a changeResponse. The dialog first
// runs it as a dry run, then runs it for real when the person presses Apply.
//
// Optional confirmPhrase = { phrase, label } adds a "type this phrase" field; Apply stays disabled until
// it matches (on top of the production field), and call() then also gets m.confirmPhrase. When the
// preview carries a `digest`, call() gets it back as m.digest, so the apply is tied to what was reviewed.
export function confirmChange(S, { title, call, onDone, onBack, confirmPhrase }) {
  const mine = form !== null || R !== null; // a modal that is not ours must be closed, not reused
  if (modalOpen() && !mine) hideModal();
  R = {
    S, title, call, onDone, onBack, typed: '', phrase: confirmPhrase || null, typedPhrase: '',
    phase: 'loading', preview: null, error: null, applyError: '', applyCode: '', busy: false,
  };
  if (modalOpen()) setModalHTML(reviewHTML());
  else showModal(reviewHTML(), cleanup);
  widen();
  runPreview();
}

/* ---------- form framework ---------- */

// form = { def, st, S, onDone }. A def has: title, sub(st), body(st,S), validate(st,S) -> string[],
// request(st,S) -> { title, call }, optional load(f) and structural (Set of state keys that need a redraw).
let form = null;

const field = (label, input, help = '') => `<label class="field-group"><span>${label}</span>${input}${help ? `<small class="muted">${help}</small>` : ''}</label>`;
const textInput = (id, value, placeholder = '', extra = '') => `<input id="${id}" class="field mono" value="${esc(value)}" placeholder="${esc(placeholder)}" autocomplete="off" spellcheck="false"${extra}>`;
// Text with a numeric keypad, not type=number: the caret survives a redraw and "e" or "-" cannot be typed.
const numberInput = (id, value, placeholder = '') => `<input id="${id}" type="text" inputmode="numeric" pattern="[0-9]*" class="field mono tnum" value="${esc(value)}" placeholder="${esc(placeholder)}" autocomplete="off">`;
const areaInput = (id, value, placeholder) => `<textarea id="${id}" class="field mono f-area" rows="4" placeholder="${esc(placeholder)}" autocomplete="off" spellcheck="false">${esc(value)}</textarea>`;
const selectInput = (id, options, current) => `<select id="${id}" class="field mono">${options.map(([value, label]) => `<option value="${esc(value)}"${value === current ? ' selected' : ''}>${esc(label)}</option>`).join('')}</select>`;
const loadingBlock = () => '<span class="skel w3"></span><span class="skel w2"></span><span class="skel w3"></span>';
const loadError = (message) => `<div class="alert alert-err">${icon('warn', 'icon icon-lg')}<div>${esc(message)}</div></div>`;

function errorsHTML(st, errors) {
  if (!st.touched || !errors.length) return '';
  return `<ul class="f-errors">${errors.map((message) => `<li>${esc(message)}</li>`).join('')}</ul>`;
}

function formHTML() {
  const { def, st, S } = form;
  const errors = st.loading || st.loadError ? ['loading'] : def.validate(st, S);
  return `
    <div class="modal-head">
      <div class="grow"><h2>${esc(def.title(st))}</h2><p class="muted small trunc">${esc(def.sub ? def.sub(st, S) : S.ctx)}</p></div>
      <button class="btn btn-ghost btn-icon" data-act="close-panel" aria-label="Close">${icon('x')}</button>
    </div>
    <div class="modal-body">
      ${st.loading ? loadingBlock() : (st.loadError ? loadError(st.loadError) : def.body(st, S))}
      <div id="f-errors">${errorsHTML(st, st.loading ? [] : errors)}</div>
    </div>
    <div class="modal-foot">
      <button class="btn" data-act="close-panel">Cancel</button>
      <button class="btn btn-primary" id="f-go" data-act="f-review"${errors.length ? ' disabled' : ''}>${esc(def.cta || 'Review changes')}</button>
    </div>`;
}

function redraw() {
  if (!form) return;
  const active = document.activeElement;
  const id = active && active.id;
  const caret = active && active.selectionStart;
  const scroll = active && active.scrollTop;
  setModalHTML(formHTML());
  const again = id && $('#' + id);
  if (again) {
    again.focus();
    if (typeof caret === 'number' && again.type !== 'number') again.setSelectionRange(caret, caret);
    if (typeof scroll === 'number') again.scrollTop = scroll;
  }
}

// Typing does not rebuild the dialog: only the error list and the button change.
function light() {
  const { def, st, S } = form;
  const errors = def.validate(st, S);
  const box = $('#f-errors');
  if (box) box.innerHTML = errorsHTML(st, errors);
  const go = $('#f-go');
  if (go) go.disabled = errors.length > 0;
}

function openForm(def, S, st, onDone) {
  if (modalOpen() && form === null && R === null) hideModal(); // not ours: close it, do not reuse it
  const f = { def, st: { touched: false, ...st }, S, onDone };
  form = f;
  if (modalOpen()) setModalHTML(formHTML());
  else showModal(formHTML(), cleanup);
  widen();
  if (def.load && (!def.needsLoad || def.needsLoad(f.st))) {
    f.st.loading = true;
    redraw();
    def.load(f).then(() => { f.st.loading = false; }).catch((error) => {
      f.st.loading = false;
      f.st.loadError = error.message;
    }).finally(() => { if (form === f) redraw(); });
  }
  const first = $('#modal input, #modal select, #modal textarea');
  if (first) first.focus();
}

function review() {
  if (!form) return;
  const { def, st, S, onDone } = form;
  st.touched = true;
  if (def.validate(st, S).length) { light(); return; }
  const request = def.request(st, S);
  confirmChange(S, {
    title: request.title,
    call: request.call,
    onDone,
    onBack: () => { R = null; reviewToken += 1; setModalHTML(formHTML()); },
  });
}

// Binds ids to state keys: fields = { 'f-name': 'name' }. Checkboxes use .checked.
function applyField(def, st, el) {
  const key = (def.fields || {})[el.id];
  if (!key) return false;
  st[key] = el.type === 'checkbox' ? el.checked : el.value;
  return true;
}

/* ---------- deploy ---------- */

const DEPLOY_PRESET = { name: 'web', image: 'nginx:1.27', replicas: '2', port: '80', expose: true, svcType: 'ClusterIP', svcPort: '80' };

export function deployRequest(st) {
  const lines = parsePairs(st.envText);
  const request = {
    namespace: st.namespace.trim(),
    name: st.name.trim(),
    image: st.image.trim(),
    replicas: Number(st.replicas),
    port: st.port.trim() ? Number(st.port) : 0,
    env: lines.pairs.map(([name, value]) => ({ name, value })),
    expose: { enabled: false, type: '', port: 0 },
  };
  for (const [key, field] of [['cpuRequest', 'cpuRequest'], ['memoryRequest', 'memoryRequest'], ['cpuLimit', 'cpuLimit'], ['memoryLimit', 'memoryLimit']]) {
    if (st[key].trim()) request[field] = st[key].trim();
  }
  if (st.expose) request.expose = { enabled: true, type: st.svcType, port: Number((st.svcPort || st.port).trim()) };
  return request;
}

function deployErrors(st) {
  const errors = [];
  if (!isLabel(st.name.trim())) errors.push('Name: lowercase letters, digits and dashes, up to 63 characters.');
  if (!isLabel(st.namespace.trim())) errors.push('Namespace: lowercase letters, digits and dashes.');
  if (!isImage(st.image.trim())) errors.push('Image: for example nginx:1.27 (no spaces).');
  if (!isInt(st.replicas, 1, 100)) errors.push('Replicas: a number from 1 to 100.');
  if (st.port.trim() && !isPort(st.port.trim())) errors.push('Port: a number from 1 to 65535, or leave it empty.');
  const env = parsePairs(st.envText);
  errors.push(...env.errors);
  checkEnvPairs(env.pairs, errors);
  checkQuantity('CPU request', st.cpuRequest, CPU_MILLI, '100m or 0.5', errors);
  checkQuantity('CPU limit', st.cpuLimit, CPU_MILLI, '500m or 1', errors);
  checkQuantity('Memory request', st.memoryRequest, MEM_BYTES, '128Mi or 1Gi', errors);
  checkQuantity('Memory limit', st.memoryLimit, MEM_BYTES, '256Mi or 1Gi', errors);
  if (st.cpuRequest.trim() && st.cpuLimit.trim() && CPU_MILLI(st.cpuLimit.trim()) < CPU_MILLI(st.cpuRequest.trim())) errors.push('CPU limit must not be lower than the request.');
  if (st.memoryRequest.trim() && st.memoryLimit.trim() && MEM_BYTES(st.memoryLimit.trim()) < MEM_BYTES(st.memoryRequest.trim())) errors.push('Memory limit must not be lower than the request.');
  if (st.expose) {
    if (!isPort((st.svcPort || st.port).trim())) errors.push('Service port: enter a port, here or in the container port field.');
    if (/^[0-9]/.test(st.name.trim())) errors.push('A Service name must start with a letter, so the name cannot start with a digit.');
  }
  return errors;
}

const DEPLOY = {
  title: () => 'New deployment',
  sub: (st, S) => `Creates a Deployment on ${S.ctx}`,
  fields: {
    'f-name': 'name', 'f-ns': 'namespace', 'f-image': 'image', 'f-replicas': 'replicas', 'f-port': 'port', 'f-env': 'envText',
    'f-cpur': 'cpuRequest', 'f-memr': 'memoryRequest', 'f-cpul': 'cpuLimit', 'f-meml': 'memoryLimit',
    'f-expose': 'expose', 'f-svctype': 'svcType', 'f-svcport': 'svcPort',
  },
  structural: new Set(['expose']),
  async load(f) {
    try {
      f.st.namespaces = (await api.namespaces(f.S.ctx)).map((n) => n.name);
    } catch {
      f.st.namespaces = []; // the input still works as free text
    }
  },
  body(st) {
    const nsField = st.namespaces && st.namespaces.length
      ? selectInput('f-ns', (st.namespaces.includes(st.namespace) ? st.namespaces : [st.namespace, ...st.namespaces]).map((n) => [n, n]), st.namespace)
      : textInput('f-ns', st.namespace, 'default');
    return `
      <div class="f-grid">
        ${field('Name', textInput('f-name', st.name, 'web'))}
        ${field('Namespace', nsField)}
      </div>
      <div class="f-label-row"><label for="f-image">Image</label><button class="btn btn-ghost f-link" data-act="f-deploy-preset">Try with nginx</button></div>
      ${textInput('f-image', st.image, 'nginx:1.27')}
      <div class="f-grid">
        ${field('Replicas', numberInput('f-replicas', st.replicas, '1'), 'From 1 to 100.')}
        ${field('Container port (optional)', numberInput('f-port', st.port, '80'))}
      </div>
      ${field('Environment variables (optional)', areaInput('f-env', st.envText, 'LOG_LEVEL=info\nREGION=eu-west-1'), 'One KEY=VALUE per line. Use Secrets outside the console for sensitive values.')}
      <button class="btn f-toggle" data-act="f-deploy-resources" aria-expanded="${st.showRes}">${icon(st.showRes ? 'down' : 'right')}Resources (optional)</button>
      ${st.showRes ? `<div class="f-grid">
        ${field('CPU request', textInput('f-cpur', st.cpuRequest, '100m'))}
        ${field('CPU limit', textInput('f-cpul', st.cpuLimit, '500m'))}
        ${field('Memory request', textInput('f-memr', st.memoryRequest, '128Mi'))}
        ${field('Memory limit', textInput('f-meml', st.memoryLimit, '256Mi'))}
      </div>` : ''}
      <label class="check"><input type="checkbox" id="f-expose"${st.expose ? ' checked' : ''}> Expose with a Service</label>
      ${st.expose ? `<div class="f-grid">
        ${field('Service type', selectInput('f-svctype', [['ClusterIP', 'ClusterIP (inside the cluster)'], ['LoadBalancer', 'LoadBalancer (public address)']], st.svcType))}
        ${field('Service port', numberInput('f-svcport', st.svcPort, st.port || '80'))}
      </div>${st.svcType === 'LoadBalancer' ? `<div class="alert alert-warn">${icon('warn', 'icon icon-lg')}<div>A LoadBalancer asks your cloud provider for a public address, which can cost money.</div></div>` : ''}` : ''}
      <p class="muted small">The deployment is labelled <span class="mono">app.kubernetes.io/managed-by=k8scli</span>.</p>`;
  },
  validate: (st) => deployErrors(st),
  request: (st) => {
    const body = deployRequest(st);
    return { title: `Create ${body.name}`, call: (m) => api.createDeployment({ ...m, ...body }) };
  },
};

export function openDeployForm(S, onDone) {
  openForm(DEPLOY, S, {
    name: '', namespace: S.ns && S.ns !== '_all' ? S.ns : 'default', image: '', replicas: '1', port: '', envText: '', showRes: false,
    cpuRequest: '', memoryRequest: '', cpuLimit: '', memoryLimit: '', expose: false, svcType: 'ClusterIP', svcPort: '', namespaces: [],
  }, onDone);
}

/* ---------- update image ---------- */

const IMAGE = {
  title: () => 'Update image',
  sub: (st) => `${st.dep.namespace} / ${st.dep.name}`,
  fields: { 'f-container': 'container', 'f-newimage': 'image' },
  structural: new Set(['container']),
  async load(f) {
    f.st.detail = await api.deployment(f.S.ctx, f.st.dep.namespace, f.st.dep.name);
    f.st.container = f.st.detail.containers[0] ? f.st.detail.containers[0].name : '';
    f.st.image = (f.st.detail.containers[0] || {}).image || '';
  },
  current: (st) => ((st.detail.containers.find((c) => c.name === st.container)) || {}).image || '',
  body(st) {
    const containers = st.detail.containers;
    return `
      ${containers.length > 1 ? field('Container', selectInput('f-container', containers.map((c) => [c.name, c.name]), st.container)) : ''}
      <div><div class="muted small">Current image</div><code class="cli block">${esc(IMAGE.current(st))}</code></div>
      ${field('New image', textInput('f-newimage', st.image, 'nginx:1.27'), 'Changing the image rolls the pods one by one.')}`;
  },
  validate(st) {
    if (!isImage(st.image.trim())) return ['Image: for example nginx:1.27 (no spaces).'];
    if (st.image.trim() === IMAGE.current(st)) return ['That is already the current image.'];
    return [];
  },
  request: (st) => ({
    title: `Update ${st.dep.name}`,
    call: (m) => api.setImage({ ...m, namespace: st.dep.namespace, name: st.dep.name, container: st.container, image: st.image.trim() }),
  }),
};

export function openImageForm(S, dep, onDone) {
  openForm(IMAGE, S, { dep, image: '', container: '', detail: { containers: [] } }, onDone);
}

/* ---------- restart ---------- */

const RESTART = {
  title: () => 'Restart deployment',
  sub: (st) => `${st.dep.namespace} / ${st.dep.name}`,
  cta: 'Review restart',
  body: (st) => `
    <p>This replaces the pods of <strong class="mono">${esc(st.dep.name)}</strong> one by one with fresh ones, using the deployment's rolling update strategy.
      It is the same as <span class="mono">kubectl rollout restart</span>.</p>
    <div class="alert alert-info">${icon('info', 'icon icon-lg')}<div>There is no downtime when the deployment has more than one replica and its readiness checks pass.</div></div>`,
  validate: () => [],
  request: (st) => ({
    title: `Restart ${st.dep.name}`,
    call: (m) => api.restartDeployment({ ...m, namespace: st.dep.namespace, name: st.dep.name }),
  }),
};

export function openRestartForm(S, dep, onDone) {
  openForm(RESTART, S, { dep }, onDone);
}

/* ---------- environment ---------- */

export function envChanges(container, st) {
  const set = {};
  const unset = [];
  const errors = [];
  const names = new Set();
  container.env.forEach((entry, index) => {
    names.add(entry.name);
    if (entry.from) return;
    if (st.removed.has(index)) { unset.push(entry.name); return; }
    const value = index in st.edits ? st.edits[index] : entry.value;
    if (value !== entry.value) set[entry.name] = value;
  });
  const added = new Set();
  st.added.forEach((row) => {
    const name = row.name.trim();
    if (!name && !row.value) return;
    if (!ENV_NAME.test(name)) { errors.push(`"${name}" is not a valid variable name.`); return; }
    if (names.has(name) || added.has(name)) { errors.push(`"${name}" already exists.`); return; }
    added.add(name);
    set[name] = row.value;
  });
  Object.entries(set).forEach(([name, value]) => {
    if (value.length > MAX_VALUE) errors.push(`The value of ${name} is too long.`);
  });
  if (!errors.length && !Object.keys(set).length && !unset.length) errors.push('Change, add or remove at least one variable.');
  return { set, unset, errors };
}

const ENV = {
  title: () => 'Environment variables',
  sub: (st) => `${st.dep.namespace} / ${st.dep.name}`,
  structural: new Set(['container']),
  fields: { 'f-container': 'container' },
  async load(f) {
    f.st.detail = await api.deployment(f.S.ctx, f.st.dep.namespace, f.st.dep.name);
    f.st.container = f.st.detail.containers[0] ? f.st.detail.containers[0].name : '';
  },
  current: (st) => st.detail.containers.find((c) => c.name === st.container) || { env: [] },
  body(st) {
    const containers = st.detail.containers;
    const container = ENV.current(st);
    const rows = container.env.map((entry, index) => {
      if (entry.from) {
        return `<tr class="f-env-ro"><td class="mono">${esc(entry.name)}</td><td class="muted small">from ${esc(entry.from)}</td><td></td></tr>`;
      }
      const removed = st.removed.has(index);
      return `<tr${removed ? ' class="f-env-removed"' : ''}><td class="mono">${esc(entry.name)}</td>
        <td><input id="f-ev-${index}" class="field mono f-wide-field" value="${esc(index in st.edits ? st.edits[index] : entry.value)}" aria-label="Value of ${esc(entry.name)}"${removed ? ' disabled' : ''} autocomplete="off" spellcheck="false"></td>
        <td class="act"><button class="btn btn-ghost btn-icon" data-act="f-env-remove" data-arg="${index}" aria-label="${removed ? 'Undo removing' : 'Remove'} ${esc(entry.name)}">${icon(removed ? 'refresh' : 'trash')}</button></td></tr>`;
    }).join('');
    const added = st.added.map((row, index) => `<tr><td><input id="f-en-${index}" class="field mono" value="${esc(row.name)}" placeholder="NAME" aria-label="New variable name" autocomplete="off" spellcheck="false"></td>
      <td><input id="f-evn-${index}" class="field mono f-wide-field" value="${esc(row.value)}" placeholder="value" aria-label="New variable value" autocomplete="off" spellcheck="false"></td>
      <td class="act"><button class="btn btn-ghost btn-icon" data-act="f-env-drop" data-arg="${index}" aria-label="Discard this row">${icon('x')}</button></td></tr>`).join('');
    return `
      ${containers.length > 1 ? field('Container', selectInput('f-container', containers.map((c) => [c.name, c.name]), st.container)) : ''}
      <div class="scroll-x"><table class="tbl f-env">
        <thead><tr><th scope="col">Name</th><th scope="col">Value</th><th class="act" scope="col"><span class="sr-only">Actions</span></th></tr></thead>
        <tbody>${rows}${added}${!rows && !added ? '<tr><td colspan="3" class="muted">No variables yet.</td></tr>' : ''}</tbody></table></div>
      <div><button class="btn" data-act="f-env-add">${icon('plus')}Add variable</button></div>
      <div class="alert alert-warn">${icon('warn', 'icon icon-lg')}<div>Changing environment variables starts a rollout: the pods are replaced one by one.
        Variables that come from a Secret or ConfigMap are shown but cannot be edited here.</div></div>`;
  },
  validate: (st) => envChanges(ENV.current(st), st).errors,
  request(st) {
    const { set, unset } = envChanges(ENV.current(st), st);
    return {
      title: `Update ${st.dep.name}`,
      call: (m) => api.setEnv({ ...m, namespace: st.dep.namespace, name: st.dep.name, container: st.container, set, unset }),
    };
  },
};

export function openEnvForm(S, dep, onDone) {
  openForm(ENV, S, { dep, container: '', detail: { containers: [] }, edits: {}, removed: new Set(), added: [] }, onDone);
}

function envInput(el, st) {
  let match = /^f-ev-(\d+)$/.exec(el.id);
  if (match) { st.edits[Number(match[1])] = el.value; return true; }
  match = /^f-en-(\d+)$/.exec(el.id);
  if (match) { st.added[Number(match[1])].name = el.value; return true; }
  match = /^f-evn-(\d+)$/.exec(el.id);
  if (match) { st.added[Number(match[1])].value = el.value; return true; }
  return false;
}

/* ---------- autoscale ---------- */

const PRESETS = [
  { id: 'web', label: 'Web service', min: '2', max: '10', cpu: '70', mem: '' },
  { id: 'worker', label: 'Background worker', min: '1', max: '5', cpu: '80', mem: '' },
  { id: 'spiky', label: 'Spiky traffic', min: '3', max: '20', cpu: '60', mem: '80' },
];

export function autoscaleRequest(st) {
  return {
    namespace: st.deployment.namespace,
    deployment: st.deployment.name,
    min: Number(st.min),
    max: Number(st.max),
    cpuPercent: st.cpu.trim() ? Number(st.cpu) : 0,
    memoryPercent: st.mem.trim() ? Number(st.mem) : 0,
  };
}

function autoscaleErrors(st) {
  const errors = [];
  if (!st.deployment) errors.push('Choose a deployment.');
  if (!isInt(st.min, 1, 500)) errors.push('Minimum: a number from 1 to 500.');
  if (!isInt(st.max, 1, 500)) errors.push('Maximum: a number from 1 to 500.');
  if (isInt(st.min, 1, 500) && isInt(st.max, 1, 500) && Number(st.min) > Number(st.max)) errors.push('The minimum cannot be higher than the maximum.');
  if (st.cpu.trim() && !isInt(st.cpu, 1, 200)) errors.push('CPU target: a percentage from 1 to 200, or leave it empty.');
  if (st.mem.trim() && !isInt(st.mem, 1, 200)) errors.push('Memory target: a percentage from 1 to 200, or leave it empty.');
  if (!st.cpu.trim() && !st.mem.trim()) errors.push('Set a CPU or a memory target.');
  return errors;
}

const AUTOSCALE = {
  title: (st) => (st.existing ? 'Edit autoscaler' : 'Autoscale deployment'),
  sub: (st, S) => (st.deployment ? `${st.deployment.namespace} / ${st.deployment.name}` : `Choose a deployment on ${S.ctx}`),
  fields: { 'f-min': 'min', 'f-max': 'max', 'f-cpu': 'cpu', 'f-mem': 'mem' },
  // These redraw so the preset highlight and the "outside min-max" note stay current.
  structural: new Set(['min', 'max', 'cpu', 'mem']),
  needsLoad: (st) => !st.deployment || !!st.existing,
  async load(f) {
    if (f.st.existing) {
      // Editing: the autoscaler's own "current" count lags behind, so ask the deployment itself.
      try {
        const detail = await api.deployment(f.S.ctx, f.st.deployment.namespace, f.st.deployment.name);
        f.st.deployment = { ...f.st.deployment, desired: detail.replicas };
      } catch { /* the form still works without the replica count */ }
      return;
    }
    const scope = f.S.ns && f.S.ns !== '_all' ? f.S.ns : '_all';
    f.st.deployments = await api.deployments(f.S.ctx, scope);
    f.st.deployment = f.st.deployments[0] || null;
  },
  body(st) {
    const picker = st.deployments
      ? field('Deployment', st.deployments.length
        ? selectInput('f-dep', st.deployments.map((d) => [`${d.namespace}/${d.name}`, `${d.namespace} / ${d.name}`]), st.deployment ? `${st.deployment.namespace}/${st.deployment.name}` : '')
        : '<span class="muted">There are no deployments in this scope.</span>')
      : '';
    const presets = PRESETS.map((p) => `<button class="chip" data-act="f-as-preset" data-arg="${p.id}" aria-pressed="${st.min === p.min && st.max === p.max && st.cpu === p.cpu && st.mem === p.mem}">${esc(p.label)}</button>`).join('');
    const replicas = st.deployment && Number.isFinite(st.deployment.desired) ? st.deployment.desired : null;
    const min = Number(st.min);
    const max = Number(st.max);
    const outside = replicas !== null && isInt(st.min, 1, 500) && isInt(st.max, 1, 500) && min <= max && (replicas < min || replicas > max);
    return `
      ${picker}
      ${replicas !== null ? `<p class="muted small">It runs <strong class="tnum">${replicas}</strong> replica${replicas === 1 ? '' : 's'} now.</p>` : ''}
      <div class="row g2 wrap"><span class="muted small">Start from</span>${presets}</div>
      <div class="f-grid">
        ${field('Minimum replicas', numberInput('f-min', st.min, '2'))}
        ${field('Maximum replicas', numberInput('f-max', st.max, '10'))}
        ${field('Target CPU utilization (%)', numberInput('f-cpu', st.cpu, '70'), 'Average across the pods. Leave empty to ignore CPU.')}
        ${field('Target memory utilization (%)', numberInput('f-mem', st.mem, ''), 'Optional.')}
      </div>
      ${outside ? `<div class="alert alert-warn">${icon('warn', 'icon icon-lg')}<div>The deployment runs ${replicas} replicas now. The autoscaler will move it into ${min}–${max}.</div></div>` : ''}
      <div class="alert alert-info">${icon('info', 'icon icon-lg')}<div>The cluster needs <span class="strong">metrics-server</span>, and the containers need CPU requests, or the target is never reached. The review warns if requests are missing.</div></div>`;
  },
  validate: (st) => autoscaleErrors(st),
  request: (st) => {
    const body = autoscaleRequest(st);
    return { title: `${st.existing ? 'Update' : 'Create'} autoscaler for ${body.deployment}`, call: (m) => api.saveAutoscaler({ ...m, ...body }) };
  },
};

export function openAutoscaleForm(S, { deployment, existing } = {}, onDone) {
  // `deployment` wins: the HPA's `target` is "Deployment/name", not a bare name.
  const target = deployment || (existing
    ? { namespace: existing.namespace, name: String(existing.target).replace(/^Deployment\//, '') }
    : null);
  const preset = PRESETS[0];
  openForm(AUTOSCALE, S, {
    existing: existing || null,
    deployment: target,
    deployments: target ? null : [],
    min: existing ? String(existing.min) : preset.min,
    max: existing ? String(existing.max) : preset.max,
    cpu: existing ? (existing.cpuPercent ? String(existing.cpuPercent) : '') : preset.cpu,
    mem: existing ? (existing.memoryPercent ? String(existing.memoryPercent) : '') : preset.mem,
  }, onDone);
}

/* ---------- remove autoscaler ---------- */

const DELETE_HPA = {
  title: () => 'Remove autoscaler',
  sub: (st) => `${st.hpa.namespace} / ${st.hpa.name}`,
  cta: 'Review removal',
  body: (st) => `
    <p>The autoscaler <strong class="mono">${esc(st.hpa.name)}</strong> stops managing <strong class="mono">${esc(st.hpa.target)}</strong>.
      The deployment keeps its current <strong class="tnum">${esc(st.hpa.current)}</strong> replicas until you change them.</p>`,
  validate: () => [],
  request: (st) => ({
    title: `Remove ${st.hpa.name}`,
    call: (m) => api.deleteAutoscaler({ ...m, namespace: st.hpa.namespace, name: st.hpa.name }),
  }),
};

export function openDeleteAutoscalerForm(S, hpa, onDone) {
  openForm(DELETE_HPA, S, { hpa }, onDone);
}

/* ---------- namespace ---------- */

function labelPairs(text, errors) {
  const parsed = parsePairs(text);
  errors.push(...parsed.errors);
  const labels = {};
  for (const [key, value] of parsed.pairs) {
    if (/\s/.test(key) || key.length > 253) errors.push(`"${key}" is not a valid label name.`);
    else if (key in labels) errors.push(`"${key}" appears twice.`);
    labels[key] = value.trim();
  }
  return labels;
}

const NAMESPACE = {
  title: () => 'New namespace',
  sub: (st, S) => `Creates a namespace on ${S.ctx}`,
  fields: { 'f-nsname': 'name', 'f-labels': 'labels' },
  structural: new Set(),
  body: (st) => `
    ${field('Name', textInput('f-nsname', st.name, 'team-payments'), 'Lowercase letters, digits and dashes.')}
    ${field('Labels (optional)', areaInput('f-labels', st.labels, 'team=payments\nenv=staging'), 'One KEY=VALUE per line.')}`,
  validate(st) {
    const errors = [];
    if (!isLabel(st.name.trim())) errors.push('Name: lowercase letters, digits and dashes, up to 63 characters.');
    else if (st.name.trim().startsWith('kube-')) errors.push('Name: "kube-" is reserved for Kubernetes, pick another prefix.');
    labelPairs(st.labels, errors);
    return errors;
  },
  request(st) {
    const labels = labelPairs(st.labels, []);
    return { title: `Create ${st.name.trim()}`, call: (m) => api.createNamespace({ ...m, name: st.name.trim(), labels }) };
  },
};

export function openNamespaceForm(S, onDone) {
  openForm(NAMESPACE, S, { name: '', labels: '' }, onDone);
}

/* ---------- config map ---------- */

const CONFIGMAP = {
  title: () => 'New ConfigMap',
  sub: (st, S) => `Creates a ConfigMap on ${S.ctx}`,
  fields: { 'f-cmname': 'name', 'f-cmns': 'namespace', 'f-cmdata': 'dataText' },
  structural: new Set(),
  async load(f) {
    try {
      f.st.namespaces = (await api.namespaces(f.S.ctx)).map((n) => n.name);
    } catch {
      f.st.namespaces = [];
    }
  },
  body(st) {
    const nsField = st.namespaces && st.namespaces.length
      ? selectInput('f-cmns', (st.namespaces.includes(st.namespace) ? st.namespaces : [st.namespace, ...st.namespaces]).map((n) => [n, n]), st.namespace)
      : textInput('f-cmns', st.namespace, 'default');
    return `
      <div class="f-grid">
        ${field('Name', textInput('f-cmname', st.name, 'app-settings'))}
        ${field('Namespace', nsField)}
      </div>
      ${field('Values', areaInput('f-cmdata', st.dataText, 'LOG_LEVEL=info\nFEATURE_X=on'), 'One KEY=VALUE per line. You can add and edit keys later.')}
      <p class="muted small">ConfigMaps are for non-sensitive settings. The console never lists or reads Secrets.</p>`;
  },
  validate(st) {
    const errors = [];
    if (!isSubdomain(st.name.trim())) errors.push('Name: lowercase letters, digits, dashes and dots.');
    if (!isLabel(st.namespace.trim())) errors.push('Namespace: lowercase letters, digits and dashes.');
    const parsed = parsePairs(st.dataText);
    errors.push(...parsed.errors);
    const seen = new Set();
    let total = 0;
    for (const [key, value] of parsed.pairs) {
      if (!CONFIG_KEY.test(key) || key === '.' || key === '..' || key.length > 253) errors.push(`"${key}" is not a valid key.`);
      else if (seen.has(key)) errors.push(`"${key}" appears twice.`);
      seen.add(key);
      if (value.length > MAX_VALUE) errors.push(`The value of ${key} is too long.`);
      total += key.length + value.length;
    }
    if (total > MAX_TOTAL) errors.push('A ConfigMap can hold at most 1 MiB.');
    return errors;
  },
  request(st) {
    const data = Object.fromEntries(parsePairs(st.dataText).pairs);
    return { title: `Create ${st.name.trim()}`, call: (m) => api.createConfigMap({ ...m, namespace: st.namespace.trim(), name: st.name.trim(), data }) };
  },
};

export function openConfigMapForm(S, { namespace } = {}, onDone) {
  openForm(CONFIGMAP, S, {
    name: '', dataText: '', namespaces: [],
    namespace: namespace || (S.ns && S.ns !== '_all' ? S.ns : 'default'),
  }, onDone);
}

/* ---------- events (called from app.js) ---------- */

export function formsClick(act, el) {
  if (!act.startsWith('f-')) return false;
  const st = form && form.st;

  switch (act) {
    case 'f-review': review(); break;
    case 'f-review-retry': if (R) runPreview(); break;
    case 'f-apply': applyChange(); break;
    case 'f-back': if (R && R.onBack) R.onBack(); break;
    case 'f-deploy-preset': Object.assign(st, DEPLOY_PRESET, { touched: true }); redraw(); break;
    case 'f-deploy-resources': st.showRes = !st.showRes; redraw(); break;
    case 'f-env-add': st.added.push({ name: '', value: '' }); redraw(); break;
    case 'f-env-drop': st.added.splice(Number(el.dataset.arg), 1); st.touched = true; redraw(); break;
    case 'f-env-remove': {
      const index = Number(el.dataset.arg);
      if (st.removed.has(index)) st.removed.delete(index); else st.removed.add(index);
      st.touched = true;
      redraw();
      break;
    }
    case 'f-as-preset': {
      const preset = PRESETS.find((p) => p.id === el.dataset.arg);
      Object.assign(st, { min: preset.min, max: preset.max, cpu: preset.cpu, mem: preset.mem, touched: true });
      redraw();
      break;
    }
    default: return false;
  }
  return true;
}

export function formsInput(el) {
  if (!el.id || !el.id.startsWith('f-')) return false;

  if (el.id === 'f-typed' || el.id === 'f-phrase') {
    if (R) {
      if (el.id === 'f-typed') R.typed = el.value;
      else R.typedPhrase = el.value;
      const apply = $('#f-apply');
      if (apply) apply.disabled = !canApply();
    }
    return true;
  }
  if (!form) return true;

  const { def, st } = form;
  st.touched = true;

  if (el.id === 'f-dep') {
    st.deployment = st.deployments.find((d) => `${d.namespace}/${d.name}` === el.value) || null;
    redraw();
    return true;
  }
  if (el.id === 'f-container') {
    st.container = el.value;
    if (def === ENV) { st.edits = {}; st.removed = new Set(); st.added = []; }
    if (def === IMAGE) st.image = IMAGE.current(st);
    redraw();
    return true;
  }
  if (def === ENV && envInput(el, st)) { light(); return true; }

  if (applyField(def, st, el)) {
    if (def.structural && def.structural.has(def.fields[el.id])) redraw();
    else light();
  }
  return true;
}

// 'input' already handled every change; swallow 'change' so the page handlers ignore these ids.
export function formsChange(el) {
  return !!(el.id && el.id.startsWith('f-'));
}

export const formHelpers = { parsePairs, envChanges, deployRequest, autoscaleRequest, deployErrors, autoscaleErrors, CPU_MILLI, MEM_BYTES, isLabel, isSubdomain, isImage };
