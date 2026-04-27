package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

// fakeStore is a minimal Store implementation; only the methods the
// handlers reach are non-stub.
type fakeStore struct {
	store.Store // embed nil interface so missing methods panic loudly if hit
	appConfig   *store.AppConfig
	getErr      error
	saveErr     error
	saved       *store.AppConfig
}

func (f *fakeStore) GetAppConfig(_ context.Context) (*store.AppConfig, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.appConfig, nil
}

func (f *fakeStore) SaveAppConfig(_ context.Context, c *store.AppConfig) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = c
	f.appConfig = c
	return nil
}

func newSetupHandler(t *testing.T, baseURL string, st store.Store) *SetupHandler {
	t.Helper()
	return &SetupHandler{
		Cfg:   &config.Config{BaseURL: baseURL},
		Store: st,
	}
}

func doGET(t *testing.T, h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func TestSetup_FreshInstall_RendersForm(t *testing.T) {
	h := newSetupHandler(t, "https://example.test", &fakeStore{})
	rr := doGET(t, h.Get, "/setup")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `action="https://github.com/settings/apps/new?state=`) {
		t.Errorf("missing form action in body")
	}
	if !strings.Contains(body, `name="manifest"`) {
		t.Errorf("missing manifest hidden field in body")
	}
	// Manifest JSON is HTML-escaped inside value=, so verify a known field present.
	if !strings.Contains(body, "gharp-runners") {
		t.Errorf("manifest payload not embedded; body=%s", body)
	}

	cookies := rr.Result().Cookies()
	var stateCk *http.Cookie
	for _, c := range cookies {
		if c.Name == stateCookie {
			stateCk = c
			break
		}
	}
	if stateCk == nil {
		t.Fatalf("missing %s cookie", stateCookie)
	}
	if !stateCk.HttpOnly {
		t.Errorf("state cookie should be HttpOnly")
	}
	if stateCk.MaxAge != int(stateCookieTTL.Seconds()) {
		t.Errorf("state cookie MaxAge = %d, want %d", stateCk.MaxAge, int(stateCookieTTL.Seconds()))
	}
	if stateCk.Path != "/github/app/callback" {
		t.Errorf("state cookie Path = %q, want /github/app/callback", stateCk.Path)
	}
	if stateCk.Value == "" || len(stateCk.Value) < 16 {
		t.Errorf("state cookie value too short: %q", stateCk.Value)
	}
}

func TestSetup_HTTPSCookieIsSecure(t *testing.T) {
	h := newSetupHandler(t, "https://example.test", &fakeStore{})
	rr := doGET(t, h.Get, "/setup")
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookie && !c.Secure {
			t.Errorf("state cookie should be Secure when BaseURL is https")
		}
	}
}

func TestSetup_HTTPCookieNotSecure(t *testing.T) {
	h := newSetupHandler(t, "http://localhost:8080", &fakeStore{})
	rr := doGET(t, h.Get, "/setup")
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookie && c.Secure {
			t.Errorf("state cookie should NOT be Secure for plain http BaseURL")
		}
	}
}

func TestSetup_ConfiguredInstall_RendersInstallLink(t *testing.T) {
	st := &fakeStore{appConfig: &store.AppConfig{
		Slug: "gharp-test", BaseURL: "https://example.test",
	}}
	h := newSetupHandler(t, "https://example.test", st)
	rr := doGET(t, h.Get, "/setup")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	want := "https://github.com/apps/gharp-test/installations/new"
	if !strings.Contains(body, want) {
		t.Errorf("install link missing; body=%s", body)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookie {
			t.Errorf("should not set state cookie when already configured")
		}
	}
}

func TestSetup_BaseURLMismatch_RendersForm(t *testing.T) {
	// app_config.BaseURL is from a previous deployment — treat as fresh.
	st := &fakeStore{appConfig: &store.AppConfig{
		Slug: "old-app", BaseURL: "https://old.example.test",
	}}
	h := newSetupHandler(t, "https://example.test", st)
	rr := doGET(t, h.Get, "/setup")

	if rr.Code != http.StatusOK {
		t.Fatal(rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `name="manifest"`) {
		t.Errorf("expected fresh form, got: %s", rr.Body.String())
	}
}

func TestSetup_StoreError_500(t *testing.T) {
	st := &fakeStore{getErr: errors.New("disk on fire")}
	h := newSetupHandler(t, "https://example.test", st)
	rr := doGET(t, h.Get, "/setup")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}
