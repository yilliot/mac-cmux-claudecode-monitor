# MacMonitor

A lightweight macOS menu bar app (Go) showing live **memory** and **disk** usage
in the top-right status bar, plus a **Keep Awake while Claude Code runs** toggle.
Reads stats via syscalls — idles near 0% CPU, ~20–40 MB RAM.

## Install (prebuilt)

Download `MacMonitor.app.zip` from the
[latest release](https://github.com/yilliot/mac-cmux-claudecode-monitor/releases/latest),
unzip, and move `MacMonitor.app` to `/Applications`. It's ad-hoc signed, so on
first launch right-click → **Open** to clear Gatekeeper. Then run
[`./install-hooks.sh`](#one-time-setup-install-the-hooks) to enable Claude detection.

## Run (development)

```sh
go mod tidy
go run .
```

A compact two-line indicator appears in the menu bar:

```
m:9☕️    ← memory used (GB), top line; ☕️/◦ keep-awake marker (see below)
s:234    ← storage free (GB), bottom line
```

Click it for the menu: the Keep Awake toggle and full memory/disk details.

## Keep Awake while Claude Code runs

A single **On/Off** toggle in the menu keeps the Mac from sleeping — but only
while Claude Code is actively working. It's driven by Claude Code's own lifecycle
**hooks** (not CPU guessing), so it's accurate in cmux, iTerm, or any terminal.

- **Off** — does nothing; the Mac sleeps normally.
- **On** — holds a `caffeinate` assertion *only while Claude is running*. The
  top-bar shows `◦` when watching an idle Claude and `☕️` while it's running and
  the Mac is held awake.

### One-time setup: install the hooks

```sh
./install-hooks.sh
```

This merges five hooks into `~/.claude/settings.json` (backing it up first) that
write `running`/`idle` to `~/.macmonitor/claude-state`:

| Event | Writes | Means |
|---|---|---|
| `UserPromptSubmit`, `PreToolUse`, `PostToolUse` | `running` | Claude is working |
| `Stop` | `idle` | Claude finished its turn |
| `Notification` (`idle_prompt`/`permission_prompt`) | `idle` | waiting on you |

Restart any open Claude Code sessions afterward so the hooks load. MacMonitor
polls the state file every 2s; no detection runs until you flip the toggle On.

### Limitation: lid-close sleep

`caffeinate` only blocks *idle* sleep. Closing the lid triggers a separate forced
**clamshell sleep** that this toggle cannot override — the Mac will still sleep.
To keep running with the lid shut, either use clamshell mode (external display +
power) or set `sudo pmset -b disablesleep 1` (reverts with `0`; runs hot in a bag).

## Build a real .app bundle

```sh
./build.sh
open MacMonitor.app
```

The bundle uses `LSUIElement=true` — menu bar only, no Dock icon, no window.

## Tuning

Edit the intervals in `main.go`:

- `memInterval`  — memory refresh (default 2s)
- `diskInterval` — disk refresh (default 30s; disk changes slowly)
- `diskMount`    — which volume to track (default `/`)

## Stack

- [caseymrm/menuet](https://github.com/caseymrm/menuet) — menu bar UI
- [shirou/gopsutil](https://github.com/shirou/gopsutil) — system stats
