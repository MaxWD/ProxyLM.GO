# Security Policy

## Supported Versions

Only the latest minor release branch receives security fixes. Pre-1.0 releases older
than the current minor are not supported.

| Version  | Supported          |
|----------|--------------------|
| v0.9.x   | Yes (current)      |
| < v0.9   | No                 |

Once v1.0.0 is released, this table will be updated to reflect stable support windows.

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub Issues, Discussions,
or any other public forum.** Public disclosure before a fix is available puts all users
at risk.

### Preferred channel — GitHub Security Advisories (private)

Use the GitHub private advisory form:
https://github.com/MaxWD/ProxyLM.GO/security/advisories/new

This is the preferred channel. Your report stays confidential and is visible only to
the maintainer until a coordinated disclosure date is agreed upon.

### Alternative channel — email

If you are unable to use GitHub Security Advisories, send a PGP-encrypted or plaintext
email to:

    maxim.dolgushew.w@gmail.com

Include the subject line `[SECURITY] ProxyLM.GO` and provide as much detail as possible
(see the template below).

### What to include in a report

- Affected component and version (output of `proxylm version`).
- Steps to reproduce the vulnerability.
- Proof-of-concept code or payload (if available).
- Potential impact and attack scenario.
- Any suggested fix or mitigation.

### Response timeline (SLA)

| Milestone                              | Target        |
|----------------------------------------|---------------|
| Acknowledgment of receipt              | 7 days        |
| Fix or mitigation for High / Critical  | 30 days       |
| Fix or mitigation for Medium / Low     | 90 days       |
| Coordinated public disclosure          | Agreed with reporter |

These are best-effort targets. Complex issues involving upstream dependencies may take
longer; the maintainer will communicate delays promptly.

## Scope

### In scope

The following are considered valid security issues:

- Authentication and authorization bypass in the admin API or IPC layer.
- Privilege escalation through the service installer or daemon.
- Remote code execution via crafted HTTP requests to the proxy.
- SQL injection or path traversal in storage or config handling.
- Information disclosure of API keys, admin tokens, or request content via logs, DB,
  or TUI.
- Denial-of-service through authenticated endpoints (if reachable without brute-force).
- Bugs in the scheduler, router, or retry logic that allow request forgery between
  tenants.

### Out of scope

The following are **not** considered security issues for this project:

- Denial-of-service via unauthenticated flood (network-layer; mitigate at the operator
  level with a reverse proxy or firewall).
- Vulnerabilities in upstream LLM servers (LM Studio, Ollama, or any OpenAI-compatible
  backend) — report those to the respective projects.
- Security misconfigurations in operator-supplied `config.yaml` (e.g., binding to
  `0.0.0.0` on a public network, weak `admin_key`). These are operator responsibility.
- Theoretical issues without a demonstrated exploit path.

## Disclosure Policy

ProxyLM.GO follows **coordinated disclosure**. Once a fix is ready and released:

1. A GitHub Security Advisory is published.
2. The release notes in `CHANGELOG.md` include a `Security` section.
3. Credit is given to the reporter (unless they prefer to remain anonymous).
