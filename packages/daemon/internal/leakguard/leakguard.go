// Package leakguard is the one check a Legion write to a public GitHub repository passes before
// it writes. Two checks run, in this order, and either one refuses the write:
//
//   - The shared public leak check, a script this repository does not hold, found at each call
//     (Resolve) and run in the shape the write has: a push, a gh call, or a piece of text. The
//     deployment names it (Setting); under a deployment that names none, a write where no check
//     is named skips it rather than being refused for its absence (Guard.sharedCheck).
//   - The deployment's private-name rules, a gitleaks rule file the deployment names in
//     LEGION_PRIVATE_NAME_RULES_FILE (RulesFileVar), run in process over the records the write is
//     made of, and only when the destination repository is one GitHub reports public. A
//     deployment that names no rule file runs no private-name rule: the guard runs the shared
//     check alone, where one runs, reads no visibility, builds no record, and says nothing of it,
//     write after write. The daemon says it once, at boot, in its own log (internal/daemon's
//     privateNameRulesNone).
//
// A call site reaches the guard through one of three entry points — Push, GhCall and Text — and
// reads one of three answers from a refused write: a *Refusal, which is about the text the write
// carries; a *SetupFailure, which is the check missing, the check failing, the engine failing, a
// rule file that cannot be used, or a destination whose visibility cannot be read; and a
// *NotFound, which is a destination GitHub answers 404 for. All three refuse the write; they are
// three types so that a call site can say which one it met, the way the shared check itself keeps
// "LEAK CHECK: refusing" apart from "LEAK CHECK SETUP ERROR". A write with no repository behind
// it, the daemon's branch create, builds no guard: it runs the rule set the daemon compiled at
// boot through RuleSet.Check, which answers a *Refusal or a *SetupFailure.
//
// A rule-pass refusal shows what matched: each line of its report names the record, the class
// (the description of the deployment's rule), the text that matched and the line it stands on,
// quoted and cut to the bounds RuleSet.scan names. The agent that would make the write wrote that
// text, so there is nothing in it to hide from the agent the refusal is shown to; this
// repository's public CI never prints the guard's report. Nothing the detection engine itself
// printed is reported. A record about a file is named by its path, and a report or a setup failure
// prints a record's label, a path, a repository slug or an option on one line and bounded
// (shownText): as it was written, where that is printable text of at most 200 bytes.
//
// The rule pass of a push reads git's output as git writes it, the added paths and the diff, and
// scans each record as it arrives (pass), and a refusal shows at most maxLinesShown lines,
// counting the rest (shownReport). So the pass holds one line of git's output, the record in hand,
// the detector's cost of scanning it, and one commit's author, committer and message, whatever the
// number and size of the push's files; what grows with the push is the list of its commits, about
// one line of a commit's id each. One line is held whole, a few times over (the reader's buffer,
// the record's text, the detector's lower-cased copy), however long it is: a line is neither
// split, since a match could cross the split and pass unread, nor refused for its length, which
// would refuse pushes the rules pass. A push of a 73 MB line took 416 MiB; a hook the kernel
// stops for its memory refuses the push.
//
// The guard sets neither PUBLIC_LEAK_CHECK_REPO_NAMES nor PUBLIC_LEAK_CHECK_INTERNAL for the
// shared check. That is a decision of this issue's approved spec, not an omission: with those two
// switches on, the shared check refuses a dispatch:// link, and 786 tracked files of this
// repository hold one, so every write of this repository would be refused. A later reader who
// wants either switch is changing that decision, not filling a gap.
package leakguard

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// Visibility is what GitHub reports a repository as. The rule-file pass runs for a public
// destination alone, as the shared check's own content scan does: a private or internal
// destination is not somewhere a private name can leak to.
type Visibility string

const (
	Public   Visibility = "public"
	Private  Visibility = "private"
	Internal Visibility = "internal"
)

// Options are what a guard is built with, and a field left empty is the ordinary case, a role's:
// every call resolves the check for itself (Resolve) and reads the deployment's rule file
// (RulesFileVar) in this process's own environment, and makes the private copy of a check under a
// deployment's digest under this package's PrivateCopyBase or its fallback. A caller sets what it
// holds of its own: `legion gh` an observer of the visibility answer (OnVisibility), the daemon
// the environment it resolves the machine's check in (Env), and the answer loop's gh tool, on
// every call, the check its host answered, the environment the guard's children start from, which
// names the deployment's rule file where the deployment names one, and the directory of the
// private copy (Check, Env and PrivateCopyBase, internal/ghtool's Answer.leakGuard). Visibility,
// Clock and Curl are seams a test fills. The engine needs no seam: it is linked into this binary,
// and the rules it runs are the deployment's, so a test names a fixture rule file in Env as a
// deployment names its own.
type Options struct {
	// Check is the shared public leak check's path, which stands in front of every place Resolve
	// reads. Empty, every call resolves the check for itself (Resolve), as a role's `legion gh`
	// does. The answer loop's gh tool sets it on every call, to the check its host answered for
	// that call (internal/ghtool's Answer.leakGuard, Host.LeakCheck), so its guard resolves none
	// of its own; a test fills it with a stand-in.
	Check string
	// Visibility is the REST read of what GitHub reports a repository slug ("owner/name"). Nil is
	// the ordinary case, gh's REST read with headers (Guard.ghVisibility). It replaces that first
	// read alone: a read it refuses for a spent rate limit still goes on to the GraphQL read and
	// ls-remote, and one that failed on its connection (a *transportError) to curl and ls-remote
	// (Guard.Visibility), which a test stands gh, curl and git in for on Env's PATH.
	Visibility func(ctx context.Context, slug string) (Visibility, error)
	// Clock supplies the time and bounded wait of a rate-limited visibility read. Nil is wall time.
	Clock Clock
	// OnVisibility observes the source of a successful visibility answer. A caller uses it for
	// audit logging and leaves it nil when it has no audit record of its own.
	OnVisibility func(VisibilitySource)
	// Env is the environment the guard's children start from: the shared check, the visibility
	// reads, and the git commands a push's records are read with. Nil means this process's own
	// (os.Environ()). Every child keeps all of it but the read with no credential (Guard.lsRemote),
	// which keeps only the names lsRemoteInherits allows. The guard adds to it the names that make
	// a credential prompt fail closed (promptsNothing) and RunByRealGhVar blanked, and each child's
	// own names on top of those. It is also where the guard reads RulesFileVar (Guard.rules).
	Env []string
	// Curl is the curl the guard's reads through curl run (Guard.CurlGet). Empty is the ordinary
	// case, curlProgram: the system's own curl at a fixed path, never the first curl on the
	// caller's PATH, since the curl that runs is handed a token on its standard input. A test fills
	// it with a stand-in of its own: no shipped binary sets it, and cmd/legion's test binary sets
	// the value it passes from LEGION_CMD_TEST_CURL (its TestMain).
	Curl string
	// PrivateCopyBase is the directory a guard under a deployment's digest makes the copy of the
	// check it runs under (Guard.sharedCheck, privateCopy), in place of this package's
	// PrivateCopyBase and its fallback. Empty is the ordinary case, a role's: the role's own
	// private directory where it is there, else /tmp. A caller that is no role sets it to a
	// directory of its own that only its user reaches: the daemon's answer loop, whose container
	// may hold the worker image's root-owned, empty PrivateCopyBase and binds nothing writable
	// there (internal/ghtool's Answer.leakGuard). The copy is made in it and nowhere else.
	PrivateCopyBase string
}

// Guard runs the two checks. It holds no answer of an earlier call: every call site builds a
// guard of its own for the one write it is about to make (cmd/legion's gh and leak-check, and
// internal/ghtool's call of the answer loop), and each entry point reads a destination's
// visibility at most once, so there is nothing a shared answer would spare.
type Guard struct {
	opts Options
	// noAnswer is the askpass program the children are pointed at, resolved once from the base
	// environment's PATH.
	noAnswer string
	// curl is the curl the guard's reads through curl run (Guard.CurlGet): Options.Curl, or
	// curlProgram where that is empty. This package's own tests set it directly.
	curl string
	// copyBase and copyFallback are where the private copy of a check under a deployment's digest
	// is made (privateCopy): Options.PrivateCopyBase for both where it is set, and otherwise
	// PrivateCopyBase and privateCopyFallback. This package's own tests set them directly.
	copyBase, copyFallback string
	// waitDelay, where it is not 0, stands in for procgroup.WaitDelay in every child of the guard,
	// as exec's WaitDelay and as the wait readOutput gives a child's output after it exits
	// (Guard.exitDelay). This package's own tests set it to an hour, so that the end after the
	// exit never comes first and the end a test checks is the one the read takes.
	waitDelay time.Duration
	// onWait, where it is not nil, is handed the argv of each child whose output the guard reads as
	// it is written (Guard.readOutput) and what that child's Wait answered. This package's own
	// tests set it, to see which error a child's exit gave the read's end (readEndDelay).
	onWait func(argv []string, waitErr error)
}

// New is a guard with opts' seams. It resolves no check: the shared check publishes often, and a
// guard that resolved one at start would keep running a copy the current rules have moved past, so
// every call resolves it again.
func New(opts Options) *Guard {
	g := &Guard{opts: opts, curl: curlProgram, copyBase: PrivateCopyBase, copyFallback: privateCopyFallback}
	if opts.Curl != "" {
		g.curl = opts.Curl
	}
	if opts.PrivateCopyBase != "" {
		g.copyBase, g.copyFallback = opts.PrivateCopyBase, opts.PrivateCopyBase
	}
	g.noAnswer = noAnswer(g.base())
	return g
}

// base is the environment the guard's children start from before anything is added to it: the
// one the caller gave, or this process's own where it gave none.
func (g *Guard) base() []string {
	if g.opts.Env != nil {
		return g.opts.Env
	}
	return os.Environ()
}

// RunByRealGhVar is the variable `legion gh` gives every real gh it runs, naming that gh. A `legion
// gh` started under it is one a real gh led back to, a wrapper or shim run as the real gh, and it
// refuses, exit 2, once it has answered gh's own git credential helper and the shared check's
// visibility read (cmd/legion's runGh). It is exported, as VisibilityReadVar is, so that the name
// `legion gh` sets and refuses under and the name the guard blanks (Guard.env) are written once.
const RunByRealGhVar = "LEGION_GH_RUN_BY_REAL_GH"

// env is the environment a child of this guard starts from: base, the names that make a
// credential prompt fail closed, RunByRealGhVar blanked, and extra on top, which wins over all
// three because exec takes the last value of a name. inherit, where it is not nil, keeps only the
// names of base it answers true for (child.inherit). The base is the caller's rather than read
// here, so that a child costs one copy of the process environment and not two (Guard.run).
//
// RunByRealGhVar is blanked because the guard, and not the real gh, is what runs its children. A
// real gh's own push reaches the guard with that variable set: gh's git inherits it, and git's
// pre-push hook runs `legion leak-check pre-push`. The guard's visibility reads run `gh`, which in
// a pod is `legion gh`, and the shared check's own read reaches `legion gh` too (its
// answerVisibilityRead, which reads through a guard of its own). Under the variable every one of
// them would be refused, and so would the push. Blanking it stays bounded: `legion gh` runs each
// read with a real gh it gives the variable again, so a real gh that leads back to `legion gh` is
// still refused one start later.
func (g *Guard) env(base []string, inherit func(name string) bool, extra ...string) []string {
	out := make([]string, 0, len(base)+5+len(extra))
	if inherit == nil {
		out = append(out, base...)
	} else {
		for _, entry := range base {
			if name, _, _ := strings.Cut(entry, "="); inherit(name) {
				out = append(out, entry)
			}
		}
	}
	out = append(out, promptsNothing(g.noAnswer)...)
	out = append(out, RunByRealGhVar+"=")
	return append(out, extra...)
}

// Record is one piece of a write the rules read.
type Record struct {
	// Label is what the report calls the record, as it was written: "the title", "commit 1a2b3c4
	// message", a file's path. A report line holds the label as shownText shows it, and then the
	// text that matched and the line it stands on (RuleSet.scan).
	Label string
	// Text is what the rules match against.
	Text string
	// Path is the path of the file the destination will hold the text at, which is what the
	// engine's path rules and a rule file's path-scoped allowlist read: a pushed file's added or
	// renamed path and each of its added lines carry it. Every other record carries none, so that
	// no label is read as a path. A file a gh call sends carries none either: GitHub never sees
	// the name the file has here, so its label is that name and its Path is empty.
	Path string
	// FirstLine is the record's first line in its file, counted from 1, so that a report names
	// the file's line: an added line's line in the file it is added to. 0 is a record that is no
	// file's line, whose own first line is line 1.
	FirstLine int
}

// RefRecord is the record of a ref name, the whole of what the daemon's branch create has to
// show (RuleSet.Check) and one record of every push. A report shows the ref name as the line its
// match stands on, as it shows any record's text.
func RefRecord(ref string) Record {
	return Record{Label: "the ref name", Text: ref}
}

// rules is the private-name rule set a write of what runs: the rule file RulesFileVar names in
// the base environment, read and compiled at this write; else none, which is nil with no error. A
// write that runs no rule says nothing of it, since the daemon's boot has said once that the
// deployment names no rule file. An empty RulesFileVar names none, as an unset one does.
//
// A file named that cannot be used — missing, unreadable, longer than MaxRulesFile, or one
// LoadRules refuses — is a setup failure naming the file, as shownText shows it, and why, so the
// write is refused: a deployment that named rules meant them to run, and a guard that ran none in
// their place would pass what they refuse.
func (g *Guard) rules(what string) (*RuleSet, error) {
	path := value(g.base(), RulesFileVar)
	if path == "" {
		return nil, nil
	}
	named := "the private-name rule file " + shownText(path) + ", which " + RulesFileVar + " names, "
	content, err := ReadRulesContent(path)
	if err != nil {
		return nil, &SetupFailure{What: what, Why: named + err.Error()}
	}
	rules, err := LoadRules(content)
	if err != nil {
		return nil, &SetupFailure{What: what, Why: named + "is no rule file the guard can run", Err: err}
	}
	return rules, nil
}

// TextRequest is a piece of text a write would publish, as the shared check's text shape reads it:
// the first line is the title and the rest is the body.
type TextRequest struct {
	// Repo is the destination repository, as "owner/name". Empty where the caller knows no
	// destination, which the guard reads as public (scanThrough).
	Repo string
	// Text is the whole text: line one the title, the rest the body.
	Text string
}

// Text checks a piece of text a write would publish: a pull request's title and body, an issue
// comment, a release note. The shared check reads it on standard input in its check-public-text
// shape, and the rule pass, where a rule is named, then reads the title and the body as two
// records of their own. Under a deployment that runs no shared check (sharedCheck) the rule pass
// is the whole of it.
func (g *Guard) Text(ctx context.Context, req TextRequest) error {
	const what = "the text"
	check, err := g.sharedCheck(what)
	if err != nil {
		return err
	}
	defer check.remove()
	if check.path != "" {
		if err := g.shared(ctx, what, child{
			argv:  modeArgv(check.path, "check-public-text", "-"),
			stdin: strings.NewReader(req.Text),
		}); err != nil {
			return err
		}
	}
	rules, err := g.rules(what)
	if err != nil {
		return err
	}
	if rules == nil {
		return nil
	}
	public, err := g.scanThrough(ctx, what, req.Repo)
	if err != nil {
		return err
	}
	if !public {
		return nil
	}
	title, body, _ := strings.Cut(req.Text, "\n")
	return rules.Check(ctx, what, Record{Label: "the title", Text: title}, Record{Label: "the body", Text: body})
}

// scanThrough is whether the rule pass runs for a write to slug: it does for a destination GitHub
// reports public, and for one the caller could not name, which is the fail-closed answer — a
// destination nobody can name is scanned as if it were public. A visibility that cannot be read is
// a setup failure and refuses the write, as it does in the shared check. A read GitHub answered
// 404 refuses the write too, as a *NotFound, which says what GitHub answered.
//
// A slug that is no repository GitHub could serve is a setup failure before any read is made, and
// that is where the one recursion this package can make is broken. A pod's gh is `legion gh`,
// which runs this guard; the exemption that command makes for the check's own visibility read
// requires the slug to be one ghrepo.Parse takes (its ghVisibilityRead), so a read for any other
// slug would go back through `legion gh`, into the guard, into a read of the same slug, without
// end. The guard therefore reads no visibility it could not have exempted. The refusal carries no
// error of Parse's, which says again the slug the refusal names. Each refusal names the slug as
// shownText shows it.
func (g *Guard) scanThrough(ctx context.Context, what, slug string) (bool, error) {
	if slug == "" {
		return true, nil
	}
	if _, err := ghrepo.Parse("", slug); err != nil {
		return false, &SetupFailure{
			What: what,
			Why:  "the destination " + shownText(slug) + " is no repository GitHub serves, so no visibility can be read for it",
		}
	}
	visibility, err := g.Visibility(ctx, slug)
	switch {
	case errors.As(err, new(*notFoundError)):
		return false, &NotFound{What: what, Repo: shownText(slug)}
	case err != nil:
		return false, &SetupFailure{
			What: what,
			Why:  "the visibility of " + shownText(slug) + " could not be read",
			Err:  err,
		}
	}
	return visibility == Public, nil
}

// value is the value of key in env, as exec reads it: the last entry of that name wins.
func value(env []string, key string) string {
	for _, entry := range slices.Backward(env) {
		if name, v, ok := strings.Cut(entry, "="); ok && name == key {
			return v
		}
	}
	return ""
}
