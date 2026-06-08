#!/bin/bash
# install-hooks.sh — wire Claude Code's lifecycle hooks into the keep-awake
# detection used by MacMonitor.
#
# The hooks write a one-word state ("running"/"idle") to ~/.macmonitor/claude-state.
# MacMonitor reads that file: when its "Keep Awake while Claude runs" toggle is On,
# it holds a caffeinate assertion only while Claude is actively working — in cmux,
# a plain terminal, anywhere Claude Code runs. The hooks are Claude Code's own, so
# they fire regardless of which terminal hosts the session.
#
# Idempotent: re-running it just rewrites the same hook entries. A timestamped
# backup of settings.json is made first.
set -euo pipefail

SETTINGS="${HOME}/.claude/settings.json"
STATE_DIR="${HOME}/.macmonitor"
STATE_FILE="${STATE_DIR}/claude-state"

command -v jq >/dev/null || { echo "jq is required (brew install jq)"; exit 1; }

mkdir -p "${STATE_DIR}"
echo idle > "${STATE_FILE}"            # start from a known-idle baseline
mkdir -p "$(dirname "${SETTINGS}")"
[ -f "${SETTINGS}" ] || echo '{}' > "${SETTINGS}"

cp "${SETTINGS}" "${SETTINGS}.bak.$(date +%s)"

RUN="echo running > ${STATE_FILE}"
IDLE="echo idle > ${STATE_FILE}"

# Merge our hook entries into whatever hooks already exist (last-writer wins on
# these specific events). Running on prompt-submit and around every tool call;
# idle on Stop and on idle/permission notifications.
jq \
  --arg run  "${RUN}" \
  --arg idle "${IDLE}" \
  '.hooks = (.hooks // {})
   | .hooks.UserPromptSubmit = [{"hooks":[{"type":"command","command":$run}]}]
   | .hooks.PreToolUse       = [{"hooks":[{"type":"command","command":$run}]}]
   | .hooks.PostToolUse      = [{"hooks":[{"type":"command","command":$run}]}]
   | .hooks.Stop             = [{"hooks":[{"type":"command","command":$idle}]}]
   | .hooks.Notification     = [{"matcher":"idle_prompt|permission_prompt","hooks":[{"type":"command","command":$idle}]}]' \
  "${SETTINGS}" > "${SETTINGS}.tmp"

mv "${SETTINGS}.tmp" "${SETTINGS}"

echo "Installed Claude Code keep-awake hooks into ${SETTINGS}"
echo "State file: ${STATE_FILE} (currently: $(cat "${STATE_FILE}"))"
echo "Restart any running Claude Code sessions so the new hooks load."
