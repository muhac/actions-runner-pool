package handlers

import (
	"log/slog"
	"net/http"
)

type DashboardHandler struct {
	Log *slog.Logger
}

func (h *DashboardHandler) Get(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTmpl.Execute(w, nil); err != nil {
		if h.Log != nil {
			h.Log.Error("render dashboard template", "error", err)
		}
	}
}
