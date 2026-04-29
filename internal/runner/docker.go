package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"text/template"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
)

type Launcher struct {
	cfg                 *config.Config
	launchObserveWindow time.Duration
}

func NewLauncher(cfg *config.Config) *Launcher {
	return &Launcher{
		cfg:                 cfg,
		launchObserveWindow: 2 * time.Second,
	}
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
// starts the container. It observes the process briefly so early docker
// failures after Start (for example daemon/name/pull errors) can be retried;
// after that, container lifecycle is tracked via webhook events and the
// reconciliation loop, not this call.
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
	return l.observeLaunch(ctx, cmd)
}

func (l *Launcher) observeLaunch(ctx context.Context, cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	window := l.launchObserveWindow
	if window <= 0 {
		window = 2 * time.Second
	}
	timer := time.NewTimer(window)
	defer timer.Stop()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("runner command exited during launch: %w", err)
		}
		return nil
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
