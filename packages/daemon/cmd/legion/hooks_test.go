package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// `legion start --check-config` reads the words of a hook that names a `prompt_file` as boot does
// (daemon.CheckStart): a file that holds words passes, and a missing one is refused with exit 1,
// no OK line, and boot's own sentence naming the hook and the operator's path. The loader reads
// no file it names, so this check is the one proof that a hook's words can come from a file;
// without the read, a deployment would start on a hook whose prompt says nothing.
func TestStartCheckConfigReadsAHooksPromptFile(t *testing.T) {
	legionState(t)
	dir := t.TempDir()
	present, missing := filepath.Join(dir, "stuck.md"), filepath.Join(dir, "missing.md")
	if err := os.WriteFile(present, []byte("Say what is in your way.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	config, _ := workflowConfig(t, 13370, hookNaming(present))
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)
	if want := demoCheckConfigOK + tmuxCapabilityGaps; code != 0 || out.String() != want || errb.Len() != 0 {
		t.Fatalf("a prompt file that is there: exit code = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), want)
	}

	config, _ = workflowConfig(t, 13370, hookNaming(missing))
	out.Reset()
	errb.Reset()
	code = run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)
	want := "legion start: hook stuck prompt_file " + missing + " could not be read: open " + missing + ": no such file or directory\n"
	if code != 1 || out.Len() != 0 || errb.String() != want {
		t.Fatalf("a missing prompt file: exit code = %d, stdout %q, stderr %q; want 1, nothing, and %q", code, out.String(), errb.String(), want)
	}
}

// hookNaming is a `hooks:` block of one hook that reads its words from promptFile.
func hookNaming(promptFile string) string {
	return "hooks:\n  - name: stuck\n    on: work_back\n    every: 5\n    roles: [implementer]\n    prompt_file: " + promptFile + "\n"
}
