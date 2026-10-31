// Apps (Helm): the releases in the cluster, a catalog of reviewed apps and, when the
// server allows it, a search of Artifact Hub (ADR 0004 and its amendment).
// A catalog entry decides the chart and the form only offers its fields. A chart found
// through search is a community chart: the server fetches it by id, never from a URL the
// browser sends, and the review has to be read and confirmed by typing the chart name.
// Install, upgrade and uninstall go through confirmChange(), which previews the
// request with a real Helm dry run before anything is applied.

import { api } from './api.js';
import { $, esc, icon, badge, TONE, toast, cliFor, showModal, setModalHTML, hideModal, modalOpen, showDrawer, hideDrawer, drawerOpen } from './ui.js';
import { registerPage } from './pages.js';
import { pageHead, refreshControls, emptyState, errorState, namespaceSelect, matches } from './views.js';
import { cardHead } from './dashboard.js';
import { confirmChange } from './forms.js';

const DRAWER_LABEL = 'Pod logs'; // the label index.html gives the drawer; restored on close
const MAX_RELEASE_NAME = 53; // Helm's limit
const NAME = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const CONTROL = /[\u0000-\u001f\u007f]/;
const MAX_TEXT = 1024;
const MAX_LISTED = 200;
const MAX_SETTINGS = 20;
const MAX_SETTING_VALUE = 1024;
const SETTING_PATH = /^[A-Za-z0-9_-]+(\[\d+\])*(\.[A-Za-z0-9_-]+(\[\d+\])*)*$/;
const HUB_DEBOUNCE_MS = 400;
const HUB_MIN_CHARS = 2;
const MANAGED_TIP = 'Installed with the Helm CLI or another tool. The console only changes releases it installed itself, so use the Helm CLI for this one.';

let form = null; // the install or upgrade form: { mode: install|upgrade|hub, entry, S, app, name, namespace, createNs, values, touched, handedOver }
let drawer = null; // the release open in the drawer: { S, id, detail }
let lastS = null; // the app state, so partial updates can redraw one block without a full render
let cat = { q: '', category: 'all' }; // the local catalog filter
// The Artifact Hub search. Kept at module level: the page is rebuilt on every refresh, the search must survive it.
let hub = { q: '', page: 1, items: [], hasMore: false, status: 'idle', error: null, seq: 0, timer: null, controller: null, moreLoading: false, moreError: null };

/* ---------- small helpers ---------- */

// Only plain https links from the catalog become links; anything else is dropped.
const safeHref = (url) => (typeof url === 'string' && url.startsWith('https://') ? url : '');
const fieldId = (key) => `hm-f-${String(key).replace(/[^a-zA-Z0-9_-]/g, '_')}`;
const releaseId = (r) => `${r.namespace}/${r.name}`;
const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;

// The detail endpoint has no "health", so derive the tone from Helm's status words.
function statusTone(status) {
  if (status === 'deployed') return 'ok';
  if (status === 'failed') return 'err';
  if (/^pending|uninstalling/.test(status)) return 'warn';
  return 'neutral';
}

const managedBadge = (managed) => (managed ? badge('Managed by k8scli', 'accent') : badge('Installed elsewhere', 'neutral'));

// The table is already wide, so it uses short words; the full wording is the tooltip.
const managedChip = (managed, community) => (managed
  ? `<span class="badge b-accent" title="Installed by this console${community ? ' from Artifact Hub' : ''}, so it can upgrade and uninstall it">Managed</span>`
  : `<span class="badge b-neutral" title="${esc(MANAGED_TIP)}">Elsewhere</span>`);

// Releases the console installed without a catalog entry came from Artifact Hub (ADR 0004).
const isCommunity = (r) => !!r.managed && !r.catalogId;

function helmBar(S, command, extra = '') {
  const full = cliFor(S, command);
  return `<div class="cli-bar">
    <span class="muted small nowrap">Helm equivalent</span>
    <code class="cli grow">${esc(full)}</code>
    ${extra ? `<span class="muted small tnum hide-sm">${esc(extra)}</span>` : ''}
    <button class="btn btn-ghost btn-icon" data-act="copy" data-arg="${esc(full)}" aria-label="Copy command">${icon('copy')}</button>
  </div>`;
}

const scrollTo = (id) => { const el = $(id); if (el) el.scrollIntoView({ block: 'start' }); };

/* ---------- the page ---------- */

// All namespaces at once, so the catalog can tell what is already installed; the
// table filters by scope. A user who may not list Secrets cluster-wide still gets
// the namespace they are looking at.
async function listReleases(S) {
  try {
    return { all: true, list: await api.helmReleases(S.ctx, '_all') };
  } catch (error) {
    // Only a permission problem is worth a second try; a connection error would just fail twice.
    if (S.ns === '_all' || error.status !== 403) throw error;
    return { all: false, list: await api.helmReleases(S.ctx, S.ns) };
  }
}

const inScope = (S, r) => S.ns === '_all' || r.namespace === S.ns;

function releaseRow(S, r, all) {
  const id = esc(releaseId(r));
  const actions = r.managed
    ? `<button class="btn" data-act="hm-upgrade" data-id="${id}">${icon('refresh')}Upgrade</button>
       <button class="btn btn-ghost btn-icon" data-act="hm-uninstall" data-id="${id}" aria-label="Uninstall ${esc(r.name)}" title="Uninstall this app">${icon('trash')}</button>`
    : `<span class="hm-tip" title="${esc(MANAGED_TIP)}"><button class="btn" disabled aria-disabled="true">${icon('refresh')}Upgrade</button></span>
       <span class="hm-tip" title="${esc(MANAGED_TIP)}"><button class="btn btn-ghost btn-icon" disabled aria-disabled="true" aria-label="Uninstall ${esc(r.name)} (not allowed here)">${icon('trash')}</button></span>`;
  return `<tr>
    ${all ? `<td class="muted">${esc(r.namespace)}</td>` : ''}
    <td><button class="btn link-btn hm-link mono strong" data-act="hm-details" data-id="${id}" title="Show details, notes and history">${esc(r.name)}</button></td>
    <td class="mono">${esc(r.chart)} <span class="muted">${esc(r.chartVersion)}</span></td>
    <td class="mono muted">${esc(r.appVersion || '–')}</td>
    <td>${badge(r.status, TONE[r.health] || 'neutral')}</td>
    <td class="tnum">${r.revision}</td>
    <td class="muted tnum">${esc(r.updated)}</td>
    <td>${managedChip(r.managed, isCommunity(r))}</td>
    <td class="act"><div class="row g1 end">${actions}</div></td></tr>`;
}

function releasesTable(S, rows) {
  const all = S.ns === '_all';
  const columns = [...(all ? ['Namespace'] : []), 'Release', 'Chart', 'App', 'Status', 'Revision', 'Updated', 'Managed'];
  return `<div class="scroll-x"><table class="tbl">
    <thead><tr>${columns.map((c) => `<th scope="col">${c}</th>`).join('')}<th class="act" scope="col"><span class="sr-only">Actions</span></th></tr></thead>
    <tbody>${rows.map((r) => releaseRow(S, r, all)).join('')}</tbody></table></div>`;
}

function skeletonTable(columns) {
  const widths = ['w3', 'w2', 'w1', 'w2', 'w1', 'w1', 'w2', 'w2', 'w2'];
  let rows = '';
  for (let r = 0; r < 3; r++) {
    rows += '<tr>';
    for (let c = 0; c < columns; c++) rows += `<td><span class="skel ${widths[(c + r) % widths.length]}"></span></td>`;
    rows += '</tr>';
  }
  return `<div class="scroll-x"><table class="tbl"><thead><tr>${'<th></th>'.repeat(columns)}</tr></thead><tbody>${rows}</tbody></table></div>`;
}

function installedCard(S) {
  const L = S.load;
  const all = S.ns === '_all';
  const needle = S.q.trim().toLowerCase();
  const names = L.status === 'ready' ? L.data.namespaces : [];
  const toolbar = `<div class="toolbar">${namespaceSelect(S, names)}<span class="grow"></span>
    <span class="muted small">Release history lives in Secrets in the release's namespace, like the Helm CLI.</span></div>`;

  let body = '';
  let extra = '';
  if (L.status === 'loading') {
    body = skeletonTable(all ? 9 : 8);
  } else if (L.status === 'error') {
    body = errorState(L.error, 'apps');
  } else {
    const scoped = L.data.releases.list.filter((r) => inScope(S, r));
    const rows = scoped.filter((r) => matches(needle, r.name, r.chart, r.namespace));
    extra = `${plural(rows.length, 'release', 'releases')}`;
    if (!scoped.length) {
      body = emptyState(all ? 'No apps installed in this cluster' : `No apps installed in "${S.ns}"`,
        'Pick an app from the catalog below. You will see everything it creates before it is applied.', 'Browse the catalog', 'hm-catalog');
    } else if (!rows.length) {
      body = emptyState('No releases match', 'Clear the search to see the rest.', 'Clear filters', 'clear-filters');
    } else {
      body = releasesTable(S, rows);
    }
  }
  return `<div class="card clip" id="hm-installed">${cardHead('Installed apps')}${toolbar}${body}${helmBar(S, `helm list${all ? ' -A' : ` -n ${S.ns}`}`, extra)}</div>`;
}

/* ---------- catalog ---------- */

const categoryOf = (entry) => entry.category || 'Other';
const cardId = (id) => `hm-card-${String(id).replace(/[^a-zA-Z0-9_-]/g, '_')}`;

function catalogCard(entry, installed, recommended) {
  const docs = safeHref(entry.docs);
  return `<article class="card hm-card${recommended ? ' hm-recommended' : ''}" id="${cardId(entry.id)}">
    <div class="hm-card-top">
      <span class="hm-card-icon">${icon('package')}</span>
      <div class="grow"><h3>${esc(entry.name)}</h3><div class="muted small">${esc(categoryOf(entry))}</div></div>
    </div>
    <p class="hm-card-desc">${esc(entry.description)}</p>
    ${recommended ? '<p class="hm-card-note small">Adds CPU and memory to the dashboard and makes autoscaling work.</p>' : ''}
    <div class="hm-card-meta">
      ${recommended ? badge('Recommended', 'accent') : ''}
      <span class="mono small muted">${esc(entry.chart)} ${esc(entry.version)}</span>
      ${entry.clusterScoped ? badge('Cluster-wide', 'warn') : ''}${installed ? badge('Installed', 'ok') : ''}
    </div>
    <div class="hm-card-foot">
      <button class="btn btn-primary" data-act="hm-install" data-id="${esc(entry.id)}">${icon('download')}Install</button>
      ${docs ? `<a class="small hm-docs" href="${esc(docs)}" target="_blank" rel="noopener noreferrer">Docs${icon('external', 'icon icon-inline')}</a>` : ''}
    </div>
  </article>`;
}

function skeletonCards(count = 6) {
  const card = '<div class="card hm-card"><span class="skel w2 tall"></span><span class="skel w3"></span><span class="skel w3"></span><span class="skel w1"></span></div>';
  return `<div class="hm-grid">${card.repeat(count)}</div>`;
}

// The chips, the groups and the cards. It lives in its own container so typing in the filter
// redraws only this block and keeps the caret where it was.
function catalogBodyHTML(S) {
  const L = S.load;
  if (L.status === 'loading') return skeletonCards();
  if (L.status !== 'ready') return '';
  const { catalog, releases } = L.data;
  if (!catalog.length) return '<div class="card"><div class="card-empty"><span>The catalog is empty.</span></div></div>';

  const typed = cat.q.trim().toLowerCase();
  const top = S.q.trim().toLowerCase();
  const matching = catalog.filter((e) => matches(typed, e.name, e.description, e.chart, categoryOf(e)) && matches(top, e.name, e.description, e.chart, categoryOf(e)));
  const counts = new Map();
  for (const e of matching) counts.set(categoryOf(e), (counts.get(categoryOf(e)) || 0) + 1);
  const categories = [...new Set(catalog.map(categoryOf))].sort((a, b) => a.localeCompare(b));
  if (cat.category !== 'all' && !categories.includes(cat.category)) cat.category = 'all';

  const chip = (value, label, count) => `<button class="chip" data-act="hm-cat" data-arg="${esc(value)}" aria-pressed="${cat.category === value}">${esc(label)} <span class="hm-count tnum">${count}</span></button>`;
  const shown = matching
    .filter((e) => cat.category === 'all' || categoryOf(e) === cat.category)
    .sort((a, b) => categoryOf(a).localeCompare(categoryOf(b)) || a.name.localeCompare(b.name));
  const chips = `<div class="hm-chips-row"><div class="hm-chips" role="group" aria-label="Filter by category">${chip('all', 'All', matching.length)}${categories.map((c) => chip(c, c, counts.get(c) || 0)).join('')}</div>
    <span class="muted small tnum nowrap">${shown.length} of ${catalog.length} apps</span></div>`;

  const cardFor = (e) => {
    const installed = releases.list.some((r) => r.catalogId === e.id);
    // Nothing with this chart anywhere in the cluster: worth pointing out for metrics-server.
    const present = releases.list.some((r) => r.chart === e.chart);
    return catalogCard(e, installed, e.id === 'metrics-server' && !present);
  };

  if (!shown.length) return `${chips}${emptyState('No apps match', 'Clear the filters to see the whole catalog.', 'Clear filters', 'hm-clear-catalog')}`;

  const groups = new Map();
  for (const e of shown) groups.set(categoryOf(e), [...(groups.get(categoryOf(e)) || []), e]);
  if (cat.category !== 'all' || groups.size < 2) return `${chips}<div class="hm-grid">${shown.map(cardFor).join('')}</div>`;
  return `${chips}${[...groups].map(([name, list]) => `<div class="hm-group">
    <h3 class="hm-group-title">${esc(name)} <span class="muted tnum">${list.length}</span></h3>
    <div class="hm-grid">${list.map(cardFor).join('')}</div></div>`).join('')}`;
}

function updateCatalog() {
  const box = $('#hm-catalog-body');
  if (box && lastS) box.innerHTML = catalogBodyHTML(lastS);
}

function catalogSection(S) {
  const L = S.load;
  if (L.status === 'error') return ''; // the page error above already says why nothing loaded
  const ready = L.status === 'ready';
  const hubOn = ready && L.data.capabilities.search;
  const total = ready ? L.data.catalog.length : 0;
  return `<section id="hm-catalog" class="hm-section">
    <div class="hm-section-head"><h2>Catalog${total ? ` <span class="muted tnum">${total}</span>` : ''}</h2><span class="muted small">Reviewed apps with pinned chart versions</span></div>
    ${ready ? `<label class="hm-filter"><span class="sr-only">Filter the catalog</span>${icon('search')}
      <input id="hm-filter" class="field" placeholder="Filter apps by name, description or chart" value="${esc(cat.q)}" autocomplete="off" spellcheck="false"></label>` : ''}
    <div id="hm-catalog-body" class="hm-section">${catalogBodyHTML(S)}</div>
    ${ready && !hubOn ? `<p class="hm-note muted small">${icon('info')}<span>This console runs in catalog-only mode, so only the apps in this catalog can be installed.</span></p>` : ''}
    <div class="alert alert-info">${icon('info', 'icon icon-lg')}<div class="grow"><strong>How the catalog works</strong>
      <div class="alert-body">Each app uses a fixed chart repository and a pinned, verified chart version, and the console only changes releases it installed itself.${hubOn ? ' Charts you find through Artifact Hub are community charts: the console shows everything they create, and you confirm it yourself.' : ''} Releases made with the Helm CLI are listed, but stay under the Helm CLI.</div></div></div>
  </section>`;
}

/* ---------- Artifact Hub search ---------- */

const artifactHubUrl = (item) => {
  const repo = (item.repository && item.repository.name) || '';
  return repo && item.name ? `https://artifacthub.io/packages/helm/${encodeURIComponent(repo)}/${encodeURIComponent(item.name)}` : '';
};

const stars = (n) => (n > 0 ? `<span class="small tnum muted" title="Stars on Artifact Hub">★ ${Number(n).toLocaleString('en-US')}</span>` : '');

const SEVERITIES = [['critical', 'Critical', 'err'], ['high', 'High', 'err'], ['medium', 'Medium', 'warn'], ['low', 'Low', 'neutral']];

function securityBadges(security) {
  if (!security) return '';
  const parts = SEVERITIES.filter(([key]) => security[key] > 0).map(([key, label, tone]) =>
    `<span class="badge b-${tone} hm-sev hm-sev-${key}" title="${security[key]} ${label.toLowerCase()} severity vulnerabilities in the chart's images, as reported by Artifact Hub">${label} ${security[key]}</span>`);
  return parts.length ? parts.join('') : '<span class="muted small">No vulnerabilities reported</span>';
}

function hubRow(item, installAllowed) {
  const repo = item.repository || {};
  const page = artifactHubUrl(item);
  const publisher = repo.organization || repo.name || '';
  const reason = item.reason || (installAllowed ? 'This chart cannot be installed from here.' : 'Installing from search is turned off on this console.');
  const action = item.inCatalog
    ? `${badge('In catalog', 'ok')}<button class="btn" data-act="hm-show-catalog" data-arg="${esc(item.inCatalog)}">Show in catalog</button>`
    : (item.installable && installAllowed
      ? `<button class="btn btn-primary" data-act="hm-hub-install" data-id="${esc(item.id)}">${icon('download')}Install</button>`
      : `<span class="hm-tip" title="${esc(reason)}"><button class="btn" disabled aria-disabled="true">${icon('download')}Install</button></span>`);
  return `<li class="hm-hit">
    <div class="hm-hit-main">
      <div class="hm-hit-title">
        <strong>${esc(item.displayName || item.name)}</strong>
        ${repo.verifiedPublisher ? badge('Verified publisher', 'ok') : ''}${repo.official ? badge('Official', 'accent') : ''}
        ${item.signed ? badge('Signed', 'neutral') : ''}${item.deprecated ? badge('Deprecated', 'warn') : ''}
      </div>
      <div class="hm-hit-meta small muted">
        <span class="mono">${esc(item.name)} ${esc(item.version)}</span>
        ${item.appVersion ? `<span>app ${esc(item.appVersion)}</span>` : ''}
        ${publisher ? `<span>${esc(publisher)}</span>` : ''}${stars(item.stars)}
        ${item.license ? `<span>${esc(item.license)}</span>` : ''}
        ${page ? `<a href="${esc(page)}" target="_blank" rel="noopener noreferrer">Artifact Hub${icon('external', 'icon icon-inline')}</a>` : ''}
      </div>
      ${item.description ? `<p class="hm-hit-desc">${esc(item.description)}</p>` : ''}
      <div class="hm-hit-sec">${securityBadges(item.security)}</div>
      ${!item.installable && !item.inCatalog ? `<p class="hm-hit-reason small muted">${esc(reason)}</p>` : ''}
    </div>
    <div class="hm-hit-act">${action}</div>
  </li>`;
}

function hubSkeleton() {
  const row = '<li class="hm-hit"><div class="hm-hit-main"><span class="skel w2"></span><span class="skel w3"></span><span class="skel w1"></span></div></li>';
  return `<ul class="hm-results" aria-hidden="true">${row.repeat(3)}</ul>`;
}

function hubError(error) {
  const limited = error.status === 429 || error.code === 'rate_limited';
  return `<div class="alert ${limited ? 'alert-warn' : 'alert-err'}">${icon('warn', 'icon icon-lg')}<div class="grow">
    <strong>${limited ? 'Artifact Hub is busy' : "Couldn't search Artifact Hub"}</strong>
    <p>${limited ? 'Artifact Hub is limiting requests right now. Wait a moment and search again.' : esc(error.message)}</p>
    ${!limited && error.hint ? `<p>${esc(error.hint)}</p>` : ''}
    <div class="row g2 mt2"><button class="btn" data-act="hm-hub-retry">${icon('refresh')}Try again</button></div></div></div>`;
}

function hubResultsHTML() {
  const q = hub.q.trim();
  if (q.length < HUB_MIN_CHARS) {
    return q.length
      ? `<p class="muted small">Type at least ${HUB_MIN_CHARS} characters to search.</p>`
      : '<p class="muted small">Try a name such as redis, postgresql or loki. Results come from artifacthub.io through this console.</p>';
  }
  if (hub.status === 'loading') return hubSkeleton();
  if (hub.status === 'error') return hubError(hub.error);
  if (hub.status !== 'ready') return '';
  if (!hub.items.length) return `<div class="card-empty"><span>No charts found for "${esc(q)}". Try a different word.</span></div>`;

  const installAllowed = !!(lastS && lastS.load.data && lastS.load.data.capabilities.hubInstall);
  const more = hub.moreError
    ? `<div class="alert alert-err">${icon('warn', 'icon icon-lg')}<div class="grow">${esc(hub.moreError.status === 429 ? 'Artifact Hub is busy. Wait a moment and try again.' : hub.moreError.message)}</div></div>` : '';
  const loadMore = hub.hasMore
    ? `<div class="hm-more"><button class="btn" data-act="hm-hub-more"${hub.moreLoading ? ' disabled' : ''}>${hub.moreLoading ? 'Loading…' : 'Load more'}</button></div>` : '';
  return `<ul class="hm-results">${hub.items.map((i) => hubRow(i, installAllowed)).join('')}</ul>${more}${loadMore}
    <p class="muted small">Only Helm charts of type application from verified publishers or official repositories can be installed here.</p>`;
}

function updateHub() {
  const box = $('#hm-hub-results');
  if (box) box.innerHTML = hubResultsHTML();
}

function hubSection(S) {
  const L = S.load;
  if (L.status !== 'ready' || !L.data.capabilities.search) return '';
  return `<section id="hm-hub" class="hm-section">
    <div class="card hm-hub-card">
      <div class="hm-hub-head"><span class="hm-card-icon">${icon('search')}</span>
        <div class="grow"><h2>Search Artifact Hub</h2>
          <p class="muted small">Find any Helm chart. Charts from verified publishers and official repositories can be installed here. Each install shows everything it creates and asks you to confirm.</p></div></div>
      <label class="hm-hub-search"><span class="sr-only">Search Artifact Hub</span>${icon('search')}
        <input id="hm-hub-q" class="field" placeholder="Search charts, for example redis, postgresql or loki" value="${esc(hub.q)}" autocomplete="off" spellcheck="false"></label>
      <div id="hm-hub-results" aria-live="polite">${hubResultsHTML()}</div>
    </div>
  </section>`;
}

// A new request replaces the one in flight: the old answer, if it still arrives, is ignored.
function hubReset() {
  clearTimeout(hub.timer);
  hub.seq += 1;
  if (hub.controller) hub.controller.abort();
  Object.assign(hub, { controller: null, items: [], page: 1, hasMore: false, status: 'idle', error: null, moreLoading: false, moreError: null });
}

function onHubInput(value) {
  hub.q = value;
  hubReset();
  if (value.trim().length >= HUB_MIN_CHARS) {
    hub.status = 'loading'; // show the skeleton during the debounce so the box feels alive
    hub.timer = setTimeout(() => hubSearch(1), HUB_DEBOUNCE_MS);
  }
  updateHub();
}

async function hubSearch(page) {
  const q = hub.q.trim();
  if (q.length < HUB_MIN_CHARS) return;
  const seq = ++hub.seq;
  if (hub.controller) hub.controller.abort();
  const controller = new AbortController();
  hub.controller = controller;
  if (page === 1) { hub.status = 'loading'; hub.error = null; } else { hub.moreLoading = true; hub.moreError = null; }
  updateHub();
  try {
    const result = await api.helmSearch(q, page, controller.signal);
    if (seq !== hub.seq) return;
    const known = new Set(hub.items.map((i) => i.id));
    hub.items = hub.items.concat((result.items || []).filter((i) => !known.has(i.id)));
    hub.page = result.page || page;
    hub.hasMore = !!result.hasMore;
    hub.status = 'ready';
  } catch (error) {
    if (seq !== hub.seq || error.name === 'AbortError') return;
    if (page === 1) { hub.status = 'error'; hub.error = error; } else hub.moreError = error;
  }
  hub.moreLoading = false;
  hub.controller = null;
  updateHub();
}

// "In catalog" results point at the card instead of installing a second copy.
function showInCatalog(id) {
  cat = { q: '', category: 'all' };
  const filter = $('#hm-filter');
  if (filter) filter.value = '';
  updateCatalog();
  const card = $('#' + cardId(id));
  if (!card) return;
  card.scrollIntoView({ block: 'center' });
  card.classList.add('hm-flash');
  setTimeout(() => card.classList.remove('hm-flash'), 2500);
}

/* ---------- install and upgrade form ---------- */

function typedFields() {
  return form.entry.fields || [];
}

// Validation mirrors what the server enforces; the server stays the authority.
function fieldError(fd, value) {
  const label = fd.label || fd.key;
  const text = value === undefined || value === null ? '' : String(value);
  if (fd.type === 'int') {
    const range = fd.min !== undefined && fd.max !== undefined ? ` between ${fd.min} and ${fd.max}` : (fd.min !== undefined ? ` of at least ${fd.min}` : (fd.max !== undefined ? ` of at most ${fd.max}` : ''));
    if (!/^-?\d+$/.test(text.trim())) return `${label} must be a whole number${range}.`;
    const n = Number(text);
    if ((fd.min !== undefined && n < fd.min) || (fd.max !== undefined && n > fd.max)) return `${label} must be a whole number${range}.`;
  } else if (fd.type === 'select') {
    if (!(fd.options || []).includes(text)) return `Pick one of the options for ${label}.`;
  } else if (fd.type !== 'bool') {
    if (text.length > MAX_TEXT) return `${label} is too long (at most ${MAX_TEXT} characters).`;
    if (CONTROL.test(text)) return `${label} cannot contain line breaks or control characters.`;
  }
  return '';
}

// "path=value" lines, one per setting. Scalars only; the server still checks that every path exists
// in the chart's default values, so this only catches what is wrong on its face.
function parseSettings(text) {
  const errors = [];
  const settings = {};
  const lines = String(text || '').split('\n').map((raw, index) => ({ line: raw.trim(), number: index + 1 })).filter((l) => l.line && !l.line.startsWith('#'));
  if (lines.length > MAX_SETTINGS) errors.push(`Use at most ${MAX_SETTINGS} settings.`);
  for (const { line, number } of lines) {
    const at = line.indexOf('=');
    if (at < 1) { errors.push(`Line ${number}: write it as path=value.`); continue; }
    const path = line.slice(0, at).trim();
    const value = line.slice(at + 1).trim();
    if (!SETTING_PATH.test(path)) errors.push(`Line ${number}: "${path.slice(0, 40)}" is not a valid setting path. Use letters, digits, "-", "_" and "." (for example service.type).`);
    else if (path in settings) errors.push(`Line ${number}: ${path} is set twice.`);
    else if (!value) errors.push(`Line ${number}: ${path} has no value.`);
    else if (value.length > MAX_SETTING_VALUE) errors.push(`Line ${number}: the value of ${path} is too long (at most ${MAX_SETTING_VALUE} characters).`);
    else if (CONTROL.test(value)) errors.push(`Line ${number}: the value of ${path} cannot contain control characters.`);
    else if (/^[{[]/.test(value)) errors.push(`Line ${number}: ${path} must be a single value, not a list or an object.`);
    else settings[path] = value;
  }
  return { errors, settings };
}

function formErrors() {
  const f = form;
  const errors = [];
  if (f.mode === 'install' || f.mode === 'hub') {
    const name = f.name.trim();
    if (!name) errors.push('Give the release a name.');
    else if (name.length > MAX_RELEASE_NAME || !NAME.test(name)) errors.push(`The release name uses lowercase letters, digits and "-", starts and ends with a letter or digit, and has at most ${MAX_RELEASE_NAME} characters.`);
    const ns = f.namespace.trim();
    if (!ns) errors.push('Choose a namespace.');
    else if (ns.length > 63 || !NAME.test(ns)) errors.push('The namespace uses lowercase letters, digits and "-", starts and ends with a letter or digit, and has at most 63 characters.');
  }
  for (const fd of typedFields()) {
    const message = fieldError(fd, f.values[fd.key]);
    if (message && !(fd.secret && !f.values[fd.key])) errors.push(message);
  }
  if (f.mode === 'hub') errors.push(...parseSettings(f.settings).errors);
  return errors;
}

function errorsHTML(errors) {
  return form.touched && errors.length ? errors.map((m) => `<li>${esc(m)}</li>`).join('') : '';
}

// Only what the catalog entry declares leaves the browser; secrets only when typed.
function fieldsPayload() {
  const out = {};
  for (const fd of typedFields()) {
    const value = form.values[fd.key];
    if (fd.secret && (value === undefined || value === '')) continue;
    out[fd.key] = String(value ?? '').trim();
  }
  return out;
}

function fieldHTML(fd) {
  const id = fieldId(fd.key);
  const value = form.values[fd.key] ?? '';
  const label = esc(fd.label || fd.key);
  const help = fd.help ? `<small class="muted">${esc(fd.help)}</small>` : '';
  const keyAttr = `data-key="${esc(fd.key)}"`;

  if (fd.type === 'bool') {
    return `<div class="hm-field hm-field-bool"><label class="check"><input type="checkbox" id="${id}" ${keyAttr}${value === 'true' ? ' checked' : ''}> ${label}</label>${help}</div>`;
  }
  let input;
  if (fd.type === 'select') {
    input = `<select id="${id}" ${keyAttr} class="field">${(fd.options || []).map((o) => `<option value="${esc(o)}"${o === value ? ' selected' : ''}>${esc(o)}</option>`).join('')}</select>`;
  } else if (fd.type === 'int') {
    input = `<input id="${id}" ${keyAttr} class="field mono tnum" type="text" inputmode="numeric" pattern="-?[0-9]*" value="${esc(value)}" autocomplete="off">`;
  } else if (fd.secret) {
    const keep = form.mode === 'upgrade' ? 'Leave empty to keep the current value' : '';
    input = `<input id="${id}" ${keyAttr} class="field mono" type="password" value="${esc(value)}" placeholder="${esc(keep)}" autocomplete="new-password" spellcheck="false">`;
  } else {
    input = `<input id="${id}" ${keyAttr} class="field mono" type="text" value="${esc(value)}" autocomplete="off" spellcheck="false">`;
  }
  return `<label class="field-group hm-field"><span>${label}${fd.secret ? ' <span class="muted small">secret</span>' : ''}</span>${input}${help}</label>`;
}

function namespaceHint() {
  const known = form.S.load.data ? form.S.load.data.namespaces : [];
  const ns = form.namespace.trim();
  if (form.mode === 'upgrade' || !known.length || !ns || known.includes(ns) || form.createNs) return '';
  return `The namespace "${ns}" does not exist yet. Tick the box below to create it.`;
}

function formHTML() {
  const f = form;
  const { entry, S } = f;
  const upgrade = f.mode === 'upgrade';
  const community = f.mode === 'hub';
  const docs = safeHref(entry.docs);
  const known = S.load.data ? S.load.data.namespaces : [];
  const errors = formErrors();
  const hint = namespaceHint();

  const identity = upgrade
    ? `<div class="hm-grid-form">
        <label class="field-group"><span>Release</span><input class="field mono" value="${esc(f.name)}" disabled></label>
        <label class="field-group"><span>Namespace</span><input class="field mono" value="${esc(f.namespace)}" disabled></label></div>`
    : `<div class="hm-grid-form">
        <label class="field-group"><span>Release name</span>
          <input id="hm-name" class="field mono" value="${esc(f.name)}" autocomplete="off" spellcheck="false"></label>
        <label class="field-group"><span>Namespace</span>
          <input id="hm-ns" class="field mono" value="${esc(f.namespace)}" list="hm-ns-list" autocomplete="off" spellcheck="false">
          <datalist id="hm-ns-list">${known.map((n) => `<option value="${esc(n)}"></option>`).join('')}</datalist></label></div>
      <p class="small tone-warn" id="hm-ns-hint"${hint ? '' : ' hidden'}>${esc(hint)}</p>
      <label class="check"><input type="checkbox" id="hm-create-ns"${f.createNs ? ' checked' : ''}> Create the namespace if it does not exist</label>`;

  const fields = typedFields();
  const repo = (f.item && f.item.repository) || {};
  const communityNotice = community ? `<div class="alert alert-warn">${icon('warn', 'icon icon-lg')}<div class="grow"><strong>Community chart</strong>
    <div class="alert-body">This chart is not in the reviewed catalog. It comes from ${repo.official ? 'an official repository' : 'a verified publisher'}${repo.organization || repo.name ? ` (${esc(repo.organization || repo.name)})` : ''} on Artifact Hub, but this console has not reviewed it.
      The next step lists every object and image it will create and flags risky ones. Read that list, then type the chart name to confirm.</div></div></div>` : '';
  const settingsBlock = community ? `<div class="hm-settings">
      <button class="btn btn-ghost hm-toggle" data-act="hm-settings" aria-expanded="${f.showSettings}" aria-controls="hm-settings-panel">${icon(f.showSettings ? 'down' : 'right')}Settings (optional)</button>
      <div id="hm-settings-panel"${f.showSettings ? '' : ' hidden'}>
        <label class="field-group"><span>One setting per line, as path=value</span>
          <textarea id="hm-settings" class="field mono hm-area" rows="5" placeholder="replicaCount=2&#10;service.type=ClusterIP" autocomplete="off" spellcheck="false">${esc(f.settings)}</textarea>
          <small class="muted">Text, numbers, true or false only, up to ${MAX_SETTINGS} lines. Each path must exist in the chart's default values.</small></label>
      </div></div>` : '';
  return `
    <div class="modal-head">
      <div class="grow"><h2>${upgrade ? 'Upgrade' : 'Install'} ${esc(entry.name)}</h2>
        <p class="muted small mono">${esc(entry.chart)} ${esc(entry.version)}${docs ? ` · <a href="${esc(docs)}" target="_blank" rel="noopener noreferrer">Docs${icon('external', 'icon icon-inline')}</a>` : ''}</p></div>
      <button class="btn btn-ghost btn-icon" data-act="close-panel" aria-label="Close">${icon('x')}</button>
    </div>
    <div class="modal-body">
      <p>${esc(entry.description)}</p>
      ${entry.clusterScoped ? `<div class="alert alert-warn">${icon('warn', 'icon icon-lg')}<div><strong>This app creates cluster-wide resources</strong>
        <div class="alert-body">Things like ClusterRoles, CRDs or webhooks affect the whole cluster, not only one namespace. The next step lists everything it creates.</div></div></div>` : ''}
      ${communityNotice}
      ${S.app.readOnly ? `<div class="alert alert-info">${icon('eye', 'icon icon-lg')}<div>The console is read-only, so you can preview this ${upgrade ? 'upgrade' : 'install'} but not apply it.</div></div>` : ''}
      ${identity}
      ${community ? settingsBlock : (fields.length ? `<div class="hm-grid-form">${fields.map(fieldHTML).join('')}</div>` : '<p class="muted small">This app has no settings to fill in.</p>')}
      <ul class="hm-errors" id="hm-errors">${errorsHTML(errors)}</ul>
    </div>
    <div class="modal-foot">
      <button class="btn" data-act="close-panel">Cancel</button>
      <button class="btn btn-primary" id="hm-review" data-act="hm-review"${errors.length ? ' disabled' : ''}>Review changes</button>
    </div>`;
}

function onFormClosed() {
  const modal = $('#modal');
  if (modal) modal.classList.remove('hm-wide');
  if (form && form.handedOver) return; // the review dialog took over; keep what was typed
  form = null;
}

function showForm() {
  showModal(formHTML(), onFormClosed);
  $('#modal').classList.add('hm-wide');
  const first = $('#modal input:not([disabled]), #modal select');
  if (first) first.focus();
}

function redraw() {
  if (!form) return;
  const active = document.activeElement;
  const id = active && active.id;
  const caret = active && active.selectionStart;
  setModalHTML(formHTML());
  const again = id && $('#' + id);
  if (again) {
    again.focus();
    try { if (typeof caret === 'number') again.setSelectionRange(caret, caret); } catch { /* checkboxes and selects have no caret */ }
  }
}

// Typing does not rebuild the dialog: only the error list, the hint and the button change.
function light() {
  if (!form) return;
  const errors = formErrors();
  const box = $('#hm-errors');
  if (box) box.innerHTML = errorsHTML(errors);
  const go = $('#hm-review');
  if (go) go.disabled = errors.length > 0;
  const hint = $('#hm-ns-hint');
  if (hint) {
    const text = namespaceHint();
    hint.textContent = text;
    hint.hidden = !text;
  }
}

function defaultValues(entry) {
  const values = {};
  for (const fd of entry.fields || []) values[fd.key] = fd.secret ? '' : String(fd.default ?? '');
  return values;
}

function openInstall(S, entry, app) {
  const known = S.load.data.namespaces;
  const wanted = entry.defaultNamespace || 'default';
  form = {
    mode: 'install', entry, S, app, name: entry.defaultName || entry.id, namespace: wanted,
    createNs: !!entry.createNamespace || (known.length > 0 && !known.includes(wanted)),
    values: defaultValues(entry), touched: false, handedOver: false,
  };
  showForm();
}

// A DNS label made from a chart name, to suggest a release name and a namespace.
const slug = (name) => String(name).toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, MAX_RELEASE_NAME).replace(/-+$/g, '') || 'app';

function openHubInstall(S, item, app) {
  const known = S.load.data.namespaces;
  const name = slug(item.name);
  const page = artifactHubUrl(item);
  form = {
    mode: 'hub', item, S, app, name, namespace: name,
    createNs: known.length === 0 || !known.includes(name),
    entry: { id: `hub:${item.id}`, name: item.displayName || item.name, chart: item.name, version: item.version, description: item.description || '', docs: page, clusterScoped: false, fields: [] },
    values: {}, settings: '', showSettings: false, touched: false, handedOver: false,
  };
  showForm();
}

// A release from Artifact Hub has no catalog entry: the review (and the typed chart name) is the whole flow.
function startHubUpgrade(S, row, app) {
  confirmChange(S, {
    title: `Upgrade ${row.name}`,
    call: (m) => api.helmUpgrade({ ...m, namespace: row.namespace, name: row.name }),
    confirmPhrase: { phrase: row.chart, label: 'This release came from Artifact Hub, outside the reviewed catalog. Type the chart name to confirm you read the review.' },
    onDone: () => afterChange(S, app, row.namespace, row.name),
  });
}

async function openUpgrade(S, row, app) {
  if (!row.catalogId) { startHubUpgrade(S, row, app); return; }
  const entry = S.load.data.catalog.find((e) => e.id === row.catalogId);
  if (!entry) {
    toast('This app is no longer in the catalog, so the console cannot upgrade it.', 'warn');
    return;
  }
  let detail;
  try {
    detail = await api.helmRelease(S.ctx, row.namespace, row.name);
  } catch (error) {
    toast(esc(error.message), 'warn');
    return;
  }
  const values = defaultValues(entry);
  for (const fd of entry.fields || []) {
    // Secrets are never returned, so they start empty and keep their current value unless retyped.
    if (!fd.secret && detail.fields && detail.fields[fd.key] !== undefined) values[fd.key] = String(detail.fields[fd.key]);
  }
  form = { mode: 'upgrade', entry, S, app, name: row.name, namespace: row.namespace, createNs: false, values, touched: false, handedOver: false };
  showForm();
}

function reopenForm() {
  if (!form) return;
  form.handedOver = false;
  hideModal(); // runs the review dialog's own cleanup before the form comes back
  showForm();
}

function reviewForm() {
  if (!form) return;
  form.touched = true;
  if (formErrors().length) { light(); return; }

  const f = form;
  const snapshot = { name: f.name.trim(), namespace: f.namespace.trim(), createNamespace: f.createNs, fields: fieldsPayload() };
  const upgrade = f.mode === 'upgrade';
  f.handedOver = true;

  if (f.mode === 'hub') {
    const item = f.item;
    const settings = parseSettings(f.settings).settings;
    confirmChange(f.S, {
      title: `Install ${f.entry.name}`,
      // confirmChange adds the typed phrase and the reviewed digest (m.confirmPhrase, m.digest) on the real apply.
      call: (m) => api.helmInstall({
        ...m, source: 'hub', packageId: item.id, version: item.version,
        name: snapshot.name, namespace: snapshot.namespace, createNamespace: snapshot.createNamespace, settings,
      }),
      confirmPhrase: { phrase: item.name, label: 'This chart is not in the reviewed catalog. Type its name to confirm you read the list above.' },
      onDone: () => afterChange(f.S, f.app, snapshot.namespace, snapshot.name),
      onBack: reopenForm,
    });
    return;
  }
  confirmChange(f.S, {
    title: upgrade ? `Upgrade ${snapshot.name}` : `Install ${f.entry.name}`,
    call: (m) => (upgrade
      ? api.helmUpgrade({ ...m, namespace: snapshot.namespace, name: snapshot.name, fields: snapshot.fields })
      : api.helmInstall({ ...m, catalogId: f.entry.id, name: snapshot.name, namespace: snapshot.namespace, createNamespace: snapshot.createNamespace, fields: snapshot.fields })),
    onDone: () => afterChange(f.S, f.app, snapshot.namespace, snapshot.name),
    onBack: reopenForm,
  });
}

// After a real change: refresh, follow the release into its namespace, and show it.
async function afterChange(S, app, namespace, name) {
  form = null;
  if (S.ns !== '_all' && S.ns !== namespace) {
    app.setNamespace(namespace);
    setTimeout(() => scrollTo('#hm-installed'), 500);
  } else {
    await app.refresh();
    scrollTo('#hm-installed');
  }
  if (drawer && drawer.id === `${namespace}/${name}` && drawerOpen()) await loadDrawer();
}

/* ---------- uninstall ---------- */

function startUninstall(S, row, app) {
  confirmChange(S, {
    title: `Uninstall ${row.name}`,
    call: (m) => api.helmUninstall({ ...m, namespace: row.namespace, name: row.name }),
    onDone: () => {
      if (drawer && drawer.id === releaseId(row)) hideDrawer();
      return app.refresh();
    },
  });
}

/* ---------- details drawer ---------- */

function settingsTable(entry, fields) {
  const keys = Object.keys(fields || {});
  const hidden = ((entry && entry.fields) || []).filter((fd) => fd.secret);
  if (!keys.length && !hidden.length) return '<p class="muted small">Default settings.</p>';
  const label = (key) => (((entry && entry.fields) || []).find((fd) => fd.key === key) || {}).label || key;
  return `<table class="tbl hm-mini"><tbody>
    ${keys.map((k) => `<tr><td class="muted">${esc(label(k))}</td><td class="mono">${esc(fields[k])}</td></tr>`).join('')}
    ${hidden.map((fd) => `<tr><td class="muted">${esc(fd.label || fd.key)}</td><td class="muted">Hidden. Secret values are never shown.</td></tr>`).join('')}
  </tbody></table>`;
}

function resourcesList(resources) {
  if (!resources.length) return '<p class="muted small">No resources reported.</p>';
  const shown = resources.slice(0, MAX_LISTED);
  const more = resources.length - shown.length;
  return `<ul class="hm-res">${shown.map((r) => `<li><span class="hm-kind">${esc(r.kind)}</span> <span class="mono">${esc(r.name)}</span>${r.namespace ? ` <span class="muted small">${esc(r.namespace)}</span>` : ''}</li>`).join('')}</ul>
    ${more > 0 ? `<p class="muted small">and ${more} more</p>` : ''}`;
}

function historyTable(history) {
  if (!history.length) return '<p class="muted small">No history.</p>';
  return `<div class="scroll-x"><table class="tbl hm-mini"><thead><tr><th>Revision</th><th>Status</th><th>Chart</th><th>Updated</th><th>Description</th></tr></thead>
    <tbody>${history.map((h) => `<tr><td class="tnum">${h.revision}</td><td>${badge(h.status, statusTone(h.status))}</td><td class="mono">${esc(h.chart)}</td><td class="muted tnum">${esc(h.updated)}</td><td>${esc(h.description)}</td></tr>`).join('')}</tbody></table></div>`;
}

function drawerHTML() {
  const { S, id, detail: d, error } = drawer;
  const head = (title) => `<div class="drawer-head">
      <div class="grow"><div class="muted small">Helm release${d ? ` · ${esc(d.namespace)}` : ''}</div><h2 class="mono trunc">${esc(title)}</h2></div>
      <button class="btn btn-ghost btn-icon" data-act="close-panel" aria-label="Close">${icon('x')}</button></div>`;

  if (error) {
    return `${head(id.split('/')[1])}<div class="drawer-body"><div class="alert alert-err">${icon('warn', 'icon icon-lg')}<div class="grow"><strong>Couldn't load this release</strong>
      <code class="cli block">${esc(error.message)}</code>${error.hint ? `<p>${esc(error.hint)}</p>` : ''}</div></div></div>`;
  }
  if (!d) return `${head(id.split('/')[1])}<div class="drawer-body"><span class="skel w3"></span><span class="skel w2"></span><span class="skel w3"></span></div>`;

  const entry = S.load.data ? S.load.data.catalog.find((e) => e.id === d.catalogId) : null;
  const actions = d.managed
    ? `<div class="row g2 wrap"><button class="btn" data-act="hm-upgrade" data-id="${esc(id)}">${icon('refresh')}Upgrade</button>
        <button class="btn" data-act="hm-uninstall" data-id="${esc(id)}">${icon('trash')}Uninstall</button></div>`
    : `<div class="alert alert-info">${icon('info', 'icon icon-lg')}<div>${esc(MANAGED_TIP)}</div></div>`;

  return `${head(d.name)}
    <div class="drawer-body">
      <div class="row g2 wrap">${badge(d.status, statusTone(d.status))}${managedBadge(d.managed)}${d.managed && !d.catalogId ? badge('From Artifact Hub', 'warn') : ''}
        <span class="mono small muted">${esc(d.chart)} ${esc(d.chartVersion)} · revision ${d.revision}</span></div>
      ${actions}
      <section class="hm-block"><h4>Notes</h4>${d.notes ? `<pre class="hm-notes">${esc(d.notes)}</pre>` : '<p class="muted small">This chart did not print any notes.</p>'}</section>
      <section class="hm-block"><h4>Settings</h4>${settingsTable(entry, d.fields)}</section>
      <section class="hm-block"><h4>Resources <span class="muted small">${(d.resources || []).length}</span></h4>${resourcesList(d.resources || [])}</section>
      <section class="hm-block"><h4>History</h4>${historyTable(d.history || [])}</section>
    </div>`;
}

async function loadDrawer() {
  if (!drawer) return;
  const current = drawer;
  const [namespace, name] = current.id.split('/');
  try {
    current.detail = await api.helmRelease(current.S.ctx, namespace, name);
    current.error = null;
  } catch (error) {
    current.error = error;
  }
  if (drawer === current && drawerOpen()) $('#drawer').innerHTML = drawerHTML();
}

function openDetails(S, id) {
  drawer = { S, id, detail: null, error: null };
  showDrawer(drawerHTML(), () => {
    drawer = null;
    $('#drawer').setAttribute('aria-label', DRAWER_LABEL);
  });
  $('#drawer').setAttribute('aria-label', `Helm release ${id.split('/')[1]}`);
  loadDrawer();
}

/* ---------- registration ---------- */

registerPage('helm', {
  scoped: true,

  async load(S) {
    const [releases, catalog, namespaces, capabilities] = await Promise.all([
      listReleases(S),
      api.helmCatalog(),
      // The selector is a convenience: a user who cannot list namespaces still gets the page.
      api.namespaces(S.ctx).then((list) => list.map((n) => n.name), () => []),
      // An older server has no such endpoint: treat that as catalog-only.
      api.helmCapabilities().then((c) => ({ search: !!c.search, hubInstall: !!c.hubInstall }), () => ({ search: false, hubInstall: false })),
    ]);
    return { releases, catalog, namespaces, capabilities };
  },

  html(S) {
    lastS = S;
    const scope = S.ns === '_all' ? 'all namespaces' : esc(S.ns);
    const head = pageHead('Apps (Helm)',
      `Install common apps in a few clicks. Every install shows what it will create before it applies. Showing ${scope} on <span class="mono">${esc(S.ctx)}</span>.`,
      refreshControls(S));
    return `${head}${installedCard(S)}${hubSection(S)}${catalogSection(S)}`;
  },

  async onClick(act, el, app) {
    if (!act.startsWith('hm-')) return false;
    const S = app.S;
    // A review that was cancelled leaves the form's memory behind; drop it, secrets included.
    if (form && form.handedOver && !modalOpen()) form = null;

    const rows = S.load.data ? S.load.data.releases.list : [];
    const row = el.dataset.id ? rows.find((r) => releaseId(r) === el.dataset.id) : null;

    switch (act) {
      case 'hm-catalog': scrollTo('#hm-catalog'); break;
      case 'hm-install': {
        const entry = S.load.data && S.load.data.catalog.find((e) => e.id === el.dataset.id);
        if (entry) openInstall(S, entry, app);
        break;
      }
      case 'hm-details': if (row) openDetails(S, releaseId(row)); break;
      case 'hm-upgrade': if (row && row.managed) await openUpgrade(S, row, app); break;
      case 'hm-uninstall': if (row && row.managed) startUninstall(S, row, app); break;
      case 'hm-review': reviewForm(); break;
      case 'hm-cat': cat.category = el.dataset.arg; updateCatalog(); break;
      case 'hm-clear-catalog': cat = { q: '', category: 'all' }; { const f = $('#hm-filter'); if (f) f.value = ''; } updateCatalog(); break;
      case 'hm-show-catalog': showInCatalog(el.dataset.arg); break;
      case 'hm-hub-install': {
        const item = hub.items.find((i) => i.id === el.dataset.id);
        if (item && item.installable) openHubInstall(S, item, app);
        break;
      }
      case 'hm-hub-retry': hubSearch(1); break;
      case 'hm-hub-more': if (!hub.moreLoading) hubSearch(hub.page + 1); break;
      case 'hm-settings':
        if (form) { form.showSettings = !form.showSettings; redraw(); }
        break;
      default: return false;
    }
    return true;
  },

  onInput(el) {
    if (!el.id || !el.id.startsWith('hm-')) return false;
    if (el.id === 'hm-hub-q') { onHubInput(el.value); return true; }
    if (el.id === 'hm-filter') { cat.q = el.value; updateCatalog(); return true; }
    if (!form || form.handedOver) return true;
    form.touched = true;

    if (el.id === 'hm-name') form.name = el.value;
    else if (el.id === 'hm-settings') form.settings = el.value;
    else if (el.id === 'hm-ns') form.namespace = el.value;
    else if (el.id === 'hm-create-ns') { form.createNs = el.checked; light(); return true; }
    else if (el.dataset.key !== undefined) form.values[el.dataset.key] = el.type === 'checkbox' ? String(el.checked) : el.value;
    else return true;
    light();
    return true;
  },

  // 'input' already handled every change; swallow 'change' so the global handlers ignore these ids.
  onChange(el) {
    return !!(el.id && el.id.startsWith('hm-'));
  },
});
