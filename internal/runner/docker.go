package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"text/template"

	"github.com/muhac/actions-runner-pool/internal/config"
)

type Launcher struct {
	cfg *config.Config
}

func NewLauncher(cfg *config.Config) *Launcher {
	return &Launcher{cfg: cfg}
}

// Spec is the data passed into each template element when rendering the
// docker run argv. Keep field names in sync with config.requiredPlaceholders.
type Spec struct {
	ContainerName     string
	RegistrationToken string
	RunnerName        string
	RepoURL           string
	Labels            string
	Image             string
}

// Launch renders the configured RUNNER_COMMAND template against spec and
// starts the container. Returns once the docker daemon has acknowledged
// (Start, not Wait) — container lifecycle is tracked via webhook events
// and the reconciliation loop, not this call.
func (l *Launcher) Launch(ctx context.Context, spec Spec) error {
	if spec.Image == "" {
		spec.Image = l.cfg.RunnerImage
	}
	args := make([]string, len(l.cfg.RunnerCommand))
	for i, raw := range l.cfg.RunnerCommand {
		rendered, err := renderArg(raw, spec)
		if err != nil {
			return fmt.Errorf("render arg %d (%q): %w", i, raw, err)
		}
		args[i] = rendered
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release process for %q: %w", args[0], err)
	}
	return nil
}

func renderArg(raw string, spec Spec) (string, error) {
	t, err := template.New("arg").Parse(raw)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, spec); err != nil {
		return "", err
	}
	return buf.String(), nil
}
