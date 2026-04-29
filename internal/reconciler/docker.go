package reconciler

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ExecDocker implements Docker by shelling out to the `docker` CLI.
// We deliberately avoid the docker SDK to keep the dependency tree
// minimal — the launcher already shells out, so the deployment already
// has a working `docker` binary on PATH.
type ExecDocker struct{}

func NewExecDocker() *ExecDocker { return &ExecDocker{} }

// Inspect returns whether a container with the given name exists at
// all (any state). We use `docker inspect --format=` and treat exit
// code 1 with a "No such object" stderr as the canonical "gone"
// signal. Any other error is propagated so the caller can stay
// conservative.
func (ExecDocker) Inspect(ctx context.Context, containerName string) (bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--type=container", "--format={{.Id}}", containerName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// docker inspect exits 1 for missing objects. Distinguish from
		// a real failure (daemon down, permission denied) by checking
		// the stderr signature — anything else is propagated so
		// sweepGhostRunners stays conservative.
		out := stderr.String()
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 &&
			(strings.Contains(out, "No such object") || strings.Contains(out, "No such container")) {
			return false, nil
		}
		return false, fmt.Errorf("docker inspect %q: %w (stderr=%q)", containerName, err, out)
	}
	return true, nil
}

// ListByPrefix asks docker for all containers (any state) whose name
// matches `prefix*`. The `--filter name=` accepts a substring match,
// so we also enforce HasPrefix on the result to avoid false positives
// like a hypothetical `pre-gharp-foo`.
func (ExecDocker) ListByPrefix(ctx context.Context, prefix string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "name="+prefix, "--format", "{{.Names}}")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker ps: %w (stderr=%q)", err, stderr.String())
	}
	var out []string
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		name := strings.TrimSpace(sc.Text())
		if name == "" {
			continue
		}
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		out = append(out, name)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan docker ps output: %w", err)
	}
	return out, nil
}

// ForceRemove issues `docker rm -f`. A "no such container" outcome
// means the container died between our list and the remove call —
// effectively success, so it's swallowed.
func (ExecDocker) ForceRemove(ctx context.Context, containerName string) error {
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", containerName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		out := stderr.String()
		if strings.Contains(out, "No such container") {
			return nil
		}
		return fmt.Errorf("docker rm -f %q: %w (stderr=%q)", containerName, err, out)
	}
	return nil
}
