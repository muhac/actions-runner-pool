package github

import (
	"context"
	"errors"
)

// RegistrationToken mints a single-use runner registration token for a repo.
// POST /repos/{owner}/{repo}/actions/runners/registration-token
func (c *Client) RegistrationToken(ctx context.Context, installationToken, repoFullName string) (string, error) {
	return "", errors.New("RegistrationToken: not implemented")
}

type RepoRunner struct {
	ID     int64
	Name   string
	Status string // online, offline
	Busy   bool
	Labels []string
}

// ListRepoRunners is used by the reconciliation loop to detect ghost runners.
func (c *Client) ListRepoRunners(ctx context.Context, installationToken, repoFullName string) ([]RepoRunner, error) {
	return nil, errors.New("ListRepoRunners: not implemented")
}

// DeleteRepoRunner removes a runner from GitHub's roster.
func (c *Client) DeleteRepoRunner(ctx context.Context, installationToken, repoFullName string, runnerID int64) error {
	return errors.New("DeleteRepoRunner: not implemented")
}
