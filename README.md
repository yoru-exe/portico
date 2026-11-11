# Portico

A small web server and reverse proxy written in Go. Standard library only, no dependencies, one binary.

## Build

    go build -o portico .

## Quick start

    portico serve -dir ./public              # static site on :8080
    portico serve -dir ./dist -spa           # single-page app (index.html fallback)
    portico proxy http://localhost:3000      # reverse proxy
    portico proxy -policy least_conn http://localhost:3000 http://localhost:3001

Flags go before positional arguments.

## Config file

    portico init          # writes an example portico.json
    portico validate      # checks it
    portico run           # runs it
    kill -HUP <pid>       # hot reload, zero downtime; a bad config is rejected and the old one keeps running

See `portico init` output for the full format. Features:

- Host routing (exact names, `*.example.com`, catch-all) and longest-prefix path routing
- Static files: no directory listings, dotfiles hidden (`.env`, `.git`), optional SPA mode
- Reverse proxy: round-robin or least-connections, active health checks, `strip_prefix`,
  `preserve_host`, trusted X-Forwarded-* headers, WebSocket support
- Redirects (301/302/303/307/308, optional `keep_path`)
- TLS with your own cert/key, HTTP/2, optional HTTP to HTTPS redirect
- gzip for text responses, per-IP rate limiting, access log, panic recovery,
  graceful shutdown, slowloris protection

## Not (yet) included

- Automatic Let's Encrypt certificates: add `golang.org/x/crypto/acme/autocert`
  and plug its `GetCertificate` into the TLS config in `main.go`
- TLS cert reload on SIGHUP (listen addresses also need a restart)
- HTTP/3, caching, per-route timeouts, metrics endpoint

## Layout

    main.go        CLI, startup, signals, TLS
    config.go      config structs, validation, defaults
    server.go      hot-reload runtime, routing, static, proxy, balancer, health checks
    middleware.go  recover, access log, rate limit, gzip
