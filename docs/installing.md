# Installing k8scli and k8scli-ui, and their icons

`k8scli` is the command line. `k8scli-ui` opens the [web console](web-console.md). They are the same
binary under two names, and each one has its own icon where the system can show one.

| System | What you get | Where the icons show up |
|--------|--------------|-------------------------|
| macOS (Homebrew formula) | `k8scli` and `k8scli-ui` commands | Nowhere: a command line binary cannot carry an icon on macOS. Use the app below |
| macOS (app) | `k8scli-ui.app`, double-click to open the console | Finder, Dock, Launchpad, Spotlight |
| Linux (`.deb`, `.rpm`) | `k8scli` and `k8scli-ui` commands, two menu entries | Application menu and launcher, in every size from 16 to 512 px, plus a scalable and a symbolic icon |
| Windows (MSI) | `k8scli.exe`, `k8scli-ui.cmd`, Start Menu shortcuts, optional desktop shortcut | Shortcuts, Apps and features, and `k8scli.exe` itself |
| Windows (zip or portable `.exe`) | The executable | `k8scli.exe` itself (Explorer, taskbar) |

## macOS

```bash
brew install 1985epma/k8scli/k8scli      # the commands
```

For the app, either download `k8scli-ui_<version>_macOS_<arch>.zip` from the
[releases](https://github.com/1985epma/k8scli/releases) and move `k8scli-ui.app` to `/Applications`,
or build it from source:

```bash
make install-local     # also copies k8scli-ui.app to ~/Applications (use --no-app to skip it)
make app               # only builds dist/k8scli-ui.app and a zip
```

The app opens Terminal.app running `k8scli ui`, so the logs stay visible and closing the window stops the
console. The first launch asks to let the app control Terminal (System Settings > Privacy & Security >
Automation).

The app is signed ad hoc, not notarized. An app downloaded from the internet is quarantined, so the first
time use right-click > Open (or `xattr -dr com.apple.quarantine /Applications/k8scli-ui.app`). An app built
on your own Mac, with `make app` or `make install-local`, is not quarantined. Notarizing needs an Apple
Developer ID and is not done yet.

## Linux

Install the `.deb` or `.rpm` from the [releases](https://github.com/1985epma/k8scli/releases)
(see the [README](../README.md#linux)). The package installs:

- `/usr/bin/k8scli` and `/usr/bin/k8scli-ui` (a link to `k8scli`)
- `/usr/share/applications/k8scli.desktop` and `k8scli-ui.desktop`. Both run in a terminal, so the console's
  address and logs are visible
- the icons under `/usr/share/icons/hicolor/`

The package refreshes the icon cache and the desktop database after installing and removing. If the menu
entries do not appear right away, log out and in again.

## Windows

The MSI puts a **k8scli** folder in the Start Menu with two shortcuts: **k8scli web console** (runs
`k8scli.exe ui`) and **k8scli (terminal)** (a console with `k8scli` on the path and its help shown). A
desktop shortcut is optional and off by default:

```powershell
msiexec /i k8scli_<version>_windows_amd64.msi ADDLOCAL=MainFeature,DesktopShortcutFeature
```

The MSI also sets the icon shown in Apps and features. `k8scli.exe` carries the icon, the manifest and the
version information (Properties > Details) in every build, including the zip and the portable `.exe`.

## Building the icons

The artwork lives in [assets/icons/](../assets/icons/README.md) and is generated from two SVG masters with
`scripts/make-icons.sh`. Packaging reads the generated files from there.
