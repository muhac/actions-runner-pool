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

// ListByPrefix: parses names line-by-line and re-checks the prefix
// (docker's --filter name= is substring, not prefix).
func TestExecDocker_ListByPrefix_FiltersAndTrims(t *testing.T) {
	withFakeDocker(t, `printf 'gharp-1-aaaa\ngharp-2-bbbb\npre-gharp-3-cccc\n\n'`)
	got, err := NewExecDocker().ListByPrefix(context.Background(), "gharp-")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"gharp-1-aaaa", "gharp-2-bbbb"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
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
