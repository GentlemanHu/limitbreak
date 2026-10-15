# Extending LimitBreak

LimitBreak has four extension points. Each one has a narrow contract, and none
of them can make the daemon send a message without fresh quota evidence.

| Extension | What it adds | How | Needs a rebuild |
| --- | --- | --- | --- |
| Quota source | Where quota and reset times come from | `readonly-http` source speaking `normalized-v1` | No |
| Reset signal | "A reset just happened, check now" hints | `POST /api/signals` webhook, or a polled feed in Go | Webhook: no · feed: yes |
| Client | Another agent client (Codex CLI, your own tool) | `clients[]` in `config.json` + `orchestrator event` | No |
| Executor / observer for a new desktop app | A new way to type into a GUI | Go code in `internal/executor` | Yes |

The configuration lives in
`~/Library/Application Support/Quota Session Guard/config.json` (the folder keeps
its historical name). Unknown JSON fields are rejected, so a typo fails loudly
instead of being ignored. The examples below write `$LB` for that folder and
`$LB/bin/orchestrator` for the installed CLI.

## The rules every extension keeps

1. **Only fresh quota evidence allows a send.** Signals, webhooks and client
   events can make the daemon *check* sooner; they can never mark quota as
   recovered.
2. **No automatic resend after an uncertain send.** If a continue was started
   but not confirmed within `verify_seconds`, the session goes to "needs
   attention" and stays there until a person acts.
3. **No shell, no plugins.** Nothing is loaded at runtime. Commands run with an
   explicit argument vector; placeholders are replaced as whole arguments.
4. **Local only.** The daemon listens on loopback. Write requests need the local
   token or, for `/api/signals` only, an HMAC signature.

## 1. Quota sources (`readonly-http`, `normalized-v1`)

Any service that can report quota as JSON can be a source. Add it in the app
(Workbench → Sources → Add) or in `config.json`:

```json
{
  "sources": [
    { "id": "my-quota", "type": "readonly-http", "url": "https://quota.example.com/v1/accounts",
      "schema": "normalized-v1", "credential_env": "MY_QUOTA_TOKEN", "poll_seconds": 180 }
  ]
}
```

The URL must be HTTPS (literal loopback HTTP is allowed). `credential_env` names
an environment variable sent as a Bearer token; the value never goes into the
config file. Redirects are not followed and environment proxies are not used.

Response body:

```json
{
  "accounts": [
    {
      "id": "acct-1", "provider": "codex", "plan": "plus", "models": ["gpt-5"],
      "disabled": false, "checked_at": "2026-10-15T08:00:00Z",
      "windows": [
        { "name": "five_hour", "used_percent": 100, "blocked": true, "reset_at": "2026-10-15T10:00:00Z" }
      ]
    }
  ]
}
```

`provider` is what reset signals and client sessions match against, so use the
same lower-case name everywhere (`codex`, `claude`, `deepseek`, …).

## 2. Reset signals

A reset signal says "this provider's quota was (or is about to be) reset". The
daemon then re-queries every account of that provider at once, again after
3 minutes and after 10 minutes. Sending still waits for a quota read that shows
the account allowed.

### Built-in feed: Codex Resets

Enabled by default in `auto` mode: it polls
`https://codex-resets.com/api/v1/status` every 60 s, but only while at least one
`codex` account exists. It uses `ETag`/`If-None-Match` and honours `Retry-After`
on 429/503. The first poll only records the current reset unless it is less than
30 minutes old. Data from [Codex Resets](https://codex-resets.com); its forecast
("watch") is shown as a third-party prediction, never as a commitment.

```json
{ "signals": { "feeds": [ { "id": "codex-resets", "type": "codex-resets", "mode": "auto", "poll_seconds": 60 } ] } }
```

`mode` is `auto`, `on` or `off`. `"feeds": []` turns polling off. Omitting
`signals.feeds` keeps the default feed.

### Webhook: `POST /api/signals`

Anything that learns about a reset (a bot, a cron job, another monitor) can push
it:

```json
{ "source": "my-bot", "provider": "codex", "id": "2026-10-15-reset", "kind": "reset_executed",
  "at": "2026-10-15T06:28:00Z", "text": "optional, shown in the UI", "url": "https://optional.link" }
```

- `id` is the idempotency key per `source`; repeating it answers
  `{"ok":true,"duplicate":true}` and does nothing.
- `kind` is `reset_executed` (default) or `reset_scheduled`.
- `source` cannot reuse the ID of a polled feed.

Authentication, either:

- the local token: `Authorization: Bearer <token>` (what the CLI uses), or
- an HMAC, for senders that should not hold the local token. Put a random secret
  of at least 32 characters in `$LB/webhook-secret` with mode `0600` (or point
  `signals.webhook_secret_file` elsewhere) and restart the daemon. Then send:
  - `X-LimitBreak-Timestamp`: Unix seconds, within ±5 minutes
  - `X-LimitBreak-Signature`: `sha256=` + hex(HMAC-SHA256(secret, timestamp + "." + body))

```bash
body='{"source":"my-bot","provider":"codex","id":"r-123"}'
ts=$(date +%s)
sig=$(printf '%s.%s' "$ts" "$body" | openssl dgst -sha256 -hmac "$(cat "$LB/webhook-secret")" -hex | sed 's/^.* //')
curl -sS -X POST http://127.0.0.1:8318/api/signals -H 'Content-Type: application/json' \
  -H "X-LimitBreak-Timestamp: $ts" -H "X-LimitBreak-Signature: sha256=$sig" --data "$body"
```

The daemon only listens on loopback. To receive webhooks from the internet, run
your own relay that verifies the sender and forwards locally.

From a script on the same machine the CLI is simpler:

```bash
"$LB/bin/orchestrator" signal --config "$LB/config.json" -- --provider codex --id r-123 --source my-bot
```

### Adding a polled feed in Go

1. Implement `signals.Source` (`internal/signals/signals.go`): `Descriptor()`
   and `Poll(ctx, prev, now)`. Return new resets in `Result.Signals` and your
   bookkeeping (last ID, ETag, watch) in `Result.State`; return `*RetryError`
   for rate limiting.
2. Register the constructor in `signals.Factories` and add the type to
   `config.SignalSourceTypes` (and its defaults in `EffectiveSignalSources`).
3. Add a descriptor in `internal/adapters/registry.go` with kind
   `reset_signal`, and tests modelled on `internal/signals/signals_test.go`.

## 3. Other clients (Codex CLI, WorkBuddy, DeepSeek, your own agent)

LimitBreak can host sessions of any client that can (a) tell it what happened
and (b) be continued by a command. It does not need to know anything else about
the client.

### Declare the client

```json
{
  "clients": [
    { "id": "codex", "name": "Codex CLI", "provider": "codex",
      "command": ["/Users/me/bin/limitbreak-codex", "{session_id}", "{cwd}", "{message}"],
      "env": ["OPENAI_API_KEY"], "timeout_seconds": 600 }
  ]
}
```

- `id`: lower-case `[a-z0-9._-]`. Sessions are stored with executor `client:<id>`.
- `provider`: matches account providers from your quota sources and signals.
- `command[0]`: absolute path to a regular executable owned by you or root and
  not writable by group/others (checked before every run).
- Placeholders `{session_id}`, `{cwd}`, `{message}` must each be a whole argument;
  `{message}` exactly once. A message that starts with `-` is refused.
- The command runs detached in its own process group, in the session's cwd, and
  is killed after `timeout_seconds` (default 120). Its environment is HOME, USER,
  LOGNAME, PATH, LANG, LC_*, TMPDIR, the names in `env`, plus
  `LIMITBREAK_CLIENT_ID` and `LIMITBREAK_SESSION_ID`.

### Report events

```bash
"$LB/bin/orchestrator" event --config "$LB/config.json" -- \
  --client codex --session <session-id> --type limit_hit --cwd "$PWD" [--model m] [--title t] [--event-id id]
```

| `--type` | Meaning |
| --- | --- |
| `session_start` / `session_end` | The session appeared / ended |
| `turn_started` | A prompt was submitted (also confirms a continue) |
| `turn_finished` | A turn completed (also confirms a continue) |
| `limit_hit` | The turn stopped because of a usage limit — this is what queues a continue |
| `turn_failed` | The turn stopped for another reason |

Events are spooled locally first, so they are not lost while the daemon
restarts. `--event-id` makes retries idempotent.

### What happens

1. `limit_hit` → the session waits for its provider's quota.
2. Quota reads show an account allowed → the daemon runs `command` once.
3. Your wrapper must make the client report `turn_started` or `turn_finished`
   within `verify_seconds` (default 45 s). Otherwise the session goes to
   "needs attention" and is **not** run again automatically.

A session ID belongs to exactly one client: an event from client A for a session
recorded by client B (or by Claude) is rejected.

### Example wrapper for Codex CLI (untested)

This is a sketch, not a supported integration: we have not run it against Codex
CLI, WorkBuddy or DeepSeek clients. Check your client's own documentation for
how to resume a session non-interactively; for Codex CLI that is, as far as we
know, `codex exec resume <SESSION_ID> <PROMPT>`.

```bash
#!/bin/sh
# limitbreak-codex <session_id> <cwd> <message>
set -u
LB="$HOME/Library/Application Support/Quota Session Guard"
report() { "$LB/bin/orchestrator" event --config "$LB/config.json" -- --client codex --session "$1" --type "$2" --cwd "$3"; }
report "$1" turn_started "$2"
if codex exec resume "$1" "$3"; then
  report "$1" turn_finished "$2"
else
  report "$1" turn_failed "$2"   # use limit_hit instead when the output shows a usage limit
fi
```

To get `limit_hit` and the session ID in the first place, the client has to
expose them: a hook or notify mechanism, a log you can parse, or a wrapper you
start sessions through. LimitBreak only consumes the events above.

## 4. New desktop executors and observers

Typing into a GUI app (as the Claude desktop executor does) needs Accessibility
access and careful verification, so it is not configurable. Implement
`executor.Backend` (`Resume`, `Probe`), route it in `executor.Router`, add an
`adapters` descriptor and tests, and keep the same rules: verify the target
before sending, report `Submitted` only when the message was handed over, and
leave confirmation to an observed event.

## API reference (local, `http://127.0.0.1:8318`)

All requests need `Authorization: Bearer <token>` except `GET /api/health` and
a signed `POST /api/signals`. The token is in `config.json`; keep it private.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/health` | Liveness |
| GET | `/api/snapshot` | Accounts, sessions, jobs, audit, sources, signals |
| POST | `/api/refresh` | Query quota now |
| GET | `/api/adapters` | Declared adapters and their limitations |
| GET | `/api/sources` | Quota source health |
| POST | `/api/signals` | Push a reset signal (see above) |
| POST | `/api/hooks` | Session events (used by `orchestrator hook` / `event`) |
