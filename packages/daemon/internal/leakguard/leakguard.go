// Package leakguard is the one check a Legion write to a public GitHub repository passes before
// it writes. Two checks run, in this order, and either one refuses the write:
//
//   - The shared public leak check, a script this repository does not hold, found at each call
//     (Resolve) and run in the shape the write has: a push, a gh call, or a piece of text.
//   - This repository's own private-name rules, the embedded private-names.toml, run in process
//     over the records the write is made of, and only when the destination repository is one
//     GitHub reports public.
//
// A call site reaches the guard through one of four entry points — Push, GhCall, Text and Rules —
// and reads one of two answers from a refused write: a *Refusal, which is about the text the write
// carries, and a *SetupFailure, which is the check missing, the check failing, the engine failing,
// or a destination whose visibility cannot be read. Both refuse the write; they are two types so
// that a call site can say which one it met, the way the shared check itself keeps
// "LEAK CHECK: refusing" apart from "LEAK CHECK SETUP ERROR".
//
// What the guard reports is the record and the class: never the text that matched, never the
// component of a path that matched, and never anything the detection engine itself printed. The
// classes are the descriptions of the rules in private-names.toml, and a path in a report has
// every component the repository and company rules match replaced by "<name>", the masking
// .github/scripts/check-private-names.sh does (its lines 145-154).
//
// The guard sets neither PUBLIC_LEAK_CHECK_REPO_NAMES nor PUBLIC_LEAK_CHECK_INTERNAL for the
// shared check. That is a decision of this issue's approved spec, not an omission: with those two
// switches on, the shared check refuses a dispatch:// link, and 786 tracked files of this
// repository hold one, so every write of this repository would be refused. A later reader who
// wants either switch is changing that decision, not filling a gap.
package leakguard

import (
	"context"
	"os"
	"slices"
	"strings"

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

// Options are the guard's seams, each of them a field a test fills and a call site leaves alone.
// The engine needs no seam: its rules are embedded in this package and it is linked into this
// binary, so there is no copy of it for a test to stand in for.
type Options struct {
	// Check is the shared public leak check's path. Empty is the ordinary case, where every call
	// resolves the check for itself (Resolve); a test fills it with a stand-in of its own.
	Check string
	// Visibility reads what GitHub reports a repository slug ("owner/name"). Nil is the ordinary
	// case, the gh read the shared check itself makes (Guard.ghVisibility).
	Visibility func(ctx context.Context, slug string) (Visibility, error)
	// Clock supplies the time and bounded wait of a rate-limited visibility read. Nil is wall time.
	Clock Clock
	// OnVisibility observes the source of a successful visibility answer. A caller uses it for
	// audit logging and leaves it nil when it has no audit record of its own.
	OnVisibility func(VisibilitySource)
	// Env is the environment every child process starts from: the shared check, that gh read, and
	// the git commands a push's records are read with. Nil means this process's own
	// (os.Environ()). The guard adds to it the names that make a credential prompt fail closed
	// (promptsNothing), and each child's own names on top of those.
	Env []string
}

// Guard runs the two checks. It holds no answer of an earlier call: every call site builds a
// guard of its own for the one write it is about to make (cmd/legion's gh and leak-check,
// internal/rescue, internal/ghbranch), and each entry point reads a destination's visibility at
// most once, so there is nothing a shared answer would spare.
type Guard struct {
	opts Options
	// noAnswer is the askpass program the children are pointed at, resolved once from the base
	// environment's PATH.
	noAnswer string
}

// New is a guard with opts' seams. It resolves no check: the shared check publishes often, and a
// guard that resolved one at start would keep running a copy the current rules have moved past, so
// every call resolves it again.
func New(opts Options) *Guard {
	g := &Guard{opts: opts}
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

// env is the environment a child of this guard starts from: base, the names that make a
// credential prompt fail closed, and extra on top, which wins over both because exec takes the
// last value of a name. The base is the caller's rather than read here, so that a child costs
// one copy of the process environment and not two (Guard.run).
func (g *Guard) env(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+4+len(extra))
	out = append(out, base...)
	out = append(out, promptsNothing(g.noAnswer)...)
	return append(out, extra...)
}

// Record is one piece of a write the rules read: Label is what the report calls it, and Text is
// what the rules match against. A label never carries the write's own text, since the report is
// printed and that text is what must not be, with one exception this repository's own check
// already makes: a record about a file carries that file's path, masked in the report as
// .github/scripts/check-private-names.sh masks one. The path is also what the engine's path
// rules read, and it is the whole label for a second reason — the rule file's one path-scoped
// allowlist is anchored on a whole path, and a label with anything else in it would take that
// allowlist away.
type Record struct {
	Label string
	Text  string
}

// RefRecord is the record of a ref name, the whole of what the daemon's branch create has to
// show and one record of every push. Its label names no ref: a ref name is text of the caller's,
// and a report that printed it would carry out of this process the very thing it refused.
func RefRecord(ref string) Record {
	return Record{Label: "the ref name", Text: ref}
}

// Rules runs the rule-file pass over records and nothing else: no shared check, and no visibility
// read. It is what a write with no repository behind it has — the daemon's branch create, which
// holds a ref name and no checkout at all.
func (g *Guard) Rules(ctx context.Context, records ...Record) error {
	return g.rules(ctx, "the write", records)
}

// rules is the rule-file pass over records, reported as a refusal of what.
func (g *Guard) rules(ctx context.Context, what string, records []Record) error {
	engine, err := compiled()
	if err != nil {
		return &SetupFailure{What: what, Why: "the private-name rules could not be read", Err: err}
	}
	lines, err := engine.scan(ctx, records)
	if err != nil {
		return &SetupFailure{What: what, Why: "the private-name rules did not finish", Err: err}
	}
	if len(lines) == 0 {
		return nil
	}
	return &Refusal{What: what, By: byRules, Lines: lines}
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
// shape, and the rule pass then reads the title and the body as two records of their own.
func (g *Guard) Text(ctx context.Context, req TextRequest) error {
	const what = "the text"
	check, err := g.Resolve()
	if err != nil {
		return &SetupFailure{What: what, Why: whyNoCheck, Err: err}
	}
	if err := g.shared(ctx, what, child{
		argv:  []string{check, "check-public-text", "-"},
		stdin: strings.NewReader(req.Text),
	}); err != nil {
		return err
	}
	public, err := g.scanThrough(ctx, what, req.Repo)
	if err != nil {
		return err
	}
	if !public {
		return nil
	}
	title, body, _ := strings.Cut(req.Text, "\n")
	return g.rules(ctx, what, []Record{{Label: "the title", Text: title}, {Label: "the body", Text: body}})
}

// scanThrough is whether the rule pass runs for a write to slug: it does for a destination GitHub
// reports public, and for one the caller could not name, which is the fail-closed answer — a
// destination nobody can name is scanned as if it were public. A visibility that cannot be read is
// a setup failure and refuses the write, as it does in the shared check.
//
// A slug that is no repository GitHub could serve is a setup failure before any read is made, and
// that is where the one recursion this package can make is broken. A pod's gh is `legion gh`,
// which runs this guard; the exemption that command makes for the check's own visibility read
// requires the slug to be one ghrepo.Parse takes (its ghVisibilityRead), so a read for any other
// slug would go back through `legion gh`, into the guard, into a read of the same slug, without
// end. The guard therefore reads no visibility it could not have exempted. The refusal carries no
// error of Parse's: that refusal repeats the slug as it was written, and a report carries a
// masked name alone.
func (g *Guard) scanThrough(ctx context.Context, what, slug string) (bool, error) {
	if slug == "" {
		return true, nil
	}
	if _, err := ghrepo.Parse("", slug); err != nil {
		return false, &SetupFailure{
			What: what,
			Why:  "the destination " + maskPath(slug) + " is no repository GitHub serves, so no visibility can be read for it",
		}
	}
	visibility, err := g.Visibility(ctx, slug)
	if err != nil {
		return false, &SetupFailure{
			What: what,
			Why:  "the visibility of " + maskPath(slug) + " could not be read",
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
