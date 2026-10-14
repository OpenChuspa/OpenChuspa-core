package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExposicion(t *testing.T) {
	m := New("0.3.0")
	errs := m.Counter("chuspa_maintenance_errors_total", "Fallos del mantenimiento.")
	m.GaugeFunc("chuspa_disk_free_bytes", "Espacio libre.", func() float64 { return 42 })
	m.ObserveRequest("PUT", 200, 30*time.Millisecond, 100, 0)
	m.ObserveRequest("PUT", 200, 2*time.Second, 50, 0)
	m.ObserveRequest("BREW", 405, time.Millisecond, 0, 10)
	errs.Add(2)

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{
		`chuspa_build_info{version="0.3.0"} 1`,
		`chuspa_requests_total{method="PUT",code="200"} 2`,
		`chuspa_requests_total{method="OTHER",code="405"} 1`,
		`chuspa_request_duration_seconds_bucket{method="PUT",le="0.05"} 1`,
		`chuspa_request_duration_seconds_bucket{method="PUT",le="+Inf"} 2`,
		`chuspa_request_duration_seconds_count{method="PUT"} 2`,
		`chuspa_received_bytes_total 150`,
		`chuspa_sent_bytes_total 10`,
		`chuspa_maintenance_errors_total 2`,
		`chuspa_disk_free_bytes 42`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("falta %q en:\n%s", want, out)
		}
	}
	if rec := httptest.NewRecorder(); func() int { m.ServeHTTP(rec, httptest.NewRequest("GET", "/otra", nil)); return rec.Code }() != 404 {
		t.Error("una ruta distinta de /metrics debería dar 404")
	}
}
