package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Panic recovery
// ---------------------------------------------------------------------------

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec) // used by net/http and ReverseProxy to abort a response
				}
				log.Printf("panic serving %s: %v\n%s", r.URL.Path, rec, debug.Stack())
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Access log
// ---------------------------------------------------------------------------

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() { http.NewResponseController(w.ResponseWriter).Flush() }

// Hijack is needed for WebSocket proxying.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking not supported")
	}
	w.status, w.wrote = http.StatusSwitchingProtocols, true
	return h.Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func loggingMiddleware(m *Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.inflight.Add(1)
		defer m.inflight.Add(-1)
		w.Header().Set("Server", "Portico")
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		m.observe(sw.status, sw.bytes, time.Since(start))
		log.Printf("%s %s %s %s -> %d %dB %s",
			clientIP(r), r.Host, r.Method, r.URL.RequestURI(),
			sw.status, sw.bytes, time.Since(start).Round(time.Microsecond))
	})
}

// ---------------------------------------------------------------------------
// Rate limiting (token bucket per client IP)
// ---------------------------------------------------------------------------

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
}

// allow reports whether ip may make a request now; if not, how long to wait.
func (l *limiter) allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

func (l *limiter) cleanup(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.mu.Lock()
			for ip, b := range l.buckets {
				if time.Since(b.last) > 5*time.Minute {
					delete(l.buckets, ip)
				}
			}
			l.mu.Unlock()
		}
	}
}

func rateLimitMiddleware(ctx context.Context, cfg *RateLimitConfig, next http.Handler) http.Handler {
	l := &limiter{
		rate:    cfg.RequestsPerSecond,
		burst:   float64(cfg.Burst),
		buckets: map[string]*bucket{},
	}
	go l.cleanup(ctx)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RemoteAddr is used on purpose: X-Forwarded-For can be spoofed.
		if ok, wait := l.allow(clientIP(r)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Gzip
// ---------------------------------------------------------------------------

var gzPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

func compressible(ct string) bool {
	ct = strings.ToLower(ct)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	switch {
	case ct == "", ct == "text/event-stream":
		return false
	case strings.HasPrefix(ct, "text/"),
		strings.HasSuffix(ct, "+json"),
		strings.HasSuffix(ct, "+xml"):
		return true
	}
	switch ct {
	case "application/json", "application/javascript", "application/xml",
		"application/wasm", "image/svg+xml":
		return true
	}
	return false
}

type gzipWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	compress    bool
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	h := g.Header()
	h.Add("Vary", "Accept-Encoding")
	if code >= 200 && code != http.StatusNoContent && code != http.StatusNotModified &&
		h.Get("Content-Encoding") == "" && compressible(h.Get("Content-Type")) {
		g.compress = true
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		g.gz = gzPool.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		if g.Header().Get("Content-Type") == "" && g.Header().Get("Content-Encoding") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.compress {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipWriter) Flush() {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.compress {
		g.gz.Flush()
	}
	http.NewResponseController(g.ResponseWriter).Flush()
}

func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) close() {
	if g.gz != nil {
		g.gz.Close()
		gzPool.Put(g.gz)
	}
}

func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			r.Header.Get("Upgrade") != "" || // WebSockets
			r.Header.Get("Range") != "" || // byte ranges must not be compressed
			r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}
