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
