package reconciler

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
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
// matches `prefix*`, returning each name with its CreatedAt timestamp
// so the orphan sweep can do per-container grace gating. The
// `--filter name=` accepts a substring match, so we also enforce
// HasPrefix on the result to avoid false positives like
// `pre-gharp-foo`.
//
// Format: `{{.Names}}|{{.CreatedAt}}`. CreatedAt is docker's "human"
// time string ("2026-04-23 10:35:53 -0400 EDT"), which time.Parse
// handles via the layout below. Parse failures yield a zero-value
// CreatedAt — sweepOrphanContainers treats that as "old enough to
// remove" so an undatable orphan still gets cleaned up.
func (ExecDocker) ListByPrefix(ctx context.Context, prefix string) ([]ContainerInfo, error) {
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "name="+prefix, "--format", "{{.Names}}|{{.CreatedAt}}")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker ps: %w (stderr=%q)", err, stderr.String())
	}
	var out []ContainerInfo
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		name, createdRaw, _ := strings.Cut(line, "|")
		name = strings.TrimSpace(name)
		if name == "" || !strings.HasPrefix(name, prefix) {
			continue
		}
		info := ContainerInfo{Name: name}
		if t, err := parseDockerCreatedAt(strings.TrimSpace(createdRaw)); err == nil {
			info.CreatedAt = t
		}
		out = append(out, info)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan docker ps output: %w", err)
	}
	return out, nil
}

// parseDockerCreatedAt parses docker's `{{.CreatedAt}}` format, which
// is a Go time printed via the default String() formatting:
// "2026-04-23 10:35:53 -0400 EDT". Some installs include nanoseconds
// or omit the zone abbreviation, so we try a couple of layouts.
func parseDockerCreatedAt(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty createdAt")
	}
	layouts := []string{
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized createdAt format: %q", s)
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
