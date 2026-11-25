package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// jjUsage is the help of `legion jj`, which reads its own arguments (passThrough), --help among
// them.
const jjUsage = `usage: legion jj --real <jj> -- <jj argument>…
       legion jj --after <directory> -- <jj argument>…

A jj wrapper runs this command, never a person. It runs a jj with the arguments after --.
Before a jj git push, it first sets the author of a commit the push sends from that
commit's first Legion-Role trailer: the identity LEGION_ROLE_IDENTITIES gives that role, by
one jj metaedit --update-author for each role, where its rules let it
(docs/deployment.md). It prints one line for each commit it rewrote.
With --after, it first expands the arguments as jj does, through jj's aliases and
ui.default-command, so a command that jj runs as a jj git push is one too.

  --real <jj>          run <jj>, the real jj (the pod's form: /usr/local/bin/jj)
  --after <directory>  run the next jj on PATH after <directory>, the wrapper's own directory
                       (the form of the jj first on a tmux pane's PATH)
`

// wrappedJJAt is legion jj's loop guard's variable, as wrappedGitAt is legion git's: a chain of
// jj programs (wrapperChain). Each run of `legion jj` sets it in the environment of every jj it
// runs, the step's own commands and the jj it replaces itself with: to the chain pickAfter gives
// (--after), or to the real jj alone (--real). It decides which jj a legion jj --after runs
// (pickAfter), and not whether the step runs. A jj that a jj command starts, such as the inner jj
// of `jj util exec -- jj git push`, finds it set by the outer command's legion jj, listing the
// real jj, which is on PATH: to pickAfter that is a re-entry, as is a push that a host's jj
// wrapper running jj by PATH brings back into the pane's jj. Only the first needs the step, and
// wrappedJJStepped tells the two apart. Its name starts with neither LEGION_ nor JJ_: fixtureenv
// drops both prefixes, and jj reads JJ_ variables of its own.
const wrappedJJAt = "WRAPPED_JJ_AT"

// wrappedJJStepped is the variable that marks a command legion jj has read already: a legion jj
// sets it to the digest of the words it runs the command with (jjStepped), in the environment of
// that command alone, after it ran the step before a push, printed the line of a table it cannot
// use, or read the command as no push the step runs before, expanded or not. A legion jj whose
// command's own words have that digest runs it at once, with no read and no step: it is the same
// command, come back through a host's jj wrapper that runs jj by PATH, or through a second Legion
// bin directory on PATH, as a controller started from a pane has, so it reads the aliases once
// and prints a line that it cannot expand the command once. A command that a jj command starts
// with other words, such as the inner `jj git push` of `jj util exec -- jj git push`, is read
// afresh, and a push among them runs the step. The step's own commands are no pushes the step
// runs before (jjPush.steps), so they need no mark. A host's jj wrapper that changes the words of
// a push makes the step run once more, before the words that wrapper runs; that is harmless,
// since the step is idempotent: its second run finds nothing to change. Its name starts with
// neither LEGION_ nor JJ_, as wrappedJJAt's does.
const wrappedJJStepped = "WRAPPED_JJ_STEPPED"

// jjStepped is the value of wrappedJJStepped for a command of words: the hex of the SHA-256 of the
// words joined by NUL, which no word holds.
func jjStepped(words []string) string {
	sum := sha256.Sum256([]byte(strings.Join(words, "\x00")))
	return hex.EncodeToString(sum[:])
}

// runJJ is `legion jj`. Both of Legion's jj wrappers run it: the pod's /opt/legion/bin/jj
// (packages/daemon/docker/jj) hands it each line it expands into a `jj git push`, with --real
// /usr/local/bin/jj, after its two setup checks, and the jj first on a tmux pane's PATH
// (workerbin.Install's bin/jj) hands it every command with --after and its own directory. It reads
// its arguments and puts back the signals' actions as runGit does (startWrapper), chooses the jj to
// run (pickAfter for --after), runs the author step before a push (jjStep), and replaces itself
// with that jj (execWrapped), so a stop of the process group and its budgets reach jj as before.
// A process under shell completion (jjCompleting), a process with no table
// (runtime.RoleIdentitiesVariable unset, as a controller's is), and a command whose
// wrappedJJStepped is the digest of its own words (jjStepped), the same command come back after
// legion jj read it, run that jj at once, with nothing printed and no other process started. Under
// --after, every other command is first expanded as jj expands it (expandJJ): a command whose
// command word is a built-in command, `jj git push` among them, and one with no command word that
// holds a word of help or version (jjHelpWord), with no read of the aliases, and any other, a bare
// `jj` included, after one read of jj's aliases and ui.default-command (readJJAliases), which
// prints jjOnceOnly of its standard error. A command that is no push the step runs before
// (readJJPush), as the role wrote it or as jj expands it, runs that jj at once with the role's own
// words, byte for byte, which jj then expands itself, and with wrappedJJStepped set to their
// digest; one that legion jj cannot expand (jjUnexpandable) prints one line first, `legion jj:
// could not expand <what>: <why>; the command runs as written`. The --real form expands nothing:
// its line is the pod's wrapper's expansion already. A table that is set but cannot be used
// (runtime.ReadRoleIdentities) prints one line before a push, which then runs with the role's own
// words, as jj expands them, and with no step.
// A push the step runs before, written or expanded, is the line the step reads and the line the
// real push runs with the step's words. Under --after, the step's first command is a read of the
// configuration of the repository the push loads (jjStep.readConfiguration), which prints what jj
// prints once there, such as `Your repo appears to have been copied`, where the pod's wrapper's
// read of git.executable-path prints it in a pod; a push that the read of the aliases expanded and
// that names no -R gets none, since that read loaded the same repository. The push that runs after
// the step gets each revision argument in the words the step gives it (jjStep.given), the role's
// own where they still name the commits jjStep.revisionGiven says it names; the line of an
// argument it gets as the ids of its commits comes after the step's lines of its rewrites
// (report). The step takes at most one snapshot of the working copy, the one the push would take
// itself, as jjStep.rewrite says, and jjStep.snapshotted says how the real push runs after it.
// After a step that changes nothing more (errJJAsWritten), past a dry run it cannot read or a push
// whose command would pass jjArgMax bytes (jjStep.checkArgMax), only a commit that a metaedit of
// the step moved counts as replaced, and every argument is held to the commits it named before the
// step only where a metaedit ran.
// The push that runs after the step, or after the line of a table it cannot use, gets
// wrappedJJStepped set to the digest of the words it runs with, the step's --ignore-working-copy
// among them; the step's own commands do not.
//
// An interrupt or a terminate ends legion jj where it stands. The read of the aliases and the
// step's own commands, its read of the configuration and its git reads among them, are children in
// this process's group, so a signal sent to the group ends them too, and the command never starts.
func runJJ(ctx context.Context, args []string, _, stderr io.Writer) int {
	if status, runs := startWrapper(ctx, "jj", jjUsage, args, stderr); !runs {
		return status
	}
	jjArgs := args[3:]
	next, chain := args[1], wrapperChain{ran: []string{args[1]}}
	if args[0] == "--after" {
		pick, err := pickAfter("jj", args[1], os.Getenv("PATH"), os.Getenv(wrappedJJAt))
		if err != nil {
			fmt.Fprintf(stderr, "legion jj: %v\n", err)
			return 127
		}
		next, chain = pick.run, pick.chain
	}
	env := withVariable(os.Environ(), wrappedJJAt, chain.String())
	value, set := os.LookupEnv(runtime.RoleIdentitiesVariable)
	if jjCompleting() || !set || os.Getenv(wrappedJJStepped) == jjStepped(jjArgs) {
		return execWrapped("legion jj", next, jjArgs, env, nil, stderr)
	}
	line, read := jjArgs, false
	if args[0] == "--after" {
		var err error
		if line, read, err = expandJJ(jjArgs, next, env, stderr); err != nil {
			fmt.Fprintf(stderr, "legion jj: %v; the command runs as written\n", err)
		}
	}
	push := readJJPush(line)
	if !push.steps() {
		return execWrapped("legion jj", next, jjArgs, withVariable(env, wrappedJJStepped, jjStepped(jjArgs)), nil, stderr)
	}
	table, why := runtime.ReadRoleIdentities(value)
	if why != "" {
		fmt.Fprintf(stderr, "legion jj: %s is %s; the push runs with no author step\n", runtime.RoleIdentitiesVariable, why)
		return execWrapped("legion jj", next, jjArgs, withVariable(env, wrappedJJStepped, jjStepped(jjArgs)), nil, stderr)
	}
	step := &jjStep{push: push, next: next, env: env, table: table, stderr: stderr}
	var err error
	if args[0] == "--after" && (!read || push.repository) {
		err = step.readConfiguration()
	}
	if err == nil {
		err = step.rewrite()
	}
	step.report()
	var failed jjStepFailure
	switch {
	case errors.As(err, &failed):
		fmt.Fprint(stderr, failed.output)
		return failed.code
	case err != nil && !errors.Is(err, errJJAsWritten):
		fmt.Fprintf(stderr, "legion jj: %v\n", err)
		return 1
	}
	given := step.given(errors.Is(err, errJJAsWritten))
	for _, g := range given {
		if g.line != "" {
			fmt.Fprintln(stderr, g.line)
		}
	}
	words := step.words(given, false)
	if step.snapshotted {
		words = slices.Concat([]string{"--ignore-working-copy"}, words)
	}
	return execWrapped("legion jj", next, words, withVariable(env, wrappedJJStepped, jjStepped(words)), nil, stderr)
}

// jjDryRunFlags are the dry run's own flags, after the push's options and in front of a first `--`
// (jjStep.dryRun): --dry-run, --ignore-working-copy and --color=never.
var jjDryRunFlags = []string{"--dry-run", "--ignore-working-copy", "--color=never"}

// jjDryRunQuiet is the dry run's own configuration, `--config ui.quiet=false`, so that neither a
// flag nor the repository's configuration silences it (jjStep.dryRun). jj keeps a --config only
// from the deepest level of the command line that holds one (readJJPush), and layers what it
// keeps, --config, --config-file and --config-toml alike, in the order written, the later winning:
// at the end of the line, past a --config of the role's in front of `git push`, this one would
// make jj drop the role's, which the push reads, and right after the role's last --config, it
// would lose to a --config-file of the role's after it at that level. So it goes in front of the
// word at configEnd (jjPush.configEnd), the end of the level that holds the role's kept --config,
// where jj keeps the role's --config and this one alike and layers this one after each --config,
// --config-file and --config-toml it keeps at that level or a shallower one. Where the role wrote
// no --config, configEnd is end, and this one goes in front of jjDryRunFlags: measured with jj
// 0.38.0, 0.45.1-sami and 0.46.0, in a repository with ui.quiet = true, a dry run with it there
// and one with it after jjDryRunFlags print the same and exit the same.
// The one case it cannot win is a kept --config-file or --config-toml of the role's at a deeper
// level than its --config that sets ui.quiet = true: the step then cannot read the dry run, prints
// its one line, and the push runs as written.
var jjDryRunQuiet = []string{"--config", "ui.quiet=false"}

// jjStep is the step that runs before a push: it reads what the push sends from its dry run, and
// sets the author of each of those commits whose first Legion-Role trailer names a role of the
// table, where it is not that role's identity, by one jj metaedit --update-author for each role,
// where its rules let it (setAuthors, errJJAsWritten).
// Each revision argument reaches each pass of the dry run and the real push in the words
// revisionGiven gives it, which name the commits it named before the step, as the step left them:
// once a metaedit of the step has run, every argument is held to them, whether or not it moved the
// argument's own commits.
// Each of its jj commands runs next with the caller's repository and configuration options
// (jjPush.options), and each of its git reads (gitMoved) runs the first git on PATH with -C and
// the workspace's root; they run one after another, each as a child in this process's group. Each
// jj command but one runs with --ignore-working-copy, so it takes no snapshot; the one is the
// snapshot the push would take itself (snapshot), which the step takes as rewrite says;
// snapshotted says how the real push runs after it.
type jjStep struct {
	push   jjPush
	next   string   // the jj each command of the step runs, and the push after them
	env    []string // the environment of each, the loop guard's variable set
	table  map[legionclaim.Role]runtime.GitIdentity
	stderr io.Writer
	// jjRun is what the current run of the step's reads and rewrites keeps (once), which rewrite
	// forgets after the step's snapshot.
	jjRun
	// snapshotted is whether the step has taken its one snapshot of the working copy (snapshot),
	// as rewrite says. It outlasts the runs after it. After that snapshot, the real push runs with
	// --ignore-working-copy (runJJ), on every path, a step that changes nothing more
	// (errJJAsWritten) among them, and takes no snapshot of its own, so the step's snapshot is the
	// push's one snapshot: @ resolves against it, a write another process makes after it stays in
	// the working copy for the next jj command, and a warning a snapshot prints is printed once.
	// The role's own --ignore-working-copy keeps the step from taking it (jjPush.noSnapshot), so
	// the real push never gets that flag twice, which jj 0.38 refuses.
	snapshotted bool
}

// jjRun is what one run of the step's reads and rewrites (once) keeps: every field such a run
// writes. rewrite forgets it all after the step's snapshot by one assignment, so that nothing the
// run before that snapshot read reaches the run after it.
type jjRun struct {
	// revisions are what the step keeps of each revision argument, by its place in push.revisions
	// (jjArgument).
	revisions []jjArgument
	// edited is whether a metaedit of the step has run, the fill of a commit with no author
	// included: from then on every revision argument is held to its set (revisionGiven).
	edited bool
	// followed is each commit the step follows through its rewrites (advance).
	followed []*jjFollowed
	// rewrites are the commits a metaedit of the step rewrote or found rewritten already, in
	// order.
	rewrites []jjRewrite
}

// jjFollowed is a commit the step follows through each of its metaedits: origin, the commit id
// the step read, and commit, the commit that stands for it now, of change; hidden is whether that
// commit is hidden now. arg is whether it is a revision argument's (readRevisions), working
// whether the commit it stood for after the step's read, the one the step read or the visible one
// readRevisions read a hidden one as, was the working-copy commit, and moved whether a metaedit of
// the step moved it to another commit, the one that replaced it (advance) or the one a rewrite
// before the step made of it already (existing), apart from readRevisions' reading of a hidden
// commit as its change's one visible commit.
type jjFollowed struct {
	origin, commit, change      string
	hidden, arg, working, moved bool
}

// jjRewrite is a commit a metaedit of the step rewrote under role's identity, or found rewritten
// so already (existing).
type jjRewrite struct {
	role   legionclaim.Role
	commit *jjFollowed
}

// jjStepFailure is a command of the step that failed: legion jj ends with code, after output,
// what the command printed on standard error, and runs no push. Four go on past one:
// readRevisions, revisionSet and revisionGiven, past the failure of their reads of a revision
// argument (readRevision, ids), and metaedit, past jj's refusal to make a commit that exists
// already (jjCommitExists) where that commit stands for one of its commits (existing). Where it
// stands for none, or the read of it is itself such a failure (jj refuses it, or it cannot start),
// metaedit answers that refusal; where the step cannot parse what jj printed for that read (read),
// metaedit answers that error, which runJJ prints as `legion jj: <error>`, ending with code 1.
type jjStepFailure struct {
	code   int
	output string
}

func (f jjStepFailure) Error() string { return fmt.Sprintf("a command of the step exited %d", f.code) }

// errJJAsWritten is a step that has printed its one line of what it could not do and changes
// nothing more: read what the push sends (unreadable), or set authors where the push's command
// would then pass jjArgMax bytes (checkArgMax). The push then runs as the role wrote it, but
// where a metaedit of the step ran: each revision argument is then held to the commits it named
// before the step, each one that metaedit moved as the commit that replaced it (jjStep.given).
// Where the step took its snapshot before it gave up, that push runs as jjStep.snapshotted says
// too.
var errJJAsWritten = errors.New("the step changes nothing more; the push runs as written")

// errJJSnapshot is a run of the step (once) that would change a commit or a word of the push while
// the step's snapshot is due (snapshotDue). Its causes are:
//
//   - a metaedit, which answers errJJSnapshot before it runs any command (metaedit): the fill of a
//     commit with no author that the dry run refuses (dryRunFilled), or the rewrite of an author
//     where the push's command would still be under jjArgMax bytes (setAuthors, which asks
//     checkArgMax first);
//   - once every read of the run is made and it has nothing else to change, a hidden commit that a
//     revision argument names and that the step reads as its change's one visible commit, which
//     the argument's words then name in its place (readRevisions, standsIn).
//
// A run that answers errJJSnapshot ends there, before any metaedit and any line of the step's,
// and rewrite takes the snapshot and runs again.
var errJJSnapshot = errors.New("the step has a commit or a word to change and takes the push's snapshot first")

// jjConfigurationRead is the read of the configuration that the pane's legion jj makes before the
// step's first command (jjStep.readConfiguration), after the push's options: `jj config list`
// with a template that prints nothing, which loads the configuration of the repository the push
// loads, the repository's own included, and reads no commit.
var jjConfigurationRead = []string{"--ignore-working-copy", "--color=never", "config", "list", "-T", `""`}

// readConfiguration is the step's first command under the tmux form, where runJJ asks for it:
// next with the caller's options (jjPush.options), then jjConfigurationRead, in env. jj prints a
// warning such as `Your repo appears to have been copied` (lib/src/secure_config.rs) for the first
// command that loads the configuration of a repository in that state, and for none after it. The
// step's reads and its dry run print nothing of their standard error (jjStep.jj, dryRunFilled),
// so the first of them would take such a warning from the role; this read takes it, and prints
// jjOnceOnly of its standard error (passOn), as the snapshot does, and as the pod's wrapper's read
// of git.executable-path does in a pod. It takes no snapshot and adds no operation. A read that
// cannot start or exits non-zero is a jjStepFailure, which runJJ prints whole, and the push does
// not run: jj alone fails on the same configuration.
func (s *jjStep) readConfiguration() error {
	return s.passOn(slices.Concat(s.push.options, jjConfigurationRead))
}

// passOn runs one of the two commands of the step whose standard error it prints,
// readConfiguration and snapshot: next with args, in env (output), and prints jjOnceOnly of what
// it printed on standard error. A command that cannot start or exits non-zero is output's
// jjStepFailure, of which passOn prints nothing: runJJ prints it whole.
func (s *jjStep) passOn(args []string) error {
	_, stderr, err := s.output(s.env, args)
	if err == nil {
		fmt.Fprint(s.stderr, jjOnceOnly(stderr))
	}
	return err
}

// rewrite runs the step's reads and rewrites (once) one time or two, and takes the step's one
// snapshot of the working copy, the one the push would take itself (snapshot), where the push takes
// a snapshot of its own (jjPush.noSnapshot): before the step's first read where git has moved HEAD
// or a branch since jj last imported them, and otherwise before the step changes a commit or a word
// of the push. Where the step's snapshot is due (snapshotDue), rewrite first asks whether git has
// moved them (gitMoved), with reads that import nothing. Where git has, the step takes the snapshot
// before its first read, which imports git's HEAD and refs in jj's own order and keeps the working
// copy current, so that the run reads what git did, and that run is the one run: it cannot answer
// errJJSnapshot, as the snapshot is taken. Otherwise the first run reads with
// --ignore-working-copy, as every command of the step but its snapshot does, and so reads each
// commit as it is before any snapshot. Where it finds nothing to change, or where it gives up
// before it changes anything (errJJAsWritten), the step takes no snapshot and adds no operation,
// and the real push takes its snapshot as jj alone does. Where it finds a commit or a word to
// change (errJJSnapshot), the step takes the snapshot, forgets all that the first run read (jjRun),
// and runs once more, with no other snapshot. The real push then runs as snapshotted says, so each
// commit id that the run after the snapshot reads, @'s and those of @'s descendants included, names
// the commit the push sees. Before a push with the role's own --ignore-working-copy, which takes no
// snapshot (jjPush.noSnapshot), every read of the step sees what the push will see: the step
// compares nothing and takes no snapshot, and the first run is the one run. An error is a
// jjStepFailure, a step that changes nothing more (errJJAsWritten), or output of jj's it cannot
// parse.
func (s *jjStep) rewrite() error {
	if s.snapshotDue() {
		moved, err := s.gitMoved()
		if err == nil && moved {
			err = s.snapshot()
		}
		if err != nil {
			return err
		}
	}
	err := s.once()
	if !errors.Is(err, errJJSnapshot) {
		return err
	}
	if err := s.snapshot(); err != nil {
		return err
	}
	s.jjRun = jjRun{}
	return s.once()
}

// once runs the step's reads and rewrites one time (rewrite): the read of each revision argument
// (readRevisions), the dry run and the loop over a commit with no author (dryRunFilled), and the
// rewrite of every other author (setAuthors). It ends with errJJSnapshot where errJJSnapshot says;
// its own last check is that list's word in place of a hidden commit (standsIn). One that ends in
// errJJAsWritten, past a dry run it cannot read or checkArgMax, gives each argument as the role
// wrote it but where a metaedit ran (given), and so takes no snapshot that the step has not taken.
func (s *jjStep) once() error {
	if err := s.readRevisions(); err != nil {
		return err
	}
	heads, err := s.dryRunFilled()
	if err != nil {
		return err
	}
	if err := s.setAuthors(heads); err != nil {
		return err
	}
	if s.snapshotDue() && s.standsIn() {
		return errJJSnapshot
	}
	return nil
}

// snapshot takes the one snapshot of the working copy that the push would take itself: one jj
// command, next with the caller's options (jjPush.options), --quiet where the push has the role's
// own (jjPush.quiet), and no --ignore-working-copy, in env, a read of the root commit's id, which
// needs no working-copy commit, before which jj 0.38 and 0.45 alike snapshot the working copy (`jj
// util snapshot` exists on 0.45 alone). It prints jjOnceOnly of what that command prints on
// standard error (passOn), as the push's own snapshot would print it: jj's warning of a file it
// refuses to snapshot, its line of the descendants of @ it rebased, which --quiet silences, its
// lines of an import of what git changed, or a warning jj prints once that no command before it
// took. A read before the step takes a one-time warning such as `Your repo appears to have been
// copied` first: the pod's wrapper's in a pod, and under tmux the read of the aliases
// (readJJAliases) or readConfiguration. A snapshot that finds nothing new adds no operation. Once
// it has run, the real push runs as snapshotted says.
// A command that cannot start or exits non-zero is a jjStepFailure, with what it printed on
// standard error (output), which runJJ prints whole: the push would fail at the same snapshot.
func (s *jjStep) snapshot() error {
	flags := []string{"--color=never"}
	if s.push.quiet {
		flags = append(flags, "--quiet")
	}
	if err := s.passOn(slices.Concat(s.push.options, flags, []string{"log", "--no-graph", "-r", "root()", "-T", "commit_id"})); err != nil {
		return err
	}
	s.snapshotted = true
	return nil
}

// snapshotDue is whether the step's snapshot (snapshot) is still due: it has not taken it, and the
// push takes a snapshot of its own (jjPush.noSnapshot). What the step does while it is due,
// rewrite says.
func (s *jjStep) snapshotDue() bool {
	return !s.snapshotted && !s.push.noSnapshot
}

// dryRunFilled is what the push sends, read from its dry run, each pass of which has each revision
// argument in the words the step gives it then (given): the full ids of the commits it adds or
// moves a ref to (parseJJDryRun, fullHeads). A dry run that jj refuses because a commit it sends
// has no author (jjNoAuthorCommit) gets that commit filled, by the metaedit of the role its
// trailer names, and runs again. jj prints each commit id short, as its first 12 hex digits, and
// the step never hands jj such an id to resolve: jj would resolve it as a prefix over every commit
// of the repository, hidden ones and other issues' included, and fail where a second commit starts
// with it. Each pass reads instead, with jjCommitTemplate, the commits the step can fill
// (readFillable), and takes the refused commit for the one commit of that read whose id starts
// with the refusal's. A refused commit that the step can fill, one the push sends that is mutable
// and that no remote holds, is in that read, so a single match is that commit. One it cannot fill,
// immutable or held by another remote, is not, and a commit of the read is taken for it only where
// the two ids share their first 12 digits. The loop has a bound it can prove: a dry run that names
// a commit an earlier pass already named or made, or a pass beyond the number of commits the first
// pass read, ends it as a dry run the step cannot read. So does a refusal whose id starts the id
// of no commit of the read, or of more than one, a commit whose trailer names no role of the
// table, and any other failure of the dry run. The metaedit of a fill answers errJJSnapshot while
// the step's snapshot is due (metaedit), which ends the loop.
func (s *jjStep) dryRunFilled() ([]string, error) {
	var seen []string
	bound, passes := -1, 0
	for {
		given := s.given(false)
		// The dry run of the push with each revision argument as given gives it to this pass
		// (dryRun).
		_, stderr, code, err := s.command(s.env, s.dryRun(given))
		if err != nil {
			return nil, err
		}
		if code == 0 {
			heads, err := parseJJDryRun(stderr)
			if err != nil {
				return nil, s.unreadable(err.Error())
			}
			return s.fullHeads(given, heads)
		}
		id, refusal, ok := jjNoAuthorCommit(stderr)
		if !ok {
			return nil, s.unreadable(jjErrorLine(stderr, "the dry run", code))
		}
		fillable, err := s.readFillable(given)
		if err != nil {
			return nil, err
		}
		if bound < 0 {
			bound = len(fillable)
		}
		matches := jjStartingWith(fillable, func(c jjCommitRead) string { return c.commit }, id)
		if len(matches) != 1 || passes >= bound {
			return nil, s.unreadable(refusal)
		}
		c := matches[0]
		if slices.Contains(seen, c.commit) {
			return nil, s.unreadable(refusal)
		}
		if _, ok := s.table[c.role]; !ok {
			return nil, s.unreadable(refusal)
		}
		passes++
		seen = append(seen, c.commit)
		f := s.follow(c, false)
		if err := s.metaedit(c.role, []*jjFollowed{f}); err != nil {
			return nil, err
		}
		if f.commit != f.origin {
			seen = append(seen, f.commit)
		}
	}
}

// dryRun is the words of a pass of the dry run, with each revision argument as given gives it to
// that pass: the push's words without the ones that stay on the real push alone (jjStep.words),
// with jjDryRunQuiet in front of the word at configEnd, and jjDryRunFlags in front of the first
// `--`, or at the end where there is none (jjPush.end). So the dry run reads the configuration the
// push reads, the role's --config in front of `git push` included, and the words from a `--` on
// stay after every option, as the push has them.
func (s *jjStep) dryRun(given []jjGiven) []string {
	p := &s.push
	words := s.appendWords(make([]string, 0, len(p.args)+len(jjDryRunFlags)+len(jjDryRunQuiet)), given, true, 0, p.configEnd)
	words = s.appendWords(append(words, jjDryRunQuiet...), given, true, p.configEnd, p.end)
	words = append(words, jjDryRunFlags...)
	return s.appendWords(words, given, true, p.end, len(p.args))
}

// readFillable reads, with jjCommitTemplate, the commits the step can fill:
// `mutable() ~ ::remote_bookmarks()`, and, where the push has revision arguments, every mutable
// ancestor of the commits they name as given, a pass of its dry run, gives them (revisionTerms),
// each of those commits among them, that no remote bookmark holds, hidden ones included: jj's
// all() holds a hidden commit that a revset names, and its ancestors, and a commit the push sends
// is visible or such an ancestor. The revision arguments go to as many reads as keep each revset
// under jjWordBound bytes (jjWords, readEach).
func (s *jjStep) readFillable(given []jjGiven) ([]jjCommitRead, error) {
	terms := s.revisionTerms(given)
	if len(terms) == 0 {
		return s.read("mutable() ~ ::remote_bookmarks()")
	}
	return s.readEach(jjWords("(mutable() | (::(", terms, ") & mutable())) ~ ::remote_bookmarks()"))
}

// fullHeads is the full commit id of each of heads, the ids a dry run with each revision argument
// as given gives it prints short. It reads the ids of the commits of `bookmarks() | tags()` and of
// each revision argument as given gives it (revisionTerms), in as few commands as keep each revset
// under jjWordBound bytes (jjWords), each id once (eachID): every ref a push adds or moves goes to
// a local bookmark's or a tag's target or to a commit a revision argument names, so the commit a
// head stands for is in that read, and is the one commit of it whose id starts with the head. A
// head that starts the id of no commit of the read, or of more than one, makes the dry run one the
// step cannot read.
func (s *jjStep) fullHeads(given []jjGiven, heads []string) ([]string, error) {
	if len(heads) == 0 {
		return nil, nil
	}
	ids, err := s.eachID(jjWords("", append([]string{"bookmarks()", "tags()"}, s.revisionTerms(given)...), ""))
	if err != nil {
		return nil, err
	}
	full := make([]string, len(heads))
	for i, head := range heads {
		matches := jjStartingWith(ids, func(id string) string { return id }, head)
		if len(matches) != 1 {
			return nil, s.unreadable(fmt.Sprintf("the dry run's commit id %s starts the id of %d commits that a bookmark, a tag or a revision of the push names, not of one", head, len(matches)))
		}
		full[i] = matches[0]
	}
	return full, nil
}

// jjStartingWith is each of items whose commit id, as id reads it, starts with short.
func jjStartingWith[T any](items []T, id func(T) string, short string) []T {
	var matches []T
	for _, item := range items {
		if strings.HasPrefix(id(item), short) {
			matches = append(matches, item)
		}
	}
	return matches
}

// setAuthors rewrites the author of each commit the push sends: the set
// `(::(<each of heads>) ~ ::remote_bookmarks()) & mutable()`, heads being the full ids of the dry
// run's new commits (fullHeads), every commit of which whose role is in the table and whose
// author's email is not that role's. It decides before its first metaedit, never after, whether
// the push's command would still be under jjArgMax bytes once each revision argument has the
// words the step may give it after that rewrite (checkArgMax); where it would not, it rewrites
// nothing. It runs one metaedit for each role with such commits, in claim.Roles' order, each in as
// many parts as keep each command under jjArgMax bytes (metaedit), and reads the set again before
// each metaedit after its first, from what the dry run's new commits stand for after the one
// before. Nothing to rewrite runs no metaedit, and reads no set of a revision argument. A set with
// a commit to rewrite gets checkArgMax's decision before its first metaedit can answer
// errJJSnapshot (metaedit): where the push's command would pass the limit, the step changes
// nothing and takes no snapshot that it has not taken (errJJAsWritten); else, where the step's
// snapshot is due, the run after that snapshot decides again.
func (s *jjStep) setAuthors(heads []string) error {
	if len(heads) == 0 {
		return nil
	}
	set, err := s.readSet(heads)
	if err != nil {
		return err
	}
	// A new commit outside the set is immutable or held by a remote, and so is every commit under
	// it: no metaedit of the step reaches it, and the set is read again without it.
	var tips []*jjFollowed
	for _, head := range heads {
		if i := slices.IndexFunc(set, func(c jjSetCommit) bool { return c.commit.commit == head }); i >= 0 {
			tips = append(tips, set[i].commit)
		}
	}
	roles := map[legionclaim.Role]bool{}
	for _, c := range set {
		if s.misauthored(c) {
			roles[c.role] = true
		}
	}
	if len(roles) == 0 {
		return nil
	}
	if err := s.checkArgMax(); err != nil {
		return err
	}
	first := true
	for _, role := range legionclaim.Roles {
		if !roles[role] {
			continue
		}
		if !first {
			var now []string
			for _, tip := range tips {
				now = append(now, tip.commit)
			}
			if set, err = s.readSet(now); err != nil {
				return err
			}
		}
		var commits []*jjFollowed
		for _, c := range set {
			if c.role == role && s.misauthored(c) {
				commits = append(commits, c.commit)
			}
		}
		if len(commits) == 0 {
			continue
		}
		if err := s.metaedit(role, commits); err != nil {
			return err
		}
		first = false
	}
	return nil
}

// jjSetCommit is one commit of the set the push sends, as the step follows it.
type jjSetCommit struct {
	commit *jjFollowed
	email  string
	role   legionclaim.Role
}

// misauthored is whether c's trailer names a role of the table whose email is not c's author's.
func (s *jjStep) misauthored(c jjSetCommit) bool {
	identity, ok := s.table[c.role]
	return ok && c.email != identity.Email
}

// readSet reads the set of commits the push sends whose new commits are heads, full commit ids,
// each of which commit_id() names exactly, in as few reads as keep each revset under jjWordBound
// bytes (jjWords, readEach), each commit followed (follow).
func (s *jjStep) readSet(heads []string) ([]jjSetCommit, error) {
	commits, err := s.readEach(jjWords("(::(", jjCommitIDs(heads), ") ~ ::remote_bookmarks()) & mutable()"))
	if err != nil {
		return nil, err
	}
	set := make([]jjSetCommit, len(commits))
	for i, c := range commits {
		set[i] = jjSetCommit{commit: s.follow(c, false), email: c.email, role: c.role}
	}
	return set, nil
}

// metaedit runs `jj metaedit --update-author` over commits under role's identity: the caller's
// options first, then that identity as --config user.name and user.email, each a TOML string
// (workspace.TOMLString), in front of `metaedit`, at the level of the caller's, where jj keeps both
// and the identity's, written later, wins over a --config of the push's, which outranks JJ_USER and
// JJ_EMAIL, set to the same. jj keeps a --config only from the deepest level of the command line
// that holds one (readJJPush): measured with jj 0.45.1-sami and 0.38.0, `jj --config
// 'revset-aliases.sendme="@-"' --ignore-working-copy metaedit --update-author -r sendme --config
// 'user.name="n"'` fails with `Revision sendme doesn't exist`, and with that last --config in
// front of `metaedit` it runs. While the step's snapshot is due (snapshotDue) it runs no
// command and answers errJJSnapshot, so that no metaedit of the step runs before that snapshot,
// whichever caller asks for it. It reads the set of each revision argument first, before any
// rewrite (revisionSets), and marks the step edited once its command has run. It runs in
// parts, in order, each over as many of commits as keep its command, arguments and environment,
// under jjArgMax bytes (jjSize), and at least one: one metaedit of some 35,000 commits would pass
// that limit. After each part it follows every followed commit through it (advance), so that the
// next part names each of its commits by the commit that stands for it then, and records each
// commit of the part as one role rewrote. A commit's timestamps are whole seconds, so the commit
// a part would make of one of its commits can exist already: an earlier rewrite of that commit in
// the same second made it, a refused push's step among them, and a `jj undo` or a `jj op restore`
// may have hidden it since. jj then refuses the whole part and changes nothing
// (jjExistingCommit). Where that commit can stand for one of commits (existing), the step records
// that one's rewrite, and the metaedit runs again over the rest of commits, if any. Each pass, a
// part or such a refusal, leaves one commit fewer at least, so the passes end. Where it can stand
// for none of them, metaedit answers jj's refusal as it is, as it answers any other failure: a
// jjStepFailure, after which runJJ prints jj's error and exits with jj's status, and no push runs.
func (s *jjStep) metaedit(role legionclaim.Role, commits []*jjFollowed) error {
	if s.snapshotDue() {
		return errJJSnapshot
	}
	s.revisionSets()
	identity := s.table[role]
	env := withVariable(withVariable(slices.Clone(s.env), "JJ_USER", identity.Name), "JJ_EMAIL", identity.Email)
	config := []string{"--config", "user.name=" + workspace.TOMLString(identity.Name), "--config", "user.email=" + workspace.TOMLString(identity.Email)}
	// fixed is what each part's command counts against jjArgMax but its -r words: next, the words
	// jj puts in front of args, args' own, and the environment.
	fixed := jjSize(s.next, "--ignore-working-copy", "metaedit", "--update-author") + jjSize(s.push.options...) + jjSize(config...) + jjSize(env...)
	for len(commits) > 0 {
		in := map[string]bool{}
		var hidden []string
		args := append(slices.Clone(config), "metaedit", "--update-author")
		n, size := 0, fixed
		for _, c := range commits {
			if size += jjSize("-r", c.commit); n > 0 && size >= jjArgMax {
				break
			}
			n++
			in[c.commit] = true
			args = append(args, "-r", c.commit)
			if c.hidden && !slices.Contains(hidden, c.change) {
				hidden = append(hidden, c.change)
			}
		}
		// A hidden commit's change can already have a visible commit that replaced it, which
		// names it in jj evolog as the commit the metaedit makes will: the one visible before is
		// not this step's.
		var before map[string][]jjCommitRead
		if len(hidden) > 0 {
			var err error
			if before, err = s.visible(hidden); err != nil {
				return err
			}
		}
		_, err := s.jj(env, args...)
		made, exists := jjExistingCommit(err)
		if !exists {
			if err != nil {
				return err
			}
			s.edited = true
			for _, c := range commits[:n] {
				s.rewrites = append(s.rewrites, jjRewrite{role: role, commit: c})
			}
			if err := s.advance(in, before); err != nil {
				return err
			}
			commits = commits[n:]
			continue
		}
		taken, readErr := s.existing(made, commits)
		if readErr != nil {
			return readErr
		}
		if taken == nil {
			return err
		}
		s.rewrites = append(s.rewrites, jjRewrite{role: role, commit: taken})
		commits = slices.DeleteFunc(commits, func(c *jjFollowed) bool { return c == taken })
	}
	return nil
}

// jjCommitExists is the line of jj's refusal to make a commit that exists already that names
// the commit, `Newly-created commit <id> already exists`, which jj 0.38 and 0.45 alike print as
// the cause of `Internal error: Unexpected error from backend`. Its group is the commit id.
var jjCommitExists = regexp.MustCompile(`Newly-created commit ([0-9a-f]+) already exists`)

// jjExistingCommit reads err, the failure of a command of the step (jjStepFailure), as jj's
// refusal to make a commit that exists already (jjCommitExists): ok is whether it is one, and id
// the commit id it names.
func jjExistingCommit(err error) (id string, ok bool) {
	var failed jjStepFailure
	if !errors.As(err, &failed) {
		return "", false
	}
	match := jjCommitExists.FindStringSubmatch(failed.output)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// existing is the one of commits that made stands for, made being a commit id that jj refused
// to make again because a commit has it already: the one commit of commits that is of made's
// change, where that one is hidden and no other of commits is. made is then the commit the
// metaedit would have made of it, and a hidden commit has no visible descendant for the refused
// metaedit to rebase; only another hidden commit of commits could descend from it, and the next
// pass would rewrite that one onto the old commit, not onto made. Every followed commit that
// stands for that one stands for made now, and has moved, hidden as made is. It is nil where made
// is of no change of commits, of the change of more than one, or of a visible one's, whose
// bookmarks and descendants only the refused metaedit would have moved onto made, where another
// of commits is hidden, and where the read of made is a command of the step that failed
// (jjStepFailure). Where the step cannot parse what jj printed for that read (read), it answers
// that error, and no commit.
func (s *jjStep) existing(made string, commits []*jjFollowed) (*jjFollowed, error) {
	read, err := s.read("commit_id(" + made + ")")
	var failed jjStepFailure
	if errors.As(err, &failed) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(read) != 1 || read[0].commit != made {
		return nil, nil
	}
	var taken *jjFollowed
	for _, c := range commits {
		if c.change != read[0].change {
			continue
		}
		if taken != nil {
			return nil, nil
		}
		taken = c
	}
	if taken == nil || !taken.hidden || slices.ContainsFunc(commits, func(c *jjFollowed) bool { return c != taken && c.hidden }) {
		return nil, nil
	}
	old := taken.commit
	for _, f := range s.followed {
		if f.commit == old {
			f.commit, f.hidden, f.moved = made, read[0].hidden, true
		}
	}
	return taken, nil
}

// advance follows each followed commit through the metaedit that just ran over in: a
// commit that metaedit hid, itself or by rebasing it, stands now for the visible commit that
// replaced it (successor), and has moved. A commit that was hidden already and that the metaedit
// did not name stays as it is. before is what s.visible read of the changes of in's hidden
// commits ahead of the metaedit.
func (s *jjStep) advance(in map[string]bool, before map[string][]jjCommitRead) error {
	var moving []*jjFollowed
	var changes []string
	for _, f := range s.followed {
		if f.hidden && !in[f.commit] {
			continue
		}
		moving = append(moving, f)
		if !slices.Contains(changes, f.change) {
			changes = append(changes, f.change)
		}
	}
	if len(moving) == 0 {
		return nil
	}
	visible, err := s.visible(changes)
	if err != nil {
		return err
	}
	successors := map[string]string{}
	for _, f := range moving {
		now := visible[f.change]
		if slices.ContainsFunc(now, func(c jjCommitRead) bool { return c.commit == f.commit }) {
			f.hidden = false
			continue
		}
		next, known := successors[f.commit]
		if !known {
			if next, err = s.successor(f.commit, now, before[f.change]); err != nil {
				return err
			}
			successors[f.commit] = next
		}
		if next == "" {
			f.hidden = true
			continue
		}
		f.commit, f.hidden, f.moved = next, false, true
	}
	return nil
}

// successor is the commit that replaced old, which is hidden now, among visible, the visible
// commits of old's change: the one there is, or, where the change is divergent, the one whose
// `jj evolog` names old second. Where several do, it is the one the step's own metaedit made, the
// one not among before. It is "" when no single commit replaced old.
func (s *jjStep) successor(old string, visible, before []jjCommitRead) (string, error) {
	switch len(visible) {
	case 0:
		return "", nil
	case 1:
		return visible[0].commit, nil
	}
	var named []string
	for _, c := range visible {
		out, err := s.jj(s.env, "--color=never", "evolog", "-r", c.commit, "--no-graph", "-n", "2", "-T", `commit.commit_id() ++ "\0"`)
		if err != nil {
			return "", err
		}
		if entries := strings.Split(out, "\x00"); len(entries) > 1 && entries[1] == old {
			named = append(named, c.commit)
		}
	}
	if len(named) > 1 {
		named = slices.DeleteFunc(named, func(commit string) bool {
			return slices.ContainsFunc(before, func(c jjCommitRead) bool { return c.commit == commit })
		})
	}
	if len(named) == 1 {
		return named[0], nil
	}
	return "", nil
}

// follow is the followed commit that c is now, or a new one that starts at c. A revision
// argument's (arg) is always new: it stands for what the role named, not a commit the step read to
// rewrite, and readRevisions may read a hidden one as another commit.
func (s *jjStep) follow(c jjCommitRead, arg bool) *jjFollowed {
	if !arg {
		if i := slices.IndexFunc(s.followed, func(f *jjFollowed) bool { return !f.arg && f.commit == c.commit }); i >= 0 {
			return s.followed[i]
		}
	}
	f := &jjFollowed{origin: c.commit, commit: c.commit, change: c.change, hidden: c.hidden, arg: arg, working: c.working}
	s.followed = append(s.followed, f)
	return f
}

// visible is the visible commits of each of changes, by change id, each read with
// jjCommitTemplate (readEach of jjChanges).
func (s *jjStep) visible(changes []string) (map[string][]jjCommitRead, error) {
	commits, err := s.readEach(jjChanges(changes))
	if err != nil {
		return nil, err
	}
	visible := map[string][]jjCommitRead{}
	for _, c := range commits {
		if !c.hidden {
			visible[c.change] = append(visible[c.change], c)
		}
	}
	return visible, nil
}

// report prints one line for each commit a metaedit of the step rewrote or found rewritten
// already (existing), with the id the step read and the id of the commit that stands for it after
// every metaedit of the step. A commit that the step changed not at all, or that stands for a
// hidden commit, prints nothing.
func (s *jjStep) report() {
	for _, r := range s.rewrites {
		if f := r.commit; !f.hidden && f.commit != f.origin {
			fmt.Fprintf(s.stderr, "legion jj: %s: %s -> %s\n", r.role, f.origin, f.commit)
		}
	}
}

// unreadable prints the one line of a dry run the step cannot read, naming line, what of it the
// step could not read: its first line that the step could not read or act on, jj's refusal of a
// commit with no author that the step cannot fill among them (dryRunFilled), or a commit id of a
// block that the step could not place among the commits it read (fullHeads), and answers
// errJJAsWritten (asWritten): the push then runs with the role's own words, but where a metaedit of
// the step ran, past which each revision argument is held to its set (jjStep.given), and as
// snapshotted says where the step took its snapshot.
func (s *jjStep) unreadable(line string) error {
	return s.asWritten("read what the push sends", line)
}

// asWritten prints the one line of a step that could not do what, naming line: what of the dry run
// it could not read (unreadable), or the revision argument with which the push's command would
// pass jjArgMax bytes (checkArgMax). It answers errJJAsWritten: the step changes nothing more, and
// the push runs as the role wrote it, but where a metaedit of the step ran, past which each
// revision argument is held to its set (jjStep.given). Where the step took its snapshot, that push
// runs as snapshotted says too, and the line ends `, with --ignore-working-copy`.
func (s *jjStep) asWritten(what, line string) error {
	ignoring := ""
	if s.snapshotted {
		ignoring = ", with --ignore-working-copy"
	}
	fmt.Fprintf(s.stderr, "legion jj: could not %s: %s; the push runs as written%s\n", what, line, ignoring)
	return errJJAsWritten
}
