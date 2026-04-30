package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

type jobsStore struct {
	store.Store
	jobs      []*store.Job
	listErr   error
	last      store.JobListFilter
	listCalls int
}

func (s *jobsStore) ListJobs(_ context.Context, f store.JobListFilter) ([]*store.Job, error) {
	s.last = f
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.jobs, nil
}

func TestJobs_OpenWhenAdminTokenEmpty(t *testing.T) {
	st := &jobsStore{jobs: []*store.Job{{ID: 1, Repo: "a/b", Status: "pending", ReceivedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0)}}}
	h := &JobsHandler{Cfg: &config.Config{}, Store: st}

	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if st.listCalls != 1 {
		t.Fatalf("ListJobs calls = %d, want 1", st.listCalls)
	}
	if st.last.Limit != defaultJobsLimit {
		t.Fatalf("default limit = %d, want %d", st.last.Limit, defaultJobsLimit)
	}

	var out jobsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Count != 1 || len(out.Jobs) != 1 {
		t.Fatalf("count=%d len(jobs)=%d", out.Count, len(out.Jobs))
	}
}

func TestJobs_TokenRequiredWhenAdminTokenSet(t *testing.T) {
	st := &jobsStore{}
	h := &JobsHandler{Cfg: &config.Config{AdminToken: "secret"}, Store: st}

	t.Run("missing", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
		rr := httptest.NewRecorder()
		h.Get(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		rr := httptest.NewRecorder()
		h.Get(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
	})

	t.Run("valid", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rr := httptest.NewRecorder()
		h.Get(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
	})
}

func TestJobs_FilterParsing(t *testing.T) {
	st := &jobsStore{}
	h := &JobsHandler{Cfg: &config.Config{}, Store: st}

	req := httptest.NewRequest(http.MethodGet, "/jobs?status=pending,completed&status=dispatched&repo=owner/repo&limit=999", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if st.last.Repo != "owner/repo" {
		t.Fatalf("repo=%q", st.last.Repo)
	}
	if st.last.Limit != maxJobsLimit {
		t.Fatalf("limit=%d want=%d", st.last.Limit, maxJobsLimit)
	}
	if len(st.last.Statuses) != 3 {
		t.Fatalf("statuses=%v", st.last.Statuses)
	}
}

func TestJobs_InvalidQueryReturns400(t *testing.T) {
	h := &JobsHandler{Cfg: &config.Config{}, Store: &jobsStore{}}

	for _, tc := range []string{
		"/jobs?status=unknown",
		"/jobs?limit=0",
		"/jobs?limit=NaN",
	} {
		t.Run(tc, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc, nil)
			rr := httptest.NewRecorder()
			h.Get(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rr.Code)
			}
		})
	}
}
