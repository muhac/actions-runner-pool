package github

import (
	"net/http"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
)

type Client struct {
	cfg  *config.Config
	http *http.Client
}

func NewClient(cfg *config.Config) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}
