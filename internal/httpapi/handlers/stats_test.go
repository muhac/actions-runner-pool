package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

func TestStats_OpenWhenAdminTokenEmpty(t *testing.T) {
	st := &jobsStore{summary: &store.Summary{
		JobsByStatus:    map[string]int64{"pending": 2, "completed": 7, "deferred": 1},
		RunnersByStatus: map[string]int64{"starting": 1, "busy": 3, "finished": 9},
	}}
	h := &StatsHandler{Cfg: &config.Config{MaxConcurrentRunners: 5}, Store: st}

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var out statsResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Jobs["pending"] != 2 || out.Jobs["dispatched"] != 0 || out.Jobs["deferred"] != 1 {
		t.Fatalf("jobs counts = %+v", out.Jobs)
	}
	if out.Runners["starting"] != 1 || out.Runners["idle"] != 0 || out.Runners["busy"] != 3 {
		t.Fatalf("runner counts = %+v", out.Runners)
	}
	if out.Capacity.MaxConcurrentRunners != 5 || out.Capacity.ActiveRunners != 4 || out.Capacity.AvailableSlots != 1 {
		t.Fatalf("capacity = %+v", out.Capacity)
	}
	if out.UpdatedAt.IsZero() {
		t.Fatal("updated_at is zero")
	}
}

func TestStats_RequiresAdminTokenWhenConfigured(t *testing.T) {
	h := &StatsHandler{Cfg: &config.Config{AdminToken: "secret"}, Store: &jobsStore{}}

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/stats", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr = httptest.NewRecorder()
	h.Get(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status with token = %d, want 200", rr.Code)
	}
}

func TestStats_ClampsAvailableSlotsAtZero(t *testing.T) {
	st := &jobsStore{summary: &store.Summary{
		JobsByStatus:    map[string]int64{},
		RunnersByStatus: map[string]int64{"starting": 2, "idle": 1, "busy": 4},
	}}
	h := &StatsHandler{Cfg: &config.Config{MaxConcurrentRunners: 3}, Store: st}

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	var out statsResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Capacity.ActiveRunners != 7 || out.Capacity.AvailableSlots != 0 {
		t.Fatalf("capacity = %+v", out.Capacity)
	}
}

func TestStats_StoreError(t *testing.T) {
	h := &StatsHandler{
		Cfg:   &config.Config{},
		Store: &jobsStore{summaryErr: errors.New("boom")},
	}

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}
