# Security Policy

The router sits on the request path between agents and model providers and
handles provider API keys, BYOK secrets, and user traffic. We take security
reports seriously.

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately via GitHub Security Advisories:

➡️ **[Open a private advisory](https://github.com/workweave/router/security/advisories/new)**

If you can't use GitHub advisories, email **security@workweave.ai**.

Please include:

- A description of the vulnerability and its impact.
- Steps to reproduce (a minimal proof-of-concept is ideal).
- Affected version / commit, and your environment if relevant.

## What to expect

- We aim to acknowledge a report within **3 business days**.
- We'll keep you updated as we investigate and work on a fix.
- We'll coordinate disclosure with you and credit you in the release notes
  unless you'd prefer to remain anonymous.

## Scope

In scope: the router server (`internal/`, `cmd/`), the install scripts under
`install/`, and the dashboard frontend.

Particularly interested in:

- Leakage of API keys, BYOK secrets, or bearer tokens (in logs, errors, or
  responses).
- Auth bypass on `/admin/v1/*` or the dashboard.
- SSRF / request smuggling through the proxy path.
- Cross-format translation bugs that could exfiltrate one tenant's data to
  another.

Out of scope: vulnerabilities in upstream model providers themselves, and
issues that require a pre-compromised host or a malicious deployment operator.

## Handling of secrets

The router never logs raw bearer tokens or full key hashes — only an 8-char
prefix + 4-char suffix. BYOK secrets are encrypted at rest via AES-256-GCM
(Tink) and held in plaintext only for a request's lifetime. If you find a code
path that violates this, treat it as a security issue and report it privately.
