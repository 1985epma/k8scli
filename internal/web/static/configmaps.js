// Configuration: the ConfigMaps of a namespace and a key editor in the drawer.
// Secrets are never listed or read here. Every change goes through confirmChange,
// which previews it with a dry run before it can be applied.

import { api } from './api.js';
import { $, esc, icon, badge, toast, showDrawer } from './ui.js';
import { registerPage } from './pages.js';
import { pageHead, refreshControls, skeletonRows, emptyState, errorState, namespaceSelect, matches } from './views.js';
import { kubectlBar } from './dashboard.js';
import { openConfigMapForm, confirmChange } from './forms.js';

const PREVIEW_CHARS = 140;
const DRAWER_LABEL = 'Pod logs'; // the label index.html gives the drawer; restored on close

let cm = null; // the ConfigMap open in the drawer: { namespace, name, age, data, binaryKeys, mode, key }
let appRef = null;

/* ---------- helpers ---------- */

function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  return `${(bytes / 1024).toFixed(bytes < 10240 ? 1 : 0)} KiB`;
}

function preview(value) {
  const flat = value.replace(/\s+/g, ' ').trim();
  if (!flat) return '<span class="muted">(empty)</span>';
  const lines = value.split('\n').length;
  const text = flat.length > PREVIEW_CHARS ? `${flat.slice(0, PREVIEW_CHARS)}…` : flat;
  return `${esc(text)}${lines > 1 ? ` <span class="muted small nowrap">${lines} lines</span>` : ''}`;
}

/* ---------- list ---------- */

function table(all, rows) {
  const columns = [...(all ? ['Namespace'] : []), 'Name', 'Keys', 'Size', 'Age'];
  return `<div class="scroll-x"><table class="tbl">
    <thead><tr>${columns.map((c) => `<th scope="col">${c}</th>`).join('')}<th class="act" scope="col"><span class="sr-only">Actions</span></th></tr></thead>
    <tbody>${rows.map((c) => {
      const id = esc(c.namespace + '/' + c.name);
      return `<tr class="row-link" data-act="cm-open" data-id="${id}">
        ${all ? `<td class="muted">${esc(c.namespace)}</td>` : ''}
        <td><div class="row g2"><button class="link-btn mono strong" data-act="cm-open" data-id="${id}">${esc(c.name)}</button>${c.managed ? badge('k8scli', 'accent') : ''}</div></td>
        <td class="tnum">${c.keys}</td><td class="muted tnum">${formatSize(c.size)}</td><td class="muted tnum">${esc(c.age)}</td>
        <td class="act"><button class="btn" data-act="cm-open" data-id="${id}">${icon('pencil')}Open</button></td></tr>`;
    }).join('')}</tbody></table></div>`;
}

/* ---------- drawer ---------- */

function noticeReadOnly(S) {
  return S.app.readOnly
    ? '<div class="alert alert-info">' + icon('eye', 'icon icon-lg') + '<div>Read-only mode: you can preview a change, but it cannot be applied.</div></div>' : '';
}

function keyRow(key, value) {
  return `<li class="cm-key">
    <div class="cm-key-top"><span class="mono strong trunc">${esc(key)}</span><span class="grow"></span>
      <button class="btn btn-ghost btn-icon" data-act="cm-edit" data-arg="${esc(key)}" aria-label="Edit ${esc(key)}" title="Edit this key">${icon('pencil')}</button>
      <button class="btn btn-ghost btn-icon" data-act="cm-remove" data-arg="${esc(key)}" aria-label="Remove ${esc(key)}" title="Remove this key">${icon('trash')}</button></div>
    <div class="value-preview mono" title="${esc(value.slice(0, 300))}">${preview(value)}</div></li>`;
}

function editor() {
  const adding = cm.mode === 'add';
  const current = adding ? '' : cm.data[cm.key];
  return `<div class="cm-editor">
    <h3>${adding ? 'Add a key' : `Edit <span class="mono">${esc(cm.key)}</span>`}</h3>
    ${adding ? '<label class="field-group"><span>Key</span><input id="cm-new-key" class="field mono" placeholder="app.properties" autocomplete="off" spellcheck="false"><small class="muted">Letters, digits, "-", "_" and ".".</small></label>' : ''}
    <label class="field-group"><span>Value</span><textarea id="cm-value" class="field mono cm-textarea" rows="8" spellcheck="false">${esc(current)}</textarea></label>
    <div class="row g2"><button type="button" class="btn btn-primary" data-act="cm-review">Review change</button>
      <button type="button" class="btn" data-act="cm-cancel">Cancel</button></div>
  </div>`;
}

function drawerHTML(S) {
  const keys = Object.keys(cm.data).sort();
  const list = keys.length
    ? `<ul class="cm-keys">${keys.map((k) => keyRow(k, cm.data[k])).join('')}</ul>`
    : '<div class="card-empty"><span class="muted">This ConfigMap has no text keys.</span></div>';
  const binary = cm.binaryKeys.length
    ? `<p class="muted small">Binary keys (not shown): ${cm.binaryKeys.map((k) => `<span class="mono">${esc(k)}</span>`).join(', ')}</p>` : '';

  return `<div class="drawer-head">
      <div class="grow"><div class="muted small">ConfigMap · ${esc(cm.namespace)}</div><h2 class="mono trunc">${esc(cm.name)}</h2></div>
      <button class="btn btn-ghost btn-icon" data-act="close-panel" aria-label="Close">${icon('x')}</button>
    </div>
    <div class="drawer-body">
      ${noticeReadOnly(S)}
      ${cm.mode === 'view' ? `${list}${binary}<div><button class="btn" data-act="cm-add">${icon('plus')}Add a key</button></div>` : editor()}
      <p class="muted small">Secrets are never shown or edited here.</p>
    </div>`;
}

function renderDrawer(S) {
  $('#drawer').innerHTML = drawerHTML(S);
  const field = $('#cm-new-key') || $('#cm-value');
  if (field) field.focus();
}

async function openConfigMap(S, id) {
  const [namespace, name] = id.split('/');
  try {
    const detail = await api.configMap(S.ctx, namespace, name);
    cm = { namespace, name, age: detail.age, data: detail.data || {}, binaryKeys: detail.binaryKeys || [], mode: 'view', key: '' };
  } catch (error) {
    toast(esc(error.message), 'warn');
    return;
  }
  showDrawer(drawerHTML(S), () => {
    cm = null;
    $('#drawer').setAttribute('aria-label', DRAWER_LABEL);
  });
  $('#drawer').setAttribute('aria-label', `ConfigMap ${name}`);
}

// A value of null removes the key (the API's contract for /api/configmaps/data).
function reviewChange(S, key, value) {
  const { namespace, name } = cm;
  const verb = value === null ? 'Remove' : (key in cm.data ? 'Update' : 'Add');
  confirmChange(S, {
    title: `${verb} "${key}" in ${name}`,
    call: (mutation) => api.setConfigMapKey({ ...mutation, namespace, name, key, value }),
    onDone: async () => {
      try {
        const detail = await api.configMap(S.ctx, namespace, name);
        if (cm && cm.name === name) {
          cm = { ...cm, data: detail.data || {}, binaryKeys: detail.binaryKeys || [], mode: 'view', key: '' };
          renderDrawer(S);
        }
      } catch (error) {
        toast(esc(error.message), 'warn');
      }
      if (appRef) appRef.refresh();
    },
  });
}

/* ---------- page ---------- */

registerPage('configmaps', {
  scoped: true,

  async load(S) {
    const [configMaps, namespaces] = await Promise.all([
      api.configMaps(S.ctx, S.ns),
      api.namespaces(S.ctx).then((list) => list.map((n) => n.name), () => []),
    ]);
    return { configMaps, namespaces };
  },

  html(S) {
    const L = S.load;
    const all = S.ns === '_all';
    const newButton = `<button class="btn btn-primary" data-act="cm-new">${icon('plus')}New ConfigMap</button>`;
    const head = pageHead('Configuration', `ConfigMaps in ${all ? 'all namespaces' : esc(S.ns)} on <span class="mono">${esc(S.ctx)}</span>. Change one key at a time, with a preview first.`, refreshControls(S) + newButton);

    const toolbar = `<div class="toolbar">${namespaceSelect(S, L.status === 'ready' ? L.data.namespaces : [])}<span class="grow"></span>
      <span class="muted small">${icon('eye')} Secrets are never listed here.</span></div>`;

    let body;
    let extra = '';
    if (L.status === 'loading') {
      body = `<div class="scroll-x"><table class="tbl"><thead><tr>${'<th></th>'.repeat(6)}</tr></thead><tbody>${skeletonRows(6)}</tbody></table></div>`;
    } else if (L.status === 'error') {
      body = errorState(L.error, 'ConfigMaps');
    } else {
      const needle = S.q.trim().toLowerCase();
      const rows = L.data.configMaps.filter((c) => matches(needle, c.name, c.namespace));
      extra = `${rows.length} ConfigMaps`;
      if (!L.data.configMaps.length) {
        body = emptyState(all ? 'No ConfigMaps in this cluster' : `No ConfigMaps in "${S.ns}"`,
          'A ConfigMap holds settings that your pods read, so you can change them without rebuilding an image.',
          'New ConfigMap', 'cm-new');
      } else if (!rows.length) {
        body = emptyState('No ConfigMaps match', 'Clear the search to see the rest.', 'Clear filters', 'clear-filters');
      } else {
        body = table(all, rows);
      }
    }

    const command = `kubectl get configmaps${all ? ' -A' : ` -n ${S.ns}`}`;
    return `${head}<div class="card clip">${toolbar}${body}${kubectlBar(S, command, extra)}</div>`;
  },

  async onClick(act, el, app) {
    appRef = app;
    const S = app.S;
    // The drawer buttons only exist while a ConfigMap is open, but a stale click must not throw.
    if (act.startsWith('cm-') && act !== 'cm-new' && act !== 'cm-open' && !cm) return true;
    switch (act) {
      case 'cm-new':
        openConfigMapForm(S, { namespace: S.ns === '_all' ? '' : S.ns }, app.refresh);
        return true;
      case 'cm-open':
        await openConfigMap(S, el.dataset.id);
        return true;
      case 'cm-edit':
        cm.mode = 'edit';
        cm.key = el.dataset.arg;
        renderDrawer(S);
        return true;
      case 'cm-add':
        cm.mode = 'add';
        cm.key = '';
        renderDrawer(S);
        return true;
      case 'cm-cancel':
        cm.mode = 'view';
        renderDrawer(S);
        return true;
      case 'cm-remove':
        reviewChange(S, el.dataset.arg, null);
        return true;
      case 'cm-review': {
        const key = cm.mode === 'add' ? $('#cm-new-key').value.trim() : cm.key;
        if (!key) {
          toast('Enter a key name first.', 'warn');
          return true;
        }
        if (!/^[-._a-zA-Z0-9]+$/.test(key) || key === '.' || key === '..' || key.length > 253) {
          toast('A key may only use letters, digits, "-", "_" and ".".', 'warn');
          return true;
        }
        reviewChange(S, key, $('#cm-value').value);
        return true;
      }
      default:
        return false;
    }
  },
});
