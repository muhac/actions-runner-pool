package config

import (
	"log/slog"
	"strings"
	"testing"
)

// withEnv runs fn with key=val temporarily set; cleared after.
func withEnv(t *testing.T, kv map[string]string, fn func()) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
	fn()
}

func TestLoad_DefaultsApply(t *testing.T) {
	withEnv(t, map[string]string{
		"BASE_URL": "https://example.test",
	}, func() {
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Port != "8080" {
			t.Errorf("Port default = %q, want 8080", c.Port)
		}
		if c.RunnerImage != "myoung34/github-runner:latest" {
			t.Errorf("RunnerImage default = %q", c.RunnerImage)
		}
		if c.MaxConcurrentRunners != 4 {
			t.Errorf("MaxConcurrentRunners default = %d, want 4", c.MaxConcurrentRunners)
		}
		if c.LogLevel != slog.LevelInfo {
			t.Errorf("LogLevel default = %v, want info", c.LogLevel)
		}
		if len(c.RunnerCommand) == 0 {
			t.Errorf("RunnerCommand default empty")
		}
	})
}

func TestLoad_BaseURLRequired(t *testing.T) {
	// no BASE_URL
	_, err := Load()
	if err == nil {
		t.Fatal("expected error when BASE_URL is unset")
	}
	if !strings.Contains(err.Error(), "BASE_URL") {
		t.Errorf("error should mention BASE_URL, got: %v", err)
	}
}

func TestLoad_RunnerCommandInvalidJSON(t *testing.T) {
	withEnv(t, map[string]string{
		"BASE_URL":       "https://example.test",
		"RUNNER_COMMAND": "not-json",
	}, func() {
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "RUNNER_COMMAND") {
			t.Fatalf("want RUNNER_COMMAND parse error, got: %v", err)
		}
	})
}

func TestLoad_RunnerCommandWrongType(t *testing.T) {
	withEnv(t, map[string]string{
		"BASE_URL":       "https://example.test",
		"RUNNER_COMMAND": `{"foo":"bar"}`,
	}, func() {
		_, err := Load()
		if err == nil {
			t.Fatal("want error for non-array JSON")
		}
	})
}

func TestLoad_RunnerCommandEmptyArray(t *testing.T) {
	withEnv(t, map[string]string{
		"BASE_URL":       "https://example.test",
		"RUNNER_COMMAND": `[]`,
	}, func() {
		_, err := Load()
		if err == nil {
			t.Fatal("want error for empty array")
		}
	})
}

func TestLoad_RunnerCommandMissingPlaceholder(t *testing.T) {
	for _, p := range requiredPlaceholders {
		t.Run(p, func(t *testing.T) {
			// Build a minimal command containing every placeholder EXCEPT p.
			parts := []string{"docker", "run"}
			for _, q := range requiredPlaceholders {
				if q == p {
					continue
				}
				parts = append(parts, q)
			}
			parts = append(parts, "{{.Image}}")
			cmd := jsonStringArray(t, parts)

			withEnv(t, map[string]string{
				"BASE_URL":       "https://example.test",
				"RUNNER_COMMAND": cmd,
			}, func() {
				_, err := Load()
				if err == nil {
					t.Fatalf("expected error when %s missing", p)
				}
				if !strings.Contains(err.Error(), p) {
					t.Errorf("error should name missing placeholder %s, got: %v", p, err)
				}
			})
		})
	}
}

func TestLoad_RunnerCommandValidArray(t *testing.T) {
	cmd := jsonStringArray(t, []string{
		"docker", "run", "--rm",
		"--name", "{{.ContainerName}}",
		"-e", "REPO_URL={{.RepoURL}}",
		"-e", "RUNNER_TOKEN={{.RegistrationToken}}",
		"-e", "RUNNER_NAME={{.RunnerName}}",
		"-e", "LABELS={{.Labels}}",
		"{{.Image}}",
	})
	withEnv(t, map[string]string{
		"BASE_URL":       "https://example.test",
		"RUNNER_COMMAND": cmd,
	}, func() {
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got, want := len(c.RunnerCommand), 14; got != want {
			t.Errorf("RunnerCommand len = %d, want %d", got, want)
		}
	})
}

func TestLoad_LogLevelMapping(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"weird":   slog.LevelInfo, // unknown -> info
		"":        slog.LevelInfo,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			env := map[string]string{"BASE_URL": "https://example.test"}
			if in != "" {
				env["LOG_LEVEL"] = in
			}
			withEnv(t, env, func() {
				c, err := Load()
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if c.LogLevel != want {
					t.Errorf("LogLevel(%q) = %v, want %v", in, c.LogLevel, want)
				}
			})
		})
	}
}

func TestLoad_MaxConcurrentRunnersInvalid(t *testing.T) {
	withEnv(t, map[string]string{
		"BASE_URL":               "https://example.test",
		"MAX_CONCURRENT_RUNNERS": "not-a-number",
	}, func() {
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.MaxConcurrentRunners != 4 {
			t.Errorf("invalid int should fall back to default 4, got %d", c.MaxConcurrentRunners)
		}
	})
}

func TestLoad_GitHubAPIBase(t *testing.T) {
	cases := []struct {
		name string
		set  string
		want string
	}{
		{"default", "", "https://api.github.com"},
		{"override", "https://gh.example.com/api/v3", "https://gh.example.com/api/v3"},
		{"trailing-slash-stripped", "https://gh.example.com/api/v3/", "https://gh.example.com/api/v3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"BASE_URL": "https://example.test"}
			if tc.set != "" {
				env["GITHUB_API_BASE"] = tc.set
			}
			withEnv(t, env, func() {
				c, err := Load()
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if c.GitHubAPIBase != tc.want {
					t.Errorf("GitHubAPIBase = %q, want %q", c.GitHubAPIBase, tc.want)
				}
			})
		})
	}
}

func TestLoad_GitHubAPIBaseInvalid(t *testing.T) {
	for _, in := range []string{"/", "not a url", "no-scheme.example.com"} {
		t.Run(in, func(t *testing.T) {
			withEnv(t, map[string]string{
				"BASE_URL":        "https://example.test",
				"GITHUB_API_BASE": in,
			}, func() {
				_, err := Load()
				if err == nil {
					t.Fatalf("expected error for %q", in)
				}
				if !strings.Contains(err.Error(), "GITHUB_API_BASE") {
					t.Errorf("error should mention GITHUB_API_BASE, got: %v", err)
				}
			})
		})
	}
}

// helper: serialize a []string as JSON without bringing in a dep.
func jsonStringArray(t *testing.T, parts []string) string {
	t.Helper()
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range parts {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for _, r := range s {
			switch r {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			default:
				b.WriteRune(r)
			}
		}
		b.WriteByte('"')
	}
	b.WriteByte(']')
	return b.String()
}
