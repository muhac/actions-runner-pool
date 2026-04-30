package store

import "time"

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

type Installation struct {
	ID           int64
	AccountID    int64
	AccountLogin string
	AccountType  string
	CreatedAt    time.Time
}

// RepoInstallation is one row of the installation_repos join table —
// "this App is installed on this repo via this installation". Used by
// the GitHub-side ghost sweep to enumerate all repos the App can see.
type RepoInstallation struct {
	Repo           string
	InstallationID int64
}

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

type JobListFilter struct {
	Statuses []string
	Repo     string
	Limit    int
}

type Summary struct {
	JobsByStatus    map[string]int64
	RunnersByStatus map[string]int64
	ActiveRunners   int64
}

type Runner struct {
	ContainerName string
	Repo          string
	RunnerName    string
	Labels        string
	Status        string
	StartedAt     time.Time
	FinishedAt    *time.Time
}
