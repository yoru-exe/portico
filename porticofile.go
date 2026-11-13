package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// A Porticofile is a compact, Caddyfile-style alternative to the JSON config:
//
//	{
//		encode gzip
//	}
//
//	example.com, www.example.com {
//		reverse_proxy /api/* localhost:3000 localhost:3001 {
//			health_check /health 10s
//		}
//		file_server ./public
//	}
//
// It is parsed into the same Config as the JSON format, so both go through the
// same validation and behave identically.

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type pfError struct {
	line int
	msg  string
}

func (e *pfError) Error() string { return fmt.Sprintf("line %d: %s", e.line, e.msg) }

type pfTok struct {
	s      string
	quoted bool
}

type pfLine struct {
	no   int
	toks []pfTok
}

// pfTokenize splits the source into lines of tokens. Tokens are separated by
// spaces or tabs; "#" starts a comment; "double quotes" group a token.
func pfTokenize(src string) ([]pfLine, error) {
	src = strings.TrimPrefix(src, "\ufeff")
	var out []pfLine
	for i, raw := range strings.Split(src, "\n") {
		no := i + 1
		raw = strings.TrimRight(raw, "\r")
		var toks []pfTok
		for j := 0; j < len(raw); {
			switch c := raw[j]; {
			case c == ' ' || c == '\t':
				j++
			case c == '#':
				j = len(raw)
			case c == '"':
				var sb strings.Builder
				j++
				closed := false
				for j < len(raw) {
					if raw[j] == '\\' && j+1 < len(raw) && (raw[j+1] == '"' || raw[j+1] == '\\') {
						sb.WriteByte(raw[j+1])
						j += 2
						continue
					}
					if raw[j] == '"' {
						closed = true
						j++
						break
					}
					sb.WriteByte(raw[j])
					j++
				}
				if !closed {
					return nil, &pfError{no, "unterminated quoted string"}
				}
				toks = append(toks, pfTok{sb.String(), true})
			default:
				k := j
				for k < len(raw) && raw[k] != ' ' && raw[k] != '\t' {
					k++
				}
				toks = append(toks, pfTok{raw[j:k], false})
				j = k
			}
		}
		if len(toks) > 0 {
			out = append(out, pfLine{no, toks})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Parser: lines of tokens -> tree of directives
// ---------------------------------------------------------------------------

type pfNode struct {
	name     string // empty for the global "{" block
	args     []string
	block    []*pfNode
	hasBlock bool
	line     int
}

type pfParser struct {
	lines []pfLine
	pos   int
}

func (p *pfParser) parseBlock(depth, openLine int) ([]*pfNode, error) {
	var nodes []*pfNode
	for p.pos < len(p.lines) {
		ln := p.lines[p.pos]
		p.pos++

		if len(ln.toks) == 1 && !ln.toks[0].quoted && ln.toks[0].s == "}" {
			if depth == 0 {
				return nil, &pfError{ln.no, "unexpected }"}
			}
			return nodes, nil
		}

		toks := ln.toks
		open := false
		if last := toks[len(toks)-1]; !last.quoted && last.s == "{" {
			open = true
			toks = toks[:len(toks)-1]
		}
		for _, t := range toks {
			if !t.quoted && (t.s == "{" || t.s == "}") {
				return nil, &pfError{ln.no, "braces must stand alone: put { at the end of a line and } on a line of its own"}
			}
		}

		n := &pfNode{line: ln.no}
		if len(toks) > 0 {
			n.name = toks[0].s
			for _, t := range toks[1:] {
				n.args = append(n.args, t.s)
			}
		}
		if open {
			blk, err := p.parseBlock(depth+1, ln.no)
			if err != nil {
				return nil, err
			}
			n.block, n.hasBlock = blk, true
		}
		nodes = append(nodes, n)
	}
	if depth > 0 {
		return nil, &pfError{openLine, "missing closing } for the block opened here"}
	}
	return nodes, nil
}

// parsePorticofile parses source text into a Config (without validating it).
// filename is only used in error messages.
func parsePorticofile(src, filename string) (*Config, error) {
	wrap := func(err error) error {
		var pe *pfError
		if errors.As(err, &pe) {
			return fmt.Errorf("%s:%d: %s", filename, pe.line, pe.msg)
		}
		return fmt.Errorf("%s: %w", filename, err)
	}
	lines, err := pfTokenize(src)
	if err != nil {
		return nil, wrap(err)
	}
	p := &pfParser{lines: lines}
	nodes, err := p.parseBlock(0, 0)
	if err != nil {
		return nil, wrap(err)
	}
	cfg, err := adaptPorticofile(nodes)
	if err != nil {
		return nil, wrap(err)
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// Adapter: directive tree -> Config
// ---------------------------------------------------------------------------

func errAt(n *pfNode, format string, a ...any) error {
	return &pfError{n.line, fmt.Sprintf(format, a...)}
}

func adaptPorticofile(nodes []*pfNode) (*Config, error) {
	cfg := &Config{}
	haveGlobal := false
	for _, n := range nodes {
		if n.name == "" && n.hasBlock {
			if haveGlobal {
				return nil, errAt(n, "only one global { } block is allowed")
			}
			haveGlobal = true
			if err := adaptGlobal(cfg, n); err != nil {
				return nil, err
			}
			continue
		}
		if !n.hasBlock {
			return nil, errAt(n, "%q is not a directive here: sites look like `example.com { ... }`", n.name)
		}
		site, err := adaptSite(n)
		if err != nil {
			return nil, err
		}
		cfg.Sites = append(cfg.Sites, *site)
	}
	return cfg, nil
}

func noArgs(d *pfNode) error {
	if len(d.args) != 0 {
		return errAt(d, "%s takes no arguments", d.name)
	}
	return nil
}

func oneArg(d *pfNode, what string) (string, error) {
	if len(d.args) != 1 {
		return "", errAt(d, "usage: %s <%s>", d.name, what)
	}
	return d.args[0], nil
}

func durArg(d *pfNode, s string) (time.Duration, error) {
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, errAt(d, "%q is not a duration (examples: 500ms, 10s, 2m)", s)
	}
	return v, nil
}

func intArg(d *pfNode, s string) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return 0, errAt(d, "%q is not a non-negative whole number", s)
	}
	return v, nil
}

func adaptGlobal(cfg *Config, g *pfNode) error {
	tc := &TLSConfig{}
	var haveTLS bool
	var tlsDependent *pfNode // first directive that only makes sense with tls
	for _, d := range g.block {
		switch d.name {
		case "listen":
			a, err := oneArg(d, "address")
			if err != nil {
				return err
			}
			cfg.Listen = a
		case "metrics":
			a, err := oneArg(d, "address")
			if err != nil {
				return err
			}
			cfg.MetricsListen = a
		case "encode":
			if len(d.args) != 1 || d.args[0] != "gzip" {
				return errAt(d, "only `encode gzip` is supported")
			}
			cfg.Gzip = true
		case "rate_limit":
			if len(d.args) < 1 || len(d.args) > 2 {
				return errAt(d, "usage: rate_limit <requests-per-second> [burst]")
			}
			rps, err := strconv.ParseFloat(strings.TrimSuffix(d.args[0], "/s"), 64)
			if err != nil || rps <= 0 {
				return errAt(d, "%q is not a positive number of requests per second", d.args[0])
			}
			rl := &RateLimitConfig{RequestsPerSecond: rps}
			if len(d.args) == 2 {
				if rl.Burst, err = intArg(d, d.args[1]); err != nil {
					return err
				}
			}
			cfg.RateLimit = rl
		case "https_listen":
			a, err := oneArg(d, "address")
			if err != nil {
				return err
			}
			tc.Listen = a
			if tlsDependent == nil {
				tlsDependent = d
			}
		case "redirect_http":
			if err := noArgs(d); err != nil {
				return err
			}
			tc.RedirectHTTP = true
			if tlsDependent == nil {
				tlsDependent = d
			}
		case "http3":
			if err := noArgs(d); err != nil {
				return err
			}
			tc.HTTP3 = true
			if tlsDependent == nil {
				tlsDependent = d
			}
		case "tls":
			if haveTLS {
				return errAt(d, "tls may only appear once")
			}
			haveTLS = true
			if err := adaptTLS(tc, d); err != nil {
				return err
			}
		default:
			return errAt(d, "unknown global option %q (valid: listen, metrics, encode, rate_limit, tls, https_listen, redirect_http, http3)", d.name)
		}
	}
	if tlsDependent != nil && !haveTLS {
		return errAt(tlsDependent, "%s needs a `tls` directive (for example `tls auto you@example.com`)", tlsDependent.name)
	}
	if haveTLS {
		cfg.TLS = tc
	}
	return nil
}

func adaptTLS(tc *TLSConfig, d *pfNode) error {
	if len(d.args) >= 1 && d.args[0] == "auto" {
		if len(d.args) > 2 {
			return errAt(d, "usage: tls auto [email] [{ ... }]")
		}
		a := &AutoTLSConfig{}
		if len(d.args) == 2 {
			a.Email = d.args[1]
		}
		for _, s := range d.block {
			switch s.name {
			case "domains":
				if len(s.args) == 0 {
					return errAt(s, "usage: domains <domain>...")
				}
				a.Domains = append(a.Domains, s.args...)
			case "ca":
				v, err := oneArg(s, "directory-url")
				if err != nil {
					return err
				}
				a.DirectoryURL = v
			case "ca_cert":
				v, err := oneArg(s, "file")
				if err != nil {
					return err
				}
				a.CACertFile = v
			case "storage":
				v, err := oneArg(s, "directory")
				if err != nil {
					return err
				}
				a.CacheDir = v
			default:
				return errAt(s, "unknown tls option %q (valid: domains, ca, ca_cert, storage)", s.name)
			}
		}
		tc.Auto = a
		return nil
	}
	if len(d.args) == 2 && !d.hasBlock {
		tc.Cert, tc.Key = d.args[0], d.args[1]
		return nil
	}
	return errAt(d, "usage: `tls auto [email]` or `tls <cert-file> <key-file>`")
}

// parseAddr turns a site address into a host the router understands.
func parseAddr(d *pfNode, s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if strings.ContainsAny(s, "/ ") {
		return "", errAt(d, "site address %q must be a host name (paths are matched inside the block)", s)
	}
	if s == "" || s == "*" || strings.HasPrefix(s, ":") {
		return "*", nil // catch-all (a bare port like :8080 means any host)
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	return strings.Trim(s, "[]"), nil
}

func adaptSite(n *pfNode) (*SiteConfig, error) {
	fields := strings.Fields(strings.ReplaceAll(strings.Join(append([]string{n.name}, n.args...), " "), ",", " "))
	site := &SiteConfig{}
	seen := map[string]bool{}
	for _, f := range fields {
		h, err := parseAddr(n, f)
		if err != nil {
			return nil, err
		}
		if !seen[h] {
			seen[h] = true
			site.Hosts = append(site.Hosts, h)
		}
	}

	for _, d := range n.block {
		var rc RouteConfig
		var err error
		switch d.name {
		case "reverse_proxy":
			rc, err = adaptProxy(d)
		case "file_server":
			rc, err = adaptFileServer(d)
		case "redir":
			rc, err = adaptRedir(d)
		case "encode", "rate_limit", "listen", "metrics", "tls", "https_listen", "redirect_http", "http3":
			err = errAt(d, "%s is a global option: move it into the global { } block at the top of the file", d.name)
		default:
			err = errAt(d, "unknown directive %q (valid in a site: reverse_proxy, file_server, redir)", d.name)
		}
		if err != nil {
			return nil, err
		}
		site.Routes = append(site.Routes, rc)
	}
	if len(site.Routes) == 0 {
		return nil, errAt(n, "site %q has no directives", n.name)
	}
	return site, nil
}

// matcherPath converts "*" or "/api/*" or "/api" into a route path prefix.
func matcherPath(m string) string {
	m = strings.TrimSuffix(m, "*")
	if len(m) > 1 {
		m = strings.TrimSuffix(m, "/")
	}
	if m == "" {
		return "/"
	}
	return m
}

func isMatcher(s string) bool { return s == "*" || strings.HasPrefix(s, "/") }

func normalizeUpstream(s string) string {
	switch {
	case strings.Contains(s, "://"):
		return s
	case strings.HasPrefix(s, ":"):
		return "http://localhost" + s
	}
	return "http://" + s
}

func adaptProxy(d *pfNode) (RouteConfig, error) {
	args, path := d.args, "/"
	if len(args) > 0 && isMatcher(args[0]) {
		path, args = matcherPath(args[0]), args[1:]
	}
	if len(args) == 0 {
		return RouteConfig{}, errAt(d, "usage: reverse_proxy [/path/*] <upstream>...  (for example localhost:3000)")
	}
	pc := &ProxyConfig{}
	for _, a := range args {
		pc.Upstreams = append(pc.Upstreams, normalizeUpstream(a))
	}
	rc := RouteConfig{Path: path, Proxy: pc}

	for _, s := range d.block {
		switch s.name {
		case "strip_prefix":
			if err := noArgs(s); err != nil {
				return rc, err
			}
			rc.StripPrefix = true
		case "preserve_host":
			if err := noArgs(s); err != nil {
				return rc, err
			}
			pc.PreserveHost = true
		case "policy":
			v, err := oneArg(s, "round_robin|least_conn")
			if err != nil {
				return rc, err
			}
			pc.Policy = v
		case "response_header_timeout":
			v, err := oneArg(s, "duration")
			if err != nil {
				return rc, err
			}
			dv, err := durArg(s, v)
			if err != nil {
				return rc, err
			}
			pc.ResponseHeaderTimeout = Duration(dv)
		case "health_check":
			if len(s.args) < 1 || len(s.args) > 3 {
				return rc, errAt(s, "usage: health_check <path> [interval [timeout]]")
			}
			hc := &HealthCheckConfig{Path: s.args[0]}
			if len(s.args) >= 2 {
				dv, err := durArg(s, s.args[1])
				if err != nil {
					return rc, err
				}
				hc.Interval = Duration(dv)
			}
			if len(s.args) == 3 {
				dv, err := durArg(s, s.args[2])
				if err != nil {
					return rc, err
				}
				hc.Timeout = Duration(dv)
			}
			pc.HealthCheck = hc
		case "cache":
			if len(s.args) > 1 {
				return rc, errAt(s, "usage: cache [default-ttl] [{ max_mb N; max_entry_kb N }]")
			}
			cc := &CacheConfig{}
			if len(s.args) == 1 {
				dv, err := durArg(s, s.args[0])
				if err != nil {
					return rc, err
				}
				cc.DefaultTTL = Duration(dv)
			}
			for _, o := range s.block {
				v, err := oneArg(o, "number")
				if err != nil {
					return rc, err
				}
				n, err := intArg(o, v)
				if err != nil {
					return rc, err
				}
				switch o.name {
				case "max_mb":
					cc.MaxMB = n
				case "max_entry_kb":
					cc.MaxEntryKB = n
				default:
					return rc, errAt(o, "unknown cache option %q (valid: max_mb, max_entry_kb)", o.name)
				}
			}
			pc.Cache = cc
		default:
			return rc, errAt(s, "unknown reverse_proxy option %q (valid: strip_prefix, preserve_host, policy, health_check, cache, response_header_timeout)", s.name)
		}
	}
	return rc, nil
}

func adaptFileServer(d *pfNode) (RouteConfig, error) {
	spa := false
	var rest []string
	for _, a := range d.args {
		if a == "spa" {
			spa = true
		} else {
			rest = append(rest, a)
		}
	}
	if d.hasBlock {
		return RouteConfig{}, errAt(d, "file_server does not take a block")
	}
	switch len(rest) {
	case 1:
		return RouteConfig{Path: "/", Static: &StaticConfig{Root: rest[0], SPA: spa}}, nil
	case 2:
		if !isMatcher(rest[0]) {
			break
		}
		return RouteConfig{Path: matcherPath(rest[0]), Static: &StaticConfig{Root: rest[1], SPA: spa}}, nil
	}
	return RouteConfig{}, errAt(d, "usage: file_server [/path/*] <directory> [spa]")
}

func adaptRedir(d *pfNode) (RouteConfig, error) {
	args := d.args
	code := 0
	if n := len(args); n >= 2 {
		switch last := args[n-1]; {
		case last == "permanent":
			code, args = 301, args[:n-1]
		case last == "temporary":
			code, args = 302, args[:n-1]
		default:
			if v, err := strconv.Atoi(last); err == nil {
				code, args = v, args[:n-1]
			}
		}
	}
	path, to := "/", ""
	switch len(args) {
	case 1:
		to = args[0]
	case 2:
		if !isMatcher(args[0]) {
			return RouteConfig{}, errAt(d, "usage: redir [/path/*] <target-url> [301|302|permanent|temporary]")
		}
		path, to = matcherPath(args[0]), args[1]
	default:
		return RouteConfig{}, errAt(d, "usage: redir [/path/*] <target-url> [301|302|permanent|temporary]")
	}
	keep := false
	if strings.HasSuffix(to, "{uri}") {
		keep, to = true, strings.TrimSuffix(to, "{uri}")
	}
	return RouteConfig{Path: path, Redirect: &RedirectConfig{To: to, Code: code, KeepPath: keep}}, nil
}
