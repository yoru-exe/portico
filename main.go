// Portico: a small web server and reverse proxy.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
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
            "health_check": { "path": "/health", "interval": "10s", "timeout": "2s" }
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

// Run starts the servers and blocks until shutdown.
func Run(cfg *Config, cfgPath string) error {
	srv := &Server{}
	if err := srv.Reload(cfg); err != nil {
		return err
	}

	var servers []*http.Server
	errCh := make(chan error, 2)

	httpSrv := newHTTPServer(cfg.Listen, srv)

	if cfg.TLS != nil {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
		if err != nil {
			return fmt.Errorf("loading TLS certificate: %w", err)
		}
		tlsSrv := newHTTPServer(cfg.TLS.Listen, srv)
		tlsSrv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		servers = append(servers, tlsSrv)
		go func() {
			log.Printf("https listening on %s (HTTP/2 enabled)", cfg.TLS.Listen)
			errCh <- tlsSrv.ListenAndServeTLS("", "")
		}()
		if cfg.TLS.RedirectHTTP {
			httpSrv.Handler = httpsRedirect(cfg.TLS.Listen)
		}
	}

	servers = append(servers, httpSrv)
	go func() {
		log.Printf("Portico %s: http listening on %s", version, cfg.Listen)
		errCh <- httpSrv.ListenAndServe()
	}()

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
				newCfg, err := LoadConfig(cfgPath)
				if err != nil {
					log.Printf("reload failed, keeping current config: %v", err)
					continue
				}
				if err := srv.Reload(newCfg); err != nil {
					log.Printf("reload failed, keeping current config: %v", err)
					continue
				}
				if newCfg.Listen != cfg.Listen || (newCfg.TLS == nil) != (cfg.TLS == nil) {
					log.Print("note: listen address and TLS settings only change on restart")
				}
				log.Print("config reloaded")
				continue
			}

			log.Printf("received %v, shutting down...", s)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, s := range servers {
				s.Shutdown(ctx)
			}
			return nil
		}
	}
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
