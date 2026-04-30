// Package github provides GitHub App authentication and API interactions.
package github

import (
	"net/http"
	"sync"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
)

// Client provides GitHub API operations via an installed App.
type Client struct {
	cfg        *config.Config
	http       *http.Client
	tokenCache sync.Map // installationID int64 -> cachedInstallationToken
	nowFn      func() time.Time
}

// NewClient creates a new GitHub client.
func NewClient(cfg *config.Config) *Client {
	return &Client{
		cfg:   cfg,
		http:  &http.Client{Timeout: 30 * time.Second},
		nowFn: time.Now,
	}
}
