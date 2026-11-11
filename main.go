// Portico: a small web server and reverse proxy.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

const version = "0.1.0"

const usage = `Portico ` + version + ` - a small web server and reverse proxy

Usage:
  portico run [-config portico.json]             run from a config file (SIGHUP reloads it)
  portico serve [-dir .] [-listen :8080] [-spa]  serve a directory
  portico proxy [-listen :8080] URL...           reverse proxy to one or more upstreams
  portico init [-config portico.json]            write an example config file
  portico validate [-config portico.json]        check a config file
  portico version
`

const sampleConfig = `{
  "listen": ":8080",
  "gzip": true,
  "metrics_listen": "127.0.0.1:9090",
  "rate_limit": { "requests_per_second": 50, "burst": 100 },

  "sites": [
    {
      "hosts": ["localhost", "example.test"],
      "routes": [
        {
          "path": "/api",
          "strip_prefix": true,
          "proxy": {
            "upstreams": ["http://localhost:3000", "http://localhost:3001"],
            "policy": "round_robin",
            "health_check": { "path": "/health", "interval": "10s", "timeout": "2s" },
            "cache": { "default_ttl": "30s", "max_mb": 64 }
          }
        },
        {
          "path": "/old",
          "redirect": { "to": "https://example.test/new", "code": 301 }
        },
        {
          "path": "/",
          "static": { "root": "./public", "spa": false }
        }
      ]
    }
  ]
}
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "serve":
		err = cmdServe(args)
	case "proxy":
		err = cmdProxy(args)
	case "init":
		err = cmdInit(args)
	case "validate":
		err = cmdValidate(args)
	case "version", "-v", "--version":
		fmt.Println("portico", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "portico:", err)
		os.Exit(1)
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := fs.String("config", "portico.json", "path to config file")
	fs.Parse(args)
	cfg, err := LoadConfig(*path)
	if err != nil {
		return err
	}
	return Run(cfg, *path)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", ".", "directory to serve")
	listen := fs.String("listen", ":8080", "address to listen on")
	spa := fs.Bool("spa", false, "serve index.html for unknown paths (single-page apps)")
	gz := fs.Bool("gzip", true, "compress text responses")
	fs.Parse(args)

	cfg := &Config{
		Listen: *listen,
		Gzip:   *gz,
		Sites: []SiteConfig{{Routes: []RouteConfig{
			{Path: "/", Static: &StaticConfig{Root: *dir, SPA: *spa}},
		}}},
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	return Run(cfg, "")
}

func cmdProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "address to listen on")
	policy := fs.String("policy", "round_robin", "round_robin or least_conn")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New("give at least one upstream URL, e.g. portico proxy http://localhost:3000")
	}

	cfg := &Config{
		Listen: *listen,
		Gzip:   true,
		Sites: []SiteConfig{{Routes: []RouteConfig{
			{Path: "/", Proxy: &ProxyConfig{
				Upstreams:   fs.Args(),
				Policy:      *policy,
				HealthCheck: &HealthCheckConfig{Path: "/"},
			}},
		}}},
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	return Run(cfg, "")
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := fs.String("config", "portico.json", "where to write the config")
	fs.Parse(args)
	if _, err := os.Stat(*path); err == nil {
		return fmt.Errorf("%s already exists", *path)
	}
	if err := os.WriteFile(*path, []byte(sampleConfig), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", *path)
	return nil
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	path := fs.String("config", "portico.json", "path to config file")
	fs.Parse(args)
	if _, err := LoadConfig(*path); err != nil {
		return err
	}
	fmt.Println("config OK")
	return nil
}

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second, // protects against slowloris
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: it would cut off streaming responses and WebSockets.
	}
}

// certStore holds the current manual TLS certificate so it can be swapped on reload.
type certStore struct {
	cert atomic.Pointer[tls.Certificate]
}

func (c *certStore) get(*tls.ClientHelloInfo) (*tls.Certificate, error) { return c.cert.Load(), nil }

// newACMEManager builds the autocert manager. The allowed domain list is read
// through a pointer so a config reload can add or remove domains.
func newACMEManager(a *AutoTLSConfig, domains *atomic.Pointer[[]string]) (*autocert.Manager, error) {
	m := &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		Email:  a.Email,
		Cache:  autocert.DirCache(a.CacheDir),
		HostPolicy: func(_ context.Context, host string) error {
			host = hostOnly(host) // the HTTP-01 handler passes r.Host, which may carry a port
			for _, d := range *domains.Load() {
				if d == host {
					return nil
				}
			}
			return fmt.Errorf("portico: %q is not one of the configured domains", host)
		},
	}
	if a.DirectoryURL != "" || a.CACertFile != "" {
		client := &acme.Client{DirectoryURL: a.DirectoryURL}
		if client.DirectoryURL == "" {
			client.DirectoryURL = acme.LetsEncryptURL
		}
		if a.CACertFile != "" {
			pem, err := os.ReadFile(a.CACertFile)
			if err != nil {
				return nil, err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("%s: no certificates found", a.CACertFile)
			}
			client.HTTPClient = &http.Client{
				Timeout:   30 * time.Second,
				Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
			}
		}
		m.Client = client
	}
	return m, nil
}

// Run starts the servers and blocks until shutdown.
func Run(cfg *Config, cfgPath string) error {
	srv := NewServer()
	if err := srv.Reload(cfg); err != nil {
		return err
	}

	var servers []*http.Server
	errCh := make(chan error, 3)

	httpSrv := newHTTPServer(cfg.Listen, srv)

	var h3srv h3Server
	var (
		certs       *certStore               // manual TLS
		autoDomains atomic.Pointer[[]string] // automatic TLS
	)

	if cfg.TLS != nil {
		tlsSrv := newHTTPServer(cfg.TLS.Listen, srv)

		var onHTTP http.Handler = srv // what plain HTTP serves (besides ACME challenges)
		if cfg.TLS.RedirectHTTP {
			onHTTP = httpsRedirect(cfg.TLS.Listen)
		}

		if a := cfg.TLS.Auto; a != nil {
			autoDomains.Store(&a.Domains)
			mgr, err := newACMEManager(a, &autoDomains)
			if err != nil {
				return err
			}
			tlsSrv.TLSConfig = mgr.TLSConfig() // GetCertificate + h2 + acme-tls/1
			tlsSrv.TLSConfig.MinVersion = tls.VersionTLS12
			httpSrv.Handler = mgr.HTTPHandler(onHTTP) // answers HTTP-01 challenges
			log.Printf("automatic HTTPS for %s (certs cached in %s)", strings.Join(a.Domains, ", "), a.CacheDir)
		} else {
			certs = &certStore{}
			cert, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
			if err != nil {
				return fmt.Errorf("loading TLS certificate: %w", err)
			}
			certs.cert.Store(&cert)
			tlsSrv.TLSConfig = &tls.Config{GetCertificate: certs.get, MinVersion: tls.VersionTLS12}
			httpSrv.Handler = onHTTP
		}

		if cfg.TLS.HTTP3 {
			h3, err := newHTTP3(cfg.TLS.Listen, srv, tlsSrv.TLSConfig.Clone())
			if err != nil {
				return err
			}
			_, port, _ := net.SplitHostPort(cfg.TLS.Listen)
			tlsSrv.Handler = altSvc(port, srv) // tell browsers HTTP/3 is available
			h3srv = h3
			go func() {
				log.Printf("http3 listening on %s (udp)", cfg.TLS.Listen)
				errCh <- h3.ListenAndServe()
			}()
		}

		servers = append(servers, tlsSrv)
		go func() {
			log.Printf("https listening on %s (HTTP/2 enabled)", cfg.TLS.Listen)
			errCh <- tlsSrv.ListenAndServeTLS("", "")
		}()
	}

	servers = append(servers, httpSrv)
	go func() {
		log.Printf("Portico %s: http listening on %s", version, cfg.Listen)
		errCh <- httpSrv.ListenAndServe()
	}()

	if cfg.MetricsListen != "" {
		ms := newHTTPServer(cfg.MetricsListen, metricsHandler(srv))
		servers = append(servers, ms)
		go func() {
			log.Printf("metrics at http://%s/metrics", cfg.MetricsListen)
			errCh <- ms.ListenAndServe()
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case err := <-errCh:
			return err

		case s := <-sig:
			if s == syscall.SIGHUP {
				if cfgPath == "" {
					log.Print("reload ignored: not started from a config file")
					continue
				}
				if err := reload(srv, cfgPath, cfg, certs, &autoDomains); err != nil {
					srv.metrics.reloadFailures.Add(1)
					log.Printf("reload failed, keeping current config: %v", err)
					continue
				}
				srv.metrics.reloads.Add(1)
				log.Print("config reloaded")
				continue
			}

			log.Printf("received %v, shutting down...", s)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, s := range servers {
				s.Shutdown(ctx)
			}
			if h3srv != nil {
				h3srv.Close()
			}
			return nil
		}
	}
}

// reload applies a new config. Everything that can fail is checked before
// anything is swapped, so a failed reload changes nothing.
func reload(srv *Server, path string, old *Config, certs *certStore, domains *atomic.Pointer[[]string]) error {
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}

	var newCert *tls.Certificate
	if certs != nil && cfg.TLS != nil && cfg.TLS.Auto == nil {
		c, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
		if err != nil {
			return fmt.Errorf("loading TLS certificate: %w", err)
		}
		newCert = &c
	}

	if err := srv.Reload(cfg); err != nil {
		return err
	}

	if newCert != nil {
		certs.cert.Store(newCert)
		log.Print("TLS certificate reloaded")
	}
	if cfg.TLS != nil && cfg.TLS.Auto != nil && domains.Load() != nil {
		domains.Store(&cfg.TLS.Auto.Domains)
	}
	if cfg.Listen != old.Listen || cfg.MetricsListen != old.MetricsListen ||
		(cfg.TLS == nil) != (old.TLS == nil) ||
		(cfg.TLS != nil && old.TLS != nil && (cfg.TLS.Listen != old.TLS.Listen || (cfg.TLS.Auto == nil) != (old.TLS.Auto == nil))) {
		log.Print("note: listen addresses and the TLS mode only change on restart")
	}
	return nil
}

// altSvc advertises HTTP/3 on the same port number so clients can upgrade.
func altSvc(port string, next http.Handler) http.Handler {
	v := fmt.Sprintf(`h3=":%s"; ma=86400`, port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", v)
		next.ServeHTTP(w, r)
	})
}

func httpsRedirect(tlsAddr string) http.Handler {
	_, port, _ := net.SplitHostPort(tlsAddr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostOnly(r.Host)
		if strings.Contains(host, ":") { // IPv6 literal
			host = "[" + host + "]"
		}
		if port != "" && port != "443" {
			host += ":" + port
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}
