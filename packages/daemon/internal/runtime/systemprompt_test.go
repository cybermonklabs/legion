package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// A claim's prompt file is its own under the secrets directory, so the daemon prunes it with the
// claim's other files, and it is written as every secret file there is (WriteSecretFile).
func TestWriteSystemPromptWritesTheClaimsOwnFile(t *testing.T) {
	state := t.TempDir()
	path, err := WriteSystemPrompt(state, "legion-legion-legion-42-tester", "the prompt")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(state, "secrets", "legion-legion-legion-42-tester-system-prompt.md")
	if path != want {
		t.Fatalf("WriteSystemPrompt wrote %s, want %s", path, want)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "the prompt" {
		t.Fatalf("the prompt file holds %q, %v; want the prompt", got, err)
	}
}
