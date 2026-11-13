package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard) // keep access-log lines out of test output
	os.Exit(m.Run())
}

// Regression tests for bugs found by exercising Portico against real servers.

func TestHostOnlyTrailingDot(t *testing.T) {
	if got := hostOnly("Example.COM."); got != "example.com" {
		t.Errorf("hostOnly = %q", got)
	}
	if got := hostOnly("example.com.:8080"); got != "example.com" {
		t.Errorf("hostOnly with port = %q", got)
	}
}

func TestAcceptsGzip(t *testing.T) {
	for in, want := range map[string]bool{
		"gzip":              true,
		"deflate, gzip, br": true,
		"GZIP":              true,
		"gzip;q=0":          false,
		"gzip; q=0.0, br":   false,
		"gzip;q=0.5":        true,
		"br, deflate":       false,
		"":                  false,
		"identity":          false,
		"gzip;q=notanumber": true,
		"deflate;q=0, gzip": true,
		"notgzip":           false,
	} {
		if got := acceptsGzip(in); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCleanURLPath(t *testing.T) {
	for in, want := range map[string]string{
		"/api/../admin":  "/admin",
		"//a//b":         "/a/b",
		"/a/./b":         "/a/b",
		"/a/b/..":        "/a",
		"/a/b/../":       "/a/",
		"/a/b/":          "/a/b/",
		"/":              "/",
		"/../../etc":     "/etc",
		"/.well-known/x": "/.well-known/x",
	} {
		if got := cleanURLPath(in); got != want {
			t.Errorf("cleanURLPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanPathMiddleware(t *testing.T) {
	var seen string
	h := cleanPathMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.URL.Path }))
	req := httptest.NewRequest("GET", "http://x/api/../admin//panel", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "/admin/panel" {
		t.Errorf("handler saw %q, want /admin/panel", seen)
	}
	if req.URL.Path != "/api/../admin//panel" {
		t.Error("the caller's request must not be mutated")
	}
}

func TestStripURLPrefixKeepsEncoding(t *testing.T) {
	cases := []struct{ in, prefix, path, escaped string }{
		{"/api/a%2Fb", "/api", "/a/b", "/a%2Fb"},
		{"/api", "/api", "/", "/"},
		{"/api/users?x=1", "/api", "/users", "/users"},
		{"/%61pi/x", "/api", "/x", "/x"}, // prefix spelled with an escape
		{"/api/sp%20ace", "/api", "/sp ace", "/sp%20ace"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.in)
		if err != nil {
			t.Fatal(err)
		}
		stripURLPrefix(u, c.prefix)
		if u.Path != c.path || u.EscapedPath() != c.escaped {
			t.Errorf("%s: path=%q escaped=%q, want %q / %q", c.in, u.Path, u.EscapedPath(), c.path, c.escaped)
		}
	}
}

func TestEventStreamsAreNeverCached(t *testing.T) {
	c := &responseCache{defaultTTL: time.Minute}
	h := http.Header{"Content-Type": {"text/event-stream"}}
	if _, ok := c.cacheable(200, h); ok {
		t.Error("text/event-stream must not be cacheable")
	}
}

func TestLoadConfigStrict(t *testing.T) {
	dir := t.TempDir()
	good := `{"sites":[{"routes":[{"path":"/","static":{"root":"` + filepath.ToSlash(dir) + `"}}]}]}`
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	if _, err := LoadConfig(write("ok.json", good)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if _, err := LoadConfig(write("ok2.json", good+"\n\n")); err != nil {
		t.Errorf("trailing whitespace must be fine: %v", err)
	}
	if _, err := LoadConfig(write("trail.json", good+`{"listen":":9999"}`)); err == nil || !strings.Contains(err.Error(), "unexpected data") {
		t.Errorf("trailing JSON object must be rejected, got %v", err)
	}
	if _, err := LoadConfig(write("empty.json", "")); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty file: want a clear error, got %v", err)
	}
}

// ---- proxy behaviour against real HTTP servers ---------------------------

func startProxy(t *testing.T, cfg *Config) *httptest.Server {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	rt, err := Build(cfg, &Metrics{start: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.stop)
	srv := httptest.NewServer(rt.Handler)
	t.Cleanup(srv.Close)
	return srv
}

func proxyCfg(strip bool, upstreams ...string) *Config {
	return &Config{Sites: []SiteConfig{{Routes: []RouteConfig{{
		Path: "/api", StripPrefix: strip, Proxy: &ProxyConfig{Upstreams: upstreams},
	}}}}}
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestProxyServerHeaderAndEncodedPath(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "secret-upstream/1.0")
		fmt.Fprint(w, r.URL.EscapedPath())
	}))
	defer up.Close()
	p := startProxy(t, proxyCfg(true, up.URL))

	resp, body := get(t, p.URL+"/api/a%2Fb")
	if got := resp.Header.Values("Server"); len(got) != 1 || got[0] != "Portico" {
		t.Errorf("Server headers = %v, want exactly [Portico]", got)
	}
	if body != "/a%2Fb" {
		t.Errorf("upstream saw path %q, want /a%%2Fb", body)
	}
}

func TestProxyRoutesAfterPathNormalisation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "UPSTREAM "+r.URL.Path) }))
	defer up.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "admin"), []byte("STATIC admin"), 0o644)
	cfg := proxyCfg(true, up.URL)
	cfg.Sites[0].Routes = append(cfg.Sites[0].Routes, RouteConfig{Path: "/", Static: &StaticConfig{Root: dir}})
	p := startProxy(t, cfg)

	// A raw request line with dot segments (net/http's client would clean it).
	conn, err := net.Dial("tcp", strings.TrimPrefix(p.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET /api/../admin HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	b, _ := io.ReadAll(conn)
	if !strings.Contains(string(b), "STATIC admin") || strings.Contains(string(b), "UPSTREAM") {
		t.Errorf("/api/../admin must be routed as /admin to the static route, got:\n%s", b)
	}
}

func TestProxyFailsOverWhenAnUpstreamIsDown(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "live") }))
	defer live.Close()
	// An address nothing listens on.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + l.Addr().String()
	l.Close()

	p := startProxy(t, proxyCfg(false, dead, live.URL, dead))
	for i := 0; i < 30; i++ {
		resp, body := get(t, p.URL+"/api/x")
		if resp.StatusCode != 200 || body != "live" {
			t.Fatalf("request %d: got %d %q; a dead upstream must not cause client-visible errors", i, resp.StatusCode, body)
		}
	}
}

func TestProxyAllUpstreamsDown(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + l.Addr().String()
	l.Close()
	p := startProxy(t, proxyCfg(false, dead))
	if resp, _ := get(t, p.URL+"/api/x"); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestProxyReusesUpstreamConnections(t *testing.T) {
	var conns atomic.Int64
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	up.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	up.Start()
	defer up.Close()
	p := startProxy(t, proxyCfg(false, up.URL))

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 100}}
	var wg sync.WaitGroup
	for w := 0; w < 50; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				resp, err := client.Get(p.URL + "/api/x")
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}()
	}
	wg.Wait()
	// 1000 requests from 50 concurrent clients. With the old 2-idle-connection
	// default this opened hundreds of connections; pooled it stays near 50.
	if n := conns.Load(); n > 120 {
		t.Errorf("proxy opened %d upstream connections for 1000 requests; pooling is broken", n)
	}
}

func TestResponseHeaderTimeoutDefaults(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0: defaultResponseHeaderTimeout, -time.Second: 0, 5 * time.Second: 5 * time.Second,
	} {
		p := &ProxyConfig{ResponseHeaderTimeout: Duration(in)}
		if got := p.responseHeaderTimeout(); got != want {
			t.Errorf("responseHeaderTimeout(%v) = %v, want %v", in, got, want)
		}
	}
}
