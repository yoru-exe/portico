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

    portico init          # writes an example portico.json (or: init -format porticofile)
    portico validate      # checks it
    portico run           # runs it (-config FILE, default ./Porticofile or ./portico.json)
    kill -HUP <pid>       # hot reload, zero downtime

A reload is all-or-nothing: if the new config (or TLS certificate) is invalid, nothing
changes and the old one keeps serving. Unknown keys are errors, so typos are caught.

## Docker

    docker run -p 8080:8080 ghcr.io/yoru-exe/portico                       # welcome page, like nginx
    docker run -p 8080:8080 -v ./site:/srv:ro ghcr.io/yoru-exe/portico     # serve your own site
    docker run -p 8080:8080 -v ./portico.json:/etc/portico/portico.json:ro ghcr.io/yoru-exe/portico

The same image is published to Docker Hub as `yoruexe/portico`
(`docker run -p 8080:8080 yoruexe/portico`).

Tags: `latest` and `1.2.3` / `1.2` / `1` (releases), `edge` (main branch). Images are
multi-arch (`linux/amd64`, `linux/arm64`), built `FROM distroless/static` (no shell, ~10 MB),
run as a non-root user (uid 65532) and include HTTP/3.

| Path in container          | Purpose                                                          |
|----------------------------|------------------------------------------------------------------|
| `/etc/portico/portico.json`| config (the default serves `/srv` on port 8080)                  |
| `/srv`                     | default web root                                                 |
| `/data`                    | working directory and a volume: ACME certificates are kept here |

Reload without downtime: `docker kill -s HUP <container>`. For automatic HTTPS publish
`-p 80:80 -p 443:443 -p 443:443/udp`, mount a volume on `/data`, and set
`--sysctl net.ipv4.ip_unprivileged_port_start=0` so the non-root user can bind 80/443
(Docker 20.10+ already does this by default). See `docker-compose.yml` for a full example.

Build it yourself: `docker build --build-arg VERSION=dev -t portico .`

### Publishing (maintainers)

Pushing to `main` publishes `ghcr.io/<owner>/portico:edge`; pushing a tag such as `v1.2.3`
publishes the release tags. The first time, make the package public under
*Profile -> Packages -> portico -> Package settings -> Change visibility*.
To also push to Docker Hub, set the repository variable `DOCKERHUB_IMAGE` (`yoruexe/portico`)
and the secrets `DOCKERHUB_USERNAME` (`yoruexe`) / `DOCKERHUB_TOKEN` (an access token with
Read & Write scope, from Docker Hub -> Account settings -> Personal access tokens).

## Porticofile (Caddyfile-style config)

Prefer something shorter than JSON? Any config file whose name does not end in `.json` is read as a
**Porticofile**. `portico run` picks up `./Porticofile` automatically (else `./portico.json`).

    portico init -format porticofile   # write an example Porticofile
    portico validate                   # check it (errors name the file and line)
    portico adapt                      # print it as the equivalent JSON
    portico run

```
# global options (optional, one block, must come with no address)
{
	listen :80
	encode gzip
	metrics 127.0.0.1:9090
	rate_limit 50 100            # requests per second, optional burst
	tls auto you@example.com     # automatic HTTPS
	redirect_http
	http3
}

example.com, www.example.com {
	reverse_proxy /api/* localhost:3000 localhost:3001 {
		strip_prefix
		policy least_conn
		health_check /health 10s 2s
		cache 30s
	}
	redir /old https://example.com/new 301
	file_server ./public spa
}
```

Everything in the JSON format has an equivalent, and both formats go through the same validation.
Run `portico adapt` to see exactly what a Porticofile means.

| Directive (inside a site block) | Meaning |
|---|---|
| `reverse_proxy [/path/*] upstream... { ... }` | proxy to one or more upstreams (`host:port`, `:port`, or a full URL). Options: `strip_prefix`, `preserve_host`, `policy round_robin\|least_conn`, `health_check <path> [interval [timeout]]`, `cache [ttl] { max_mb N; max_entry_kb N }`, `response_header_timeout <duration>` |
| `file_server [/path/*] <dir> [spa]` | serve static files; `spa` falls back to `index.html` |
| `redir [/path/*] <url> [301\|302\|303\|307\|308\|permanent\|temporary]` | redirect; end the URL with `{uri}` to append the original request URI |

| Global option | Meaning |
|---|---|
| `listen <addr>` / `https_listen <addr>` / `metrics <addr>` | listen addresses |
| `encode gzip` | compress text responses |
| `rate_limit <per-second> [burst]` | per-client-IP rate limit |
| `tls auto [email] { domains ...; ca <url>; ca_cert <file>; storage <dir> }` | automatic HTTPS (see below) |
| `tls <cert-file> <key-file>` | your own certificate |
| `redirect_http`, `http3` | redirect HTTP to HTTPS; serve HTTP/3 (needs a `-tags http3` build) |

Site addresses are host names (`example.com`, `*.example.com`, `localhost`); a bare port such as `:8080`
or `*` matches any host. A path matcher is `/prefix/*` (or `*` for everything); the longest prefix wins.
Comments start with `#`; values with spaces go in "double quotes".

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

### Proxy behaviour worth knowing

- **Failover:** if an upstream refuses connections, the request is retried on the next one
  (body-less requests such as GET), and the failed upstream is skipped for 10 seconds.
  Active `health_check`s take upstreams in and out of rotation on top of that.
- **Timeouts:** connecting to an upstream times out after 5s; waiting for response headers after 60s
  (`response_header_timeout`, `-1s` disables). Idle upstream connections are pooled (256 per upstream).
- **Paths** are normalised before routing: `/api/../x` and `//x` become `/x` (like nginx), so a
  request can't slip past a route or reach an upstream with dot segments in its path.
  Percent-encoding is preserved (`/api/a%2Fb` with `strip_prefix` reaches the upstream as `/a%2Fb`).
- The upstream's `Server` header is replaced by Portico's; `X-Forwarded-*` headers are set by Portico
  and any sent by the client are dropped (so they can't be spoofed).

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
    Dockerfile, docker/, docker-compose.yml, .github/workflows/   container image and CI

## Changelog

See [CHANGELOG.md](CHANGELOG.md) for what changed in each release.

## License

Apache License 2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE). Dependencies keep their own
licenses: `golang.org/x/crypto` (BSD-3-Clause) and, in HTTP/3 builds, `quic-go` (MIT).
