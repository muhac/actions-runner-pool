package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// WorkflowJobStatus is the GitHub API's view of a workflow_job: the
// truth-of-record. Used by the dispatch path's pre-launch check to
// catch jobs that completed/cancelled while we were minting tokens —
// without this, a webhook delay or drop would let us happily launch
// a runner for a job nobody is going to claim.
type WorkflowJobStatus struct {
	// Status is one of "queued", "in_progress", "completed".
	Status string
	// Conclusion is non-empty only when Status == "completed":
	// "success", "failure", "cancelled", "skipped", "timed_out", ...
	Conclusion string
	// NotFound is true when GitHub returned 404 (job deleted or
	// inaccessible). When true, Status and Conclusion are empty —
	// callers should treat this as terminal.
	NotFound bool
	// AuthFailed is true when GitHub returned 401/403. Distinct from
	// NotFound because the most common cause is "the App was just
	// uninstalled and the installation token is no longer valid" —
	// callers handling 404 confirmation should treat this as a
	// confirmation that the job is no longer reachable, not as a
	// transient error to retry past.
	AuthFailed bool
}

// WorkflowJob fetches the current status of a workflow job from
// GitHub. Used by the scheduler immediately before launching a
// container as a final correctness check against stale or dropped
// webhook events.
//
// 404 is returned as NotFound=true rather than an error: it's a
// well-defined terminal state (the job no longer exists), and the
// caller's policy is to mark the job cancelled and abort.
func (c *Client) WorkflowJob(ctx context.Context, installationToken, repoFullName string, jobID int64) (*WorkflowJobStatus, error) {
	owner, repo, err := splitRepoFullName(repoFullName)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d",
		c.cfg.GitHubAPIBase, url.PathEscape(owner), url.PathEscape(repo), jobID)
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
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return &WorkflowJobStatus{NotFound: true}, nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &WorkflowJobStatus{AuthFailed: true}, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("workflow job: status %d", resp.StatusCode)
	}
	var body struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode workflow job: %w", err)
	}
	if body.Status == "" {
		return nil, errors.New("workflow job: empty status in response")
	}
	return &WorkflowJobStatus{Status: body.Status, Conclusion: body.Conclusion}, nil
}
