// Renderers of the Resilience page: pure functions from state to an HTML string. The page module
// (resilience.js) owns the state, the fetching and the events. Everything that comes from the
// cluster or the API goes through esc().

import { esc, icon, badge } from './ui.js';
import { pageHead, emptyState, errorState } from './views.js';
import { cardHead, okNote } from './dashboard.js';
import {
  SEVERITIES, sevRank, sevTone, sevLabel, scoreTone, count, humanSeconds, ageSeconds, ageTone, when, elapsedSeconds,
  PHASE_TITLE, RISK_TONE, STEP_TONE, RUN_TONE, TERMINAL, SCOPE_PARTS, scopePhases, confirmPhraseFor, groupByPhase, previewSummary,
  planOrder, OUTCOME, VERDICT, KIND_TONE, rtoTrend,
} from './resilience-util.js';

export const TABS = [
  ['readiness', 'Readiness'],
  ['backups', 'Backups'],
  ['plans', 'Plans'],
  ['runs', 'Runs'],
  ['drills', 'Drills & reports'],
  ['tools', 'Tools'],
];

/* ---------- shared blocks ---------- */

const loadingCards = (n = 3) => `<div class="rs-stack">${'<section class="card"><div class="card-head"><span class="skel w2"></span></div><div class="skel-body"><span class="skel w3"></span><span class="skel w3"></span><span class="skel w2"></span></div></section>'.repeat(n)}</div>`;

const bar = (command) => `<div class="cli-bar">
  <span class="muted small nowrap">CLI equivalent</span><code class="cli grow">${esc(command)}</code>
  <button class="btn btn-ghost btn-icon" data-act="copy" data-arg="${esc(command)}" aria-label="Copy command">${icon('copy')}</button></div>`;

const notices = (list) => (list && list.length
  ? `<div class="alert alert-warn">${icon('info', 'icon icon-lg')}<div class="grow"><strong>Some data is missing</strong><ul class="notices">${list.map((n) => `<li>${esc(n)}</li>`).join('')}</ul></div></div>` : '');

const alertBox = (tone, title, html) => `<div class="alert alert-${tone}">${icon(tone === 'err' ? 'xcircle' : tone === 'ok' ? 'okcircle' : tone === 'warn' ? 'warn' : 'info', 'icon icon-lg')}<div class="grow"><strong>${esc(title)}</strong>${html ? `<div class="alert-body">${html}</div>` : ''}</div></div>`;

function stateOf(tab, what, render) {
  if (!tab || tab.status === 'idle' || tab.status === 'loading') return loadingCards(2);
  if (tab.status === 'error') return `<div class="card clip">${errorState(tab.error, what)}</div>`;
  return render(tab.data);
}

const option = (value, label, selected, disabled = false) => `<option value="${esc(value)}"${selected ? ' selected' : ''}${disabled ? ' disabled' : ''}>${esc(label)}</option>`;

// The capability line shown above anything that can change a cluster.
function capsNote(caps, { power = false, plan = null } = {}) {
  if (!caps) return '';
  if (caps.readOnly) return alertBox('info', 'The console is read-only', 'You can read, preview and download. Starting a run is blocked.');
  if (!caps.allowRuns) {
    return alertBox('info', 'Real runs are switched off', `Previews work. To run for real, start the console with <span class="mono">k8scli ui --allow-dr-runs</span> (or <span class="mono">serve --allow-dr-runs</span>).`);
  }
  if (power && !caps.allowPower && !(plan && plan.labOnly)) {
    return alertBox('warn', 'Powering machines is switched off for this plan', 'A run that powers nodes or the cluster needs a plan marked <span class="mono">safety.labOnly</span>, or the console started with <span class="mono">--allow-dr-power</span>. Applications and databases can still run.');
  }
  return '';
}

/* ---------- page frame ---------- */

export function frame(S, ST, body, refreshing) {
  const tabs = TABS.map(([id, label]) => `<button class="rs-tab" role="tab" id="rs-tab-${id}" aria-selected="${ST.tab === id}" data-act="rs-tab" data-arg="${id}">${esc(label)}</button>`).join('');
  const refresh = `<button class="btn" data-act="rs-refresh"${refreshing ? ' disabled' : ''}>${icon('refresh')}Refresh</button>`;
  return `${pageHead('Resilience', `Check, rehearse and measure how <span class="mono">${esc(S.ctx)}</span> recovers. The score counts signals; only a drill proves a recovery.`, refresh)}
    <div class="rs-tabs" role="tablist" aria-label="Resilience sections">${tabs}</div>
    <div class="rs-panel" role="tabpanel" aria-labelledby="rs-tab-${ST.tab}">${body}</div>`;
}

/* ---------- readiness ---------- */

export function gauge(score, tone, none = false) {
  const pct = none ? 0 : Math.max(0, Math.min(100, Math.round(score || 0)));
  const circ = 2 * Math.PI * 52;
  const len = (pct / 100) * circ;
  return `<svg class="rs-gauge rs-gauge-${none ? 'none' : tone}" viewBox="0 0 120 120" role="img" aria-label="${none ? 'Nothing to score' : `Readiness score ${pct} of 100`}">
    <circle class="rs-gauge-bg" cx="60" cy="60" r="52" fill="none" stroke-width="10"/>
    <circle class="rs-gauge-fg" cx="60" cy="60" r="52" fill="none" stroke-width="10" stroke-linecap="round" stroke-dasharray="${len.toFixed(1)} ${(circ - len).toFixed(1)}" transform="rotate(-90 60 60)"/>
    <text class="rs-gauge-num" x="60" y="68" text-anchor="middle">${none ? '–' : pct}</text></svg>`;
}

const visibleFindings = (list, min) => (list || []).filter((f) => sevRank(f.severity) >= sevRank(min)).sort((a, b) => sevRank(b.severity) - sevRank(a.severity));

function findingItem(f) {
  const fix = f.fix
    ? `<div class="rs-fix"><span class="muted small">Fix</span><code class="cli">${esc(f.fix)}</code>
        <button class="btn btn-ghost btn-icon" data-act="copy" data-arg="${esc(f.fix)}" aria-label="Copy the fix">${icon('copy')}</button></div>` : '';
  const where = f.name ? `<span class="muted small mono trunc">${esc(f.kind || '')} ${esc(f.name)}</span>` : '';
  return `<li class="rs-finding"><div class="rs-finding-top">${badge(sevLabel(f.severity), sevTone(f.severity))}<span class="mono small nowrap">${esc(f.rule)}</span>${where}</div>
    <div class="rs-finding-msg">${esc(f.message)}</div>${fix}</li>`;
}

function workloadBlock(ns, w, ui) {
  const key = `wl:${ns.name}/${w.kind}/${w.name}`;
  const shown = visibleFindings(w.findings, ui.sev);
  const open = ui.open.has(key);
  const tone = scoreTone(w.score);
  return `<li class="rs-wl">
    <button class="rs-wl-head" data-act="rs-toggle" data-arg="${esc(key)}" aria-expanded="${open}">
      ${icon(open ? 'down' : 'right')}
      <span class="mono strong trunc">${esc(w.name)}</span><span class="muted small">${esc(w.kind)}${w.stopped ? ' · stopped' : ` · ${w.replicas} replica${w.replicas === 1 ? '' : 's'}`}</span>
      <span class="grow"></span>${shown.length ? `<span class="muted small">${shown.length} finding${shown.length === 1 ? '' : 's'}</span>` : ''}${badge(String(w.score), tone)}
    </button>
    ${open ? `<ul class="rs-findings">${shown.length ? shown.map(findingItem).join('') : '<li class="rs-finding muted small">Nothing to report at this severity.</li>'}</ul>` : ''}</li>`;
}

function namespaceBlock(ns, ui, defaultOpen) {
  const key = `ns:${ns.name}`;
  const open = ui.open.has(key) ? true : ui.closed.has(key) ? false : defaultOpen;
  const tone = scoreTone(ns.score);
  const nsFindings = visibleFindings(ns.findings, ui.sev);
  const flagged = (ns.workloads || []).filter((w) => visibleFindings(w.findings, ui.sev).length).length;
  const lowestFirst = [...(ns.workloads || [])].sort((a, b) => a.score - b.score || a.name.localeCompare(b.name));
  const extra = [flagged ? `${flagged} with findings` : '', nsFindings.length ? `${nsFindings.length} for the namespace` : ''].filter(Boolean).join(' · ');
  return `<section class="card clip">
    <button class="rs-ns-head" data-act="rs-toggle" data-arg="${esc(key)}" data-default-open="${defaultOpen}" aria-expanded="${open}">
      ${icon(open ? 'down' : 'right')}<span class="mono strong">${esc(ns.name)}</span>
      <span class="muted small">${(ns.workloads || []).length} workload${(ns.workloads || []).length === 1 ? '' : 's'}${extra ? ` · ${esc(extra)}` : ''}</span>
      <span class="grow"></span>${badge(`${ns.score}`, tone)}
    </button>
    ${open && nsFindings.length ? `<div class="rs-ns-findings"><h4 class="rs-h4">Namespace-level findings</h4><ul class="rs-findings rs-findings-flat">${nsFindings.map(findingItem).join('')}</ul></div>` : ''}
    ${open ? `<ul class="rs-wls">${lowestFirst.length ? lowestFirst.map((w) => workloadBlock(ns, w, ui)).join('') : '<li class="card-empty"><span class="muted">No Deployments or StatefulSets here.</span></li>'}</ul>` : ''}
  </section>`;
}

export function readinessView(S, tab, ui) {
  return stateOf(tab, 'the readiness check', (d) => {
    const tone = scoreTone(d.score);
    const s = d.summary || {};
    const nothing = !s.workloads;
    const sev = SEVERITIES.map((v) => `<button class="chip" data-act="rs-sev" data-arg="${v}" aria-pressed="${ui.sev === v}">${v === 'high' ? 'High only' : v === 'medium' ? 'Medium and up' : 'All findings'}</button>`).join('');
    const headline = nothing ? 'Nothing to score in this scope'
      : s.high ? `${s.high} high-severity finding${s.high === 1 ? '' : 's'} to look at first`
        : s.medium ? `${s.medium} medium-severity finding${s.medium === 1 ? '' : 's'}` : 'No high or medium findings';
    const namespaces = [...(d.namespaces || [])].sort((a, b) => a.score - b.score || a.name.localeCompare(b.name));
    const open = namespaces.length <= 3;
    const cluster = visibleFindings(d.cluster, ui.sev);
    return `<div class="rs-stack">
      <section class="card rs-hero">
        ${gauge(d.score, tone, nothing)}
        <div class="grow">
          <div class="rs-hero-title">${esc(headline)}</div>
          ${nothing ? '<p class="muted rs-disclaimer">There are no Deployments or StatefulSets here, so there is no score. Pick another namespace, or all namespaces.</p>' : ''}
          <div class="rs-counts">${badge(`${s.high || 0} high`, s.high ? 'err' : 'neutral')}${badge(`${s.medium || 0} medium`, s.medium ? 'warn' : 'neutral')}${badge(`${s.low || 0} low`, 'neutral')}
            <span class="muted small">${s.workloads || 0} workload${s.workloads === 1 ? '' : 's'} in ${s.namespaces || 0} namespace${s.namespaces === 1 ? '' : 's'}${s.stopped ? ` · ${s.stopped} stopped` : ''}</span></div>
          <p class="muted small rs-disclaimer">${esc(d.disclaimer || 'The score counts risk signals found in the cluster objects. It does not prove that anything can be recovered: only a restore drill does.')}</p>
        </div>
      </section>
      ${notices(d.notices)}${d.truncated ? alertBox('warn', 'The list was cut short', 'There are more workloads than the check reads in one go. Pick a single namespace for the full picture.') : ''}
      <div class="rs-filter"><span class="muted small">Show</span>${sev}</div>
      ${cluster.length ? `<section class="card clip">${cardHead('Cluster', `${cluster.length} finding${cluster.length === 1 ? '' : 's'}`)}<ul class="rs-findings rs-findings-flat">${cluster.map(findingItem).join('')}</ul></section>` : ''}
      ${namespaces.length ? namespaces.map((ns) => namespaceBlock(ns, ui, open)).join('') : `<div class="card clip">${emptyState('No workloads to score', 'The check reads Deployments and StatefulSets. There are none in this scope.', 'Show all namespaces', 'ns-all')}</div>`}
      <div class="card clip">${bar(`k8scli dr check${S.ns === '_all' ? '' : ` -n ${S.ns}`}`)}</div>
    </div>`;
  });
}

/* ---------- backups ---------- */

const SOURCE_TITLE = {
  'velero-backup': 'Velero backups',
  'velero-schedule': 'Velero schedules',
  'volume-snapshot': 'Volume snapshots',
  'velero-storage-location': 'Backup storage locations',
};

function backupRows(items) {
  return items.map((i) => {
    const age = ageSeconds(i);
    const isLocation = i.source === 'velero-storage-location';
    const statusTone = i.ok ? 'ok' : 'err';
    const ageCell = isLocation ? '<span class="muted">—</span>' : age === null ? '<span class="muted">unknown</span>' : badge(humanSeconds(age), ageTone(age));
    const covers = (i.namespaces || []).length ? (i.namespaces.includes('*') ? 'all namespaces' : i.namespaces.join(', ')) : esc(i.namespace || '');
    return `<tr><td class="mono strong cell-name trunc">${esc(i.name)}</td><td class="muted">${(i.namespaces || []).length ? esc(covers) : covers}</td>
      <td>${badge(i.status || (i.ok ? 'ok' : 'not ok'), statusTone)}</td><td>${ageCell}</td><td class="muted small">${esc(i.detail || '')}</td></tr>`;
  }).join('');
}

export function backupsView(S, tab) {
  return stateOf(tab, 'the backups', (d) => {
    const items = d.items || [];
    if (!d.veleroInstalled && !d.snapshotsInstalled && !items.length) {
      return `<div class="rs-stack">${notices(d.notices)}<div class="card clip"><div class="empty">
        ${icon('shield', 'icon icon-xl muted')}<h3>No backup tooling found</h3>
        <p class="muted">This cluster has no Velero and no CSI volume snapshots, so there is nothing to restore from. Velero backs up Kubernetes objects and, with a plugin, volumes.</p>
        <button class="btn btn-primary" data-act="nav" data-arg="helm">${icon('plus')}Install Velero from Apps (Helm)</button></div></div></div>`;
    }
    const head = '<thead><tr><th scope="col">Name</th><th scope="col">Covers</th><th scope="col">Status</th><th scope="col">Age</th><th scope="col">Detail</th></tr></thead>';
    const groups = Object.keys(SOURCE_TITLE).map((source) => {
      const list = items.filter((i) => i.source === source)
        .sort((a, b) => (ageSeconds(a) ?? 1e12) - (ageSeconds(b) ?? 1e12));
      if (!list.length) return '';
      const bad = list.filter((i) => !i.ok).length;
      return `<section class="card clip">${cardHead(SOURCE_TITLE[source], `${list.length} item${list.length === 1 ? '' : 's'}${bad ? ` · ${bad} not ok` : ''}`)}
        <div class="scroll-x"><table class="tbl">${head}<tbody>${backupRows(list)}</tbody></table></div></section>`;
    }).join('');
    // Not .map(ageSeconds): map passes the index as the second argument, which ageSeconds reads as "now".
    const newest = items.filter((i) => (i.source === 'velero-backup' || i.source === 'volume-snapshot') && i.ok).map((i) => ageSeconds(i)).filter((x) => x !== null).sort((a, b) => a - b)[0];
    const hero = newest === undefined
      ? alertBox('warn', 'No successful backup yet', 'Velero or snapshots are installed, but nothing has completed. Until one does, you have nothing to restore.')
      : alertBox(ageTone(newest) === 'ok' ? 'ok' : 'warn', `Newest successful backup: ${humanSeconds(newest)} ago`, 'This age is the worst-case data loss if you restored it now (an estimate: nothing was restored to check).');
    return `<div class="rs-stack">${hero}${notices(d.notices)}${groups || okNote('Nothing listed.')}<div class="card clip">${bar('k8scli dr backups')}</div></div>`;
  });
}

/* ---------- plans ---------- */

const planPhrase = (plan, direction) => (plan && plan.confirmPhrase && plan.confirmPhrase[direction]) || '';

function planRows(list, openName) {
  return list.map((p) => `<tr${p.name === openName ? ' class="rs-row-open"' : ''}>
    <td class="mono strong">${esc(p.name)}</td><td class="mono muted">${esc(p.context || '')}</td>
    <td class="tnum">${count(p.apps)}</td><td class="tnum">${count(p.databases)}</td>
    <td class="muted">${esc((p.provider && p.provider.kind) || p.provider || '—')}</td>
    <td>${p.valid ? badge('Valid', 'ok') : badge(`${(p.problems || []).length || 1} problem${(p.problems || []).length === 1 ? '' : 's'}`, 'err')}${p.labOnly ? ' ' + badge('Lab only', 'neutral') : ''}</td>
    <td class="act"><div class="row g1 end">
      <button class="btn" data-act="rs-plan-open" data-arg="${esc(p.name)}">Open</button>
      <button class="btn btn-ghost" data-act="rs-plan-preview" data-arg="${esc(p.name)}"${p.valid ? '' : ' disabled'} title="${p.valid ? 'Preview a run of this plan' : 'Fix the plan first'}">Preview</button>
    </div></td></tr>`).join('');
}

function orderList(items) {
  return `<ol class="rs-order">${items.map((i) => (i.kind === 'phase'
    ? `<li class="rs-order-phase muted">${esc(i.name)}</li>`
    : `<li><span class="mono strong">${esc(i.name)}</span> <span class="muted small">${esc(i.kind === 'database' ? `database · ${i.detail || ''}` : 'app')} · ${esc(i.namespace)} · tier ${i.tier}</span></li>`)).join('')}</ol>`;
}

function planSummary(editor, listItem) {
  const plan = editor.detail && editor.detail.plan;
  if (!plan) return '';
  const order = planOrder(plan);
  const t = plan.targets || {};
  const phraseDown = planPhrase(listItem, 'down') || confirmPhraseFor(plan.context, 'down', plan.provider ? ['apps', 'databases', 'nodes', 'power'] : ['apps', 'databases']);
  const phraseUp = planPhrase(listItem, 'up') || confirmPhraseFor(plan.context, 'up', ['apps']);
  return `<section class="card clip">${cardHead('Plan summary', plan.name || '')}
    <div class="rs-summary">
      <dl class="rs-dl">
        <div><dt>Context</dt><dd class="mono">${esc(plan.context)}</dd></div>
        <div><dt>Standby context</dt><dd class="mono">${esc(plan.drContext || '—')}</dd></div>
        <div><dt>Default targets</dt><dd>RTO ${esc(t.rto || '—')} · RPO ${esc(t.rpo || '—')}</dd></div>
        <div><dt>Provider</dt><dd>${esc(plan.provider ? plan.provider.kind : 'none (no power step)')}</dd></div>
        <div><dt>Lab only</dt><dd>${plan.safety && plan.safety.labOnly ? 'Yes' : 'No'}</dd></div>
        <div><dt>To confirm a run down</dt><dd>${phraseDown ? `<span class="mono">${esc(phraseDown)}</span>` : 'nothing to type'}</dd></div>
        <div><dt>To confirm a run up</dt><dd>${phraseUp ? `<span class="mono">${esc(phraseUp)}</span>` : 'nothing to type'}</dd></div>
      </dl>
      <div class="rs-orders"><div><h4>Going down</h4>${orderList(order.down)}</div><div><h4>Coming up</h4>${orderList(order.up)}</div></div>
    </div></section>`;
}

export function plansView(S, ST) {
  const caps = ST.caps || {};
  const list = ST.plans || [];
  const ed = ST.editor;
  const exists = ed && list.some((p) => p.name === ed.name);
  const table = list.length
    ? `<div class="scroll-x"><table class="tbl"><thead><tr><th scope="col">Plan</th><th scope="col">Context</th><th scope="col">Apps</th><th scope="col">Databases</th><th scope="col">Provider</th><th scope="col">State</th><th class="act" scope="col"><span class="sr-only">Actions</span></th></tr></thead><tbody>${planRows(list, ed && ed.name)}</tbody></table></div>`
    : `<div class="empty"><h3>No plans yet</h3><p class="muted">A plan lists your apps and databases by tier, the objectives, and how machines are powered. Start from the example.</p>
        <button class="btn btn-primary" data-act="rs-plan-new">${icon('plus')}New plan</button></div>`;
  const problems = (ed && ed.problems) || [];
  const editor = ed ? `<section class="card clip">${cardHead(ed.isNew ? 'New plan' : `Edit ${ed.name}`, caps.planDir ? `Saved in ${caps.planDir}` : '')}
      <div class="rs-editor">
        ${ed.isNew ? `<label class="field-group"><span>File name</span><input id="rs-plan-name" class="field mono" value="${esc(ed.name)}" placeholder="my-system" autocomplete="off" spellcheck="false"><small class="muted">Lowercase letters, digits and dashes. Saved as <span class="mono">${esc(ed.name || 'name')}.yaml</span>.</small></label>` : ''}
        <label class="field-group"><span>Plan (YAML)</span><textarea id="rs-plan-yaml" class="field rs-yaml" rows="20" spellcheck="false" autocomplete="off">${esc(ed.yaml)}</textarea></label>
        ${problems.length ? `<ul class="rs-problems">${problems.map((p) => `<li>${esc(p)}</li>`).join('')}</ul>` : ''}
        ${ed.error ? alertBox('err', 'Could not save the plan', `<pre class="rs-err">${esc(ed.error)}</pre>`) : ''}
        ${ed.saved ? alertBox('ok', 'Plan saved', 'It is valid and ready to preview in Runs.') : ''}
        ${exists && ed.isNew ? `<label class="check"><input type="checkbox" id="rs-plan-overwrite"${ed.overwrite ? ' checked' : ''}> A plan with this name exists. Replace it</label>` : ''}
        <div class="row g2 wrap">
          <button class="btn btn-primary" data-act="rs-plan-save"${ed.saving || caps.readOnly ? ' disabled' : ''}${caps.readOnly ? ' title="The console is read-only"' : ''}>${ed.saving ? 'Saving…' : 'Validate and save'}</button>
          <button class="btn" data-act="rs-plan-example">Insert the example</button>
          <button class="btn btn-ghost" data-act="rs-plan-close">Close</button>
          ${caps.readOnly ? '<span class="muted small">Read-only mode: saving is blocked.</span>' : ''}
        </div>
      </div></section>` : '';
  const current = ed && list.find((p) => p.name === ed.name);
  return `<div class="rs-stack">
    <section class="card clip">${cardHead('Plans', 'Files in the plan folder')}
      <div class="toolbar"><button class="btn btn-primary" data-act="rs-plan-new">${icon('plus')}New plan</button><span class="grow"></span>
        <span class="muted small">Plans are checked as typed data: unknown fields are rejected.</span></div>${table}
      ${bar('k8scli dr plan validate <file>')}</section>
    ${editor}${ed && !ed.isNew ? planSummary(ed, current) : ''}
  </div>`;
}

/* ---------- runs ---------- */

function stepIcon(step, current) {
  if (current) return '<span class="rs-spin" aria-label="Running"></span>';
  const map = { done: ['check', 'tone-ok'], skipped: ['minus', 'tone-muted'], failed: ['xcircle', 'tone-err'], aborted: ['warn', 'tone-warn'], deferred: ['timer', 'tone-muted'] };
  const [name, cls] = map[step.status] || [null, ''];
  return name ? `<span class="${cls}">${icon(name)}</span>` : '<span class="rs-dot" aria-label="Waiting"></span>';
}

function previewStep(step) {
  const risk = step.risk || 'low';
  return `<li class="rs-step${step.status === 'deferred' ? ' rs-step-deferred' : ''}">
    <div class="rs-step-top">${badge(risk, RISK_TONE[risk] || 'neutral')}<span class="strong grow">${esc(step.title)}</span>
      ${step.status === 'skipped' ? badge('Already done', 'neutral') : ''}${step.status === 'deferred' ? badge('Computed later', 'neutral') : ''}</div>
    <div class="muted small">${esc(step.describe || '')}</div>
    ${step.equivalent ? `<div class="rs-fix"><code class="cli">${esc(step.equivalent)}</code><button class="btn btn-ghost btn-icon" data-act="copy" data-arg="${esc(step.equivalent)}" aria-label="Copy command">${icon('copy')}</button></div>` : ''}
  </li>`;
}

const BLOCKED = {
  dr_runs_disabled: 'Real runs are switched off. Start the console with --allow-dr-runs.',
  read_only: 'The console is read-only.',
  dr_power_disabled: 'Powering machines needs a plan marked labOnly, or --allow-dr-power.',
};

export function previewHTML(result) {
  if (!result) return '';
  const steps = result.steps || [];
  const sum = previewSummary(steps);
  const groups = groupByPhase(steps);
  const deferred = steps.some((s) => s.status === 'deferred');
  return `<div class="rs-preview">
    <div class="rs-preview-sum"><strong>${sum.total} step${sum.total === 1 ? '' : 's'}</strong>
      <span class="muted">${sum.change} would change · ${sum.done} already done${sum.deferred ? ` · ${sum.deferred} computed later` : ''}</span>
      ${sum.destructive ? badge(`${sum.destructive} destructive`, 'err') : ''}</div>
    ${deferred ? '<p class="muted small">A phase marked "computed later" cannot be read yet, usually because the cluster is off. It is worked out when the run reaches it.</p>' : ''}
    ${steps.length ? groups.map((g) => `<div class="rs-phase"><h4>${esc(PHASE_TITLE[g.phase] || g.phase)}</h4><ol class="rs-steps">${g.steps.map(previewStep).join('')}</ol></div>`).join('')
      : '<p class="muted">The plan has nothing to do for this selection.</p>'}
  </div>`;
}

function liveStep(step, current, now) {
  const took = elapsedSeconds(step.started, step.finished, now);
  const manual = step.status === 'failed' && /resume/i.test(step.err || '');
  return `<li class="rs-step rs-step-${esc(step.status)}${current ? ' rs-step-current' : ''}">
    <div class="rs-step-top">${stepIcon(step, current)}<span class="strong grow">${esc(step.title)}</span>
      ${badge(step.risk || 'low', RISK_TONE[step.risk] || 'neutral')}${took !== null && step.status !== 'pending' ? `<span class="muted small tnum">${esc(humanSeconds(took))}</span>` : ''}</div>
    <div class="muted small">${esc(step.describe || '')}${step.gate ? ` Gate: ${esc(step.gate)}${step.gateWaited ? `, waited ${esc(step.gateWaited)}` : ''}.` : ''}</div>
    ${step.err ? `<pre class="rs-err${manual ? ' rs-err-manual' : ''}">${esc(step.err)}</pre>` : ''}
  </li>`;
}

export function runDetailHTML(run, ST, now = Date.now()) {
  const d = run.data;
  if (!d) return `<section class="card clip">${cardHead('Run', run.id)}<div class="skel-body"><span class="skel w3"></span><span class="skel w3"></span></div></section>`;
  const live = run.live;
  const steps = d.steps || [];
  const currentIndex = live ? steps.findIndex((s) => s.status === 'pending') : -1;
  const done = steps.filter((s) => s.status === 'done' || s.status === 'skipped').length;
  const total = elapsedSeconds(d.started, live ? null : d.finished, now);
  const failed = steps.find((s) => s.status === 'failed');
  const canResume = !live && (d.status === 'failed' || d.status === 'aborted') && ST.caps && ST.caps.allowRuns && !ST.caps.readOnly;
  const phrase = confirmPhraseFor(d.context, d.direction, [...new Set(steps.map((s) => s.phase))]);
  return `<section class="card clip rs-run">${cardHead(`${d.direction === 'up' ? 'Start' : 'Stop'} run · ${d.plan || ''}`, run.id)}
    <div class="rs-run-head">
      ${badge(d.status, RUN_TONE[d.status] || 'neutral')}
      <span class="muted small">${done} of ${steps.length} steps</span>
      <span class="muted small">elapsed <span id="rs-elapsed" class="tnum" data-start="${esc(d.started || '')}" data-live="${live ? '1' : '0'}">${esc(humanSeconds(total))}</span></span>
      <span class="grow"></span>
      ${live ? `<button class="btn" data-act="rs-abort" data-arg="${esc(run.id)}">${icon('x')}Abort</button>` : ''}
    </div>
    ${live ? `<div class="alert alert-info rs-note">${icon('info', 'icon icon-lg')}<div>Closing this tab does not stop the run. Stopping the console does: the run is left unfinished and can be resumed.</div></div>` : ''}
    ${run.error ? alertBox('warn', 'Could not read the run', esc(run.error)) : ''}
    ${steps.length ? `<ol class="rs-steps rs-live">${steps.map((s, i) => liveStep(s, i === currentIndex, now)).join('')}</ol>` : (live ? '<div class="card-empty"><span class="rs-spin" aria-hidden="true"></span><span class="muted">Waiting for the first step…</span></div>' : '')}
    ${d.status === 'succeeded' ? alertBox('ok', 'The run finished', `All ${steps.length} steps are done or were already satisfied.`) : ''}
    ${d.status === 'failed' && failed ? alertBox('err', `Stopped at: ${failed.title}`, 'Fix the cause, then resume. Steps that are done are skipped. The text above is the exact error, including any instructions for you to follow.') : ''}
    ${d.status === 'aborted' ? alertBox('warn', 'The run was aborted', 'Nothing is lost: the ledger is consistent. Resume continues from the first step that is not done.') : ''}
    ${canResume ? `<div class="rs-resume">
      ${phrase ? `<label class="field-group"><span>Type <span class="mono">${esc(phrase)}</span> to resume</span><input id="rs-resume-phrase" class="field mono" value="${esc(ST.run.phrase || '')}" autocomplete="off" spellcheck="false"></label>` : ''}
      <button class="btn btn-primary" id="rs-resume" data-act="rs-resume" data-arg="${esc(run.id)}"${phrase && ST.run.phrase !== phrase ? ' disabled' : ''}>${icon('refresh')}Resume</button></div>` : ''}
    ${bar(`k8scli dr status ${run.id}`)}
  </section>`;
}

function scopeChecks(f, caps, plan) {
  return SCOPE_PARTS.map((part) => {
    const unavailable = part.key === 'power' && plan && !plan.provider;
    const note = part.key === 'power' && plan && !plan.provider ? 'The plan has no provider.' : part.key === 'power' && caps && !caps.allowPower ? 'You can preview it. A real run needs the extra opt-in.' : part.help;
    return `<label class="check rs-scope${part.destructive ? ' rs-scope-danger' : ''}"><input type="checkbox" id="rs-scope-${part.key}" data-scope="${part.key}"${f.scope[part.key] ? ' checked' : ''}${unavailable ? ' disabled' : ''}>
      <span><span class="strong">${esc(part.label)}</span>${part.destructive ? ' ' + badge('destructive', 'err') : ''}<br><span class="muted small">${esc(note)}</span></span></label>`;
  }).join('');
}

function runHistory(ST) {
  const L = ST.runsList;
  if (!L || L.status === 'loading' || L.status === 'idle') return `<section class="card clip">${cardHead('History')}<div class="skel-body"><span class="skel w3"></span></div></section>`;
  if (L.status === 'error') return `<section class="card clip">${cardHead('History')}<div class="card-empty"><span class="muted">${esc(L.error.message)}</span></div></section>`;
  const list = L.data || [];
  const body = list.length
    ? `<div class="scroll-x"><table class="tbl"><thead><tr><th scope="col">Run</th><th scope="col">Plan</th><th scope="col">Direction</th><th scope="col">Status</th><th scope="col">Started</th><th scope="col">Took</th><th class="act" scope="col"><span class="sr-only">Actions</span></th></tr></thead><tbody>${list.map((r) => `<tr>
        <td class="mono small">${esc(r.id)}</td><td class="mono">${esc(r.plan || '')}</td><td>${esc(r.direction || '')}</td>
        <td>${badge(r.status, RUN_TONE[r.status] || 'neutral')}${r.live ? ' ' + badge('live', 'accent') : ''}${r.failed ? `<div class="muted small">stopped at ${esc(r.failed)}</div>` : ''}</td><td class="muted small">${esc(when(r.started))}</td>
        <td class="muted small tnum">${esc(humanSeconds(elapsedSeconds(r.started, r.finished)))}</td>
        <td class="act"><button class="btn" data-act="rs-run-open" data-arg="${esc(r.id)}">Open</button></td></tr>`).join('')}</tbody></table></div>`
    : '<div class="card-empty"><span class="muted">No runs yet.</span></div>';
  return `<section class="card clip">${cardHead('History', 'Newest first')}${body}</section>`;
}

export function runsView(S, ST, now) {
  const caps = ST.caps || {};
  const f = ST.runForm;
  const plans = (ST.plans || []);
  const plan = plans.find((p) => p.name === f.plan);
  const phases = scopePhases(f.scope);
  const needsPower = phases.includes('power');
  const previewOk = f.preview && f.preview.key === ST.runKey();
  const pv = previewOk ? f.preview.data : null;
  // The server's own answer wins: it knows the phrase, and whether a gate would stop the run.
  const required = pv && typeof pv.confirmPhrase === 'string' ? pv.confirmPhrase : (plan ? confirmPhraseFor(plan.context, f.direction, phases) : '');
  const clientBlocked = caps.readOnly ? 'read_only' : !caps.allowRuns ? 'dr_runs_disabled' : (needsPower && plan && !plan.labOnly && !caps.allowPower ? 'dr_power_disabled' : '');
  const blocked = pv && pv.blocked !== undefined ? pv.blocked : clientBlocked;
  const canRun = !blocked && plan && plan.valid && previewOk && !f.starting && (!required || f.phrase === required);
  const planOptions = plans.length
    ? plans.map((p) => option(p.name, `${p.name}${p.valid ? '' : ' (invalid)'}`, p.name === f.plan, !p.valid)).join('')
    : '<option value="">No plans</option>';
  const form = `<section class="card clip">${cardHead('Run a plan', 'Always preview first')}
    <div class="rs-form">
      ${capsNote(caps, { power: needsPower, plan })}
      <div class="fields">
        <label class="field-group"><span>Plan</span><select id="rs-plan" class="field">${planOptions}</select></label>
        <div class="field-group"><span>Direction</span><div class="rs-seg" role="group" aria-label="Direction">
          <button class="rs-seg-btn" data-act="rs-dir" data-arg="down" aria-pressed="${f.direction === 'down'}">Stop (down)</button>
          <button class="rs-seg-btn" data-act="rs-dir" data-arg="up" aria-pressed="${f.direction === 'up'}">Start (up)</button></div></div>
      </div>
      <fieldset class="rs-scopes"><legend class="muted small">What to include</legend>${scopeChecks(f, caps, plan)}</fieldset>
      ${f.error ? alertBox('err', 'Could not preview', esc(f.error)) : ''}
      <div class="row g2 wrap"><button class="btn btn-primary" data-act="rs-preview"${!plan || !plan.valid || f.previewing || !phases.length ? ' disabled' : ''}>${f.previewing ? 'Reading the plan…' : 'Preview the plan'}</button>
        ${!plans.length ? '<button class="btn" data-act="rs-tab" data-arg="plans">Create a plan</button>' : ''}</div>
      ${f.preview ? `<div class="rs-preview-wrap">${previewHTML(f.preview.data)}</div>` : ''}
      ${f.preview ? `<div class="rs-go">
        ${!previewOk ? alertBox('warn', 'The selection changed', 'Preview again before you run. A run only starts for the plan you just read.') : ''}
        ${required ? `<label class="field-group"><span>Type <span class="mono">${esc(required)}</span> to confirm</span><input id="rs-phrase" class="field mono" value="${esc(f.phrase)}" autocomplete="off" spellcheck="false"></label>` : ''}
        ${f.runError ? alertBox('err', 'The run did not start', esc(f.runError)) : ''}
        <div class="row g2 wrap"><button class="btn btn-primary rs-danger-btn" id="rs-run" data-act="rs-run"${canRun ? '' : ' disabled'}>${f.starting ? 'Starting…' : `${f.direction === 'down' ? 'Stop' : 'Start'} for real`}</button>
          <span class="muted small">${esc(caps.readOnly ? BLOCKED.read_only : (BLOCKED[blocked] || ''))}</span></div></div>` : ''}
    </div>
    ${bar(`k8scli dr ${f.direction} --plan ${plan ? `${plan.name}.yaml` : '<file>'} --dry-run`)}
  </section>`;
  const live = ST.run ? runDetailHTML(ST.run, ST, now) : '';
  return `<div class="rs-stack">${live}${form}${runHistory(ST)}</div>`;
}

/* ---------- drills and reports ---------- */

function objectiveCell(o) {
  if (!o) return '<span class="muted">—</span>';
  return `<span class="strong tnum">${esc(o.human || humanSeconds(o.seconds))}</span> ${o.kind ? badge(o.kind, KIND_TONE[o.kind] || 'neutral') : ''}`;
}

function verdictBadge(v) {
  const x = VERDICT[v] || VERDICT.unknown;
  return badge(x.label, x.tone);
}

export function trendChart(reports) {
  const points = rtoTrend(reports);
  if (points.length < 2) return '<div class="card-empty"><span class="muted">The trend needs at least two drills.</span></div>';
  const W = 600;
  const H = 130;
  const max = Math.max(...points.map((p) => Math.max(p.seconds, p.target || 0)), 1);
  const gap = 8;
  const bw = (W - gap * (points.length + 1)) / points.length;
  const bars = points.map((p, i) => {
    const h = Math.max(2, (p.seconds / max) * (H - 34));
    const x = gap + i * (bw + gap);
    const tone = p.recovered ? (p.verdict === 'fail' ? 'err' : p.verdict === 'pass' ? 'ok' : 'neutral') : 'err';
    const target = p.target ? `<line class="rs-target" x1="${x.toFixed(1)}" x2="${(x + bw).toFixed(1)}" y1="${(H - 18 - (p.target / max) * (H - 34)).toFixed(1)}" y2="${(H - 18 - (p.target / max) * (H - 34)).toFixed(1)}"/>` : '';
    return `<g><rect class="rs-bar rs-bar-${tone}" x="${x.toFixed(1)}" y="${(H - 18 - h).toFixed(1)}" width="${bw.toFixed(1)}" height="${h.toFixed(1)}" rx="3"><title>${esc(humanSeconds(p.seconds))}</title></rect>${target}
      <text class="rs-axis" x="${(x + bw / 2).toFixed(1)}" y="${H - 4}" text-anchor="middle">${esc(humanSeconds(p.seconds))}</text></g>`;
  }).join('');
  return `<svg class="rs-trend" viewBox="0 0 ${W} ${H}" role="img" aria-label="RTO of the last ${points.length} drills">${bars}</svg>`;
}

export function reportHTML(rep) {
  const o = OUTCOME[rep.outcome] || { tone: 'neutral', label: rep.outcome || 'Unknown' };
  const stages = rep.stages || [];
  const total = Math.max(1, ...stages.map((s) => s.seconds || 0), (rep.rto && rep.rto.seconds) || 0);
  const W = 600;
  let prev = 0;
  const segs = stages.map((s, i) => {
    const x = (prev / total) * W;
    const w = Math.max(1, (((s.seconds || 0) - prev) / total) * W);
    prev = s.seconds || 0;
    return `<rect class="rs-tl rs-tl-${i % 5}" x="${x.toFixed(1)}" y="0" width="${w.toFixed(1)}" height="20" rx="2"><title>${esc(s.title)}: ${esc(s.human || humanSeconds(s.seconds))}</title></rect>`;
  }).join('');
  const legend = stages.map((s, i) => `<li><span class="rs-key rs-tl-${i % 5}"></span><span class="grow">${esc(s.title)}</span><span class="muted tnum">${esc(s.human || humanSeconds(s.seconds))}</span></li>`).join('');
  const apps = (rep.apps || []).map((a) => `<tr><td class="mono strong">${esc(a.name)}</td><td class="muted">${esc(a.namespace)}</td><td>${a.recovered ? badge('Healthy again', 'ok') : badge('Never healthy', 'err')}</td>
    <td>${objectiveCell(a.rto)}</td><td class="muted small">${esc((a.rto && a.rto.target) || 'no target')}</td><td>${objectiveCell(a.rpo)}</td><td>${verdictBadge(a.verdict)}</td></tr>`).join('');
  const dbs = (rep.databases || []).map((d) => `<tr><td class="mono strong">${esc(d.name)}</td><td class="muted">${esc(d.namespace)} · ${esc(d.kind)}</td><td>${objectiveCell(d.rto)}</td><td>${objectiveCell(d.rpo)}</td><td>${verdictBadge(d.verdict)}</td></tr>`).join('');
  const figures = (rep.rpoFigures || []).map((g) => `<tr><td class="mono strong">${esc(g.subject)}</td><td class="muted">${esc(g.namespace || '')}</td><td class="tnum">${esc(g.human || humanSeconds(g.seconds))}</td><td>${badge(g.kind, KIND_TONE[g.kind] || 'neutral')}</td><td class="muted small">${esc(g.source || '')}${g.note ? ` · ${esc(g.note)}` : ''}</td></tr>`).join('');
  const list = (items) => `<ul class="notices">${items.map((x) => `<li>${esc(x)}</li>`).join('')}</ul>`;
  return `<section class="card clip rs-report">${cardHead(`Drill report · ${rep.plan || ''}`, rep.id)}
    <div class="rs-report-head">${badge(o.label, o.tone)}${verdictBadge(rep.verdict)}${rep.mode ? badge(rep.mode, 'neutral') : ''}
      <span class="muted small">${esc(rep.context || '')} · ${esc(when(rep.started))}${rep.actor ? ` · by ${esc(rep.actor)}` : ''}</span><span class="grow"></span>
      <a class="btn" href="/api/dr/report?id=${encodeURIComponent(rep.id)}&amp;format=md" download>${icon('download2')}Download Markdown</a></div>
    <div class="kpis rs-kpis">
      <div class="card kpi"><div class="muted small">Time to recover (RTO)</div><div class="kpi-value tnum">${esc((rep.rto && rep.rto.human) || humanSeconds(rep.rto && rep.rto.seconds))}</div>
        <div class="small kpi-sub">${rep.rto && rep.rto.kind ? badge(rep.rto.kind, KIND_TONE[rep.rto.kind] || 'neutral') : ''} ${esc(rep.rto && rep.rto.target ? `target ${rep.rto.target}` : 'no target')} ${rep.rto ? verdictBadge(rep.rto.verdict) : ''}</div></div>
      <div class="card kpi"><div class="muted small">Data loss (RPO)</div><div class="kpi-value tnum">${esc(rep.rpo ? (rep.rpo.human || humanSeconds(rep.rpo.seconds)) : 'unknown')}</div>
        <div class="small kpi-sub">${rep.rpo && rep.rpo.kind ? badge(rep.rpo.kind, KIND_TONE[rep.rpo.kind] || 'neutral') : ''} ${esc(rep.rpo && rep.rpo.target ? `target ${rep.rpo.target}` : 'no target')} ${rep.rpo ? verdictBadge(rep.rpo.verdict) : ''}</div></div>
      <div class="card kpi"><div class="muted small">State afterwards</div><div class="kpi-value">${rep.stateRestored ? 'Restored' : 'Different'}</div><div class="small kpi-sub muted">replica counts compared with the baseline</div></div>
    </div>
    <p class="muted small rs-honest">"Measured" figures come from clocks and data seen during the drill. "Estimated" figures come from metadata, such as the age of a backup: nothing was restored to check them.</p>
    ${stages.length ? `<div class="rs-block"><h4>Timeline</h4><svg class="rs-timeline" viewBox="0 0 ${W} 20" preserveAspectRatio="none" role="img" aria-label="Recovery timeline">${segs}</svg><ul class="rs-legend">${legend}</ul></div>` : ''}
    ${apps ? `<div class="rs-block"><h4>Applications</h4><div class="scroll-x"><table class="tbl"><thead><tr><th scope="col">App</th><th scope="col">Namespace</th><th scope="col">Result</th><th scope="col">RTO</th><th scope="col">Target</th><th scope="col">RPO</th><th scope="col">Verdict</th></tr></thead><tbody>${apps}</tbody></table></div></div>` : ''}
    ${dbs ? `<div class="rs-block"><h4>Databases</h4><div class="scroll-x"><table class="tbl"><thead><tr><th scope="col">Database</th><th scope="col">Where</th><th scope="col">RTO</th><th scope="col">RPO</th><th scope="col">Verdict</th></tr></thead><tbody>${dbs}</tbody></table></div></div>` : ''}
    ${figures ? `<div class="rs-block"><h4>Where the RPO figures come from</h4><div class="scroll-x"><table class="tbl"><thead><tr><th scope="col">Subject</th><th scope="col">Namespace</th><th scope="col">Figure</th><th scope="col">Kind</th><th scope="col">Source</th></tr></thead><tbody>${figures}</tbody></table></div></div>` : ''}
    ${(rep.failures || []).length ? alertBox('err', 'What went wrong', list(rep.failures)) : ''}${notices(rep.notices)}
    ${bar(`k8scli dr report ${rep.id}`)}
  </section>`;
}

const EVENT_LABEL = { 'run-start': 'Run started', 'run-done': 'Run finished', 'run-fail': 'Run failed', 'run-abort': 'Run aborted', 'step-start': 'Step started', 'step-done': 'Step done', 'step-skip': 'Step skipped', 'step-fail': 'Step failed', 'gate-wait': 'Waiting on a gate', 'gate-ok': 'Gate passed', 'gate-fail': 'Gate failed', 'step-manual': 'Waiting for you' };

export function drillProgressHTML(active, now = Date.now()) {
  const d = active.data;
  const live = active.live;
  const events = (d && d.events) || [];
  const recent = events.slice(-14).reverse();
  const took = d ? elapsedSeconds(d.started, null, now) : null;
  const statusTone = d ? (RUN_TONE[d.status] || 'neutral') : 'accent';
  return `<section class="card clip rs-run">${cardHead(`Drill · ${(d && d.plan) || active.plan || ''}`, active.id)}
    <div class="rs-run-head">${badge(d ? d.status : 'starting', statusTone)}
      <span class="muted small">elapsed <span id="rs-elapsed" class="tnum" data-start="${esc((d && d.started) || active.startedAt || '')}" data-live="${live ? '1' : '0'}">${esc(humanSeconds(took))}</span></span>
      ${d && d.downRun ? `<button class="btn btn-ghost" data-act="rs-run-open" data-arg="${esc(d.downRun)}">Down run</button>` : ''}${d && d.upRun ? `<button class="btn btn-ghost" data-act="rs-run-open" data-arg="${esc(d.upRun)}">Up run</button>` : ''}</div>
    ${live ? `<div class="alert alert-info rs-note">${icon('info', 'icon icon-lg')}<div>The plan is going down, then up. Applications are unavailable until the drill ends. The report opens here when it is written.</div></div>` : ''}
    ${active.error ? alertBox('warn', 'Could not read the drill', esc(active.error)) : ''}
    ${d && d.error ? alertBox('err', 'The drill stopped', `<pre class="rs-err">${esc(d.error)}</pre>`) : ''}
    ${recent.length ? `<ol class="rs-steps rs-live">${recent.map((e) => `<li class="rs-step"><div class="rs-step-top">${badge(EVENT_LABEL[e.kind] || e.kind, /fail|abort/.test(e.kind) ? 'err' : /done|ok/.test(e.kind) ? 'ok' : 'neutral')}<span class="grow">${esc(e.step || e.msg || '')}</span><span class="muted small tnum">${esc(when(e.time))}</span></div>${e.err ? `<pre class="rs-err">${esc(e.err)}</pre>` : ''}</li>`).join('')}</ol>`
    : (live ? '<div class="card-empty"><span class="rs-spin" aria-hidden="true"></span><span class="muted">Waiting for the first event…</span></div>' : '')}
  </section>`;
}

export function drillsView(S, ST, now) {
  const caps = ST.caps || {};
  const d = ST.drill;
  const plans = ST.plans || [];
  const plan = plans.find((p) => p.name === d.plan);
  const phases = scopePhases({ apps: true, databases: true, nodes: d.full, power: d.full });
  const previewOk = d.preview && d.preview.key === ST.drillKey();
  const required = previewOk && d.preview.down && typeof d.preview.down.confirmPhrase === 'string' ? d.preview.down.confirmPhrase : (plan ? confirmPhraseFor(plan.context, 'down', phases) : '');
  const fullBlocked = plan && !plan.labOnly;
  const clientBlocked = caps.readOnly ? 'read_only' : !caps.allowRuns ? 'dr_runs_disabled' : '';
  const blocked = previewOk && d.preview.down && d.preview.down.blocked !== undefined ? d.preview.down.blocked : clientBlocked;
  const canStart = !blocked && plan && plan.valid && plan.labOnly && previewOk && !d.starting && (!required || d.phrase === required);
  // The console only drills lab plans: a drill stops things on purpose, and a production drill is a terminal decision.
  const planOptions = plans.length ? plans.map((p) => option(p.name, `${p.name}${p.valid ? '' : ' (invalid)'}${p.labOnly ? ' · lab' : ' · terminal only'}`, p.name === d.plan, !p.valid || !p.labOnly)).join('') : '<option value="">No plans</option>';
  const reports = ST.reports;
  const list = (reports && reports.data) || [];
  const rows = list.map((r) => `<tr><td class="muted small">${esc(when(r.started))}</td><td class="mono strong">${esc(r.plan || '')}</td><td>${esc(r.mode || '')}</td>
    <td>${badge((OUTCOME[r.outcome] || {}).label || r.outcome || '—', (OUTCOME[r.outcome] || {}).tone || 'neutral')}</td>
    <td>${objectiveCell(r.rto)}</td><td>${r.rto ? verdictBadge(r.rto.verdict || r.verdict) : verdictBadge(r.verdict)}</td>
    <td class="act"><button class="btn" data-act="rs-report-open" data-arg="${esc(r.id)}">Open</button></td></tr>`).join('');
  const reportsCard = !reports || reports.status === 'idle' || reports.status === 'loading'
    ? `<section class="card clip">${cardHead('Reports')}<div class="skel-body"><span class="skel w3"></span><span class="skel w3"></span></div></section>`
    : reports.status === 'error' ? `<section class="card clip">${cardHead('Reports')}${errorState(reports.error, 'the reports')}</section>`
      : `<section class="card clip">${cardHead('Reports', 'Newest first')}${list.length
        ? `<div class="scroll-x"><table class="tbl"><thead><tr><th scope="col">When</th><th scope="col">Plan</th><th scope="col">Mode</th><th scope="col">Outcome</th><th scope="col">RTO</th><th scope="col">Verdict</th><th class="act" scope="col"><span class="sr-only">Actions</span></th></tr></thead><tbody>${rows}</tbody></table></div>`
        : '<div class="empty"><h3>No drills yet</h3><p class="muted">A drill stops and starts a plan and measures how long recovery takes. Run one in a lab first.</p></div>'}</section>`;
  const trend = list.length ? `<section class="card clip">${cardHead('RTO trend', 'Overall time to recover, last drills. The line marks the target.')}<div class="rs-trend-wrap">${trendChart(list)}</div></section>` : '';
  let active = '';
  if (d.active) active = drillProgressHTML(d.active, now);
  const reportCard = ST.report && ST.report.status === 'loading' ? `<section class="card clip">${cardHead('Report')}<div class="skel-body"><span class="skel w3"></span><span class="skel w3"></span></div></section>`
    : ST.report && ST.report.status === 'error' ? `<section class="card clip">${cardHead('Report')}${errorState(ST.report.error, 'the report')}</section>`
      : ST.report && ST.report.data ? reportHTML(ST.report.data) : '';
  return `<div class="rs-stack">
    ${active}
    <section class="card clip">${cardHead('Run a drill', 'Stop, start and measure')}
      <div class="rs-form">
        ${capsNote(caps)}
        <p class="muted small">A drill stops the plan, starts it again, waits for every health gate and writes a report. Run it in a lab: while it runs the plan's applications are down.</p>
        <div class="fields"><label class="field-group"><span>Plan</span><select id="rs-drill-plan" class="field">${planOptions}</select></label></div>
        <label class="check rs-scope rs-scope-danger"><input type="checkbox" id="rs-drill-full"${d.full ? ' checked' : ''}${fullBlocked || !plan ? ' disabled' : ''}>
          <span><span class="strong">Full drill: also nodes and power</span> ${badge('lab only', 'err')}<br><span class="muted small">${plan && !plan.labOnly ? 'This plan is not marked labOnly, so it cannot be drilled from the console.' : 'Cordons and drains nodes and powers the machines off and on. Use it only on a lab cluster.'}</span></span></label>
        ${plan && plan.labOnly ? '<p class="muted small">This plan is marked as a lab, so the production gate does not apply.</p>' : ''}
        ${d.error ? alertBox('err', 'Could not preview the drill', esc(d.error)) : ''}
        <div class="row g2 wrap"><button class="btn btn-primary" data-act="rs-drill-preview"${!plan || !plan.valid || d.previewing ? ' disabled' : ''}>${d.previewing ? 'Reading the plan…' : 'Preview the drill'}</button></div>
        ${d.preview ? `<div class="rs-preview-wrap"><h4 class="rs-h4">Going down</h4>${previewHTML(d.preview.down)}<h4 class="rs-h4">Coming back up</h4>${previewHTML(d.preview.up)}</div>
          <div class="rs-go">${!previewOk ? alertBox('warn', 'The selection changed', 'Preview again before you start.') : ''}
            ${required ? `<label class="field-group"><span>Type <span class="mono">${esc(required)}</span> to confirm</span><input id="rs-drill-phrase" class="field mono" value="${esc(d.phrase)}" autocomplete="off" spellcheck="false"></label>` : ''}
            ${d.startError ? alertBox('err', 'The drill did not start', esc(d.startError)) : ''}
            <div class="row g2 wrap"><button class="btn btn-primary rs-danger-btn" id="rs-drill-go" data-act="rs-drill-go"${canStart ? '' : ' disabled'}>${d.starting ? 'Starting…' : 'Start the drill'}</button>
              <span class="muted small">${esc(caps.readOnly ? BLOCKED.read_only : (BLOCKED[blocked] || ''))}</span></div></div>` : ''}
      </div>
      ${bar(`k8scli dr drill --plan ${plan ? `${plan.name}.yaml` : '<file>'}`)}
    </section>
    ${reportCard}${trend}${reportsCard}</div>`;
}

/* ---------- tools ---------- */

function diffItems(items) {
  return `<ul class="rs-diff">${items.map((i) => `<li>${badge(sevLabel(i.severity), sevTone(i.severity))}<span class="mono small">${esc(i.kind)} ${esc(i.name)}</span><span class="grow">${esc(i.change)}${i.detail ? ` · ${esc(i.detail)}` : ''}</span></li>`).join('')}</ul>`;
}

function diffResult(t) {
  if (!t.diff) return '';
  if (t.diff.status === 'loading') return '<div class="skel-body"><span class="skel w3"></span><span class="skel w3"></span></div>';
  if (t.diff.status === 'error') return errorState(t.diff.error, 'the comparison');
  const d = t.diff.data;
  const s = d.summary || {};
  const nss = (d.namespaces || []).filter((n) => n.missingInStandby || (n.items || []).length);
  return `<div class="rs-diff-body">
    <div class="rs-counts">${s.identical ? badge('Identical', 'ok') : badge(`${s.high || 0} high`, s.high ? 'err' : 'neutral')}${s.identical ? '' : badge(`${s.medium || 0} medium`, s.medium ? 'warn' : 'neutral')}${s.identical ? '' : badge(`${s.low || 0} low`, 'neutral')}
      <span class="muted small">${esc(d.primary)} against ${esc(d.standby)}</span></div>
    ${notices(d.notices)}${d.truncated ? alertBox('warn', 'The list was cut short', 'Compare one namespace for the full picture.') : ''}
    ${(d.cluster || []).length ? `<h4 class="rs-h4">Cluster</h4>${diffItems(d.cluster)}` : ''}
    ${nss.map((n) => `<h4 class="rs-h4 mono">${esc(n.name)}${n.missingInStandby ? ' ' + badge('missing in standby', 'err') : ''}</h4>${diffItems(n.items || [])}`).join('')}
    ${s.identical ? '<p class="muted">Nothing differs in what was compared: images, replicas, ConfigMap keys, Services, Ingress hosts and CRDs.</p>' : ''}</div>`;
}

export function toolsView(S, ST) {
  const t = ST.tools;
  const nsList = t.namespaces || [];
  const exportNs = t.exportNs || (S.ns !== '_all' ? S.ns : (nsList[0] || ''));
  const others = (S.app.contexts || []).filter((c) => c.name !== S.ctx);
  const nsOptions = (selected, all) => (all ? option('_all', 'All namespaces', selected === '_all') : '') + nsList.map((n) => option(n, n, n === selected)).join('');
  return `<div class="rs-stack">
    <section class="card clip">${cardHead('Export a namespace', 'Clean manifests you can keep in Git')}
      <div class="rs-form">
        <p class="muted small">Downloads a zip of Deployments, StatefulSets, Services, Ingresses, ConfigMaps, PVC definitions, CronJobs and more, with cluster-set fields removed. <strong>Secrets are left out</strong>: only their names are listed, never a value.</p>
        <div class="row g2 wrap"><label class="row g2 muted small">Namespace<select id="rs-export-ns" class="field mono">${nsOptions(exportNs, false) || option('', 'No namespaces', true)}</select></label>
          ${exportNs ? `<a class="btn btn-primary" id="rs-export" href="${esc(`/api/dr/export?context=${encodeURIComponent(S.ctx)}&namespace=${encodeURIComponent(exportNs)}`)}" download>${icon('download2')}Download zip</a>` : '<button class="btn btn-primary" disabled>Download zip</button>'}</div>
      </div>${bar(`k8scli dr export -n ${exportNs || '<namespace>'} -o ./export`)}</section>
    <section class="card clip">${cardHead('Compare with another cluster', 'Drift between the primary and a standby')}
      <div class="rs-form">
        ${others.length ? `<div class="row g2 wrap">
          <label class="row g2 muted small">Standby<select id="rs-diff-to" class="field mono">${others.map((c) => option(c.name, c.name, c.name === (t.diffTo || others[0].name))).join('')}</select></label>
          <label class="row g2 muted small">Namespace<select id="rs-diff-ns" class="field mono">${nsOptions(t.diffNs || '_all', true)}</select></label>
          <button class="btn btn-primary" data-act="rs-diff">Compare</button></div>`
    : '<p class="muted">Only one context is in your kubeconfig, so there is nothing to compare with. Add the standby cluster first.</p>'}
        ${diffResult(t)}
      </div>${bar(`k8scli dr diff ${S.ctx} ${t.diffTo || (others[0] && others[0].name) || '<standby>'} -n ${t.diffNs && t.diffNs !== '_all' ? t.diffNs : '<namespace>'}`)}</section>
  </div>`;
}

