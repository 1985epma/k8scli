# k8scli icons

Two original marks, not derived from the Kubernetes, Helm, Docker or any CNCF logo. Brand blue is the
console accent `#2B5CE6`.

| Icon | Idea |
|------|------|
| **k8scli** | A blue rounded tile with a white terminal prompt, `>_`: the command line. |
| **k8scli-ui** | The same tile with the prompt inside a white window with a title bar: the web console. The window is white and the glyph is blue (the CLI icon inverted), so the two read as a family and stay distinct at a glance. |

## Files

| File | What it is |
|------|------------|
| `k8scli.svg`, `k8scli-ui.svg` | Masters (1024 x 1024). Everything else is generated from them. |
| `k8scli-symbolic.svg` | Monochrome 16 x 16 `>_` that follows `currentColor`, for Linux panels and menus. |
| `src/k8scli-small.svg`, `src/k8scli-ui-small.svg` | Simplified masters used for 16, 24 and 32 px: a bolder glyph and less margin, because the detail of the large masters does not survive that small. |
| `src/k8scli-ui-fullbleed.svg` | The UI mark on a full square, without rounded corners or transparency, for the iOS home-screen icon (iOS applies its own mask). |
| `k8scli-<size>.png`, `k8scli-ui-<size>.png` | 16, 24, 32, 48, 64, 128, 256, 512 and 1024 px, transparent background. |
| `k8scli.ico`, `k8scli-ui.ico` | Windows icons with 16, 24, 32, 48, 64, 128 and 256 px (PNG-compressed entries). |
| `k8scli.icns`, `k8scli-ui.icns` | macOS icons (a full iconset up to 512 px @2x). |

The web console uses `internal/web/static/favicon.svg`, `favicon-32.png` and `apple-touch-icon.png`, which
the same script copies and renders.

## Regenerate

```bash
scripts/make-icons.sh
```

It needs headless Chrome (or `rsvg-convert`), `sips` and `iconutil` (macOS), and `python3`; set
`CHROME=/path/to/chrome` to use another browser. The `.ico` files are packed by `scripts/make-ico.py`
(standard library only). To change a mark, edit the SVG masters and run the script; the generated files are
committed.

## License

Same as the repository (see `LICENSE`).
