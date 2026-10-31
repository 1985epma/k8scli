#!/usr/bin/env bash
# Builds k8scli-ui.app, a double-clickable macOS app that opens the k8scli web console.
#
#   scripts/build-macos-app.sh --binary ./k8scli --version 0.1.1 --out dist
#
# The bundle is self-contained: it carries the k8scli binary, and its launcher opens Terminal.app
# running `k8scli ui`, so the logs are visible, Ctrl+C or closing the window stops the console, and
# the console sees your normal shell environment (PATH for aws, gcloud and kubelogin credential plugins).
#
# Options:
#   --binary FILE   the k8scli binary for the target architecture (default: ./k8scli)
#   --version V     version shown in Finder, 1.2.3 or v1.2.3-rc1 (default: dev)
#   --icns FILE     the app icon (default: assets/icons/k8scli-ui.icns)
#   --out DIR       where k8scli-ui.app is created (default: dist)
#   --zip FILE      also write a zip of the app, ready to attach to a release or a Homebrew cask
#   --os NAME       do nothing unless NAME is darwin (lets GoReleaser call this for every build target)
#
# Signing: on a Mac the bundle gets an ad-hoc signature (`codesign --sign -`), which is enough to run on
# Apple Silicon. It is NOT notarized: an app downloaded from the internet may need right-click > Open
# the first time. Other systems skip the signature and say so.

set -euo pipefail

ROOT_DIR=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
BINARY="$ROOT_DIR/k8scli"
VERSION=dev
ICNS="$ROOT_DIR/assets/icons/k8scli-ui.icns"
OUT_DIR="$ROOT_DIR/dist"
ZIP_FILE=""

die() { printf 'error: %s\n' "$*" >&2; exit 1; }

while [ "$#" -gt 0 ]; do
	case "$1" in
		--binary) BINARY="${2:?--binary needs a file}"; shift 2 ;;
		--version) VERSION="${2:?--version needs a value}"; shift 2 ;;
		--icns) ICNS="${2:?--icns needs a file}"; shift 2 ;;
		--out) OUT_DIR="${2:?--out needs a directory}"; shift 2 ;;
		--zip) ZIP_FILE="${2:?--zip needs a file}"; shift 2 ;;
		--os)
			[ "${2:-}" = "darwin" ] || exit 0
			shift 2
			;;
		-h | --help)
			sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'
			exit 0
			;;
		*) die "unknown argument: $1" ;;
	esac
done

[ -f "$BINARY" ] || die "binary not found: $BINARY (run make build first)"
[ -f "$ICNS" ] || die "icon not found: $ICNS"

# Finder wants a numeric x.y.z; drop a leading v and anything after the numbers (-rc1, -next).
NUMERIC=$(printf '%s' "$VERSION" | sed -E 's/^v//; s/^([0-9]+(\.[0-9]+){0,2}).*$/\1/')
case "$NUMERIC" in
	[0-9]*) ;;
	*) NUMERIC=0.0.0 ;;
esac

APP="$OUT_DIR/k8scli-ui.app"
rm -rf "${APP:?}"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

install -m 0755 "$BINARY" "$APP/Contents/Resources/k8scli"
install -m 0644 "$ICNS" "$APP/Contents/Resources/k8scli-ui.icns"

cat >"$APP/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>k8scli web console</string>
	<key>CFBundleDisplayName</key>
	<string>k8scli web console</string>
	<key>CFBundleIdentifier</key>
	<string>io.github.1985epma.k8scli-ui</string>
	<key>CFBundleExecutable</key>
	<string>k8scli-ui</string>
	<key>CFBundleIconFile</key>
	<string>k8scli-ui</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>${NUMERIC}</string>
	<key>CFBundleVersion</key>
	<string>${NUMERIC}</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>LSMinimumSystemVersion</key>
	<string>12.0</string>
	<key>NSHighResolutionCapable</key>
	<true/>
	<key>LSUIElement</key>
	<false/>
	<key>NSHumanReadableCopyright</key>
	<string>MIT License. https://github.com/1985epma/k8scli</string>
</dict>
</plist>
EOF

# The launcher is quoted ('EOF') so nothing in it is expanded while writing the file.
cat >"$APP/Contents/MacOS/k8scli-ui" <<'EOF'
#!/bin/sh
# Opens Terminal.app running the bundled k8scli with `ui`. `exec` makes the console the shell's
# process, so Ctrl+C or closing the window ends it.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
bin="$here/../Resources/k8scli"

# Single-quote the path for the shell (a ' becomes '\''), then escape it for an AppleScript string.
quoted=$(printf '%s' "$bin" | sed "s/'/'\\\\''/g")
command="exec '$quoted' ui"
escaped=$(printf '%s' "$command" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')

# `do script` first: when Terminal is not running it opens one window instead of two.
exec /usr/bin/osascript \
	-e 'tell application "Terminal"' \
	-e "do script \"$escaped\"" \
	-e 'activate' \
	-e 'end tell'
EOF
chmod 0755 "$APP/Contents/MacOS/k8scli-ui"

if command -v plutil >/dev/null 2>&1; then
	plutil -lint "$APP/Contents/Info.plist" >/dev/null || die "Info.plist is not valid"
fi

if command -v codesign >/dev/null 2>&1; then
	codesign --force --deep --sign - "$APP" >/dev/null 2>&1 || die "ad-hoc signing failed"
	printf 'Signed %s (ad-hoc, not notarized)\n' "$APP"
else
	printf 'codesign not found: %s is not signed (build it on a Mac to get the ad-hoc signature)\n' "$APP" >&2
fi

if [ -n "$ZIP_FILE" ]; then
	mkdir -p "$(dirname "$ZIP_FILE")"
	rm -f "${ZIP_FILE:?}"
	zip_abs="$(CDPATH='' cd -- "$(dirname "$ZIP_FILE")" && pwd)/$(basename "$ZIP_FILE")"
	if command -v ditto >/dev/null 2>&1; then
		ditto -c -k --keepParent --norsrc --noextattr --noqtn --noacl "$APP" "$zip_abs"
	else
		(cd "$OUT_DIR" && zip -qry "$zip_abs" k8scli-ui.app)
	fi
	printf 'Wrote %s\n' "$zip_abs"
fi

printf 'Built %s (version %s)\n' "$APP" "$NUMERIC"
