// Autoscaling: the HorizontalPodAutoscalers of a namespace, with create, edit and
// remove. The forms live in forms.js; this page lists and starts them.

import { api } from './api.js';
import { esc, icon, badge, TONE } from './ui.js';
import { registerPage } from './pages.js';
import { pageHead, refreshControls, skeletonRows, emptyState, errorState, namespaceSelect, matches } from './views.js';
import { kubectlBar } from './dashboard.js';
import { openAutoscaleForm, openDeleteAutoscalerForm } from './forms.js';

const HEALTH_LABEL = { ok: 'Healthy', warn: 'Adjusting', error: 'Problem', done: 'Idle' };

const percent = (value) => (value > 0 ? `${value}%` : '–');

// The API sends the target as "Kind/name". Deployments are the common case, so show just the name for them.
const parseTarget = (target) => {
  const [kind, ...rest] = String(target).split('/');
  return rest.length ? { kind, name: rest.join('/') } : { kind: 'Deployment', name: kind };
};
const targetLabel = (target) => { const t = parseTarget(target); return t.kind === 'Deployment' ? t.name : `${t.kind}/${t.name}`; };

// "42% of 80%": what the pods use now against what the autoscaler aims for.
function cpuCell(a) {
  if (!a.cpuPercent) return '<span class="muted">–</span>';
  const current = a.currentCPU >= 0 ? `${a.currentCPU}%` : '?';
  return `<span class="tnum${a.currentCPU > a.cpuPercent ? ' tone-warn strong' : ''}">${current}</span><span class="muted"> of ${a.cpuPercent}%</span>`;
}

function table(all, rows) {
  const columns = [...(all ? ['Namespace'] : []), 'Autoscaler', 'Target', 'Range', 'CPU', 'Memory', 'Replicas', 'Status', 'Age'];
  return `<div class="scroll-x"><table class="tbl">
    <thead><tr>${columns.map((c) => `<th scope="col">${c}</th>`).join('')}<th class="act" scope="col"><span class="sr-only">Actions</span></th></tr></thead>
    <tbody>${rows.map((a) => {
      const atMax = a.current >= a.max && a.desired >= a.max;
      return `<tr>
        ${all ? `<td class="muted">${esc(a.namespace)}</td>` : ''}
        <td class="mono strong">${esc(a.name)}</td>
        <td class="mono">${esc(targetLabel(a.target))}</td>
        <td class="tnum">${a.min}–${a.max}</td>
        <td>${cpuCell(a)}</td>
        <td class="tnum">${percent(a.memoryPercent)}</td>
        <td class="tnum">${a.current} <span class="muted">→</span> ${a.desired}${atMax ? ' <span class="muted small">at max</span>' : ''}</td>
        <td>${badge(HEALTH_LABEL[a.health] || a.health, TONE[a.health] || 'neutral')}</td>
        <td class="muted tnum">${esc(a.age)}</td>
        <td class="act"><div class="row g1 end">
          <button class="btn" data-act="as-edit" data-id="${esc(a.namespace + '/' + a.name)}"${parseTarget(a.target).kind === 'Deployment' ? '' : ' disabled title="Only autoscalers of deployments can be edited here"'}>${icon('pencil')}Edit</button>
          <button class="btn btn-ghost btn-icon" data-act="as-delete" data-id="${esc(a.namespace + '/' + a.name)}" aria-label="Remove ${esc(a.name)}" title="Remove this autoscaler">${icon('trash')}</button>
        </div></td></tr>`;
    }).join('')}</tbody></table></div>`;
}

function requirements() {
  return `<div class="alert alert-info">${icon('info', 'icon icon-lg')}<div class="grow"><strong>What autoscaling needs</strong>
    <ul class="notices">
      <li>metrics-server running in the cluster. Check with <span class="mono">kubectl top pods</span>.</li>
      <li>CPU requests on the containers. Without them the CPU percentage has nothing to compare against.</li>
      <li>A minimum of at least 1 replica. The autoscaler then keeps the count between the minimum and the maximum.</li>
    </ul></div></div>`;
}

registerPage('autoscaling', {
  scoped: true,

  async load(S) {
    const [autoscalers, namespaces] = await Promise.all([
      api.autoscalers(S.ctx, S.ns),
      // The selector is a convenience: a user who cannot list namespaces still gets the page.
      api.namespaces(S.ctx).then((list) => list.map((n) => n.name), () => []),
    ]);
    return { autoscalers, namespaces };
  },

  html(S) {
    const L = S.load;
    const all = S.ns === '_all';
    const scope = all ? 'all namespaces' : esc(S.ns);
    const newButton = `<button class="btn btn-primary" data-act="as-new">${icon('plus')}New autoscaler</button>`;
    const head = pageHead('Autoscaling', `Add replicas when load goes up and remove them when it drops, in ${scope} on <span class="mono">${esc(S.ctx)}</span>.`, refreshControls(S) + newButton);

    const toolbar = `<div class="toolbar">${namespaceSelect(S, L.status === 'ready' ? L.data.namespaces : [])}</div>`;
    let body;
    let extra = '';
    if (L.status === 'loading') {
      body = `<div class="scroll-x"><table class="tbl"><thead><tr>${'<th></th>'.repeat(9)}</tr></thead><tbody>${skeletonRows(9)}</tbody></table></div>`;
    } else if (L.status === 'error') {
      body = errorState(L.error, 'autoscalers');
    } else {
      const needle = S.q.trim().toLowerCase();
      const rows = L.data.autoscalers.filter((a) => matches(needle, a.name, targetLabel(a.target), a.namespace));
      extra = `${rows.length} autoscalers`;
      if (!L.data.autoscalers.length) {
        body = emptyState(all ? 'No autoscalers in this cluster' : `No autoscalers in "${S.ns}"`,
          'An autoscaler keeps a deployment between a minimum and a maximum number of replicas, based on CPU or memory use.',
          'New autoscaler', 'as-new');
      } else if (!rows.length) {
        body = emptyState('No autoscalers match', 'Clear the search to see the rest.', 'Clear filters', 'clear-filters');
      } else {
        body = table(all, rows);
      }
    }

    const command = `kubectl autoscale deployment <name> --min=2 --max=10 --cpu-percent=80${all ? '' : ` -n ${S.ns}`}`;
    return `${head}<div class="card clip">${toolbar}${body}${kubectlBar(S, command, extra)}</div>${requirements()}`;
  },

  onClick(act, el, app) {
    const S = app.S;
    if (act === 'as-new') {
      openAutoscaleForm(S, {}, app.refresh);
      return true;
    }
    if (act === 'as-edit' || act === 'as-delete') {
      const row = (S.load.data ? S.load.data.autoscalers : []).find((a) => a.namespace + '/' + a.name === el.dataset.id);
      if (!row) return true;
      if (act === 'as-edit') openAutoscaleForm(S, { existing: row, deployment: { namespace: row.namespace, name: parseTarget(row.target).name } }, app.refresh);
      else openDeleteAutoscalerForm(S, row, app.refresh);
      return true;
    }
    return false;
  },
});
