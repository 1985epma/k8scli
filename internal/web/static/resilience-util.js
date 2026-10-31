// Pure helpers for the Resilience page: formatting, tones and the few rules the page mirrors from
// the server (confirmation phrases, scope phases). No DOM here, so Node can test them.

export const SEVERITIES = ['high', 'medium', 'low'];
const RANK = { high: 3, medium: 2, low: 1 };

export const sevRank = (severity) => RANK[severity] || 0;
export const sevTone = (severity) => (severity === 'high' ? 'err' : severity === 'medium' ? 'warn' : 'neutral');
export const sevLabel = (severity) => ({ high: 'High', medium: 'Medium', low: 'Low' }[severity] || String(severity || ''));

// The score counts risk signals, so the tone is a hint, never a promise.
export const scoreTone = (score) => (score >= 80 ? 'ok' : score >= 50 ? 'warn' : 'err');

export const count = (value) => (Array.isArray(value) ? value.length : Number(value) || 0);

/* ---------- time ---------- */

const EPOCH_FLOOR = Date.parse('2000-01-01T00:00:00Z');

// Go encodes a zero time as year 1, which means "unknown".
export function validTime(iso) {
  const t = Date.parse(iso);
  return Number.isFinite(t) && t > EPOCH_FLOOR ? t : null;
}

export function humanSeconds(total) {
  if (total === null || total === undefined || !Number.isFinite(total)) return 'unknown';
  const s = Math.max(0, Math.round(total));
  if (s < 1) return '<1s';
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (s < 3600) return `${m}m ${String(s % 60).padStart(2, '0')}s`;
  const h = Math.floor(s / 3600);
  if (s < 86400) return `${h}h ${String(m % 60).padStart(2, '0')}m`;
  return `${Math.floor(s / 86400)}d ${String(h % 24).padStart(2, '0')}h`;
}

// Age of a backup item: from its time when it has one, else from the Go duration (nanoseconds).
export function ageSeconds(item, now = Date.now()) {
  const t = validTime(item && item.time);
  if (t !== null) return Math.max(0, Math.round((now - t) / 1000));
  if (item && typeof item.age === 'number' && item.age > 0) return Math.round(item.age / 1e9);
  return null;
}

// Backups older than a day are worth a look, older than three days are a problem.
export function ageTone(seconds, warnAfter = 24 * 3600, failAfter = 72 * 3600) {
  if (seconds === null) return 'neutral';
  return seconds >= failAfter ? 'err' : seconds >= warnAfter ? 'warn' : 'ok';
}

export function when(iso) {
  const t = validTime(iso);
  return t === null ? '' : new Date(t).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

export function elapsedSeconds(startIso, endIso, now = Date.now()) {
  const start = validTime(startIso);
  if (start === null) return null;
  const end = validTime(endIso);
  return Math.max(0, Math.round(((end === null ? now : end) - start) / 1000));
}

/* ---------- runs ---------- */

export const PHASES = ['prepare', 'apps', 'databases', 'nodes', 'power'];
export const PHASE_TITLE = {
  prepare: 'Prepare (CronJobs)',
  apps: 'Applications',
  databases: 'Databases',
  nodes: 'Nodes',
  power: 'Power (machines or cluster)',
};
export const RISK_TONE = { low: 'ok', high: 'warn', destructive: 'err' };
export const STEP_TONE = { done: 'ok', skipped: 'neutral', failed: 'err', aborted: 'warn', deferred: 'neutral', pending: 'neutral' };
export const RUN_TONE = { succeeded: 'ok', running: 'accent', failed: 'err', aborted: 'warn', 'dry-run': 'neutral' };
export const TERMINAL = ['succeeded', 'failed', 'aborted'];

// The page picks parts of a plan; the engine works with phases.
export const SCOPE_PARTS = [
  { key: 'apps', label: 'Applications and CronJobs', phases: ['prepare', 'apps'], help: 'Scale workloads and pause CronJobs, by tier.', destructive: false },
  { key: 'databases', label: 'Databases', phases: ['databases'], help: 'Hibernate CloudNativePG, scale StatefulSets. Checks backup age first.', destructive: false },
  { key: 'nodes', label: 'Nodes', phases: ['nodes'], help: 'Cordon and drain worker nodes, uncordon them later.', destructive: false },
  { key: 'power', label: 'Power', phases: ['power'], help: 'Stop or start the machines or the cluster through the provider.', destructive: true },
];

export function scopePhases(selection) {
  const phases = [];
  for (const part of SCOPE_PARTS) if (selection[part.key]) phases.push(...part.phases);
  return phases;
}

export const looksLikeProduction = (name) => String(name || '').toLowerCase().includes('prod');

// What the person has to type, mirroring engine.ConfirmPhrase for an explicit set of phases:
// powering down needs "stop <context>", any other change in a production-looking context needs the context name.
export function confirmPhraseFor(context, direction, phases) {
  if (!context) return '';
  if (direction === 'down' && phases.includes('power')) return `stop ${context}`;
  return looksLikeProduction(context) ? context : '';
}

export function groupByPhase(steps) {
  const groups = [];
  const index = new Map();
  for (const step of steps || []) {
    const phase = step.phase || 'other';
    if (!index.has(phase)) {
      index.set(phase, { phase, steps: [] });
      groups.push(index.get(phase));
    }
    index.get(phase).steps.push(step);
  }
  return groups;
}

export function previewSummary(steps) {
  const out = { total: 0, change: 0, done: 0, deferred: 0, destructive: 0 };
  for (const step of steps || []) {
    out.total += 1;
    if (step.status === 'deferred') out.deferred += 1;
    else if (step.status === 'skipped' || step.status === 'done') out.done += 1;
    else out.change += 1;
    if (step.risk === 'destructive') out.destructive += 1;
  }
  return out;
}

/* ---------- plans ---------- */

export const PLAN_FILE = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;

export function yamlTemplate(context) {
  return `# A disaster-recovery plan. Tiers order startup: tier 0 starts first and stops last.
version: 1
name: my-system
context: ${context || 'my-cluster'}

targets:
  rto: 30m
  rpo: 1h

apps:
  - name: api
    namespace: my-namespace
    tier: 1
    health:
      kind: pods-ready
  - name: web
    namespace: my-namespace
    tier: 2
    dependsOn: [api]

# databases:
#   - name: db
#     namespace: my-namespace
#     kind: cnpg          # or statefulset
#     ref: my-db
#     tier: 0
#     maxBackupAge: 24h

# provider:               # only needed to power nodes or the whole cluster
#   kind: kind
#   params:
#     name: my-cluster

safety:
  labOnly: false
`;
}

// Order of a plan going down and coming up, as the engine runs it: apps by tier, then databases by tier,
// then nodes and power; "up" is the exact reverse.
export function planOrder(plan) {
  if (!plan) return { down: [], up: [] };
  const apps = (plan.apps || []).map((a) => ({ kind: 'app', name: a.name, namespace: a.namespace, tier: a.tier || 0 }));
  const dbs = (plan.databases || []).map((d) => ({ kind: 'database', name: d.name, namespace: d.namespace, tier: d.tier || 0, detail: d.kind }));
  const byTierDesc = (list) => [...list].sort((a, b) => b.tier - a.tier || a.name.localeCompare(b.name));
  const down = [{ kind: 'phase', name: 'Pause CronJobs' }, ...byTierDesc(apps), ...byTierDesc(dbs), { kind: 'phase', name: 'Cordon and drain nodes' }];
  if (plan.provider) down.push({ kind: 'phase', name: `Power off (${plan.provider.kind})` });
  const up = [];
  if (plan.provider) up.push({ kind: 'phase', name: `Power on (${plan.provider.kind})` });
  up.push({ kind: 'phase', name: 'Uncordon nodes' });
  up.push(...[...dbs].sort((a, b) => a.tier - b.tier || a.name.localeCompare(b.name)));
  up.push(...[...apps].sort((a, b) => a.tier - b.tier || a.name.localeCompare(b.name)));
  up.push({ kind: 'phase', name: 'Resume CronJobs' });
  return { down, up };
}

/* ---------- reports ---------- */

export const OUTCOME = {
  recovered: { tone: 'ok', label: 'Recovered' },
  'recovered-with-differences': { tone: 'warn', label: 'Recovered with differences' },
  'not-recovered': { tone: 'err', label: 'Not recovered' },
};
export const VERDICT = { pass: { tone: 'ok', label: 'Meets target' }, fail: { tone: 'err', label: 'Misses target' }, unknown: { tone: 'neutral', label: 'No target' } };
export const KIND_TONE = { measured: 'accent', estimated: 'neutral' };

// RTO of the last drills, oldest first, for the small trend chart.
export function rtoTrend(reports, limit = 12) {
  const points = (reports || [])
    .map((r) => ({
      id: r.id,
      at: validTime(r.started || r.finished),
      seconds: r.rto && Number.isFinite(r.rto.seconds) ? r.rto.seconds : null,
      target: r.rto && r.rto.targetSeconds ? r.rto.targetSeconds : null,
      verdict: (r.rto && r.rto.verdict) || r.verdict || 'unknown',
      recovered: r.outcome !== 'not-recovered',
    }))
    .filter((p) => p.seconds !== null)
    .sort((a, b) => (a.at || 0) - (b.at || 0));
  return points.slice(-limit);
}
