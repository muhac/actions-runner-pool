// Package config provides environment-based configuration for the autoscaler.
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
	"time"
)

// Config holds the application configuration loaded from environment variables.
type Config struct {
	Port        string
	BaseURL     string
	StoreDSN    string
	AdminToken  string
	RunnerImage string
	// RunnerNamePrefix scopes both the container/runner names the
	// scheduler generates AND the orphan sweep the reconciler
	// performs. Default "gharp-". Override to isolate a deployment
	// from sibling deployments sharing the same docker daemon (e.g.
	// integration tests setting a unique per-run prefix so the
	// reconciler doesn't reach into other deployments' containers).
	RunnerNamePrefix string
	RunnerCommand    []string
	RunnerLabels     []string
	// RunnerLabelSet is the precomputed lower-cased + trimmed set of
	// RunnerLabels — used by webhook label admission on the hot path.
	// Built once at Load so we don't reallocate + restring per webhook.
	RunnerLabelSet             map[string]struct{}
	RunnerDynamicLabelPrefixes []string
	AllowPublicRepos           bool
	RepoAllowlist              []string
	// RepoAllowlistSet is the precomputed lower-cased + trimmed set of
	// public repositories allowed even when AllowPublicRepos is false.
	RepoAllowlistSet     map[string]struct{}
	MaxConcurrentRunners int
	// RunnerMaxLifetime caps how long a runner row can stay in the
	// active set before the reconciler force-removes its container and
	// marks the row finished. Defends against EPHEMERAL runners that
	// register but never get assigned a job — without this they hold a
	// cap slot until the user notices.
	RunnerMaxLifetime time.Duration
	// ShutdownDrainTimeout is the maximum time allowed for graceful
	// shutdown after SIGTERM: the scheduler gets this long to finish any
	// in-flight dispatch, and the HTTP server gets this long to drain
	// open connections. Should be at least as long as your slowest
	// expected runner launch (image pull + container start + retry budget).
	ShutdownDrainTimeout time.Duration
	DockerHost           string
	// RunnerWorkdirRoot is the host path containing per-runner workdirs,
	// organized as <root>/<containerName>/. When set, reconciler cleanup
	// removes these directories after runner teardown and via periodic
	// orphan scans.
	RunnerWorkdirRoot string
	// MaintenanceCommand is an optional argv (no shell) executed on a
	// recurring schedule, e.g. ["docker","system","prune","-f","--volumes"].
	// Enabled only when both MaintenanceCommand and MaintenanceInterval are
	// set. No placeholder substitution is performed.
	MaintenanceCommand []string
	// MaintenanceInterval is how often MaintenanceCommand is run.
	// Zero (default) disables the periodic maintenance task entirely.
	MaintenanceInterval time.Duration
	GitHubAPIBase       string
	GitHubWebBase       string
	LogLevel            slog.Level
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

// Load reads environment variables and returns a validated Config.
func Load() (*Config, error) {
	c := &Config{
		Port:                 envOr("PORT", "8080"),
		BaseURL:              strings.TrimRight(os.Getenv("BASE_URL"), "/"),
		StoreDSN:             envOr("STORE_DSN", "file:gharp.db?_pragma=journal_mode(WAL)"),
		AdminToken:           strings.TrimSpace(os.Getenv("ADMIN_TOKEN")),
		RunnerImage:          envOr("RUNNER_IMAGE", "myoung34/github-runner:latest"),
		RunnerNamePrefix:     envOr("RUNNER_NAME_PREFIX", "gharp-"),
		MaxConcurrentRunners: envInt("MAX_CONCURRENT_RUNNERS", 4),
		RunnerMaxLifetime:    envDuration("RUNNER_MAX_LIFETIME", 2*time.Hour),
		ShutdownDrainTimeout: envDuration("SHUTDOWN_DRAIN_TIMEOUT", 30*time.Second),
		DockerHost:           os.Getenv("DOCKER_HOST"),
		RunnerWorkdirRoot:    strings.TrimSpace(os.Getenv("RUNNER_WORKDIR_ROOT")),
		MaintenanceInterval:  envDuration("MAINTENANCE_INTERVAL", 0),
		GitHubAPIBase:        strings.TrimRight(envOr("GITHUB_API_BASE", "https://api.github.com"), "/"),
		GitHubWebBase:        strings.TrimRight(envOr("GITHUB_WEB_BASE", "https://github.com"), "/"),
		RunnerLabels:         parseLabels(os.Getenv("RUNNER_LABELS")),
		RunnerDynamicLabelPrefixes: parseDynamicLabelPrefixes(
			os.Getenv("RUNNER_DYNAMIC_LABEL_PREFIXES"),
		),
		AllowPublicRepos: envBool("ALLOW_PUBLIC_REPOS"),
		RepoAllowlist:    parseList(os.Getenv("REPO_ALLOWLIST")),
		LogLevel:         parseLogLevel(envOr("LOG_LEVEL", "info")),
	}

	if c.BaseURL == "" {
		return nil, errors.New("BASE_URL is required (must be reachable from GitHub)")
	}

	if c.MaxConcurrentRunners < 1 {
		return nil, fmt.Errorf("MAX_CONCURRENT_RUNNERS must be >= 1, got %d", c.MaxConcurrentRunners)
	}

	if c.RunnerMaxLifetime <= 0 {
		// A non-positive lifetime would either be a no-op (zero) or
		// cause every just-launched runner to be reaped immediately —
		// neither is what an operator could reasonably want, so reject
		// at startup rather than degrade silently.
		return nil, fmt.Errorf("RUNNER_MAX_LIFETIME must be a positive duration, got %s", c.RunnerMaxLifetime)
	}

	if c.ShutdownDrainTimeout <= 0 {
		return nil, fmt.Errorf("SHUTDOWN_DRAIN_TIMEOUT must be a positive duration, got %s", c.ShutdownDrainTimeout)
	}

	if c.RunnerNamePrefix == "" {
		// Empty prefix would let the orphan sweep target literally any
		// container on the host (substring match returns everything).
		// Refuse rather than nuke unrelated containers.
		return nil, errors.New("RUNNER_NAME_PREFIX must be non-empty")
	}

	if u, err := url.Parse(c.GitHubAPIBase); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("GITHUB_API_BASE must be an absolute URL with scheme and host, got %q", c.GitHubAPIBase)
	}
	if u, err := url.Parse(c.GitHubWebBase); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("GITHUB_WEB_BASE must be an absolute URL with scheme and host, got %q", c.GitHubWebBase)
	}

	cmd, err := loadRunnerCommand()
	if err != nil {
		return nil, fmt.Errorf("RUNNER_COMMAND: %w", err)
	}
	c.RunnerCommand = cmd

	c.RunnerLabelSet = make(map[string]struct{}, len(c.RunnerLabels))
	for _, l := range c.RunnerLabels {
		c.RunnerLabelSet[strings.ToLower(strings.TrimSpace(l))] = struct{}{}
	}
	for i, prefix := range c.RunnerDynamicLabelPrefixes {
		c.RunnerDynamicLabelPrefixes[i] = strings.ToLower(strings.TrimSpace(prefix))
	}
	c.RepoAllowlistSet = make(map[string]struct{}, len(c.RepoAllowlist))
	for _, repo := range c.RepoAllowlist {
		c.RepoAllowlistSet[strings.ToLower(strings.TrimSpace(repo))] = struct{}{}
	}

	joined := strings.Join(c.RunnerCommand, " ")
	for _, p := range requiredPlaceholders {
		if !strings.Contains(joined, p) {
			return nil, fmt.Errorf("RUNNER_COMMAND missing required placeholder %s", p)
		}
	}

	maintCmd, err := loadMaintenanceCommand()
	if err != nil {
		return nil, fmt.Errorf("MAINTENANCE_COMMAND: %w", err)
	}
	c.MaintenanceCommand = maintCmd

	// Warn on partial configuration; missing half means disabled.
	hasCmd := len(c.MaintenanceCommand) > 0
	hasInterval := c.MaintenanceInterval > 0
	if hasCmd && !hasInterval {
		slog.Default().Warn("MAINTENANCE_COMMAND is set but MAINTENANCE_INTERVAL is missing or zero — periodic maintenance disabled")
	}
	if hasInterval && !hasCmd {
		slog.Default().Warn("MAINTENANCE_INTERVAL is set but MAINTENANCE_COMMAND is empty — periodic maintenance disabled")
	}

	if c.RunnerWorkdirRoot != "" {
		if err := os.MkdirAll(c.RunnerWorkdirRoot, 0o700); err != nil {
			return nil, fmt.Errorf("RUNNER_WORKDIR_ROOT %q is not accessible: %w", c.RunnerWorkdirRoot, err)
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

func loadMaintenanceCommand() ([]string, error) {
	raw := strings.TrimSpace(os.Getenv("MAINTENANCE_COMMAND"))
	if raw == "" {
		return nil, nil
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

func envBool(key string) bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(key)), "true")
}

// envDuration parses a Go time.Duration string ("90m", "2h30m", "10s").
// Falls back to the default on missing or unparseable values — the
// caller's positive-duration check at Load time will reject defaults
// that are themselves non-positive.
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// parseLabels splits "a,b, c" into ["a","b","c"]. Empty input defaults to
// ["self-hosted"] so we never accept jobs targeting GitHub-hosted runners.
func parseLabels(s string) []string {
	if s == "" {
		return []string{"self-hosted"}
	}
	return parseList(s)
}

func parseDynamicLabelPrefixes(s string) []string {
	if s == "" {
		return []string{"gharp-"}
	}
	return parseList(s)
}

func parseList(s string) []string {
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
