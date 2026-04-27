package github

import (
	"net/http"
	"sync"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
)

type Client struct {
	cfg        *config.Config
	http       *http.Client
	tokenCache sync.Map // installationID int64 -> cachedInstallationToken
}

func NewClient(cfg *config.Config) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}
