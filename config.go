package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	Listen    string           `json:"listen"`
	TLS       *TLSConfig       `json:"tls,omitempty"`
	Gzip      bool             `json:"gzip"`
	RateLimit *RateLimitConfig `json:"rate_limit,omitempty"`
	Sites     []SiteConfig     `json:"sites"`
}

type TLSConfig struct {
	Listen       string `json:"listen"`
	Cert         string `json:"cert"`
	Key          string `json:"key"`
	RedirectHTTP bool   `json:"redirect_http"`
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
	if c.TLS != nil {
		if c.TLS.Listen == "" {
			c.TLS.Listen = ":8443"
		}
		if c.TLS.Cert == "" || c.TLS.Key == "" {
			return errors.New("tls: cert and key are required")
		}
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
	return nil
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
