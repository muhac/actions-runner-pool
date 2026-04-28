package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port                 string
	BaseURL              string
	StoreDSN             string
	RunnerImage          string
	RunnerCommand        []string
	RunnerLabels         []string
	MaxConcurrentRunners int
	DockerHost           string
	GitHubAPIBase        string
	LogLevel             slog.Level
}

var defaultRunnerCommand = []string{
	"docker", "run", "--rm",
	"--name", "{{.ContainerName}}",
	"-e", "REPO_URL={{.RepoURL}}",
	"-e", "RUNNER_TOKEN={{.RegistrationToken}}",
	"-e", "RUNNER_NAME={{.RunnerName}}",
	"-e", "LABELS={{.Labels}}",
	"-e", "EPHEMERAL=1",
	"{{.Image}}",
}

var requiredPlaceholders = []string{
	"{{.ContainerName}}",
	"{{.RegistrationToken}}",
	"{{.RunnerName}}",
	"{{.RepoURL}}",
	"{{.Labels}}",
}

func Load() (*Config, error) {
	c := &Config{
		Port:                 envOr("PORT", "8080"),
		BaseURL:              strings.TrimRight(os.Getenv("BASE_URL"), "/"),
		StoreDSN:             envOr("STORE_DSN", "file:gharp.db?_pragma=journal_mode(WAL)"),
		RunnerImage:          envOr("RUNNER_IMAGE", "myoung34/github-runner:latest"),
		MaxConcurrentRunners: envInt("MAX_CONCURRENT_RUNNERS", 4),
		DockerHost:           os.Getenv("DOCKER_HOST"),
		GitHubAPIBase:        strings.TrimRight(envOr("GITHUB_API_BASE", "https://api.github.com"), "/"),
		RunnerLabels:         parseLabels(os.Getenv("RUNNER_LABELS")),
		LogLevel:             parseLogLevel(envOr("LOG_LEVEL", "info")),
	}

	if c.BaseURL == "" {
		return nil, errors.New("BASE_URL is required (must be reachable from GitHub)")
	}

	if u, err := url.Parse(c.GitHubAPIBase); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("GITHUB_API_BASE must be an absolute URL with scheme and host, got %q", c.GitHubAPIBase)
	}

	cmd, err := loadRunnerCommand()
	if err != nil {
		return nil, fmt.Errorf("RUNNER_COMMAND: %w", err)
	}
	c.RunnerCommand = cmd

	joined := strings.Join(c.RunnerCommand, " ")
	for _, p := range requiredPlaceholders {
		if !strings.Contains(joined, p) {
			return nil, fmt.Errorf("RUNNER_COMMAND missing required placeholder %s", p)
		}
	}

	return c, nil
}

func loadRunnerCommand() ([]string, error) {
	raw := os.Getenv("RUNNER_COMMAND")
	if raw == "" {
		return append([]string(nil), defaultRunnerCommand...), nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("must be a JSON array of strings: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("must be a non-empty JSON array")
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// parseLabels splits "a,b, c" into ["a","b","c"]. Empty input → nil so the
// webhook can detect "no filter, serve everything".
func parseLabels(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
