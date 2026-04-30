package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

func TestMetrics_OpenWhenAdminTokenEmpty(t *testing.T) {
	st := &jobsStore{
		summary: &store.Summary{
			JobsByStatus:    map[string]int64{"pending": 2, "completed": 3},
			RunnersByStatus: map[string]int64{"starting": 1, "busy": 2},
			ActiveRunners:   3,
		},
	}
	h := NewMetricsHandler(&config.Config{MaxConcurrentRunners: 4}, st, nil)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`gharp_jobs_total{status="pending"} 2`,
		`gharp_jobs_total{status="completed"} 3`,
		`gharp_runners_total{status="starting"} 1`,
		`gharp_runners_total{status="busy"} 2`,
		`gharp_active_runners 3`,
		`gharp_max_concurrent_runners 4`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "gharp_pending_jobs") {
		t.Fatalf("gharp_pending_jobs should not be present (redundant with gharp_jobs_total{status=pending})")
	}
}

func TestMetrics_TokenRequiredWhenAdminTokenSet(t *testing.T) {
	st := &jobsStore{summary: &store.Summary{JobsByStatus: map[string]int64{}, RunnersByStatus: map[string]int64{}}}
	h := NewMetricsHandler(&config.Config{AdminToken: "secret"}, st, nil)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr = httptest.NewRecorder()
	h.Get(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}
