#!/usr/bin/env sh

set -eu

MODE=${1:-snapshot}
ROOT_DIR=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)

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