package reconciler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// withFakeDocker drops a tiny shell shim named `docker` into PATH so
// the exec wrapper can run end-to-end without a real docker daemon.
func withFakeDocker(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// Inspect: container exists -> true.
func TestExecDocker_Inspect_Exists(t *testing.T) {
	withFakeDocker(t, `echo abc123; exit 0`)
	exists, err := NewExecDocker().Inspect(context.Background(), "gharp-1-aaaa")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !exists {
		t.Fatal("expected exists=true")
	}
}

// Inspect: missing container -> exists=false, no error. This branch is
// what makes the ghost-runner sweep work; if we accidentally surfaced
// the exit-1 as an error here, sweepGhostRunners would skip and the cap
// deadlock would persist.
func TestExecDocker_Inspect_NotFound(t *testing.T) {
	withFakeDocker(t, `echo "Error: No such container: $5" 1>&2; exit 1`)
	exists, err := NewExecDocker().Inspect(context.Background(), "gharp-2-bbbb")
	if err != nil {
		t.Fatalf("expected nil error on not-found, got %v", err)
	}
	if exists {
		t.Fatal("expected exists=false")
	}
}

// Inspect: daemon down -> propagated error so the loop stays
// conservative instead of marking everything finished.
func TestExecDocker_Inspect_DaemonDown(t *testing.T) {
	withFakeDocker(t, `echo "Cannot connect to the Docker daemon" 1>&2; exit 1`)
	_, err := NewExecDocker().Inspect(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error when daemon is down")
	}
}

// ListByPrefix: parses `name|createdAt` line-by-line, re-checks the
// prefix (docker's --filter name= is substring, not prefix), and
// surfaces a parsed CreatedAt so the orphan sweep can do per-container
// grace gating. Unparsable timestamps yield zero CreatedAt — the
// reconciler's policy treats that as old enough to remove.
func TestExecDocker_ListByPrefix_FiltersAndParsesCreatedAt(t *testing.T) {
	withFakeDocker(t, `printf 'gharp-1-aaaa|2026-04-23 10:35:53 -0400 EDT\ngharp-2-bbbb|2026-04-23 11:00:00 +0000 UTC\npre-gharp-3-cccc|2026-04-23 10:35:53 -0400 EDT\ngharp-4-dddd|garbage-time\n\n'`)
	got, err := NewExecDocker().ListByPrefix(context.Background(), "gharp-")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 (full=%+v)", len(got), got)
	}
	if got[0].Name != "gharp-1-aaaa" || got[0].CreatedAt.IsZero() {
		t.Fatalf("entry 0 = %+v", got[0])
	}
	if got[1].Name != "gharp-2-bbbb" || got[1].CreatedAt.IsZero() {
		t.Fatalf("entry 1 = %+v", got[1])
	}
	// Unparsable timestamp → zero CreatedAt, but name still surfaced.
	if got[2].Name != "gharp-4-dddd" || !got[2].CreatedAt.IsZero() {
		t.Fatalf("entry 2 = %+v (want zero CreatedAt for garbage timestamp)", got[2])
	}
}

// ForceRemove: missing container is swallowed as success — racing with
// the container exiting between list and remove must not flake the
// loop.
func TestExecDocker_ForceRemove_NoSuchContainer(t *testing.T) {
	withFakeDocker(t, `echo "Error: No such container: $3" 1>&2; exit 1`)
	if err := NewExecDocker().ForceRemove(context.Background(), "gharp-99-zzzz"); err != nil {
		t.Fatalf("expected nil for missing container, got %v", err)
	}
}
