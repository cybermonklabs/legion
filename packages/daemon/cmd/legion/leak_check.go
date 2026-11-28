package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

const (
	leakCheckPrePushUsage = "usage: legion leak-check pre-push <remote> <url>  (git's pre-push hook runs it, never a person: git's update lines on standard input, the repository as the working directory)"
	leakCheckTextUsage    = "usage: legion leak-check text <kind>  (the text on standard input, <kind> what a refusal calls the write)"
)

// leakCheckStdin is where the command reads the write it checks — git's update lines for a
// pre-push, the text itself for a text: the process's own standard input, which a test replaces.
var leakCheckStdin io.Reader = os.Stdin

// newLeakCheckCommand is `legion leak-check`, the image's shell door to
// packages/daemon/internal/leakguard: the guard the daemon runs in process is the one a pod's git
// hook and a pod's own scripts run here, so one set of rules answers for every write. Its two
// commands are the two shapes a write reaches the guard in from a shell: a push, which git's
// pre-push hook runs, and a piece of text, which anything about to publish one runs.
func newLeakCheckCommand(stderr io.Writer) *cobra.Command {
	return group(&cobra.Command{
		Use:    "leak-check",
		Short:  "run the leak guard over a write the shell is about to make: git's pre-push hook runs its pre-push form, and the rule file's port test runs its text form (never a person)",
		Hidden: true,
	},
		leakCheckCommand("pre-push <remote> <url>", 2, leakCheckPrePushUsage, "check a push before git makes it", runLeakCheckPrePush, stderr),
		leakCheckCommand("text <kind>", 1, leakCheckTextUsage, "check a text about to be published", runLeakCheckText, stderr),
	)
}

// leakCheckCommand is one shape of `legion leak-check`: it takes words words, and any other count
// is a usage error, the guard's StatusSetupFailure, with usage on stderr.
//
// The guard starts each of its children, the shared check among them, in a process group of its
// own (leakguard's Guard.run), which none of the terminal's signals reach: git runs its pre-push
// hook, and so this command, in the terminal's foreground group with git. So the guard runs under
// a context that ends on a SIGINT or a SIGTERM (main's signal.NotifyContext, which cobra hands
// every command) and on a SIGHUP, which this command catches while the shape runs and reports
// (hangupCancels), and the end of that context stops the child's group. Without the SIGHUP, a
// pane that closes during a push would kill this command where it stood and leave the shared check
// running, reparented and waited for by nobody. A signal that ended the context before the guard's
// answer is reported ends the command with 128+<signal> rather than with the setup failure its
// stopped check reads as (leakCheckReport), which refuses the push either way.
func leakCheckCommand(use string, words int, usage, short string, do func(ctx context.Context, args []string, stderr io.Writer) int, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    use,
		Short:  short,
		Hidden: true,
		RunE: runs(func(ctx context.Context, args []string) int {
			if len(args) != words {
				fmt.Fprintln(stderr, usage)
				return leakguard.StatusSetupFailure
			}
			guarded, stop := hangupCancels(ctx)
			defer stop()
			return do(guarded, args, stderr)
		}),
	}
}

// runLeakCheckPrePush is the hook's half: git hands it the remote's name and URL and the update
// lines it computed, and the repository it runs the hook in is the working directory, which is
// the repository the guard reads the push's commits from.
func runLeakCheckPrePush(ctx context.Context, args []string, stderr io.Writer) int {
	remote, url := args[0], args[1]
	what := leakguard.PushName(remote)
	updates, err := leakguard.ParseUpdates(leakCheckStdin)
	if err != nil {
		return leakCheckReport(ctx, stderr, "", &leakguard.SetupFailure{What: what, Why: "git's update lines could not be read", Err: err})
	}
	dir, err := os.Getwd()
	if err != nil {
		return leakCheckReport(ctx, stderr, "", &leakguard.SetupFailure{What: what, Why: "the repository the hook runs in could not be read", Err: err})
	}
	guard := leakguard.New(leakguard.Options{Curl: leakguardCurl})
	return leakCheckReport(ctx, stderr, "", guard.Push(ctx, leakguard.PushRequest{
		Dir:     dir,
		Remote:  remote,
		URL:     url,
		Updates: updates,
	}))
}

// runLeakCheckText is the text half: the whole of standard input is the text, its first line the
// title and the rest the body, and kind is what a refusal calls it ("the pull request body").
// It names no destination repository, which the guard reads as public and so scans, the
// fail-closed answer: a caller that runs this knows it is about to publish the text. Its one
// caller today is .github/scripts/check-private-names-port.test.sh, which drives this door and
// the frozen check-private-names.sh over one fixture set and compares their verdicts.
func runLeakCheckText(ctx context.Context, args []string, stderr io.Writer) int {
	kind := args[0]
	text, err := io.ReadAll(leakCheckStdin)
	if err != nil {
		return leakCheckReport(ctx, stderr, kind, &leakguard.SetupFailure{What: kind, Why: "the text could not be read", Err: err})
	}
	guard := leakguard.New(leakguard.Options{Curl: leakguardCurl})
	return leakCheckReport(ctx, stderr, kind, guard.Text(ctx, leakguard.TextRequest{Text: string(text)}))
}

// leakCheckReport prints one answer of the guard and is the status to exit with. A cleared write
// prints nothing here, and so does one cleared with no rule file named: the guard then ran the
// shared check alone, where one runs, and the daemon's boot has said once that the deployment
// names no rule file.
// A refusal prints the guard's own report. One by the private-name rules — the deployment's rule
// file, which LEGION_PRIVATE_NAME_RULES_FILE names — shows, for each finding, the record, the
// rule's description, the text that matched and the line it stands on: the agent about to make
// the write wrote that text, so there is nothing in it to hide from the agent the refusal is shown
// to. A setup failure prints the guard's own line, which says it is one, so a person reading a log
// tells a broken pod from a write that names something private; and a not-found prints the
// guard's line for a destination GitHub answers 404 for, which is neither.
//
// The statuses are the shared public leak check's own vocabulary, which is what a pre-push hook
// needs, and they are the guard's exported constants rather than a second copy of the three
// numbers: 0 for a write it cleared, 3 for one it refused, and 2 for a check that could not be
// made to run and for a destination GitHub answers 404 for. A usage error above is 2 as well — a
// call nobody could read is a call nobody has checked — and so is an error of none of the guard's
// three kinds, which fails closed.
//
// what renames the write where the caller has a better word for it than the guard's own — the
// text shape's kind — and leaves the guard's wording alone when it is empty, which is what the
// push shape wants: the guard already calls it "the push to origin".
//
// A signal that ended ctx — main's SIGINT or SIGTERM, or the SIGHUP leakCheckCommand catches — is
// the status whatever the guard answered, 128+<signal> (endedBySignal), and nothing is printed:
// the guard's answer is then the setup failure of a check the signal stopped, which says nothing
// about the write and is not reported as if it did. The push is refused either way.
func leakCheckReport(ctx context.Context, stderr io.Writer, what string, err error) int {
	if status, ok := signalled(ctx); ok {
		return status
	}
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
	var notFound *leakguard.NotFound
	if errors.As(err, &notFound) {
		notFound.What = cmp.Or(what, notFound.What)
		fmt.Fprintln(stderr, notFound.Error())
		return leakguard.StatusSetupFailure
	}
	fmt.Fprintln(stderr, (&leakguard.SetupFailure{
		What: cmp.Or(what, "the write"),
		Why:  "the guard answered with none of a refusal, a setup failure and a not-found answer",
		Err:  err,
	}).Error())
	return leakguard.StatusSetupFailure
}
