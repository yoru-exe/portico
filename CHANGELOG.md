# Changelog

All notable changes to Portico are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While the version is below 1.0.0, minor releases may include behaviour changes; they are
called out under **Changed**.

## [Unreleased]

### Fixed
- Docker Hub publishing is now enabled by the `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` repository
  secrets alone (the image defaults to `<username>/portico`). Previously it also required the
  `DOCKERHUB_IMAGE` variable and was silently skipped when that was missing or in the wrong place.
  The workflow summary now states whether Docker Hub is enabled and which secret is missing.

## [0.2.0] - 2026-11-13

A reliability release: a compact new config format, much better proxy failover and connection
handling, and the project is now Apache-2.0 licensed.

### Added
- **Porticofile**, a short Caddyfile-style config format. Any config file whose name does not end
  in `.json` is read as one, and `portico run` picks up `./Porticofile` automatically. It supports
  `reverse_proxy`, `file_server` and `redir` directives plus a global options block (listen
  addresses, `encode gzip`, `rate_limit`, `tls`, `redirect_http`, `http3`, `metrics`), and goes
  through the same validation as the JSON format.
- `portico adapt` prints a Porticofile as the equivalent JSON.
- `portico init -format porticofile` writes an example Porticofile.
- Porticofile errors name the file and line, e.g. `Porticofile:11: unknown reverse_proxy option "strip_prefx"`.
- `response_header_timeout` option for proxy routes (default 60s, `-1s` disables).
- Apache-2.0 license (`LICENSE`, `NOTICE`). Both files, and an `org.opencontainers.image.licenses`
  label, are also included in the Docker image.
- Regression tests for every fix below, and tests for the Porticofile.

### Changed
- Upstream connections time out after 5s, and waiting for response headers after 60s. Long-polling
  endpoints that delay their headers for longer need a larger `response_header_timeout`.
- Request paths are normalised before routing: `//` is merged and `.`/`..` segments are resolved
  (as nginx does).
- Upstream traffic no longer goes through `HTTP_PROXY`/`HTTPS_PROXY` from the environment.
- Without `-config`, Portico uses `./Porticofile` if it exists, else `./portico.json`. The Docker
  image's default command is unchanged.
- Upstream responses no longer pass their own `Server` header through; Portico's is sent instead.

### Fixed
- **Failover:** a dead upstream no longer causes client-visible 502 errors. Connection failures are
  retried on the next upstream (for requests without a body) and the failed upstream is skipped
  for 10 seconds. Previously roughly half of all requests failed until the next health check.
- **Connection pooling:** upstream connections are now reused. Under a 150-client load test, new
  TCP connections to the upstream dropped from 1,708 to 171 per 3,000 requests.
- `%2F` and other percent-encoding in paths was decoded when `strip_prefix` was enabled, changing
  what the upstream received. Encoding is now preserved.
- Paths containing dot segments (e.g. `/api/../admin`) could match a route and reach the upstream
  unnormalised, confusing route matching.
- Duplicate `Server` headers on proxied responses leaked the upstream's server software.
- `Accept-Encoding: gzip;q=0` was still compressed.
- Hosts written with a trailing dot (`example.com.`) returned 404.
- JSON config files with extra data after the config object were accepted, and empty files produced
  a cryptic error.
- The response cache could store and replay `text/event-stream` responses.
- Prometheus label values were not escaped.
- Automatic HTTPS: the HTTP-01 challenge handler rejected requests whose `Host` header carried a
  port (for example when port 80 is forwarded from another port).

## [0.1.0] - 2026-11-12

First release.

### Added
- Static file server: no directory listings, dotfiles hidden (except `.well-known`), optional
  single-page-app mode.
- Reverse proxy with round-robin and least-connections load balancing, active health checks,
  `strip_prefix`, `preserve_host`, trusted `X-Forwarded-*` headers and WebSocket support.
- Host routing (exact names, `*.example.com`, catch-all) and longest-prefix path routing; redirects.
- HTTPS with your own certificate, HTTP/2, HTTP to HTTPS redirect, and certificate reload on `SIGHUP`.
- Automatic HTTPS via ACME (Let's Encrypt), with certificates cached on disk and a configurable CA.
- Opt-in HTTP/3 (QUIC) with `go build -tags http3`.
- In-memory LRU response cache for proxy routes.
- gzip compression, per-IP rate limiting, access log, panic recovery, graceful shutdown and
  slowloris protection.
- Prometheus metrics endpoint.
- Zero-downtime configuration reload with `SIGHUP`; an invalid config is rejected and the old one
  keeps serving.
- Command line: `run`, `serve`, `proxy`, `init`, `validate`, `version`.
- Multi-arch (amd64, arm64) non-root Docker image published to GitHub Container Registry, with
  GitHub Actions workflows for tests and image publishing.

[Unreleased]: https://github.com/yoru-exe/portico/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/yoru-exe/portico/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/yoru-exe/portico/releases/tag/v0.1.0
