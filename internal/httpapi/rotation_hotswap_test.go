package httpapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

// TestRotation_WebhookSecretHotSwap exercises the no-restart hot-swap
// property of PATCH /admin/app-config end-to-end through the real
// router + real sqlite:
//
//  1. Send a webhook signed with secret A → 200.
//  2. PATCH new secret B via the endpoint.
//  3. Send a webhook signed with B → 200.
//  4. Replay the original A-signed payload → 401 (signature mismatch).
//
// Crucially this exercises the live read path in
// `handlers/webhook.go:GetAppConfig` — there is no in-memory cache
// to invalidate, so the rotation is visible to the very next webhook.
func TestRotation_WebhookSecretHotSwap(t *testing.T) {
	const secretA = "secretA-AAAAAAAAAAAAAAAA" // ≥ 16 chars
	const secretB = "secretB-BBBBBBBBBBBBBBBB"

	st, err := store.OpenSQLite("file:" + t.TempDir() + "/rotate.db?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Seed app_config with secret A and a real PEM (router PATCH path
	// won't be exercised for PEM here, but we need a non-empty PEM so
	// the row passes any future validation).
	pemStr := freshPEM(t)
	if err := st.SaveAppConfig(t.Context(), &store.AppConfig{
		AppID: 1, Slug: "gharp-hotswap-test",
		WebhookSecret: secretA, PEM: []byte(pemStr),
		ClientID: "Iv1.test", BaseURL: "http://127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		AdminToken:     "admintok",
		AllowAdminEdit: true,
	}
	h := NewRouter(cfg, st, nil, nil, nil)

	// Step 1: webhook signed with A passes.
	body := []byte(`{"zen":"hello"}`)
	if code := postWebhook(t, h, body, secretA); code != http.StatusOK {
		t.Fatalf("pre-rotation webhook with secret A: status=%d want 200", code)
	}

	// Step 2: PATCH new secret B.
	patchBody := `{"webhook_secret":"` + secretB + `"}`
	req := httptest.NewRequest(http.MethodPatch, "/admin/app-config", strings.NewReader(patchBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer admintok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH /admin/app-config: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Rotated []string `json:"rotated"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Rotated) != 1 || resp.Rotated[0] != "webhook_secret" {
		t.Fatalf("rotated=%v want [webhook_secret]", resp.Rotated)
	}

	// Step 3: webhook signed with B passes — the hot-swap took effect
	// without a restart.
	if code := postWebhook(t, h, body, secretB); code != http.StatusOK {
		t.Fatalf("post-rotation webhook with secret B: status=%d want 200", code)
	}

	// Step 4: replaying the original A-signed payload now fails — proves
	// the old secret is no longer accepted.
	if code := postWebhook(t, h, body, secretA); code != http.StatusUnauthorized {
		t.Fatalf("replay with old secret A: status=%d want 401", code)
	}

	// Step 5: a malformed signature header (missing the "sha256="
	// prefix that the webhook handler expects) is also rejected.
	// This guards the negative side of the post-rotation signature
	// path: bad headers stay 401 regardless of which secret is live.
	mac := hmac.New(sha256.New, []byte(secretB))
	mac.Write(body)
	rawDigest := hex.EncodeToString(mac.Sum(nil)) // intentionally missing "sha256=" prefix
	badReq := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	badReq.Header.Set("Content-Type", "application/json")
	badReq.Header.Set("X-GitHub-Event", "ping")
	badReq.Header.Set("X-Hub-Signature-256", rawDigest)
	badRR := httptest.NewRecorder()
	h.ServeHTTP(badRR, badReq)
	if badRR.Code != http.StatusUnauthorized {
		t.Fatalf("malformed signature post-rotation: status=%d want 401", badRR.Code)
	}
}

// TestRotation_PatchDeniedWhenFlagOff confirms the kill-switch wins
// over a valid bearer for the rotation endpoint, end-to-end through
// the router.
func TestRotation_PatchDeniedWhenFlagOff(t *testing.T) {
	st, err := store.OpenSQLite("file:" + t.TempDir() + "/denied.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pemStr := freshPEM(t)
	if err := st.SaveAppConfig(t.Context(), &store.AppConfig{
		AppID: 1, Slug: "gharp", WebhookSecret: "AAAAAAAAAAAAAAAA",
		PEM: []byte(pemStr), BaseURL: "http://127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}
	// AllowAdminEdit: false (default).
	h := NewRouter(&config.Config{AdminToken: "admintok"}, st, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPatch, "/admin/app-config",
		strings.NewReader(`{"webhook_secret":"BBBBBBBBBBBBBBBB"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer admintok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", rr.Code)
	}

	// And the old secret still verifies signatures.
	if code := postWebhook(t, h, []byte(`{"zen":"hi"}`), "AAAAAAAAAAAAAAAA"); code != http.StatusOK {
		t.Fatalf("post-denied webhook: status=%d want 200", code)
	}
}

func postWebhook(t *testing.T, h http.Handler, body []byte, secret string) int {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", sig)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

// TestRotation_ConcurrentDifferentFields_NoLostUpdate is the
// regression test for the lost-update race in the read-modify-write
// version of the handler. Two concurrent PATCH requests against the
// real router + real sqlite, each rotating a different field. After
// both return, both new values must be persisted — neither rotation
// is allowed to silently undo the other.
func TestRotation_ConcurrentDifferentFields_NoLostUpdate(t *testing.T) {
	originalSecret := "originalAAAAAAAAAAAAAAAA"
	originalClient := "client-original"
	pemStr := freshPEM(t)

	st, err := store.OpenSQLite("file:" + t.TempDir() + "/concurrent.db?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveAppConfig(t.Context(), &store.AppConfig{
		AppID: 1, Slug: "concurrent-test",
		WebhookSecret: originalSecret, PEM: []byte(pemStr),
		ClientID: "Iv1.test", ClientSecret: originalClient,
		BaseURL: "http://127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{AdminToken: "admintok", AllowAdminEdit: true}
	h := NewRouter(cfg, st, nil, nil, nil)

	const newSecret = "rotatedAAAAAAAAAAAAAAAAAA"
	const newClient = "client-rotated"

	doPatch := func(body string) int {
		req := httptest.NewRequest(http.MethodPatch, "/admin/app-config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer admintok")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	// Two goroutines, each rotating a different field. Launching them
	// in parallel and waiting on a barrier maximises the window where
	// both have read the same pre-rotation snapshot.
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		if code := doPatch(`{"webhook_secret":"` + newSecret + `"}`); code != http.StatusOK {
			t.Errorf("webhook PATCH status=%d want 200", code)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		if code := doPatch(`{"client_secret":"` + newClient + `"}`); code != http.StatusOK {
			t.Errorf("client PATCH status=%d want 200", code)
		}
	}()
	close(start)
	wg.Wait()

	got, err := st.GetAppConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.WebhookSecret != newSecret {
		t.Errorf("WebhookSecret = %q, want %q (lost-update on webhook side)", got.WebhookSecret, newSecret)
	}
	if got.ClientSecret != newClient {
		t.Errorf("ClientSecret = %q, want %q (lost-update on client side)", got.ClientSecret, newClient)
	}
	// PEM should be untouched.
	if string(got.PEM) != pemStr {
		t.Errorf("PEM changed despite neither request touching it")
	}
}

func freshPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}
