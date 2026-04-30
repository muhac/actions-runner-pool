package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// splitRepoFullName parses "owner/repo" and returns each segment.
// Rejects empty, missing slash, multiple slashes, and empty segments.
func splitRepoFullName(repoFullName string) (owner, repo string, err error) {
	parts := strings.Split(repoFullName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repoFullName must be \"owner/repo\", got %q", repoFullName)
	}
	return parts[0], parts[1], nil
}

// RegistrationToken mints a single-use runner registration token for a repo.
// POST /repos/{owner}/{repo}/actions/runners/registration-token.
//
// Tokens are single-use under EPHEMERAL=1; never cache.
// RegistrationToken returns a runner registration token for a repository.
func (c *Client) RegistrationToken(ctx context.Context, installationToken, repoFullName string) (string, error) {
	owner, repo, err := splitRepoFullName(repoFullName)
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/runners/registration-token",
		c.cfg.GitHubAPIBase, url.PathEscape(owner), url.PathEscape(repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
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

// RepoRunner represents a GitHub Actions runner in a repository.
type RepoRunner struct {
	ID     int64
	Name   string
	Status string // online, offline
	Busy   bool
	Labels []string
}

// ListRepoRunners pages through GET /repos/{owner}/{repo}/actions/runners
// and returns every registered runner. Used by the reconciler's
// GitHub-side ghost sweep to find runners GitHub still has on file
// that we no longer have an active row for (most often: a runner
// container exited cleanly but `--rm` removed it before we deregistered;
// or a process crash mid-launch).
//
// Pagination follows GitHub's standard 100-per-page limit. Caller
// concurrency is the loop's tick (~5 min), so the per-call cost is
// the dominant rate-limit factor — we ask for the max page size to
// keep round-trips low for accounts with many runners.
func (c *Client) ListRepoRunners(ctx context.Context, installationToken, repoFullName string) ([]RepoRunner, error) {
	owner, repo, err := splitRepoFullName(repoFullName)
	if err != nil {
		return nil, err
	}
	var out []RepoRunner
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/runners?per_page=100&page=%d",
			c.cfg.GitHubAPIBase, url.PathEscape(owner), url.PathEscape(repo), page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+installationToken)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		// Decode + close per iteration so a mid-pagination ctx
		// cancel doesn't leak the prior body.
		body, err := decodeListRepoRunnersPage(resp)
		if err != nil {
			return nil, err
		}
		out = append(out, body.runners...)
		// GitHub's response includes `total_count` covering ALL
		// runners across pages; once we've collected that many
		// we're done. Also stop if the page came back short of the
		// page size (defensive against API inconsistency).
		if int64(len(out)) >= body.totalCount || len(body.runners) < 100 {
			return out, nil
		}
	}
}

type listRepoRunnersPage struct {
	totalCount int64
	runners    []RepoRunner
}

func decodeListRepoRunnersPage(resp *http.Response) (listRepoRunnersPage, error) {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return listRepoRunnersPage{}, fmt.Errorf("list repo runners: status %d", resp.StatusCode)
	}
	var raw struct {
		TotalCount int64 `json:"total_count"`
		Runners    []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Busy   bool   `json:"busy"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"runners"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return listRepoRunnersPage{}, fmt.Errorf("decode list repo runners: %w", err)
	}
	out := listRepoRunnersPage{totalCount: raw.TotalCount, runners: make([]RepoRunner, 0, len(raw.Runners))}
	for _, r := range raw.Runners {
		labels := make([]string, len(r.Labels))
		for i, l := range r.Labels {
			labels[i] = l.Name
		}
		out.runners = append(out.runners, RepoRunner{
			ID: r.ID, Name: r.Name, Status: r.Status, Busy: r.Busy, Labels: labels,
		})
	}
	return out, nil
}

// DeleteRepoRunner removes a runner from GitHub's roster.
// 404 is swallowed as success — the runner may have expired or been
// deleted concurrently between our List and our Delete, and the
// caller's intent ("make sure this runner is gone") is satisfied
// either way.
func (c *Client) DeleteRepoRunner(ctx context.Context, installationToken, repoFullName string, runnerID int64) error {
	owner, repo, err := splitRepoFullName(repoFullName)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/runners/%d",
		c.cfg.GitHubAPIBase, url.PathEscape(owner), url.PathEscape(repo), runnerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+installationToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("delete repo runner: status %d", resp.StatusCode)
	}
	return nil
}
