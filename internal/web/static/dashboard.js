// Dashboard: the home page. It answers "is my cluster OK?" first, then shows
// where to look: namespaces, pods that need attention, restarts, warnings, usage.

import { api } from './api.js';
import { esc, icon, badge, TONE, cliFor } from './ui.js';
import { registerPage } from './pages.js';
import { pageHead, refreshControls, errorState, kpiCard } from './views.js';
import { openLogs } from './panels.js';

const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;
const ago = (age) => (age ? `${age} ago` : '');

/* ---------- helpers other pages reuse ---------- */

// Same bar as views.js' cliBar, for the commands that only exist in kubectl.
// cliFor() prepends "k8scli use <ctx> &&" when the console looks at another context,
// which also points kubectl at the right cluster.
export function kubectlBar(S, command, extra = '') {
  const full = cliFor(S, command);
  return `<div class="cli-bar">
    <span class="muted small nowrap">kubectl equivalent</span>
    <code class="cli grow">${esc(full)}</code>
    ${extra ? `<span class="muted small tnum hide-sm">${esc(extra)}</span>` : ''}
    <button class="btn btn-ghost btn-icon" data-act="copy" data-arg="${esc(full)}" aria-label="Copy command">${icon('copy')}</button>
  </div>`;
}

export function cardHead(title, note = '') {
  return `<div class="card-head"><h3>${esc(title)}</h3>${note ? `<span class="muted small">${esc(note)}</span>` : ''}</div>`;
}

export function okNote(text) {
  return `<div class="card-empty">${icon('okcircle', 'icon icon-lg tone-ok')}<span>${esc(text)}</span></div>`;
}

/* ---------- pieces ---------- */

function hero(d) {
  const t = d.totals || {};
  const nodes = d.nodes || { ready: 0, total: 0 };
  const downNodes = nodes.total - nodes.ready;
  const hasError = downNodes > 0 || (d.attentionPods || []).some((p) => p.health === 'error');
  const parts = [];
  if (t.attention) parts.push(`${plural(t.attention, 'pod needs', 'pods need')} attention`);
  if (t.deploymentsNotReady) parts.push(`${plural(t.deploymentsNotReady, 'deployment is', 'deployments are')} not fully ready`);
  if (downNodes > 0) parts.push(`${plural(downNodes, 'node is', 'nodes are')} not ready`);

  const healthy = parts.length === 0;
  const tone = healthy ? 'ok' : (hasError ? 'err' : 'warn');
  const title = healthy ? 'Everything looks healthy' : parts.join(' · ');
  const detail = `${t.running ?? 0} of ${t.pods ?? 0} pods running across ${plural(t.namespaces ?? 0, 'namespace', 'namespaces')} · ${nodes.ready} of ${nodes.total} nodes ready`;

  return `<div class="status-hero status-${tone}" role="status">
    ${icon(healthy ? 'okcircle' : 'warn', 'icon status-icon')}
    <div class="grow"><div class="status-title">${esc(title)}</div><div class="status-sub">${esc(detail)}</div></div>
    ${t.attention ? `<button class="btn" data-act="dash-attention">Show these pods</button>` : ''}
  </div>`;
}

function kpiRow(d) {
  const t = d.totals || {};
  const nodes = d.nodes || { ready: 0, total: 0 };
  return `<div class="kpis">
    ${kpiCard('Kubernetes', esc(d.version || 'unknown'), [d.platform, `${nodes.ready}/${nodes.total} nodes ready`].filter(Boolean).join(' · '), nodes.ready < nodes.total ? 'err' : '')}
    ${kpiCard('Pods running', `${t.running ?? 0}<span class="kpi-total"> / ${t.pods ?? 0}</span>`, `${plural(t.namespaces ?? 0, 'namespace', 'namespaces')}`)}
    ${kpiCard('Need attention', t.attention ?? 0, t.attention ? 'not ready or failing' : 'nothing to fix', t.attention ? 'err' : 'ok')}
    ${kpiCard('Restarts', t.restarts ?? 0, 'since the pods were created')}
  </div>`;
}

function namespacesCard(d) {
  const rows = d.namespaces || [];
  if (!rows.length) return `<section class="card clip span-2">${cardHead('Namespaces')}${okNote('No namespaces to show.')}</section>`;
  return `<section class="card clip span-2">${cardHead('Namespaces', 'Select one to open its pods')}
    <div class="scroll-x"><table class="tbl">
      <thead><tr><th scope="col">Namespace</th><th scope="col">Pods</th><th scope="col">Running</th><th scope="col">Attention</th><th scope="col">Restarts</th><th scope="col">Deployments</th></tr></thead>
      <tbody>${rows.map((n) => `<tr class="row-link" data-act="dash-ns" data-arg="${esc(n.name)}">
        <td><button class="link-btn mono strong" data-act="dash-ns" data-arg="${esc(n.name)}">${esc(n.name)}</button></td>
        <td class="tnum">${n.pods}</td><td class="tnum">${n.running}</td>
        <td>${n.attention ? badge(String(n.attention), 'err') : '<span class="muted">–</span>'}</td>
        <td class="tnum${n.restarts > 5 ? ' tone-err strong' : ''}">${n.restarts}</td><td class="tnum">${n.deployments}</td>
      </tr>`).join('')}</tbody></table></div></section>`;
}

function podRows(pods) {
  return pods.map((p) => `<tr>
    <td><div class="mono strong trunc cell-name" title="${esc(p.name)}">${esc(p.name)}</div><div class="muted small">${esc(p.namespace)} · ${esc(p.age)}</div></td>
    <td>${badge(p.status, TONE[p.health] || 'neutral')}</td>
    <td class="tnum${p.restarts > 5 ? ' tone-err strong' : ''}">${p.restarts}</td>
    <td class="act"><button class="btn btn-ghost btn-icon" data-act="dash-logs" data-id="${esc(p.namespace + '/' + p.name)}" aria-label="View logs of ${esc(p.name)}" title="View logs">${icon('term')}</button></td>
  </tr>`).join('');
}

const PODS_HEAD = '<thead><tr><th scope="col">Pod</th><th scope="col">Status</th><th scope="col">Restarts</th><th class="act" scope="col"><span class="sr-only">Actions</span></th></tr></thead>';

function attentionCard(d) {
  const pods = d.attentionPods || [];
  const body = pods.length
    ? `<div class="scroll-x"><table class="tbl">${PODS_HEAD}<tbody>${podRows(pods)}</tbody></table></div>`
    : okNote('Every pod is running and ready.');
  const total = (d.totals || {}).attention || 0;
  return `<section class="card clip">${cardHead('Pods that need attention', total > pods.length ? `Showing ${pods.length} of ${total}` : '')}${body}</section>`;
}

function restartsCard(d) {
  const pods = d.topRestarts || [];
  const body = pods.length
    ? `<div class="scroll-x"><table class="tbl">${PODS_HEAD}<tbody>${podRows(pods)}</tbody></table></div>`
    : okNote('No pod has restarted.');
  return `<section class="card clip">${cardHead('Most restarts')}${body}</section>`;
}

function warningsCard(d) {
  const events = d.warnings || [];
  const body = events.length
    ? `<ul class="events">${events.map((e) => `<li class="event">
        <div class="event-top">${badge(e.reason, 'warn')}<span class="mono small trunc">${esc(e.object)}</span><span class="muted small trunc">${esc(e.namespace)}</span>
          <span class="grow"></span><span class="muted small tnum nowrap">${e.count > 1 ? `×${e.count} · ` : ''}${esc(ago(e.age))}</span></div>
        <div class="event-msg small" title="${esc(e.message)}">${esc(e.message)}</div></li>`).join('')}</ul>`
    : okNote('No warning events lately.');
  return `<section class="card clip span-2">${cardHead('Recent warnings', 'Newest first')}${body}</section>`;
}

// Bars are SVG so no inline style is needed; widths are relative to the busiest pod.
function barList(items, valueOf, label) {
  if (!items.length) return '<div class="card-empty"><span class="muted">No data.</span></div>';
  const max = Math.max(...items.map(valueOf), 1);
  return `<ul class="bars">${items.map((m) => {
    const width = Math.max(2, Math.round((valueOf(m) / max) * 100));
    return `<li class="bar-row"><span class="bar-label" title="${esc(m.namespace + '/' + m.name)}"><span class="mono trunc">${esc(m.name)}</span><span class="muted small trunc">${esc(m.namespace)}</span></span>
      <svg class="bar" viewBox="0 0 100 8" preserveAspectRatio="none" role="img" aria-label="${esc(label(m))}"><rect class="bar-bg" width="100" height="8" rx="2"/><rect class="bar-fg" width="${width}" height="8" rx="2"/></svg>
      <span class="bar-value mono tnum">${esc(label(m))}</span></li>`;
  }).join('')}</ul>`;
}

function usageCard(d) {
  const m = d.metrics || { available: false };
  if (!m.available) {
    return `<section class="card clip span-2">${cardHead('Resource usage')}
      <div class="alert alert-info">${icon('info', 'icon icon-lg')}<div class="grow"><strong>Usage is not available</strong>
        <p>This cluster does not answer metrics requests, so CPU and memory cannot be shown. Install Metrics Server from the catalog to turn it on:</p>
        <button class="btn btn-primary mt2" data-act="nav" data-arg="helm">Open Apps (Helm)</button></div></div></section>`;
  }
  return `<section class="card clip span-2">${cardHead('Resource usage', 'Top pods, relative to the busiest one')}
    <div class="usage">
      <div><h4>CPU</h4>${barList(m.topCPU || [], (x) => x.cpuMilli, (x) => `${x.cpuMilli}m`)}</div>
      <div><h4>Memory</h4>${barList(m.topMemory || [], (x) => x.memoryMiB, (x) => `${x.memoryMiB} MiB`)}</div>
    </div></section>`;
}

function skeleton() {
  const card = (cls = '') => `<section class="card ${cls}"><div class="card-head"><span class="skel w2"></span></div><div class="skel-body"><span class="skel w3"></span><span class="skel w3"></span><span class="skel w2"></span></div></section>`;
  const kpi = '<div class="card kpi"><span class="skel w1"></span><span class="skel w2 tall"></span><span class="skel w3"></span></div>';
  return `<div class="status-hero status-neutral"><span class="skel w3"></span></div>
    <div class="kpis">${kpi.repeat(4)}</div>
    <div class="dash-grid">${card('span-2')}${card()}${card()}</div>`;
}

/* ---------- page ---------- */

registerPage('dashboard', {
  scoped: false,

  load: (S) => api.dashboard(S.ctx),

  html(S) {
    const L = S.load;
    const head = pageHead('Dashboard', `How <span class="mono">${esc(S.ctx)}</span> is doing right now.`, refreshControls(S));
    if (L.status === 'loading') return head + skeleton();
    if (L.status === 'error') return `${head}<div class="card clip">${errorState(L.error, 'the dashboard')}</div>`;

    const d = L.data;
    const notices = (d.notices || []).length
      ? `<div class="alert alert-warn">${icon('info', 'icon icon-lg')}<div class="grow"><strong>Some data is missing</strong>
          <ul class="notices">${d.notices.map((n) => `<li>${esc(n)}</li>`).join('')}</ul></div></div>` : '';

    return `${head}${hero(d)}${kpiRow(d)}${notices}
      <div class="dash-grid">${attentionCard(d)}${restartsCard(d)}${namespacesCard(d)}${warningsCard(d)}${usageCard(d)}</div>`;
  },

  async onClick(act, el, app) {
    const S = app.S;
    if (act === 'dash-ns') {
      app.setNamespace(el.dataset.arg, 'pods');
      return true;
    }
    if (act === 'dash-attention') {
      S.problems = true;
      app.setNamespace('_all', 'pods');
      return true;
    }
    if (act === 'dash-logs') {
      const d = S.load.data || {};
      const pod = [...(d.attentionPods || []), ...(d.topRestarts || [])].find((p) => p.namespace + '/' + p.name === el.dataset.id);
      if (pod) openLogs(S, pod);
      return true;
    }
    return false;
  },
});
