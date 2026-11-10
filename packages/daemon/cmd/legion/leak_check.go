package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

const (
	leakCheckPrePushUsage = "usage: legion leak-check pre-push <remote> <url>  (git's pre-push hook runs it, never a person: git's update lines on standard input, the repository as the working directory)"
	leakCheckTextUsage    = "usage: legion leak-check text <kind>  (the text on standard input, <kind> what a refusal calls the write)"
)

// leakCheckCommands are the two shapes a write reaches the guard in from a shell: a push, which
// git's pre-push hook runs, and a piece of text, which anything about to publish one runs.
var leakCheckCommands = map[string]command{
	"pre-push": runLeakCheckPrePush,
	"text":     runLeakCheckText,
}

// leakCheckStdin is where the command reads the write it checks — git's update lines for a
// pre-push, the text itself for a text: the process's own standard input, which a test replaces.
var leakCheckStdin io.Reader = os.Stdin

// runLeakCheck is `legion leak-check`, the image's shell door to packages/daemon/internal/leakguard:
// the guard the daemon runs in process is the one a pod's git hook and a pod's own scripts run
// here, so one set of rules answers for every write.
func runLeakCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "leak-check", leakCheckCommands, args, stdout, stderr)
}

// runLeakCheckPrePush is the hook's half: git hands it the remote's name and URL and the update
// lines it computed, and the repository it runs the hook in is the working directory, which is
// the repository the guard reads the push's commits from.
func runLeakCheckPrePush(ctx context.Context, args []string, _, stderr io.Writer) int {
	flags := newFlags("leak-check pre-push", leakCheckPrePushUsage, stderr)
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 2 {
		flags.Usage()
		return leakguard.StatusSetupFailure
	}
	remote, url := flags.Arg(0), flags.Arg(1)
	what := leakguard.PushName(remote)
	updates, err := leakguard.ParseUpdates(leakCheckStdin)
	if err != nil {
		return leakCheckReport(stderr, "", &leakguard.SetupFailure{What: what, Why: "git's update lines could not be read", Err: err})
	}
	dir, err := os.Getwd()
	if err != nil {
		return leakCheckReport(stderr, "", &leakguard.SetupFailure{What: what, Why: "the repository the hook runs in could not be read", Err: err})
	}
	guard := leakguard.New(leakguard.Options{})
	return leakCheckReport(stderr, "", guard.Push(ctx, leakguard.PushRequest{
		Dir:     dir,
		Remote:  remote,
		URL:     url,
		Updates: updates,
	}))
}

// runLeakCheckText is the text half: the whole of standard input is the text, its first line the
// title and the rest the body, and kind is what a refusal calls it ("the pull request body").
// It names no destination repository, which the guard reads as public and so scans, the
// fail-closed answer: a caller that runs this knows it is about to publish the text.
func runLeakCheckText(ctx context.Context, args []string, _, stderr io.Writer) int {
	flags := newFlags("leak-check text", leakCheckTextUsage, stderr)
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return leakguard.StatusSetupFailure
	}
	kind := flags.Arg(0)
	text, err := io.ReadAll(leakCheckStdin)
	if err != nil {
		return leakCheckReport(stderr, kind, &leakguard.SetupFailure{What: kind, Why: "the text could not be read", Err: err})
	}
	guard := leakguard.New(leakguard.Options{})
	return leakCheckReport(stderr, kind, guard.Text(ctx, leakguard.TextRequest{Text: string(text)}))
}

// leakCheckReport prints one answer of the guard and is the status to exit with. A cleared write
// prints nothing. A refusal prints the guard's own report, which names the record and the class
// and never the text that matched; a setup failure prints the guard's own line, which says it is
// one, so a person reading a log tells a broken pod from a write that names something private.
//
// The statuses are the shared public leak check's own vocabulary, which is what a pre-push hook
// needs, and they are the guard's exported constants rather than a second copy of the three
// numbers: 0 for a write it cleared, 3 for one it refused, and 2 for a check that could not be
// made to run. A usage error above is 2 as well — a call nobody could read is a call nobody has
// checked — and so is an error of neither of the guard's two kinds, which is the fail-closed
// half of the pair.
//
// what renames the write where the caller has a better word for it than the guard's own — the
// text shape's kind — and leaves the guard's wording alone when it is empty, which is what the
// push shape wants: the guard already calls it "the push to origin".
func leakCheckReport(stderr io.Writer, what string, err error) int {
	if err == nil {
		return leakguard.StatusClean
	}
	var refusal *leakguard.Refusal
	if errors.As(err, &refusal) {
		refusal.What = cmp.Or(what, refusal.What)
		fmt.Fprintln(stderr, refusal.Error())
		return leakguard.StatusRefused
	}
	var setup *leakguard.SetupFailure
	if errors.As(err, &setup) {
		setup.What = cmp.Or(what, setup.What)
		fmt.Fprintln(stderr, setup.Error())
		return leakguard.StatusSetupFailure
	}
	fmt.Fprintln(stderr, (&leakguard.SetupFailure{
		What: cmp.Or(what, "the write"),
		Why:  "the guard answered with neither a refusal nor a setup failure",
		Err:  err,
	}).Error())
	return leakguard.StatusSetupFailure
}
