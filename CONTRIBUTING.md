# Contributing to LimitBreak

Thanks for helping. 中文贡献者同样欢迎，Issue 和 PR 可以用中文或英文。

## Ground rules

LimitBreak sends messages into people's coding sessions, so changes are held to a few hard rules:

1. **Never send on uncertain evidence twice.** An uncertain delivery is recorded as uncertain and is not resent automatically.
2. **Never read secrets or conversation content.** Only the non-secret registry fields listed in the README may be read from `~/.claude/sessions/`.
3. **Never spoof identity.** No forged permission modes, no fake origins, no impersonated clients.
4. **Every send goes through the shared gate** (`quotaEligible` and the dispatch checks in `internal/scheduler`). New executors must not bypass it.
5. **Tests do not touch real sessions.** Use the fixtures in `internal/scheduler/*_test.go`.

## Development setup

- macOS 14+, Xcode command-line tools, Go 1.24+.
- `bash scripts/verify.sh` runs everything CI runs (`go vet`, `go test -race`, Python tests, Swift compile with warnings as errors, native contract tests).
- `QSO_ALLOW_ADHOC=1 bash scripts/build-app.sh` builds a local ad-hoc app. Ad-hoc builds change code identity, so Accessibility must be re-granted after each build; use `QSO_SIGN_IDENTITY` with your own certificate for stable permissions.

## Layout

| Path | What lives there |
|---|---|
| `cmd/orchestrator` | daemon entry point, CLI, hooks endpoint |
| `internal/scheduler` | state machine, queue, gates, dispatch |
| `internal/proxy` | CLIProxyAPI client and quota parsers |
| `internal/adapters` | `QuotaSource` / `SessionObserver` / `ResumeExecutor` registry |
| `internal/direct`, `internal/executor` | delivery channels |
| `tray/` | SwiftUI workbench and menu-bar app |
| `bridge/` | Claude Desktop Accessibility bridge |
| `scripts/` | build, verify, sign, package, install |

## Adding a provider or executor

- **Quota only:** prefer configuration (`queries`) or a CLIProxyAPI quota plugin; add a parser in `internal/proxy/provider_parsers.go` with fixture tests if the payload is new.
- **Session auto-continue (e.g. Codex):** implement `SessionObserver` and `ResumeExecutor`, register them in `internal/adapters/registry.go`, and add scheduler tests that cover consent, native grace, the hourly cap and uncertain results.

## Pull requests

- Keep PRs focused; include tests for behaviour changes.
- UI text is Simplified Chinese first; keep strings short and factual.
- Update `CHANGELOG.md` under *Unreleased*.
- Do not commit build output, `runtime/`, databases, certificates or tokens (`.gitignore` covers the usual paths).
