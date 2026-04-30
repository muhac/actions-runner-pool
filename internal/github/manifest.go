package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Manifest represents a GitHub App manifest.
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	HookAttributes     map[string]string `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

// AppCredentials holds GitHub App credentials from the manifest exchange.
type AppCredentials struct {
	AppID         int64
	Slug          string
	WebhookSecret string
	PEM           []byte
	ClientID      string
	ClientSecret  string
}

// BuildManifest creates a GitHub App manifest for the given base URL.
func BuildManifest(baseURL string) Manifest {
	return Manifest{
		// GitHub App names are globally unique; suffix the BaseURL hash so
		// two unrelated gharp deployments don't collide. Users can rename
		// in the GitHub UI; the slug returned by ConvertCode is what we
		// actually use afterwards.
		Name: "gharp-" + nameSuffix(baseURL),
		URL:  baseURL,
		HookAttributes: map[string]string{
			"url": baseURL + "/github/webhook",
		},
		RedirectURL: baseURL + "/github/app/callback",
		Public:      false,
		DefaultPermissions: map[string]string{
			"administration": "write",
			"actions":        "read",
			"metadata":       "read",
		},
		// installation / installation_repositories are NOT listed here:
		// GitHub treats them as built-in App lifecycle events that fire
		// automatically once the App is installed. Including them makes
		// the manifest fail validation ("Default events unsupported").
		// They still arrive at our webhook and we still handle them.
		DefaultEvents: []string{"workflow_job"},
	}
}

// nameSuffix returns the first 6 hex chars of sha256(baseURL) — short enough
// to keep the App name readable and unique enough to not collide.
func nameSuffix(baseURL string) string {
	h := sha256.Sum256([]byte(baseURL))
	return hex.EncodeToString(h[:])[:6]
}

// ConvertCode exchanges a temporary installation code for App credentials.
// Calls POST /app-manifests/{code}/conversions with no auth header.
// The code is single-use and expires in ~10 minutes.
func (c *Client) ConvertCode(ctx context.Context, code string) (*AppCredentials, error) {
	endpoint := fmt.Sprintf("%s/app-manifests/%s/conversions", c.cfg.GitHubAPIBase, url.PathEscape(code))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("manifest convert: status %d", resp.StatusCode)
	}
	var body struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		WebhookSecret string `json:"webhook_secret"`
		PEM           string `json:"pem"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode manifest response: %w", err)
	}
	return &AppCredentials{
		AppID:         body.ID,
		Slug:          body.Slug,
		WebhookSecret: body.WebhookSecret,
		PEM:           []byte(body.PEM),
		ClientID:      body.ClientID,
		ClientSecret:  body.ClientSecret,
	}, nil
}
