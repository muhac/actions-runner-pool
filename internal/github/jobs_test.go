package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkflowJob_HappyPath_Queued(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/repos/alice/repo/actions/jobs/12345"
		if r.URL.Path != want {
			t.Errorf("path = %q, want %q", r.URL.Path, want)
		}
		_, _ = w.Write([]byte(`{"status":"queued","conclusion":null}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)

	got, err := c.WorkflowJob(context.Background(), "tok", "alice/repo", 12345)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" || got.Conclusion != "" || got.NotFound {
		t.Fatalf("got %+v", got)
	}
}

// Cancelled job: Status=completed, Conclusion=cancelled. The dispatch
// abort path keys on Status != queued, so verify both fields decode.
func TestWorkflowJob_Cancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"completed","conclusion":"cancelled"}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	got, err := c.WorkflowJob(context.Background(), "tok", "a/b", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || got.Conclusion != "cancelled" {
		t.Fatalf("got %+v", got)
	}
}

// 404 is the "job deleted or inaccessible" terminal state and must be
// surfaced as NotFound=true rather than an error — caller policy is
// to mark the job cancelled.
func TestWorkflowJob_404_ReturnsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	got, err := c.WorkflowJob(context.Background(), "tok", "a/b", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.NotFound {
		t.Fatalf("expected NotFound=true, got %+v", got)
	}
}

func TestWorkflowJob_AuthHeaderForwarded(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	if _, err := c.WorkflowJob(context.Background(), "install-tok-77", "a/b", 1); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer install-tok-77" {
		t.Errorf("Authorization = %q", got)
	}
}

// 401 (e.g. installation token revoked because App was uninstalled
// mid-dispatch) is surfaced as AuthFailed rather than an error so the
// scheduler's confirm404 path can treat it equivalently to NotFound.
func TestWorkflowJob_401_ReturnsAuthFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	got, err := c.WorkflowJob(context.Background(), "tok", "a/b", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.AuthFailed || got.NotFound {
		t.Fatalf("expected AuthFailed=true, NotFound=false, got %+v", got)
	}
}

func TestWorkflowJob_403_ReturnsAuthFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	got, err := c.WorkflowJob(context.Background(), "tok", "a/b", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.AuthFailed {
		t.Fatalf("expected AuthFailed=true, got %+v", got)
	}
}

// Rate-limited 403s must NOT surface as AuthFailed: callers treat
// AuthFailed as terminal ("App uninstalled") and cancel real jobs.
// GitHub's primary rate limit is a 403 with x-ratelimit-remaining: 0;
// the secondary limit is a 403/429 with Retry-After. Both must come
// back as plain errors so callers take their transient-failure path
// (skip this tick / proceed optimistically) and retry later.
func TestWorkflowJob_RateLimited_IsErrorNotAuthFailed(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
	}{
		{"primary_403_remaining_zero", http.StatusForbidden,
			map[string]string{"X-RateLimit-Remaining": "0"}},
		{"secondary_403_retry_after", http.StatusForbidden,
			map[string]string{"Retry-After": "60"}},
		{"secondary_429_retry_after", http.StatusTooManyRequests,
			map[string]string{"Retry-After": "60"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)
			c := newTestClient(t, srv.URL)
			got, err := c.WorkflowJob(context.Background(), "tok", "a/b", 1)
			if err == nil {
				t.Fatalf("expected error, got %+v", got)
			}
			if !strings.Contains(err.Error(), "rate limited") {
				t.Fatalf("error should identify rate limiting, got: %v", err)
			}
		})
	}
}

// A 403 with a non-zero remaining quota is a genuine permission
// failure, not a rate limit — it must keep the AuthFailed semantics.
func TestWorkflowJob_403_RemainingNonZero_StaysAuthFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	got, err := c.WorkflowJob(context.Background(), "tok", "a/b", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.AuthFailed {
		t.Fatalf("expected AuthFailed=true, got %+v", got)
	}
}

func TestWorkflowJob_500IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	_, err := c.WorkflowJob(context.Background(), "tok", "a/b", 1)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want 500 in error, got %v", err)
	}
}

func TestWorkflowJob_RejectsBadRepoFullName(t *testing.T) {
	c := newTestClient(t, "https://example.test")
	for _, in := range []string{"", "no-slash", "/missing-owner", "missing-repo/", "a/b/c"} {
		t.Run(in, func(t *testing.T) {
			if _, err := c.WorkflowJob(context.Background(), "tok", in, 1); err == nil {
				t.Fatalf("expected validation error for %q", in)
			}
		})
	}
}

func TestWorkflowJob_EmptyStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":""}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	if _, err := c.WorkflowJob(context.Background(), "tok", "a/b", 1); err == nil {
		t.Fatal("expected error on empty status")
	}
}
