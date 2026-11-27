package sandbox

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A system prompt longer than Linux lets one argv string be (MAX_ARG_STRLEN, 131072 bytes with its
// NUL) launches in a pod all the same, for an Oh My Pi role and a Claude Code role alike, because
// no argv string carries it: the agent's argv names a file of the generation's own directory —
// Oh My Pi's `--append-system-prompt <path>`, Claude Code's `--append-system-prompt-file <path>` —
// and the start command carries the whole prompt as that file, which the role's launcher writes
// there before the agent starts. Measured at 200000 bytes: one role prompt file of that size with
// no trailing newline and no other part, so the composed prompt is the file's text exactly. No
// argv string of either start command is longer than 4096 bytes, so the bound is nowhere near.
func TestAPromptOver131072BytesLaunchesInAPodAsAPath(t *testing.T) {
	t.Parallel()
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	const size = 200_000
	line, last := "A line of the role's prompt, which grows with the deployment's instructions.\n", "The prompt's own last line."
	text := strings.Repeat(line, size/len(line)+1)[:size-len(last)-1] + "\n" + last
	rolePrompt := filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(rolePrompt, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, flag string
		spec       runtime.SpawnSpec
	}{
		{"an Oh My Pi role", "--append-system-prompt", workerSpec(t)},
		{"a Claude Code role", "--append-system-prompt-file", claudeWorkerSpec(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			spec.Prompt = runtime.PromptParts{RolePromptPaths: []string{rolePrompt}}
			l, err := r.prepare(spec)
			if err != nil {
				t.Fatalf("prepare refused a launch whose system prompt is %d bytes: %v", len(text), err)
			}
			start := launcherCommand(l, r)
			for i, arg := range start.Argv {
				if len(arg) > 4096 {
					t.Errorf("word %d of the start command's argv is %d bytes, want none over 4096: %.80q", i, len(arg), arg)
				}
			}
			at := slices.Index(start.Argv, tc.flag)
			if at < 0 || at+1 == len(start.Argv) {
				t.Fatalf("the start command's argv names no %s value: %q", tc.flag, start.Argv)
			}
			path := start.Argv[at+1]
			if dir := generationDir(spec.Generation); !filepath.IsAbs(path) || filepath.Dir(path) != dir {
				t.Errorf("%s names %q, want an absolute path of the generation's own directory %s", tc.flag, path, dir)
			}
			carried, ok := start.Files[filepath.Base(path)]
			switch {
			case !ok:
				t.Errorf("the start command carries the files %v, want %s among them, the file %s names",
					slices.Sorted(maps.Keys(start.Files)), filepath.Base(path), tc.flag)
			case carried != text:
				t.Errorf("the start command carries %s as %d bytes, want the whole %d-byte prompt, byte for byte",
					filepath.Base(path), len(carried), len(text))
			}
		})
	}
}
