package handlers

import (
	"log/slog"
	"net/http"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

type SetupHandler struct {
	Cfg   *config.Config
	Store store.Store
	Log   *slog.Logger
}

// GET /setup — render the manifest creation form (or "already configured" page).
// TODO: render web/templates/setup.html with manifest JSON + state cookie.
func (h *SetupHandler) Get(w http.ResponseWriter, r *http.Request) {
	existing, err := h.Store.GetAppConfig(r.Context())
	if err != nil {
		if h.Log != nil {
			h.Log.Error("failed to load app config", "error", err)
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if existing != nil {
		http.Error(w, "already configured", http.StatusConflict)
		return
	}
	http.Error(w, "setup form not yet implemented", http.StatusNotImplemented)
}
