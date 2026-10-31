// Setup wizards. Each one is a list of steps; the page shows a stepper, one
// step at a time, and a footer with Back / Next. Progress is kept in memory so
// leaving the page and coming back does not lose it.

import { api } from './api.js';
import { $, esc, icon, toast } from './ui.js';

const RELEASES = 'https://github.com/1985epma/k8scli/releases';
const progress = {};
const DEFAULTS = { completion: () => ({ shell: guessShell() }) };
const stateOf = (id) => (progress[id] ||= { step: 0, d: DEFAULTS[id] ? DEFAULTS[id]() : {} });

/* ---------- building blocks ---------- */

const val = (value, placeholder) => (value && value.trim() ? value.trim() : placeholder);

function cmdBlock(text, label = '') {
  return `<div class="cmd">
    ${label ? `<div class="cmd-label">${esc(label)}</div>` : ''}
    <div class="cmd-row"><pre><code>${esc(text)}</code></pre>
      <button class="btn btn-ghost btn-icon" data-act="copy" data-arg="${esc(text)}" aria-label="Copy command">${icon('copy')}</button></div>
  </div>`;
}

function callout(tone, title, html) {
  const glyph = { info: 'info', ok: 'okcircle', err: 'xcircle', warn: 'warn' }[tone];
  return `<div class="alert alert-${tone}">${icon(glyph, 'icon icon-lg')}<div class="grow"><strong>${esc(title)}</strong>${html ? `<div class="alert-body">${html}</div>` : ''}</div></div>`;
}

function textField(wz, name, label, placeholder, value, help = '') {
  return `<label class="field-group"><span>${esc(label)}</span>
    <input class="field mono" data-wz="${wz}" data-wz-field="${name}" value="${esc(value || '')}" placeholder="${esc(placeholder)}" autocomplete="off" spellcheck="false">
    ${help ? `<small class="muted">${esc(help)}</small>` : ''}</label>`;
}

function selectField(wz, name, label, options, value) {
  return `<label class="field-group"><span>${esc(label)}</span>
    <select class="field" data-wz="${wz}" data-wz-field="${name}">${options.map(([v, l]) =>
      `<option value="${esc(v)}"${value === v ? ' selected' : ''}>${esc(l)}</option>`).join('')}</select></label>`;
}

function choices(wz, field, items, selected) {
  return `<div class="choices" role="radiogroup">${items.map((item) => `
    <button class="choice" role="radio" aria-checked="${selected === item.id}" data-act="wz-choose" data-wz="${wz}" data-field="${field}" data-arg="${item.id}">
      ${icon(item.icon || 'box', 'icon icon-lg')}
      <span class="choice-title">${esc(item.name)}</span>
      <span class="choice-desc muted">${esc(item.desc)}</span>
    </button>`).join('')}</div>`;
}

const external = (href, label) => `<a href="${esc(href)}" target="_blank" rel="noopener noreferrer">${esc(label)}${icon('external', 'icon icon-inline')}</a>`;

// The newest release is unknown to the page, so default to the version running
// here and tell people to check the releases page.
function releaseVersion(S) {
  const match = /^v?(\d+\.\d+\.\d+)/.exec((S.app && S.app.version) || '');
  return match ? match[1] : '0.1.1';
}

/* ---------- connect a cluster ---------- */

const PROVIDERS = [
  { id: 'docker-desktop', name: 'Docker Desktop', desc: 'Kubernetes built into Docker Desktop', icon: 'box' },
  { id: 'minikube', name: 'minikube', desc: 'A local single-node cluster', icon: 'box' },
  { id: 'kind', name: 'kind', desc: 'Kubernetes in Docker containers', icon: 'box' },
  { id: 'eks', name: 'Amazon EKS', desc: 'Managed Kubernetes on AWS', icon: 'server' },
  { id: 'gke', name: 'Google GKE', desc: 'Managed Kubernetes on Google Cloud', icon: 'server' },
  { id: 'aks', name: 'Azure AKS', desc: 'Managed Kubernetes on Azure', icon: 'server' },
  { id: 'other', name: 'Another cluster', desc: 'I already have a kubeconfig file', icon: 'plug' },
];

function credentialsStep(d, S) {
  const wz = 'connect';
  const steps = (items) => `<ol class="steps">${items.map((i) => `<li>${i}</li>`).join('')}</ol>`;

  switch (d.provider) {
    case 'docker-desktop':
      return `<p>Docker Desktop creates the cluster for you. There is no command to run.</p>
        ${steps(['Open <strong>Docker Desktop</strong> and go to <strong>Settings → Kubernetes</strong>.',
          'Turn on <strong>Enable Kubernetes</strong> and choose <strong>Apply &amp; restart</strong>.',
          'Wait until the Kubernetes status in the bottom bar turns green.'])}
        ${callout('info', 'What you get', 'A context named <span class="mono">docker-desktop</span> is added to your kubeconfig.')}`;
    case 'minikube':
      return `<p>Start a local cluster. minikube adds its own context when it finishes.</p>
        ${cmdBlock('minikube start', 'Start the cluster')}
        ${callout('info', 'What you get', 'A context named <span class="mono">minikube</span> in your kubeconfig.')}`;
    case 'kind':
      return `<p>Create a cluster with the name you want.</p>
        <div class="fields">${textField(wz, 'clusterName', 'Cluster name', 'dev', d.clusterName)}</div>
        ${cmdBlock(`kind create cluster --name ${val(d.clusterName, 'dev')}`, 'Create the cluster')}
        ${callout('info', 'What you get', `A context named <span class="mono">kind-${esc(val(d.clusterName, 'dev'))}</span>.`)}`;
    case 'eks':
      return `<p>Fill in your cluster and k8scli builds the command. It writes the cluster into your kubeconfig.</p>
        <div class="fields">
          ${textField(wz, 'clusterName', 'Cluster name', 'my-cluster', d.clusterName)}
          ${textField(wz, 'region', 'AWS region', 'us-east-1', d.region)}
          ${textField(wz, 'profile', 'AWS profile (optional)', 'default', d.profile, 'Leave empty to use your default profile.')}
        </div>
        ${cmdBlock('aws sts get-caller-identity', '1. Check you are signed in to AWS')}
        ${cmdBlock(`aws eks update-kubeconfig --name ${val(d.clusterName, '<cluster-name>')} --region ${val(d.region, '<region>')}${d.profile && d.profile.trim() ? ` --profile ${d.profile.trim()}` : ''}`, '2. Add the cluster to your kubeconfig')}
        ${callout('info', 'You need', 'AWS CLI v2 installed, and an IAM identity that is allowed to call <span class="mono">eks:DescribeCluster</span>. The kubeconfig uses the <span class="mono">aws</span> command to get tokens, so it must stay on your PATH.')}`;
    case 'gke':
      return `<p>Fill in your cluster and k8scli builds the command.</p>
        <div class="fields">
          ${textField(wz, 'clusterName', 'Cluster name', 'my-cluster', d.clusterName)}
          ${textField(wz, 'location', 'Region or zone', 'us-central1', d.location)}
          ${textField(wz, 'project', 'Project ID', 'my-project', d.project)}
        </div>
        ${cmdBlock('gcloud components install gke-gcloud-auth-plugin', '1. Install the auth plugin (once)')}
        ${cmdBlock(`gcloud container clusters get-credentials ${val(d.clusterName, '<cluster-name>')} --location ${val(d.location, '<region-or-zone>')} --project ${val(d.project, '<project-id>')}`, '2. Add the cluster to your kubeconfig')}
        ${callout('info', 'You need', 'The Google Cloud CLI, signed in with <span class="mono">gcloud auth login</span>.')}`;
    case 'aks':
      return `<p>Fill in your cluster and k8scli builds the command.</p>
        <div class="fields">
          ${textField(wz, 'resourceGroup', 'Resource group', 'my-group', d.resourceGroup)}
          ${textField(wz, 'clusterName', 'Cluster name', 'my-cluster', d.clusterName)}
        </div>
        ${cmdBlock('az login', '1. Sign in to Azure')}
        ${cmdBlock(`az aks get-credentials --resource-group ${val(d.resourceGroup, '<resource-group>')} --name ${val(d.clusterName, '<cluster-name>')}`, '2. Add the cluster to your kubeconfig')}
        ${callout('info', 'Azure AD clusters', 'If the cluster uses Azure AD sign-in, also run <span class="mono">kubelogin convert-kubeconfig -l azurecli</span>.')}`;
    default: {
      const paths = ((S.app && S.app.kubeconfigPaths) || []).map((p) => `<span class="mono">${esc(p)}</span>`).join(', ');
      return `<p>k8scli reads the same files as kubectl: the paths in <span class="mono">$KUBECONFIG</span>, or <span class="mono">~/.kube/config</span> when it is not set.</p>
        ${paths ? callout('info', 'This console is reading', paths) : ''}
        ${cmdBlock('mkdir -p ~/.kube && cp /path/to/your-config ~/.kube/config', 'Option 1: put the file in the default place')}
        ${cmdBlock('export KUBECONFIG="$HOME/.kube/config:/path/to/your-config"', 'Option 2: merge several files (restart k8scli serve afterwards)')}
        ${cmdBlock('k8scli serve --kubeconfig /path/to/your-config', 'Option 3: open the console on one specific file')}`;
    }
  }
}

// Pick the context the provider most likely just created.
function guessContext(d, contexts) {
  const find = (test) => contexts.find((c) => test(c.name));
  const guess = {
    'docker-desktop': () => find((n) => n === 'docker-desktop'),
    minikube: () => find((n) => n === 'minikube'),
    kind: () => find((n) => n.startsWith('kind-')),
    eks: () => find((n) => n.includes('eks') || n.startsWith('arn:aws:eks')),
    gke: () => find((n) => n.startsWith('gke_')),
    aks: () => find((n) => n === (d.clusterName || '').trim()),
  }[d.provider];
  const found = guess ? guess() : null;
  return (found || contexts.find((c) => c.current) || contexts[0] || {}).name || null;
}

function verifyStep(d, S) {
  const contexts = S.app.contexts;
  if (!contexts.length) {
    return `${callout('warn', 'No contexts found yet', 'Run the command from the previous step, then scan again.')}
      <div class="row g2 mt3"><button class="btn" data-act="wz-rescan" data-wz="connect">${icon('refresh')}Scan the kubeconfig</button>
      <button class="btn btn-ghost" data-act="wz-goto" data-wz="connect" data-arg="1">Back to the command</button></div>`;
  }
  d.context = d.context && contexts.some((c) => c.name === d.context) ? d.context : guessContext(d, contexts);

  const list = contexts.map((c) => `
    <button class="choice choice-row" role="radio" aria-checked="${d.context === c.name}" data-act="wz-pick-context" data-wz="connect" data-arg="${esc(c.name)}">
      <span class="mono strong grow trunc">${esc(c.name)}</span>
      ${c.production ? '<span class="badge b-err">production</span>' : ''}
      <span class="muted small">${esc(c.cluster)}</span>
    </button>`).join('');

  const check = d.check || { status: 'idle' };
  let result = '';
  if (check.status === 'running') {
    result = callout('info', 'Checking the connection…', 'Asking the API server for its version. This can take a few seconds.');
  } else if (check.status === 'ok') {
    const r = check.data;
    result = callout('ok', `Connected to ${r.context}`,
      `Kubernetes <span class="mono">${esc(r.version)}</span> · ${r.nodesReady} of ${r.nodesTotal} nodes ready · ${r.namespaces.length} namespaces`);
  } else if (check.status === 'fail') {
    const r = check.data;
    result = `<div class="alert alert-err">${icon('xcircle', 'icon icon-lg')}<div class="grow"><strong>Couldn't connect to ${esc(d.context)}</strong>
      <code class="cli block">${esc(r.error)}</code>
      ${r.hint ? `<p>${esc(r.hint)}</p>` : '<p>Check the details above, fix the credentials and try again.</p>'}
      <div class="row g2 mt2"><button class="btn" data-act="wz-goto" data-wz="connect" data-arg="1">Back to the command</button></div></div></div>`;
  }

  return `<p>Pick the context for your cluster, then check that k8scli can reach it. Nothing is changed.</p>
    <div class="choices choices-list" role="radiogroup" aria-label="Contexts">${list}</div>
    <div class="row g2 wrap mt3">
      <button class="btn btn-primary" data-act="wz-check" data-wz="connect"${check.status === 'running' ? ' disabled' : ''}>${check.status === 'ok' ? 'Check again' : 'Check connection'}</button>
      <button class="btn" data-act="wz-rescan" data-wz="connect">${icon('refresh')}Scan the kubeconfig again</button>
    </div>
    <div class="mt3">${result}</div>`;
}

function defaultsStep(d, S) {
  const ctx = S.app.contexts.find((c) => c.name === d.context) || {};
  const namespaces = (d.check && d.check.data && d.check.data.namespaces) || [];
  d.ns = d.ns || ctx.namespace || (namespaces.includes('default') ? 'default' : namespaces[0] || 'default');
  const nsField = namespaces.length
    ? selectField('connect', 'ns', 'Namespace to open first', namespaces.map((n) => [n, n]), d.ns)
    : textField('connect', 'ns', 'Namespace to open first', 'default', d.ns);
  const readOnly = S.app.readOnly;

  return `<p>You are connected. Choose where to start.</p>
    <div class="fields">${nsField}</div>
    <div class="row g2 wrap mt3">
      <button class="btn btn-primary" data-act="wz-open" data-wz="connect">Open ${esc(d.context)} in the console</button>
      <button class="btn" data-act="wz-default" data-wz="connect"${readOnly ? ' disabled title="The console is read-only"' : ''}>Make it the CLI default</button>
    </div>
    ${readOnly ? '<p class="muted small mt2">Read-only mode: the console cannot write your kubeconfig. Use the command below instead.</p>' : ''}
    <h3 class="mt4">Try the same in the terminal</h3>
    ${cmdBlock(`k8scli use ${d.context}`, 'Make it the default context for the CLI')}
    ${cmdBlock(`k8scli pods -n ${d.ns}`, 'List the pods')}
    ${cmdBlock(`k8scli scale <deployment> 3 -n ${d.ns} --dry-run`, 'Preview a scale without changing anything')}`;
}

const connect = {
  title: 'Connect a cluster',
  intro: 'Get credentials for your cluster and check that k8scli can reach it. About two minutes.',
  steps: [
    { title: 'Where does your cluster run?', valid: (d) => !!d.provider, body: (d) => `<p>Pick the closest match. The next step shows the exact commands.</p>${choices('connect', 'provider', PROVIDERS, d.provider)}` },
    { title: 'Get credentials', valid: () => true, body: credentialsStep },
    { title: 'Check the connection', valid: (d) => d.check && d.check.status === 'ok', body: verifyStep },
    { title: 'Choose where to start', valid: () => true, body: defaultsStep },
  ],
};

/* ---------- install the CLI ---------- */

function installStep(d, S) {
  const version = val(d.version, releaseVersion(S));
  const arch = d.arch || 'amd64';
  const versionField = textField('install', 'version', 'Release', releaseVersion(S), d.version,
    `Defaults to the version running here. See ${RELEASES} for newer ones.`);

  switch (d.platform) {
    case 'macos':
      return `<p>Homebrew installs the binary and the zsh completion.</p>
        ${cmdBlock('brew install 1985epma/k8scli/k8scli', 'Install')}
        ${callout('info', 'If Homebrew says "Repository not found"', 'Add the tap first: <span class="mono">brew tap 1985epma/k8scli &amp;&amp; brew update</span>.')}`;
    case 'linux': {
      const kind = d.pkg || 'deb';
      const file = kind === 'binary' ? `k8scli_\${VERSION}_linux_\${ARCH}` : `k8scli_\${VERSION}_linux_\${ARCH}.${kind}`;
      const installCmd = { deb: `sudo dpkg -i "${file}"`, rpm: `sudo rpm -i "${file}"`, binary: `chmod +x "${file}"\nsudo install -m 0755 "${file}" /usr/local/bin/k8scli` }[kind];
      return `<div class="fields">
          ${versionField}
          ${selectField('install', 'arch', 'CPU', [['amd64', 'x86-64 (amd64)'], ['arm64', 'ARM (arm64)']], arch)}
          ${selectField('install', 'pkg', 'Format', [['deb', 'Debian / Ubuntu (.deb)'], ['rpm', 'RHEL / Fedora (.rpm)'], ['binary', 'Single binary']], kind)}
        </div>
        ${cmdBlock(`VERSION=${version}\nARCH=${arch}\nBASE="https://github.com/1985epma/k8scli/releases/download/v\${VERSION}"\n\ncurl -fsSLO "\${BASE}/${file}"\ncurl -fsSLO "\${BASE}/checksums.txt"\nsha256sum --ignore-missing -c checksums.txt\n\n${installCmd}`, 'Download, verify the checksum, install')}
        ${callout('info', 'The checksum must say OK', 'If <span class="mono">sha256sum</span> reports a failure, delete the file and download it again.')}`;
    }
    case 'windows':
      return `<p>Use the installer, which adds k8scli to your PATH, or the portable executable.</p>
        <div class="fields">${versionField}</div>
        <ol class="steps">
          <li>Open ${external(RELEASES, 'the releases page')} and download the <strong>.msi</strong>, or <span class="mono">k8scli_${esc(version)}_windows_amd64.exe</span> for the portable build.</li>
          <li>Run the installer, or put the .exe in a folder on your PATH.</li>
        </ol>
        ${cmdBlock(`Get-FileHash .\\k8scli_${version}_windows_amd64.exe -Algorithm SHA256`, 'PowerShell: compare this hash with the line in checksums.txt')}`;
    case 'docker':
      return `<p>No install needed. The image reads your kubeconfig from a read-only mount.</p>
        ${cmdBlock('docker run --rm -v ~/.kube:/home/nonroot/.kube:ro ghcr.io/1985epma/k8scli:latest pods', 'List pods from a container')}
        ${callout('info', 'The web console in a container', 'Add <span class="mono">-p 127.0.0.1:8484:8484</span> and run <span class="mono">serve --addr 0.0.0.0:8484 --allow-remote</span>. Keep the published port on 127.0.0.1.')}`;
    default:
      return `<p>Build from the source. This needs Go.</p>
        ${cmdBlock('git clone https://github.com/1985epma/k8scli.git\ncd k8scli\nmake build\nmake install', 'Build and install into $GOPATH/bin')}
        ${cmdBlock('export PATH="$PATH:$(go env GOPATH)/bin"', 'If the command is not found')}`;
  }
}

function verifyDownloadStep(d, S) {
  const version = val(d.version, releaseVersion(S));
  return `<p>Releases are signed, so you can prove the file came from this repository's release workflow. <strong>This step is optional</strong> and needs <span class="mono">cosign</span> and the GitHub CLI.</p>
    ${cmdBlock(`cosign verify-blob \\\n  --bundle checksums.txt.sigstore.json \\\n  --certificate-identity-regexp 'https://github.com/1985epma/k8scli/\\.github/workflows/release\\.yml@refs/tags/.*' \\\n  --certificate-oidc-issuer https://token.actions.githubusercontent.com \\\n  checksums.txt`, '1. Verify the signature of the checksum file')}
    ${cmdBlock(`gh attestation verify k8scli_${version}_linux_amd64.deb --repo 1985epma/k8scli`, '2. Verify the build provenance of a file')}
    ${callout('info', 'Homebrew and Docker users', 'Homebrew checks the checksum for you. For the image, run <span class="mono">cosign</span> or <span class="mono">gh attestation verify</span> against <span class="mono">ghcr.io/1985epma/k8scli</span>.')}`;
}

const install = {
  title: 'Install the CLI',
  intro: 'Put k8scli on another machine, or share these steps with a teammate.',
  steps: [
    {
      title: 'Where will you install it?', valid: (d) => !!d.platform,
      body: (d) => `<p>You are already running this console, so k8scli works here. This is for another machine.</p>${choices('install', 'platform', [
        { id: 'macos', name: 'macOS', desc: 'Homebrew', icon: 'download' },
        { id: 'linux', name: 'Linux', desc: '.deb, .rpm or a single binary', icon: 'download' },
        { id: 'windows', name: 'Windows', desc: 'MSI installer or portable .exe', icon: 'download' },
        { id: 'docker', name: 'Docker', desc: 'Run it from a container image', icon: 'box' },
        { id: 'source', name: 'From source', desc: 'Build with Go and make', icon: 'term' },
      ], d.platform)}`,
    },
    { title: 'Install', valid: () => true, body: installStep },
    { title: 'Verify the download', valid: () => true, body: verifyDownloadStep },
    {
      title: 'Check that it works', valid: () => true,
      body: () => `<p>Open a new terminal and run:</p>
        ${cmdBlock('k8scli version', 'Prints the version, the commit and the build date')}
        ${cmdBlock('k8scli contexts', 'Lists the contexts in your kubeconfig')}
        ${callout('info', 'Next', 'No cluster yet? Continue with <strong>Connect a cluster</strong>.')}
        <div class="mt3"><button class="btn btn-primary" data-act="nav" data-arg="setup-connect">Connect a cluster</button></div>`,
    },
  ],
};

/* ---------- shell completion ---------- */

function guessShell() {
  const platform = (navigator.userAgent || '').toLowerCase();
  if (platform.includes('windows')) return 'powershell';
  return platform.includes('mac') ? 'zsh' : 'bash';
}

function completionInstall(d) {
  switch (d.shell) {
    case 'zsh':
      return `${cmdBlock('mkdir -p ~/.zsh/completion\nk8scli completion zsh > ~/.zsh/completion/_k8scli', '1. Write the completion file')}
        ${cmdBlock('fpath=(~/.zsh/completion $fpath)\nautoload -Uz compinit && compinit', '2. Add this to ~/.zshrc (once)')}`;
    case 'bash':
      return `${cmdBlock('mkdir -p ~/.local/share/bash-completion/completions\nk8scli completion bash > ~/.local/share/bash-completion/completions/k8scli', 'Write the completion file')}
        ${callout('info', 'It needs bash-completion', 'Install the <span class="mono">bash-completion</span> package if completion does not load. As an alternative, add <span class="mono">source &lt;(k8scli completion bash)</span> to ~/.bashrc.')}`;
    case 'fish':
      return cmdBlock('mkdir -p ~/.config/fish/completions\nk8scli completion fish > ~/.config/fish/completions/k8scli.fish', 'Write the completion file');
    default:
      return `${cmdBlock("Add-Content $PROFILE 'k8scli completion powershell | Out-String | Invoke-Expression'", 'Load it in every PowerShell session')}
        ${callout('info', 'First time using a profile?', 'Create it with <span class="mono">New-Item -Type File -Force $PROFILE</span>, then run the command above.')}`;
  }
}

const completion = {
  title: 'Shell completion',
  intro: 'Press Tab to complete commands and flags in your shell.',
  steps: [
    {
      title: 'Which shell do you use?', valid: (d) => !!d.shell,
      body: (d) => {
        return `<p>We picked the likely one for this computer. Change it if you use another.</p>${choices('completion', 'shell', [
          { id: 'zsh', name: 'zsh', desc: 'The default on macOS', icon: 'term' },
          { id: 'bash', name: 'bash', desc: 'Most Linux distributions', icon: 'term' },
          { id: 'fish', name: 'fish', desc: 'The friendly shell', icon: 'term' },
          { id: 'powershell', name: 'PowerShell', desc: 'Windows, and PowerShell on any OS', icon: 'term' },
        ], d.shell)}
        ${callout('info', 'Installed with Homebrew or make install-local?', 'zsh completion is already set up. You only need this wizard for other shells.')}`;
      },
    },
    { title: 'Enable it', valid: () => true, body: (d) => `<p>Run these in your terminal.</p>${completionInstall(d)}` },
    {
      title: 'Try it', valid: () => true,
      body: () => `<p>Open a <strong>new</strong> terminal window, then type and press <kbd>Tab</kbd>:</p>
        ${cmdBlock('k8scli lo', 'Completes to "logs"')}
        ${cmdBlock('k8scli po', 'Offers "pods" and "pods-all"')}
        ${callout('ok', 'That is it', 'Completion is generated by k8scli itself, so it always matches the commands you have installed.')}`,
    },
  ],
};

const WIZARDS = { connect, install, completion };
const ROUTES = { 'setup-connect': 'connect', 'setup-install': 'install', 'setup-completion': 'completion' };

/* ---------- page ---------- */

export function wizardView(route, S) {
  const id = ROUTES[route];
  const wizard = WIZARDS[id];
  const { step, d } = stateOf(id);
  const current = wizard.steps[step];
  const last = step === wizard.steps.length - 1;
  const valid = current.valid(d, S);

  const stepper = wizard.steps.map((s, index) => {
    const state = index < step ? 'done' : index === step ? 'current' : 'todo';
    const clickable = index < step ? ` data-act="wz-goto" data-wz="${id}" data-arg="${index}"` : ' disabled';
    return `<li class="step step-${state}"><button${clickable}${index === step ? ' aria-current="step"' : ''}>
      <span class="step-num">${index < step ? icon('check', 'icon') : index + 1}</span><span class="step-title">${esc(s.title)}</span></button></li>`;
  }).join('');

  return `<div class="page-head"><div class="grow"><h1>${esc(wizard.title)}</h1><p class="muted">${esc(wizard.intro)}</p></div></div>
    <div class="card wz" data-wz="${id}">
      <ol class="stepper" aria-label="Steps">${stepper}</ol>
      <div class="wz-body"><h2>${esc(current.title)}</h2>${current.body(d, S)}</div>
      <div class="wz-foot">
        <button class="btn" data-act="wz-back" data-wz="${id}"${step === 0 ? ' disabled' : ''}>Back</button>
        <span class="grow muted small">Step ${step + 1} of ${wizard.steps.length}</span>
        ${last
          ? `<button class="btn btn-primary" data-act="wz-finish" data-wz="${id}">Finish</button>`
          : `<button class="btn btn-primary" data-act="wz-next" data-wz="${id}"${valid ? '' : ' disabled'}>Next</button>`}
      </div>
    </div>`;
}

export const isWizardRoute = (route) => route in ROUTES;

/* ---------- events ---------- */

function keepFocus(app, field) {
  const target = field || document.activeElement;
  const name = target && target.dataset && target.dataset.wzField;
  const caret = target && target.selectionStart;
  app.rerender();
  if (!name) return;
  const again = $(`[data-wz-field="${name}"]`);
  if (!again) return;
  again.focus();
  try { if (typeof caret === 'number') again.setSelectionRange(caret, caret); } catch { /* selects have no caret */ }
}

export function wizardInput(target, app) {
  const id = target.dataset.wz;
  if (!id || !target.dataset.wzField) return false;
  const { d } = stateOf(id);
  d[target.dataset.wzField] = target.value;
  keepFocus(app, target);
  return true;
}

export async function wizardClick(act, el, app) {
  if (!act.startsWith('wz-')) return false;
  const id = el.dataset.wz;
  const wizard = WIZARDS[id];
  const w = stateOf(id);
  const arg = el.dataset.arg;
  const S = app.S;

  switch (act) {
    case 'wz-choose':
      w.d[el.dataset.field] = arg;
      if (id === 'connect' && el.dataset.field === 'provider') { delete w.d.check; w.d.context = null; }
      app.rerender();
      break;
    case 'wz-next':
      w.step = Math.min(w.step + 1, wizard.steps.length - 1);
      app.rerender();
      app.scrollTop();
      break;
    case 'wz-back':
      w.step = Math.max(w.step - 1, 0);
      app.rerender();
      app.scrollTop();
      break;
    case 'wz-goto':
      // The stepper only lets people go back, never skip ahead of validation.
      w.step = Math.max(0, Math.min(Number(arg), w.step));
      app.rerender();
      app.scrollTop();
      break;
    case 'wz-pick-context':
      w.d.context = arg;
      delete w.d.check;
      w.d.ns = '';
      app.rerender();
      break;
    case 'wz-rescan':
      await app.reloadState();
      delete w.d.check;
      app.rerender();
      break;
    case 'wz-check':
      w.d.check = { status: 'running' };
      app.rerender();
      try {
        const data = await api.check(w.d.context);
        w.d.check = { status: data.ok ? 'ok' : 'fail', data };
      } catch (error) {
        w.d.check = { status: 'fail', data: { error: error.message, hint: error.hint } };
      }
      w.d.ns = '';
      app.rerender();
      break;
    case 'wz-open':
      app.useContext(w.d.context, w.d.ns);
      app.navigate('pods');
      break;
    case 'wz-default':
      try {
        await api.setDefault(w.d.context);
        await app.reloadState();
        toast(`<span class="mono">${esc(w.d.context)}</span> is now the default context for the CLI.`);
      } catch (error) {
        toast(esc(error.message), 'warn');
      }
      break;
    case 'wz-finish':
      w.step = 0;
      app.navigate(id === 'install' ? 'setup-connect' : 'pods');
      break;
    default:
      return false;
  }
  return true;
}
