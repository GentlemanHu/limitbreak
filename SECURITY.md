# Security Policy

## Reporting a vulnerability

Please report security issues privately through GitHub's **Report a vulnerability** button (Security → Advisories) on this repository. Do not open a public issue for anything that could let another local user, website or process send messages into a session, read tokens, or bypass the consent and pause controls.

Include the version (`VERSION` file or the About panel), macOS version, and steps to reproduce. You can expect an acknowledgement within a few days; fixes are released as a new tagged version.

## Scope

In scope:

- the local HTTP API and SSE endpoint (token handling, `Origin` rejection, loopback binding);
- the MCP server and its scoped profiles;
- the hooks endpoint and hook installation;
- the direct-delivery socket channel and the Accessibility bridge;
- reading of `~/.claude/sessions/*.json` (only non-secret fields are allowed);
- the installer script and release artifacts.

Out of scope: vulnerabilities in CLIProxyAPI, Claude, or macOS themselves (please report those upstream).

## Design guarantees

- Private state lives in `~/Library/Application Support/Quota Session Guard/` with `0600` files.
- `.key` token files and conversation content are never read.
- Permission modes are never asserted or spoofed.
- Automatic sending requires an explicit, timestamped consent and stops immediately on emergency pause.
