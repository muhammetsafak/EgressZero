# Security Policy

## Supported versions

Only the latest release receives security fixes.

## Reporting a vulnerability

Please **do not** open a public issue for security problems.

- Preferred: use GitHub's [private vulnerability reporting](https://github.com/muhammetsafak/EgressZero/security/advisories/new) on this repository.
- Alternatively, email [info@muhammetsafak.com.tr](mailto:info@muhammetsafak.com.tr).

You will get an initial response within a few days. Please include steps to reproduce and the impact you foresee.

## Scope notes

EgressZero is designed to run **behind a CDN**, not exposed directly to end users. Reports about behavior that only matters when the proxy is deployed without the documented `PROXY_AUTH_SECRET` / firewall setup are still welcome, but will be triaged as hardening rather than vulnerabilities.
