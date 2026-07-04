# aistat

A single static Go binary that reports Claude, Codex, and Copilot usage limits from the terminal — and switches the live credential between stored Claude or Codex accounts without a browser round-trip. JSON by default; `-h`/`--human` for the nested text rendering.

CLI surface (provider-scoped with bulk-on-omission as of the multi-account effort):

```
aistat                                 # default: same as `aistat usage`
aistat usage [provider]                # report usage across all providers/accounts
aistat usage --refresh                 # bypass 90 s per-account cache; writes through

aistat switch                          # fan out to every provider with ≥ 2 stored accounts (auto-pick best by headroom)
aistat switch <provider>               # target one provider; auto-pick
aistat switch <provider> --to <id>     # target one provider, one stored account (email or uuid)
aistat switch --to <id>                # infer provider from id-uniqueness (no provider arg)

aistat switch --if-needed              # conditional: only switch when the active account exceeds a threshold
                                       #   AISTAT_IF_ABOVE_5H (default 85) / AISTAT_IF_ABOVE_WEEKLY (default 95)
                                       #   resolved per run: process env > ~/.config/aistat/autoswitch.env > defaults; "off" disables a window
                                       #   weekly binds on whichever of seven_day/thirty_day has least headroom; model-scoped windows (seven_day_fable) are shown in `usage` but do NOT drive the trigger or ranking
aistat switch --notify                 # desktop notification on switch / threshold-with-no-alternative (darwin)

aistat autoswitch install              # write thresholds env file + launchd agent (StartInterval, default 300s) running `switch --if-needed --notify`
aistat autoswitch uninstall            # bootout + remove the plist (thresholds file kept)
aistat autoswitch status               # installed/loaded state, interval, effective thresholds

aistat accounts list                   # list every provider's stored accounts (JSON: {claude:[...], codex:[...]}; text -h: section headers)
aistat accounts list <provider>        # scope to one provider
aistat accounts remove <id>            # infer provider from id-uniqueness; error if id matches > 1 provider
aistat accounts remove <id> <provider> # explicit provider

aistat -h | --human                    # text rendering (affects `usage` and `accounts list`)
aistat --debug                         # per-request / per-provider lines on stderr
aistat --version
aistat --help
```

JSON is the default; exit codes: 0 success / 1 any-provider-failed / 2 usage error / 3 stdout-write error.

## Repo layout

- [cmd/aistat/](cmd/aistat/) — `main`, hand-rolled subcommand dispatch (`scanGlobals` + per-subcommand FlagSets), provider registry, fake-provider hooks. One file per subcommand: [main.go](cmd/aistat/main.go), [usage.go](cmd/aistat/usage.go), [switch.go](cmd/aistat/switch.go) (CLI-private `switchable` interface + `buildSwitchHandles` registry + bulk / single / inferred dispatch), [switch_ifneeded.go](cmd/aistat/switch_ifneeded.go) (conditional-switch gate: trigger evaluation + notification plumbing), [autoswitch.go](cmd/aistat/autoswitch.go) (darwin launchd install/uninstall/status), [accounts.go](cmd/aistat/accounts.go) (`providerStore` + multi-provider list/remove with id-uniqueness inference).
- [internal/accounts/](internal/accounts/) — provider-neutral persisted account store. `Account` is opaque identity + metadata + `RawBlob`; provider packages own credential-shape parsing. `Provider` (closed set: `ProviderClaude`, `ProviderCodex`) with `validate()` gating all `OpenStore` calls. Backends: `store_darwin.go` keychain at `aistat:accounts:<provider>:<uuid>` + process flock on `$CACHE/aistat/store.lock`; `store_linux.go` JSON at `~/.config/aistat/accounts/<provider>.json` with flock on `.<provider>.lock` sentinel (NOT the data file — survives atomic-rename); `MemoryStore` for tests.
- [internal/providers/](internal/providers/) — one subpackage per provider (`claude`, `codex`, `copilot`). Each owns its credential source, HTTP calls, and response normalization into the shared `Limit` type in [types.go](internal/providers/types.go). `AccountResult` lives here too — same type carried on both `ProviderOutput` (in-process) and `ProviderResult` (JSON-serialized).
- [internal/providers/usagecache/](internal/providers/usagecache/) — provider-neutral 90 s file-backed usage cache. `New(provider, nowFn, warnFn)` validates the provider char-set and writes `$CACHE/aistat/usage/<provider>-v1.json` with `<provider>.cache.lock`. Warn strings include the provider name.
- [internal/providers/multiaccount/](internal/providers/multiaccount/) — provider-neutral helpers consumed by both Claude and Codex multi-account `Fetch`: `SortAccountResults`, `RecordFetchOutcome`, `RecomputeResetAfter`, `Budget(base, perAccount, count)`.
- [internal/autoswitch/](internal/autoswitch/) — pure pieces of conditional switching: threshold resolution (process env > ~/.config/aistat/autoswitch.env > defaults; values 1-100 or "off") and launchd plist generation/parsing. The env file exists because launchd does not inherit the shell environment — editing it retunes thresholds without a plist reload. `WriteFileAtomic` is the shared temp+rename writer used for both the env file and the plist.
- [internal/notify/](internal/notify/) — desktop notifications: osascript on darwin behind a runner seam, silent no-op elsewhere. Notification failures are never fatal.
- [internal/providers/claude/](internal/providers/claude/) — Claude's multi-account machinery: [claude.go](internal/providers/claude/claude.go) (Fetch + FetchForSwitch + cache wiring), [profile.go](internal/providers/claude/profile.go) (identity endpoint), [refresh.go](internal/providers/claude/refresh.go) (OAuth refresh), [reconcile.go](internal/providers/claude/reconcile.go) (pure decision tree: byte-match → profile lookup → fallback), [account.go](internal/providers/claude/account.go) (`StoredAccessToken` / `StoredRefreshToken` / `StoredExpiresAt` token-parsing helpers operating on the opaque `accounts.Account`). Model-scoped weekly limits from the usage response's `limits` array surface as `seven_day_<model>` windows for visibility in `usage` (key derived from `scope.model.display_name`; top-level keys win on collision) — they are informational only and, unlike `seven_day`/`thirty_day`, do NOT drive the `--if-needed` trigger or auto-pick ranking (a spent model budget blocks one model, not the account).
- [internal/providers/codex/](internal/providers/codex/) — Codex's multi-account machinery, structurally a mirror of Claude's: [codex.go](internal/providers/codex/codex.go) (Fetch + FetchForSwitch + cache wiring + `rotateRawBlob`), [refresh.go](internal/providers/codex/refresh.go) (OAuth refresh against `https://auth.openai.com/oauth/token` with client_id confirmed via Codex binary inspection), [reconcile.go](internal/providers/codex/reconcile.go) (pure decision tree: byte-match → JWT `sub` lookup → live-unstored), [account.go](internal/providers/codex/account.go) (Codex-shaped `Stored*` helpers). `StoredExpiresAt` decodes expiry from the **access_token** JWT's `exp` (via `cred.ParseJWTExp`) — the long-lived API credential — NOT the short-lived OIDC `id_token`, which is identity-only (gating on the id_token's ~1 h `exp` made the refresh gate fire on every run). Identity is the `sub` claim of the OIDC `id_token` — no network endpoint, JWT-payload decode only. Slot-vs-duration window labelling is by `limit_window_seconds`, NOT by slot position, so free-account weekly windows that land in the primary slot are not mislabelled as `five_hour`.
- [internal/render/](internal/render/) — `json` and `text` renderers. The JSON shape is the public contract; the text renderer is a thin presentation layer over the same model. Provider-agnostic: when `len(result.Accounts) > 0` for ANY provider the renderer emits the nested per-account view, otherwise the legacy flat form (still in use for Copilot, and as a Claude/Codex fallback).
- [internal/cred/](internal/cred/) — credential read/write and JWT decoding. `Credential.Raw []byte` preserves the verbatim provider blob (every byte the upstream CLI wrote) so a switch re-publishes byte-for-byte. `ReadClaudeCredential` / `WriteClaudeLiveBlob` (macOS Keychain item `Claude Code-credentials` via a `runSecurity` test seam; Linux `~/.claude/.credentials.json`). `ReadCodexCredential` / `WriteCodexLiveBlob` (file-only on both OSes: `~/.codex/auth.json`, mode 0600, atomic rename + fsync). `ParseCodexIDToken` (exported here to avoid a `cred` ↔ `providers/codex` import cycle) decodes the OIDC `id_token` payload for `sub` / `email` / `exp` without signature verification; `ParseJWTExp` similarly decodes just the `exp` claim of any JWT (no signature check) and is used by `codex.StoredExpiresAt` to read the access-token expiry.
- [internal/httpx/](internal/httpx/) — shared HTTP transport: `Doer.GetJSON` (Authorization-reserved) + `Doer.PostForm` (no Authorization by default — used by both refresh clients) sharing an unexported `setCommonHeaders` / `do` split. `do` runs a bounded retry loop (max 3 attempts) that honors `Retry-After` (capped at 10 s) on transient classifications, falling back to exponential backoff with ±20 % jitter. `Classifier` takes `*http.Response` so callers can inspect headers.
- [internal/orchestrate/](internal/orchestrate/) — parallel fan-out across providers; one failing provider does not block the others. Preserves per-account rows on provider-level error (D8 contract).
- [internal/testutil/](internal/testutil/) — shared test helpers.

## Design principles

These are the principles every change should respect. When in doubt, optimize for the next reader.

### Simple

- One credential source and a small set of declared HTTPS endpoints per provider. No fallbacks, no probing, no auto-discovery. New endpoints are constants with cited sources.
- No feature flags, no compatibility shims, no dead branches "in case." Delete code that isn't used.
- No catches for convoluted error states that are unlikely to be reached.

### Robust

- Fail closed with an actionable message — the error names the next command the user should run to recover. `claude /login`, `aistat usage` to refresh stored credentials, `codex login`, `gh auth refresh -s user`, etc.
- One failing component never poisons another. Record its error in-band and keep the rest of the work going. For Claude / Codex multi-account: a single transient per-account failure does not flip the exit code.
- `aistat switch` is the ONLY path that mutates the live credential (Claude Keychain item or `~/.codex/auth.json`); `aistat usage` is observation-only (modulo cache writes and refresh-rotation persistence). Switch's order-of-operations: read-only resolve active → pick target → write live (first mutation; on error, store untouched) → run the same write-capable Reconcile `usage` uses.
- `FetchForSwitch` performs NO refresh and NO store mutation. Stored accounts with expired or revoked access tokens are excluded from auto-pick with a per-account warn (and the user runs `aistat usage` or `codex login` / `claude /login` to recover). This prevents publishing stale rotated-away tokens to the live credential.
- `switch --if-needed` fails closed: if the active account cannot be resolved or its usage fetch fails, nothing is written and the exit code is 1. A timer poll that finds nothing to do (below threshold, no better account, only one account) exits 0.
- Per-account usage is cached for 90 seconds at `$CACHE/aistat/usage/<provider>-v1.json`, keyed by `account.uuid` (Claude's UUID is the OAuth profile UUID; Codex's UUID is the `id_token` `sub` claim). Both `aistat usage` (reporting) and `aistat switch`'s candidate / active-account reads go through the same `fetchLimitsCached` path. `aistat usage --refresh` bypasses the read path but still writes through.
- All HTTP transient errors retry inside `httpx.Doer` (max 3 attempts, `Retry-After`-aware, ctx-deadline-respecting). Providers do not retry; the orchestrator does not retry. The Claude provider's `perAccountBudget = 15 s` is sized so the retry loop can honor one max-length `Retry-After: 10` sleep + slack on attempts 1+2 before `sleepWithCtx` short-circuits a second sleep.

### Maintainable

- Each package reads end-to-end without jumping files. Names describe the domain, not the implementation.
- Comments are reserved for the non-obvious *why* — a vendor quirk, an invariant, a workaround. Don't restate what the code says.
- Verbatim error strings asserted by tests are the contract; the implementer matches them byte-for-byte.

### Elegant

- One source of truth per concept. `accounts.Account` is opaque (identity + metadata + `RawBlob`); provider packages own credential-shape parsing via small `Stored*` helpers. `Credential.Raw` is the verbatim live blob. Renderers are pure functions of the model, never parallel implementations.
- Prefer the standard library. Reach for a third-party dependency only when the alternative is materially worse. (Current `go.mod` has none beyond Go itself.)

## Known limitations (Codex)

These are upstream OAuth-provider behaviors aistat cannot work around without re-authentication. Both fail closed with actionable errors. **`codex login --device-auth` is the right flow on remote / headless machines** (no browser required on the host), but per upstream [code](https://github.com/openai/codex/blob/main/codex-rs/login/src/device_code_auth.rs) and [docs](https://developers.openai.com/codex/auth) device-auth uses the same persistence path and the same single-cached-login model as the browser flow — the revocation semantics below apply equally.

- **Refresh-token rotation race.** OpenAI's `/oauth/token` endpoint single-uses each refresh_token; the Codex CLI rotates it on every refresh and writes the new value to `~/.codex/auth.json`. If the Codex CLI runs between aistat reading the file and aistat sending its refresh request, aistat's in-memory copy is stale and the server returns a 401 whose body reads `Your refresh token has already been used to generate a new access token.` aistat tightens this to `stale refresh token (codex CLI rotated it); retry or run codex login to recover` (matched on `already been used` in `refreshErrorMessage`). Recovery: `codex login` (or just wait for the next aistat run — the cache will catch up next pass). Note: since `StoredExpiresAt` now gates on the long-lived access_token's `exp`, routine reporting no longer triggers a refresh on every run — only when the access token is genuinely near expiry — so this race is hit far less often.
- **Switch-side token revocation.** When a new account logs in on the same OAuth client via the browser flow (`codex logout && codex login`), OpenAI's server invalidates the previous account's tokens. aistat's `switch` re-publishes the stored blob byte-for-byte, but server-revoked tokens stay revoked — the next usage call returns a 401 whose body carries either `token_revoked` or `token_invalidated` (OpenAI uses both interchangeably for this condition). aistat tightens both to `tokens revoked by upstream (likely a codex login for another account); run codex login to recover` (matched by `isRevokedTokenErr`). Recovery: `codex login` for the now-active account.

## Known limitations (autoswitch)

- **uninstall assumes bootout succeeds.** `aistat autoswitch uninstall` ignores `launchctl bootout` failures (the job is usually simply not loaded) and removes the plist. In the unlikely case bootout failed for a real reason, the launchd job can keep running from its in-memory registration while `status` reports `installed: no`. Recovery: `launchctl bootout gui/$UID/com.drogers0.aistat.autoswitch` manually.
- **First launchd run may need Keychain approval.** The agent runs the same binary path you use interactively, so existing Keychain ACLs usually apply — but a fresh install can surface a one-time "Always Allow" prompt or a denied read. Check `~/Library/Logs/aistat-autoswitch.log` when the agent seems idle.

## Working in this repo

- Tests follow the table-driven `t.Run` subtest convention — see [TESTING.md](TESTING.md).
- Run `go test ./...` (and `go test -race ./...`) before declaring a change done. Use `go vet ./...` and `staticcheck` (pinned in CI to `2025.1.1`) for static checks. CI runs on both ubuntu-latest and macos-latest — fix Linux failures locally with `GOOS=linux go vet ./... && GOOS=linux ~/go/bin/staticcheck ./...`.
- Fake-mode smoke (`go build -tags=fake -o /tmp/aistat ./cmd/aistat && /tmp/aistat --fake`) renders all providers (Claude + Codex + Copilot) in JSON without touching real credentials; add `-h` for human-readable. Vet + staticcheck under `-tags=fake` are part of the smoke surface.
- The Claude provider is macOS/Linux-only. macOS reads/writes the Keychain item `Claude Code-credentials` (and the per-account store at service `aistat:accounts:claude:<uuid>`); Linux reads/writes `~/.claude/.credentials.json` and the per-account store at `~/.config/aistat/accounts/claude.json`. The Codex provider is portable: live blob always `~/.codex/auth.json`, per-account store mirrors Claude's split (`aistat:accounts:codex:<sub>` on macOS, `~/.config/aistat/accounts/codex.json` on Linux). Copilot is portable.
- The module path is `github.com/drogers0/aistat/v2`. Go 1.22+.
- Live tests are gated behind env vars (`AISTAT_LIVE=1` for Claude multi-account smoke; `AISTAT_LIVE_KEYCHAIN=1` for darwin keychain write tests). CI does not set either.
