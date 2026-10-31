// Namespaces: list with pod counts, jump to a namespace's pods or deployments,
// and create a new one. There is no delete here on purpose (ADR 0003).

import { api } from './api.js';
import { esc, icon, badge } from './ui.js';
import { registerPage } from './pages.js';
import { pageHead, refreshControls, skeletonRows, emptyState, errorState, matches } from './views.js';
import { kubectlBar } from './dashboard.js';
import { openNamespaceForm } from './forms.js';

const STATUS_TONE = { Active: 'ok', Terminating: 'warn' };

function table(S, rows) {
  return `<div class="scroll-x"><table class="tbl">
    <thead><tr><th scope="col">Namespace</th><th scope="col">Status</th><th scope="col">Age</th><th scope="col">Pods</th><th scope="col">Running</th><th scope="col">Attention</th><th scope="col">Deployments</th><th class="act" scope="col"><span class="sr-only">Actions</span></th></tr></thead>
    <tbody>${rows.map((n) => `<tr>
      <td><div class="row g2"><span class="mono strong">${esc(n.name)}</span>${S.ns === n.name ? badge('In use', 'accent') : ''}</div></td>
      <td>${badge(n.status, STATUS_TONE[n.status] || 'neutral')}</td>
      <td class="muted tnum">${esc(n.age)}</td>
      <td class="tnum">${n.pods}</td><td class="tnum">${n.running}</td>
      <td>${n.attention ? badge(String(n.attention), 'err') : '<span class="muted">–</span>'}</td>
      <td class="tnum">${n.deployments}</td>
      <td class="act"><div class="row g1 end">
        <button class="btn" data-act="nsp-open" data-arg="pods" data-id="${esc(n.name)}">${icon('box')}Pods</button>
        <button class="btn" data-act="nsp-open" data-arg="deployments" data-id="${esc(n.name)}">${icon('layers')}Deployments</button>
      </div></td></tr>`).join('')}</tbody></table></div>`;
}

registerPage('namespaces', {
  scoped: false,

  load: (S) => api.namespaces(S.ctx),

  html(S) {
    const L = S.load;
    const newButton = `<button class="btn btn-primary" data-act="nsp-new">${icon('plus')}New namespace</button>`;
    const head = pageHead('Namespaces', `Groups of resources on <span class="mono">${esc(S.ctx)}</span>. Open one to see its pods or deployments.`, refreshControls(S) + newButton);

    let body;
    let extra = '';
    if (L.status === 'loading') {
      body = `<div class="scroll-x"><table class="tbl"><thead><tr>${'<th></th>'.repeat(8)}</tr></thead><tbody>${skeletonRows(8)}</tbody></table></div>`;
    } else if (L.status === 'error') {
      body = errorState(L.error, 'namespaces');
    } else {
      const needle = S.q.trim().toLowerCase();
      const rows = L.data.filter((n) => matches(needle, n.name));
      extra = `${rows.length} of ${L.data.length}`;
      if (!L.data.length) {
        body = emptyState('No namespaces yet', 'Namespaces keep teams and apps apart. Create the first one to get started.', 'New namespace', 'nsp-new');
      } else if (!rows.length) {
        body = emptyState('No namespaces match', 'Clear the search to see all of them.', 'Clear filters', 'clear-filters');
      } else {
        body = table(S, rows);
      }
    }
    return `${head}<div class="card clip">${body}${kubectlBar(S, 'kubectl create namespace <name>', extra)}</div>`;
  },

  onClick(act, el, app) {
    if (act === 'nsp-new') {
      openNamespaceForm(app.S, app.refresh);
      return true;
    }
    if (act === 'nsp-open') {
      app.setNamespace(el.dataset.id, el.dataset.arg);
      return true;
    }
    return false;
  },
});
