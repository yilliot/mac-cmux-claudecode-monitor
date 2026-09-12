#!/bin/bash
# install-hooks.sh — wire Claude Code's lifecycle hooks into the keep-awake
# detection used by MacMonitor.
#
# The hooks run ~/.macmonitor/hook.sh, which records each session's state in its
# own file under ~/.macmonitor/sessions/. MacMonitor reads that directory: when
# its "Keep Awake while Claude runs" toggle is On, it holds a caffeinate
# assertion while ANY session is actively working — in cmux, a plain terminal,
# anywhere Claude Code runs. The hooks are Claude Code's own, so they fire
# regardless of which terminal hosts the session.
#
# Idempotent and non-destructive: our entries are appended to whatever hooks you
# already have, and re-running replaces only our own previous entries. A
# timestamped backup of settings.json is made first either way.
set -euo pipefail

SETTINGS="${HOME}/.claude/settings.json"
STATE_DIR="${HOME}/.macmonitor"
SESSION_DIR="${STATE_DIR}/sessions"
HOOK_SH="${STATE_DIR}/hook.sh"
SRC_HOOK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/hook.sh"

command -v jq >/dev/null || { echo "jq is required (brew install jq)"; exit 1; }
[ -f "${SRC_HOOK}" ] || { echo "hook.sh not found next to this script"; exit 1; }

mkdir -p "${SESSION_DIR}"
install -m 0755 "${SRC_HOOK}" "${HOOK_SH}"

# The pre-per-session state file is no longer used; its presence would only make
# MacMonitor fall back to the old shared-file behaviour.
rm -f "${STATE_DIR}/claude-state"

mkdir -p "$(dirname "${SETTINGS}")"
[ -f "${SETTINGS}" ] || echo '{}' > "${SETTINGS}"

BACKUP="${SETTINGS}.bak.$(date +%s)"
cp "${SETTINGS}" "${BACKUP}"

BEFORE=$(jq '[.hooks // {} | .[]? | .[]? | .hooks[]?] | length' "${SETTINGS}")

# Append our entries, having first stripped any we installed previously (either
# the current hook.sh form or the original inline "echo ... > claude-state").
# Everyone else's hooks are left untouched.
jq \
  --arg hook "${HOOK_SH}" \
  '
  def mine($cmd): (($cmd // "") | test("\\.macmonitor/(hook\\.sh|claude-state)"));
  def clean($ev):
    ((.hooks[$ev] // [])
      | map(.hooks = ((.hooks // []) | map(select(mine(.command) | not))))
      | map(select((.hooks | length) > 0)));
  # RHS is evaluated against the root object, so clean/$ev resolve correctly.
  def add($ev; $entry): .hooks[$ev] = (clean($ev) + [$entry]);
  def cmd($arg): {"hooks":[{"type":"command","command":($hook + " " + $arg)}]};

    .hooks = (.hooks // {})
  | add("SessionStart";     cmd("idle"))
  | add("UserPromptSubmit"; cmd("running"))
  | add("PreToolUse";       cmd("running"))
  | add("PostToolUse";      cmd("running"))
  | add("Stop";             cmd("idle"))
  | add("SessionEnd";       cmd("end"))
  | add("Notification";     (cmd("idle") + {"matcher":"idle_prompt|permission_prompt"}))
  ' "${SETTINGS}" > "${SETTINGS}.tmp"

# Never leave a broken settings.json behind.
jq -e . "${SETTINGS}.tmp" >/dev/null || { echo "generated settings.json was invalid; left original in place"; rm -f "${SETTINGS}.tmp"; exit 1; }
mv "${SETTINGS}.tmp" "${SETTINGS}"

AFTER=$(jq '[.hooks // {} | .[]? | .[]? | .hooks[]?] | length' "${SETTINGS}")

echo "Installed Claude Code keep-awake hooks into ${SETTINGS}"
echo "  hook script:  ${HOOK_SH}"
echo "  session dir:  ${SESSION_DIR}"
echo "  backup:       ${BACKUP}"
echo "  hook commands: ${BEFORE} before -> ${AFTER} after (yours were kept)"
echo "Restart any running Claude Code sessions so the new hooks load."
