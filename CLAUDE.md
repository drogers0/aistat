# aistat

A single static Go binary that reports Claude, Codex, and Copilot usage limits from the terminal — and (Claude only) switches the live keychain credential between stored Claude accounts without a browser round-trip. JSON by default; `-h`/`--human` for the nested text rendering.

CLI surface (as of v2.1.0 — the positional `aistat <provider>` form is gone):

```
aistat                            # default: same as `aistat usage`
aistat usage [provider]           # report usage across all providers/accounts
aistat switch [--to <email|uuid>] # switch live Claude credential (auto-picks most headroom)
aistat accounts list              # list stored Claude accounts
aistat accounts remove <id>       # remove a stored Claude account
```

## Repo layout

- [cmd/aistat/](cmd/aistat/) — `main`, hand-rolled subcommand dispatch (`scanGlobals` + per-subcommand FlagSets), provider registry, fake-provider hooks. One file per subcommand: [main.go](cmd/aistat/main.go), [usage.go](cmd/aistat/usage.go), [switch.go](cmd/aistat/switch.go), [accounts.go](cmd/aistat/accounts.go).
- [internal/providers/](internal/providers/) — one subpackage per provider (`claude`, `codex`, `copilot`). Each owns its credential source, HTTP calls, and response normalization into the shared `Limit` type in [types.go](internal/providers/types.go). `AccountResult` lives here too — same type carried on both `ProviderOutput` (in-process) and `ProviderResult` (JSON-serialized).
- [internal/providers/claude/](internal/providers/claude/) — Claude's multi-account machinery: [claude.go](internal/providers/claude/claude.go) (Fetch + FetchForSwitch + cache wiring via `fetchLimitsCached` / `fetchLimitsFresh`), [profile.go](internal/providers/claude/profile.go) (identity endpoint), [refresh.go](internal/providers/claude/refresh.go) (OAuth refresh), [reconcile.go](internal/providers/claude/reconcile.go) (pure decision tree: byte-match → profile lookup → fallback), [usagecache.go](internal/providers/claude/usagecache.go) (90 s file-backed usage cache, keyed by `account.uuid`).
- [internal/accounts/](internal/accounts/) — multi-account credential store. Platform-specific backends (`store_darwin.go` keychain + process flock on `$CACHE/aistat/store.lock`; `store_linux.go` JSON file with flock on `.claude.lock` sentinel — NOT the data file, to survive atomic-rename). `MemoryStore` for tests.
- [internal/render/](internal/render/) — `json` and `text` renderers. The JSON shape is the public contract; the text renderer is a thin presentation layer over the same model. For Claude, the renderer routes on `len(result.Accounts) > 0`: nested per-account view when populated, legacy flat view otherwise.
- [internal/cred/](internal/cred/) — credential lookup AND the `WriteClaudeLiveBlob` writer that `aistat switch` uses. `Credential.Raw []byte` preserves the verbatim Claude credential JSON (including fields aistat doesn't parse) so a switch re-publishes byte-for-byte what the Claude CLI wrote. The darwin writer uses a `runSecurity` seam so tests can mock without touching the real keychain.
- [internal/httpx/](internal/httpx/) — shared HTTP transport: `Doer.GetJSON` (Authorization-reserved) + `Doer.PostForm` (no Authorization by default — used by the refresh client) sharing an unexported `setCommonHeaders`/`do` split. `do` runs a bounded retry loop (max 3 attempts) that honors `Retry-After` (capped at 10s) on transient classifications, falling back to exponential backoff with ±20% jitter. `Classifier` takes `*http.Response` so callers can inspect headers.
- [internal/orchestrate/](internal/orchestrate/) — parallel fan-out across providers; one failing provider does not block the others. Preserves per-account rows on provider-level error (D8 contract).
- [internal/testutil/](internal/testutil/) — shared test helpers.

## Design principles

These are the principles every change should respect. When in doubt, optimize for the next reader.

### Simple

- One credential source and a small set of declared HTTPS endpoints per provider. No fallbacks, no probing, no auto-discovery. New endpoints are constants with cited sources.
- No feature flags, no compatibility shims, no dead branches "in case." Delete code that isn't used.
- No catches for convoluted error states that are unlikely to be reached.

### Robust

- Fail closed with an actionable message — the error names the next command the user should run to recover. `claude /login`, `aistat usage` to refresh stored credentials, `gh auth refresh -s user`, etc.
- One failing component never poisons another. Record its error in-band and keep the rest of the work going. For Claude multi-account: a single transient per-account failure does not flip the exit code.
- `aistat switch` is the ONLY path that mutates the Claude CLI's live keychain item; the regular `aistat usage` path is observation-only. Switch's order-of-operations: read-only resolve active → pick target → write live keychain (first mutation; on error, store untouched) → run the same write-capable Reconcile `usage` uses.
- `FetchForSwitch` performs NO refresh and NO store mutation. Stored accounts with expired access tokens are excluded from auto-pick with a per-account warn (and the user runs `aistat usage` to refresh). This prevents publishing stale rotated-away tokens to the live keychain.
- Per-account Claude usage is cached for 90 seconds at `$CACHE/aistat/usage/claude-v1.json`, keyed by `account.uuid`. Both `aistat usage` (reporting) and `aistat switch`'s candidate / active-account reads (`FetchForSwitch`, `FetchUsage`) go through the same `fetchLimitsCached` path — one code path, one source of truth for "get this account's limits." `aistat usage --refresh` bypasses the read path but still writes through. The trade-off accepted: auto-pick decisions may use data up to 90 s stale; the alternative (always fresh) made rate-limited accounts silently excluded from auto-pick.
- All HTTP transient errors retry inside `httpx.Doer` (max 3 attempts, `Retry-After`-aware, ctx-deadline-respecting). Providers do not retry; the orchestrator does not retry. The Claude provider's `perAccountBudget = 15s` is sized so the retry loop can honor one max-length `Retry-After: 10` sleep + slack on attempts 1+2 before `sleepWithCtx` short-circuits a second sleep.

### Maintainable

- Each package reads end-to-end without jumping files. Names describe the domain, not the implementation.
- Comments are reserved for the non-obvious *why* — a vendor quirk, an invariant, a workaround. Don't restate what the code says.
- Verbatim error strings asserted by tests are the contract; the implementer matches them byte-for-byte.

### Elegant

- One source of truth per concept. `Account.RawBlob` is the verbatim Claude credential JSON; access/refresh-token/expires methods re-parse it on every call (no must-stay-in-sync invariants). Renderers are pure functions of the model, never parallel implementations.
- Prefer the standard library. Reach for a third-party dependency only when the alternative is materially worse. (Current `go.mod` has none beyond Go itself.)

## Working in this repo

- Run `go test ./...` (and `go test -race ./...`) before declaring a change done. Use `go vet ./...` and `staticcheck` (pinned in CI to `2025.1.1`) for static checks. CI runs on both ubuntu-latest and macos-latest — fix Linux failures locally with `GOOS=linux go vet ./... && GOOS=linux ~/go/bin/staticcheck ./...`.
- Fake-mode smoke (`go build -tags=fake -o /tmp/aistat ./cmd/aistat && /tmp/aistat --fake -h`) renders the nested Claude account format without touching real credentials. Vet + staticcheck under `-tags=fake` are part of the smoke surface.
- The Claude provider is macOS/Linux-only. macOS reads/writes the Keychain item `Claude Code-credentials` (and the per-account store at service `aistat:accounts:claude:<uuid>`); Linux reads/writes `~/.claude/.credentials.json` and the per-account store at `~/.config/aistat/accounts/claude.json`. Codex and Copilot are portable.
- The module path is `github.com/drogers0/aistat/v2`. Go 1.22+.
- Live tests are gated behind env vars (`AISTAT_LIVE=1` for multi-account smoke; `AISTAT_LIVE_KEYCHAIN=1` for darwin keychain write tests). CI does not set either.
