// Package metrics expone las métricas del motor en el formato de texto de
// Prometheus, sin dependencias externas.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Límites superiores, en segundos, del histograma de duración de peticiones.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// Metrics reúne lo que se publica en /metrics.
type Metrics struct {
	version string

	mu        sync.Mutex
	requests  map[[2]string]uint64 // método, código -> cantidad
	durations map[string]*histogram

	bytesIn, bytesOut atomic.Uint64

	counters []*Counter
	gauges   []gauge
}

type histogram struct {
	counts []uint64 // acumuladas por bucket
	count  uint64
	sum    float64
}

// Counter es un contador con nombre, que solo sube.
type Counter struct {
	name, help string
	v          atomic.Uint64
}

// Add suma n al contador.
func (c *Counter) Add(n uint64) { c.v.Add(n) }

type gauge struct {
	name, help string
	fn         func() float64
}

// New crea el registro de métricas.
func New(version string) *Metrics {
	return &Metrics{version: version, requests: map[[2]string]uint64{}, durations: map[string]*histogram{}}
}

// Counter registra un contador. Se llama al arrancar, no por petición.
func (m *Metrics) Counter(name, help string) *Counter {
	c := &Counter{name: name, help: help}
	m.mu.Lock()
	m.counters = append(m.counters, c)
	m.mu.Unlock()
	return c
}

// GaugeFunc registra un valor que se calcula al momento de consultar.
func (m *Metrics) GaugeFunc(name, help string, fn func() float64) {
	m.mu.Lock()
	m.gauges = append(m.gauges, gauge{name, help, fn})
	m.mu.Unlock()
}

// ObserveRequest registra una petición atendida.
func (m *Metrics) ObserveRequest(method string, status int, d time.Duration, in, out int64) {
	if in > 0 {
		m.bytesIn.Add(uint64(in))
	}
	if out > 0 {
		m.bytesOut.Add(uint64(out))
	}
	method = normalizeMethod(method)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[2]string{method, strconv.Itoa(status)}]++
	h := m.durations[method]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(durationBuckets))}
		m.durations[method] = h
	}
	secs := d.Seconds()
	for i, le := range durationBuckets {
		if secs <= le {
			h.counts[i]++
		}
	}
	h.count++
	h.sum += secs
}

// normalizeMethod evita que un cliente invente etiquetas sin límite.
func normalizeMethod(m string) string {
	switch m {
	case "GET", "HEAD", "PUT", "POST", "DELETE", "OPTIONS":
		return m
	}
	return "OTHER"
}

// ServeHTTP escribe las métricas en formato de texto de Prometheus.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m.write(w)
}

// write escribe todas las métricas.
func (m *Metrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	header(w, "chuspa_build_info", "gauge", "Versión del motor.")
	fmt.Fprintf(w, "chuspa_build_info{version=%q} 1\n", m.version)

	header(w, "chuspa_requests_total", "counter", "Peticiones atendidas por método y código HTTP.")
	keys := make([][2]string, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		fmt.Fprintf(w, "chuspa_requests_total{method=%q,code=%q} %d\n", k[0], k[1], m.requests[k])
	}

	header(w, "chuspa_request_duration_seconds", "histogram", "Duración de las peticiones por método.")
	methods := make([]string, 0, len(m.durations))
	for k := range m.durations {
		methods = append(methods, k)
	}
	sort.Strings(methods)
	for _, method := range methods {
		h := m.durations[method]
		for i, le := range durationBuckets {
			fmt.Fprintf(w, "chuspa_request_duration_seconds_bucket{method=%q,le=%q} %d\n", method, strconv.FormatFloat(le, 'g', -1, 64), h.counts[i])
		}
		fmt.Fprintf(w, "chuspa_request_duration_seconds_bucket{method=%q,le=\"+Inf\"} %d\n", method, h.count)
		fmt.Fprintf(w, "chuspa_request_duration_seconds_sum{method=%q} %g\n", method, h.sum)
		fmt.Fprintf(w, "chuspa_request_duration_seconds_count{method=%q} %d\n", method, h.count)
	}

	header(w, "chuspa_received_bytes_total", "counter", "Bytes de cuerpo recibidos según Content-Length.")
	fmt.Fprintf(w, "chuspa_received_bytes_total %d\n", m.bytesIn.Load())
	header(w, "chuspa_sent_bytes_total", "counter", "Bytes de respuesta enviados.")
	fmt.Fprintf(w, "chuspa_sent_bytes_total %d\n", m.bytesOut.Load())

	for _, c := range m.counters {
		header(w, c.name, "counter", c.help)
		fmt.Fprintf(w, "%s %d\n", c.name, c.v.Load())
	}
	for _, g := range m.gauges {
		header(w, g.name, "gauge", g.help)
		fmt.Fprintf(w, "%s %g\n", g.name, g.fn())
	}
}

func header(w io.Writer, name, typ, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}
