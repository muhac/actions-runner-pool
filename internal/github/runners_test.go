package github

import (
	"context"
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
