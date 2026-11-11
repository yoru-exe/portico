package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// Duration is a time.Duration that is written as a string in JSON, e.g. "10s".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`duration must be a string such as "10s" or "1m30s"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

type Config struct {
	Listen string     `json:"listen"`
	TLS    *TLSConfig `json:"tls,omitempty"`
	Gzip   bool       `json:"gzip"`
	// MetricsListen, e.g. "127.0.0.1:9090", serves Prometheus metrics at /metrics.
	// Keep it on localhost or a private network: it is not authenticated.
	MetricsListen string           `json:"metrics_listen,omitempty"`
	RateLimit     *RateLimitConfig `json:"rate_limit,omitempty"`
	Sites         []SiteConfig     `json:"sites"`
}

type TLSConfig struct {
	Listen       string `json:"listen"`
	Cert         string `json:"cert"`
	Key          string `json:"key"`
	RedirectHTTP bool   `json:"redirect_http"`
	// HTTP3 serves HTTP/3 (QUIC) on the same port number over UDP and advertises
	// it with Alt-Svc. Requires a binary built with: go build -tags http3
	HTTP3 bool `json:"http3"`
	// Auto enables automatic certificates via ACME (Let's Encrypt).
	// Use it instead of cert/key. Needs ports 80 and 443 reachable from the internet.
	Auto *AutoTLSConfig `json:"auto,omitempty"`
}

type AutoTLSConfig struct {
	// Domains to get certificates for. Defaults to every non-wildcard,
	// non-localhost host used in "sites".
	Domains  []string `json:"domains"`
	Email    string   `json:"email"`
	CacheDir string   `json:"cache_dir"` // where certs/account key are stored (default "portico-certs")
	// DirectoryURL selects the ACME CA. Defaults to Let's Encrypt production.
	// Use https://acme-staging-v02.api.letsencrypt.org/directory while testing.
	DirectoryURL string `json:"directory_url"`
	CACertFile   string `json:"ca_cert_file"` // extra root CA to trust when talking to the CA (private/test CAs)
}

type RateLimitConfig struct {
	RequestsPerSecond float64 `json:"requests_per_second"`
	Burst             int     `json:"burst"`
}

type SiteConfig struct {
	// Hosts this site answers for. Supports exact names and "*.example.com".
	// Empty means "any host" (a catch-all site).
	Hosts  []string      `json:"hosts"`
	Routes []RouteConfig `json:"routes"`
}

type RouteConfig struct {
	Path        string          `json:"path"`
	StripPrefix bool            `json:"strip_prefix"` // proxy only
	Static      *StaticConfig   `json:"static,omitempty"`
	Proxy       *ProxyConfig    `json:"proxy,omitempty"`
	Redirect    *RedirectConfig `json:"redirect,omitempty"`
}

type StaticConfig struct {
	Root string `json:"root"`
	SPA  bool   `json:"spa"` // serve index.html for unknown extension-less paths
}

type ProxyConfig struct {
	Upstreams    []string           `json:"upstreams"`
	Policy       string             `json:"policy"` // round_robin (default) | least_conn
	PreserveHost bool               `json:"preserve_host"`
	HealthCheck  *HealthCheckConfig `json:"health_check,omitempty"`
	Cache        *CacheConfig       `json:"cache,omitempty"`
}

// CacheConfig enables an in-memory response cache for a proxy route. Only GET
// requests without Authorization/Cookie headers are cached, and only responses
// that allow it (no Set-Cookie, no private/no-store/no-cache, no Vary other than
// Accept-Encoding). Freshness comes from s-maxage/max-age, else default_ttl.
type CacheConfig struct {
	DefaultTTL Duration `json:"default_ttl"`  // 0 = only cache when upstream sends max-age
	MaxMB      int      `json:"max_mb"`       // total cache size, default 64
	MaxEntryKB int      `json:"max_entry_kb"` // largest cacheable body, default 1024
}

type HealthCheckConfig struct {
	Path     string   `json:"path"`
	Interval Duration `json:"interval"`
	Timeout  Duration `json:"timeout"`
}

type RedirectConfig struct {
	To       string `json:"to"`
	Code     int    `json:"code"`
	KeepPath bool   `json:"keep_path"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // catch typos in the config
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// Validate checks the config and fills in defaults.
func (c *Config) Validate() error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if rl := c.RateLimit; rl != nil {
		if rl.RequestsPerSecond <= 0 {
			return errors.New("rate_limit: requests_per_second must be > 0")
		}
		if rl.Burst <= 0 {
			rl.Burst = int(math.Max(1, math.Ceil(rl.RequestsPerSecond)))
		}
	}
	if len(c.Sites) == 0 {
		return errors.New("at least one site is required")
	}
	for i := range c.Sites {
		s := &c.Sites[i]
		if len(s.Routes) == 0 {
			return fmt.Errorf("site %d: at least one route is required", i+1)
		}
		for j := range s.Routes {
			if err := s.Routes[j].validate(); err != nil {
				return fmt.Errorf("site %d, route %d: %w", i+1, j+1, err)
			}
		}
	}
	if c.TLS != nil {
		if err := c.validateTLS(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateTLS() error {
	t := c.TLS
	manual := t.Cert != "" || t.Key != ""
	switch {
	case t.Auto != nil && manual:
		return errors.New("tls: use either auto or cert/key, not both")
	case t.Auto == nil && (t.Cert == "" || t.Key == ""):
		return errors.New("tls: set cert and key, or enable auto")
	}
	if t.Listen == "" {
		t.Listen = ":8443"
		if t.Auto != nil {
			t.Listen = ":443"
		}
	}
	a := t.Auto
	if a == nil {
		return nil
	}
	if len(a.Domains) == 0 {
		a.Domains = c.autoDomains()
	}
	if len(a.Domains) == 0 {
		return errors.New("tls.auto: no domains; set tls.auto.domains or use real host names in sites")
	}
	for i, d := range a.Domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if strings.HasPrefix(d, "*") {
			return fmt.Errorf("tls.auto: wildcard domain %q needs the DNS-01 challenge, which is not supported", d)
		}
		a.Domains[i] = d
	}
	if a.CacheDir == "" {
		a.CacheDir = "portico-certs"
	}
	if a.DirectoryURL != "" {
		if u, err := url.Parse(a.DirectoryURL); err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("tls.auto.directory_url %q must be an https URL", a.DirectoryURL)
		}
	}
	if a.CACertFile != "" {
		if _, err := os.Stat(a.CACertFile); err != nil {
			return fmt.Errorf("tls.auto.ca_cert_file: %w", err)
		}
	}
	return nil
}

// autoDomains lists hosts from the sites that look like public DNS names.
func (c *Config) autoDomains() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range c.Sites {
		for _, h := range s.Hosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" || strings.HasPrefix(h, "*") || h == "localhost" ||
				!strings.Contains(h, ".") || net.ParseIP(h) != nil || seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

func (r *RouteConfig) validate() error {
	if r.Path == "" {
		r.Path = "/"
	}
	if !strings.HasPrefix(r.Path, "/") {
		return fmt.Errorf("path %q must start with /", r.Path)
	}
	n := 0
	for _, set := range []bool{r.Static != nil, r.Proxy != nil, r.Redirect != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("route %q needs exactly one of static, proxy or redirect", r.Path)
	}

	switch {
	case r.Static != nil:
		if r.Static.Root == "" {
			return errors.New("static.root is required")
		}
		st, err := os.Stat(r.Static.Root)
		if err != nil {
			return fmt.Errorf("static.root: %w", err)
		}
		if !st.IsDir() {
			return fmt.Errorf("static.root %q is not a directory", r.Static.Root)
		}

	case r.Proxy != nil:
		p := r.Proxy
		if len(p.Upstreams) == 0 {
			return errors.New("proxy.upstreams must not be empty")
		}
		for _, raw := range p.Upstreams {
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("invalid upstream %q (want e.g. http://localhost:3000)", raw)
			}
		}
		switch p.Policy {
		case "", "round_robin", "least_conn":
		default:
			return fmt.Errorf("unknown proxy.policy %q (want round_robin or least_conn)", p.Policy)
		}
		if cc := p.Cache; cc != nil {
			if cc.MaxMB <= 0 {
				cc.MaxMB = 64
			}
			if cc.MaxEntryKB <= 0 {
				cc.MaxEntryKB = 1024
			}
			if cc.DefaultTTL < 0 {
				return errors.New("cache.default_ttl must not be negative")
			}
		}
		if hc := p.HealthCheck; hc != nil {
			if hc.Path == "" {
				hc.Path = "/"
			}
			if hc.Interval <= 0 {
				hc.Interval = Duration(10 * time.Second)
			}
			if hc.Timeout <= 0 {
				hc.Timeout = Duration(3 * time.Second)
			}
		}

	case r.Redirect != nil:
		rd := r.Redirect
		if rd.To == "" {
			return errors.New("redirect.to is required")
		}
		if rd.Code == 0 {
			rd.Code = 302
		}
		switch rd.Code {
		case 301, 302, 303, 307, 308:
		default:
			return fmt.Errorf("redirect.code %d is not a redirect status", rd.Code)
		}
	}
	return nil
}
