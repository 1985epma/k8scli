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

ROOT_DIR=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

BIN_DIR="$PREFIX/bin"
mkdir -p "$BIN_DIR"

printf 'Building k8scli...\n'
go build -o "$TMP_DIR/k8scli" "$ROOT_DIR"
install "$TMP_DIR/k8scli" "$BIN_DIR/k8scli"
printf 'Installed binary: %s\n' "$BIN_DIR/k8scli"

case "$SHELL_NAME" in
	zsh)
		COMPLETION_DIR="$PREFIX/share/zsh/site-functions"
		COMPLETION_FILE="$COMPLETION_DIR/_k8scli"
		mkdir -p "$COMPLETION_DIR"
		go run "$ROOT_DIR" completion zsh > "$COMPLETION_FILE"
		printf 'Installed zsh completion: %s\n' "$COMPLETION_FILE"
		;;
	bash)
		COMPLETION_DIR="$PREFIX/share/bash-completion/completions"
		COMPLETION_FILE="$COMPLETION_DIR/k8scli"
		mkdir -p "$COMPLETION_DIR"
		go run "$ROOT_DIR" completion bash > "$COMPLETION_FILE"
		printf 'Installed bash completion: %s\n' "$COMPLETION_FILE"
		;;
	fish)
		COMPLETION_DIR="$PREFIX/share/fish/vendor_completions.d"
		COMPLETION_FILE="$COMPLETION_DIR/k8scli.fish"
		mkdir -p "$COMPLETION_DIR"
		go run "$ROOT_DIR" completion fish > "$COMPLETION_FILE"
		printf 'Installed fish completion: %s\n' "$COMPLETION_FILE"
		;;
	*)
		echo "Unsupported shell: $SHELL_NAME" >&2
		exit 1
		;;
esac

printf 'Installation complete. Ensure %s is in your PATH.\n' "$BIN_DIR"