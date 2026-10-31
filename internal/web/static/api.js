// Thin client for the console's JSON API. Every error becomes an ApiError that
// keeps the server's message and, when there is one, its hint.

export class ApiError extends Error {
  constructor(status, body) {
    super(body && body.error ? body.error : 'Request failed');
    this.status = status;
    this.code = body && body.code;
    this.hint = (body && body.hint) || '';
  }
}

const query = (params) => {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== '') search.set(key, value);
  }
  const text = search.toString();
  return text ? '?' + text : '';
};

async function request(path, { method = 'GET', body, signal } = {}) {
  const response = await fetch(path, {
    method,
    credentials: 'same-origin',
    signal,
    headers: { 'X-K8scli-Request': '1', ...(body ? { 'Content-Type': 'application/json' } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await response.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { /* not JSON */ }
  if (!response.ok) throw new ApiError(response.status, data || { error: text || response.statusText });
  return data;
}

export const api = {
  state: () => request('/api/state'),
  overview: (context) => request('/api/overview' + query({ context })),
  pods: (context, namespace) => request('/api/pods' + query({ context, namespace })),
  deployments: (context, namespace) => request('/api/deployments' + query({ context, namespace })),
  scale: (body) => request('/api/scale', { method: 'POST', body }),
  setDefault: (name) => request('/api/contexts/default', { method: 'POST', body: { name } }),
  check: (context) => request('/api/check' + query({ context })),

  // Read views (ADR 0003)
  dashboard: (context) => request('/api/dashboard' + query({ context })),
  namespaces: (context) => request('/api/namespaces' + query({ context })),
  configMaps: (context, namespace) => request('/api/configmaps' + query({ context, namespace })),
  configMap: (context, namespace, name) => request('/api/configmap' + query({ context, namespace, name })),
  deployment: (context, namespace, name) => request('/api/deployment' + query({ context, namespace, name })),
  helmCatalog: () => request('/api/helm/catalog'),
  helmCapabilities: () => request('/api/helm/capabilities'),
  helmSearch: (q, page, signal) => request('/api/helm/search' + query({ q, page }), { signal }),
  helmReleases: (context, namespace) => request('/api/helm/releases' + query({ context, namespace })),
  helmRelease: (context, namespace, name) => request('/api/helm/release' + query({ context, namespace, name })),
  // Disaster recovery (ADR 0005). Reads are always available; real runs need --allow-dr-runs.
  drCapabilities: () => request('/api/dr/capabilities'),
  drPlans: () => request('/api/dr/plans'),
  drPlan: (name) => request('/api/dr/plan' + query({ name })),
  drCheck: (context, namespace) => request('/api/dr/check' + query({ context, namespace })),
  drBackups: (context) => request('/api/dr/backups' + query({ context })),
  drDiff: (context, to, namespace) => request('/api/dr/diff' + query({ context, to, namespace })),
  drExportUrl: (context, namespace) => '/api/dr/export' + query({ context, namespace }),
  drRuns: (limit) => request('/api/dr/runs' + query({ limit })),
  drRun: (id) => request('/api/dr/run' + query({ id })),
  drReports: (limit) => request('/api/dr/reports' + query({ limit })),
  drReport: (id) => request('/api/dr/report' + query({ id })),
  autoscalers: (context, namespace) => request('/api/autoscalers' + query({ context, namespace })),

  // Guided write actions. Every body carries { context, dryRun, confirm } plus its own fields,
  // and every answer is a changeResponse { action, summary, changes[], equivalent, dryRun }.
  createNamespace: (body) => request('/api/namespaces', { method: 'POST', body }),
  createConfigMap: (body) => request('/api/configmaps', { method: 'POST', body }),
  setConfigMapKey: (body) => request('/api/configmaps/data', { method: 'POST', body }),
  createDeployment: (body) => request('/api/deployments/create', { method: 'POST', body }),
  setImage: (body) => request('/api/deployments/image', { method: 'POST', body }),
  restartDeployment: (body) => request('/api/deployments/restart', { method: 'POST', body }),
  setEnv: (body) => request('/api/deployments/env', { method: 'POST', body }),
  saveAutoscaler: (body) => request('/api/autoscalers', { method: 'POST', body }),
  deleteAutoscaler: (body) => request('/api/autoscalers/delete', { method: 'POST', body }),
  drSavePlan: (body) => request('/api/dr/plans', { method: 'POST', body }),
  drStartRun: (body) => request('/api/dr/runs', { method: 'POST', body }),
  drAbort: (body) => request('/api/dr/runs/abort', { method: 'POST', body }),
  drResume: (body) => request('/api/dr/runs/resume', { method: 'POST', body }),
  drDrill: (body) => request('/api/dr/drill', { method: 'POST', body }),
  helmInstall: (body) => request('/api/helm/install', { method: 'POST', body }),
  helmUpgrade: (body) => request('/api/helm/upgrade', { method: 'POST', body }),
  helmUninstall: (body) => request('/api/helm/uninstall', { method: 'POST', body }),

  // Returns the raw Response so the caller can read the stream as it arrives.
  async logs(params, signal) {
    const response = await fetch('/api/logs' + query(params), { credentials: 'same-origin', signal });
    if (!response.ok) {
      let data = null;
      try { data = await response.json(); } catch { /* not JSON */ }
      throw new ApiError(response.status, data || { error: response.statusText });
    }
    return response;
  },
};
