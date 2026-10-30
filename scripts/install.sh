#!/usr/bin/env sh

set -eu

PREFIX=/usr/local
SHELL_NAME=zsh

while [ "$#" -gt 0 ]; do
	case "$1" in
		--prefix)
			PREFIX="$2"
			shift 2
			;;
		--shell)
			SHELL_NAME="$2"
			shift 2
			;;
		-h|--help)
			echo "Usage: $0 [--prefix <path>] [--shell zsh|bash|fish]"
			exit 0
			;;
		*)
			echo "Unknown argument: $1" >&2
			exit 1
			;;
	esac
done

if ! command -v go >/dev/null 2>&1; then
	echo "go not found in PATH; install Go first: https://go.dev/dl/" >&2
	exit 1
fi

ROOT_DIR=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

BIN_DIR="$PREFIX/bin"
BUILT_BIN="$TMP_DIR/k8scli"

VERSION=$(git -C "$ROOT_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)

printf 'Building k8scli %s...\n' "$VERSION"
(cd "$ROOT_DIR" && go build -trimpath \
	-ldflags "-s -w -X github.com/1985epma/k8scli/cmd.version=$VERSION" \
	-o "$BUILT_BIN" .)

mkdir -p "$BIN_DIR"
install "$BUILT_BIN" "$BIN_DIR/k8scli"
printf 'Installed binary: %s\n' "$BIN_DIR/k8scli"

case "$SHELL_NAME" in
	zsh)
		COMPLETION_DIR="$PREFIX/share/zsh/site-functions"
		COMPLETION_FILE="$COMPLETION_DIR/_k8scli"
		;;
	bash)
		COMPLETION_DIR="$PREFIX/share/bash-completion/completions"
		COMPLETION_FILE="$COMPLETION_DIR/k8scli"
		;;
	fish)
		COMPLETION_DIR="$PREFIX/share/fish/vendor_completions.d"
		COMPLETION_FILE="$COMPLETION_DIR/k8scli.fish"
		;;
	*)
		echo "Unsupported shell: $SHELL_NAME" >&2
		exit 1
		;;
esac

# Reuse the binary that was just built instead of recompiling with `go run`.
mkdir -p "$COMPLETION_DIR"
"$BUILT_BIN" completion "$SHELL_NAME" > "$COMPLETION_FILE"
printf 'Installed %s completion: %s\n' "$SHELL_NAME" "$COMPLETION_FILE"

printf 'Installation complete. Ensure %s is in your PATH.\n' "$BIN_DIR"
