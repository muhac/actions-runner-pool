package handlers

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/github"
	"github.com/muhac/actions-runner-pool/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

var (
	dashboardTmpl = template.Must(template.ParseFS(templatesFS, "templates/dashboard.html"))
	setupTmpl     = template.Must(template.ParseFS(templatesFS, "templates/setup.html"))
	setupDoneTmpl = template.Must(template.ParseFS(templatesFS, "templates/setup_done.html"))
)

const (
	stateCookie    = "gharp_state"
	stateCookieTTL = 10 * time.Minute
)

type SetupHandler struct {
	Cfg   *config.Config
	Store store.Store
	Log   *slog.Logger
}

// GET /setup
//   - If app_config exists and BaseURL matches, render setup_done with the
//     install link (so users can install on additional accounts).
//   - Otherwise render the manifest creation form with a fresh state cookie.
func (h *SetupHandler) Get(w http.ResponseWriter, r *http.Request) {
	existing, err := h.Store.GetAppConfig(r.Context())
	if err != nil {
		h.logError("load app config", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if existing != nil && existing.BaseURL == h.Cfg.BaseURL {
		h.renderDone(w, existing.Slug)
		return
	}

	state, err := randomState()
	if err != nil {
		h.logError("generate state", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/github/app/callback",
		HttpOnly: true,
		Secure:   strings.HasPrefix(h.Cfg.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(stateCookieTTL.Seconds()),
	})

	manifestJSON, err := json.Marshal(github.BuildManifest(h.Cfg.BaseURL))
	if err != nil {
		h.logError("marshal manifest", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := setupTmpl.Execute(w, struct {
		BaseURL      string
		State        string
		ManifestJSON string
	}{
		BaseURL:      h.Cfg.BaseURL,
		State:        state,
		ManifestJSON: string(manifestJSON),
	}); err != nil {
		h.logError("render setup template", err)
	}
}

func (h *SetupHandler) renderDone(w http.ResponseWriter, slug string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := setupDoneTmpl.Execute(w, struct {
		Slug        string
		SettingsURL string
		InstallURL  string
	}{
		Slug:        slug,
		SettingsURL: "https://github.com/settings/apps/" + slug,
		InstallURL:  "https://github.com/apps/" + slug + "/installations/new",
	}); err != nil {
		h.logError("render setup_done template", err)
	}
}

func (h *SetupHandler) logError(msg string, err error) {
	if h.Log != nil {
		h.Log.Error(msg, "error", err)
	}
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
