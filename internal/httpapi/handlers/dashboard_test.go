package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboard_RendersPageShell(t *testing.T) {
	h := &DashboardHandler{}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`id="app"`,
		`id="authForm"`,
		`type="submit"`,
		`authForm.addEventListener("submit"`,
		`/stats`,
		`/jobs`,
		`sessionStorage`,
		`retry`,
		`cancel`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard body missing %q", want)
		}
	}
}
