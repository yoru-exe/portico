package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMatchPrefix(t *testing.T) {
	cases := []struct {
		path, prefix string
		want         bool
	}{
		{"/anything", "/", true},
		{"/api", "/api", true},
		{"/api/", "/api", true},
		{"/api/users", "/api", true},
		{"/api/users", "/api/", true},
		{"/apiary", "/api", false},
		{"/ap", "/api", false},
	}
	for _, c := range cases {
		if got := matchPrefix(c.path, c.prefix); got != c.want {
			t.Errorf("matchPrefix(%q, %q) = %v, want %v", c.path, c.prefix, got, c.want)
		}
	}
}

func TestHostOnly(t *testing.T) {
	for in, want := range map[string]string{
		"Example.com": "example.com", "example.com:8080": "example.com",
		"[::1]:443": "::1", "[::1]": "::1", "127.0.0.1:80": "127.0.0.1",
	} {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCacheable(t *testing.T) {
	c := &responseCache{defaultTTL: 0}
	h := func(kv ...string) http.Header {
		hd := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			hd.Add(kv[i], kv[i+1])
		}
		return hd
	}
	cases := []struct {
		name   string
		status int
		hdr    http.Header
		ttl    time.Duration
		ok     bool
	}{
		{"max-age", 200, h("Cache-Control", "max-age=60"), 60 * time.Second, true},
		{"s-maxage wins", 200, h("Cache-Control", "max-age=60, s-maxage=5"), 5 * time.Second, true},
		{"no cache-control, no default", 200, h(), 0, false},
		{"no-store", 200, h("Cache-Control", "no-store, max-age=60"), 0, false},
		{"private", 200, h("Cache-Control", "private, max-age=60"), 0, false},
		{"no-cache", 200, h("Cache-Control", "no-cache, max-age=60"), 0, false},
		{"set-cookie", 200, h("Cache-Control", "max-age=60", "Set-Cookie", "a=b"), 0, false},
		{"vary other", 200, h("Cache-Control", "max-age=60", "Vary", "Accept-Encoding, Cookie"), 0, false},
		{"vary accept-encoding ok", 200, h("Cache-Control", "max-age=60", "Vary", "Accept-Encoding"), 60 * time.Second, true},
		{"500 never", 500, h("Cache-Control", "max-age=60"), 0, false},
		{"206 never", 206, h("Cache-Control", "max-age=60"), 0, false},
		{"404 ok", 404, h("Cache-Control", "max-age=10"), 10 * time.Second, true},
		{"bad max-age", 200, h("Cache-Control", "max-age=abc"), 0, false},
	}
	for _, tc := range cases {
		ttl, ok := c.cacheable(tc.status, tc.hdr)
		if ok != tc.ok || (ok && ttl != tc.ttl) {
			t.Errorf("%s: got (%v,%v), want (%v,%v)", tc.name, ttl, ok, tc.ttl, tc.ok)
		}
	}
}

func TestCacheLRUEviction(t *testing.T) {
	c := newCache(&CacheConfig{MaxMB: 1, MaxEntryKB: 600}, &Metrics{})
	mk := func(k string, n int) *cacheEntry {
		return &cacheEntry{key: k, body: make([]byte, n), expires: time.Now().Add(time.Minute)}
	}
	c.put(mk("a", 400<<10))
	c.put(mk("b", 400<<10))
	c.get("a") // a is now more recent than b
	c.put(mk("c", 400<<10))
	if c.get("b") != nil {
		t.Error("b should have been evicted (least recently used)")
	}
	if c.get("a") == nil || c.get("c") == nil {
		t.Error("a and c should still be cached")
	}
	if c.bytes > c.maxBytes {
		t.Errorf("cache over budget: %d > %d", c.bytes, c.maxBytes)
	}
}

func TestEtagMatches(t *testing.T) {
	if !etagMatches(`"v1"`, `"v1"`) || !etagMatches(`W/"v1", "x"`, `"v1"`) || !etagMatches("*", `"v1"`) {
		t.Error("expected match")
	}
	if etagMatches(`"v2"`, `"v1"`) || etagMatches("", `"v1"`) {
		t.Error("expected no match")
	}
}

func TestLimiter(t *testing.T) {
	l := &limiter{rate: 10, burst: 3, buckets: map[string]*bucket{}}
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("1.1.1.1"); !ok {
			t.Fatalf("request %d within burst was denied", i+1)
		}
	}
	if ok, wait := l.allow("1.1.1.1"); ok || wait <= 0 {
		t.Errorf("4th request should be denied with a wait, got ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.allow("2.2.2.2"); !ok {
		t.Error("a different IP has its own bucket")
	}
}

func TestConfigValidation(t *testing.T) {
	dir := t.TempDir()
	static := RouteConfig{Static: &StaticConfig{Root: dir}}
	cases := map[string]struct {
		cfg     Config
		wantErr string
	}{
		"ok":       {Config{Sites: []SiteConfig{{Routes: []RouteConfig{static}}}}, ""},
		"no sites": {Config{}, "at least one site"},
		"two handlers": {Config{Sites: []SiteConfig{{Routes: []RouteConfig{
			{Static: &StaticConfig{Root: dir}, Redirect: &RedirectConfig{To: "/x"}}}}}}, "exactly one"},
		"bad upstream": {Config{Sites: []SiteConfig{{Routes: []RouteConfig{
			{Proxy: &ProxyConfig{Upstreams: []string{"localhost:3000"}}}}}}}, "invalid upstream"},
		"tls both": {Config{TLS: &TLSConfig{Cert: "a", Key: "b", Auto: &AutoTLSConfig{}},
			Sites: []SiteConfig{{Routes: []RouteConfig{static}}}}, "either auto or cert/key"},
		"tls neither": {Config{TLS: &TLSConfig{}, Sites: []SiteConfig{{Routes: []RouteConfig{static}}}}, "cert and key"},
		"auto wildcard": {Config{TLS: &TLSConfig{Auto: &AutoTLSConfig{Domains: []string{"*.example.com"}}},
			Sites: []SiteConfig{{Routes: []RouteConfig{static}}}}, "DNS-01"},
		"auto no public hosts": {Config{TLS: &TLSConfig{Auto: &AutoTLSConfig{}},
			Sites: []SiteConfig{{Hosts: []string{"localhost", "127.0.0.1", "*.dev.test", "intranet"}, Routes: []RouteConfig{static}}}}, "no domains"},
	}
	for name, tc := range cases {
		err := tc.cfg.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error: %v", name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error %v, want it to contain %q", name, err, tc.wantErr)
		}
	}
}

func TestAutoDomainsDerivedFromHosts(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		TLS: &TLSConfig{Auto: &AutoTLSConfig{}},
		Sites: []SiteConfig{{
			Hosts:  []string{"Example.com", "www.example.com", "localhost", "10.0.0.1", "*.wild.example.com", "example.com"},
			Routes: []RouteConfig{{Static: &StaticConfig{Root: dir}}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cfg.TLS.Auto.Domains, ",")
	if got != "example.com,www.example.com" {
		t.Errorf("domains = %q", got)
	}
	if cfg.TLS.Listen != ":443" || cfg.TLS.Auto.CacheDir != "portico-certs" {
		t.Errorf("defaults not applied: %+v", cfg.TLS)
	}
}

func TestSafeFSHidesDotfiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("S=1"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".well-known"), 0o755)
	os.WriteFile(filepath.Join(dir, ".well-known", "x.txt"), []byte("ok"), 0o644)
	os.MkdirAll(filepath.Join(dir, "empty"), 0o755)
	fs := safeFS{http.Dir(dir)}
	if _, err := fs.Open("/.env"); err == nil {
		t.Error(".env must not be served")
	}
	if _, err := fs.Open("/.well-known/x.txt"); err != nil {
		t.Errorf(".well-known should be served: %v", err)
	}
	if _, err := fs.Open("/empty"); err == nil {
		t.Error("directory without index.html must not be listable")
	}
}
