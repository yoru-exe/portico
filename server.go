package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Hot-reloadable server
// ---------------------------------------------------------------------------

// Runtime is one fully built configuration. A new one is built on every
// reload and swapped in atomically; the old one is stopped afterwards.
type Runtime struct {
	Handler   http.Handler
	balancers []*balancer        // exposed for metrics
	stop      context.CancelFunc // stops background work (health checks, cleanup)
}

// Server is the http.Handler given to net/http. It forwards to whichever
// Runtime is current, which is what makes zero-downtime reloads possible.
type Server struct {
	rt      atomic.Pointer[Runtime]
	metrics *Metrics
}

func NewServer() *Server { return &Server{metrics: &Metrics{start: time.Now()}} }

// buildCtx carries shared state while a config is turned into handlers.
type buildCtx struct {
	ctx       context.Context
	m         *Metrics
	balancers []*balancer
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.rt.Load().Handler.ServeHTTP(w, r)
}

func (s *Server) Reload(cfg *Config) error {
	rt, err := Build(cfg, s.metrics)
	if err != nil {
		return err
	}
	if old := s.rt.Swap(rt); old != nil {
		old.stop()
	}
	return nil
}

// Build turns a validated Config into a handler chain.
func Build(cfg *Config, m *Metrics) (*Runtime, error) {
	ctx, cancel := context.WithCancel(context.Background())
	bc := &buildCtx{ctx: ctx, m: m}
	router := &Router{sites: map[string]*Site{}}

	for _, sc := range cfg.Sites {
		site := &Site{}
		for _, rc := range sc.Routes {
			h, err := buildRoute(bc, rc)
			if err != nil {
				cancel()
				return nil, err
			}
			site.routes = append(site.routes, route{prefix: rc.Path, handler: h})
		}
		// Longest prefix wins, like nginx location blocks.
		sort.SliceStable(site.routes, func(i, j int) bool {
			return len(site.routes[i].prefix) > len(site.routes[j].prefix)
		})

		hosts := sc.Hosts
		if len(hosts) == 0 {
			hosts = []string{"*"}
		}
		for _, h := range hosts {
			h = strings.ToLower(h)
			if _, dup := router.sites[h]; dup {
				cancel()
				return nil, fmt.Errorf("host %q is configured more than once", h)
			}
			router.sites[h] = site
		}
	}

	var h http.Handler = router
	if cfg.Gzip {
		h = gzipMiddleware(h)
	}
	if cfg.RateLimit != nil {
		h = rateLimitMiddleware(ctx, cfg.RateLimit, h)
	}
	h = loggingMiddleware(m, h)
	h = recoverMiddleware(h)
	return &Runtime{Handler: h, balancers: bc.balancers, stop: cancel}, nil
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

type route struct {
	prefix  string
	handler http.Handler
}

type Site struct{ routes []route }

type Router struct{ sites map[string]*Site }

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	site := rt.lookup(hostOnly(r.Host))
	if site == nil {
		http.Error(w, "no site configured for this host", http.StatusNotFound)
		return
	}
	for _, ro := range site.routes {
		if matchPrefix(r.URL.Path, ro.prefix) {
			ro.handler.ServeHTTP(w, r)
			return
		}
	}
	http.NotFound(w, r)
}

// lookup finds the site for a host: exact match, then "*.parent", then "*".
func (rt *Router) lookup(host string) *Site {
	if s := rt.sites[host]; s != nil {
		return s
	}
	if i := strings.IndexByte(host, '.'); i >= 0 {
		if s := rt.sites["*"+host[i:]]; s != nil {
			return s
		}
	}
	return rt.sites["*"]
}

// matchPrefix matches on path-segment boundaries: "/api" matches "/api" and
// "/api/users" but not "/apiary".
func matchPrefix(p, prefix string) bool {
	trimmed := strings.TrimSuffix(prefix, "/")
	if trimmed == "" {
		return true
	}
	return p == trimmed || strings.HasPrefix(p, trimmed+"/")
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return strings.ToLower(h)
}

func buildRoute(bc *buildCtx, rc RouteConfig) (http.Handler, error) {
	switch {
	case rc.Static != nil:
		return staticHandler(rc), nil
	case rc.Proxy != nil:
		return proxyHandler(bc, rc)
	case rc.Redirect != nil:
		return redirectHandler(rc.Redirect), nil
	}
	return nil, errors.New("route has no handler")
}

// ---------------------------------------------------------------------------
// Redirects
// ---------------------------------------------------------------------------

func redirectHandler(rd *RedirectConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		to := rd.To
		if rd.KeepPath {
			to = strings.TrimSuffix(to, "/") + r.URL.RequestURI()
		}
		http.Redirect(w, r, to, rd.Code)
	})
}

// ---------------------------------------------------------------------------
// Static files
// ---------------------------------------------------------------------------

// safeFS wraps http.Dir to (1) never list directories and (2) hide dotfiles
// such as .env and .git, except .well-known.
type safeFS struct{ fs http.FileSystem }

func (s safeFS) Open(name string) (http.File, error) {
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") && seg != ".well-known" {
			return nil, fs.ErrNotExist
		}
	}
	f, err := s.fs.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		idx, err := s.fs.Open(path.Join(name, "index.html"))
		if err != nil {
			f.Close()
			return nil, fs.ErrNotExist
		}
		idx.Close()
	}
	return f, nil
}

func staticHandler(rc RouteConfig) http.Handler {
	fsys := safeFS{http.Dir(rc.Static.Root)}
	fileServer := http.FileServer(fsys)

	var h http.Handler = fileServer
	if rc.Static.SPA {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := path.Clean("/" + r.URL.Path)
			if path.Ext(p) == "" {
				if f, err := fsys.Open(p); err != nil {
					r2 := r.Clone(r.Context())
					r2.URL.Path = "/"
					r = r2
				} else {
					f.Close()
				}
			}
			fileServer.ServeHTTP(w, r)
		})
	}
	if prefix := strings.TrimSuffix(rc.Path, "/"); prefix != "" {
		h = http.StripPrefix(prefix, h)
	}
	return h
}

// ---------------------------------------------------------------------------
// Reverse proxy + load balancing + health checks
// ---------------------------------------------------------------------------

type upstream struct {
	target  *url.URL
	proxy   *httputil.ReverseProxy
	healthy atomic.Bool
	active  atomic.Int64

	requests, failures atomic.Uint64 // for metrics
}

func (u *upstream) setHealthy(ok bool) {
	if u.healthy.Swap(ok) != ok {
		if ok {
			log.Printf("upstream %s is healthy again", u.target.Host)
		} else {
			log.Printf("upstream %s marked DOWN", u.target.Host)
		}
	}
}

type balancer struct {
	ups    []*upstream
	route  string
	policy string
	next   atomic.Uint64
}

// pick returns a healthy upstream, or nil when none are available.
func (b *balancer) pick() *upstream {
	n := uint64(len(b.ups))
	start := b.next.Add(1) - 1

	if b.policy == "least_conn" {
		var best *upstream
		var bestLoad int64
		for i := uint64(0); i < n; i++ { // rotating start breaks ties fairly
			u := b.ups[(start+i)%n]
			if !u.healthy.Load() {
				continue
			}
			if load := u.active.Load(); best == nil || load < bestLoad {
				best, bestLoad = u, load
			}
		}
		return best
	}

	for i := uint64(0); i < n; i++ {
		if u := b.ups[(start+i)%n]; u.healthy.Load() {
			return u
		}
	}
	return nil
}

func (b *balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u := b.pick()
	if u == nil {
		http.Error(w, "no healthy upstream available", http.StatusServiceUnavailable)
		return
	}
	u.requests.Add(1)
	u.active.Add(1)
	defer u.active.Add(-1)
	u.proxy.ServeHTTP(w, r)
}

func proxyHandler(bc *buildCtx, rc RouteConfig) (http.Handler, error) {
	pc := rc.Proxy
	strip := ""
	if rc.StripPrefix {
		strip = strings.TrimSuffix(rc.Path, "/")
	}

	b := &balancer{policy: pc.Policy, route: rc.Path}
	for _, raw := range pc.Upstreams {
		target, err := url.Parse(raw)
		if err != nil {
			return nil, err
		}
		u := &upstream{target: target}
		u.healthy.Store(true) // optimistic until a health check says otherwise
		u.proxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				if strip != "" {
					p := strings.TrimPrefix(pr.Out.URL.Path, strip)
					if !strings.HasPrefix(p, "/") {
						p = "/" + p
					}
					pr.Out.URL.Path = p
					pr.Out.URL.RawPath = ""
				}
				pr.SetURL(target)
				pr.SetXForwarded() // sets X-Forwarded-For/Host/Proto, drops spoofed ones
				if pc.PreserveHost {
					pr.Out.Host = pr.In.Host
				}
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				if !errors.Is(err, context.Canceled) {
					u.failures.Add(1)
					log.Printf("proxy error (%s): %v", target.Host, err)
				}
				http.Error(w, "bad gateway", http.StatusBadGateway)
			},
		}
		b.ups = append(b.ups, u)
	}

	if hc := pc.HealthCheck; hc != nil {
		go b.healthLoop(bc.ctx, hc)
	}
	bc.balancers = append(bc.balancers, b)

	var h http.Handler = b
	if pc.Cache != nil {
		h = newCache(pc.Cache, bc.m).wrap(h)
	}
	return h, nil
}

func (b *balancer) healthLoop(ctx context.Context, hc *HealthCheckConfig) {
	client := &http.Client{
		Timeout: time.Duration(hc.Timeout),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	check := func() {
		var wg sync.WaitGroup
		for _, u := range b.ups {
			wg.Add(1)
			go func(u *upstream) {
				defer wg.Done()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.target.JoinPath(hc.Path).String(), nil)
				if err != nil {
					u.setHealthy(false)
					return
				}
				resp, err := client.Do(req)
				if err != nil {
					if ctx.Err() == nil {
						u.setHealthy(false)
					}
					return
				}
				resp.Body.Close()
				u.setHealthy(resp.StatusCode >= 200 && resp.StatusCode < 400)
			}(u)
		}
		wg.Wait()
	}

	check()
	t := time.NewTicker(time.Duration(hc.Interval))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
