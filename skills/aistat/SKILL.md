---
name: aistat
description: >-
  Check and manage LLM usage limits (Claude, Codex, Copilot) via the `aistat`
  CLI. Use when the user asks how much headroom they have left, which account
  to use, or to rotate to a fresher account before hitting a limit.
allowed-tools: Bash(aistat usage:*), Bash(aistat accounts list:*)
---

# aistat — read usage and switch accounts

`aistat` reports Claude, Codex, and Copilot usage limits and can switch the live
credential between stored accounts. JSON is the default output; `-h`/`--human`
gives a readable rendering.

## When to use

- The user asks how much usage / headroom they have left on Claude, Codex, or Copilot.
- The user asks which account to use, or whether they're about to hit a limit.
- The user asks to rotate to a fresher account before a limit bites.

## Reading usage

Run `aistat usage` for all providers, or `aistat usage <claude|codex|copilot>`
for one. JSON is the default and is what you should parse.

**Two output shapes — detect, don't assume.** For each provider under
`providers.<name>`:

- If an `accounts` array is present, iterate it and read each row's own
  `limits.<window>` (this is how **Claude and Codex** render — always per-account,
  even with a single stored account; each row carries `email`, `active`, `plan`).
  Claude rows may additionally carry `address`, `organization_name`, and
  `organization_type`. Treat these as additive optional fields.
- Otherwise read `providers.<name>.limits.<window>` directly (this is how
  **Copilot** renders — a single flat map, no `accounts`).

Claude can have several contexts with the same email. The printed full
`address` is the canonical selector for one of them. A `claude_max` or
`claude_pro` row uses `<email>/personal-<short-account-id>` and may still carry
its real organization name and type. A team, unknown, or empty organization
type uses `<email>/<slug>-<short-org-id>`. The UUID-hex suffix is the identity;
the slug is cosmetic, and email is only a tie-breaker when duplicate
organization UUID prefixes require it. `personal-<hex>` is exclusively the
personal account-UUID form. A unique organization slug, email substring, or
UUID prefix is also accepted, but use the printed full address when selecting
among same-email contexts. `accounts list` is where to read it.

After `claude /login`, run `aistat usage` to capture the newly active context.
Claude organization identity comes from the profile endpoint, not from the
opaque credential blob. A nil organization is only a defensive wire case, not
the observed shape of a personal Max response.

Each window carries:

- `used_percent` — how much of the window is consumed. **Higher = less headroom.**
- `remaining_percent` — `100 - used_percent`.
- `reset_after_seconds` — time until the window refills.

## Window keys (they differ per provider)

- **Claude:** `five_hour` (short burst), `seven_day` (sustained), plus
  informational `seven_day_<model>` (e.g. `seven_day_sonnet`).
- **Codex:** `five_hour`, `seven_day`, `thirty_day`, plus `code_review_<window>`
  sub-limits. An unrecognized duration falls back to a generic `window_<N>s`
  key — treat any key you don't recognize as just another window keyed by its
  own `used_percent`.
- **Copilot:** `month` only. No 5-hour / weekly tiers, so the rotation heuristic
  below doesn't apply to Copilot.

## Rotating accounts (mutating — gated)

Switching mutates the live credential. **Prefer the conditional form**, which
only switches when a threshold is crossed and otherwise prints `no switch
needed`:

```
aistat switch <provider> --if-above-5h <N> --if-above-weekly <N>
```

A sensible trigger is `--if-above-5h 85 --if-above-weekly 95` (the CLI's own
defaults). When warranted it auto-picks the account with the most headroom.

**Do not run a bare, unconditional `aistat switch` on your own.** It rotates the
live credential immediately, with no threshold gate. Run it only when the user
has explicitly asked to force a rotation, and expect a permission prompt (it is
deliberately not in this skill's allowed tools).

## Fail closed — surface recovery commands

If `aistat` exits non-zero, it names the exact command to recover (e.g.
`claude /login`, `codex login`, `gh auth refresh -s user`). Relay that command
to the user verbatim rather than retrying blindly.
