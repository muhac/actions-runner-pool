package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"

	"github.com/muhac/actions-runner-pool/internal/config"
)

func newTestPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
}

func newTestClient(t *testing.T, base string) *Client {
	t.Helper()
	return NewClient(&config.Config{GitHubAPIBase: base})
}

func TestAppJWT_HeaderClaims(t *testing.T) {
	c := newTestClient(t, "")
	signed, err := c.AppJWT(newTestPEM(t), 12345)
	if err != nil {
		t.Fatalf("AppJWT: %v", err)
	}
	parser := jwtv5.NewParser()
	tok, _, err := parser.ParseUnverified(signed, jwtv5.MapClaims{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tok.Method.Alg() != "RS256" {
		t.Errorf("alg = %s, want RS256", tok.Method.Alg())
	}
	claims := tok.Claims.(jwtv5.MapClaims)
	if iss, _ := claims["iss"].(string); iss != "12345" {
		t.Errorf("iss = %q, want 12345", iss)
	}
	iat, _ := claims["iat"].(float64)
	exp, _ := claims["exp"].(float64)
	if d := exp - iat; d < 9*60 || d > 12*60 {
		t.Errorf("exp-iat = %v sec, want ~10min (with 60s skew)", d)
	}
}

// installationTokenServer returns an httptest server that hands out unique
// installation tokens with the configured expires_at.
type installationTokenServer struct {
	hits      atomic.Int64
	expiresAt time.Time
}

func newInstallationTokenServer(t *testing.T, expiresAt time.Time) (*httptest.Server, *installationTokenServer) {
	t.Helper()
	state := &installationTokenServer{expiresAt: expiresAt}
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		state.hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "tok-" + strconv.FormatInt(state.hits.Load(), 10),
			"expires_at": state.expiresAt,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, state
}

func resetInstallationTokenCache(t *testing.T) {
	t.Helper()
	installationTokenCache.Range(func(k, _ any) bool {
		installationTokenCache.Delete(k)
		return true
	})
	t.Cleanup(func() {
		installationTokenCache.Range(func(k, _ any) bool {
			installationTokenCache.Delete(k)
			return true
		})
	})
}

func TestInstallationToken_CachesUntilExpiry(t *testing.T) {
	resetInstallationTokenCache(t)
	srv, state := newInstallationTokenServer(t, time.Now().Add(1*time.Hour))
	c := newTestClient(t, srv.URL)

	tok1, err := c.InstallationToken(context.Background(), "fake-jwt", 7)
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := c.InstallationToken(context.Background(), "fake-jwt", 7)
	if err != nil {
		t.Fatal(err)
	}
	if tok1 != tok2 {
		t.Errorf("expected cached token reuse, got %q != %q", tok1, tok2)
	}
	if h := state.hits.Load(); h != 1 {
		t.Errorf("server hits = %d, want 1 (cache miss only)", h)
	}
}

func TestInstallationToken_RefreshAfterMargin(t *testing.T) {
	resetInstallationTokenCache(t)
	// expires_at = now + 4 min; effective TTL = 4min - 5min margin = -1min,
	// so the very first cached entry is already expired by our policy.
	srv, state := newInstallationTokenServer(t, time.Now().Add(4*time.Minute))
	c := newTestClient(t, srv.URL)

	if _, err := c.InstallationToken(context.Background(), "fake-jwt", 9); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InstallationToken(context.Background(), "fake-jwt", 9); err != nil {
		t.Fatal(err)
	}
	if h := state.hits.Load(); h != 2 {
		t.Errorf("server hits = %d, want 2 (cache miss twice — entry expired by margin)", h)
	}
}

func TestInstallationToken_Non2xxIsError(t *testing.T) {
	resetInstallationTokenCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	if _, err := c.InstallationToken(context.Background(), "fake-jwt", 1); err == nil {
		t.Fatalf("expected error on 401")
	}
}
