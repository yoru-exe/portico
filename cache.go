package main

import (
	"container/list"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// In-memory response cache for proxy routes (LRU, size-bounded)
// ---------------------------------------------------------------------------

type cacheEntry struct {
	key     string
	status  int
	header  http.Header
	body    []byte
	stored  time.Time
	expires time.Time
}

func (e *cacheEntry) size() int { return len(e.body) + 512 } // 512 ~ header overhead

type responseCache struct {
	mu         sync.Mutex
	ll         *list.List // front = most recently used
	items      map[string]*list.Element
	bytes      int
	maxBytes   int
	maxEntry   int
	defaultTTL time.Duration
	m          *Metrics
}

func newCache(cc *CacheConfig, m *Metrics) *responseCache {
	return &responseCache{
		ll:         list.New(),
		items:      map[string]*list.Element{},
		maxBytes:   cc.MaxMB << 20,
		maxEntry:   cc.MaxEntryKB << 10,
		defaultTTL: time.Duration(cc.DefaultTTL),
		m:          m,
	}
}

func (c *responseCache) get(key string) *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.items[key]
	if el == nil {
		return nil
	}
	e := el.Value.(*cacheEntry)
	if time.Now().After(e.expires) {
		c.removeLocked(el)
		return nil
	}
	c.ll.MoveToFront(el)
	return e
}

func (c *responseCache) put(e *cacheEntry) {
	if e.size() > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.items[e.key]; old != nil {
		c.removeLocked(old)
	}
	c.items[e.key] = c.ll.PushFront(e)
	c.bytes += e.size()
	for c.bytes > c.maxBytes && c.ll.Len() > 0 {
		c.removeLocked(c.ll.Back())
	}
}

func (c *responseCache) removeLocked(el *list.Element) {
	e := c.ll.Remove(el).(*cacheEntry)
	delete(c.items, e.key)
	c.bytes -= e.size()
}

// cacheable decides, from the response headers, whether a response may be
// stored and for how long.
func (c *responseCache) cacheable(status int, h http.Header) (time.Duration, bool) {
	switch status {
	case 200, 301, 308, 404:
	default:
		return 0, false
	}
	if h.Get("Set-Cookie") != "" || h.Get("Content-Range") != "" {
		return 0, false
	}
	if strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "text/event-stream") {
		return 0, false // streams must never be stored and replayed
	}
	// The cache key already includes Accept-Encoding; any other Vary is unsafe.
	for _, v := range h.Values("Vary") {
		for _, tok := range strings.Split(v, ",") {
			if t := strings.ToLower(strings.TrimSpace(tok)); t != "" && t != "accept-encoding" {
				return 0, false
			}
		}
	}
	cc := parseCacheControl(h.Get("Cache-Control"))
	if _, ok := cc["no-store"]; ok {
		return 0, false
	}
	if _, ok := cc["private"]; ok {
		return 0, false
	}
	if _, ok := cc["no-cache"]; ok {
		return 0, false
	}
	ttl := c.defaultTTL
	if v, ok := cc["s-maxage"]; ok {
		ttl = parseSeconds(v)
	} else if v, ok := cc["max-age"]; ok {
		ttl = parseSeconds(v)
	}
	return ttl, ttl > 0
}

func parseCacheControl(v string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, val, _ := strings.Cut(part, "=")
		out[strings.ToLower(k)] = strings.Trim(val, `"`)
	}
	return out
}

func parseSeconds(v string) time.Duration {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

func requestForbidsCache(r *http.Request) bool {
	cc := parseCacheControl(r.Header.Get("Cache-Control"))
	_, noCache := cc["no-cache"]
	_, noStore := cc["no-store"]
	return noCache || noStore
}

func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	norm := func(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "W/") }
	for _, t := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimSpace(t) == "*" || norm(t) == norm(etag) {
			return true
		}
	}
	return false
}

func (c *responseCache) serve(w http.ResponseWriter, r *http.Request, e *cacheEntry) {
	h := w.Header()
	for k, v := range e.header {
		h[k] = append([]string(nil), v...)
	}
	h.Set("Age", strconv.Itoa(int(time.Since(e.stored).Seconds())))
	h.Set("X-Cache", "HIT")
	if etag := e.header.Get("ETag"); etag != "" && etagMatches(r.Header.Get("If-None-Match"), etag) {
		h.Del("Content-Length")
		h.Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(e.body)))
	w.WriteHeader(e.status)
	w.Write(e.body)
}

// wrap returns a handler that answers from the cache when it can and
// otherwise passes the request on, storing the response if it is cacheable.
func (c *responseCache) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet ||
			r.Header.Get("Authorization") != "" ||
			r.Header.Get("Cookie") != "" ||
			r.Header.Get("Range") != "" ||
			r.Header.Get("Upgrade") != "" ||
			requestForbidsCache(r) {
			w.Header().Set("X-Cache", "BYPASS")
			next.ServeHTTP(w, r)
			return
		}
		key := r.Host + "\x00" + r.URL.RequestURI() + "\x00" + r.Header.Get("Accept-Encoding")
		if e := c.get(key); e != nil {
			c.m.cacheHits.Add(1)
			c.serve(w, r, e)
			return
		}
		c.m.cacheMisses.Add(1)
		cw := &captureWriter{ResponseWriter: w, cache: c, key: key}
		next.ServeHTTP(cw, r)
		cw.finish()
	})
}

// captureWriter streams the response to the client while keeping a copy
// (bounded by max_entry_kb) so it can be stored afterwards.
type captureWriter struct {
	http.ResponseWriter
	cache   *responseCache
	key     string
	wrote   bool
	status  int
	hdr     http.Header
	ttl     time.Duration
	capture bool
	buf     []byte
}

func (c *captureWriter) WriteHeader(code int) {
	if c.wrote || (code < 200 && code != http.StatusSwitchingProtocols) {
		c.ResponseWriter.WriteHeader(code) // 1xx informational responses pass through
		return
	}
	c.wrote = true
	c.status = code
	h := c.Header()
	h.Set("X-Cache", "MISS")
	if ttl, ok := c.cache.cacheable(code, h); ok {
		c.capture, c.ttl = true, ttl
		c.hdr = h.Clone()
		c.hdr.Del("X-Cache")
		c.hdr.Del("Date")
		c.hdr.Del("Age")
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.WriteHeader(http.StatusOK)
	}
	if c.capture {
		if len(c.buf)+len(b) > c.cache.maxEntry {
			c.capture, c.buf = false, nil // too big to cache
		} else {
			c.buf = append(c.buf, b...)
		}
	}
	return c.ResponseWriter.Write(b)
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *captureWriter) finish() {
	if !c.capture {
		return
	}
	// A declared Content-Length must match what we actually captured.
	if cl := c.hdr.Get("Content-Length"); cl != "" {
		if n, err := strconv.Atoi(cl); err != nil || n != len(c.buf) {
			return
		}
	}
	now := time.Now()
	c.cache.put(&cacheEntry{
		key: c.key, status: c.status, header: c.hdr, body: c.buf,
		stored: now, expires: now.Add(c.ttl),
	})
	c.cache.m.cacheStores.Add(1)
}
