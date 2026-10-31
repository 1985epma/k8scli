// Page renderers. Each takes the app state S and returns an HTML string;
// app.js owns the events. Everything that came from the cluster is escaped.

import { esc, icon, badge, TONE, cliFor } from './ui.js';

export const VIEWS = {
  dashboard: { title: 'Dashboard', group: 'Overview', icon: 'dashboard' },
  pods: { title: 'Pods', group: 'Workloads', icon: 'box' },
  deployments: { title: 'Deployments', group: 'Workloads', icon: 'layers' },
  autoscaling: { title: 'Autoscaling', group: 'Workloads', icon: 'trend' },
  helm: { title: 'Apps (Helm)', group: 'Workloads', icon: 'package' },
  resilience: { title: 'Resilience', group: 'Cluster', icon: 'shield' },
  namespaces: { title: 'Namespaces', group: 'Cluster', icon: 'folder' },
  configmaps: { title: 'Configuration', group: 'Cluster', icon: 'sliders' },
  contexts: { title: 'Contexts', group: 'Cluster', icon: 'server' },
  'setup-connect': { title: 'Connect a cluster', group: 'Setup', icon: 'plug' },
  'setup-install': { title: 'Install the CLI', group: 'Setup', icon: 'download' },
  'setup-completion': { title: 'Shell completion', group: 'Setup', icon: 'term' },
};
export const DATA_VIEWS = ['pods', 'deployments'];

export function sidebarHTML(S) {
  const groups = {};
  for (const [id, view] of Object.entries(VIEWS)) (groups[view.group] ||= []).push([id, view]);

  const nav = Object.entries(groups).map(([group, items]) => `
    <div class="nav-group">${esc(group)}</div>
    ${items.map(([id, view]) => `
      <button class="nav-item" data-act="nav" data-arg="${id}" title="${esc(view.title)}"${S.view === id ? ' aria-current="page"' : ''}>
        ${icon(view.icon, 'icon icon-lg')}<span class="nav-label">${esc(view.title)}</span>
      </button>`).join('')}`).join('');

  return `
    <div class="brand">
      <div class="brand-mark mono">k8</div>
      <div class="brand-text"><strong>k8scli</strong><span class="muted small">Web console</span></div>
    </div>
    <nav class="nav">${nav}</nav>
    <div class="nav-foot"><span class="mono small muted nav-label">${esc(S.app ? S.app.version : '')}</span></div>`;
}

/* ---------- shared blocks ---------- */

export function agoText(updatedAt) {
  const seconds = Math.max(0, Math.round((Date.now() - updatedAt) / 1000));
  if (seconds < 5) return 'just now';
  if (seconds < 60) return `${seconds}s ago`;
  return `${Math.round(seconds / 60)}m ago`;
}

export function pageHead(title, subtitle, right = '') {
  return `<div class="page-head">
    <div class="grow"><h1>${title}</h1><p class="muted">${subtitle}</p></div>
    <div class="row g2 wrap">${right}</div>
  </div>`;
}

export function refreshControls(S) {
  const L = S.load;
  const stamp = L.status === 'ready'
    ? `<span class="muted small" id="updated-ago">Updated ${agoText(L.updatedAt)}</span>` : '';
  const failed = L.refreshError
    ? `<span class="tone-err small" title="${esc(L.refreshError)}">Refresh failed</span>` : '';
  return `${failed}${stamp}<button class="btn" data-act="refresh">${icon('refresh')}Refresh</button>`;
}

export function cliBar(S, command, extra = '') {
  const full = cliFor(S, command);
  return `<div class="cli-bar">
    <span class="muted small nowrap">CLI equivalent</span>
    <code class="cli grow">${esc(full)}</code>
    ${extra ? `<span class="muted small tnum hide-sm">${esc(extra)}</span>` : ''}
    <button class="btn btn-ghost btn-icon" data-act="copy" data-arg="${esc(full)}" aria-label="Copy command">${icon('copy')}</button>
  </div>`;
}

export function skeletonRows(columns) {
  const widths = ['w3', 'w1', 'w2', 'w1', 'w1', 'w2', 'w2'];
  let rows = '';
  for (let row = 0; row < 6; row++) {
    rows += '<tr>';
    for (let col = 0; col < columns; col++) rows += `<td><span class="skel ${widths[(col + row) % widths.length]}"></span></td>`;
    rows += '</tr>';
  }
  return rows;
}

export function emptyState(title, text, ctaLabel, ctaAct, ctaArg = '') {
  return `<div class="empty">
    <svg class="empty-art" width="120" height="96" viewBox="0 0 120 96" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <path d="M60 8 100 30v36L60 88 20 66V30Z" stroke-dasharray="4 5"/><path d="M20 30 60 52l40-22M60 52v36" opacity=".55"/>
      <circle cx="60" cy="52" r="5" class="empty-dot"/>
    </svg>
    <h3>${esc(title)}</h3>
    <p class="muted">${esc(text)}</p>
    <button class="btn btn-primary" data-act="${ctaAct}" data-arg="${esc(ctaArg)}">${icon('plus')}${esc(ctaLabel)}</button>
  </div>`;
}

export function errorState(error, what) {
  if (error.status === 401) {
    return `<div class="alert alert-err">${icon('warn', 'icon icon-lg')}<div>
      <strong>Your session ended</strong>
      <p>Open the link printed by <span class="mono">k8scli serve</span> again to start a new one.</p></div></div>`;
  }
  return `<div class="alert alert-err">${icon('warn', 'icon icon-lg')}<div class="grow">
    <strong>Couldn't load ${esc(what)}</strong>
    <code class="cli block">${esc(error.message)}</code>
    ${error.hint ? `<p>${esc(error.hint)}</p>` : ''}
    <div class="row g2 mt2"><button class="btn btn-primary" data-act="retry">${icon('refresh')}Try again</button>
    <button class="btn" data-act="nav" data-arg="setup-connect">Check the connection</button></div>
  </div></div>`;
}

export function kpiCard(label, value, sub, tone = '') {
  return `<div class="card kpi"><div class="muted small">${esc(label)}</div>
    <div class="kpi-value tnum">${value}</div><div class="small kpi-sub ${tone ? 'tone-' + tone : 'muted'}">${esc(sub)}</div></div>`;
}

function kpis(S, items) {
  if (S.load.status === 'loading') {
    const placeholder = '<div class="card kpi"><span class="skel w1"></span><span class="skel w2 tall"></span><span class="skel w3"></span></div>';
    return `<div class="kpis">${placeholder.repeat(4)}</div>`;
  }
  if (S.load.status !== 'ready') return '';
  return `<div class="kpis">${items.join('')}</div>`;
}

// `available` lists the namespaces to offer; pods and deployments take them from the overview.
export function namespaceSelect(S, available) {
  const known = available || (S.load.overview && S.load.overview.namespaces) || [];
  const shown = known.includes(S.ns) || S.ns === '_all' ? known : [S.ns, ...known];
  const options = [`<option value="_all"${S.ns === '_all' ? ' selected' : ''}>All namespaces</option>`]
    .concat(shown.map((name) => `<option value="${esc(name)}"${S.ns === name ? ' selected' : ''}>${esc(name)}</option>`));
  return `<label class="row g2 muted small">Namespace
    <select id="ns-select" class="field mono">${options.join('')}</select></label>`;
}

export const matches = (needle, ...parts) => !needle || parts.join(' ').toLowerCase().includes(needle);

/* ---------- pods ---------- */

export function podsView(S) {
  const L = S.load;
  const all = S.ns === '_all';
  const scope = all ? 'all namespaces' : esc(S.ns);
  const needle = S.q.trim().toLowerCase();

  const list = L.status === 'ready' ? L.list : [];
  const attention = list.filter((p) => p.health === 'warn' || p.health === 'error');
  const running = list.filter((p) => p.status === 'Running').length;
  const rows = list.filter((p) => (!S.problems || p.health === 'warn' || p.health === 'error') && matches(needle, p.name, p.namespace));

  const columns = [...(all ? ['Namespace'] : []), 'Name', 'Ready', 'Status', 'Restarts', 'Age', ...(S.wide ? ['IP', 'Node'] : [])];
  const head = `<thead><tr>${columns.map((c) => `<th scope="col">${c}</th>`).join('')}<th class="act" scope="col"><span class="sr-only">Actions</span></th></tr></thead>`;

  let body;
  if (L.status === 'loading') {
    body = `<div class="scroll-x"><table class="tbl">${head}<tbody>${skeletonRows(columns.length + 1)}</tbody></table></div>`;
  } else if (L.status === 'error') {
    body = errorState(L.error, 'pods');
  } else if (!rows.length) {
    body = list.length
      ? emptyState('No pods match', 'Clear the search or the "Needs attention" filter to see the rest.', 'Clear filters', 'clear-filters')
      : all
        ? emptyState('No pods in this cluster', 'Once a deployment starts replicas, they show up here with status and restarts.', 'Open deployments', 'nav', 'deployments')
        : emptyState(`No pods in "${S.ns}"`, 'Once a deployment starts replicas in this namespace, they show up here.', 'Show all namespaces', 'ns-all');
  } else {
    body = `<div class="scroll-x"><table class="tbl">${head}<tbody>${rows.map((p) => `<tr>
      ${all ? `<td class="muted">${esc(p.namespace)}</td>` : ''}
      <td class="mono strong">${esc(p.name)}</td>
      <td class="tnum">${esc(p.ready)}</td>
      <td>${badge(p.status, TONE[p.health] || 'neutral')}</td>
      <td class="tnum${p.restarts > 5 ? ' tone-err strong' : ''}">${esc(p.restarts)}</td>
      <td class="muted tnum">${esc(p.age)}</td>
      ${S.wide ? `<td class="mono muted">${esc(p.ip || '<none>')}</td><td class="mono muted">${esc(p.node || '<none>')}</td>` : ''}
      <td class="act"><button class="btn btn-ghost btn-icon" data-act="row-menu" data-kind="pod" data-id="${esc(p.namespace + '/' + p.name)}" aria-label="Actions for ${esc(p.name)}" aria-haspopup="menu">${icon('more')}</button></td>
    </tr>`).join('')}</tbody></table></div>`;
  }

  const toolbar = `<div class="toolbar">${namespaceSelect(S)}
    <button class="chip" data-act="problems" aria-pressed="${S.problems}">Needs attention${L.status === 'ready' ? ` (${attention.length})` : ''}</button>
    <label class="check"><input type="checkbox" id="wide-toggle"${S.wide ? ' checked' : ''}> Show IP and node</label></div>`;

  const cmd = `k8scli ${all ? 'pods-all' : `pods -n ${S.ns}`}${S.wide ? ' -o wide' : ''}`;
  const extra = L.status === 'ready' && rows.length ? `${rows.length} of ${list.length} pods` : '';

  return pageHead('Pods', `Status of the pods in ${scope} on <span class="mono">${esc(S.ctx)}</span>.`, refreshControls(S)) +
    kpis(S, L.status === 'ready' ? clusterKpis(L.overview).concat([
      kpiCard('Pods running', `${running}<span class="kpi-total"> / ${list.length}</span>`, all ? 'all namespaces' : `in ${S.ns}`),
      kpiCard('Need attention', attention.length, attention.length ? 'not ready or failing' : 'everything is healthy', attention.length ? 'err' : 'ok'),
    ]).slice(0, 4) : []) +
    `<div class="card clip">${toolbar}${body}${cliBar(S, cmd, extra)}</div>`;
}

function clusterKpis(overview) {
  const ready = overview.nodes.filter((n) => n.ready).length;
  return [
    kpiCard('Kubernetes', esc(overview.version || 'unknown'), overview.platform || ''),
    kpiCard('Nodes ready', `${ready}<span class="kpi-total"> / ${overview.nodes.length}</span>`,
      overview.warnings.length ? 'node list not available' : (ready === overview.nodes.length ? 'all nodes ready' : 'some nodes are not ready'),
      overview.nodes.length && ready < overview.nodes.length ? 'err' : ''),
  ];
}

/* ---------- deployments ---------- */

export function deploymentsView(S) {
  const L = S.load;
  const all = S.ns === '_all';
  const needle = S.q.trim().toLowerCase();
  const list = L.status === 'ready' ? L.list : [];
  const rows = list.filter((d) => matches(needle, d.name, d.namespace));
  const fine = list.filter((d) => d.health === 'ok' || d.health === 'done').length;

  const columns = [...(all ? ['Namespace'] : []), 'Deployment', 'Ready', 'Desired', 'Status', 'Age'];
  const head = `<thead><tr>${columns.map((c) => `<th scope="col">${c}</th>`).join('')}<th class="act" scope="col"><span class="sr-only">Actions</span></th></tr></thead>`;

  let body;
  if (L.status === 'loading') {
    body = `<div class="scroll-x"><table class="tbl">${head}<tbody>${skeletonRows(columns.length + 1)}</tbody></table></div>`;
  } else if (L.status === 'error') {
    body = errorState(L.error, 'deployments');
  } else if (!rows.length) {
    body = list.length
      ? emptyState('No deployments match', 'Clear the search to see the rest.', 'Clear filters', 'clear-filters')
      : emptyState(all ? 'No deployments in this cluster' : `No deployments in "${S.ns}"`,
        'Deployments appear here so you can change their replicas with a dry run and a safety check.',
        all ? 'Open pods' : 'Show all namespaces', all ? 'nav' : 'ns-all', all ? 'pods' : '');
  } else {
    const label = { ok: 'Available', warn: 'Updating', error: 'Unavailable', done: 'Scaled to zero' };
    body = `<div class="scroll-x"><table class="tbl">${head}<tbody>${rows.map((d) => `<tr>
      ${all ? `<td class="muted">${esc(d.namespace)}</td>` : ''}
      <td class="mono strong">${esc(d.name)}</td>
      <td class="tnum">${esc(d.ready)}</td><td class="tnum">${esc(d.desired)}</td>
      <td>${badge(label[d.health] || d.health, TONE[d.health] || 'neutral')}</td>
      <td class="muted tnum">${esc(d.age)}</td>
      <td class="act"><div class="row g1 end">
        <button class="btn" data-act="scale" data-id="${esc(d.namespace + '/' + d.name)}">${icon('scale')}Scale</button>
        <button class="btn btn-ghost btn-icon" data-act="row-menu" data-kind="dep" data-id="${esc(d.namespace + '/' + d.name)}" aria-label="Actions for ${esc(d.name)}" aria-haspopup="menu">${icon('more')}</button>
      </div></td>
    </tr>`).join('')}</tbody></table></div>`;
  }

  const toolbar = `<div class="toolbar">${namespaceSelect(S)}<span class="grow"></span>
    <span class="muted small">Quick scale in the CLI: <span class="mono">k8scli set &lt;deployment&gt; 2|4|6|8</span></span></div>`;
  const extra = L.status === 'ready' ? `${rows.length} deployments` : '';

  const newButton = `<button class="btn btn-primary" data-act="deploy-new">${icon('rocket')}New deployment</button>`;
  return pageHead('Deployments', `Change replicas in ${all ? 'all namespaces' : esc(S.ns)} on <span class="mono">${esc(S.ctx)}</span>. Production contexts ask you to type their name first.`, refreshControls(S) + newButton) +
    kpis(S, L.status === 'ready' ? clusterKpis(L.overview).concat([
      kpiCard('Deployments available', `${fine}<span class="kpi-total"> / ${list.length}</span>`, all ? 'all namespaces' : `in ${S.ns}`),
      kpiCard('Not fully ready', list.length - fine, list.length - fine ? 'fewer ready than desired' : 'everything is up', list.length - fine ? 'err' : 'ok'),
    ]).slice(0, 4) : []) +
    `<div class="card clip">${toolbar}${body}${cliBar(S, `k8scli scale <deployment> <replicas>${all ? '' : ` -n ${S.ns}`}`, extra)}</div>`;
}

/* ---------- contexts ---------- */

export function contextsView(S) {
  const contexts = S.app.contexts;
  const needle = S.q.trim().toLowerCase();
  const rows = contexts.filter((c) => matches(needle, c.name, c.cluster, c.user));
  const head = `<thead><tr><th scope="col">State</th><th scope="col">Name</th><th scope="col">Cluster</th><th scope="col">User</th><th scope="col">Namespace</th><th class="act" scope="col"><span class="sr-only">Actions</span></th></tr></thead>`;

  let body;
  if (S.app.kubeconfigError) {
    body = `<div class="alert alert-err">${icon('warn', 'icon icon-lg')}<div><strong>Couldn't read the kubeconfig</strong>
      <code class="cli block">${esc(S.app.kubeconfigError)}</code></div></div>`;
  } else if (!contexts.length) {
    body = emptyState('No contexts found', 'Connect a cluster and it shows up here, ready to use.', 'Connect a cluster', 'nav', 'setup-connect');
  } else if (!rows.length) {
    body = emptyState('No contexts match', 'Clear the search to see all contexts.', 'Clear filters', 'clear-filters');
  } else {
    body = `<div class="scroll-x"><table class="tbl">${head}<tbody>${rows.map((c) => `<tr>
      <td><div class="row g1 wrap">${c.name === S.ctx ? badge('In use', 'accent') : ''}${c.current ? badge('CLI default', 'neutral') : ''}</div></td>
      <td class="mono strong">${esc(c.name)}${c.production ? ' ' + badge('production', 'err') : ''}</td>
      <td class="mono muted">${esc(c.cluster)}</td><td class="mono muted">${esc(c.user)}</td><td class="mono muted">${esc(c.namespace || 'default')}</td>
      <td class="act"><div class="row g1 end">
        ${c.name === S.ctx ? '' : `<button class="btn" data-act="use-ctx" data-arg="${esc(c.name)}">Use here</button>`}
        <button class="btn btn-ghost btn-icon" data-act="row-menu" data-kind="ctx" data-id="${esc(c.name)}" aria-label="Actions for ${esc(c.name)}" aria-haspopup="menu">${icon('more')}</button>
      </div></td></tr>`).join('')}</tbody></table></div>`;
  }

  const paths = S.app.kubeconfigPaths.map((p) => `<span class="mono">${esc(p)}</span>`).join(', ');
  return pageHead('Contexts', 'Read from your kubeconfig, sorted by name. "Use here" only changes this console; "CLI default" is what <span class="mono">k8scli use</span> writes.', '') +
    `<div class="card clip">${body}<div class="cli-bar"><span class="muted small">Reading</span><span class="small grow trunc">${paths || 'no kubeconfig files'}</span></div>${cliBar(S, 'k8scli contexts', `${contexts.length} contexts`)}</div>`;
}

export function noContextView() {
  return pageHead('Welcome', 'k8scli needs a cluster to talk to.', '') +
    `<div class="card clip">${emptyState('No cluster connected yet', 'The setup wizard walks you through getting credentials and checking the connection.', 'Connect a cluster', 'nav', 'setup-connect')}</div>`;
}
