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
	ID          int64
	AccountID   int64
	AccountLogin string
	AccountType string
	CreatedAt   time.Time
}

type Job struct {
	ID         int64
	Repo       string
	Action     string
	Labels     string
	DedupeKey  string
	Status     string
	Conclusion string
	RunnerID   int64
	RunnerName string
	ReceivedAt time.Time
	UpdatedAt  time.Time
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
