package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

func TestRouter_JobsRoute(t *testing.T) {
	st, err := store.OpenSQLite("file:" + t.TempDir() + "/router-test.db")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := NewRouter(&config.Config{}, st, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}
