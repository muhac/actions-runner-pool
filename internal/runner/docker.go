// Package runner manages the execution of GitHub Actions runners via Docker containers.
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

// Launcher executes GitHub Actions runners as Docker containers.
type Launcher struct {
	cfg                 *config.Config
	launchObserveWindow time.Duration
}

// NewLauncher creates a new Launcher instance.
func NewLauncher(cfg *config.Config) *Launcher {
	return &Launcher{
		cfg:                 cfg,
		launchObserveWindow: 2 * time.Second,
	}
}

// Spec provides the data for rendering and executing a docker run command.
type Spec struct {
	ContainerName     string
	RegistrationToken string
	RunnerName        string
	RepoURL           string
	Labels            string
	Image             string
}

// Launch renders the runner command template and starts a Docker container.
// Observes process briefly to catch early startup failures, then relies on
// webhooks and reconciliation for lifecycle tracking.
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
