#!/bin/sh
# route.sh — read `aistat`, pick the provider with the most headroom on its
# tightest window, and run a one-off prompt against that provider's CLI.
#
# Usage:
#   ./route.sh "your prompt here"
#
# Defaults to a Standard-tier model at medium reasoning effort on each provider.
# Edit the dispatch lines below to bias differently.

set -eu

if [ $# -lt 1 ]; then
  echo "usage: $0 \"prompt\"" >&2
  exit 2
fi

prompt="$*"

json=$(aistat usage 2>/dev/null)

claude_remaining=$(printf '%s' "$json" | jq -r '
  .providers.claude.accounts // []
  | map(select(.active == true) | .limits.five_hour.remaining_percent // 0)
  | first // 0')
codex_remaining=$(printf '%s' "$json" | jq -r '
  .providers.codex.limits.five_hour.remaining_percent // 0')
copilot_remaining=$(printf '%s' "$json" | jq -r '
  .providers.copilot.limits.month.remaining_percent // 0')

best="claude"; best_score="$claude_remaining"
[ "$(printf '%s\n%s\n' "$codex_remaining"   "$best_score" | sort -n | tail -1)" = "$codex_remaining"   ] && { best="codex";   best_score="$codex_remaining"; }
[ "$(printf '%s\n%s\n' "$copilot_remaining" "$best_score" | sort -n | tail -1)" = "$copilot_remaining" ] && { best="copilot"; best_score="$copilot_remaining"; }

case "$best" in
  claude)
    echo "<running with claude>" >&2
    exec claude -p "$prompt" --permission-mode bypassPermissions --model sonnet --effort medium
    ;;
  codex)
    echo "<running with codex>" >&2
    exec codex exec --dangerously-bypass-approvals-and-sandbox -m gpt-5.4 -c 'model_reasoning_effort="medium"' "$prompt"
    ;;
  copilot)
    echo "<running with copilot>" >&2
    exec copilot -p "$prompt" --allow-all --autopilot --model claude-sonnet-4.6 --effort medium
    ;;
esac
