package handlers

import (
	"context"
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
	if existing, _ := h.Store.GetAppConfig(context.Background()); existing != nil {
		http.Error(w, "already configured", http.StatusConflict)
		return
	}
	http.Error(w, "setup form not yet implemented", http.StatusNotImplemented)
}
