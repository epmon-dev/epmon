# Security policy

epmon is a self-hosted monitor: it probes operator-configured URLs and
serves the results. There is no hosted service, no account system, and no
telemetry leaving your machine.

## Reporting a vulnerability

Open a private security advisory on GitHub
(`github.com/epmon-dev/epmon` → Security → Advisories). Include the
version (`/healthz` reports it via headers, or `git describe`), a minimal
config that reproduces the issue, and what you expected instead.

We aim to acknowledge within 2 business days and to ship a fix before any
public disclosure. Please do not open public issues for vulnerabilities.

## Scope notes

- Probe targets are operator-configured by definition; epmon fetches no
  user-supplied URLs beyond your own config file.
- Bearer tokens and header values are never logged. If you find one in
  output, that is a bug — report it as above.
- The managed cloud (`epmon.dev`) is a separate, proprietary codebase and
  out of scope for this repo.
