package github

import (
	"context"
	"errors"
)

type Manifest struct {
	Name               string                 `json:"name"`
	URL                string                 `json:"url"`
	HookAttributes     map[string]string      `json:"hook_attributes"`
	RedirectURL        string                 `json:"redirect_url"`
	Public             bool                   `json:"public"`
	DefaultPermissions map[string]string      `json:"default_permissions"`
	DefaultEvents      []string               `json:"default_events"`
}

type AppCredentials struct {
	ID            int64
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
func (c *Client) ConvertCode(ctx context.Context, code string) (*AppCredentials, error) {
	return nil, errors.New("ConvertCode: not implemented")
}
