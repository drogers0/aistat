<h1 align="center">aistat</h1>

<p align="center">
  <em>Usage limits and instant account switching for Claude, Codex, and Copilot — from one terminal command, turning scattered LLM logins into one coordinated budget.</em>
</p>

<p align="center">
  <a href="https://github.com/drogers0/aistat/releases/latest"><img src="https://img.shields.io/github/v/release/drogers0/aistat?color=blue" alt="Latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/drogers0/aistat?color=lightgrey" alt="License: MIT"></a>
  <a href="https://goreportcard.com/report/github.com/drogers0/aistat/v2"><img src="https://goreportcard.com/badge/github.com/drogers0/aistat/v2" alt="Go Report Card"></a>
</p>

---

A command line utility that reads your **Claude**, **Codex**, and **Copilot** usage limits from the same credential stores those tools already use — and switches between stored accounts without a browser round-trip.

```console
$ aistat -h
Claude usage
- personal@example.com (active) [Max 5x]
  - 5-hour: 92.0% (resets in 4h 53m)
  - 7-day: 71.0% (resets in 2d 5h)
  - 7-day sonnet: 58.0% (resets in 2d 5h)
- work@example.com [Max 20x]
  - 5-hour: 4.0% (resets in 4h 12m)
  - 7-day: 12.0% (resets in 5d 9h)

Codex usage
- 5-hour: 2.0% (resets in 2h 26m)
- 7-day: 0.0% (resets in 6d 21h)
- Code review 7-day: 0.0% (resets in 6d 21h)

Copilot usage
- month: 67.3% (resets in 5d 1h)
```

One command rotates the live Claude credential to the fresh account — no browser round-trip:

```console
$ aistat switch
switched to work@example.com (uuid 1a2b3c4d-…); was personal@example.com
```

## What this unlocks

`aistat` is built for **usage-aware agent routing**: pick which provider to spawn the next subtask on by which one will waste capacity if you don't use it, rather than by which one is your default. The JSON output is the routing primitive — orchestrators read it, score the candidates, and dispatch.

https://github.com/user-attachments/assets/83c6c361-c73f-4ac1-aec4-72d901752b56

<p align="center"><sub><em>Claude spawns a Codex subagent — picked because <code>aistat</code> shows Codex with the most headroom.</em></sub></p>

Three runnable examples in [`examples/`](examples/) put the pattern to work:

- **[`agent-selection.md`](examples/agent-selection.md)** — let your LLM agent manage its own usage quotas, turning your scattered LLM logins into one coordinated budget the agent draws from automatically.
- **[`route.sh "your prompt"`](examples/route.sh)** — picks the provider with the most headroom and runs the prompt against that CLI (`claude -p` / `codex exec` / `copilot -p`).
- **[`auto-switch.sh`](examples/auto-switch.sh)** — rotates the live Claude credential to a fresh stored account when the active one is past a threshold. Drop into Claude Code's `~/.claude/settings.json` as a `SessionStart` hook to start every session on your freshest slot.

`aistat` is the input; the workflow is the rest.

## Installation

```bash
curl -fsSL https://raw.githubusercontent.com/drogers0/aistat/main/install.sh | sh
```

Prebuilt releases ship for **macOS** (arm64, amd64) and **Linux** (amd64, arm64). Windows may work but isn't currently supported.

<details>
<summary>Other install options</summary>

**Pin a specific version:**

```bash
AISTAT_VERSION=v2.1.0 curl -fsSL https://raw.githubusercontent.com/drogers0/aistat/main/install.sh | sh
```

**Choose an install directory:**

```bash
curl -fsSL https://raw.githubusercontent.com/drogers0/aistat/main/install.sh | sh -s -- --prefix=$HOME/bin
```

**`go install` (requires Go 1.22+):**

```bash
go install github.com/drogers0/aistat/v2/cmd/aistat@latest
```

**Manual download:** grab a tarball from the [Releases page](https://github.com/drogers0/aistat/releases) and place `aistat` on your PATH.

</details>

## Usage

```bash
aistat                            # default: same as `aistat usage`
aistat usage                      # report usage across all providers/accounts
aistat usage <provider>           # report a single provider (claude | codex | copilot)
aistat switch                     # auto-switch claude to the account with the most headroom
aistat switch --to <email|uuid>   # switch to a specific stored claude account
aistat accounts list              # list stored claude accounts
aistat accounts remove <id>       # remove a stored claude account (email substring or uuid prefix)
```

Flags: `-h`/`--human` for text rendering (affects `usage` only), `--refresh` to bypass the per-account Claude usage cache (~90 s TTL, affects `usage` only), `--debug` for per-request diagnostics on stderr, `--version` and `--help` for the obvious.

## Multiple accounts

Whichever account is active when you call `aistat` gets stored automatically. After a `claude /login`, the next `aistat usage` adds it alongside the others — no extra setup, no separate command.

`aistat accounts list` shows every stored account, `aistat accounts remove <id>` deletes one (the currently-active account is protected — switch away with `aistat switch --to <email>` or run logout first).

`aistat switch` is the only command that mutates a live credential; `aistat usage` is read-only.

### aistat switch

`aistat switch` rotates the live credential to a different stored account — no browser round-trip:

- **Auto-pick** (`aistat switch`): picks the stored account with the most 5-hour headroom.
- **Explicit** (`aistat switch --to <email|uuid>`): match by email substring or UUID prefix.

Auto-pick buckets candidates by 5% (so 87% and 89% are equivalent) and breaks ties by most-recent use. It optimizes **relative headroom**, not "has enough quota for the workload you're about to start" — for nuanced cases, pass `--to` explicitly.


> [!NOTE]
> Multi-account support is currently Claude-only — Codex and Copilot ride on whatever single-account credential their upstream CLI writes.

> [!WARNING]
> `aistat switch` rotates the credential for the whole device, not per session. Every Claude Code session sharing this credential picks up the new account on its next read — there's no per-session or per-chat isolation.

## Authentication

`aistat` reads from the credential stores each tool already populates. If a credential is missing, the error message names the exact command to fix it.

| Provider | Set up with |
|----------|-------------|
| Claude   | `claude /login` |
| Codex    | `codex login` |
| Copilot  | `gh auth login` + `gh auth refresh -h github.com -s user` (the `user` scope is required) |

## How it works

`aistat` reads the credentials `claude /login`, `codex login`, and `gh auth login` already wrote, makes one authenticated HTTPS call per provider in parallel, and normalizes each response into a uniform `{used_percent, remaining_percent, resets_at}` shape. A failing provider doesn't block the others — its error surfaces in the JSON, and a single per-account hiccup on multi-account Claude doesn't flip the overall exit code. Claude usage is cached for 90 seconds so script-driven polling doesn't burn rate limit.

<details>
<summary>Endpoints, caching, retries, exit codes, and the JSON contract</summary>

**Endpoints.**

| Provider | Endpoints |
|----------|-----------|
| Claude   | `api.anthropic.com/api/oauth/usage`, `api.anthropic.com/api/oauth/profile`, `platform.claude.com/v1/oauth/token` |
| Codex    | `chatgpt.com/backend-api/wham/usage` |
| Copilot  | `api.github.com/users/{login}/settings/billing/premium_request/usage` |

**Caching.** Each Claude account's usage response is cached for 90 seconds so back-to-back invocations don't hammer Anthropic's rate limit. `aistat usage --refresh` bypasses the cache; `aistat switch` reads through it, so refresh first if you want a switch decision based on the freshest numbers. Override the TTL with `AISTAT_USAGE_CACHE_TTL=10s` (or any duration). If the cache can't be written, the run proceeds without it.

**User-Agent.** The Claude provider sends `User-Agent: claude-code/<version>` on the wire — Anthropic's `/oauth/usage` endpoint aggressively throttles non-`claude-code/` clients ([anthropics/claude-code#31637](https://github.com/anthropics/claude-code/issues/31637)). Override with `AISTAT_CLAUDE_USER_AGENT=<string>` (verbatim, e.g. `aistat/2.1.0`) to opt back into the honest UA. Sibling vars exist for the other providers (`AISTAT_CODEX_USER_AGENT`, `AISTAT_COPILOT_USER_AGENT`); those default to `aistat/<version>` since their endpoints don't currently appear to partition by UA.

**Reliability.** Transient HTTP failures (408, 429, 5xx, network errors) retry up to 3 times per request, honoring `Retry-After` (capped at 10s) and otherwise backing off with jitter. The CLI never blocks longer than 15 seconds per Claude account, and a single per-account failure doesn't flip the overall exit code — it's surfaced in the JSON and resolves on the next run.

**Exit codes.**

| Code | Meaning |
|------|---------|
| 0 | All providers succeeded. |
| 1 | One or more providers failed at runtime. |
| 2 | Usage error: unknown subcommand, unknown provider, malformed flags. |
| 3 | Stdout write error (broken pipe, disk full). |

**Diagnostics on stderr.** Even without `--debug`, providers may emit diagnostic lines to stderr. All start with `aistat:`.

```
aistat: copilot: Copilot-product usageItems present but none matched
  sku="Copilot Premium Request" — GitHub may have renamed the SKU; please
  file an issue at https://github.com/drogers0/aistat/issues

aistat: claude: could not capture live account profile (<reason>); rendering live row
  without storing — run `claude /login` if this persists across runs

aistat: claude: <email>: stored credential rejected (run `aistat usage` to refresh);
  excluded from auto-pick

aistat: claude: refresh endpoint rejected request (<status>: <body-snip>); this is
  likely an aistat refresh implementation issue, not your account. Run `claude /login`
  to work around it for this account and file an issue at https://github.com/drogers0/aistat/issues
```

The exit code and stdout payload are unaffected — these are heads-ups that the underlying number may be stale, or that one account is excluded from auto-pick. With `--debug`, additional per-request and per-provider lines are also written to stderr.

**JSON output contract.**

```json
{
  "checked_at": "2026-05-28T01:00:00+00:00",
  "providers": {
    "claude": {
      "accounts": [
        { "email": "personal@example.com", "plan": "default_claude_max_5x",  "active": true,  "limits": {...} },
        { "email": "work@example.com",     "plan": "default_claude_max_20x", "active": false, "limits": {...} }
      ]
    },
    "codex":   { "limits": {...} },
    "copilot": { "limits": { "month": {...} } }
  }
}
```

For Claude, `accounts` is the only view — the row with `active: true` carries the live account's limits. Codex and Copilot stay single-account: each emits a top-level `limits` and no `accounts`. Every `Limit` has the same four fields: `used_percent`, `remaining_percent`, `resets_at` (ISO 8601), `reset_after_seconds`. UUIDs surface in `aistat accounts list` and `aistat switch` output — that's where you read them when you want `accounts remove <uuid-prefix>` or `switch --to <uuid-prefix>`.

</details>

## Contributing

Issues and pull requests are welcome. Before opening a PR, run `go test ./...`, `go vet ./...`, and `staticcheck ./...`.

## Support

If `aistat` saves you a tab-switch, a ⭐ helps others find it:

```bash
gh api --method PUT user/starred/drogers0/aistat
```

(or just click the star at the [top of this page](https://github.com/drogers0/aistat))

## License

[MIT](LICENSE) © 2026 drogers0
