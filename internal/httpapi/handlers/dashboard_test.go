package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/config"
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
		`Rotate credentials`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard body missing %q", want)
		}
	}
}

// When ALLOW_ADMIN_EDIT is false (the default), the dashboard must
// render the read-only banner AND render each rotate form's submit
// button with the disabled attribute. The server-side disabled state
// is what protects an operator from filling in a form and submitting
// it only to learn at the network layer that writes are off.
func TestDashboard_ReadonlyBannerWhenFlagOff(t *testing.T) {
	h := &DashboardHandler{Cfg: &config.Config{AllowAdminEdit: false}}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "Admin writes are disabled") {
		t.Fatalf("expected read-only banner; body=%q", body[:min(400, len(body))])
	}
	if !strings.Contains(body, `ALLOW_ADMIN_EDIT=true`) {
		t.Fatal("banner should mention ALLOW_ADMIN_EDIT=true")
	}
	// The three rotate buttons should be rendered disabled.
	if strings.Count(body, `type="submit" disabled`) < 3 {
		t.Fatalf("expected 3 disabled rotate buttons, got body=%q", body[strings.Index(body, "Rotate credentials"):min(len(body), strings.Index(body, "Rotate credentials")+2000)])
	}
	if !strings.Contains(body, `const allowAdminEdit = false`) {
		t.Fatal("JS allowAdminEdit flag should be false")
	}
}

func TestDashboard_NoBannerWhenFlagOn(t *testing.T) {
	h := &DashboardHandler{Cfg: &config.Config{AllowAdminEdit: true}}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	body := rr.Body.String()
	if strings.Contains(body, "Admin writes are disabled") {
		t.Fatal("read-only banner should not render when flag is on")
	}
	if strings.Contains(body, `type="submit" disabled`) {
		t.Fatal("rotate buttons should not be disabled when flag is on")
	}
	if !strings.Contains(body, `const allowAdminEdit = true`) {
		t.Fatal("JS allowAdminEdit flag should be true")
	}
}
