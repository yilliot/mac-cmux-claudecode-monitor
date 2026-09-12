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
**hooks** (not CPU guessing), so it's accurate in cmux, iTerm, or any terminal,
and it tracks every concurrent session rather than just the most recent one.

- **Off** — does nothing; the Mac sleeps normally.
- **On** — holds a `caffeinate` assertion *only while Claude is running*. The
  top-bar shows `◦` when watching an idle Claude and `☕️` while it's running and
  the Mac is held awake.

### One-time setup: install the hooks

```sh
./install-hooks.sh          # requires jq
```

This installs `hook.sh` to `~/.macmonitor/` and **appends** these hooks to
`~/.claude/settings.json`, leaving any hooks you already have in place (a
timestamped backup is made first, and re-running replaces only its own entries):

| Event | Writes | Means |
|---|---|---|
| `SessionStart` | `idle` | session opened |
| `UserPromptSubmit`, `PreToolUse`, `PostToolUse` | `running` | Claude is working |
| `Stop` | `idle` | Claude finished its turn |
| `Notification` (`idle_prompt`/`permission_prompt`) | `idle` | waiting on you |
| `SessionEnd` | *(removes the file)* | session closed |

Each session gets **its own file** under `~/.macmonitor/sessions/`, named by the
session id that Claude Code passes to the hook on stdin. That's what makes
concurrent sessions safe: the Mac is held awake while *any* session is running,
so one cmux tab finishing can't drop the assertion out from under another that's
still working.

Restart any open Claude Code sessions afterward so the hooks load. MacMonitor
polls every 2s; no detection runs until you flip the toggle On.

### Failure modes it handles

- **Quit / crash / force-kill.** `caffeinate` is started with `-w <our pid>`, so
  it exits with the app. Nothing is left holding a power assertion — quitting via
  the menu calls `[NSApp terminate:]`, which skips Go cleanup entirely, so the
  child has to be responsible for its own exit.
- **A session that dies without running its hooks.** A `running` marker older
  than 30 minutes (`staleAfter` in `main.go`) stops counting, so a killed
  terminal can't pin the Mac awake. The threshold is deliberately generous:
  `PreToolUse` stamps the file when a tool *starts* and `PostToolUse` only
  re-stamps when it finishes, so a long build must not look stale mid-run.
  Abandoned files are deleted after 24h.
- **The toggle** is remembered across restarts (`NSUserDefaults`).

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
- `staleAfter`   — how long a `running` marker is trusted (default 30m)
- `pruneAfter`   — when abandoned session files are deleted (default 24h)

## Stack

- [caseymrm/menuet](https://github.com/caseymrm/menuet) — menu bar UI
- [shirou/gopsutil](https://github.com/shirou/gopsutil) — system stats
