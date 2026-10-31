#!/usr/bin/env bash
# Checks the Helm catalog of the web console against the upstream chart repositories.
#
# For every entry it (1) downloads the pinned archive and verifies its sha256,
# (2) renders the chart offline with each field changed, to prove that every field
# still maps to a value the chart has, and (3) prints the newest stable chart
# version that is at least seven days old, with its digest and archive URL. It never edits the catalog: a maintainer reviews the
# output (release notes, kubeVersion, values that changed) and edits
# internal/web/helm_catalog.go by hand. See docs/adr/0004-helm-apps.md.
#
# Usage: scripts/update-helm-catalog.sh
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v go >/dev/null 2>&1; then
	echo "go not found in PATH; install Go first: https://go.dev/dl/" >&2
	exit 1
fi

output="$(mktemp)"
trap 'rm -f "$output"' EXIT

status=0
K8SCLI_HELM_LIVE=1 go test ./internal/web -run 'TestHelmCatalogUpstream|TestHelmCatalogFieldsHaveAnEffect' -count=1 -v >"$output" 2>&1 || status=$?

grep '^CATALOG ' "$output" | sed 's/^CATALOG //' || true

if [ "$status" -ne 0 ]; then
	echo >&2
	echo "The check found problems (a pinned digest changed, a repository is unreachable, or no version qualified):" >&2
	grep -E '^\s+(helm_live_test\.go|--- FAIL)' "$output" >&2 || tail -n 20 "$output" >&2
	exit "$status"
fi
