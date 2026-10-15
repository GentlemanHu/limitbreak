<div align="center">

# LimitBreak

**Hit the usage limit, walk away. LimitBreak continues your Claude Code session the moment real quota comes back.**

A native macOS menu-bar app and local daemon that watches real quota through [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), waits for the reset, and sends your "continue" message to the *same* session — Claude Desktop (Code tab) or terminal `claude`.

[![CI](https://github.com/GentlemanHu/limitbreak/actions/workflows/ci.yml/badge.svg)](https://github.com/GentlemanHu/limitbreak/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/GentlemanHu/limitbreak)](https://github.com/GentlemanHu/limitbreak/releases)
![macOS 14+](https://img.shields.io/badge/macOS-14%2B-black)
![Apple Silicon + Intel](https://img.shields.io/badge/universal-arm64%20%7C%20x86__64-blue)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

English · [简体中文](README.zh-CN.md)

</div>

<p align="center">
  <img src="docs/images/recovery.png" alt="Recovery tasks: a session waiting for quota, with the live timeline and next checks" width="70%">
  &nbsp;
  <img src="docs/images/popover.png" alt="Menu-bar popover with accounts and waiting sessions" width="24%">
</p>

---

## Why

Long Claude Code runs stop at the 5-hour or weekly limit. Claude's own auto-resume only fires when it observed the reset itself, and it does not know that another account in your CLIProxyAPI pool is free. LimitBreak closes that gap:

- **Real quota, not guesses.** Reads each account's 5-hour, weekly and per-model windows from the provider's usage endpoint through CLIProxyAPI. Unknown is shown as unknown, never as 0%.
- **Same session, same model.** It only sends the continue text. It never changes the session's model, account or permission mode.
- **Silent delivery.** While the session process is running, the message goes through Claude Code's own cross-session message socket: no window switching, no keyboard stealing, works with the screen locked. If the process has exited, LimitBreak opens the session in the background so Claude starts it, then delivers through the socket the same way. Only if the process never starts does it type into the Claude window — and it waits until you are not using Claude, then gives focus back to the app you were in.
- **You can see how it was sent.** Every recovery records its channel (silent, woken then silent, or window) and the reason for any fallback.
- **Safe by construction.** One explicit consent, an emergency pause, per-session hourly caps, a circuit breaker, a fair queue with leases, and *no automatic resend on an uncertain result*.
- **Clocks everywhere.** Each session shows four live tiles: what is happening now, the next quota check, the expected auto-continue time, and how much of its pool is usable.

## Features

| | |
|---|---|
| Hosting modes | Per session: follow global / smart hosting / monitor only / off. Global default plus an emergency pause. |
| Account pools | Automatic match by the session's model (aliases → exact → prefix → provider), or pin accounts by hand — useful when CLIProxyAPI maps one model name to another provider. |
| Quota sources | Claude (OAuth usage), Codex, Antigravity, xAI and any provider CLIProxyAPI's quota plugin exposes; custom HTTP sources via config. |
| Reset signals | Built-in [Codex Resets](https://codex-resets.com) feed and a signed `POST /api/signals` webhook. A signal makes the matching accounts be re-queried at once (and again after 3 and 10 minutes); it is never treated as proof that quota is back. |
| Throttled queries | When the usage endpoint itself answers HTTP 429, LimitBreak waits for the recorded reset plus a safety margin and tries **once** (`reset_fallback`, on by default). |
| Executors | Claude Desktop (direct socket; background wake then direct socket; deep link + Accessibility as the last resort), terminal `claude` (direct socket, tmux, or background `claude -p --resume`), and any other client through a configured command (`clients[]`). |
| UI | Native SwiftUI workbench: masonry quota cards with real provider icons, session list synced with the Claude Desktop sidebar, activity log, diagnostics. Dock icon while the workbench is open. |
| Integrations | Local token-protected HTTP API, SSE events, scoped MCP server for other agents, `orchestrator event` / `orchestrator signal` CLIs. |

## Screenshots

All account names and session titles below are replaced with placeholders.

| | |
|---|---|
| ![Overview](docs/images/overview.png) **Overview** — what needs you right now. | ![Sessions](docs/images/sessions.png) **Sessions** — synced with the Claude Desktop sidebar; per-session hosting, pool and continue text. |
| ![Quota resources](docs/images/resources.png) **Quota resources** — every account's windows, resets and bound sessions. | ![Hosting rules](docs/images/hosting.png) **Hosting rules** — global defaults, delivery channels and safety switches. |
| ![Diagnostics](docs/images/diagnostics.png) **Diagnostics** — read-only readiness checks. | |

The images at the top are **Recovery tasks** (the timeline from limit hit to confirmed continue) and the **menu-bar popover**.

## Requirements

- macOS 14 or later (Apple Silicon or Intel).
- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) running locally with its management API enabled. LimitBreak reads quota through it:
  - `GET /v0/management/auth-files` to list accounts;
  - `POST /v0/management/api-call` to call each provider's usage endpoint with that account (CLIProxyAPI substitutes the token itself, so credentials never reach LimitBreak);
  - `POST /v0/management/quota/fetch` and quota plugins when your CLIProxyAPI version provides them.
- Claude Code (Desktop Code tab or terminal `claude`).

## Install

**One line** (downloads the latest release, verifies SHA-256, backs up the previous version, clears quarantine):

```bash
curl -fsSL https://raw.githubusercontent.com/GentlemanHu/limitbreak/main/scripts/install.sh | LIMITBREAK_REPO=GentlemanHu/limitbreak bash
```

**Manual:** download `LimitBreak-<version>-universal.zip` from [Releases](https://github.com/GentlemanHu/limitbreak/releases), check it with `shasum -a 256 -c SHA256SUMS`, unzip and move `LimitBreak.app` to `/Applications`.

### "LimitBreak is damaged and can't be opened"

The app is not damaged. Builds without a Developer ID signature and Apple notarization get this Gatekeeper message when the ZIP was downloaded by a browser (the file carries the `com.apple.quarantine` attribute). This is common on Apple Silicon. Remove the attribute once:

```bash
xattr -dr com.apple.quarantine /Applications/LimitBreak.app
```

Releases built with the signing secrets configured (see [Release workflow](#release-workflow)) are notarized and stapled and open normally.

## First run

1. Start CLIProxyAPI (default `127.0.0.1:8317`) and have its management key ready.
2. Open LimitBreak. The **Overview** checklist walks you through:
   - connecting CLIProxyAPI;
   - granting Accessibility (only needed for the window fallback);
   - installing Claude Code hooks (shown as a diff; written only after you confirm) — this is how LimitBreak learns that a session hit the limit;
   - granting automatic-continue consent.
3. In **Sessions**, pick a session and choose *Smart hosting*. The status card tells you exactly what will happen and when.

All private state lives in `~/Library/Application Support/Quota Session Guard/` (the historical name is kept so permissions and data survive upgrades).

## How it decides to send

```
StopFailure(rate_limit) ──► session COOLDOWN ──► poll quota (backoff by distance to reset)
                                                   │
             fresh "allowed" after the failure ◄───┤───► query throttled (429) and recorded
                                                   │     reset + safety passed → try once
                                                   ▼
                              native-grace wait (let Claude's own auto-resume win)
                                                   ▼
                     fair queue + lease ──► direct socket ─► background wake + direct socket
                                                   └──► deep link + AX (process never started)
                                                   ▼
                       VERIFYING until the session's own hook confirms (UserPromptSubmit)
```

An uncertain delivery is reported as uncertain and is never resent automatically.

## Build from source

Requirements: macOS 14+, Xcode command-line tools (Swift 5.9+), Go 1.24+.

```bash
bash scripts/verify.sh
```

```bash
QSO_ALLOW_ADHOC=1 bash scripts/build-app.sh
```

Useful variables: `QSO_SIGN_IDENTITY` (a signing identity already in your keychain), `QSO_UNIVERSAL=1` (arm64 + x86_64), `QSO_SIGN_TIMESTAMP=1` (for notarization), `QSO_APP_OUTPUT`, `QSO_CREATE_ZIP=1`. The build never overwrites existing output.

## Release workflow

- `ci.yml` runs on every push and PR: `go vet`, `go test -race`, Python tests, Swift compile with warnings as errors, native contract tests, and an ad-hoc app artifact.
- `release.yml` runs on a `v*` tag or manually. It builds a universal app, signs with Developer ID and notarizes **when** these repository secrets exist: `MACOS_CERTIFICATE_P12` (base64), `MACOS_CERTIFICATE_PASSWORD`, `MACOS_SIGN_IDENTITY`, `APPLE_ID`, `APPLE_TEAM_ID`, `APPLE_APP_PASSWORD`. Without them it publishes an ad-hoc build and says so in the release notes.

## Extending LimitBreak

Most extensions need no code. **[docs/EXTENDING.md](docs/EXTENDING.md)** describes each contract with examples:

| Extension | How | Rebuild |
|---|---|---|
| Quota source | `readonly-http` source returning `normalized-v1` JSON | No |
| Reset signal | `POST /api/signals` (Bearer token or HMAC), or a polled feed in Go (`internal/signals`) | Webhook: no |
| Another client (Codex CLI, your own agent, …) | `clients[]` in `config.json` + `orchestrator event --client …` | No |
| Desktop executor | `executor.Backend` in Go | Yes |

The scheduler, queue, safety gates and UI are provider-neutral. Claude Desktop and terminal `claude` are the tested clients. The generic client path is covered by unit tests, but it has **not** been tried against Codex CLI, WorkBuddy or DeepSeek clients; the wrapper in EXTENDING.md is a starting point, not a supported integration.

## Privacy and security

- Reads only non-secret fields of `~/.claude/sessions/*.json` (pid, start time, session ids, socket path, status, protocol version). Never reads `.key` token files, transcripts or conversation content.
- Never asserts or spoofs a permission mode; sessions in "bypass permissions" mode may ask you to approve the incoming message.
- The local API binds to loopback, requires a bearer token and rejects any request with an `Origin` header. `POST /api/signals` alone also accepts an HMAC signature, and only when you create a webhook secret.
- The Codex Resets feed sends no account data: it is a plain GET of a public status endpoint, made only while a Codex account exists. Set `signals.feeds` to `[]` to turn it off.

See [SECURITY.md](SECURITY.md) to report a vulnerability.

## FAQ

**Does pinning an account keep the model I chose in Claude?** Yes. A pool only decides whose quota to wait for. LimitBreak sends the continue text and nothing else.

**Why Go + Swift?** The daemon must keep running, survive app restarts and own the SQLite state; Go makes that small and testable. The UI, Accessibility and Dock/menu-bar integration are native macOS APIs, so they are Swift.

**Is it affiliated with Anthropic?** No. LimitBreak is an independent open-source tool. Claude is a trademark of Anthropic; provider icons are used only to identify providers (see `tray/Resources/ProviderIcons/NOTICE.md`).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [CHANGELOG.md](CHANGELOG.md).

## Acknowledgements

- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (MIT) by router-for-me — LimitBreak would not exist without it. It holds the accounts and credentials; LimitBreak only asks its management API for quota. LimitBreak is a separate project and is not affiliated with CLIProxyAPI. Please star and support it.
- Provider icons belong to their owners and are used only to identify providers; see [`tray/Resources/ProviderIcons/NOTICE.md`](tray/Resources/ProviderIcons/NOTICE.md).

## License

[MIT](LICENSE)
