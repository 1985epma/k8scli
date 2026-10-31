// Resilience (disaster recovery, ADR 0005): readiness, backups, plans, runs, drills and reports.
// This module owns the state, the fetching and the events; resilience-views.js renders and
// resilience-util.js holds the pure helpers.
//
// Nothing here can reach the cluster except through the API, which enforces everything again:
// read-only mode, the production gate, the typed confirmation phrase and the opt-in for real runs.
// The page never skips the preview: a real run only unlocks after a dry run of the same selection.

import { api, ApiError } from './api.js';
import { $, toast } from './ui.js';
import { registerPage } from './pages.js';
import { errorState, namespaceSelect } from './views.js';
import { frame, readinessView, backupsView, plansView, runsView, drillsView, toolsView, TABS } from './resilience-views.js';
import { scopePhases, yamlTemplate, PLAN_FILE, humanSeconds, elapsedSeconds } from './resilience-util.js';

const POLL_MS = 1500;

/* ---------- state ---------- */

const tabState = () => ({ status: 'idle', data: null, error: null });

const ST = {
  tab: 'readiness',
  key: '',
  caps: null,
  plans: [],
  nsNames: [],
  tabs: { readiness: tabState(), backups: tabState() },
  ui: { sev: 'low', open: new Set(), closed: new Set() },
  editor: null,
  runForm: { plan: '', direction: 'down', scope: { apps: true, databases: true, nodes: false, power: false }, preview: null, previewing: false, phrase: '', error: '', runError: '', starting: false },
  run: null,
  runsList: tabState(),
  drill: { plan: '', full: false, preview: null, previewing: false, phrase: '', error: '', startError: '', starting: false, active: null },
  reports: tabState(),
  report: null,
  tools: { exportNs: '', diffTo: '', diffNs: '_all', diff: null },
  runKey() { return `${this.runForm.plan}|${this.runForm.direction}|${scopePhases(this.runForm.scope).join(',')}`; },
  drillKey() { return `${this.drill.plan}|${this.drill.full}`; },
};

let appRef = null; // set by the first event; timers use it to redraw
let pollTimer = null;

const toErr = (e) => (e instanceof ApiError ? e : new ApiError(0, { error: (e && e.message) || 'Request failed' }));
const message = (e) => { const x = toErr(e); return x.hint ? `${x.message} ${x.hint}` : x.message; };
const list = (value, ...keys) => (Array.isArray(value) ? value : keys.map((k) => value && value[k]).find(Array.isArray) || []);
const scopeKey = (S) => `${S.ctx}|${S.ns}`;

function scopeBody(f) {
  return { apps: [], databases: [], phases: scopePhases(f.scope) };
}

/* ---------- loading ---------- */

function resetBound() {
  ST.tabs.readiness = tabState();
  ST.tabs.backups = tabState();
  ST.tools.diff = null;
  ST.tools.exportNs = '';
  ST.nsNames = [];
}

function defaults() {
  const valid = ST.plans.filter((p) => p.valid);
  const lab = valid.filter((p) => p.labOnly);
  const pick = (current, from) => (from.some((p) => p.name === current) ? current : (from[0] ? from[0].name : ''));
  ST.runForm.plan = pick(ST.runForm.plan, valid);
  ST.drill.plan = pick(ST.drill.plan, lab);
}

async function fetchTab(S, name) {
  const t = ST.tabs[name];
  t.status = 'loading';
  t.error = null;
  try {
    t.data = name === 'readiness' ? await api.drCheck(S.ctx, S.ns) : await api.drBackups(S.ctx);
    t.status = 'ready';
  } catch (e) {
    t.status = 'error';
    t.error = toErr(e);
  }
}

async function fetchRuns() {
  ST.runsList.status = ST.runsList.data ? 'ready' : 'loading';
  try { ST.runsList = { status: 'ready', data: list(await api.drRuns(20), 'runs'), error: null }; } catch (e) { ST.runsList = { status: 'error', data: null, error: toErr(e) }; }
}

async function fetchReports() {
  ST.reports.status = ST.reports.data ? 'ready' : 'loading';
  try { ST.reports = { status: 'ready', data: list(await api.drReports(20), 'reports'), error: null }; } catch (e) { ST.reports = { status: 'error', data: null, error: toErr(e) }; }
}

// Validity defaults to true when the API does not say, and a plan with problems is never valid.
const normalizePlan = (p) => ({ ...p, valid: p.valid !== false && !(p.problems && p.problems.length) });

async function fetchPlans(quiet = false) {
  try { ST.plans = list(await api.drPlans(), 'plans').map(normalizePlan); } catch (e) { ST.plans = []; if (!quiet) toast(`Could not read the plans: ${message(e)}`, 'warn'); }
  defaults();
}

// Fetches what the visible tab needs. Heavy checks (readiness, backups) are only read when missing or
// forced; cheap lists are read every time so the page stays current.
async function ensureTab(S, force = false) {
  const tab = ST.tab;
  if (tab === 'readiness' || tab === 'backups') {
    const t = ST.tabs[tab];
    if (force || t.status === 'idle' || t.status === 'error') await fetchTab(S, tab);
  } else if (tab === 'runs') {
    await fetchRuns();
  } else if (tab === 'drills') {
    await Promise.all([fetchReports(), fetchRuns()]);
  } else if (tab === 'plans') {
    await fetchPlans();
  }
}

async function loadNamespaces(S) {
  if (ST.nsNames.length) return;
  try { ST.nsNames = (await api.namespaces(S.ctx)).map((n) => n.name).sort(); } catch (e) { ST.nsNames = []; }
}

/* ---------- live runs ---------- */

const liveRuns = () => [ST.run, ST.drill.active].filter((r) => r && r.live);

function visible() {
  return appRef && appRef.S.view === 'resilience' && (ST.tab === 'runs' || ST.tab === 'drills');
}

async function pollOne(run) {
  try {
    const data = await api.drRun(run.id);
    run.data = data.run || data;
    if (data.kind === 'drill') {
      run.live = !!data.live;
      run.reportId = data.reportId || '';
    } else {
      run.live = (data.live !== undefined ? !!data.live : run.data.status === 'running') && run.data.status === 'running';
    }
    run.error = '';
  } catch (e) {
    const err = toErr(e);
    run.live = false;
    // The console only tracks a drill while it is running; after a restart the report is all that is left.
    run.error = run.drill && err.status === 404 ? 'This console no longer tracks the drill (it was restarted). Check the reports.' : message(e);
  }
}

async function tick() {
  pollTimer = null;
  const runs = liveRuns();
  if (!runs.length) return;
  await Promise.all(runs.map(pollOne));
  for (const r of [ST.run, ST.drill.active].filter((x) => x && !x.live && x.data && !x.announced)) {
    r.announced = true;
    if (r.drill && r.reportId) { ST.drill.active = null; openReport(r.reportId); toast('The drill finished. Its report is open below.'); }
    if (visible()) fetchRuns().then(() => { if (visible()) appRef.rerender(); });
    fetchReports();
  }
  if (visible()) appRef.rerender();
  startPolling();
}

function startPolling() {
  if (pollTimer || !liveRuns().length) return;
  pollTimer = setTimeout(tick, POLL_MS);
}

// The elapsed time ticks every second without redrawing the page.
setInterval(() => {
  const el = $('#rs-elapsed');
  if (el && el.dataset.live === '1') el.textContent = humanSeconds(elapsedSeconds(el.dataset.start, null));
}, 1000);

/* ---------- actions ---------- */

function keepFocus(app) {
  const a = document.activeElement;
  const id = a && a.id;
  const start = a && a.selectionStart;
  const end = a && a.selectionEnd;
  app.rerender();
  if (!id) return;
  const again = $(`#${id}`);
  if (!again) return;
  again.focus();
  try { if (typeof start === 'number') again.setSelectionRange(start, end); } catch { /* not a text control */ }
}

const normalizeRun = (data) => (data && (data.run || data.result)) || data;

async function previewRun(app) {
  const f = ST.runForm;
  f.previewing = true; f.error = ''; f.runError = '';
  app.rerender();
  try {
    const data = await api.drStartRun({ plan: f.plan, direction: f.direction, scope: scopeBody(f), dryRun: true, confirm: '' });
    f.preview = { key: ST.runKey(), data: normalizeRun(data) };
    f.phrase = '';
  } catch (e) { f.preview = null; f.error = message(e); }
  f.previewing = false;
  app.rerender();
}

async function startRun(app) {
  const f = ST.runForm;
  if (f.starting) return;
  f.starting = true; f.runError = '';
  app.rerender();
  try {
    const res = await api.drStartRun({ plan: f.plan, direction: f.direction, scope: scopeBody(f), dryRun: false, confirm: f.phrase });
    const id = res.id || (res.run && res.run.id);
    ST.run = { id, data: null, live: true, error: '', phrase: '' };
    f.preview = null; f.phrase = '';
    toast('The run started. You can follow it here.');
    app.scrollTop();
    await pollOne(ST.run);
    startPolling();
    fetchRuns();
  } catch (e) {
    f.runError = message(e);
    // The server only starts what was previewed in the last 30 minutes, byte for byte.
    if (toErr(e).code === 'dry_run_required') f.preview = null;
  }
  f.starting = false;
  app.rerender();
}

async function openRun(app, id) {
  ST.run = { id, data: null, live: false, error: '', phrase: '' };
  app.rerender();
  await pollOne(ST.run);
  startPolling();
  app.rerender();
  app.scrollTop();
}

async function resumeRun(app, id) {
  const run = ST.run && ST.run.id === id ? ST.run : (ST.drill.active && ST.drill.active.id === id ? ST.drill.active : null);
  if (!run) return;
  try {
    await api.drResume({ id, confirm: run.phrase || '' });
    run.live = true; run.announced = false; run.phrase = '';
    toast('Resuming. Steps that are done are skipped.');
    app.scrollTop();
    await pollOne(run);
    startPolling();
  } catch (e) { run.error = message(e); }
  app.rerender();
}

async function abortRun(app, id) {
  try { await api.drAbort({ id }); toast('Abort requested. The run stops after the current step.', 'warn'); } catch (e) { toast(message(e), 'warn'); }
  app.rerender();
}

async function previewDrill(app) {
  const d = ST.drill;
  d.previewing = true; d.error = ''; d.startError = '';
  app.rerender();
  const scope = { apps: true, databases: true, nodes: d.full, power: d.full };
  try {
    const call = (direction) => api.drStartRun({ plan: d.plan, direction, scope: scopeBody({ scope }), dryRun: true, confirm: '' });
    const [down, up] = await Promise.all([call('down'), call('up')]);
    d.preview = { key: ST.drillKey(), down: normalizeRun(down), up: normalizeRun(up) };
    d.phrase = '';
  } catch (e) { d.preview = null; d.error = message(e); }
  d.previewing = false;
  app.rerender();
}

async function startDrill(app) {
  const d = ST.drill;
  if (d.starting) return;
  d.starting = true; d.startError = '';
  app.rerender();
  try {
    const res = await api.drDrill({ plan: d.plan, full: d.full, confirm: d.phrase });
    d.active = { id: res.id, data: null, live: true, error: '', phrase: '', drill: true, plan: d.plan, startedAt: new Date().toISOString() };
    d.preview = null; d.phrase = '';
    toast('The drill started. The plan is going down, then up.');
    app.scrollTop();
    await pollOne(d.active);
    startPolling();
  } catch (e) { d.startError = message(e); }
  d.starting = false;
  app.rerender();
}

async function openReport(id) {
  ST.report = { id, status: 'loading', data: null, error: null };
  if (appRef) appRef.rerender();
  try {
    const data = await api.drReport(id);
    ST.report = { id, status: 'ready', data: data.report || data, error: null };
  } catch (e) { ST.report = { id, status: 'error', data: null, error: toErr(e) }; }
  if (appRef) { appRef.rerender(); appRef.scrollTop(); }
}

async function openPlan(app, name) {
  ST.editor = { isNew: false, name, yaml: '', detail: null, problems: [], error: '', saved: false, saving: false, overwrite: true, loading: true };
  app.rerender();
  try {
    const d = await api.drPlan(name);
    ST.editor = { isNew: false, name, yaml: d.yaml || '', detail: d, problems: d.problems || [], error: '', saved: false, saving: false, overwrite: true };
  } catch (e) { ST.editor = { ...ST.editor, loading: false, error: message(e) }; }
  app.rerender();
}

async function savePlan(app, S) {
  const ed = ST.editor;
  if (!ed || ed.saving) return;
  ed.error = ''; ed.saved = false;
  if (ed.isNew && !PLAN_FILE.test(ed.name || '')) {
    ed.error = 'Use lowercase letters, digits and dashes for the file name, starting and ending with a letter or digit.';
    app.rerender();
    return;
  }
  if (!String(ed.yaml || '').trim()) { ed.error = 'The plan is empty.'; app.rerender(); return; }
  ed.saving = true;
  app.rerender();
  try {
    const res = await api.drSavePlan({ name: ed.name, yaml: ed.yaml, overwrite: ed.isNew ? !!ed.overwrite : true });
    ed.saved = true;
    ed.problems = (res && res.problems) || [];
    await fetchPlans();
    if (ed.isNew) { ed.isNew = false; }
    try { ed.detail = await api.drPlan(ed.name); ed.problems = ed.detail.problems || ed.problems; } catch (e) { /* the summary is optional */ }
    toast('The plan was saved.');
  } catch (e) { ed.error = message(e); }
  ed.saving = false;
  app.rerender();
}

async function runDiff(app, S) {
  const t = ST.tools;
  t.diff = { status: 'loading' };
  app.rerender();
  try {
    t.diff = { status: 'ready', data: await api.drDiff(S.ctx, t.diffTo, t.diffNs) };
  } catch (e) { t.diff = { status: 'error', error: toErr(e) }; }
  app.rerender();
}

/* ---------- page ---------- */

registerPage('resilience', {
  scoped: true,

  async load(S) {
    if (scopeKey(S) !== ST.key) { resetBound(); ST.key = scopeKey(S); }
    ST.caps = await api.drCapabilities();
    await Promise.all([fetchPlans(true), loadNamespaces(S)]);
    const others = (S.app.contexts || []).filter((c) => c.name !== S.ctx);
    if (!others.some((c) => c.name === ST.tools.diffTo)) ST.tools.diffTo = others[0] ? others[0].name : '';
    await ensureTab(S);
    startPolling();
    return {};
  },

  html(S) {
    const L = S.load;
    if (L.status === 'loading') {
      return frame(S, ST, '<div class="rs-stack">' + '<section class="card"><div class="card-head"><span class="skel w2"></span></div><div class="skel-body"><span class="skel w3"></span><span class="skel w3"></span></div></section>'.repeat(2) + '</div>', true);
    }
    if (L.status === 'error') {
      const gone = L.error && (L.error.status === 404 || L.error.status === 501);
      return frame(S, { ...ST, tab: ST.tab }, `<div class="card clip">${gone
        ? '<div class="empty"><h3>Disaster recovery is not available</h3><p class="muted">This console build does not serve the disaster-recovery API yet.</p></div>'
        : errorState(L.error, 'the Resilience page')}</div>`, false);
    }
    const now = Date.now();
    let body = '';
    if (ST.tab === 'readiness') {
      const select = `<div class="rs-toolbar">${namespaceSelect(S, ST.nsNames)}</div>`;
      body = select + readinessView(S, ST.tabs.readiness, ST.ui);
    } else if (ST.tab === 'backups') body = backupsView(S, ST.tabs.backups);
    else if (ST.tab === 'plans') body = plansView(S, ST);
    else if (ST.tab === 'runs') body = runsView(S, ST, now);
    else if (ST.tab === 'drills') body = drillsView(S, ST, now);
    else body = toolsView(S, { ...ST, tools: { ...ST.tools, namespaces: ST.nsNames } });
    const busy = ['readiness', 'backups'].includes(ST.tab) && ST.tabs[ST.tab].status === 'loading';
    return frame(S, ST, body, busy);
  },

  async onClick(act, el, app) {
    if (!act.startsWith('rs-')) return false;
    appRef = app;
    const S = app.S;
    const arg = el.dataset.arg;
    const ui = ST.ui;

    switch (act) {
      case 'rs-tab': {
        if (!TABS.some(([id]) => id === arg)) return true;
        ST.tab = arg;
        const t = ST.tabs[arg];
        // A heavy tab that has no data yet shows its skeleton while it loads.
        const heavy = t && (t.status === 'idle' || t.status === 'error');
        if (heavy) t.status = 'loading';
        app.rerender();
        if (heavy) await fetchTab(S, arg); else await ensureTab(S);
        app.rerender();
        break;
      }
      case 'rs-refresh': {
        ST.nsNames = [];
        if (ST.tabs[ST.tab]) ST.tabs[ST.tab] = { status: 'loading', data: null, error: null };
        app.rerender();
        await Promise.all([fetchPlans(), loadNamespaces(S), ensureTab(S, true)]);
        app.rerender();
        break;
      }
      case 'rs-sev': ui.sev = arg; app.rerender(); break;
      case 'rs-toggle': {
        const isNs = arg.startsWith('ns:');
        const def = el.dataset.defaultOpen === 'true';
        const open = ui.open.has(arg) ? true : ui.closed.has(arg) ? false : (isNs ? def : false);
        if (open) { ui.open.delete(arg); ui.closed.add(arg); } else { ui.closed.delete(arg); ui.open.add(arg); }
        app.rerender();
        break;
      }

      case 'rs-plan-new':
        ST.editor = { isNew: true, name: '', yaml: yamlTemplate(S.ctx), detail: null, problems: [], error: '', saved: false, saving: false, overwrite: false };
        app.rerender();
        break;
      case 'rs-plan-open': await openPlan(app, arg); break;
      case 'rs-plan-close': ST.editor = null; app.rerender(); break;
      case 'rs-plan-example':
        if (ST.editor) { ST.editor.yaml = yamlTemplate(S.ctx); ST.editor.saved = false; app.rerender(); }
        break;
      case 'rs-plan-save': await savePlan(app, S); break;
      case 'rs-plan-preview':
        ST.runForm.plan = arg; ST.runForm.preview = null; ST.tab = 'runs';
        app.rerender();
        await fetchRuns();
        await previewRun(app);
        break;

      case 'rs-dir': ST.runForm.direction = arg; ST.runForm.preview = null; ST.runForm.phrase = ''; app.rerender(); break;
      case 'rs-preview': await previewRun(app); break;
      case 'rs-run': await startRun(app); break;
      case 'rs-abort': await abortRun(app, arg); break;
      case 'rs-resume': await resumeRun(app, arg); break;
      case 'rs-run-open': await openRun(app, arg); break;

      case 'rs-drill-preview': await previewDrill(app); break;
      case 'rs-drill-go': await startDrill(app); break;
      case 'rs-report-open': await openReport(arg); break;

      case 'rs-diff': await runDiff(app, S); break;
      default: return false;
    }
    return true;
  },

  onInput(el, app) {
    appRef = app;
    const { id } = el;
    if (id === 'rs-plan-yaml' && ST.editor) { ST.editor.yaml = el.value; ST.editor.saved = false; return true; }
    // Redraw while typing, with the focus kept. Never on `change`: that fires on blur, and a redraw
    // between mouse-down and mouse-up on the Save button would swallow the click.
    if (id === 'rs-plan-name' && ST.editor) { ST.editor.name = el.value.trim(); keepFocus(app); return true; }
    if (id === 'rs-phrase') { ST.runForm.phrase = el.value; keepFocus(app); return true; }
    if (id === 'rs-drill-phrase') { ST.drill.phrase = el.value; keepFocus(app); return true; }
    if (id === 'rs-resume-phrase') {
      const run = ST.run;
      if (run) { run.phrase = el.value; keepFocus(app); }
      return true;
    }
    return false;
  },

  onChange(el, app) {
    appRef = app;
    const { id } = el;
    const f = ST.runForm;
    if (id === 'rs-plan') { f.plan = el.value; f.preview = null; f.phrase = ''; app.rerender(); return true; }
    if (id.startsWith('rs-scope-')) { f.scope[el.dataset.scope] = el.checked; f.preview = null; f.phrase = ''; app.rerender(); return true; }
    if (id === 'rs-drill-plan') { ST.drill.plan = el.value; ST.drill.preview = null; ST.drill.full = false; app.rerender(); return true; }
    if (id === 'rs-drill-full') { ST.drill.full = el.checked; ST.drill.preview = null; ST.drill.phrase = ''; app.rerender(); return true; }
    if (id === 'rs-plan-name') return true; // handled on input
    if (id === 'rs-plan-overwrite' && ST.editor) { ST.editor.overwrite = el.checked; return true; }
    if (id === 'rs-export-ns') { ST.tools.exportNs = el.value; app.rerender(); return true; }
    if (id === 'rs-diff-to') { ST.tools.diffTo = el.value; ST.tools.diff = null; app.rerender(); return true; }
    if (id === 'rs-diff-ns') { ST.tools.diffNs = el.value; ST.tools.diff = null; app.rerender(); return true; }
    return false;
  },
});
