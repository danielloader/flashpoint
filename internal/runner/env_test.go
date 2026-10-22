package runner

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/danielloader/flashpoint/internal/config"
)

func TestAPIEnvLayers(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FROM_SHELL", "shell")
	t.Setenv("OVERRIDDEN", "shell")
	os.WriteFile(filepath.Join(root, ".env"), []byte("OVERRIDDEN=file\nexport QUOTED=\"a b\" # note\nLATER=first\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".env.local"), []byte("LATER=second\n"), 0o644)
	plan := &config.Plan{
		Root:     root,
		EnvFiles: []string{filepath.Join(root, ".env"), filepath.Join(root, ".env.missing"), filepath.Join(root, ".env.local")},
		Env:      []string{"DERIVED=${LATER:-none}"},
	}
	env, err := apiEnv(plan)
	if err != nil {
		t.Fatal(err)
	}
	got := envMap(env)
	for k, want := range map[string]string{
		"FROM_SHELL": "shell",
		"OVERRIDDEN": "file",
		"QUOTED":     "a b",
		"LATER":      "second",
		"DERIVED":    "second",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}

	os.WriteFile(filepath.Join(root, ".env.local"), []byte("LATER='unterminated\n"), 0o644)
	if _, err := apiEnv(plan); err == nil {
		t.Fatal("want a parse error")
	}
}

func TestEnvDiff(t *testing.T) {
	got := envDiff(
		[]string{"A=1", "B=1", "C=1", "D=1", "D=2"},
		[]string{"A=1", "B=2", "E=1", "D=2"},
	)
	if want := []string{"B", "C", "E"}; !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}
