// Entry point: state, routing, data loading and the single delegated event
// handler. Rendering lives in views.js, panels.js and wizards.js.

import { api, ApiError } from './api.js';
import {
  $, esc, icon, installIcons, store, toast, copyText, cliFor,
  openMenu, closeMenu, menuItemAt, menuOpen, closeTopmost, modalOpen, drawerOpen,
} from './ui.js';
import {
  VIEWS, DATA_VIEWS, sidebarHTML, podsView, deploymentsView, contextsView, noContextView, agoText,
} from './views.js';
import { openLogs, openScale, logsControlChanged, scaleInput, scaleClick, currentLogsCommand, currentScaleCommand } from './panels.js';
import { wizardView, isWizardRoute, wizardClick, wizardInput } from './wizards.js';
import { PAGES } from './pages.js';
import './dashboard.js';
import './namespaces.js';
import './autoscaling.js';
import './helm.js';
import './resilience.js';
import './configmaps.js';
import {
  openDeployForm, openImageForm, openRestartForm, openEnvForm, openAutoscaleForm,
  formsClick, formsInput, formsChange,
} from './forms.js';

const REFRESH_MS = 10000;

const S = {
  app: null, // /api/state
  ctx: null, // the context this console is looking at
  ns: '_all', // namespace, or "_all"
  view: 'pods',
  q: '',
  wide: false,
  problems: false,
  load: { status: 'loading' },
};

let loadGeneration = 0;

/* ---------- helpers ---------- */

const isDataView = (view) => DATA_VIEWS.includes(view) || !!(PAGES[view] && PAGES[view].load);
const isScoped = (view) => DATA_VIEWS.includes(view) || !!(PAGES[view] && PAGES[view].scoped);

const contextInfo = (name) => S.app.contexts.find((c) => c.name === name);
const isProduction = () => !!(S.ctx && contextInfo(S.ctx) && contextInfo(S.ctx).production);
const defaultNamespace = (name) => (contextInfo(name) && contextInfo(name).namespace) || 'default';

function chooseContext(name, namespace) {
  S.ctx = name;
  S.ns = namespace || defaultNamespace(name);
  S.load = { status: 'loading' };
  store.set('k8scli-context', name);
}

/* ---------- shell ---------- */

function curTheme() {
  const set = document.documentElement.getAttribute('data-theme');
  return set || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
}

function renderTopbar() {
  const dark = curTheme() === 'dark';
  $('#topbar').innerHTML = `
    <button class="btn btn-ghost btn-icon only-mobile" data-act="open-nav" aria-label="Open the menu">${icon('menu', 'icon icon-lg')}</button>
    <button class="btn btn-ghost btn-icon only-desktop" data-act="collapse" aria-label="Collapse the sidebar">${icon('panel', 'icon icon-lg')}</button>
    <nav class="crumbs" id="crumbs" aria-label="Breadcrumb"></nav>
    <span class="grow"></span>
    ${S.app && S.app.readOnly ? `<span class="badge b-neutral" title="Scaling and other changes are disabled">${icon('eye')}Read-only</span>` : ''}
    <label class="search"><span class="sr-only">Search</span>${icon('search')}
      <input id="q" class="field" placeholder="Search by name" autocomplete="off" value="${esc(S.q)}"><kbd class="kbd">/</kbd></label>
    <button id="ctx-btn" class="btn" data-act="ctx-menu" aria-haspopup="menu"></button>
    <button class="btn btn-ghost btn-icon" data-act="theme" aria-label="${dark ? 'Switch to the light theme' : 'Switch to the dark theme'}" title="${dark ? 'Light theme' : 'Dark theme'}">${icon(dark ? 'sun' : 'moon', 'icon icon-lg')}</button>`;
  updateTopbar();
}

function updateTopbar() {
  const button = $('#ctx-btn');
  if (!button) return;
  if (S.ctx) {
    const prod = isProduction();
    button.innerHTML = `<span class="dot ${prod ? 'dot-err' : 'dot-ok'}"></span><span class="mono trunc ctx-name">${esc(S.ctx)}</span>${icon('down', 'icon tone-muted')}`;
  } else {
    button.innerHTML = `<span class="dot"></span><span>No context</span>${icon('down', 'icon tone-muted')}`;
  }

  const view = VIEWS[S.view];
  const scope = S.view === 'contexts' ? 'kubeconfig' : (isScoped(S.view) ? (S.ns === '_all' ? 'all namespaces' : S.ns) : view.group);
  $('#crumbs').innerHTML = `${S.ctx ? `<span class="mono trunc">${esc(S.ctx)}</span>${icon('right', 'icon')}` : ''}
    <span class="trunc">${esc(scope)}</span>${icon('right', 'icon')}<strong>${esc(view.title)}</strong>`;

  $('#banner').innerHTML = isProduction() && !isWizardRoute(S.view)
    ? `<div class="banner">${icon('warn')}<span><strong>Production context.</strong> Changes ask you to type the context name first, like the CLI's confirmation.</span></div>` : '';
}

function render() {
  $('#sidebar').innerHTML = sidebarHTML(S);
  updateTopbar();

  const main = $('#main');
  const scrollTop = main.scrollTop;
  let html;
  if (!S.app) html = '';
  else if (isWizardRoute(S.view)) html = wizardView(S.view, S);
  else if (S.view === 'contexts') html = contextsView(S);
  else if (!S.ctx) html = noContextView();
  else if (PAGES[S.view]) html = PAGES[S.view].html(S);
  else html = S.view === 'pods' ? podsView(S) : deploymentsView(S);
  main.innerHTML = `<div class="page">${html}</div>`;
  main.scrollTop = scrollTop;
}

/* ---------- data ---------- */

async function load({ silent = false } = {}) {
  if (!isDataView(S.view) || !S.ctx) return;
  const view = S.view;
  const generation = ++loadGeneration;
  if (!silent) {
    S.load = { status: 'loading' };
    render();
  }
  try {
    if (PAGES[view] && PAGES[view].load) {
      const data = await PAGES[view].load(S);
      if (generation !== loadGeneration) return;
      S.load = { status: 'ready', data, updatedAt: Date.now() };
      render();
      return;
    }
    const [overview, list] = await Promise.all([
      api.overview(S.ctx),
      view === 'pods' ? api.pods(S.ctx, S.ns) : api.deployments(S.ctx, S.ns),
    ]);
    if (generation !== loadGeneration) return;
    S.load = { status: 'ready', overview, list, updatedAt: Date.now() };
  } catch (error) {
    if (generation !== loadGeneration) return;
    if (silent && S.load.status === 'ready') S.load.refreshError = error.message;
    else S.load = { status: 'error', error: error instanceof ApiError ? error : new ApiError(0, { error: error.message }) };
  }
  render();
}

async function reloadState() {
  try {
    S.app = await api.state();
  } catch (error) {
    toast(esc(error.message), 'warn');
  }
  renderTopbar();
  render();
}

/* ---------- routing ---------- */

function navigate(view) {
  if (location.hash === '#' + view) onRoute();
  else location.hash = view;
}

function onRoute() {
  const wanted = location.hash.slice(1);
  S.view = VIEWS[wanted] ? wanted : (S.app && S.app.contexts.length ? 'dashboard' : 'setup-connect');
  closeMenu();
  document.body.dataset.view = S.view;
  $('#app').dataset.navOpen = 'false';
  $('#scrim').hidden = true;
  if (isDataView(S.view) && S.ctx) {
    S.load = { status: 'loading' };
    render();
    load();
  } else {
    render();
  }
}

function setNav(open) {
  $('#app').dataset.navOpen = String(open);
  $('#scrim').hidden = !open;
}

/* ---------- menus ---------- */

function podMenu(button) {
  const [namespace, name] = button.dataset.id.split('/');
  const pod = S.load.list.find((p) => p.namespace === namespace && p.name === name);
  if (!pod) return;
  openMenu(button, [
    { label: 'View logs', icon: 'term', run: () => openLogs(S, pod) },
    { label: 'Copy name', icon: 'copy', run: () => copyText(pod.name) },
    { label: 'Copy logs command', icon: 'copy', run: () => copyText(cliFor(S, `k8scli logs ${pod.name} -n ${pod.namespace}`)) },
  ]);
}

function deploymentMenu(button) {
  const [namespace, name] = button.dataset.id.split('/');
  const dep = S.load.list.find((d) => d.namespace === namespace && d.name === name);
  if (!dep) return;
  const refresh = () => load({ silent: true });
  openMenu(button, [
    { label: 'Scale…', icon: 'scale', run: () => openScale(S, dep, refresh) },
    { label: 'Autoscale…', icon: 'trend', run: () => openAutoscaleForm(S, { deployment: dep }, refresh) },
    { label: 'Update image…', icon: 'rocket', run: () => openImageForm(S, dep, refresh) },
    { label: 'Environment…', icon: 'sliders', run: () => openEnvForm(S, dep, refresh) },
    { label: 'Restart…', icon: 'refresh', run: () => openRestartForm(S, dep, refresh) },
    { sep: true },
    { label: 'Copy scale command', icon: 'copy', run: () => copyText(cliFor(S, `k8scli scale ${dep.name} ${dep.desired} -n ${dep.namespace}`)) },
  ]);
}

async function makeDefault(name) {
  try {
    await api.setDefault(name);
    await reloadState();
    toast(`<span class="mono">${esc(name)}</span> is now the default context for the CLI.`);
  } catch (error) {
    toast(esc(error.message), 'warn');
  }
}

function contextMenu(button) {
  const name = button.dataset.id;
  openMenu(button, [
    { label: 'Use in this console', icon: 'swap', run: () => useContext(name) },
    { label: 'Make it the CLI default', icon: 'check', disabled: S.app.readOnly, run: () => makeDefault(name) },
    { label: 'Copy "k8scli use"', icon: 'copy', run: () => copyText(`k8scli use ${name}`) },
  ]);
}

function topbarContextMenu(button) {
  const items = [{ header: 'Context' }].concat(S.app.contexts.map((c) => ({
    html: `<span class="mono">${esc(c.name)}</span>${c.production ? ' <span class="badge b-err">production</span>' : ''}`,
    label: c.name, check: c.name === S.ctx, run: () => useContext(c.name),
  })));
  items.push({ sep: true }, { label: 'Manage contexts', icon: 'server', run: () => navigate('contexts') });
  openMenu(button, items, 'left');
}

function useContext(name, namespace) {
  chooseContext(name, namespace);
  renderTopbar();
  if (isDataView(S.view)) load();
  else render();
}

/* ---------- events ---------- */

const rowMenus = { pod: podMenu, dep: deploymentMenu, ctx: contextMenu };

const app = {
  S,
  navigate,
  rerender: render,
  reloadState,
  useContext,
  refresh: () => load({ silent: true }),
  // Jump to another page scoped to one namespace (used by the namespaces page).
  setNamespace(namespace, view) {
    S.ns = namespace;
    navigate(view || S.view);
    if (!view || view === S.view) load();
  },
  scrollTop: () => { $('#main').scrollTop = 0; },
};

document.addEventListener('click', async (event) => {
  const item = event.target.closest('[data-mi]');
  if (item) {
    const entry = menuItemAt(Number(item.dataset.mi));
    closeMenu();
    if (entry && entry.run && !entry.disabled) entry.run();
    return;
  }

  const el = event.target.closest('[data-act]');
  if (!el) {
    if (!event.target.closest('#menu')) closeMenu();
    if (event.target.id === 'overlay') closeTopmost();
    if (event.target.id === 'scrim') setNav(false);
    return;
  }

  const act = el.dataset.act;
  const arg = el.dataset.arg;
  if (act !== 'row-menu' && act !== 'ctx-menu') closeMenu();

  if (await wizardClick(act, el, app)) return;
  if (scaleClick(act, arg)) return;
  if (formsClick(act, el, app)) return;
  if (PAGES[S.view] && PAGES[S.view].onClick && await PAGES[S.view].onClick(act, el, app)) return;

  switch (act) {
    case 'nav': navigate(arg); break;
    case 'deploy-new': openDeployForm(S, () => load({ silent: true })); break;
    case 'collapse': {
      const root = $('#app');
      root.dataset.collapsed = root.dataset.collapsed === 'true' ? 'false' : 'true';
      break;
    }
    case 'open-nav': setNav(true); break;
    case 'theme': {
      const next = curTheme() === 'dark' ? 'light' : 'dark';
      document.documentElement.setAttribute('data-theme', next);
      store.set('k8scli-theme', next);
      renderTopbar();
      break;
    }
    case 'ctx-menu': topbarContextMenu(el); break;
    case 'row-menu': (rowMenus[el.dataset.kind] || (() => {}))(el); break;
    case 'use-ctx': useContext(arg); break;
    case 'refresh':
    case 'retry': load(); break;
    case 'problems': S.problems = !S.problems; render(); break;
    case 'ns-all': S.ns = '_all'; load(); break;
    case 'clear-filters': {
      S.q = '';
      S.problems = false;
      const search = $('#q');
      if (search) search.value = '';
      render();
      break;
    }
    case 'scale': {
      const [namespace, name] = el.dataset.id.split('/');
      const dep = S.load.list.find((d) => d.namespace === namespace && d.name === name);
      if (dep) openScale(S, dep, () => load({ silent: true }));
      break;
    }
    case 'copy': copyText(arg); break;
    case 'copy-logs': copyText(currentLogsCommand(S)); break;
    case 'copy-scale': copyText(currentScaleCommand()); break;
    case 'close-panel': closeTopmost(); break;
    default: break;
  }
});

document.addEventListener('input', (event) => {
  const { id } = event.target;
  if (id === 'q') {
    S.q = event.target.value;
    render();
    return;
  }
  if (scaleInput(id, event.target)) return;
  if (formsInput(event.target, app)) return;
  if (PAGES[S.view] && PAGES[S.view].onInput && PAGES[S.view].onInput(event.target, app)) return;
  wizardInput(event.target, app);
});

document.addEventListener('change', (event) => {
  const { id } = event.target;
  if (id === 'ns-select') { S.ns = event.target.value; load(); }
  else if (id === 'wide-toggle') { S.wide = event.target.checked; render(); }
  else if (formsChange(event.target, app)) return;
  else if (PAGES[S.view] && PAGES[S.view].onChange && PAGES[S.view].onChange(event.target, app)) return;
  else logsControlChanged(S, id, event.target);
});

document.addEventListener('keydown', (event) => {
  if (event.key === 'Escape') {
    closeMenu();
    setNav(false);
    closeTopmost();
  } else if (event.key === '/' && !/INPUT|SELECT|TEXTAREA/.test(document.activeElement.tagName)) {
    const search = $('#q');
    if (search) { event.preventDefault(); search.focus(); }
  }
});

window.addEventListener('hashchange', onRoute);
window.addEventListener('resize', closeMenu);
$('#main').addEventListener('scroll', closeMenu);

/* ---------- timers ---------- */

setInterval(() => {
  const stamp = $('#updated-ago');
  if (stamp && S.load.status === 'ready') stamp.textContent = `Updated ${agoText(S.load.updatedAt)}`;
}, 1000);

// Refresh quietly, but never under someone's hands: skip while a menu, dialog or
// form control is in use, or while the tab is in the background.
setInterval(() => {
  const active = document.activeElement;
  const typing = active && /INPUT|SELECT|TEXTAREA/.test(active.tagName) && $('#main').contains(active);
  if (document.hidden || menuOpen() || modalOpen() || drawerOpen() || typing) return;
  if (S.load.status === 'ready') load({ silent: true });
}, REFRESH_MS);

/* ---------- boot ---------- */

async function boot() {
  installIcons();
  $('#sidebar').innerHTML = sidebarHTML(S);
  renderTopbar();

  try {
    S.app = await api.state();
  } catch (error) {
    const unauthorized = error.status === 401;
    $('#main').innerHTML = `<div class="page"><div class="alert alert-err">${icon('warn', 'icon icon-lg')}<div>
      <strong>${unauthorized ? 'Your session ended' : "Couldn't reach the console"}</strong>
      <p>${unauthorized ? 'Open the link printed by <span class="mono">k8scli serve</span> again.' : esc(error.message)}</p></div></div></div>`;
    return;
  }

  // Reopen the last context, except a production one: landing there by accident is
  // worse than picking it again.
  const remembered = store.get('k8scli-context');
  const names = S.app.contexts.map((c) => c.name);
  const restorable = names.includes(remembered) && !S.app.contexts.find((c) => c.name === remembered).production;
  const initial = restorable ? remembered : (S.app.currentContext || names[0] || null);
  if (initial) chooseContext(initial);

  renderTopbar();
  onRoute();
}

boot();
