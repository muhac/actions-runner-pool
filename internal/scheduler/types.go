package scheduler

// WorkflowJobEvent is the subset of GitHub's workflow_job webhook payload
// we care about. See https://docs.github.com/en/webhooks/webhook-events-and-payloads#workflow_job
type WorkflowJobEvent struct {
	Action       string       `json:"action"` // queued | in_progress | completed
	WorkflowJob  WorkflowJob  `json:"workflow_job"`
	Repository   Repository   `json:"repository"`
	Installation Installation `json:"installation"`
}

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

type Repository struct {
	ID         int64  `json:"id"`
	FullName   string `json:"full_name"`
	HTMLURL    string `json:"html_url"`
	Private    bool   `json:"private"`
	Visibility string `json:"visibility"`
}

type Installation struct {
	ID int64 `json:"id"`
}
