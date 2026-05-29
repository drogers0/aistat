#!/bin/sh
# auto-switch.sh — rotate the live Claude credential to a fresher stored
# account if the active account's 5-hour usage is above THRESHOLD (default 80%).
# No-op if there's only one stored account or no fresher one exists.
#
# Usage:
#   ./auto-switch.sh              # threshold 80
#   THRESHOLD=60 ./auto-switch.sh # custom threshold
#
# Wire as a Claude Code SessionStart hook so every new session starts on the
# freshest stored account. Drop the script anywhere stable (e.g.
# ~/.claude/hooks/auto-switch.sh), chmod +x it, and add to ~/.claude/settings.json:
#
#   {
#     "hooks": {
#       "SessionStart": [
#         {
#           "hooks": [
#             { "type": "command", "command": "~/.claude/hooks/auto-switch.sh" }
#           ]
#         }
#       ]
#     }
#   }
#
# SessionStart hooks don't block session start regardless of exit code, so a
# stale aistat read or transient switch failure never strands the user.

set -eu

threshold="${THRESHOLD:-80}"

active_used=$(aistat usage claude 2>/dev/null | jq -r '
  .providers.claude.accounts // []
  | map(select(.active == true) | .limits.five_hour.used_percent // 0)
  | first // 0')

# jq prints floats; awk handles the comparison.
if awk "BEGIN { exit !($active_used > $threshold) }"; then
  echo "auto-switch: active Claude account at ${active_used}% (> ${threshold}%); switching"
  aistat switch
else
  echo "auto-switch: active Claude account at ${active_used}% (<= ${threshold}%); staying put"
fi
