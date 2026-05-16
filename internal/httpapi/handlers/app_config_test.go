package handlers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

// appCfgStore is the smallest in-memory store fake the rotation
// handler needs: GetAppConfig + SaveAppConfig. Other Store methods are
// satisfied by embedding the interface and panicking if reached
// (they shouldn't be).
type appCfgStore struct {
	store.Store
	mu       sync.Mutex
	current  *store.AppConfig
	getErr   error
	saveErr  error
	getCalls int
	saved    *store.AppConfig
	saveN    int
}

func (s *appCfgStore) GetAppConfig(_ context.Context) (*store.AppConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.current == nil {
		return nil, nil
	}
	cp := *s.current
	return &cp, nil
}

func (s *appCfgStore) SaveAppConfig(_ context.Context, c *store.AppConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	cp := *c
	s.saved = &cp
	s.current = &cp
	s.saveN++
	return nil
}

// generatePEM is a helper that mints a fresh RSA-2048 keypair as a PEM
// string. Tests need real PEMs because the handler validates via
// jwt.ParseRSAPrivateKeyFromPEM — using a stub string would fail
// the parse check.
func generatePEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(block))
}

func newRotateHandler(cfg *config.Config, st store.Store) *AppConfigHandler {
	return &AppConfigHandler{Cfg: cfg, Store: st}
}

func doPatch(t *testing.T, h *AppConfigHandler, body string, hdrs map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/admin/app-config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.Patch(rr, req)
	return rr
}

func seededStore(t *testing.T, pemStr string) *appCfgStore {
	t.Helper()
	return &appCfgStore{current: &store.AppConfig{
		AppID:         42,
		Slug:          "gharp-test",
		WebhookSecret: "secret-AAAAAAAAAAAAAAAA", // 24 chars, passes min
		PEM:           []byte(pemStr),
		ClientID:      "Iv1.original",
		ClientSecret:  "client-secret-original",
		BaseURL:       "https://example.test",
	}}
}

// 403 when ALLOW_ADMIN_EDIT is off, even with a valid bearer.
func TestAppConfig_Patch_ForbiddenWhenFlagOff(t *testing.T) {
	pemStr := generatePEM(t)
	st := seededStore(t, pemStr)
	h := newRotateHandler(&config.Config{AdminToken: "secret"}, st)

	rr := doPatch(t, h, `{"webhook_secret":"abcdefghij1234567890"}`, map[string]string{
		"Authorization": "Bearer secret",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", rr.Code)
	}
	if st.saveN != 0 {
		t.Fatalf("SaveAppConfig was called %d times despite forbidden", st.saveN)
	}
}

// 401 when flag is on but the bearer token is missing/wrong.
func TestAppConfig_Patch_UnauthorizedWhenBadToken(t *testing.T) {
	pemStr := generatePEM(t)
	st := seededStore(t, pemStr)
	h := newRotateHandler(&config.Config{AllowAdminEdit: true, AdminToken: "secret"}, st)

	rr := doPatch(t, h, `{"webhook_secret":"abcdefghij1234567890"}`, map[string]string{
		"Authorization": "Bearer wrong",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rr.Code)
	}
}

// Bearer-first ordering: bad token + flag off → 401 (not 403). Same
// contract as the jobs retry/cancel path; the dashboard relies on
// 401 to trigger its token-entry panel.
func TestAppConfig_Patch_UnauthorizedBeforeFlag(t *testing.T) {
	pemStr := generatePEM(t)
	st := seededStore(t, pemStr)
	// AdminToken set, flag off, bearer missing.
	h := newRotateHandler(&config.Config{AdminToken: "secret"}, st)

	rr := doPatch(t, h, `{"webhook_secret":"abcdefghij1234567890"}`, nil /* no Authorization */)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 (bearer-first)", rr.Code)
	}
}

// AdminToken empty → handler open (mirrors authorizedBearer's openness).
func TestAppConfig_Patch_OpenWhenAdminTokenEmpty(t *testing.T) {
	pemStr := generatePEM(t)
	st := seededStore(t, pemStr)
	h := newRotateHandler(&config.Config{AllowAdminEdit: true}, st)

	rr := doPatch(t, h, `{"webhook_secret":"abcdefghij1234567890"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", rr.Code, rr.Body.String())
	}
}

// 409 when app_config doesn't exist yet — operator must run /setup first.
func TestAppConfig_Patch_ConflictWhenNoExistingConfig(t *testing.T) {
	st := &appCfgStore{} // nil current
	h := newRotateHandler(&config.Config{AllowAdminEdit: true}, st)

	rr := doPatch(t, h, `{"webhook_secret":"abcdefghij1234567890"}`, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409", rr.Code)
	}
}

// 415 when content type isn't JSON.
func TestAppConfig_Patch_UnsupportedMediaType(t *testing.T) {
	pemStr := generatePEM(t)
	st := seededStore(t, pemStr)
	h := newRotateHandler(&config.Config{AllowAdminEdit: true}, st)

	req := httptest.NewRequest(http.MethodPatch, "/admin/app-config", strings.NewReader(`{"x":1}`))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	h.Patch(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d want 415", rr.Code)
	}
}

// 400 on empty body, no rotatable fields, short secret, bad pem.
func TestAppConfig_Patch_ValidationFailures(t *testing.T) {
	pemStr := generatePEM(t)

	cases := []struct {
		name string
		body string
	}{
		{"empty body", ``},
		{"empty object", `{}`},
		{"unknown field", `{"foo":"bar"}`},
		{"webhook_secret too short", `{"webhook_secret":"shortone"}`},
		{"webhook_secret only whitespace", `{"webhook_secret":"               "}`},
		{"pem all whitespace", `{"pem":"   \n\n   "}`},
		{"pem unparseable", `{"pem":"not a pem at all"}`},
		{"client_secret empty", `{"client_secret":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := seededStore(t, pemStr)
			h := newRotateHandler(&config.Config{AllowAdminEdit: true}, st)
			rr := doPatch(t, h, tc.body, nil)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400, body=%s", rr.Code, rr.Body.String())
			}
			if st.saveN != 0 {
				t.Fatalf("SaveAppConfig was called on a 400")
			}
		})
	}
}

// Happy path: each field individually rotates AND preserves untouched ones.
func TestAppConfig_Patch_RotatesSingleField(t *testing.T) {
	pemStr := generatePEM(t)
	newPEM := generatePEM(t)
	st := seededStore(t, pemStr)
	h := newRotateHandler(&config.Config{AllowAdminEdit: true}, st)

	newSecret := "rotated-AAAAAAAAAAAAAAAA"
	rr := doPatch(t, h, `{"webhook_secret":"`+newSecret+`"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	if st.saveN != 1 {
		t.Fatalf("SaveAppConfig calls=%d want 1", st.saveN)
	}
	saved := st.saved
	if saved.WebhookSecret != newSecret {
		t.Fatalf("WebhookSecret = %q, want %q", saved.WebhookSecret, newSecret)
	}
	// Untouched fields preserved.
	if string(saved.PEM) != pemStr {
		t.Fatalf("PEM should be untouched, got len=%d", len(saved.PEM))
	}
	if saved.ClientSecret != "client-secret-original" {
		t.Fatalf("ClientSecret should be untouched, got %q", saved.ClientSecret)
	}
	if saved.AppID != 42 || saved.Slug != "gharp-test" || saved.BaseURL != "https://example.test" {
		t.Fatalf("identity fields drifted: %+v", saved)
	}

	var resp patchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Rotated) != 1 || resp.Rotated[0] != "webhook_secret" {
		t.Fatalf("Rotated=%v want [webhook_secret]", resp.Rotated)
	}
	if !strings.HasPrefix(resp.WebhookSecretFingerprint, "sha256:") || len(resp.WebhookSecretFingerprint) != len("sha256:")+12 {
		t.Fatalf("unexpected fingerprint: %q", resp.WebhookSecretFingerprint)
	}

	// Now rotate just the PEM; ensure fingerprint changes and webhook_secret fingerprint stays stable.
	prevFP := resp.WebhookSecretFingerprint
	pemBody, _ := json.Marshal(map[string]string{"pem": newPEM})
	rr = doPatch(t, h, string(pemBody), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.WebhookSecretFingerprint != prevFP {
		t.Fatalf("webhook_secret fingerprint changed unexpectedly: %q vs %q", resp.WebhookSecretFingerprint, prevFP)
	}
	if len(resp.Rotated) != 1 || resp.Rotated[0] != "pem" {
		t.Fatalf("Rotated=%v want [pem]", resp.Rotated)
	}
}

// Multi-field rotation in one call.
func TestAppConfig_Patch_RotatesMultipleFields(t *testing.T) {
	pemStr := generatePEM(t)
	newPEM := generatePEM(t)
	st := seededStore(t, pemStr)
	h := newRotateHandler(&config.Config{AllowAdminEdit: true}, st)

	body, _ := json.Marshal(map[string]string{
		"webhook_secret": "rotated-AAAAAAAAAAAAAAAA",
		"pem":            newPEM,
		"client_secret":  "new-client-secret",
	})
	rr := doPatch(t, h, string(body), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	if st.saveN != 1 {
		t.Fatalf("SaveAppConfig calls=%d want 1", st.saveN)
	}
	var resp patchResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Rotated) != 3 {
		t.Fatalf("Rotated=%v want all three", resp.Rotated)
	}
}

// Same key with different line endings is not a rotation. Common
// when an operator copy-pastes a PEM from a Windows terminal or a
// browser textarea — the line endings become CRLF while the stored
// version is LF. Byte-exact comparison would mis-trigger a rotation
// + fingerprint change with no cryptographic difference. The fix is
// to compare parsed DER, which is line-ending-agnostic.
func TestAppConfig_Patch_NoOpForSamePEMWithDifferentLineEndings(t *testing.T) {
	pemStr := generatePEM(t) // LF endings from pem.EncodeToMemory
	st := seededStore(t, pemStr)
	h := newRotateHandler(&config.Config{AllowAdminEdit: true}, st)

	// Reconstitute the same key with CRLF line endings.
	pemCRLF := strings.ReplaceAll(pemStr, "\n", "\r\n")
	if pemCRLF == pemStr {
		t.Fatalf("test setup error: CRLF substitution did nothing")
	}

	body, _ := json.Marshal(map[string]string{"pem": pemCRLF})
	rr := doPatch(t, h, string(body), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if st.saveN != 0 {
		t.Fatalf("SaveAppConfig was called for CRLF-equivalent PEM (calls=%d)", st.saveN)
	}
	var resp patchResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Rotated) != 0 {
		t.Fatalf("Rotated=%v want empty for line-ending-only change", resp.Rotated)
	}
}

// No-op: posting the same secret that's already stored is a 200 with
// no SaveAppConfig call and an empty Rotated list. Useful for idempotent
// retry scripts.
func TestAppConfig_Patch_NoOpWhenValueUnchanged(t *testing.T) {
	pemStr := generatePEM(t)
	st := seededStore(t, pemStr)
	h := newRotateHandler(&config.Config{AllowAdminEdit: true}, st)

	rr := doPatch(t, h, `{"webhook_secret":"secret-AAAAAAAAAAAAAAAA"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if st.saveN != 0 {
		t.Fatalf("SaveAppConfig was called on no-op rotation (calls=%d)", st.saveN)
	}
	var resp patchResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Rotated) != 0 {
		t.Fatalf("Rotated=%v want empty", resp.Rotated)
	}
}

// 500 if the store breaks. Tests don't need GitHub mock — store
// fake is enough.
func TestAppConfig_Patch_StoreErrorReturns500(t *testing.T) {
	pemStr := generatePEM(t)
	st := seededStore(t, pemStr)
	st.saveErr = errors.New("disk on fire")
	h := newRotateHandler(&config.Config{AllowAdminEdit: true}, st)

	rr := doPatch(t, h, `{"webhook_secret":"rotated-AAAAAAAAAAAAAAAA"}`, nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rr.Code)
	}
}

func TestFingerprint_DistinctForDifferentInputs(t *testing.T) {
	a := fingerprint([]byte("alpha"))
	b := fingerprint([]byte("bravo"))
	if a == b {
		t.Fatalf("fingerprints collided: a=%s b=%s", a, b)
	}
	if !strings.HasPrefix(a, "sha256:") || len(a) != len("sha256:")+12 {
		t.Fatalf("unexpected format: %s", a)
	}
	if fingerprint(nil) != "" {
		t.Fatalf("empty input must produce empty fingerprint, not %q", fingerprint(nil))
	}
}
