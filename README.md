# Portico

A small web server and reverse proxy written in Go. One binary, automatic HTTPS, hot reload.

## Build

    go mod tidy                      # once: downloads dependencies and writes go.sum
    go build -o portico .            # standard build
    go build -tags http3 -o portico . # optional: adds HTTP/3 (QUIC), ~2 MB larger

Dependencies: `golang.org/x/crypto` (automatic certificates) and, only with `-tags http3`, `quic-go`.

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
    kill -HUP <pid>       # hot reload, zero downtime

A reload is all-or-nothing: if the new config (or TLS certificate) is invalid, nothing
changes and the old one keeps serving. Unknown keys are errors, so typos are caught.

## Automatic HTTPS (Let's Encrypt)

    {
      "listen": ":80",
      "tls": { "auto": { "email": "you@example.com" }, "redirect_http": true },
      "sites": [{ "hosts": ["example.com", "www.example.com"], "routes": [ ... ] }]
    }

Certificates are obtained on first use and renewed automatically; they are cached in
`tls.auto.cache_dir` (default `portico-certs`) so restarts don't re-issue. Requirements:
ports 80 and 443 reachable from the internet and DNS pointing at this machine. Domains
default to the non-wildcard hosts in `sites`; set `tls.auto.domains` to override. The
domain list is re-read on SIGHUP. While testing, use staging to avoid rate limits:
`"directory_url": "https://acme-staging-v02.api.letsencrypt.org/directory"`.
Wildcard certificates (DNS-01) are not supported.

To use your own certificate instead: `"tls": { "cert": "x.pem", "key": "x.key" }`.
It is re-read on SIGHUP.

## HTTP/3

Build with `-tags http3` and set `"tls": { ..., "http3": true }`. Portico then serves QUIC
on the same port number over UDP (open it in your firewall) and advertises it via `Alt-Svc`.
A binary built without the tag refuses to start if `http3` is enabled.

## Response cache

    "proxy": { ..., "cache": { "default_ttl": "30s", "max_mb": 64, "max_entry_kb": 1024 } }

In-memory LRU per proxy route. It is deliberately conservative: only GET requests with no
`Authorization`, `Cookie`, `Range` or `Cache-Control: no-cache` header are cached, and only
responses without `Set-Cookie`, `private`, `no-store`, `no-cache` or a `Vary` other than
`Accept-Encoding`. Freshness comes from `s-maxage`/`max-age`, else `default_ttl` (0 = only
cache when the upstream says so). Responses carry `X-Cache: HIT|MISS|BYPASS` and `Age`;
`If-None-Match` is answered with 304 from the cache. No request coalescing (a stampede on
an expired hot key reaches the upstream).

## Metrics

Set `"metrics_listen": "127.0.0.1:9090"` and scrape `/metrics` (Prometheus text format):
request counts by status class, in-flight, bytes, duration sum/count, cache hits/misses,
config reloads, and per-upstream health, active requests, requests and failures.
The endpoint is unauthenticated: keep it on localhost or a private network.

## Other features

- Host routing (exact, `*.example.com`, catch-all) and longest-prefix path routing
- Static files: no directory listings, dotfiles hidden (except `.well-known`), optional SPA mode
- Reverse proxy: round-robin or least-connections, active health checks, `strip_prefix`,
  `preserve_host`, trusted X-Forwarded-* headers, WebSocket support
- Redirects, gzip, per-IP rate limiting, access log, panic recovery, graceful shutdown,
  slowloris protection, HTTP/2

## Limits

Listen addresses and the TLS mode (auto/manual) change only on restart. No DNS-01/wildcard
certs, no per-route timeouts, no request coalescing in the cache.

## Tests

    go test -race ./...

## Layout

    main.go        CLI, startup, signals, TLS and ACME, reload
    config.go      config structs, validation, defaults
    server.go      hot-reload runtime, routing, static, proxy, balancer, health checks
    middleware.go  recover, access log, rate limit, gzip
    cache.go       response cache
    metrics.go     Prometheus endpoint
    http3.go       HTTP/3 listener (-tags http3); http3_stub.go is the default build
