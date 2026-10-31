#!/usr/bin/env bash
# Disaster-recovery lab for k8scli: a three-node kind cluster with a small sample system.
#
#   scripts/dr-lab.sh up           create the cluster (if needed) and seed the sample system
#   scripts/dr-lab.sh seed         (re)apply the sample system to an existing cluster
#   scripts/dr-lab.sh reset        delete the `shop` namespace and seed it again (the cluster stays)
#   scripts/dr-lab.sh status       nodes, workloads, database state and memory use
#   scripts/dr-lab.sh check-data   write a heartbeat row to the database and read the newest one back
#                                  (`check-data read` only reads; use it after a restore to measure RPO)
#   scripts/dr-lab.sh down         delete the cluster
#
# What it builds (context `kind-k8scli-lab`, every kubectl call passes --context, so the
# docker-desktop context and your current-context are never used or changed):
#
#   cluster k8scli-lab   1 control plane + 2 workers (kindest/node v1.36.4)
#   namespace shop
#     tier 0  shop-db     CloudNativePG Cluster, 2 instances, 1Gi PVCs (operator v1.30.1)
#             legacy-db   plain StatefulSet (redis), 2 replicas, 100Mi PVCs
#     tier 1  api         Deployment, 2 replicas, probes, requests, PDB, topology spread
#     tier 2  web         Deployment, 2 replicas, same, plus an HPA (min 2, max 4)
#     shop-report         CronJob every 5 minutes
#
# A matching plan is in docs/examples/k8scli-dr.lab.yaml.
#
# Coordination protocol for destructive tests (several people or agents may share the lab):
#   take the lock   mkdir "$LAB_LOCK_DIR"   (it fails if someone else holds it: wait and retry)
#   do the test     drains, node stop/start, hibernation, scaling to zero...
#   release it      rmdir "$LAB_LOCK_DIR"   (also on failure)
# Only ever act on the context kind-k8scli-lab. Set LAB_READY_FILE to a path and `up` touches it
# when everything is Ready, so other tools can wait for the lab.
set -euo pipefail

CLUSTER="k8scli-lab"
CTX="kind-${CLUSTER}"
NS="shop"
CNPG_VERSION="1.30.1"
CNPG_URL="https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v${CNPG_VERSION}/cnpg-${CNPG_VERSION}.yaml"
DIR="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/dr-lab"
LAB_READY_FILE="${LAB_READY_FILE:-}"
export LAB_LOCK_DIR="${LAB_LOCK_DIR:-${TMPDIR:-/tmp}/k8scli-dr-lab.lock}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$CTX" "$@"; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 not found in PATH"; }

cluster_exists() { kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; }

# kind rewrites the kubeconfig and makes the new cluster the current context. Put the
# user's current context back so nothing else (the k8scli default context) changes.
restore_context() {
	local previous="$1"
	if [ -n "$previous" ] && [ "$previous" != "$CTX" ]; then
		kubectl config use-context "$previous" >/dev/null
	fi
}

nodes() { docker ps -a --filter "label=io.x-k8s.kind.cluster=${CLUSTER}" --format '{{.Names}}' | sort; }

# After a stop (for example a DR test), start the node containers again: control plane first.
ensure_running() {
	local name started=0
	for name in "${CLUSTER}-control-plane" $(nodes | grep -v control-plane || true); do
		if [ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" != "true" ]; then
			log "starting node container $name"
			docker start "$name" >/dev/null
			started=1
		fi
	done
	if [ "$started" = 1 ]; then
		log "waiting for the API server"
		for _ in $(seq 1 60); do
			k get --raw /readyz >/dev/null 2>&1 && return 0
			sleep 5
		done
		die "the API server did not come back"
	fi
}

create_cluster() {
	local previous
	previous="$(kubectl config current-context 2>/dev/null || true)"
	if cluster_exists; then
		log "cluster $CLUSTER already exists"
		if ! kubectl config get-contexts -o name | grep -qx "$CTX"; then
			kind export kubeconfig --name "$CLUSTER" >/dev/null
			restore_context "$previous"
		fi
		ensure_running
		return 0
	fi
	log "creating kind cluster $CLUSTER (1 control plane + 2 workers)"
	kind create cluster --config "$DIR/kind.yaml" --wait 180s
	restore_context "$previous"
}

install_operator() {
	if k -n cnpg-system get deployment cnpg-controller-manager >/dev/null 2>&1; then
		log "CloudNativePG operator already installed"
	else
		log "installing the CloudNativePG operator v${CNPG_VERSION}"
		k apply --server-side -f "$CNPG_URL" >/dev/null
	fi
	k -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=240s
}

seed() {
	need kubectl
	log "applying the sample system in namespace $NS"
	k apply -f "$DIR/shop-base.yaml" >/dev/null

	# The operator's webhook can take a few seconds to answer after it becomes Ready.
	local attempt
	for attempt in $(seq 1 12); do
		if k apply -f "$DIR/shop-db.yaml" >/dev/null 2>&1; then
			break
		fi
		[ "$attempt" = 12 ] && die "could not create the shop-db Cluster (is the operator ready?)"
		log "waiting for the operator webhook (attempt $attempt)"
		sleep 10
	done

	log "waiting for everything to be Ready"
	k -n "$NS" rollout status statefulset/legacy-db --timeout=300s
	k -n "$NS" rollout status deployment/api --timeout=300s
	k -n "$NS" rollout status deployment/web --timeout=300s
	k -n "$NS" wait --for=condition=Ready cluster.postgresql.cnpg.io/shop-db --timeout=600s
}

cmd_up() {
	need docker
	need kind
	need kubectl
	create_cluster
	install_operator
	seed
	log "lab is ready"
	cmd_status
	if [ -n "$LAB_READY_FILE" ]; then
		touch "$LAB_READY_FILE"
		log "touched $LAB_READY_FILE"
	fi
}

cmd_down() {
	need kind
	if cluster_exists; then
		log "deleting cluster $CLUSTER"
		local previous
		previous="$(kubectl config current-context 2>/dev/null || true)"
		kind delete cluster --name "$CLUSTER"
		restore_context "$previous"
	else
		log "cluster $CLUSTER does not exist"
	fi
	if [ -n "$LAB_READY_FILE" ]; then rm -f -- "$LAB_READY_FILE"; fi
}

cmd_reset() {
	need kubectl
	log "deleting namespace $NS (and its volumes)"
	k delete namespace "$NS" --ignore-not-found --wait=true --timeout=300s
	seed
	log "lab reset"
}

cmd_status() {
	need kubectl
	log "context: $CTX"
	k get nodes -o wide --no-headers | awk '{printf "node %-28s %-9s %-14s %s\n", $1, $2, $3, $5}'
	log "namespace $NS"
	k -n "$NS" get deployment,statefulset,cronjob,hpa,pdb 2>/dev/null || true
	k -n "$NS" get cluster.postgresql.cnpg.io 2>/dev/null || true
	k -n "$NS" get pods -o wide --no-headers 2>/dev/null | awk '{printf "pod  %-34s %-6s %-12s %s\n", $1, $2, $3, $7}' || true
	if command -v docker >/dev/null 2>&1; then
		log "memory of the node containers"
		# shellcheck disable=SC2046
		docker stats --no-stream --format '{{.Name}}  {{.MemUsage}}  cpu {{.CPUPerc}}' $(docker ps -q --filter "label=io.x-k8s.kind.cluster=${CLUSTER}") 2>/dev/null || true
	fi
}

# Writes (unless `read`) a heartbeat row on the primary and prints the newest row with its age.
# After a restore, `check-data read` shows how much data survived: RPO = time of the disaster - written_at.
cmd_check_data() {
	need kubectl
	local mode="${1:-write}"
	local primary
	primary="$(k -n "$NS" get pod -l 'cnpg.io/cluster=shop-db,cnpg.io/instanceRole=primary' -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
	[ -n "$primary" ] || die "no primary pod found for shop-db (is the database running?)"
	psql() { k -n "$NS" exec "$primary" -c postgres -- psql -U postgres -d shop -v ON_ERROR_STOP=1 -tA -F '|' "$@"; }

	psql -c "SET client_min_messages = warning; CREATE TABLE IF NOT EXISTS dr_heartbeat (id bigserial PRIMARY KEY, written_at timestamptz NOT NULL DEFAULT now(), note text)" >/dev/null
	if [ "$mode" = write ]; then
		psql -c "INSERT INTO dr_heartbeat (note) VALUES ('lab check on ${primary}')" >/dev/null
	elif [ "$mode" != read ]; then
		die "usage: dr-lab.sh check-data [write|read]"
	fi
	local row
	row="$(psql -c "SELECT id, to_char(written_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'), round(extract(epoch FROM now() - written_at)), (SELECT count(*) FROM dr_heartbeat) FROM dr_heartbeat ORDER BY id DESC LIMIT 1")"
	[ -n "$row" ] || die "the heartbeat table is empty"
	IFS='|' read -r id written age total <<<"$row"
	printf 'primary=%s newest heartbeat id=%s written_at=%s age=%ss rows=%s\n' "$primary" "$id" "$written" "$age" "$total"
}


usage() {
	sed -n '2,14p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

case "${1:-}" in
up) cmd_up ;;
down) cmd_down ;;
seed) need kubectl; install_operator; seed ;;
reset) cmd_reset ;;
status) cmd_status ;;
check-data) shift; cmd_check_data "${1:-write}" ;;
-h | --help | help | "") usage ;;
*) usage >&2; exit 2 ;;
esac
