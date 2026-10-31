#!/usr/bin/env bash
# Rebuild every icon from the SVG masters in assets/icons.
#
#   scripts/make-icons.sh
#
# Needs: headless Chrome (or rsvg-convert), sips and iconutil (macOS) and python3.
# Set CHROME=/path/to/chrome to use another browser binary.
#
# Outputs, all committed:
#   assets/icons/k8scli-<size>.png and k8scli-ui-<size>.png   16 24 32 48 64 128 256 512 1024
#   assets/icons/k8scli.ico and k8scli-ui.ico                  16 24 32 48 64 128 256
#   assets/icons/k8scli.icns and k8scli-ui.icns                macOS
#   internal/web/static/favicon.svg, favicon-32.png, apple-touch-icon.png   the web console
#
# Sizes up to 32 px are drawn from the simplified masters in assets/icons/src (a bolder glyph and
# less margin), because the detail of the large masters does not survive at that size.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ICONS="$ROOT/assets/icons"
SRC="$ICONS/src"
STATIC="$ROOT/internal/web/static"
CHROME="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
SIZES=(16 24 32 48 64 128 256 512 1024)
NAMES=(k8scli k8scli-ui)

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP:?}"' EXIT

die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# render SVG SIZE OUT: a PNG with a transparent background.
render() {
	local svg="$1" size="$2" out="$3"
	if command -v rsvg-convert >/dev/null 2>&1; then
		rsvg-convert -w "$size" -h "$size" "$svg" -o "$out"
		return
	fi
	[ -x "$CHROME" ] || die "need rsvg-convert or a Chrome binary (CHROME=...)"
	local page="$TMP/page-$size-$$.html"
	printf '<!doctype html><html><body style="margin:0;background:transparent"><img src="file://%s" width="%s" height="%s" style="display:block"></body></html>' \
		"$svg" "$size" "$size" >"$page"
	"$CHROME" --headless=new --disable-gpu --hide-scrollbars --default-background-color=00000000 \
		--window-size="$size,$size" --screenshot="$out" "file://$page" >/dev/null 2>&1
	[ -s "$out" ] || die "failed to render $svg at $size px"
}

master_for() { # NAME SIZE -> the SVG to draw at that size
	if [ "$2" -le 32 ]; then printf '%s/%s-small.svg' "$SRC" "$1"; else printf '%s/%s.svg' "$ICONS" "$1"; fi
}

for name in "${NAMES[@]}"; do
	for size in "${SIZES[@]}"; do
		render "$(master_for "$name" "$size")" "$size" "$ICONS/$name-$size.png"
	done
	printf 'png  %s: %s sizes\n' "$name" "${#SIZES[@]}"

	python3 "$ROOT/scripts/make-ico.py" "$ICONS/$name.ico" \
		"$ICONS/$name-16.png" "$ICONS/$name-24.png" "$ICONS/$name-32.png" "$ICONS/$name-48.png" \
		"$ICONS/$name-64.png" "$ICONS/$name-128.png" "$ICONS/$name-256.png"
	printf 'ico  %s.ico\n' "$name"

	set_dir="$TMP/$name.iconset"
	mkdir -p "$set_dir"
	cp "$ICONS/$name-16.png"   "$set_dir/icon_16x16.png"
	cp "$ICONS/$name-32.png"   "$set_dir/icon_16x16@2x.png"
	cp "$ICONS/$name-32.png"   "$set_dir/icon_32x32.png"
	cp "$ICONS/$name-64.png"   "$set_dir/icon_32x32@2x.png"
	cp "$ICONS/$name-128.png"  "$set_dir/icon_128x128.png"
	cp "$ICONS/$name-256.png"  "$set_dir/icon_128x128@2x.png"
	cp "$ICONS/$name-256.png"  "$set_dir/icon_256x256.png"
	cp "$ICONS/$name-512.png"  "$set_dir/icon_256x256@2x.png"
	cp "$ICONS/$name-512.png"  "$set_dir/icon_512x512.png"
	cp "$ICONS/$name-1024.png" "$set_dir/icon_512x512@2x.png"
	if command -v iconutil >/dev/null 2>&1; then
		iconutil -c icns "$set_dir" -o "$ICONS/$name.icns"
		printf 'icns %s.icns\n' "$name"
	else
		printf 'skip %s.icns (iconutil is macOS only)\n' "$name"
	fi
done

# The web console: favicon (SVG and PNG fallback) and the iOS home-screen icon, which has no rounded
# corners or transparency because iOS applies its own mask.
cp "$SRC/k8scli-ui-small.svg" "$STATIC/favicon.svg"
cp "$ICONS/k8scli-ui-32.png" "$STATIC/favicon-32.png"
render "$SRC/k8scli-ui-fullbleed.svg" 180 "$STATIC/apple-touch-icon.png"
printf 'web  favicon.svg favicon-32.png apple-touch-icon.png\n'
