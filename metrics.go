package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Metrics holds process-wide counters. They live in the Server, not in the
// per-reload Runtime, so they survive config reloads.
type Metrics struct {
	start    time.Time
	inflight atomic.Int64
	status   [6]atomic.Uint64 // index 1..5 = 1xx..5xx
	bytes    atomic.Uint64
	durNanos atomic.Uint64

	cacheHits, cacheMisses, cacheStores atomic.Uint64
	reloads, reloadFailures             atomic.Uint64
}

func (m *Metrics) observe(status int, bytes int64, d time.Duration) {
	class := status / 100
	if class < 1 || class > 5 {
		class = 5
	}
	m.status[class].Add(1)
	if bytes > 0 {
		m.bytes.Add(uint64(bytes))
	}
	m.durNanos.Add(uint64(d))
}

// labelEscaper escapes label values for the Prometheus text format.
var labelEscaper = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n")

// metricsHandler renders Prometheus text format.
func metricsHandler(s *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		m := s.metrics
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		metric := func(name, typ, help string) {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		}

		metric("portico_uptime_seconds", "gauge", "Seconds since start.")
		fmt.Fprintf(w, "portico_uptime_seconds %.0f\n", time.Since(m.start).Seconds())

		metric("portico_http_requests_total", "counter", "Requests served, by status class.")
		var total uint64
		for c := 1; c <= 5; c++ {
			n := m.status[c].Load()
			total += n
			fmt.Fprintf(w, "portico_http_requests_total{code=\"%dxx\"} %d\n", c, n)
		}
		metric("portico_http_requests_in_flight", "gauge", "Requests currently being served.")
		fmt.Fprintf(w, "portico_http_requests_in_flight %d\n", m.inflight.Load())
		metric("portico_http_response_bytes_total", "counter", "Response body bytes sent.")
		fmt.Fprintf(w, "portico_http_response_bytes_total %d\n", m.bytes.Load())
		metric("portico_http_request_duration_seconds_sum", "counter", "Total time spent serving requests.")
		fmt.Fprintf(w, "portico_http_request_duration_seconds_sum %.6f\n", float64(m.durNanos.Load())/1e9)
		metric("portico_http_request_duration_seconds_count", "counter", "Number of timed requests.")
		fmt.Fprintf(w, "portico_http_request_duration_seconds_count %d\n", total)

		metric("portico_cache_hits_total", "counter", "Responses served from the cache.")
		fmt.Fprintf(w, "portico_cache_hits_total %d\n", m.cacheHits.Load())
		metric("portico_cache_misses_total", "counter", "Cacheable-looking requests that missed.")
		fmt.Fprintf(w, "portico_cache_misses_total %d\n", m.cacheMisses.Load())
		metric("portico_cache_stores_total", "counter", "Responses stored in the cache.")
		fmt.Fprintf(w, "portico_cache_stores_total %d\n", m.cacheStores.Load())

		metric("portico_config_reloads_total", "counter", "Successful config reloads.")
		fmt.Fprintf(w, "portico_config_reloads_total %d\n", m.reloads.Load())
		metric("portico_config_reload_failures_total", "counter", "Rejected config reloads.")
		fmt.Fprintf(w, "portico_config_reload_failures_total %d\n", m.reloadFailures.Load())

		// Per-upstream gauges/counters (reset when the config is reloaded).
		type row struct {
			label              string
			healthy            int
			active             int64
			requests, failures uint64
		}
		var rows []row
		if rt := s.rt.Load(); rt != nil {
			for _, b := range rt.balancers {
				for _, u := range b.ups {
					h := 0
					if u.available(time.Now()) {
						h = 1
					}
					rows = append(rows, row{
						label:    fmt.Sprintf(`route="%s",upstream="%s"`, labelEscaper.Replace(b.route), labelEscaper.Replace(u.target.Host)),
						healthy:  h,
						active:   u.active.Load(),
						requests: u.requests.Load(),
						failures: u.failures.Load(),
					})
				}
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].label < rows[j].label })
		metric("portico_upstream_healthy", "gauge", "1 if the upstream is considered healthy.")
		for _, r := range rows {
			fmt.Fprintf(w, "portico_upstream_healthy{%s} %d\n", r.label, r.healthy)
		}
		metric("portico_upstream_active_requests", "gauge", "In-flight requests per upstream.")
		for _, r := range rows {
			fmt.Fprintf(w, "portico_upstream_active_requests{%s} %d\n", r.label, r.active)
		}
		metric("portico_upstream_requests_total", "counter", "Requests sent to each upstream.")
		for _, r := range rows {
			fmt.Fprintf(w, "portico_upstream_requests_total{%s} %d\n", r.label, r.requests)
		}
		metric("portico_upstream_failures_total", "counter", "Failed proxy attempts per upstream.")
		for _, r := range rows {
			fmt.Fprintf(w, "portico_upstream_failures_total{%s} %d\n", r.label, r.failures)
		}
	})
	return mux
}
