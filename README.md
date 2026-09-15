# MacMonitor

A lightweight macOS menu bar app (Go) showing live **memory** and **disk** usage
in the top-right status bar, plus a **Keep Awake while Claude Code runs** toggle
and an On/Off switch for the **production database tunnel**.
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
s:234🔌  ← storage free (GB), bottom line; 🔌/…/⚠️/● prod-tunnel marker (see below)
```

Click it for the menu: the Keep Awake toggle, the Prod Tunnel switch with the
jump host's state, and full memory/disk details.

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

## Prod Tunnel switch

The production database is private; the only laptop path to it is
`~/herd/commun/scripts/prod-tunnel.sh`, which starts the SSM jump host on
demand, forwards RDS 3306 to `127.0.0.1:13306`, and stops the host again when
the tunnel closes. The **Prod Tunnel (commun-nat)** menu item is an On/Off
switch for that script, so you don't need a terminal pinned open for it:

- **On** — runs the script. The menu shows its progress (`starting…`,
  `waiting for the SSM agent…`), then `tunnel up: 127.0.0.1:13306 → RDS 3306`.
- **Off** — the Ctrl+C equivalent: the script's process group gets `SIGINT`, so
  its `EXIT` trap stops the host again. If the trap hasn't finished after 60s
  the group is killed and `--stop` is run explicitly, so the host can't be left
  running unnoticed.
- **Quit** (or `SIGTERM`) with the tunnel up closes it the same way first.

Under the switch, two lines show the instance and what's happening:

```
  i-0d75cf550071ee354: running · ssm Online (12s ago)   ← EC2 state, polled every 30s via --status
  tunnel up: 127.0.0.1:13306 → RDS 3306                 ← what the switch is doing
```

In-progress phases show their elapsed time (`starting… 1m38s · waiting for the
SSM agent…`), so a stall is visible rather than looking frozen. With no network
the host line reads `unreachable — <aws CLI reason>` and On simply waits until
the script's first AWS call gets through, then proceeds as normal.

The instance id, name and port are parsed from the script, so the app has no
AWS ids of its own. When the host is running but the switch is Off (e.g. after
`--keep` in a terminal), a **Stop host now** item appears, since that's the
state that quietly bills.

Top-bar marker on the storage line: `🔌` tunnel up, `…` starting/stopping,
`⚠️` the script exited with an error (click the switch to retry; the menu shows
its last line), `●` host running with no tunnel of ours, nothing when off.

Set `MACMONITOR_TUNNEL_SCRIPT=/path/to/script.sh` to point the switch at a
different script with the same flags (`--status`, `--stop`).

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
- `tunnelPoll`   — how often the jump host's EC2 state is polled (default 30s)
- `tunnelStopWait` — how long Off waits for the script to stop the host before forcing it (default 60s)

## Stack

- [caseymrm/menuet](https://github.com/caseymrm/menuet) — menu bar UI
- [shirou/gopsutil](https://github.com/shirou/gopsutil) — system stats
