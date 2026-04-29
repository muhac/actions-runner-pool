package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRegistrationToken_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/repos/alice/repo/actions/runners/registration-token"
		if r.URL.Path != want {
			t.Errorf("path = %q, want %q", r.URL.Path, want)
		}
		_, _ = w.Write([]byte(`{"token":"reg-token-abc","expires_at":"2030-01-01T00:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)

	tok, err := c.RegistrationToken(context.Background(), "install-tok", "alice/repo")
	if err != nil {
		t.Fatalf("RegistrationToken: %v", err)
	}
	if tok != "reg-token-abc" {
		t.Errorf("token = %q", tok)
	}
}

func TestRegistrationToken_AuthHeaderForwarded(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"token":"x"}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	if _, err := c.RegistrationToken(context.Background(), "install-tok-99", "a/b"); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer install-tok-99" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer install-tok-99")
	}
}

func TestRegistrationToken_NoCache(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"token":"x"}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	for range 3 {
		if _, err := c.RegistrationToken(context.Background(), "tok", "a/b"); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 3 {
		t.Errorf("hits = %d, want 3 (registration tokens never cached)", hits.Load())
	}
}

func TestRegistrationToken_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	_, err := c.RegistrationToken(context.Background(), "tok", "a/b")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want 403 in error, got %v", err)
	}
}

func TestRegistrationToken_RejectsBadRepoFullName(t *testing.T) {
	c := newTestClient(t, "https://example.test")
	for _, in := range []string{"", "no-slash", "/missing-owner", "missing-repo/", "a/b/c", "../etc/passwd"} {
		t.Run(in, func(t *testing.T) {
			_, err := c.RegistrationToken(context.Background(), "tok", in)
			if err == nil {
				t.Fatalf("expected validation error for %q", in)
			}
		})
	}
}

func TestRegistrationToken_EscapesSegments(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"token":"x"}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	// Owner with a space (valid GitHub username characters are limited but we
	// still want to assert escaping happens; "a b" exercises the encoder).
	if _, err := c.RegistrationToken(context.Background(), "tok", "a b/repo"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPath, "a%20b") {
		t.Errorf("expected escaped owner segment in path, got %q", gotPath)
	}
}

// ListRepoRunners single-page happy path: response total_count <= 100,
// one round trip, every field decodes including the labels object array.
func TestListRepoRunners_SinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":2,"runners":[
			{"id":1,"name":"a","status":"online","busy":false,"labels":[{"name":"self-hosted"},{"name":"linux"}]},
			{"id":2,"name":"b","status":"offline","busy":true,"labels":[{"name":"gpu"}]}
		]}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)

	got, err := c.ListRepoRunners(context.Background(), "tok", "alice/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	if got[0].ID != 1 || got[0].Name != "a" || got[0].Status != "online" || got[0].Busy {
		t.Fatalf("got[0]=%+v", got[0])
	}
	if len(got[0].Labels) != 2 || got[0].Labels[0] != "self-hosted" || got[0].Labels[1] != "linux" {
		t.Fatalf("got[0].Labels=%v", got[0].Labels)
	}
	if got[1].ID != 2 || !got[1].Busy {
		t.Fatalf("got[1]=%+v", got[1])
	}
}

// Pagination: total_count=150 → server returns 100 then 50 → 2
// round-trips, all 150 collected.
func TestListRepoRunners_Pagination(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		page := r.URL.Query().Get("page")
		switch page {
		case "1":
			runners := make([]map[string]any, 100)
			for i := range runners {
				runners[i] = map[string]any{"id": i + 1, "name": "r", "status": "online", "labels": []any{}}
			}
			body := map[string]any{"total_count": 150, "runners": runners}
			b, _ := json.Marshal(body)
			_, _ = w.Write(b)
		case "2":
			runners := make([]map[string]any, 50)
			for i := range runners {
				runners[i] = map[string]any{"id": 100 + i + 1, "name": "r", "status": "online", "labels": []any{}}
			}
			body := map[string]any{"total_count": 150, "runners": runners}
			b, _ := json.Marshal(body)
			_, _ = w.Write(b)
		default:
			t.Errorf("unexpected page=%s", page)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	got, err := c.ListRepoRunners(context.Background(), "tok", "alice/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 150 {
		t.Fatalf("got %d runners, want 150", len(got))
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

func TestListRepoRunners_AuthHeaderForwarded(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"total_count":0,"runners":[]}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	if _, err := c.ListRepoRunners(context.Background(), "install-tok-77", "a/b"); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer install-tok-77" {
		t.Errorf("Authorization=%q", got)
	}
}

func TestListRepoRunners_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	if _, err := c.ListRepoRunners(context.Background(), "tok", "a/b"); err == nil ||
		!strings.Contains(err.Error(), "403") {
		t.Fatalf("want 403 in error, got %v", err)
	}
}

func TestDeleteRepoRunner_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/repos/alice/repo/actions/runners/123"
		if r.URL.Path != want || r.Method != http.MethodDelete {
			t.Errorf("got %s %s, want DELETE %s", r.Method, r.URL.Path, want)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	if err := c.DeleteRepoRunner(context.Background(), "tok", "alice/repo", 123); err != nil {
		t.Fatal(err)
	}
}

// 404 from the DELETE means the runner already vanished — caller's
// intent (make sure it's gone) is satisfied, so it must not be an
// error. Otherwise a benign race between List and Delete would
// produce noisy log lines on every reconcile tick.
func TestDeleteRepoRunner_404_Swallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	if err := c.DeleteRepoRunner(context.Background(), "tok", "alice/repo", 1); err != nil {
		t.Fatalf("404 must be swallowed, got %v", err)
	}
}

func TestDeleteRepoRunner_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	if err := c.DeleteRepoRunner(context.Background(), "tok", "a/b", 1); err == nil ||
		!strings.Contains(err.Error(), "403") {
		t.Fatalf("want 403 in error, got %v", err)
	}
}
