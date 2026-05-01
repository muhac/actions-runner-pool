package store

import "time"

// AppConfig holds GitHub App credentials and configuration.
type AppConfig struct {
	AppID         int64
	Slug          string
	WebhookSecret string
	PEM           []byte
	ClientID      string
	ClientSecret  string
	BaseURL       string
	CreatedAt     time.Time
}

// Installation represents a GitHub App installation.
type Installation struct {
	ID           int64
	AccountID    int64
	AccountLogin string
	AccountType  string
	CreatedAt    time.Time
}

// RepoInstallation maps a repository to an installation.
type RepoInstallation struct {
	Repo           string
	InstallationID int64
}

// Job represents a GitHub workflow job in the queue or running.
type Job struct {
	ID           int64
	Repo         string
	JobName      string
	RunID        int64
	RunAttempt   int64
	WorkflowName string
	Action       string
	Labels       string
	DedupeKey    string
	PayloadJSON  string
	Status       string
	Conclusion   string
	RunnerID     int64
	RunnerName   string
	ReceivedAt   time.Time
	UpdatedAt    time.Time
}

// JobStatuses and RunnerStatuses enumerate all well-known status values.
// They are used by the metrics collector to guarantee stable label cardinality
// (zero-valued series are emitted for statuses absent from the DB).
var JobStatuses = []string{"pending", "dispatched", "in_progress", "completed"}
var RunnerStatuses = []string{"starting", "idle", "busy", "finished"}

// JobListFilter provides query criteria for listing jobs.
type JobListFilter struct {
	Statuses []string
	Repo     string
	Limit    int
}

// Summary summarizes runner and job counts by status.
type Summary struct {
	JobsByStatus    map[string]int64
	RunnersByStatus map[string]int64
}

// Runner represents a GitHub Actions runner managed by the autoscaler.
type Runner struct {
	ContainerName string
	Repo          string
	RunnerName    string
	Labels        string
	Status        string
	StartedAt     time.Time
	FinishedAt    *time.Time
}
