package handlers

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/github"
	"github.com/muhac/actions-runner-pool/internal/store"
)

// codeConverter is the subset of *github.Client the callback handler needs;
// extracted so tests can pass a fake without standing up an httptest server.
type codeConverter interface {
	ConvertCode(ctx context.Context, code string) (*github.AppCredentials, error)
}

// CallbackHandler handles GitHub OAuth callback during app installation.
type CallbackHandler struct {
	Cfg    *config.Config
	Store  store.Store
	GitHub codeConverter
	Log    *slog.Logger
}

// Get handles the OAuth callback from GitHub app installation.
func (h *CallbackHandler) Get(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" || code == "" {
		http.Error(w, "missing state or code query param", http.StatusBadRequest)
		return
	}

	ck, err := r.Cookie(stateCookie)
	if err != nil || ck.Value == "" {
		http.Error(w, "missing state cookie", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(ck.Value)) != 1 {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}

	creds, err := h.GitHub.ConvertCode(r.Context(), code)
	if err != nil {
		h.logError("convert manifest code", err)
		http.Error(w, "manifest conversion failed", http.StatusInternalServerError)
		return
	}
	if creds == nil {
		h.logError("convert manifest code", errors.New("nil credentials"))
		http.Error(w, "manifest conversion failed", http.StatusInternalServerError)
		return
	}

	if err := h.Store.SaveAppConfig(r.Context(), &store.AppConfig{
		AppID:         creds.AppID,
		Slug:          creds.Slug,
		WebhookSecret: creds.WebhookSecret,
		PEM:           creds.PEM,
		ClientID:      creds.ClientID,
		ClientSecret:  creds.ClientSecret,
		BaseURL:       h.Cfg.BaseURL,
	}); err != nil {
		h.logError("persist app config", err)
		http.Error(w, "failed to persist app config", http.StatusInternalServerError)
		return
	}

	// Clear the state cookie — single-use.
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    "",
		Path:     "/github/app/callback",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
	})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := setupDoneTmpl.Execute(w, struct {
		Slug        string
		SettingsURL string
		InstallURL  string
	}{
		Slug:        creds.Slug,
		SettingsURL: "https://github.com/settings/apps/" + creds.Slug,
		InstallURL:  "https://github.com/apps/" + creds.Slug + "/installations/new",
	}); err != nil {
		h.logError("render setup_done template", err)
	}
}

func (h *CallbackHandler) logError(msg string, err error) {
	if h.Log != nil {
		h.Log.Error(msg, "error", err)
	}
}
