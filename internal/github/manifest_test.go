package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildManifest_FieldsFromBaseURL(t *testing.T) {
	m := BuildManifest("https://example.test")
	if m.HookAttributes["url"] != "https://example.test/github/webhook" {
		t.Errorf("hook url = %q", m.HookAttributes["url"])
	}
	if m.RedirectURL != "https://example.test/github/app/callback" {
		t.Errorf("redirect url = %q", m.RedirectURL)
	}
	if m.DefaultPermissions["administration"] != "write" {
		t.Errorf("administration permission missing")
	}
}

func TestBuildManifest_NameDeterministicAndSuffixed(t *testing.T) {
	m1 := BuildManifest("https://a.example.test")
	m2 := BuildManifest("https://a.example.test")
	m3 := BuildManifest("https://b.example.test")
	if m1.Name != m2.Name {
		t.Errorf("same BaseURL should produce same name: %q vs %q", m1.Name, m2.Name)
	}
	if m1.Name == m3.Name {
		t.Errorf("different BaseURLs should produce different names: %q == %q", m1.Name, m3.Name)
	}
	if !strings.HasPrefix(m1.Name, "gharp-") {
		t.Errorf("name must start with gharp-, got %q", m1.Name)
	}
	if len(m1.Name) != len("gharp-")+6 {
		t.Errorf("name suffix should be 6 chars, got %q", m1.Name)
	}
}

func TestConvertCode_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/app-manifests/") || !strings.HasSuffix(r.URL.Path, "/conversions") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("expected no Authorization header, got %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{
			"id": 42,
			"slug": "gharp-test",
			"webhook_secret": "shhh",
			"pem": "-----BEGIN RSA PRIVATE KEY-----\nFAKE\n-----END RSA PRIVATE KEY-----\n",
			"client_id": "Iv1.abc",
			"client_secret": "secretvalue"
		}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)

	creds, err := c.ConvertCode(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("ConvertCode: %v", err)
	}
	if creds.AppID != 42 || creds.Slug != "gharp-test" || creds.WebhookSecret != "shhh" {
		t.Errorf("creds = %+v", creds)
	}
	if creds.ClientID != "Iv1.abc" || creds.ClientSecret != "secretvalue" {
		t.Errorf("client creds wrong: %+v", creds)
	}
	if !strings.Contains(string(creds.PEM), "BEGIN RSA PRIVATE KEY") {
		t.Errorf("pem not preserved as bytes: %q", creds.PEM)
	}
}

func TestConvertCode_NonJSONFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)

	_, err := c.ConvertCode(context.Background(), "the-code")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("want decode error, got %v", err)
	}
}

func TestConvertCode_404Surfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)

	_, err := c.ConvertCode(context.Background(), "the-code")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want 404 surfaced in error, got %v", err)
	}
}

func TestConvertCode_EscapesCode(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"id":1,"slug":"x","webhook_secret":"s","pem":"p","client_id":"i","client_secret":"sc"}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)

	// A pathologically-shaped code with a slash and reserved chars.
	if _, err := c.ConvertCode(context.Background(), "../etc/passwd?evil"); err != nil {
		t.Fatal(err)
	}
	// '/' becomes %2F, '?' becomes %3F — confirms the segment did not split
	// or terminate the path early.
	if !strings.Contains(gotPath, "%2F") || !strings.Contains(gotPath, "%3F") {
		t.Errorf("expected escaped code in path, got %q", gotPath)
	}
}
