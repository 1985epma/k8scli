#!/usr/bin/env sh

set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
TMP_DIR=$(mktemp -d)
DEFAULT_TAP_SUFFIX=$(basename "$TMP_DIR" | tr '[:upper:]' '[:lower:]' | tr -c '[:alnum:]' '-')
TAP_NAME=${TAP_NAME:-local/k8scli-test-$DEFAULT_TAP_SUFFIX}

cleanup() {
	HOMEBREW_NO_AUTO_UPDATE=1 brew uninstall --force k8scli >/dev/null 2>&1 || true
	HOMEBREW_NO_AUTO_UPDATE=1 brew uninstall --force "$TAP_NAME/k8scli" >/dev/null 2>&1 || true
	rm -rf "$TMP_DIR"
	brew untap "$TAP_NAME" >/dev/null 2>&1 || true
}

trap cleanup EXIT INT TERM

ARCHIVE="$TMP_DIR/k8scli-local.tar.gz"
SHA256_FILE="$TMP_DIR/k8scli-local.sha256"
FORMULA_FILE="$TMP_DIR/k8scli.rb"

HOMEBREW_NO_AUTO_UPDATE=1 brew uninstall --force k8scli >/dev/null 2>&1 || true
HOMEBREW_NO_AUTO_UPDATE=1 brew uninstall --force "$TAP_NAME/k8scli" >/dev/null 2>&1 || true
brew untap "$TAP_NAME" >/dev/null 2>&1 || true

tar -czf "$ARCHIVE" -C "$ROOT_DIR" go.mod go.sum LICENSE README.md install.md main.go Makefile scripts homebrew-k8scli
shasum -a 256 "$ARCHIVE" | awk '{print $1}' > "$SHA256_FILE"

sed \
	-e "s|https://github.com/1985epma/k8scli/archive/refs/heads/main.tar.gz|file://$ARCHIVE|" \
	-e "s|908c906fa60b7ea6ba511222bc21d69f108919575d6ae41265610e127b9c3cc5|$(cat "$SHA256_FILE")|" \
	"$ROOT_DIR/homebrew-k8scli/Formula/k8scli.rb" > "$FORMULA_FILE"

HOMEBREW_NO_AUTO_UPDATE=1 brew tap-new "$TAP_NAME" >/dev/null
TAP_REPO=$(brew --repo "$TAP_NAME")

mkdir -p "$TAP_REPO/Formula"
cp "$FORMULA_FILE" "$TAP_REPO/Formula/k8scli.rb"

HOMEBREW_NO_AUTO_UPDATE=1 brew install --build-from-source "$TAP_NAME/k8scli"
HOMEBREW_NO_AUTO_UPDATE=1 brew test "$TAP_NAME/k8scli"

printf 'Homebrew tap test completed for %s\n' "$TAP_NAME"