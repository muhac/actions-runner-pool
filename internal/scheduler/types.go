package scheduler

// WorkflowJobEvent represents a GitHub workflow_job webhook payload.
type WorkflowJobEvent struct {
	Action       string       `json:"action"` // queued | in_progress | completed
	WorkflowJob  WorkflowJob  `json:"workflow_job"`
	Repository   Repository   `json:"repository"`
	Installation Installation `json:"installation"`
}

// WorkflowJob represents a GitHub Actions workflow job.
type WorkflowJob struct {
	ID           int64    `json:"id"`
	RunID        int64    `json:"run_id"`
	RunAttempt   int64    `json:"run_attempt"`
	Name         string   `json:"name"`
	WorkflowName string   `json:"workflow_name"`
	Status       string   `json:"status"`
	Conclusion   string   `json:"conclusion"`
	Labels       []string `json:"labels"`
	RunnerID     int64    `json:"runner_id"`
	RunnerName   string   `json:"runner_name"`
}

// Repository represents a GitHub repository.
type Repository struct {
	ID         int64  `json:"id"`
	FullName   string `json:"full_name"`
	HTMLURL    string `json:"html_url"`
	Private    bool   `json:"private"`
	Visibility string `json:"visibility"`
}

// Installation represents a GitHub App installation.
type Installation struct {
	ID int64 `json:"id"`
}
