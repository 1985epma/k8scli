#!/usr/bin/env sh

set -eu

MODE=${1:-snapshot}
ROOT_DIR=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)

case "$MODE" in
	-h|--help|help)
		echo "Usage: $0 [snapshot|release]"
		exit 0
		;;
esac

if ! command -v goreleaser >/dev/null 2>&1; then
	echo "goreleaser not found in PATH" >&2
	echo "Install it first: https://goreleaser.com/install/" >&2
	exit 1
fi

cd "$ROOT_DIR"

case "$MODE" in
	snapshot)
		# A local snapshot should not need the whole release toolchain: skip the
		# steps whose tool is missing (SBOM needs syft, signing cosign, images docker).
		skip=""
		add_skip() { skip="${skip:+$skip,}$1"; }
		command -v syft >/dev/null 2>&1 || add_skip sbom
		command -v cosign >/dev/null 2>&1 || add_skip sign
		if ! { command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; }; then
			add_skip docker
		fi
		if [ -n "$skip" ]; then
			echo "Skipping: $skip (tool not available locally)" >&2
			exec goreleaser release --snapshot --clean --skip="$skip"
		fi
		exec goreleaser release --snapshot --clean
		;;
	release)
		exec goreleaser release --clean
		;;
	*)
		echo "Unknown mode: $MODE" >&2
		echo "Usage: $0 [snapshot|release]" >&2
		exit 1
		;;
esac