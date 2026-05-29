---
name: Agent selection
description: Pre-spawn checklist for subagent orchestration — provider selection by reset-imminence and headroom, model and effort right-sizing by task scope, and autonomous-invocation patterns per CLI.
type: instruction
---

## Before every spawn

### 1. Read

Run `aistat usage` for all providers, `aistat usage <provider>` for one.

### 2. Pick a provider

Order of preference:

1. **Reset-imminence wins.** Capacity about to reset is free — prefer providers whose reset window is shorter than the task's expected duration.
2. **Else, headroom.** Pick the provider with most slack on its tightest window

**Your own usage is paramount.** Running out mid-orchestration leaves the user with a half-finished workflow and no clean recovery. When your provider is tight, prefer a different one over driving yourself into the cap.

If the chosen provider is Claude but the active account is tight while another stored account is fresh: `aistat switch` rotates without a browser round-trip.

*Example:* Codex 92% used resets in 30 min; Claude 30% used resets in 4 hours. For a 10-min task → Codex — its 8% resets to 100% regardless.

### 3. Right-size the model

Don't overspend on intelligence. Default behavior on every provider inherits the parent's flagship-tier model. **Specify per spawn.** Each cell: `model · effort`. Left is cost-efficient, right is quality-first.

| Scope | Claude¹ | Codex | Copilot |
|---|---|---|---|
| **Trivial** — single-file, one-concern, light analysis | `haiku · medium` / `sonnet · low` | `gpt-5.4-mini · medium` / `gpt-5.4 · low` | `claude-haiku-4.5 · medium` / `gpt-5.4-mini · medium` |
| **Standard** — multi-file, bounded dependency mapping | `sonnet · medium` / `opus · low` | `gpt-5.4 · medium` / `gpt-5.5 · low` | `claude-sonnet-4.6 · medium` / `gpt-5.4 · medium` |
| **Heavy** — cross-cutting, architecture, novel surface | `sonnet · high` / `opus · high` | `gpt-5.5 · high` / `gpt-5.5 · xhigh` | `claude-sonnet-4.6 · xhigh` / `gpt-5.4 · xhigh` |

¹ Claude's `Agent` tool doesn't accept `--effort` — use `claude -p ... --effort <level>` from the Bash tool to spawn a full CLI subagent with reasoning control.

**Effort scale:** Claude uses `low | medium | high | max`. Codex and Copilot use `low | medium | high | xhigh` (no `max`).


## Invocation

One-shot autonomous patterns per CLI:

```bash
# Claude
claude -p "PROMPT" --permission-mode bypassPermissions --model sonnet --effort high

# Codex
codex exec --dangerously-bypass-approvals-and-sandbox -m gpt-5.4 -c model_reasoning_effort="high" "PROMPT"

# Copilot (standalone `copilot` or `gh copilot -- …`; with `gh`, the `--` separator is required)
copilot -p "PROMPT" --allow-all --autopilot --model claude-sonnet-4.6 --effort high
```

### Key flags

- **Claude:** `-p "prompt"` non-interactive · `--permission-mode bypassPermissions` (canonical autonomy mode; also propagates to subagents — `--allowedTools '*'` does NOT) · `--model <alias|id>` · `--effort low|medium|high|max` · `--add-dir <path>` extra file access. The `Agent` tool itself accepts only `model: haiku|sonnet|opus` — no effort. For reasoning control on a subagent, spawn via `claude -p ... --effort <level>` from the Bash tool. Subagents spawned via the `Agent` tool get a reduced toolset (no `Agent` tool — can't nest); subagents spawned via `claude -p` are full top-level sessions.
- **Codex:** `--dangerously-bypass-approvals-and-sandbox` for full autonomy; `-s read-only|workspace-write|danger-full-access` for granular. `-m <model>`. `-c model_reasoning_effort="low|medium|high|xhigh"` (config-only — no dedicated flag). `minimal` is parsed by the config layer but rejected by the model — don't use it.
- **Copilot:** `-p "prompt"` non-interactive · `--autopilot` (required for non-interactive automation) · `--allow-all` (or granular `--allow-all-tools` / `--allow-all-paths` / `--allow-all-urls`) · `--model <model>` · `--effort low|medium|high|xhigh` (alias `--reasoning-effort`). Nothing will stop Copilot from implementing once `--allow-all --autopilot` are set — **reduce permissions to the minimum required, always.**