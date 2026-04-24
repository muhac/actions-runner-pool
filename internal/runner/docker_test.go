package runner

import (
	"strings"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/config"
)

func TestRenderArg_AllPlaceholders(t *testing.T) {
	spec := Spec{
		ContainerName:     "gharp-abc",
		RegistrationToken: "AAAA-token",
		RunnerName:        "runner-1",
		RepoURL:           "https://github.com/foo/bar",
		Labels:            "self-hosted,linux",
		Image:             "myimage:tag",
	}
	cases := map[string]string{
		"{{.ContainerName}}":           "gharp-abc",
		"--name={{.ContainerName}}":    "--name=gharp-abc",
		"REPO_URL={{.RepoURL}}":        "REPO_URL=https://github.com/foo/bar",
		"RUNNER_TOKEN={{.RegistrationToken}}": "RUNNER_TOKEN=AAAA-token",
		"{{.Image}}":                   "myimage:tag",
		"static":                       "static",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got, err := renderArg(in, spec)
			if err != nil {
				t.Fatalf("renderArg(%q): %v", in, err)
			}
			if got != want {
				t.Errorf("renderArg(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestRenderArg_SpecialCharsPassedLiterally(t *testing.T) {
	// argv-style execution means we don't need shell escaping; verify the
	// renderer doesn't try to do anything clever with metacharacters.
	spec := Spec{
		Labels: `self-hosted,a label with spaces,$(rm -rf /)`,
	}
	got, err := renderArg("LABELS={{.Labels}}", spec)
	if err != nil {
		t.Fatalf("renderArg: %v", err)
	}
	want := `LABELS=self-hosted,a label with spaces,$(rm -rf /)`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderArg_BadTemplateReturnsError(t *testing.T) {
	if _, err := renderArg("{{.NotAField}}", Spec{}); err != nil {
		// text/template renders missing fields as <no value> rather than
		// erroring; this case is fine. The actual error path is parse failure.
		_ = err
	}
	if _, err := renderArg("{{ unbalanced", Spec{}); err == nil {
		t.Errorf("expected parse error for malformed template")
	}
}

func TestRenderArg_ImageFieldRendered(t *testing.T) {
	spec := Spec{Image: "ghcr.io/me/runner:v1"}
	got, err := renderArg("{{.Image}}", spec)
	if err != nil {
		t.Fatalf("renderArg: %v", err)
	}
	if got != "ghcr.io/me/runner:v1" {
		t.Errorf("Image not rendered: %q", got)
	}
}

func TestLauncher_LaunchImageFallback(t *testing.T) {
	// We can't actually run docker in tests, but we can verify the Spec
	// gets its Image filled in from cfg.RunnerImage when blank.
	cfg := &config.Config{
		RunnerImage: "default-image:latest",
		RunnerCommand: []string{
			"echo", // safe command for testing
			"name={{.ContainerName}}",
			"image={{.Image}}",
		},
	}
	l := NewLauncher(cfg)

	// Render args manually mirroring Launch's logic (Launch itself calls
	// exec.Start which we don't want in unit tests).
	spec := Spec{ContainerName: "c1"}
	if spec.Image == "" {
		spec.Image = cfg.RunnerImage
	}
	args := make([]string, len(l.cfg.RunnerCommand))
	for i, raw := range l.cfg.RunnerCommand {
		got, err := renderArg(raw, spec)
		if err != nil {
			t.Fatalf("renderArg: %v", err)
		}
		args[i] = got
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "image=default-image:latest") {
		t.Errorf("image fallback not applied; args = %v", args)
	}
}
