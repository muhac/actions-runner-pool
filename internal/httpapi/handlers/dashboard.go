package handlers

import (
	"log/slog"
	"net/http"

	"github.com/muhac/actions-runner-pool/internal/config"
)

type DashboardHandler struct {
	// Cfg is consulted only for surface flags (AllowAdminEdit) the
	// template renders into the page. It may be nil in tests; when
	// nil, the template renders as if writes are disabled — the safer
	// default.
	Cfg *config.Config
	Log *slog.Logger
}

// dashboardData is the data passed to the dashboard template. New
// flags should be added here rather than read from globals so the
// rendered HTML stays a pure function of Cfg.
type dashboardData struct {
	AllowAdminEdit bool
}

func (h *DashboardHandler) Get(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := dashboardData{}
	if h.Cfg != nil {
		data.AllowAdminEdit = h.Cfg.AllowAdminEdit
	}
	if err := dashboardTmpl.Execute(w, data); err != nil {
		if h.Log != nil {
			h.Log.Error("render dashboard template", "error", err)
		}
	}
}
