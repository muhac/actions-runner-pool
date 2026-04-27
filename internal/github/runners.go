package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// RegistrationToken mints a single-use runner registration token for a repo.
// POST /repos/{owner}/{repo}/actions/runners/registration-token.
//
// Tokens are single-use under EPHEMERAL=1; never cache.
func (c *Client) RegistrationToken(ctx context.Context, installationToken, repoFullName string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/actions/runners/registration-token", c.cfg.GitHubAPIBase, repoFullName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+installationToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("registration token: status %d", resp.StatusCode)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode registration token: %w", err)
	}
	if body.Token == "" {
		return "", errors.New("registration token: empty token in response")
	}
	return body.Token, nil
}

type RepoRunner struct {
	ID     int64
	Name   string
	Status string // online, offline
	Busy   bool
	Labels []string
}

// ListRepoRunners is used by the reconciliation loop to detect ghost runners.
// Deferred to v1.1.
func (c *Client) ListRepoRunners(ctx context.Context, installationToken, repoFullName string) ([]RepoRunner, error) {
	return nil, errors.New("ListRepoRunners: not implemented (v1.1)")
}

// DeleteRepoRunner removes a runner from GitHub's roster. Deferred to v1.1.
func (c *Client) DeleteRepoRunner(ctx context.Context, installationToken, repoFullName string, runnerID int64) error {
	return errors.New("DeleteRepoRunner: not implemented (v1.1)")
}
