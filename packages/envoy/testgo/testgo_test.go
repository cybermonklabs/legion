package testgo

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stalledGoEnv names the directory of the stand-in go TestTheStalledGoCommand runs; it is set only
// in the child TestAStalledGoCommandShowsItsStandardErrorWhenTheTestTimesOut starts.
const stalledGoEnv = "TESTGO_STALLED_GO"

// A go command that stalls, as a module download that never ends does, is named in the test's
// output when the test times out: Output hands go's standard error to the test binary's as go
// writes it, where a buffer read once go returns is lost with the timeout's panic. The stand-in go
// writes the line a module download starts with, then waits until the test binary that started it
// has exited, so it outlives that binary by a tenth of a second at most.
func TestAStalledGoCommandShowsItsStandardErrorWhenTheTestTimesOut(t *testing.T) {
	if os.Getenv(stalledGoEnv) != "" {
		t.Skip("the child runs TestTheStalledGoCommand, not this test")
	}
	dir := t.TempDir()
	stand := "#!/bin/sh\n" +
		"echo 'go: downloading example.com/stalled v1.0.0' >&2\n" +
		"parent=$PPID\n" +
		"for _ in $(seq 1 600); do\n" +
		"  kill -0 \"$parent\" 2>/dev/null || exit 1\n" +
		"  sleep 0.1\n" +
		"done\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(stand), 0o755); err != nil {
		t.Fatal(err)
	}
	killing, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(killing, os.Args[0], "-test.run=^TestTheStalledGoCommand$", "-test.timeout=3s")
	cmd.Env = append(os.Environ(), stalledGoEnv+"="+dir)
	// The stand-in holds none of this command's pipes: Output's writers are no *os.File, so exec
	// gives go pipes of the child's own, and the child's exit ends CombinedOutput. WaitDelay bounds
	// that wait only if something the child starts ever inherits these pipes.
	cmd.WaitDelay = 10 * time.Second
	output, err := cmd.CombinedOutput()
	if killing.Err() != nil {
		t.Fatalf("the child outlived its 3s -test.timeout and was killed a minute in:\n%s", output)
	}
	if err == nil || !strings.Contains(string(output), "panic: test timed out") {
		t.Fatalf("the child did not end in its -test.timeout (exit: %v):\n%s", err, output)
	}
	if !strings.Contains(string(output), "go: downloading example.com/stalled v1.0.0") {
		t.Fatalf("the child's output, which ends in its test timeout, does not show the stalled go's downloading line:\n%s", output)
	}
}

// TestTheStalledGoCommand runs the stand-in go its parent wrote, which never returns, so the
// child's -test.timeout ends it.
func TestTheStalledGoCommand(t *testing.T) {
	dir := os.Getenv(stalledGoEnv)
	if dir == "" {
		t.Skip("run by TestAStalledGoCommandShowsItsStandardErrorWhenTheTestTimesOut, in a child of its own")
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, _ = Output(exec.Command("go", "build", "."))
	t.Fatal("the stand-in go returned; it waits until this binary has exited")
}

// Output returns go's standard output alone, hands its standard error to a writer the caller set
// as well, fails with the exit status and both streams, and refuses a command whose Stdout is set.
func TestOutputReturnsStandardOutputAndFailsWithBothStreams(t *testing.T) {
	dir := t.TempDir()
	stand := "#!/bin/sh\necho to-stdout\necho to-stderr >&2\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(stand), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(dir, "go"), "version")
	var callers strings.Builder
	cmd.Stderr = &callers
	stdout, err := Output(cmd)
	if got := string(stdout); got != "to-stdout\n" {
		t.Errorf("standard output = %q, want only what go wrote there", got)
	}
	if got := callers.String(); got != "to-stderr\n" {
		t.Errorf("the caller's standard error got %q, want what go wrote there", got)
	}
	if err == nil || !strings.Contains(err.Error(), "exit status 3\n") ||
		!strings.Contains(err.Error(), "to-stdout\n") || !strings.Contains(err.Error(), "to-stderr\n") {
		t.Errorf("the error is %v, want the exit status and both streams", err)
	}
	// A go that exits 0, so only the refusal itself can make this call fail: run, it returns no
	// error.
	succeeding := filepath.Join(dir, "go-succeeds")
	if err := os.WriteFile(succeeding, []byte("#!/bin/sh\necho ran\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = Output(&exec.Cmd{Path: succeeding, Stdout: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "cmd.Stdout is set") {
		t.Errorf("Output of a command whose Stdout was set returned %v, want it refused", err)
	}
}
