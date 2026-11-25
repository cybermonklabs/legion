package gcp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// scriptTimeout bounds one command a test runs: a script under test or a tool it calls. Each runs
// in seconds; one that has not ended by then is waiting on something that will not come (a tool
// this machine lacks, a stub that never answers), and is killed so the test fails naming it rather
// than holding the whole binary to go test's -timeout.
const scriptTimeout = 2 * time.Minute

// scriptContext is the context a command of t runs in: t's own (t.Context()), so it ends with the
// test, with a deadline of scriptTimeout, or untilTheTestDeadline's when that comes first.
func scriptContext(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(untilTheTestDeadline(t), scriptTimeout)
	t.Cleanup(cancel)
	return ctx
}

// deadlineMargin is how long before go test's own deadline (-timeout) a command still running is
// killed, so the test fails naming it rather than the binary panicking with every goroutine's stack.
const deadlineMargin = 30 * time.Second

// untilTheTestDeadline is t's own context (t.Context()) with a deadline deadlineMargin before t's
// own, when it has one: the bound of a command that may rightly take minutes, such as a go install
// on an empty build cache, which scriptTimeout would cut short.
func untilTheTestDeadline(t testing.TB) context.Context {
	t.Helper()
	deadline, ok := t.(interface{ Deadline() (time.Time, bool) })
	if !ok {
		return t.Context()
	}
	at, set := deadline.Deadline()
	if !set {
		return t.Context()
	}
	ctx, cancel := context.WithDeadline(t.Context(), at.Add(-deadlineMargin))
	t.Cleanup(cancel)
	return ctx
}

// shellCommand is the command that runs the golden's shell scripts and bootcmd entries in these
// tests, with args after it: /bin/sh, the shell cloud-init and systemd run them with, or the words
// of ENVOY_TEST_SHELL, as "bash", to run the same rows under another shell. Container-Optimized OS
// ships both dash and bash (COS 133's cos-package-info.json lists app-shells/dash 0.5.13.5 and
// app-shells/bash 5.3_p3-r3), so the rows run under both.
//
// It runs in t's scriptContext, so no caller can start a shell without a deadline: the shell is
// killed when t ends, or at scriptTimeout, or deadlineMargin before go test's -timeout, whichever
// comes first, and a process it started that still holds its output is given WaitDelay to let go
// before Wait returns. A shell killed at its deadline fails t, naming the deadline and the script,
// rather than holding the whole binary to -timeout. It is the one way these tests start a shell,
// inside the package and out (export_test.go).
func shellCommand(t testing.TB, args ...string) *exec.Cmd {
	t.Helper()
	shell := strings.Fields(os.Getenv("ENVOY_TEST_SHELL"))
	if len(shell) == 0 {
		shell = []string{"/bin/sh"}
	}
	ctx := scriptContext(t)
	cmd := exec.CommandContext(ctx, shell[0], append(shell[1:], args...)...)
	cmd.Cancel = func() error {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Errorf("%s ran past its deadline (%s, or %s before go test's -timeout) and was killed: %s",
				shell[0], scriptTimeout, deadlineMargin, scriptHead(args))
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// scriptHead names a shell's command in a failure: its arguments, with a -c script cut to its
// first line and at most 120 bytes, since the scripts these tests run are pages long.
func scriptHead(args []string) string {
	words := make([]string, len(args))
	for i, arg := range args {
		line, _, cut := strings.Cut(arg, "\n")
		if len(line) > 120 {
			line, cut = line[:120], true
		}
		if cut {
			line += " …"
		}
		words[i] = line
	}
	return strings.Join(words, " ")
}

// killedBySignal says whether cmd, which has run, ended on a signal rather than exiting: in these
// tests that is shellCommand's kill at its deadline, since nothing else signals the shell.
func killedBySignal(cmd *exec.Cmd) bool {
	return cmd.ProcessState != nil && !cmd.ProcessState.Exited()
}
