package web

import "regexp"

// Constructors for the fields of the catalog entries below. Every path was
// checked against the chart's values.yaml, and scripts/update-helm-catalog.sh
// (TestHelmCatalogFieldsHaveAnEffect) proves that changing a field changes what
// the chart renders.

func toggleField(key, label, def, path, help string) helmField {
	return helmField{Key: key, Label: label, Type: fieldBool, Default: def, path: path, Help: help}
}

func numberField(key, label, path, def string, min, max int, help string) helmField {
	return helmField{Key: key, Label: label, Type: fieldInt, Default: def, Min: min, Max: max, path: path, Help: help}
}

func choiceField(key, label, path, def, help string, options ...string) helmField {
	return helmField{Key: key, Label: label, Type: fieldSelect, Default: def, Options: options, path: path, Help: help}
}

func textField(key, label, path, def string, max int, pattern *regexp.Regexp, help string) helmField {
	return helmField{Key: key, Label: label, Type: fieldString, Default: def, Max: max, pattern: pattern, path: path, Help: help}
}

var (
	// ownerPattern is what external-dns accepts as a TXT owner id.
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,63}$`)
	// domainPattern is a plain DNS domain, such as example.com.
	domainPattern = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$`)
	// hoursPattern is a Tempo retention such as 24h or 168h.
	hoursPattern = regexp.MustCompile(`^[0-9]{1,5}h$`)
	// cronPattern is five cron fields of digits, *, /, comma and dash.
	cronPattern = regexp.MustCompile(`^[0-9*/,-]+( [0-9*/,-]+){4}$`)
)

// helmCatalogMore is the second group of apps. Each entry is from the project's
// own repository, current, and at least a week old when it was pinned.
var helmCatalogMore = []helmEntry{
	{
		ID: "cloudnative-pg", Name: "CloudNativePG", Category: "Databases",
		Description: "An operator that runs and manages PostgreSQL clusters, with backups, failover and rolling updates. Install it, then create a Cluster object.",
		Chart:       "cloudnative-pg", Version: "0.29.1", AppVersion: "1.30.1",
		Repo:        "https://cloudnative-pg.github.io/charts",
		archiveURL:  "https://github.com/cloudnative-pg/charts/releases/download/cloudnative-pg-v0.29.1/cloudnative-pg-0.29.1.tgz",
		digest:      "b53d3991fe84bcf38767e7702cae78666265427a127a26fff168ab4207d2b1df",
		DefaultName: "cnpg", DefaultNamespace: "cnpg-system", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://cloudnative-pg.io/documentation/current/installation_upgrade/",
		Fields: []helmField{
			numberField("replicas", "Operator replicas", "replicaCount", "1", 1, 3, ""),
			toggleField("installCRDs", "Install the CRDs with the chart", "true", "crds.create", "The Cluster, Backup and Pooler kinds. Helm never deletes them."),
		},
	},
	{
		ID: "velero", Name: "Velero", Category: "Backup",
		Description: "Backs up and restores cluster objects and volumes. This installs the server and its CRDs; add a storage plugin and a backup location afterwards.",
		Chart:       "velero", Version: "12.2.1", AppVersion: "1.18.2",
		Repo:        "https://vmware-tanzu.github.io/helm-charts",
		archiveURL:  "https://github.com/vmware-tanzu/helm-charts/releases/download/velero-12.2.1/velero-12.2.1.tgz",
		digest:      "fe4acd70a0123c369e7af7064457b4ffdddebee413e2bb01976a4fe07c471773",
		DefaultName: "velero", DefaultNamespace: "velero", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://velero.io/docs/",
		Fields: []helmField{
			toggleField("nodeAgent", "Back up volumes with the node agent", "false", "deployNodeAgent", "Runs a DaemonSet that copies volume data. It needs access to the node's pod volumes."),
		},
	},
	{
		ID: "external-secrets", Name: "External Secrets", Category: "Security",
		Description: "Syncs secrets from Vault, AWS, GCP, Azure and others into Kubernetes Secrets.",
		Chart:       "external-secrets", Version: "2.14.0", AppVersion: "v2.14.0",
		Repo:        "https://charts.external-secrets.io",
		archiveURL:  "https://github.com/external-secrets/external-secrets/releases/download/helm-chart-2.14.0/external-secrets-2.14.0.tgz",
		digest:      "0a6d9e91b1ff50adfdbf24b50b582babfa7c5b7df42b21974ca4b3fb88ee5e34",
		DefaultName: "external-secrets", DefaultNamespace: "external-secrets", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://external-secrets.io/latest/introduction/getting-started/",
		Fields: []helmField{
			toggleField("installCRDs", "Install the CRDs with the chart", "true", "installCRDs", "Helm never deletes them."),
			numberField("replicas", "Controller replicas", "replicaCount", "1", 1, 3, ""),
		},
	},
	{
		ID: "external-dns", Name: "ExternalDNS", Category: "Networking",
		Description: "Creates DNS records for your Services and Ingresses in your DNS provider. It needs credentials for that provider, which you add afterwards.",
		Chart:       "external-dns", Version: "1.23.0", AppVersion: "0.23.0",
		Repo:        "https://kubernetes-sigs.github.io/external-dns",
		archiveURL:  "https://github.com/kubernetes-sigs/external-dns/releases/download/external-dns-helm-chart-1.23.0/external-dns-1.23.0.tgz",
		digest:      "6d149b9b04db1dc9e46b3693a4eca4ac90d6bba5e3b2a0fdd067ce3f7bfbc31e",
		DefaultName: "external-dns", DefaultNamespace: "external-dns", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://kubernetes-sigs.github.io/external-dns/latest/",
		Fields: []helmField{
			choiceField("provider", "DNS provider", "provider.name", "aws", "", "aws", "google", "azure", "cloudflare", "digitalocean", "rfc2136"),
			choiceField("policy", "What it may change", "policy", "upsert-only", "upsert-only creates and updates records and never deletes them. sync also deletes the ones it created.", "upsert-only", "create-only", "sync"),
			textField("ownerId", "Owner id", "txtOwnerId", "", 63, ownerPattern, "Marks the records it owns, so two clusters do not fight over a zone. Letters, digits . _ -"),
			{
				Key: "domain", Label: "Only this domain", Type: fieldString, Max: 253, pattern: domainPattern,
				Help: "Optional, for example example.com. Empty means every zone the credentials can see.",
				apply: func(values map[string]any, value string) {
					if value == "" {
						delete(values, "domainFilters")
						return
					}
					values["domainFilters"] = []any{value}
				},
				read: func(values map[string]any) (string, bool) {
					if list, ok := values["domainFilters"].([]any); ok && len(list) == 1 {
						if domain, ok := list[0].(string); ok {
							return domain, true
						}
					}
					return "", false
				},
			},
		},
	},
	{
		ID: "kyverno", Name: "Kyverno", Category: "Security",
		Description: "A policy engine: validate, mutate and generate objects with rules written as Kubernetes resources.",
		Chart:       "kyverno", Version: "3.9.2", AppVersion: "v1.19.2",
		Repo:        "https://kyverno.github.io/kyverno",
		archiveURL:  "https://kyverno.github.io/kyverno/kyverno-3.9.2.tgz",
		digest:      "f6fe7184544d5e2ee33a77808c09e67aa603ce6560cc197038fde31d727c861e",
		DefaultName: "kyverno", DefaultNamespace: "kyverno", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://kyverno.io/docs/installation/",
		Fields: []helmField{
			numberField("replicas", "Admission controller replicas", "admissionController.replicas", "1", 1, 5, "Use 3 for high availability: it decides which objects the cluster accepts."),
			toggleField("installCRDs", "Install the CRDs with the chart", "true", "crds.install", "Helm never deletes them."),
		},
	},
	{
		ID: "trivy-operator", Name: "Trivy Operator", Category: "Security",
		Description: "Scans the workloads of the cluster for vulnerabilities, misconfigurations and exposed secrets, and keeps the reports as Kubernetes objects.",
		Chart:       "trivy-operator", Version: "0.37.0", AppVersion: "0.35.0",
		Repo:        "https://aquasecurity.github.io/helm-charts",
		archiveURL:  "https://github.com/aquasecurity/helm-charts/releases/download/trivy-operator-0.37.0/trivy-operator-0.37.0.tgz",
		digest:      "6d23f16d950496d17253258ca2e52bd068959041298e66c0652bf0841ccedf69",
		DefaultName: "trivy-operator", DefaultNamespace: "trivy-system", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://aquasecurity.github.io/trivy-operator/latest/",
		Fields: []helmField{
			choiceField("severity", "Report severities", "trivy.severity", "UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL", "",
				"CRITICAL", "HIGH,CRITICAL", "MEDIUM,HIGH,CRITICAL", "UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL"),
			numberField("concurrentScans", "Scans at the same time", "operator.scanJobsConcurrentLimit", "10", 1, 20, "Each scan is a Job that downloads the vulnerability database."),
		},
	},
	{
		ID: "keda", Name: "KEDA", Category: "Autoscaling",
		Description: "Event-driven autoscaling: scale a workload on queue length, a schedule, Prometheus metrics and many other triggers, including down to zero.",
		Chart:       "keda", Version: "2.21.0", AppVersion: "2.21.0",
		Repo:        "https://kedacore.github.io/charts",
		archiveURL:  "https://kedacore.github.io/charts/keda-2.21.0.tgz",
		digest:      "a7da56bf41a33dad9d2fc82a6fdecee552fff947d80bf1e1dbcf4db0f9e26dee",
		DefaultName: "keda", DefaultNamespace: "keda", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://keda.sh/docs/latest/deploy/",
		Fields: []helmField{
			numberField("replicas", "Operator replicas", "operator.replicaCount", "1", 1, 2, "2 runs a standby for failover."),
			toggleField("installCRDs", "Install the CRDs with the chart", "true", "crds.install", "Helm never deletes them."),
		},
	},
	{
		ID: "argo-rollouts", Name: "Argo Rollouts", Category: "Delivery",
		Description: "Progressive delivery for Kubernetes: canary and blue-green rollouts with automatic analysis and rollback.",
		Chart:       "argo-rollouts", Version: "2.43.8", AppVersion: "v1.10.1",
		Repo:        "https://argoproj.github.io/argo-helm",
		archiveURL:  "https://github.com/argoproj/argo-helm/releases/download/argo-rollouts-2.43.8/argo-rollouts-2.43.8.tgz",
		digest:      "1c998ee3cd72a078ca73ba399644198fe8a446399b5fc5bbff94af5043a0747d",
		DefaultName: "argo-rollouts", DefaultNamespace: "argo-rollouts", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://argo-rollouts.readthedocs.io/en/stable/installation/",
		Fields: []helmField{
			numberField("replicas", "Controller replicas", "controller.replicas", "2", 1, 3, ""),
			toggleField("dashboard", "Include the dashboard", "false", "dashboard.enabled", "A small web UI for rollouts. It has no login of its own."),
		},
	},
	{
		ID: "strimzi", Name: "Strimzi Kafka Operator", Category: "Messaging",
		Description: "Runs Apache Kafka on Kubernetes with operators for clusters, topics and users. Install it, then create a Kafka object.",
		Chart:       "strimzi-kafka-operator", Version: "1.3.0", AppVersion: "1.3.0",
		Repo:        "https://strimzi.io/charts",
		archiveURL:  "https://github.com/strimzi/strimzi-kafka-operator/releases/download/1.3.0/strimzi-kafka-operator-helm-3-chart-1.3.0.tgz",
		digest:      "42677cec73d2122f4a395d9891bbccfae18ec16b3582174d49ca04695f351192",
		DefaultName: "strimzi", DefaultNamespace: "strimzi", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://strimzi.io/docs/operators/latest/deploying",
		Fields: []helmField{
			numberField("replicas", "Operator replicas", "replicas", "1", 1, 3, ""),
			toggleField("watchAnyNamespace", "Watch every namespace", "false", "watchAnyNamespace", "Off: it only manages Kafka objects in its own namespace. On: it gets cluster-wide permissions."),
		},
	},
	{
		ID: "tempo", Name: "Grafana Tempo", Category: "Observability",
		Description: "A tracing backend that stores traces cheaply and works with Grafana. This is the single-binary chart, good for small setups.",
		Chart:       "tempo", Version: "3.2.1", AppVersion: "3.1.1",
		Repo:        "https://grafana-community.github.io/helm-charts",
		archiveURL:  "https://github.com/grafana-community/helm-charts/releases/download/tempo-3.2.1/tempo-3.2.1.tgz",
		digest:      "3389c896bd3c8f2a54f054ac00eb1881c34eff2b8e53053e3ad42c9f394925c3",
		DefaultName: "tempo", DefaultNamespace: "monitoring", CreateNamespace: true,
		Docs: "https://github.com/grafana-community/helm-charts/tree/main/charts/tempo",
		Fields: []helmField{
			toggleField("persistence", "Keep traces on a volume", "false", "persistence.enabled", "Without it, traces are lost when the pod restarts."),
			textField("retention", "Keep traces for", "tempo.retention", "24h", 6, hoursPattern, "Hours, for example 24h or 168h."),
		},
	},
	{
		ID: "alloy", Name: "Grafana Alloy", Category: "Observability",
		Description: "A telemetry collector that gathers metrics, logs and traces and ships them to Prometheus, Loki, Tempo and others. It replaces Promtail.",
		Chart:       "alloy", Version: "1.14.3", AppVersion: "v1.21.2",
		Repo:        "https://grafana.github.io/helm-charts",
		archiveURL:  "https://github.com/grafana/helm-charts/releases/download/alloy-1.14.3/alloy-1.14.3.tgz",
		digest:      "6f14c2c05f3c1b038c61df8ea4a27c7bcb9f1681ece276394a321c3fe4bb08ba",
		DefaultName: "alloy", DefaultNamespace: "monitoring", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://grafana.com/docs/alloy/latest/set-up/install/kubernetes/",
		Fields: []helmField{
			choiceField("controller", "How it runs", "controller.type", "daemonset", "daemonset runs one per node, which is what log collection needs.", "daemonset", "deployment", "statefulset"),
			numberField("replicas", "Replicas", "controller.replicas", "1", 1, 5, "Used when it runs as a deployment or a statefulset."),
		},
	},
	{
		ID: "reloader", Name: "Reloader", Category: "Configuration",
		Description: "Restarts Deployments, StatefulSets and DaemonSets when a ConfigMap or Secret they use changes.",
		Chart:       "reloader", Version: "2.2.18", AppVersion: "v1.4.22",
		Repo:        "https://stakater.github.io/stakater-charts",
		archiveURL:  "https://stakater.github.io/stakater-charts/reloader-2.2.18.tgz",
		digest:      "5eb4576ba75a0e165897e8a98a6ffef15d68939430da6eb8fe80796bba75cb17",
		DefaultName: "reloader", DefaultNamespace: "reloader", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://github.com/stakater/Reloader",
		Fields: []helmField{
			toggleField("watchGlobally", "Watch every namespace", "true", "reloader.watchGlobally", "Off: it only watches its own namespace."),
			toggleField("reloadOnCreate", "Also reload when something is created", "false", "reloader.reloadOnCreate", ""),
		},
	},
	{
		ID: "descheduler", Name: "Descheduler", Category: "Autoscaling",
		Description: "Moves pods to better nodes over time: balances usage, removes duplicates and evicts pods that no longer fit their node.",
		Chart:       "descheduler", Version: "0.37.0", AppVersion: "0.37.0",
		Repo:        "https://kubernetes-sigs.github.io/descheduler",
		archiveURL:  "https://github.com/kubernetes-sigs/descheduler/releases/download/descheduler-helm-chart-0.37.0/descheduler-0.37.0.tgz",
		digest:      "adfcd1d2820e1ad08dafe95b5959c83c721e8710dd6fcbe3ee4a8881be4f8658",
		DefaultName: "descheduler", DefaultNamespace: "descheduler", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://github.com/kubernetes-sigs/descheduler#helm-chart",
		Fields: []helmField{
			choiceField("kind", "How it runs", "kind", "CronJob", "A CronJob runs on a schedule; a Deployment runs all the time.", "CronJob", "Deployment"),
			textField("schedule", "Schedule", "schedule", "*/2 * * * *", 40, cronPattern, "Five cron fields. Used when it runs as a CronJob."),
		},
	},
	{
		ID: "metallb", Name: "MetalLB", Category: "Networking",
		Description: "A load balancer for clusters without a cloud: gives Services of type LoadBalancer real addresses. Add an IPAddressPool afterwards.",
		Chart:       "metallb", Version: "0.16.1", AppVersion: "v0.16.1",
		Repo:        "https://metallb.github.io/metallb",
		archiveURL:  "https://github.com/metallb/metallb/releases/download/metallb-chart-0.16.1/metallb-0.16.1.tgz",
		digest:      "fb06bb584fcb7856f15733b2a6a2aff5b61b5c350687e341c163ae24a5938adc",
		DefaultName: "metallb", DefaultNamespace: "metallb-system", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://metallb.io/installation/",
		Fields: []helmField{
			toggleField("installCRDs", "Install the CRDs with the chart", "true", "crds.enabled", "IPAddressPool, L2Advertisement and the other kinds. Helm never deletes them."),
		},
	},
	{
		ID: "longhorn", Name: "Longhorn", Category: "Storage",
		Description: "Distributed block storage for Kubernetes. The nodes need open-iscsi and a free disk, so it does not run on Docker Desktop.",
		Chart:       "longhorn", Version: "1.13.0", AppVersion: "v1.13.0",
		Repo:        "https://charts.longhorn.io",
		archiveURL:  "https://github.com/longhorn/charts/releases/download/longhorn-1.13.0/longhorn-1.13.0.tgz",
		digest:      "e57f010d2a8bcf0da5b8b03ffecf49c5bc35ddc2663588ce8f22b7567b6133ed",
		DefaultName: "longhorn", DefaultNamespace: "longhorn-system", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://longhorn.io/docs/latest/deploy/install/install-with-helm/",
		Fields: []helmField{
			numberField("replicas", "Copies of every volume", "persistence.defaultClassReplicaCount", "3", 1, 5, "Three needs at least three nodes."),
			choiceField("uiService", "Web UI service type", "service.ui.type", "ClusterIP", "The UI has no login. Do not expose it to the internet.", "ClusterIP", "NodePort", "LoadBalancer"),
		},
	},
	{
		ID: "kube-state-metrics", Name: "kube-state-metrics", Category: "Observability",
		Description: "Turns the state of Kubernetes objects (deployments, pods, nodes) into metrics that Prometheus can scrape.",
		Chart:       "kube-state-metrics", Version: "8.6.0", AppVersion: "2.20.0",
		Repo:        "https://prometheus-community.github.io/helm-charts",
		archiveURL:  "https://github.com/prometheus-community/helm-charts/releases/download/kube-state-metrics-8.6.0/kube-state-metrics-8.6.0.tgz",
		digest:      "9bdf8485c0df29eb2070bacafbb8a4333a22663fb8049da32ff98ec521576388",
		DefaultName: "kube-state-metrics", DefaultNamespace: "monitoring", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-state-metrics",
		Fields: []helmField{
			toggleField("autosharding", "Spread the work over several pods", "false", "autosharding.enabled", "For big clusters: runs a StatefulSet that shards what it watches."),
		},
	},
	{
		ID: "vpa", Name: "Vertical Pod Autoscaler", Category: "Autoscaling",
		Description: "Recommends, and optionally applies, CPU and memory requests for your pods based on what they use. It needs metrics-server.",
		Chart:       "vpa", Version: "5.1.0", AppVersion: "1.7.1",
		Repo:        "https://charts.fairwinds.com/stable",
		archiveURL:  "https://charts.fairwinds.com/stable/vpa-5.1.0.tgz",
		digest:      "ad3e6619403b6b1e65a5afa84de837ac1995ebe72014e89e3f6b6195a988a166",
		DefaultName: "vpa", DefaultNamespace: "vpa", CreateNamespace: true, ClusterScoped: true,
		Docs: "https://github.com/FairwindsOps/charts/tree/master/stable/vpa",
		Fields: []helmField{
			toggleField("recommender", "Recommender", "true", "recommender.enabled", "Watches usage and writes recommendations."),
			toggleField("updater", "Updater", "true", "updater.enabled", "Evicts pods so they restart with the recommended requests."),
			toggleField("admission", "Admission controller", "true", "admissionController.enabled", "Sets the requests on new pods. It is a webhook: if it is down, pod creation can be affected."),
		},
	},
}
