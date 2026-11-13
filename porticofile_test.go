package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPorticofileTokenizer(t *testing.T) {
	lines, err := pfTokenize("\ufeff# comment\r\nfoo  bar\t\"two words\" # trailing\r\n\r\n  baz \"a \\\"q\\\" b\"\n")
	if err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, l := range lines {
		var row []string
		for _, tk := range l.toks {
			row = append(row, tk.s)
		}
		got = append(got, row)
	}
	want := [][]string{{"foo", "bar", "two words"}, {"baz", `a "q" b`}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokens = %q, want %q", got, want)
	}
	if lines[1].no != 4 {
		t.Errorf("line number = %d, want 4 (comments and blank lines count)", lines[1].no)
	}
	if _, err := pfTokenize(`a "unterminated`); err == nil {
		t.Error("unterminated quote must be an error")
	}
	// '#' only starts a comment at the beginning of a token (URLs may contain it).
	lines, _ = pfTokenize("redir /a https://x/y#frag")
	if len(lines[0].toks) != 3 || lines[0].toks[2].s != "https://x/y#frag" {
		t.Errorf("fragment lost: %+v", lines[0].toks)
	}
}

func TestPorticofileErrors(t *testing.T) {
	dir := filepath.ToSlash(t.TempDir())
	site := func(body string) string { return "example.com {\n" + body + "\n}\n" }
	cases := []struct{ name, src, want string }{
		{"unclosed block", "example.com {\n file_server " + dir + "\n", "Porticofile:1: missing closing }"},
		{"stray brace", "}\n", "Porticofile:1: unexpected }"},
		{"brace mid-line", "example.com { file_server x }\n", "braces must stand alone"},
		{"site without block", "example.com\n", "Porticofile:1:"},
		{"unknown directive", site("\tfoo bar"), "Porticofile:2: unknown directive \"foo\""},
		{"global option in site", site("\tencode gzip"), "Porticofile:2: encode is a global option"},
		{"empty site", "example.com {\n}\n", "has no directives"},
		{"proxy no upstream", site("\treverse_proxy /api/*"), "usage: reverse_proxy"},
		{"proxy bad option", site("\treverse_proxy localhost:3000 {\n\t\tbogus\n\t}"), "Porticofile:3: unknown reverse_proxy option"},
		{"bad duration", site("\treverse_proxy localhost:3000 {\n\t\thealth_check /h soon\n\t}"), "\"soon\" is not a duration"},
		{"file_server usage", site("\tfile_server"), "usage: file_server"},
		{"file_server matcher", site("\tfile_server a b"), "usage: file_server"},
		{"redir usage", site("\tredir"), "usage: redir"},
		{"two globals", "{\n}\n{\n}\n", "only one global"},
		{"unknown global", "{\n\tfrobnicate\n}\n", "unknown global option \"frobnicate\""},
		{"encode brotli", "{\n\tencode br\n}\n", "only `encode gzip`"},
		{"http3 without tls", "{\n\thttp3\n}\n" + site("\tfile_server "+dir), "http3 needs a `tls` directive"},
		{"redirect_http without tls", "{\n\tredirect_http\n}\n" + site("\tfile_server "+dir), "redirect_http needs a `tls` directive"},
		{"tls usage", "{\n\ttls onlyone\n}\n", "usage: `tls auto"},
		{"rate_limit bad", "{\n\trate_limit fast\n}\n", "not a positive number"},
		{"site path", "example.com/api {\n file_server x\n}\n", "must be a host name"},
	}
	for _, c := range cases {
		_, err := parsePorticofile(c.src, "Porticofile")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want it to contain %q", c.name, err, c.want)
		}
	}
}

// The Porticofile and its hand-written JSON equivalent must produce the same Config.
func TestPorticofileEqualsJSON(t *testing.T) {
	dir := filepath.ToSlash(t.TempDir())
	pf := `
# comment
{
	listen :9090
	encode gzip
	metrics 127.0.0.1:9100
	rate_limit 25/s 60
}

example.com, www.example.com {
	reverse_proxy /api/* localhost:3000 http://10.0.0.5:8000 {
		strip_prefix
		preserve_host
		policy least_conn
		health_check /health 5s 1s
		cache 30s {
			max_mb 16
			max_entry_kb 256
		}
		response_header_timeout 20s
	}
	redir /old https://example.com/new{uri} permanent
	redir /gone https://example.com/ 302
	file_server /static/* ` + dir + ` spa
	file_server ` + dir + `
}

:8081 {
	reverse_proxy :3000
}
`
	js := `{
	"listen": ":9090", "gzip": true, "metrics_listen": "127.0.0.1:9100",
	"rate_limit": {"requests_per_second": 25, "burst": 60},
	"sites": [
	 {"hosts": ["example.com","www.example.com"], "routes": [
	   {"path": "/api", "strip_prefix": true, "proxy": {
	      "upstreams": ["http://localhost:3000","http://10.0.0.5:8000"], "policy": "least_conn", "preserve_host": true,
	      "health_check": {"path": "/health", "interval": "5s", "timeout": "1s"},
	      "cache": {"default_ttl": "30s", "max_mb": 16, "max_entry_kb": 256},
	      "response_header_timeout": "20s"}},
	   {"path": "/old", "redirect": {"to": "https://example.com/new", "code": 301, "keep_path": true}},
	   {"path": "/gone", "redirect": {"to": "https://example.com/", "code": 302}},
	   {"path": "/static", "static": {"root": "` + dir + `", "spa": true}},
	   {"path": "/", "static": {"root": "` + dir + `"}}]},
	 {"hosts": ["*"], "routes": [{"path": "/", "proxy": {"upstreams": ["http://localhost:3000"]}}]}
	]}`
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "Porticofile"), []byte(pf), 0o644)
	os.WriteFile(filepath.Join(tmp, "portico.json"), []byte(js), 0o644)

	a, err := LoadConfig(filepath.Join(tmp, "Porticofile"))
	if err != nil {
		t.Fatalf("Porticofile: %v", err)
	}
	b, err := LoadConfig(filepath.Join(tmp, "portico.json"))
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("configs differ\nPorticofile: %+v\nJSON:        %+v", a, b)
		for i := range a.Sites {
			if i < len(b.Sites) && !reflect.DeepEqual(a.Sites[i], b.Sites[i]) {
				t.Errorf("site %d differs:\n %+v\n %+v", i, a.Sites[i], b.Sites[i])
				for j := range a.Sites[i].Routes {
					if !reflect.DeepEqual(a.Sites[i].Routes[j], b.Sites[i].Routes[j]) {
						t.Errorf(" route %d differs:\n  %+v\n  %+v", j, a.Sites[i].Routes[j], b.Sites[i].Routes[j])
					}
				}
			}
		}
	}
}

func TestPorticofileTLS(t *testing.T) {
	dir := filepath.ToSlash(t.TempDir())
	body := "\nexample.com {\n\tfile_server " + dir + "\n}\n"

	cfg, err := parsePorticofile("{\n\tlisten :80\n\ttls auto me@example.com {\n\t\tdomains a.com b.com\n\t\tca https://acme.test/dir\n\t\tca_cert /tmp/ca.pem\n\t\tstorage /var/certs\n\t}\n\thttps_listen :443\n\tredirect_http\n\thttp3\n}\n"+body, "Porticofile")
	if err != nil {
		t.Fatal(err)
	}
	want := &TLSConfig{Listen: ":443", RedirectHTTP: true, HTTP3: true, Auto: &AutoTLSConfig{
		Email: "me@example.com", Domains: []string{"a.com", "b.com"},
		DirectoryURL: "https://acme.test/dir", CACertFile: "/tmp/ca.pem", CacheDir: "/var/certs"}}
	if !reflect.DeepEqual(cfg.TLS, want) {
		t.Errorf("tls = %+v\nwant %+v", cfg.TLS, want)
	}

	cfg, err = parsePorticofile("{\n\ttls /etc/c.pem /etc/k.pem\n}\n"+body, "Porticofile")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLS.Cert != "/etc/c.pem" || cfg.TLS.Key != "/etc/k.pem" || cfg.TLS.Auto != nil {
		t.Errorf("manual tls = %+v", cfg.TLS)
	}

	// `tls auto` with no email: domains are derived from the site addresses.
	cfg, err = parsePorticofile("{\n\ttls auto\n}\n"+body, "Porticofile")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.TLS.Auto.Domains; len(got) != 1 || got[0] != "example.com" {
		t.Errorf("derived domains = %v", got)
	}
}

func TestPorticofileAddressesAndMatchers(t *testing.T) {
	n := &pfNode{}
	for in, want := range map[string]string{
		"Example.com": "example.com", "https://Example.com:8443": "example.com", "http://a.test": "a.test",
		":8080": "*", "*": "*", "[::1]:80": "::1", "localhost:3000": "localhost",
	} {
		got, err := parseAddr(n, in)
		if err != nil || got != want {
			t.Errorf("parseAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := parseAddr(n, "a.com/x"); err == nil {
		t.Error("an address with a path must be rejected")
	}
	for in, want := range map[string]string{"*": "/", "/api/*": "/api", "/api": "/api", "/api/": "/api", "/": "/", "/*": "/"} {
		if got := matcherPath(in); got != want {
			t.Errorf("matcherPath(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"localhost:3000": "http://localhost:3000", ":3000": "http://localhost:3000",
		"http://x:1": "http://x:1", "https://y": "https://y",
	} {
		if got := normalizeUpstream(in); got != want {
			t.Errorf("normalizeUpstream(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPorticofileRedirForms(t *testing.T) {
	check := func(args []string, path, to string, code int, keep bool) {
		t.Helper()
		rc, err := adaptRedir(&pfNode{name: "redir", args: args})
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		r := rc.Redirect
		if rc.Path != path || r.To != to || r.Code != code || r.KeepPath != keep {
			t.Errorf("%v -> path=%q to=%q code=%d keep=%v; want %q %q %d %v", args, rc.Path, r.To, r.Code, r.KeepPath, path, to, code, keep)
		}
	}
	check([]string{"/new"}, "/", "/new", 0, false)
	check([]string{"/old", "/new"}, "/old", "/new", 0, false)
	check([]string{"https://x.test", "301"}, "/", "https://x.test", 301, false)
	check([]string{"/old", "https://x.test/", "permanent"}, "/old", "https://x.test/", 301, false)
	check([]string{"/old", "https://x.test/", "temporary"}, "/old", "https://x.test/", 302, false)
	check([]string{"/", "https://x.test{uri}", "308"}, "/", "https://x.test", 308, true)
}

func TestLoadConfigPicksFormatByExtension(t *testing.T) {
	dir := t.TempDir()
	root := filepath.ToSlash(dir)
	os.WriteFile(filepath.Join(dir, "Porticofile"), []byte("localhost {\n\tfile_server "+root+"\n}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "site.conf"), []byte("localhost {\n\tfile_server "+root+"\n}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "c.json"), []byte(`{"sites":[{"routes":[{"static":{"root":"`+root+`"}}]}]}`), 0o644)
	os.WriteFile(filepath.Join(dir, "weird.json"), []byte("localhost {\n}\n"), 0o644) // Porticofile syntax in a .json file
	for _, name := range []string{"Porticofile", "site.conf", "c.json"} {
		if _, err := LoadConfig(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := LoadConfig(filepath.Join(dir, "weird.json")); err == nil {
		t.Error("a .json file is parsed as JSON, so Porticofile syntax in it must fail")
	}
	// Errors name the file and line.
	os.WriteFile(filepath.Join(dir, "bad"), []byte("localhost {\n\tbogus\n}\n"), 0o644)
	if _, err := LoadConfig(filepath.Join(dir, "bad")); err == nil || !strings.Contains(err.Error(), "bad:2:") {
		t.Errorf("error should include file:line, got %v", err)
	}
}

func TestResolveConfigPath(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(dir)
	if got := resolveConfigPath(""); got != "portico.json" {
		t.Errorf("no files: %q", got)
	}
	os.WriteFile("Porticofile", []byte("x"), 0o644)
	if got := resolveConfigPath(""); got != "Porticofile" {
		t.Errorf("with Porticofile: %q", got)
	}
	if got := resolveConfigPath("other.json"); got != "other.json" {
		t.Errorf("explicit: %q", got)
	}
}

// End to end: a Porticofile drives a real server.
func TestPorticofileServesTraffic(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		fmt.Fprint(w, "upstream saw "+r.URL.Path)
	}))
	defer up.Close()
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, "index.html"), []byte("<h1>hi</h1>"), 0o644)

	src := fmt.Sprintf(`
{
	encode gzip
}
:0 {
	reverse_proxy /api/* %s {
		strip_prefix
		cache
	}
	redir /old https://example.test{uri} 301
	file_server %s
}
`, strings.TrimPrefix(up.URL, "http://"), filepath.ToSlash(site))
	f := filepath.Join(t.TempDir(), "Porticofile")
	os.WriteFile(f, []byte(src), 0o644)
	cfg, err := LoadConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	p := startProxy(t, cfg)

	get := func(path string) (*http.Response, string) {
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Get(p.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if _, b := get("/"); b != "<h1>hi</h1>" {
		t.Errorf("static: %q", b)
	}
	if _, b := get("/api/users"); b != "upstream saw /users" {
		t.Errorf("proxy+strip_prefix: %q", b)
	}
	if r, _ := get("/api/users"); r.Header.Get("X-Cache") != "HIT" {
		t.Errorf("second request should be a cache HIT, X-Cache=%q", r.Header.Get("X-Cache"))
	}
	if r, _ := get("/old?x=1"); r.StatusCode != 301 || r.Header.Get("Location") != "https://example.test/old?x=1" { // {uri} = the full original request URI
		t.Errorf("redir with {uri}: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
}
