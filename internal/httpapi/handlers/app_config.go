package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/store"
)

// minWebhookSecretLength is a loose floor — GitHub-generated secrets
// are ~40 hex chars, but we accept any reasonably long opaque string
// so operators rotating to externally-generated material aren't
// surprised. Anything shorter than 16 is almost certainly a mistake.
const minWebhookSecretLength = 16

// appConfigBodyLimit caps the inbound JSON to defend against malicious
// or accidentally large bodies. PEMs are typically 1.6 KiB; 64 KiB is
// comfortable headroom and still tiny.
const appConfigBodyLimit = 64 * 1024

// AppConfigHandler serves the admin-only credential rotation endpoint.
// It exists separately from CallbackHandler (which does the initial
// manifest-flow setup) so the rotation surface is distinct in tests
// and in routing.
type AppConfigHandler struct {
	Cfg   *config.Config
	Store store.Store
	Log   *slog.Logger
}

// patchRequest is the JSON body accepted by PATCH /admin/app-config.
// Each field is a pointer so we can distinguish "absent" (untouched)
// from "empty string" (validation error). All fields are optional;
// at least one must be present.
type patchRequest struct {
	WebhookSecret *string `json:"webhook_secret,omitempty"`
	PEM           *string `json:"pem,omitempty"`
	ClientSecret  *string `json:"client_secret,omitempty"`
}

// patchResponse is what clients get back on a successful rotation.
// Fingerprints are sha256 prefixes (12 hex chars, 48 bits) — enough
// for an operator to confirm which version is live without exposing
// the secret itself.
type patchResponse struct {
	Rotated                  []string `json:"rotated"`
	WebhookSecretFingerprint string   `json:"webhook_secret_fingerprint"`
	PEMFingerprint           string   `json:"pem_fingerprint"`
	ClientSecretFingerprint  string   `json:"client_secret_fingerprint"`
}

// Patch is the PATCH /admin/app-config handler. See plan/eager-cuddling-sloth.md
// "Approach (B)" for the response-code matrix; the short version:
// 401=bad bearer, 403=writes disabled, 409=no setup yet, 400=bad
// values, 415=wrong Content-Type, 200=ok.
func (h *AppConfigHandler) Patch(w http.ResponseWriter, r *http.Request) {
	if status := adminWriteDenied(h.Cfg, r.Header.Get("Authorization")); status != 0 {
		writeAdminAuthError(w, status)
		return
	}

	ct := r.Header.Get("Content-Type")
	if mt := strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]); !strings.EqualFold(mt, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	body := http.MaxBytesReader(w, r.Body, appConfigBodyLimit)
	defer func() { _ = body.Close() }()
	var req patchRequest
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		// http.MaxBytesReader surfaces oversize as a typed error; the
		// other common case is a JSON syntax error. Both deserve a 400
		// with the error string — leaking it back is fine because the
		// caller is admin-authed already.
		if errors.Is(err, io.EOF) {
			http.Error(w, "request body must be JSON object", http.StatusBadRequest)
			return
		}
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.WebhookSecret == nil && req.PEM == nil && req.ClientSecret == nil {
		http.Error(w, "no rotatable fields provided; supply at least one of webhook_secret, pem, client_secret", http.StatusBadRequest)
		return
	}

	existing, err := h.Store.GetAppConfig(r.Context())
	if err != nil {
		h.logError("get app config", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if existing == nil {
		http.Error(w, "app_config is not set; run /setup first", http.StatusConflict)
		return
	}

	merged := *existing
	rotated := make([]string, 0, 3)

	if req.WebhookSecret != nil {
		v := strings.TrimSpace(*req.WebhookSecret)
		if len(v) < minWebhookSecretLength {
			http.Error(w, fmt.Sprintf("webhook_secret must be at least %d chars after trim", minWebhookSecretLength), http.StatusBadRequest)
			return
		}
		if v != existing.WebhookSecret {
			merged.WebhookSecret = v
			rotated = append(rotated, "webhook_secret")
		}
	}

	if req.PEM != nil {
		// Don't trim PEM — the surrounding whitespace and newlines are
		// part of the format. Reject only the all-whitespace case.
		if strings.TrimSpace(*req.PEM) == "" {
			http.Error(w, "pem must not be empty", http.StatusBadRequest)
			return
		}
		if _, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(*req.PEM)); err != nil {
			http.Error(w, "pem parse failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		newPEM := []byte(*req.PEM)
		if string(newPEM) != string(existing.PEM) {
			merged.PEM = newPEM
			rotated = append(rotated, "pem")
		}
	}

	if req.ClientSecret != nil {
		v := strings.TrimSpace(*req.ClientSecret)
		if v == "" {
			http.Error(w, "client_secret must not be empty", http.StatusBadRequest)
			return
		}
		if v != existing.ClientSecret {
			merged.ClientSecret = v
			rotated = append(rotated, "client_secret")
		}
	}

	if len(rotated) > 0 {
		if err := h.Store.SaveAppConfig(r.Context(), &merged); err != nil {
			h.logError("save app config", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		h.logInfo("admin/app-config rotated", "fields", rotated)
	}

	writeJSON(w, patchResponse{
		Rotated:                  rotated,
		WebhookSecretFingerprint: fingerprint([]byte(merged.WebhookSecret)),
		PEMFingerprint:           fingerprint(merged.PEM),
		ClientSecretFingerprint:  fingerprint([]byte(merged.ClientSecret)),
	})
}

// fingerprint returns the first 12 hex chars of sha256(v) — 48 bits, plenty
// for an operator to disambiguate which version is live without exposing
// the secret. Empty input → empty fingerprint (not "all zeros") so absent
// fields are visually distinct from present-but-newly-rotated ones.
func fingerprint(v []byte) string {
	if len(v) == 0 {
		return ""
	}
	sum := sha256.Sum256(v)
	return "sha256:" + hex.EncodeToString(sum[:6])
}

func (h *AppConfigHandler) logError(msg string, err error) {
	if h.Log != nil {
		h.Log.Error(msg, "error", err)
	}
}

func (h *AppConfigHandler) logInfo(msg string, args ...any) {
	if h.Log != nil {
		h.Log.Info(msg, args...)
	}
}
