package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// writePromptFiles writes each name's text under dir and returns the paths, in the order given.
func writePromptFiles(t *testing.T, dir string, files ...[2]string) []string {
	t.Helper()
	paths := make([]string, 0, len(files))
	for _, file := range files {
		path := filepath.Join(dir, file[0])
		if err := os.WriteFile(path, []byte(file[1]), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

// The one text every launch's agent reads from its prompt file: the role prompt files
// concatenated, the addressing text as written (no shell reads it any more, so nothing in it is
// escaped), the deployment instructions, then one instructions file per arm in declaration order,
// joined by blank lines, each file's trailing newlines dropped. A launch in no experiment ends at
// the deployment's own.
func TestComposePrompt(t *testing.T) {
	dir := t.TempDir()
	roles := writePromptFiles(t, dir, [2]string{"common.md", "Common.\n"}, [2]string{"tester.md", "Tester.\n\n"})
	extra := writePromptFiles(t, dir, [2]string{"deployment.md", "Deployment.\n"},
		[2]string{"memory.md", "Memory.\n"}, [2]string{"long.md", "Long plans.\n"})
	addressing := "Your topic is `notifications.role.legion-omp-LEGION-42-tester` \"$HOME\" \\"
	for _, tc := range []struct {
		name  string
		parts PromptParts
		want  string
	}{
		{name: "role prompt alone", parts: PromptParts{RolePromptPaths: roles[:1]}, want: "Common."},
		{name: "role prompts, addressing and the deployment's instructions",
			parts: PromptParts{RolePromptPaths: roles, Addressing: addressing, DeploymentInstructionsPath: extra[0]},
			want:  "Common.\nTester.\n\n" + addressing + "\n\nDeployment."},
		{name: "every fragment, one variant instructions file per arm, in declaration order",
			parts: PromptParts{RolePromptPaths: roles, Addressing: addressing, DeploymentInstructionsPath: extra[0], VariantInstructionsPaths: extra[1:]},
			want:  "Common.\nTester.\n\n" + addressing + "\n\nDeployment.\n\nMemory.\n\nLong plans."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ComposePrompt(tc.parts)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("ComposePrompt =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
	if _, err := ComposePrompt(PromptParts{RolePromptPaths: []string{filepath.Join(dir, "missing.md")}}); !errors.Is(err, os.ErrNotExist) ||
		!strings.HasPrefix(err.Error(), "prompt file: ") {
		t.Errorf("ComposePrompt over a missing role prompt = %v, want the prompt file's own error", err)
	}
}

// A prompt larger than the context the deployment gives the role's model is refused with the
// role, the prompt's size and the bound; one at the bound, and every prompt of a role the
// deployment gives no bound, is composed.
func TestSystemPromptRefusesAPromptLargerThanTheModelsContext(t *testing.T) {
	role := writePromptFiles(t, t.TempDir(), [2]string{"architect.md", strings.Repeat("x", 2000)})
	spec := SpawnSpec{Role: claim.RoleArchitect, Prompt: PromptParts{RolePromptPaths: role}}

	for _, bound := range []int{0, 2000} {
		spec.ModelContextBytes = bound
		if got, err := SystemPrompt(spec); err != nil || len(got) != 2000 {
			t.Errorf("SystemPrompt under a bound of %d = %d bytes, %v; want the whole 2000-byte prompt", bound, len(got), err)
		}
	}

	spec.ModelContextBytes = 1999
	_, err := SystemPrompt(spec)
	var refused *PromptTooLarge
	if !errors.As(err, &refused) {
		t.Fatalf("SystemPrompt under a bound of 1999 = %v, want PromptTooLarge", err)
	}
	if *refused != (PromptTooLarge{Role: claim.RoleArchitect, Bytes: 2000, Bound: 1999}) {
		t.Errorf("the refusal = %+v, want the architect's 2000 bytes over 1999", *refused)
	}
	const want = "the architect's system prompt is 2000 bytes, larger than the 1999 bytes model_context_bytes.architect gives its model's context"
	if err.Error() != want {
		t.Errorf("the refusal reads %q, want %q", err, want)
	}
}

// A tmux launch's prompt file is the claim's own under the secrets directory, so the daemon prunes
// it with the claim's other files, and it is written owner-only and whole.
func TestWriteSystemPromptWritesTheClaimsOwnFile(t *testing.T) {
	state := t.TempDir()
	path := SystemPromptPath(state, "legion-legion-legion-42-tester")
	if want := filepath.Join(state, "secrets", "legion-legion-legion-42-tester-system-prompt.md"); path != want {
		t.Fatalf("SystemPromptPath = %s, want %s", path, want)
	}
	for _, text := range []string{"first prompt", "the second, longer prompt"} {
		if err := WriteSystemPrompt(path, text); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != text {
			t.Fatalf("the prompt file holds %q, %v; want %q", got, err, text)
		}
	}
	for _, p := range []string{path, filepath.Dir(path)} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s has mode %#o, want owner-only", p, mode)
		}
	}
}
