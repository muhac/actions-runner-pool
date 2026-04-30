package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

type jobsStore struct {
	store.Store
	jobs            []*store.Job
	jobsByID        map[int64]*store.Job
	listErr         error
	getErr          error
	retryErr        error
	cancelErr       error
	summary         *store.Summary
	summaryErr      error
	last            store.JobListFilter
	listCalls       int
	retryCalls      int
	cancelCalls     int
	getCalls        int
	lastRetryJobID  int64
	lastCancelJobID int64
}

func (s *jobsStore) ListJobs(_ context.Context, f store.JobListFilter) ([]*store.Job, error) {
	s.last = f
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.jobs, nil
}

func (s *jobsStore) Summary(_ context.Context) (*store.Summary, error) {
	if s.summaryErr != nil {
		return nil, s.summaryErr
	}
	if s.summary == nil {
		return &store.Summary{JobsByStatus: map[string]int64{}, RunnersByStatus: map[string]int64{}}, nil
	}
	return s.summary, nil
}

func (s *jobsStore) GetJob(_ context.Context, jobID int64) (*store.Job, error) {
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.jobsByID == nil {
		return nil, nil
	}
	return s.jobsByID[jobID], nil
}

func (s *jobsStore) RetryJobIfCompleted(_ context.Context, jobID int64) (bool, error) {
	s.retryCalls++
	s.lastRetryJobID = jobID
	if s.retryErr != nil {
		return false, s.retryErr
	}
	j := s.jobsByID[jobID]
	if j == nil || j.Status != "completed" {
		return false, nil
	}
	j.Status = "pending"
	j.Conclusion = ""
	j.Action = "queued"
	j.RunnerID = 0
	j.RunnerName = ""
	j.UpdatedAt = time.Now().UTC()
	return true, nil
}

func (s *jobsStore) CancelJobIfPending(_ context.Context, jobID int64) (bool, error) {
	s.cancelCalls++
	s.lastCancelJobID = jobID
	if s.cancelErr != nil {
		return false, s.cancelErr
	}
	j := s.jobsByID[jobID]
	if j == nil || (j.Status != "pending" && j.Status != "dispatched") {
		return false, nil
	}
	j.Status = "completed"
	j.Conclusion = "cancelled"
	j.UpdatedAt = time.Now().UTC()
	return true, nil
}

type enqueueSpy struct {
	ids []int64
}

func (e *enqueueSpy) Enqueue(jobID int64) {
	e.ids = append(e.ids, jobID)
}

func TestJobs_OpenWhenAdminTokenEmpty(t *testing.T) {
	received := time.Unix(1, 0).UTC()
	updated := time.Unix(2, 0).UTC()
	st := &jobsStore{jobs: []*store.Job{{
		ID: 1, Repo: "a/b", JobName: "build", RunID: 9, RunAttempt: 2, WorkflowName: "CI",
		Status: "pending", ReceivedAt: received, UpdatedAt: updated,
	}}}
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
	if out.Jobs[0].JobName != "build" || out.Jobs[0].WorkflowName != "CI" || out.Jobs[0].RunAttempt != 2 {
		t.Fatalf("unexpected list payload: %+v", out.Jobs[0])
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

func TestJobs_GetByID(t *testing.T) {
	j := &store.Job{ID: 42, Repo: "a/b", JobName: "build", PayloadJSON: `{"x":1}`, Status: "completed"}
	h := &JobsHandler{Cfg: &config.Config{}, Store: &jobsStore{jobsByID: map[int64]*store.Job{42: j}}}

	req := httptest.NewRequest(http.MethodGet, "/jobs/42", nil)
	req.SetPathValue("job_id", "42")
	rr := httptest.NewRecorder()
	h.GetByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out jobDetailResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != 42 || out.PayloadJSON == "" {
		t.Fatalf("unexpected detail payload: %+v", out)
	}
}

func TestJobs_Retry(t *testing.T) {
	j := &store.Job{ID: 9, Status: "completed", Conclusion: "failure"}
	enq := &enqueueSpy{}
	st := &jobsStore{jobsByID: map[int64]*store.Job{9: j}}
	h := &JobsHandler{Cfg: &config.Config{}, Store: st, Scheduler: enq}

	req := httptest.NewRequest(http.MethodPost, "/jobs/9/retry", nil)
	req.SetPathValue("job_id", "9")
	rr := httptest.NewRecorder()
	h.Retry(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if st.retryCalls != 1 || st.lastRetryJobID != 9 {
		t.Fatalf("retry call mismatch: calls=%d id=%d", st.retryCalls, st.lastRetryJobID)
	}
	if len(enq.ids) != 1 || enq.ids[0] != 9 {
		t.Fatalf("enqueue mismatch: %+v", enq.ids)
	}
	if j.Status != "pending" || j.Conclusion != "" {
		t.Fatalf("job not retried: %+v", j)
	}
}

func TestJobs_Cancel(t *testing.T) {
	j := &store.Job{ID: 7, Status: "pending"}
	st := &jobsStore{jobsByID: map[int64]*store.Job{7: j}}
	h := &JobsHandler{Cfg: &config.Config{}, Store: st}

	req := httptest.NewRequest(http.MethodPost, "/jobs/7/cancel", nil)
	req.SetPathValue("job_id", "7")
	rr := httptest.NewRecorder()
	h.Cancel(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if st.cancelCalls != 1 || st.lastCancelJobID != 7 {
		t.Fatalf("cancel call mismatch: calls=%d id=%d", st.cancelCalls, st.lastCancelJobID)
	}
	if j.Status != "completed" || j.Conclusion != "cancelled" {
		t.Fatalf("job not cancelled: %+v", j)
	}
}

func TestJobs_ActionConflictAndValidation(t *testing.T) {
	h := &JobsHandler{Cfg: &config.Config{}, Store: &jobsStore{jobsByID: map[int64]*store.Job{1: &store.Job{ID: 1, Status: "in_progress"}}}}

	t.Run("retry conflict", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/jobs/1/retry", nil)
		req.SetPathValue("job_id", "1")
		rr := httptest.NewRecorder()
		h.Retry(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rr.Code)
		}
	})

	t.Run("cancel conflict", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/jobs/1/cancel", nil)
		req.SetPathValue("job_id", "1")
		rr := httptest.NewRecorder()
		h.Cancel(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rr.Code)
		}
	})

	t.Run("invalid path value", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/jobs/not-int", nil)
		req.SetPathValue("job_id", "nope")
		rr := httptest.NewRecorder()
		h.GetByID(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
	})
}

func TestJobs_StoreErrorsReturn500(t *testing.T) {
	h := &JobsHandler{Cfg: &config.Config{}, Store: &jobsStore{listErr: errors.New("boom")}}
	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestParseJobIDPath(t *testing.T) {
	for _, tc := range []struct {
		v     string
		valid bool
	}{
		{"1", true},
		{"0", false},
		{"-1", false},
		{"abc", false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/jobs/"+tc.v, nil)
		req.SetPathValue("job_id", tc.v)
		rr := httptest.NewRecorder()
		id, ok := parseJobIDPath(rr, req)
		if ok != tc.valid {
			t.Fatalf("%s ok=%v want=%v", tc.v, ok, tc.valid)
		}
		if ok {
			want, _ := strconv.ParseInt(tc.v, 10, 64)
			if id != want {
				t.Fatalf("id=%d want=%d", id, want)
			}
		}
	}
}
