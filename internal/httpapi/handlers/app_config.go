package handlers

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
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
		// http.MaxBytesReader surfaces oversize as a typed
		// *http.MaxBytesError that the JSON decoder wraps. Match it
		// first so we can return 413 (matches what docs advertise and
		// what an HTTP client expects for "body too large"), distinct
		// from the generic 400 for syntax errors / EOF.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
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

	// Per-field UPDATE rather than read-modify-write SaveAppConfig:
	// two concurrent rotations of *different* fields could otherwise
	// each start from a stale snapshot and the later-committing
	// request would silently undo the earlier one. Each field below
	// dispatches a single-column UPDATE; sqlite serialises writes
	// per-statement so the two rotations interleave cleanly.
	//
	// The existing snapshot is still consulted for the no-op
	// fast-path (skip the write when the caller's value matches what
	// we just read). That decision is racy under concurrent writes
	// to the *same* field — the loser may observe "no change" when
	// in fact their value differs — but the only consequence is a
	// redundant 200 with rotated=[] rather than a clobber.
	rotated := make([]string, 0, 3)
	finalWebhookSecret := []byte(existing.WebhookSecret)
	finalPEM := existing.PEM
	finalClientSecret := []byte(existing.ClientSecret)

	if req.WebhookSecret != nil {
		v := strings.TrimSpace(*req.WebhookSecret)
		if len(v) < minWebhookSecretLength {
			http.Error(w, fmt.Sprintf("webhook_secret must be at least %d chars after trim", minWebhookSecretLength), http.StatusBadRequest)
			return
		}
		if v != existing.WebhookSecret {
			if err := h.Store.UpdateAppConfigWebhookSecret(r.Context(), v); err != nil {
				h.handleUpdateErr(w, "update webhook_secret", err)
				return
			}
			rotated = append(rotated, "webhook_secret")
			finalWebhookSecret = []byte(v)
		}
	}

	if req.PEM != nil {
		// Don't trim PEM — the surrounding whitespace and newlines are
		// part of the format. Reject only the all-whitespace case.
		if strings.TrimSpace(*req.PEM) == "" {
			http.Error(w, "pem must not be empty", http.StatusBadRequest)
			return
		}
		newKey, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(*req.PEM))
		if err != nil {
			http.Error(w, "pem parse failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Compare parsed DER, not raw PEM bytes. Pasting the same key
		// with CRLF line endings (browser textarea on Windows, copy from
		// a Windows terminal) would otherwise look like a rotation even
		// though the underlying private key is identical — triggering a
		// pointless DB write, an audit-log entry, and a fingerprint
		// change that misleads the operator into thinking they
		// successfully rotated something cryptographic.
		newDER := x509.MarshalPKCS1PrivateKey(newKey)
		var existingDER []byte
		if oldKey, parseErr := jwt.ParseRSAPrivateKeyFromPEM(existing.PEM); parseErr == nil {
			existingDER = x509.MarshalPKCS1PrivateKey(oldKey)
		}
		if !bytes.Equal(newDER, existingDER) {
			newPEM := []byte(*req.PEM)
			if err := h.Store.UpdateAppConfigPEM(r.Context(), newPEM); err != nil {
				h.handleUpdateErr(w, "update pem", err)
				return
			}
			rotated = append(rotated, "pem")
			finalPEM = newPEM
		}
	}

	if req.ClientSecret != nil {
		v := strings.TrimSpace(*req.ClientSecret)
		if v == "" {
			http.Error(w, "client_secret must not be empty", http.StatusBadRequest)
			return
		}
		if v != existing.ClientSecret {
			if err := h.Store.UpdateAppConfigClientSecret(r.Context(), v); err != nil {
				h.handleUpdateErr(w, "update client_secret", err)
				return
			}
			rotated = append(rotated, "client_secret")
			finalClientSecret = []byte(v)
		}
	}

	// Log every authenticated PATCH attempt, including no-ops where
	// rotated is empty. Without this, an attacker probing with the
	// current value of a secret would leave no audit trace because
	// the no-op fast-path skipped the log line.
	h.logInfo("admin/app-config patch", "fields", rotated)

	writeJSON(w, patchResponse{
		Rotated:                  rotated,
		WebhookSecretFingerprint: fingerprint(finalWebhookSecret),
		PEMFingerprint:           fingerprint(finalPEM),
		ClientSecretFingerprint:  fingerprint(finalClientSecret),
	})
}

// handleUpdateErr maps a per-field UPDATE failure to an HTTP response.
// ErrNoAppConfig is only reachable if the app_config row was deleted
// between our GetAppConfig snapshot and the per-field UPDATE — vanishingly
// rare, but surface it as 409 to match the snapshot-was-nil path so the
// operator response is consistent.
func (h *AppConfigHandler) handleUpdateErr(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, store.ErrNoAppConfig) {
		http.Error(w, "app_config is not set; run /setup first", http.StatusConflict)
		return
	}
	h.logError(op, err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
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
