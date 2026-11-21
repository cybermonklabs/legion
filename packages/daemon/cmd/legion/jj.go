package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// jjUsage is the help of `legion jj`, which reads its own arguments (passThrough), --help among
// them.
const jjUsage = `usage: legion jj --real <jj> -- <jj argument>…
       legion jj --after <directory> -- <jj argument>…

A jj wrapper runs this command, never a person. It runs a jj with the arguments after --.
Before a jj git push, it first sets the author of each commit the push sends from that
commit's first Legion-Role trailer: the identity LEGION_ROLE_IDENTITIES gives that role, by
one jj metaedit --update-author for each role. It prints one line for each commit it rewrote.

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

// wrappedJJStepped is the variable that marks a push whose step has run: a legion jj that ran the
// step before a push, or printed the line of a table it cannot use, sets it to the digest of the
// words it runs that push with (jjStepped), in the environment of that push alone. A legion jj
// whose push's own words have that digest runs no step: it is the same push, come back through a
// host's jj wrapper that runs jj by PATH, or through a second Legion bin directory on PATH, as a
// controller started from a pane has. Every other push runs the step, a push that a jj command
// starts among them. The step's own commands are no pushes the step runs before (jjPush.steps),
// so they need no mark. A host's jj wrapper that changes the words makes the step run once more,
// before the words that wrapper runs; that is harmless, since the step is idempotent: its second
// run finds nothing to change. Its name starts with neither LEGION_ nor JJ_, as wrappedJJAt's
// does.
const wrappedJJStepped = "WRAPPED_JJ_STEPPED"

// jjStepped is the value of wrappedJJStepped for a push of words: the hex of the SHA-256 of the
// words joined by NUL, which no word holds.
func jjStepped(words []string) string {
	sum := sha256.Sum256([]byte(strings.Join(words, "\x00")))
	return hex.EncodeToString(sum[:])
}

// runJJ is `legion jj`. Both of Legion's jj wrappers run it: the pod's /opt/legion/bin/jj
// (packages/daemon/docker/jj) hands it each `jj git push` with --real /usr/local/bin/jj after its
// two setup checks, and the jj first on a tmux pane's PATH (workerbin.Install's bin/jj) hands it
// every command with --after and its own directory. It reads its arguments and puts back the
// signals' actions as runGit does (startWrapper), chooses the jj to run (pickAfter for --after),
// runs the author step before a push (jjStep), and replaces itself with that jj (execWrapped), so
// a stop of the process group and its budgets reach jj as before. A command that is no push, a
// push the step leaves alone (readJJPush), a process with no table (runtime.RoleIdentitiesVariable
// unset, as a controller's is), and a push whose wrappedJJStepped is the digest of its own words
// (jjStepped), the same push come back after its step, run that jj at once, with nothing printed
// and no other process started. A table that is set but cannot be used
// (runtime.ReadRoleIdentities) prints one line, and then runs the same. The push that runs after
// the step gets each revision argument in the words the step gives it (jjStep.given), the role's
// own where they still name the commits jjStep.revisionGiven says it names; the line of an
// argument it gets as the ids of its commits comes after the step's lines of its rewrites
// (report). After a step that changes nothing more (errJJAsWritten), past a dry run it cannot
// read, a metaedit that jj refuses because the commit it would make exists already
// (jjStep.metaedit), or a push whose command would pass jjArgMax bytes (jjStep.checkArgMax), only
// a commit that a metaedit of the step moved counts as replaced, and every argument is held to
// the commits it named before the step only where a metaedit ran. The push that runs after the
// step, or after the line of a table it cannot use, gets wrappedJJStepped set to the digest of the
// words it runs with; the step's own commands do not.
//
// An interrupt or a terminate ends legion jj where it stands. The step's own commands are
// children in this process's group, so a signal sent to the group ends them too, and the push
// never starts.
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
	push := readJJPush(jjArgs)
	value, set := os.LookupEnv(runtime.RoleIdentitiesVariable)
	if !push.steps() || !set || os.Getenv(wrappedJJStepped) == jjStepped(jjArgs) {
		return execWrapped("legion jj", next, jjArgs, env, nil, stderr)
	}
	table, why := runtime.ReadRoleIdentities(value)
	if why != "" {
		fmt.Fprintf(stderr, "legion jj: %s is %s; the push runs with no author step\n", runtime.RoleIdentitiesVariable, why)
		return execWrapped("legion jj", next, jjArgs, withVariable(env, wrappedJJStepped, jjStepped(jjArgs)), nil, stderr)
	}
	step := &jjStep{push: push, next: next, env: env, table: table, stderr: stderr}
	err := step.rewrite()
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
	return execWrapped("legion jj", next, words, withVariable(env, wrappedJJStepped, jjStepped(words)), nil, stderr)
}

// jjDryRunFlags are the words after those of a dry run of the push (dryRunFilled): --dry-run,
// --ignore-working-copy, --color=never, and `--config ui.quiet=false` as the last configuration,
// so that neither a flag nor the repository's configuration silences it.
var jjDryRunFlags = []string{"--dry-run", "--ignore-working-copy", "--color=never", "--config", "ui.quiet=false"}

// jjStep is the step that runs before a push: it reads what the push sends from its dry run, and
// sets the author of each of those commits whose first Legion-Role trailer names a role of the
// table, where it is not that role's identity, by one jj metaedit --update-author for each role.
// Each revision argument reaches each pass of the dry run and the real push in the words
// revisionGiven gives it, which name the commits it named before the step, as the step left them:
// once a metaedit of the step has run, every argument is held to them, whether or not it moved the
// argument's own commits.
// Each of its jj commands runs next, one after another, as a child in this process's group, with
// the caller's repository and configuration options (jjPush.options) and --ignore-working-copy, so
// it takes no snapshot.
type jjStep struct {
	push   jjPush
	next   string   // the jj each command of the step runs, and the push after them
	env    []string // the environment of each, the loop guard's variable set
	table  map[legionclaim.Role]runtime.GitIdentity
	stderr io.Writer
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
// already (jjCommitExists) and the failure of its read of that commit.
type jjStepFailure struct {
	code   int
	output string
}

func (f jjStepFailure) Error() string { return fmt.Sprintf("a command of the step exited %d", f.code) }

// errJJAsWritten is a step that has printed its one line of what it could not do and changes
// nothing more: read what the push sends (unreadable), set an author where jj refuses to make a
// commit that exists already (metaedit), or set authors where the push's command would then pass
// jjArgMax bytes (checkArgMax). The push then runs as the role wrote it, but where a metaedit of the
// step ran: each revision argument is then held to the commits it named before the step, each one
// that metaedit moved as the commit that replaced it (jjStep.given).
var errJJAsWritten = errors.New("the step changes nothing more; the push runs as written")

// rewrite runs the step: the read of each revision argument (readRevisions), the dry run and the
// loop over a commit with no author (dryRunFilled), and the rewrite of every other author
// (setAuthors). An error is a jjStepFailure, a step that changes nothing more (errJJAsWritten), or
// output of jj's it cannot parse.
func (s *jjStep) rewrite() error {
	if err := s.readRevisions(); err != nil {
		return err
	}
	heads, err := s.dryRunFilled()
	if err != nil {
		return err
	}
	return s.setAuthors(heads)
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
// table, and any other failure of the dry run.
func (s *jjStep) dryRunFilled() ([]string, error) {
	var seen []string
	bound, passes := -1, 0
	for {
		given := s.given(false)
		// The dry run of the push with each revision argument as given gives it to this pass: the
		// push's words without the ones that stay on the real push alone (jjStep.words), then
		// jjDryRunFlags.
		_, stderr, code, err := s.command(s.env, append(s.words(given, true), jjDryRunFlags...))
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
			return nil, s.unreadable(jjErrorLine(stderr, code))
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

// jjEachOnce is each item of revsets, read with one command for each (read), and each item once
// by its commit id, as id reads it, in the order the reads print them.
func jjEachOnce[T any](revsets []string, read func(string) ([]T, error), id func(T) string) ([]T, error) {
	var items []T
	seen := map[string]bool{}
	for _, revset := range revsets {
		got, err := read(revset)
		if err != nil {
			return nil, err
		}
		for _, item := range got {
			if key := id(item); !seen[key] {
				seen[key] = true
				items = append(items, item)
			}
		}
	}
	return items, nil
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
// before. Nothing to rewrite runs no metaedit, and reads no set of a revision argument.
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
// options first, then that identity as the command's last --config user.name and user.email,
// each a TOML string (workspace.TOMLString), since a --config of the push's outranks JJ_USER and
// JJ_EMAIL, which are set to the same. It reads the set of each revision argument first, before
// any rewrite (revisionSets), and marks the step edited once its command has run. It runs in
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
// part or such a refusal, leaves one commit fewer at least, so the passes end. Otherwise the step
// prints one line that names jj's error, and answers errJJAsWritten. Any other failure is
// answered as it is: a jjStepFailure runs no push.
func (s *jjStep) metaedit(role legionclaim.Role, commits []*jjFollowed) error {
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
		args := []string{"metaedit", "--update-author"}
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
		_, err := s.jj(env, append(args, config...)...)
		made, line, exists := jjExistingCommit(err)
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
		taken, err := s.existing(made, commits)
		if err != nil {
			return err
		}
		if taken == nil {
			return s.asWritten("set the author", line)
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
// refusal to make a commit that exists already (jjCommitExists): ok is whether it is one, id the
// commit id it names, and line the words of it that name that commit.
func jjExistingCommit(err error) (id, line string, ok bool) {
	var failed jjStepFailure
	if !errors.As(err, &failed) {
		return "", "", false
	}
	match := jjCommitExists.FindStringSubmatch(failed.output)
	if match == nil {
		return "", "", false
	}
	return match[1], match[0], true
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
// of commits is hidden, and where the step cannot read made.
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

// jjChanges is the revsets of the commits of each of changes, by change id: `change_id(<change>)`
// for each, joined by ` | ` into as few revsets as keep each under jjWordBound bytes (jjWords).
func jjChanges(changes []string) []string {
	named := make([]string, len(changes))
	for i, change := range changes {
		named[i] = "change_id(" + change + ")"
	}
	return jjWords("", named, "")
}

// jjCommitIDs is the term `commit_id(<id>)` of each of ids, full commit ids, each of which
// commit_id() names exactly, for a read of them in as few revsets as keep each under jjWordBound
// bytes (jjWords).
func jjCommitIDs(ids []string) []string {
	named := make([]string, len(ids))
	for i, id := range ids {
		named[i] = "commit_id(" + id + ")"
	}
	return named
}

// jjWordBound is the bound, in bytes, under which the step keeps each word it builds of a list of
// commit or change ids: the revset of one of its reads (jjWords), and a word of a revision argument
// that gives the push its commits as their ids or as its primary form (revisionGiven). Linux
// refuses one argv word over 131,072 bytes, and such a list grows with the commits it names, which
// a long history makes thousands. It is a variable so that this package's test binary, run as
// legion, can lower it (TestMain); nothing in the legion binary changes it.
var jjWordBound = 100_000

// jjArgMax is the bound, in bytes, under which the step keeps each command of its own that grows
// with the commits it names, the push's (checkArgMax) and each metaedit's (jjStep.metaedit): each
// string of its arguments and of its environment with the NUL that ends it, and the pointer to each
// (jjSize), which Linux refuses past 2,097,152 bytes, a quarter of the usual 8 MiB stack
// (execve(2)). It is a variable so that this package's test binary, run as legion, can lower it
// (TestMain); nothing in the legion binary changes it.
var jjArgMax = 2_097_152

// jjSize is the bytes that Linux counts against jjArgMax (execve(2)) for words, strings of a
// command's arguments or environment: each with the NUL that ends it, and the pointer to it.
func jjSize(words ...string) int {
	size := 0
	for _, word := range words {
		size += len(word) + 1 + int(unsafe.Sizeof(uintptr(0)))
	}
	return size
}

// jjWords is terms in as few revsets as keep each under jjWordBound bytes: each prefix, then terms
// joined by ` | `, then suffix, the terms in order. A term that no revset of its own keeps under
// the bound is a revset alone. A read of each, merged (readEach), is one read of them all where
// prefix and suffix take the union of the terms' commits to the union of what they take of each,
// as `::`, `&`, `~` and `|` do.
func jjWords(prefix string, terms []string, suffix string) []string {
	var words []string
	var word strings.Builder
	for _, term := range terms {
		if word.Len() > 0 && word.Len()+len(" | ")+len(term)+len(suffix) >= jjWordBound {
			words = append(words, word.String()+suffix)
			word.Reset()
		}
		if word.Len() == 0 {
			word.WriteString(prefix)
		} else {
			word.WriteString(" | ")
		}
		word.WriteString(term)
	}
	if word.Len() > 0 {
		words = append(words, word.String()+suffix)
	}
	return words
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
// the step ran, past which each revision argument is held to its set (jjStep.given).
func (s *jjStep) unreadable(line string) error {
	return s.asWritten("read what the push sends", line)
}

// asWritten prints the one line of a step that could not do what, naming line: what of the dry run
// it could not read (unreadable), jj's refusal to make a commit that exists already (metaedit), or
// the revision argument with which the push's command would pass jjArgMax bytes (checkArgMax). It
// answers errJJAsWritten: the step changes nothing more, and the push runs as the role wrote it,
// but where a metaedit of the step ran, past which each revision argument is held to its set
// (jjStep.given).
func (s *jjStep) asWritten(what, line string) error {
	fmt.Fprintf(s.stderr, "legion jj: could not %s: %s; the push runs as written\n", what, line)
	return errJJAsWritten
}

// jjCommitTemplate prints, for each commit, its change id, its commit id, whether it is hidden,
// whether it is the working-copy commit of the workspace the command runs in
// (current_working_copy, which jj reads from the operation, --ignore-working-copy or not), its
// author's email and the value of its first Legion-Role trailer, each ended by NUL, which none of
// them holds.
const jjCommitTemplate = `change_id ++ "\0" ++ commit_id ++ "\0" ++ hidden ++ "\0" ++ current_working_copy ++ "\0" ++ author.email() ++ "\0" ++ trailers.filter(|t| t.key() == "Legion-Role").map(|t| t.value()).join("\n") ++ "\0"`

// jjCommitRead is one commit as jjCommitTemplate prints it.
type jjCommitRead struct {
	change, commit  string
	hidden, working bool
	email           string
	role            legionclaim.Role
}

// readEach is each commit of the revsets of words, read with jjCommitTemplate (read), one command
// for each, and each commit once, in the order the reads print them (jjEachOnce).
func (s *jjStep) readEach(words []string) ([]jjCommitRead, error) {
	return jjEachOnce(words, s.read, func(c jjCommitRead) string { return c.commit })
}

// read is each commit of revset, read with jjCommitTemplate.
func (s *jjStep) read(revset string) ([]jjCommitRead, error) {
	out, err := s.jj(s.env, "--color=never", "log", "--no-graph", "-r", revset, "-T", jjCommitTemplate)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(out, "\x00")
	if len(fields)%6 != 1 || fields[len(fields)-1] != "" {
		return nil, fmt.Errorf("jj log -r %s printed %q, which is not one record of six fields for each commit", revset, out)
	}
	commits := make([]jjCommitRead, 0, len(fields)/6)
	for i := 0; i+6 < len(fields); i += 6 {
		role, _, _ := strings.Cut(fields[i+5], "\n")
		commits = append(commits, jjCommitRead{
			change: fields[i], commit: fields[i+1], hidden: fields[i+2] == "true", working: fields[i+3] == "true",
			email: fields[i+4], role: legionclaim.Role(role),
		})
	}
	return commits, nil
}

// ids reads the commits of revset in one command: each one's full id, once, in jj's order, and
// the id of the one that is the working-copy commit of the workspace the command runs in
// (current_working_copy, which jj reads from the operation, --ignore-working-copy or not), or ""
// where none is. A read jj refuses, or one that cannot start, is a jjStepFailure, its one error.
func (s *jjStep) ids(revset string) (ids []string, working string, err error) {
	out, err := s.jj(s.env, "--color=never", "log", "--no-graph", "-r", revset, "-T", `commit_id ++ if(current_working_copy, " @") ++ "\n"`)
	if err != nil {
		return nil, "", err
	}
	for line := range strings.Lines(out) {
		id, mark, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		if id == "" {
			continue
		}
		ids = append(ids, id)
		if mark == "@" {
			working = id
		}
	}
	return ids, working, nil
}

// eachID is the full id of each commit of revsets, read with one command for each (ids), each
// once, in the order the reads print them (jjEachOnce).
func (s *jjStep) eachID(revsets []string) ([]string, error) {
	return jjEachOnce(revsets, func(revset string) ([]string, error) {
		ids, _, err := s.ids(revset)
		return ids, err
	}, func(id string) string { return id })
}

// jj runs one jj command of the step and answers what it printed on standard output: next, with
// the caller's options (jjPush.options), then --ignore-working-copy, then args, in env. A command
// that cannot start or exits non-zero is a jjStepFailure, with what it printed on standard error.
func (s *jjStep) jj(env []string, args ...string) (string, error) {
	stdout, stderr, code, err := s.command(env, slices.Concat(s.push.options, []string{"--ignore-working-copy"}, args))
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", jjStepFailure{code: code, output: stderr}
	}
	return stdout, nil
}

// command runs next with args in env and waits for it, as a child in this process's group, with
// no standard input. It answers what the command printed and its status, a death by a signal
// being 128 and the signal's number, as a shell reads it. A file the kernel refuses to run runs
// with /bin/sh, as execWrapped runs it. A command that cannot start is a jjStepFailure whose
// status is execWrapped's for the same failure.
func (s *jjStep) command(env, args []string) (stdout, stderr string, code int, err error) {
	var out, errOut bytes.Buffer
	run := func(path string, argv []string) error {
		out.Reset()
		errOut.Reset()
		cmd := &exec.Cmd{Path: path, Args: argv, Env: env, Stdout: &out, Stderr: &errOut}
		return cmd.Run()
	}
	err = run(s.next, append([]string{s.next}, args...))
	if errors.Is(err, syscall.ENOEXEC) {
		err = run("/bin/sh", append([]string{"/bin/sh", s.next}, args...))
	}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		code = exit.ExitCode()
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = 128 + int(status.Signal())
		}
		return out.String(), errOut.String(), code, nil
	case err != nil:
		code = 126
		if errors.Is(err, syscall.ENOENT) {
			code = 127
		}
		return "", "", 0, jjStepFailure{code: code, output: fmt.Sprintf("legion jj: run %s: %v\n", s.next, err)}
	}
	return out.String(), errOut.String(), 0, nil
}

// jjErrorLine is the line the step names when it cannot read a dry run that exited code: the
// first line of output that starts with `Error: `, jj's error, which may follow a notice such as
// the `Creating bookmark push-<change> for revision <change>` of `jj git push -c`; else the first
// line that is not blank, or what stands for it when there is none.
func jjErrorLine(output string, code int) string {
	first := ""
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, "Error: ") {
			return strings.TrimSpace(line)
		}
		if first == "" {
			first = strings.TrimSpace(line)
		}
	}
	if first != "" {
		return first
	}
	return fmt.Sprintf("the dry run exited %d and printed nothing", code)
}
