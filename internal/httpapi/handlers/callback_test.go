package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/github"
)

type fakeConverter struct {
	creds      *github.AppCredentials
	err        error
	calledWith string
}

func (f *fakeConverter) ConvertCode(_ context.Context, code string) (*github.AppCredentials, error) {
	f.calledWith = code
	if f.err != nil {
		return nil, f.err
	}
	return f.creds, nil
}

func newCallbackHandler(t *testing.T, st *fakeStore, conv *fakeConverter) *CallbackHandler {
	t.Helper()
	return &CallbackHandler{
		Cfg:    &config.Config{BaseURL: "https://example.test"},
		Store:  st,
		GitHub: conv,
	}
}

func doCallback(t *testing.T, h *CallbackHandler, code, queryState, cookieState string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/github/app/callback?code=" + code + "&state=" + queryState
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if cookieState != "" {
		req.AddCookie(&http.Cookie{Name: stateCookie, Value: cookieState})
	}
	rr := httptest.NewRecorder()
	h.Get(rr, req)
	return rr
}

func TestCallback_StateMismatch_400(t *testing.T) {
	h := newCallbackHandler(t, &fakeStore{}, &fakeConverter{})
	rr := doCallback(t, h, "the-code", "queryAAA", "cookieBBB")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestCallback_NoCookie_400(t *testing.T) {
	h := newCallbackHandler(t, &fakeStore{}, &fakeConverter{})
	rr := doCallback(t, h, "the-code", "stateXYZ", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestCallback_MissingQueryParams_400(t *testing.T) {
	h := newCallbackHandler(t, &fakeStore{}, &fakeConverter{})
	req := httptest.NewRequest(http.MethodGet, "/github/app/callback", nil)
	req.AddCookie(&http.Cookie{Name: stateCookie, Value: "x"})
	rr := httptest.NewRecorder()
	h.Get(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestCallback_HappyPath_SavesAndRenders(t *testing.T) {
	st := &fakeStore{}
	conv := &fakeConverter{creds: &github.AppCredentials{
		AppID: 42, Slug: "gharp-test", WebhookSecret: "shh",
		PEM: []byte("PEM"), ClientID: "Iv1.x", ClientSecret: "sec",
	}}
	h := newCallbackHandler(t, st, conv)
	rr := doCallback(t, h, "the-code", "match", "match")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if conv.calledWith != "the-code" {
		t.Errorf("ConvertCode received %q", conv.calledWith)
	}
	if st.saved == nil || st.saved.AppID != 42 || st.saved.Slug != "gharp-test" {
		t.Errorf("SaveAppConfig got %+v", st.saved)
	}
	if st.saved.BaseURL != "https://example.test" {
		t.Errorf("BaseURL not stamped from cfg, got %q", st.saved.BaseURL)
	}
	if !strings.Contains(rr.Body.String(), "/apps/gharp-test/installations/new") {
		t.Errorf("install link missing; body=%s", rr.Body.String())
	}
}

func TestCallback_ConvertCodeFails_500WithoutSave(t *testing.T) {
	st := &fakeStore{}
	conv := &fakeConverter{err: errors.New("upstream 503")}
	h := newCallbackHandler(t, st, conv)
	rr := doCallback(t, h, "the-code", "match", "match")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rr.Code)
	}
	if st.saved != nil {
		t.Errorf("SaveAppConfig should not have been called: %+v", st.saved)
	}
}

func TestCallback_SaveError_500(t *testing.T) {
	st := &fakeStore{saveErr: errors.New("disk on fire")}
	conv := &fakeConverter{creds: &github.AppCredentials{Slug: "x"}}
	h := newCallbackHandler(t, st, conv)
	rr := doCallback(t, h, "the-code", "match", "match")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestCallback_ClearsStateCookie(t *testing.T) {
	conv := &fakeConverter{creds: &github.AppCredentials{Slug: "x"}}
	h := newCallbackHandler(t, &fakeStore{}, conv)
	rr := doCallback(t, h, "the-code", "match", "match")

	cleared := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookie && c.MaxAge < 0 {
			cleared = true
			if !c.Secure {
				t.Errorf("state cookie should be Secure when BaseURL is https")
			}
		}
	}
	if !cleared {
		t.Errorf("state cookie not cleared (MaxAge<0); cookies=%v", rr.Result().Cookies())
	}
}

func TestCallback_ClearsStateCookie_NotSecureOnHTTPBaseURL(t *testing.T) {
	conv := &fakeConverter{creds: &github.AppCredentials{Slug: "x"}}
	h := newCallbackHandler(t, &fakeStore{}, conv)
	h.Cfg.BaseURL = "http://example.test"
	rr := doCallback(t, h, "the-code", "match", "match")

	cleared := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookie && c.MaxAge < 0 {
			cleared = true
			if c.Secure {
				t.Errorf("state cookie should NOT be Secure when BaseURL is http")
			}
		}
	}
	if !cleared {
		t.Errorf("state cookie not cleared (MaxAge<0); cookies=%v", rr.Result().Cookies())
	}
}
