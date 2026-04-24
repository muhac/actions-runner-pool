package handlers

import (
	"log/slog"
	"net/http"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/github"
	"github.com/muhac/actions-runner-pool/internal/store"
)

type CallbackHandler struct {
	Cfg    *config.Config
	Store  store.Store
	GitHub *github.Client
	Log    *slog.Logger
}

// GET /github/app/callback?code=<temp>&state=<state>
// Verify state cookie, exchange code for App credentials, persist, render done page.
func (h *CallbackHandler) Get(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "callback not yet implemented", http.StatusNotImplemented)
}
