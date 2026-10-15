# Changelog

All notable changes are listed here. Versions follow the `VERSION` file and [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [1.0.0] - 2026-10-15

First public release. Earlier private builds were numbered 4.x; the public history starts here.

### Added
- **LimitBreak** menu bar app (`LimitBreak.app`) with a bundled Go daemon. It waits for real quota recovery through [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) and continues the same Claude Code session.
- Quota queries through the provider's official usage endpoints (CLIProxyAPI `api-call`), with per-account caching, `Retry-After` backoff on HTTP 429 and faster polling as a reset approaches.
- `reset_fallback` policy (default `auto`): when the quota query itself is throttled (HTTP 429/5xx/timeout), try once after the recorded reset plus `safety_seconds`. A repeat failure never loops.
- Silent direct delivery through Claude Code's own cross-session message socket. When the session's process is not running, the session is woken in the background and delivered directly; the Claude window is used only if the process never starts. A refused wake (you are typing, using Claude, or the screen is locked) is retried later, never escalated.
- `restore_focus` (default `auto`) returns focus to the app you were using; `claude_quiet` / `claude_quiet_seconds` (default on, 120 s) keep LimitBreak from switching Claude's visible session while you are using it.
- The delivery channel (`direct`, `woken_direct`, `window_after_wake`, `window`) is recorded on every job and shown on recovery cards, the session panel, session rows and the menu bar popover, including the reason for any fallback.
- Three-layer hosting switches: one-time automation consent, per-session mode with a global default, emergency pause.
- Concurrency limits count only turns LimitBreak continued; a full budget names the session holding it.
- Workbench pages: Overview, Sessions, Quota resources (masonry cards with provider icons), Recovery tasks (with an optional always-on-top window), Hosting rules and Diagnostics.
- Four live clocks on every session card: current state, next quota check, expected auto-continue, usable pool.
- Dock icon while the workbench is open; optional "always show in Dock".
- tmux / `claude -p --resume` executors for terminal sessions.
- Reset signals: a signal re-queries the matching provider's accounts at once and again after 3 and 10 minutes. It is never treated as quota evidence.
  - Built-in [Codex Resets](https://codex-resets.com) feed (`signals.feeds`, mode `auto`/`on`/`off`; `auto` polls only while a Codex account exists; `[]` turns it off). Uses `ETag` and honours `Retry-After`.
  - `POST /api/signals` webhook, authenticated by the local Bearer token or by an HMAC-SHA256 signature with a timestamp (secret in `webhook-secret`, 0600). Idempotent per `source` + `id`.
  - `orchestrator signal` CLI and a signal card in Sources.
- Generic clients: `clients[]` declares any other agent client with an argument-vector command (`{session_id}`, `{cwd}`, `{message}` placeholders, no shell). `orchestrator event` reports its lifecycle. A continue counts as confirmed only after `turn_started`/`turn_finished` within `verify_seconds`; otherwise the session needs attention and is not run again. A session ID recorded by one client (or by Claude) is rejected from another.
- Quota that comes back well before the recorded reset (a provider resetting limits early) is continued at once; the native auto-resume grace only applies around the reset Claude itself observed.
- A session that needs attention after an unconfirmed continue now says when its quota is back and that a manual send is possible (display only; never resent automatically).
- `docs/EXTENDING.md`: extension points, rules, API reference. README screenshots with placeholder names.
- GitHub Actions CI, universal release workflow with optional Developer ID signing and notarization, one-line installer, issue templates and Dependabot.
