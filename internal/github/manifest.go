package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	HookAttributes     map[string]string `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

type AppCredentials struct {
	AppID         int64
	Slug          string
	WebhookSecret string
	PEM           []byte
	ClientID      string
	ClientSecret  string
}

func BuildManifest(baseURL string) Manifest {
	return Manifest{
		Name: "gharp-runners",
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
		DefaultEvents: []string{"workflow_job", "installation", "installation_repositories"},
	}
}

// ConvertCode exchanges the temporary `code` from the manifest flow callback
// for the full App credentials. POST /app-manifests/{code}/conversions.
//
// The single-use code IS the credential; no auth header is sent. Code expires
// in ~10 min so callers must invoke immediately and not retry.
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
