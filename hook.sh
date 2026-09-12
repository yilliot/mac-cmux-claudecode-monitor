#!/bin/sh
# hook.sh — invoked by Claude Code's lifecycle hooks; installed to
# ~/.macmonitor/hook.sh by install-hooks.sh.
#
# usage: hook.sh running | idle | end
#
# Records this session's state in its own file under ~/.macmonitor/sessions/,
# named by the session id. MacMonitor holds the keep-awake assertion while ANY
# session file says "running", so concurrent sessions (cmux tabs) don't release
# it out from under each other.
#
# Claude Code passes hook context as JSON on stdin and sets no session-id
# environment variable, so the id is read from stdin's "session_id" field.
set -u

state="${1:-idle}"
dir="${HOME}/.macmonitor/sessions"

# Read the session id from the hook payload. Falls back to a shared "unknown"
# file if jq is missing or the payload has no id — degraded (sessions collide
# again) but never worse than doing nothing.
sid=""
if command -v jq >/dev/null 2>&1; then
	sid=$(jq -r '.session_id // empty' 2>/dev/null)
fi
case "$sid" in
	"" | */* | .*) sid="unknown" ;;  # reject empty and anything path-like
esac

mkdir -p "$dir" || exit 0

if [ "$state" = "end" ]; then
	rm -f "${dir}/${sid}"
else
	printf '%s\n' "$state" > "${dir}/${sid}"
fi

exit 0
