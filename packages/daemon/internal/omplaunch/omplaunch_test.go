package omplaunch

import (
	"os"
	"path/filepath"
	"testing"
)

// writeExecutable makes an executable file for the resolution tests.
func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeMise serves the configured tool's install root. Its test keeps an executable named omp
// earlier on PATH, so ResolveInvocation must not resolve through ordinary command lookup.
func writeMise(t *testing.T, path, tool, install string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = where ] && [ \"$2\" = \"" + tool + "\" ]; then\n" +
		"  printf '%s\\n' \"" + install + "\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// ResolveInvocation is the shipped resolveOmpInvocation (environment.ts:312-341): an absolute
// LEGION_OMP_PATH is resolved and used directly; otherwise the configured invocation must be
// `mise x <tool> -- omp`, kept verbatim with mise pinned to its resolved absolute path, so mise
// still activates the tool inside the pane.
func TestResolveInvocation(t *testing.T) {
	dir := t.TempDir()
	realOmp := filepath.Join(dir, "installs", "omp-18", "bin", "omp")
	writeExecutable(t, realOmp)
	linkedOmp := filepath.Join(dir, "bin", "omp")
	if err := os.MkdirAll(filepath.Dir(linkedOmp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realOmp, linkedOmp); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(dir, "mise dir", "mise")
	writeMise(t, mise, "github:sjawhar/oh-my-pi@18.1.21", filepath.Dir(filepath.Dir(realOmp)))
	notExecutable := filepath.Join(dir, "plain")
	if err := os.WriteFile(notExecutable, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	const pinned = "mise x github:sjawhar/oh-my-pi@18.1.21 -- omp"

	for _, tc := range []struct {
		name       string
		invocation string
		env        map[string]string
		want       string
		refusal    string
	}{
		{
			name:       "LEGION_OMP_PATH names an executable: its real path, used directly",
			invocation: pinned,
			env:        map[string]string{"LEGION_OMP_PATH": linkedOmp},
			want:       realOmp,
		},
		{
			name:       "LEGION_OMP_PATH wins over any invocation shape",
			invocation: "omp",
			env:        map[string]string{"LEGION_OMP_PATH": linkedOmp},
			want:       realOmp,
		},
		{
			name:       "LEGION_OMP_PATH with no invocation configured",
			invocation: "",
			env:        map[string]string{"LEGION_OMP_PATH": linkedOmp},
			want:       realOmp,
		},
		{
			// The shipped daemon falls back to its pinned default; the Go daemon has no copy of
			// the pin, so it names the key instead.
			name:       "no invocation and no LEGION_OMP_PATH",
			invocation: "",
			env:        map[string]string{"PATH": filepath.Dir(mise)},
			refusal:    "omp_invocation is not set: set it to 'mise x <tool> -- omp', or set LEGION_OMP_PATH to an absolute executable path",
		},
		{
			name:       "LEGION_OMP_PATH relative",
			invocation: pinned,
			env:        map[string]string{"LEGION_OMP_PATH": "bin/omp"},
			refusal:    "LEGION_OMP_PATH must be an absolute executable path",
		},
		{
			name:       "LEGION_OMP_PATH not executable",
			invocation: pinned,
			env:        map[string]string{"LEGION_OMP_PATH": notExecutable},
			refusal:    "LEGION_OMP_PATH is not an executable: " + notExecutable,
		},
		{
			name:       "mise resolves the configured tool's binary, not an omp earlier on PATH",
			invocation: pinned,
			env:        map[string]string{"LEGION_MISE_PATH": mise, "PATH": filepath.Dir(linkedOmp)},
			want:       "'" + mise + "' x github:sjawhar/oh-my-pi@18.1.21 -- " + realOmp,
		},
		{
			name:       "mise resolves the configured tool's binary through PATH",
			invocation: pinned,
			env:        map[string]string{"PATH": filepath.Dir(linkedOmp) + ":" + filepath.Dir(mise)},
			want:       "'" + mise + "' x github:sjawhar/oh-my-pi@18.1.21 -- " + realOmp,
		},
		{
			name:       "any other invocation",
			invocation: "omp --mode rpc",
			env:        map[string]string{"PATH": filepath.Dir(mise)},
			refusal:    "OMP invocation must be 'mise x <tool> -- omp'. Set LEGION_OMP_PATH to an absolute executable path.",
		},
		{
			name:       "no mise anywhere",
			invocation: pinned,
			env:        map[string]string{"PATH": "/nonexistent"},
			refusal:    "Missing required daemon tool: mise (set LEGION_MISE_PATH to an absolute executable path)",
		},
		{
			name:       "LEGION_MISE_PATH relative",
			invocation: pinned,
			env:        map[string]string{"LEGION_MISE_PATH": "mise"},
			refusal:    "LEGION_MISE_PATH must be an absolute executable path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveInvocation(tc.invocation, func(name string) string { return tc.env[name] })
			if tc.refusal != "" {
				if err == nil || err.Error() != tc.refusal {
					t.Fatalf("got (%q, %v), want refusal %q", got, err, tc.refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveInvocation: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The one --append-system-prompt word every pane's OMP gets is the path of the file its prompt was
// written to, one shell word however the path is spelled, and never the text or a `$(cat …)` the
// shell would expand into one argument the size of the prompt.
func TestSystemPromptArgument(t *testing.T) {
	for _, tc := range []struct {
		name, path, want string
	}{
		{name: "a plain path", path: "/state/secrets/legion-legion-legion-42-tester-system-prompt.md",
			want: "--append-system-prompt /state/secrets/legion-legion-legion-42-tester-system-prompt.md"},
		{name: "a path the shell would split or expand", path: "/state dir/$HOME/system-prompt.md",
			want: `--append-system-prompt '/state dir/$HOME/system-prompt.md'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SystemPromptArgument(tc.path); got != tc.want {
				t.Errorf("SystemPromptArgument(%q) = %s, want %s", tc.path, got, tc.want)
			}
		})
	}
}

// WithPrefix quotes each prefix element on its own and leaves the invocation — already a
// shell fragment — as it is (runtime-tmux.ts:112-118).
func TestWithPrefix(t *testing.T) {
	invocation := "/usr/bin/mise x 'github:sjawhar/oh-my-pi@18' -- omp"
	if got := WithPrefix(nil, invocation); got != invocation {
		t.Errorf("no prefix: got %q", got)
	}
	got := WithPrefix([]string{"env", "OMP_PROFILE=legion", "--", "nice", "-n 5"}, invocation)
	if want := "env 'OMP_PROFILE=legion' -- nice '-n 5' " + invocation; got != want {
		t.Errorf("prefix: got %q, want %q", got, want)
	}
}
