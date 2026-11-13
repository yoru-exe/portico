package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Reverse proxy + load balancing + health checks
// ---------------------------------------------------------------------------

const (
	dialTimeout                  = 5 * time.Second  // a dead host must fail fast so another upstream can be tried
	passiveFailWindow            = 10 * time.Second // how long an upstream is skipped after a connect failure
	defaultResponseHeaderTimeout = 60 * time.Second
)

type upstream struct {
	target      *url.URL
	proxy       *httputil.ReverseProxy
	healthy     atomic.Bool  // result of the active health checks
	failedUntil atomic.Int64 // unix nanos: skipped until then after a connect failure
	active      atomic.Int64

	requests, failures atomic.Uint64 // for metrics
}

// available reports whether the upstream should currently receive traffic.
func (u *upstream) available(now time.Time) bool {
	return u.healthy.Load() && now.UnixNano() >= u.failedUntil.Load()
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

// markFailed takes the upstream out of rotation briefly after a connect
// failure, so we don't keep sending requests to it until the next health check.
func (u *upstream) markFailed() {
	now := time.Now()
	if prev := u.failedUntil.Swap(now.Add(passiveFailWindow).UnixNano()); now.UnixNano() >= prev {
		log.Printf("upstream %s is unreachable, skipping it for %s", u.target.Host, passiveFailWindow)
	}
}

type balancer struct {
	ups    []*upstream
	route  string
	policy string
	next   atomic.Uint64
}

// pick returns an upstream that has not been tried yet, or nil.
// It prefers upstreams that are healthy and not cooling down; if there are
// none it falls back to healthy-but-cooling ones (better a retry than a sure 503).
func (b *balancer) pick(tried []*upstream) *upstream {
	now := time.Now()
	n := uint64(len(b.ups))
	start := b.next.Add(1) - 1

	for pass := 0; pass < 2; pass++ {
		var best *upstream
		var bestLoad int64
		for i := uint64(0); i < n; i++ { // rotating start breaks ties fairly
			u := b.ups[(start+i)%n]
			if wasTried(tried, u) {
				continue
			}
			ok := u.healthy.Load()
			if pass == 0 {
				ok = ok && u.available(now)
			}
			if !ok {
				continue
			}
			if b.policy != "least_conn" {
				return u
			}
			if load := u.active.Load(); best == nil || load < bestLoad {
				best, bestLoad = u, load
			}
		}
		if best != nil {
			return best
		}
	}
	return nil
}

func wasTried(tried []*upstream, u *upstream) bool {
	for _, t := range tried {
		if t == u {
			return true
		}
	}
	return false
}

// attempt carries a proxy error from the ReverseProxy's ErrorHandler back to
// the balancer, which decides whether to retry on another upstream.
type attemptKey struct{}
type attempt struct{ err error }

func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func (b *balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A connect failure means nothing was sent, so a body-less request can
	// safely go to the next upstream.
	canRetry := r.Body == nil || r.Body == http.NoBody
	var tried []*upstream

	for {
		u := b.pick(tried)
		if u == nil {
			if len(tried) > 0 {
				http.Error(w, "bad gateway", http.StatusBadGateway)
			} else {
				http.Error(w, "no healthy upstream available", http.StatusServiceUnavailable)
			}
			return
		}
		tried = append(tried, u)

		att := &attempt{}
		u.requests.Add(1)
		u.active.Add(1)
		u.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), attemptKey{}, att)))
		u.active.Add(-1)

		if att.err == nil {
			return
		}
		if isDialError(att.err) {
			u.markFailed()
			if canRetry && len(tried) < len(b.ups) {
				continue
			}
		}
		if !errors.Is(att.err, context.Canceled) {
			log.Printf("proxy error (%s): %v", u.target.Host, att.err)
		}
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
}

// newProxyTransport builds the connection pool used to talk to upstreams.
func newProxyTransport(responseHeaderTimeout time.Duration) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil // never route upstream traffic through HTTP(S)_PROXY from the environment
	t.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	// The defaults keep only 2 idle connections per upstream, which forces a new
	// TCP connection for most requests under load.
	t.MaxIdleConns = 1024
	t.MaxIdleConnsPerHost = 256
	t.ResponseHeaderTimeout = responseHeaderTimeout
	return t
}

// stripURLPrefix removes prefix from the URL path without losing percent
// encoding (so "/api/a%2Fb" becomes "/a%2Fb", not "/a/b").
func stripURLPrefix(u *url.URL, prefix string) {
	esc := u.EscapedPath()
	if strings.HasPrefix(esc, prefix) {
		rest := esc[len(prefix):]
		if !strings.HasPrefix(rest, "/") {
			rest = "/" + rest
		}
		if p, err := url.PathUnescape(rest); err == nil {
			u.Path, u.RawPath = p, rest
			return
		}
	}
	// The prefix is spelled differently in encoded form: fall back to the decoded path.
	p := strings.TrimPrefix(u.Path, prefix)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u.Path, u.RawPath = p, ""
}

func proxyHandler(bc *buildCtx, rc RouteConfig) (http.Handler, error) {
	pc := rc.Proxy
	strip := ""
	if rc.StripPrefix {
		strip = strings.TrimSuffix(rc.Path, "/")
	}

	tr := newProxyTransport(pc.responseHeaderTimeout())
	bc.transports = append(bc.transports, tr)

	b := &balancer{policy: pc.Policy, route: rc.Path}
	for _, raw := range pc.Upstreams {
		target, err := url.Parse(raw)
		if err != nil {
			return nil, err
		}
		u := &upstream{target: target}
		u.healthy.Store(true) // optimistic until a health check says otherwise
		u.proxy = &httputil.ReverseProxy{
			Transport: tr,
			Rewrite: func(pr *httputil.ProxyRequest) {
				if strip != "" {
					stripURLPrefix(pr.Out.URL, strip)
				}
				pr.SetURL(target)
				pr.SetXForwarded() // sets X-Forwarded-For/Host/Proto, drops spoofed ones
				if pc.PreserveHost {
					pr.Out.Host = pr.In.Host
				}
			},
			ModifyResponse: func(resp *http.Response) error {
				resp.Header.Del("Server") // we add our own; don't leak the upstream's
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				if !errors.Is(err, context.Canceled) {
					u.failures.Add(1)
				}
				// Hand the error to the balancer (it may retry on another upstream).
				if att, ok := r.Context().Value(attemptKey{}).(*attempt); ok {
					att.err = err
				}
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
