// kept.go is what a Client keeps of Dispatch's answers in the broker's database (Keep), and how it
// answers from them while Dispatch cannot be asked: a person's member kind (MemberKind, from
// person_member_kind), whether Dispatch holds a person (KeptHold, from person_standing, for the
// hold gate) and whether a person may see a project (Access under OrKept, from project_standing).
// The call that fetches a person answer writes its kind and, where it names held, its standing.
// A held answer the broker could not write is written again by each read that rests on it
// (keepHeldAgain).
package projectaccess

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/metrics"
)

// keepTimeout bounds the write of one answer to the Keeper, and keptReadTimeout the read of the
// kept member kind and of a kept project answer while Dispatch cannot be asked. Each runs apart
// from its caller's context, so a read whose caller's own bound passed while Dispatch did not
// answer (RelayedReadBound) is still decided by what was kept, and a caller that hangs up stops no
// write.
const (
	keepTimeout     = time.Second
	keptReadTimeout = time.Second
)

// keptLogEvery is how often a read answered from what was kept while Dispatch could not be asked
// is counted, per person or per person and project, and a served one logged (KeptFullMessage,
// KeptProjectMessage).
const keptLogEvery = time.Minute

// KeptFullMessage is logged at WARN, at most once a minute per person, when MemberKind answers a
// person full from the member kind kept for them because Dispatch could not be asked, followed by
// person=<the canonical login>, as_of=<the time the kept answer rests on> and error=<why Dispatch
// could not be asked>.
const KeptFullMessage = "broker: Dispatch could not be asked what kind of member a person is; serving them as the full member Dispatch last answered"

// keepFailedMessage is logged at WARN when an answer of the person route could not be kept: while
// Dispatch cannot be asked the person is answered from the answer kept before it, and refused
// where this one named a kind other than full or held them (keeping.unkept, keeping.unkeptHeld).
const keepFailedMessage = "broker: Dispatch's answer for a person could not be kept; while Dispatch cannot be asked they are answered from the answer kept before it, and refused where this one is not full or holds them"

// errKeepsNothing is KeptHold's failure, and a held answer's Unkept, on a client never given a
// keeper (Keep): it keeps no answer to serve while Dispatch cannot be asked.
var errKeepsNothing = errors.New("projectaccess: this client keeps no answer of Dispatch's (Keep)")

// errNotKeptYet is a held answer's Unkept while no write has kept it and none was tried since.
var errNotKeptYet = errors.New("projectaccess: no write has kept Dispatch's answer that holds the person yet")

// Keeper is where a Client keeps each answer of Dispatch it fetches, and reads back the last one,
// so that while Dispatch cannot be asked it answers from what Dispatch last gave (Keep): the
// broker's person_member_kind, person_standing and project_standing (internal/cipher/store). A
// write keeps its answer unless the row holds a later one: a person answer later by Dispatch's own
// clock (as_of), a project answer asked later, since Dispatch's project answer names no time of its
// own. So every broker process and a restart read the same answer.
type Keeper interface {
	// KeepPersonAnswer writes one answer of Dispatch's person route for person, as of asOf, in one
	// transaction: the member kind memberKind it named ("" for none), and, where held is not nil,
	// whether Dispatch holds the person, with the deadline notHeldUntil (nil for none); each
	// unless its row holds an answer Dispatch gave later.
	KeepPersonAnswer(ctx context.Context, person, memberKind string, held *bool, asOf time.Time, notHeldUntil *time.Time) error
	// KeptMemberKind is the member kind kept for person. found is false when no kind is kept.
	KeptMemberKind(ctx context.Context, person string) (member store.MemberKind, found bool, err error)
	// PersonStanding is the last answer kept of whether Dispatch holds person; found is false
	// when none is kept.
	PersonStanding(ctx context.Context, person string) (standing store.Standing, found bool, err error)
	// KeepProjectStanding writes standing, Dispatch's answer for person and project.
	KeepProjectStanding(ctx context.Context, person, project string, standing store.ProjectStanding) error
	// KeptProjectStanding is the last answer kept for person and project, as KeepProjectStanding
	// wrote it, and when it was asked; found is false when no answer is kept.
	KeptProjectStanding(ctx context.Context, person, project string) (standing store.ProjectStanding, found bool, err error)
}

// keeping is what a Client given a Keeper (Keep) holds beside it: where its WARN lines go, the
// counters of the reads it answered from what was kept, what it remembers of the answers no write
// took, and when it last counted each read answered from what was kept. A Client never given a
// keeper holds none (Client.kept is nil). Every map is guarded by the Client's mu.
type keeping struct {
	keeper Keeper
	log    *slog.Logger
	// kindUnchecked and projectUnchecked count the member-kind reads and the project reads
	// answered from what was kept (broker_member_kind_unchecked_total and
	// broker_project_visibility_unchecked_total); nil counts nothing.
	kindUnchecked    *metrics.Counter
	projectUnchecked *metrics.Counter
	// unkept is, for each person, the latest answer of the person route naming a kind other than
	// full that no write has kept: while Dispatch cannot be asked it stands over an older kept
	// answer, so a kind this process last read as not full is never served on an older full. It is
	// forgotten once a write keeps that answer or a later one, and never otherwise.
	unkept map[string]unkeptKind
	// unkeptHeld is, for each person, the latest answer that Dispatch holds them that no write has
	// kept. It is written again at each read that rests on it (keepHeldAgain), and while Dispatch
	// cannot be asked it stands over an older kept answer (KeptHold), so a person this process last
	// read as held is never served on an older answer that they were not. It is forgotten once a
	// write keeps it, or once an answer of whether Dispatch holds them as of later arrives, and
	// never otherwise.
	unkeptHeld map[string]cachedPerson
	// unkeptProjects is, for each person and project, when the latest answer that hides the
	// project from the person, which no write has kept, was asked: it stands over an older kept
	// answer as unkept does, and is forgotten the same way.
	unkeptProjects map[accessKey]time.Time
	// unkeptExists is, for each project, when the latest answer that it exists, for any person,
	// which no write has kept, was asked: a kept answer that Dispatch holds no such project asked
	// no later is no longer the project's, as the Keeper reads one beside a kept answer that it
	// exists (Keeper.KeptProjectStanding), so it refuses as nothing kept. It is forgotten once a
	// write for that project asked no earlier takes, and never otherwise.
	unkeptExists map[string]time.Time
	// unkeptPrivate is, for each project, when the latest answer that it is private, for any
	// person or for none, which no write has kept, was asked: a kept public answer asked no later
	// is no longer the project's, as the Keeper reads one beside a kept answer that it is private,
	// so it refuses as nothing kept. It is forgotten once a write of a private or no-project
	// answer for that project asked no earlier takes, and never otherwise.
	unkeptPrivate map[string]time.Time
	// kindNoted and projectNoted are when MemberKind last counted each person's read, and Access
	// each person and project's read, answered from what was kept, and under which label (noted).
	kindNoted    map[string]keptNote
	projectNoted map[accessKey]keptNote
}

// unkeptKind is an answer's member kind that no write has kept, and the as_of it rests on.
type unkeptKind struct {
	kind string
	asOf time.Time
}

// keptNote is one count of a read answered from what was kept: its label and when.
type keptNote struct {
	kept string
	at   time.Time
}

// Keep makes c keep each answer of Dispatch's person route and project route it fetches in keeper,
// whichever read fetched it, and answer from what it kept there while Dispatch cannot be asked: a
// member-kind read (MemberKind), the hold gate's read of the last hold (KeptHold) and a project
// read made under OrKept (Access). Its WARN lines go to log (nil is slog.Default()), and
// broker_member_kind_unchecked_total and broker_project_visibility_unchecked_total are counted on
// registry (nil counts nothing). It is set before c is first used, as UseClock is. A client never
// given a keeper keeps nothing and fails every read Dispatch does not answer.
func (c *Client) Keep(keeper Keeper, log *slog.Logger, registry *metrics.Registry) {
	if log == nil {
		log = slog.Default()
	}
	kept := &keeping{
		keeper:         keeper,
		log:            log,
		unkept:         map[string]unkeptKind{},
		unkeptHeld:     map[string]cachedPerson{},
		unkeptProjects: map[accessKey]time.Time{},
		unkeptExists:   map[string]time.Time{},
		unkeptPrivate:  map[string]time.Time{},
		kindNoted:      map[string]keptNote{},
		projectNoted:   map[accessKey]keptNote{},
	}
	if registry != nil {
		kept.kindUnchecked = registry.NewCounter("broker_member_kind_unchecked_total",
			"Member-kind reads the broker answered from the kind it kept for the person because Dispatch could not be asked, at most one a minute per person, by that kind: full, served; restricted, none or unreadable, refused.")
		kept.projectUnchecked = registry.NewCounter("broker_project_visibility_unchecked_total",
			"Project reads of the request machine's person rule the broker answered from the answer it kept for the person and project because Dispatch could not be asked, at most one a minute per person and project, by that answer: public, visible or missing, served; not_visible, none or unreadable, refused.")
	}
	c.kept = kept
}

// noted reports whether a read of key answered from what was kept, under the label kept, is due
// to be counted, and logged where it is served, and records it when it is: the first such read of
// key, one whose label changed, and one keptLogEvery after the last counted. Once notes holds
// maxCached reads, it first forgets those counted keptLogEvery ago or more. It takes c.mu.
func noted[K comparable](c *Client, notes map[K]keptNote, key K, kept string) bool {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := notes[key]; ok && last.kept == kept && now.Sub(last.at) < keptLogEvery {
		return false
	}
	if len(notes) >= maxCached {
		for other, note := range notes {
			if now.Sub(note.at) >= keptLogEvery {
				delete(notes, other)
			}
		}
	}
	notes[key] = keptNote{kept: kept, at: now}
	return true
}

// The labels of broker_member_kind_unchecked_total: the kind kept for a person whose member-kind
// read Dispatch did not answer. keptFull is served; every other is refused.
const (
	keptFull       = MemberFull
	keptRestricted = MemberRestricted
	// keptNone is no answer kept for the person, or a kept answer naming no kind or MemberNone.
	keptNone = "none"
	// keptUnreadable is a kept answer the database could not read, or one naming a kind this
	// broker does not know.
	keptUnreadable = "unreadable"
)

// keptAnswerExpired reports whether an answer kept at at is too old to serve while Dispatch cannot
// be asked. A zero time is no bounded answer and is never served.
func (c *Client) keptAnswerExpired(at time.Time) bool {
	return at.IsZero() || c.now().Sub(at) >= KeptAnswerTTL
}

// keptKind is MemberKind's answer for person while Dispatch cannot be asked (cause): MemberFull
// where the last answer kept for them is full, and cause otherwise, wrapped with what was kept, so
// the caller refuses as it refuses with no Keeper (Restricted's *MemberKindUncheckable, in
// uncheckableReason's words). The last answer is the later, by as_of, of the one the Keeper holds
// and one this process read and could not keep (keeping.unkept), which is never full. The Keeper
// is read apart from ctx, bounded by keptReadTimeout. Each read is counted in
// broker_member_kind_unchecked_total by its label (keptFull, keptRestricted, keptNone or
// keptUnreadable), and a served one logged at WARN (KeptFullMessage), at most once a minute per
// person.
func (c *Client) keptKind(ctx context.Context, person string, cause error) (string, error) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keptReadTimeout)
	member, found, readErr := c.kept.keeper.KeptMemberKind(readCtx, person)
	cancel()
	c.mu.Lock()
	unkept, remembered := c.kept.unkept[person]
	c.mu.Unlock()
	var kept string
	switch {
	case remembered && (readErr != nil || !found || !unkept.asOf.Before(member.AsOf)):
		member, kept = store.MemberKind{Kind: unkept.kind, AsOf: unkept.asOf}, keptLabel(unkept.kind)
	case readErr != nil:
		kept = keptUnreadable
	case !found:
		kept = keptNone
	default:
		kept = keptLabel(member.Kind)
	}
	if kept == keptFull && c.keptAnswerExpired(member.AsOf) {
		return "", fmt.Errorf("%w; the member kind kept for %s is older than %s; the broker serves a kept full for at most %s while Dispatch cannot be asked", cause, person, KeptAnswerTTL, KeptAnswerTTL)
	}
	if noted(c, c.kept.kindNoted, person, kept) {
		c.kept.kindUnchecked.Inc([2]string{"kept", kept})
		if kept == keptFull {
			c.kept.log.Warn(KeptFullMessage, "person", person, "as_of", member.AsOf.UTC().Format(time.RFC3339Nano), "error", cause)
		}
	}
	switch {
	case kept == keptFull:
		return MemberFull, nil
	case readErr != nil && kept == keptUnreadable:
		return "", fmt.Errorf("%w; the member kind kept for %s could not be read either: %v", cause, person, readErr)
	}
	return "", fmt.Errorf("%w; the member kind kept for %s is %s, and only a kept full is served while Dispatch cannot be asked", cause, person, kept)
}

// keptLabel is the label of a kept member kind: MemberFull and MemberRestricted as they are,
// MemberNone and no kind keptNone, and a kind this broker does not know keptUnreadable.
func keptLabel(kind string) string {
	switch kind {
	case MemberFull, MemberRestricted:
		return kind
	case MemberNone, "":
		return keptNone
	}
	return keptUnreadable
}

// keepPerson keeps answer, the person route's answer for person, in c's Keeper (Keep), whichever
// read fetched it: its member kind, and, where it names held, whether Dispatch holds the person
// with the deadline of a not-held answer (deadline), in one write (writePerson). An answer whose
// held Dispatch left out, as Dispatch in this tree leaves it out of every answer, keeps its kind
// alone: its kind is Dispatch's answer whatever the hold. An answer with no as_of cannot be
// ordered against a kept one, and is not written. A write that fails is logged at WARN, and what
// it would have kept is remembered (keeping.settleLocked).
func (c *Client) keepPerson(ctx context.Context, person string, answer cachedPerson) {
	if c.kept == nil || answer.asOf.IsZero() {
		return
	}
	if err := c.writePerson(ctx, person, answer); err != nil {
		c.kept.log.Warn(keepFailedMessage, "person", person, "member_kind", answer.kind, "as_of", answer.asOf.UTC().Format(time.RFC3339Nano), "error", err)
	}
}

// writePerson writes answer for person to c's Keeper, apart from ctx and bounded by keepTimeout, so
// a caller that hangs up stops no write, and settles by its outcome what c remembers of answers no
// write took. c.kept is set.
func (c *Client) writePerson(ctx context.Context, person string, answer cachedPerson) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keepTimeout)
	err := c.kept.keeper.KeepPersonAnswer(writeCtx, person, answer.kind, answer.held, answer.asOf, deadline(answer))
	cancel()
	c.mu.Lock()
	c.kept.settleLocked(person, answer, err)
	c.mu.Unlock()
	return err
}

// deadline is the deadline answer keeps beside it: its not_held_until where it says Dispatch does
// not hold the person and names one, and none otherwise, so a held answer keeps no deadline
// whatever it carried.
func deadline(answer cachedPerson) *time.Time {
	if answer.held == nil || *answer.held || answer.until.IsZero() {
		return nil
	}
	until := answer.until
	return &until
}

// settleLocked updates what k remembers for person of the answers no write took, by err, the
// outcome of the write of answer. A write that takes forgets an unkept kind, and an unkept held
// answer, as of no later than its own. One that fails remembers its kind where it is not full, and
// the answer where it holds the person, each unless an answer as of later is remembered. And an
// answer of whether Dispatch holds the person, as of later than a remembered held one, forgets
// that one whether or not its own write took: it is Dispatch's later answer. c.mu is held.
func (k *keeping) settleLocked(person string, answer cachedPerson, err error) {
	switch remembered, ok := k.unkept[person]; {
	case err == nil && ok && !remembered.asOf.After(answer.asOf):
		delete(k.unkept, person)
	case err != nil && answer.kind != MemberFull && (!ok || answer.asOf.After(remembered.asOf)):
		k.unkept[person] = unkeptKind{kind: answer.kind, asOf: answer.asOf}
	}
	if answer.held == nil {
		return
	}
	switch remembered, ok := k.unkeptHeld[person]; {
	case err != nil && *answer.held && (!ok || answer.asOf.After(remembered.asOf)):
		k.unkeptHeld[person] = answer
	case ok && (answer.asOf.After(remembered.asOf) || err == nil && answer.asOf.Equal(remembered.asOf)):
		delete(k.unkeptHeld, person)
	}
}

// keepHeldAgain writes again the held answer for person that no write has kept (keeping.unkeptHeld),
// where c remembers one, and answers why none is kept yet: nil where c remembers none, or once this
// write takes it; this write's failure otherwise; and errKeepsNothing for a client never given a
// keeper.
func (c *Client) keepHeldAgain(ctx context.Context, person string) error {
	if c.kept == nil {
		return errKeepsNothing
	}
	c.mu.Lock()
	answer, remembered := c.kept.unkeptHeld[person]
	c.mu.Unlock()
	if !remembered {
		return nil
	}
	return c.writePerson(ctx, person, answer)
}

// KeptStanding is an answer of whether Dispatch holds a person, as the client keeps it (KeptHold):
// the Keeper's row, or a held answer this client read and no write has kept.
type KeptStanding struct {
	store.Standing
	// Unkept is, for a held answer this client read and no write has kept, why: the failure of
	// the last write of it. It is nil for an answer the Keeper keeps.
	Unkept error
}

// KeptHold is the last answer of whether Dispatch holds person that c keeps, for the hold gate to
// answer from while Dispatch cannot be asked (hold.Gate.Decide). It first writes again a held
// answer no write has kept (keepHeldAgain), and then answers the later, by as_of, of the answer
// the Keeper keeps (Keeper.PersonStanding) and a held answer this client read and no write keeps,
// which carries Unkept. found is false when neither is kept, which is no error. The Keeper is read
// on ctx, which the gate bounds. It reads the store on each call; a failed read is that call's
// error, never cached. A client never given a keeper answers errKeepsNothing.
func (c *Client) KeptHold(ctx context.Context, person string) (standing KeptStanding, found bool, err error) {
	if c.kept == nil {
		return KeptStanding{}, false, errKeepsNothing
	}
	person = record.CanonicalLogin(person)
	writeErr := c.keepHeldAgain(ctx, person)
	row, found, readErr := c.kept.keeper.PersonStanding(ctx, person)
	c.mu.Lock()
	unkept, remembered := c.kept.unkeptHeld[person]
	c.mu.Unlock()
	switch {
	case remembered && (readErr != nil || !found || !unkept.asOf.Before(row.AsOf)):
		if writeErr == nil {
			writeErr = errNotKeptYet
		}
		return KeptStanding{Standing: store.Standing{Held: true, AsOf: unkept.asOf}, Unkept: writeErr}, true, nil
	case readErr != nil:
		return KeptStanding{}, false, readErr
	}
	return KeptStanding{Standing: row}, found, nil
}

// KeptProjectMessage is logged at WARN, at most once a minute per person and project, when a read
// under OrKept is answered from the answer kept for that person and project because Dispatch could
// not be asked, and that answer shows them the project, followed by person=<the canonical login,
// empty for a read naming no person>, project=<the key>, kept=<public, visible or missing>,
// asked_at=<when the kept answer was asked> and error=<why Dispatch could not be asked>.
const KeptProjectMessage = "broker: Dispatch could not be asked whether a person may see a project; serving on the answer it last gave"

// keepProjectFailedMessage is logged at WARN when an answer of the project route could not be
// kept: while Dispatch cannot be asked the person and project are answered from the answer kept
// before it, and refused where this one hides the project (keeping.unkeptProjects).
const keepProjectFailedMessage = "broker: Dispatch's answer of whether a person may see a project could not be kept; while Dispatch cannot be asked it is answered from the answer kept before it, and refused where this one hides the project"

// The labels of broker_project_visibility_unchecked_total beside keptNone and keptUnreadable: what
// was kept for a person and project whose read Dispatch did not answer. keptPublic, keptVisible
// and keptMissing are served; keptNotVisible, keptNone and keptUnreadable are refused.
const (
	// keptPublic is a public project the person may see.
	keptPublic = VisibilityPublic
	// keptVisible is a private project whose list names the person.
	keptVisible = "visible"
	// keptMissing is a key Dispatch holds no project for, which hides nothing.
	keptMissing = "missing"
	// keptNotVisible is a project the person may not see: a private project whose list does not
	// name them, or a public one Dispatch hides from a restricted member.
	keptNotVisible = "not_visible"
)

// orKeptKey marks a context whose project reads are answered from the kept answer while Dispatch
// does not answer (OrKept).
type orKeptKey struct{}

// OrKept is ctx for project reads (Access) that, where Dispatch does not answer (Unanswered), a
// client that keeps its answers (Keep) answers from the answer it kept for that person and project:
// a project Dispatch last answered public and visible to them, until an answer that it is private
// is kept for anyone, asked at or after it, or private and visible to them, is answered visible,
// carrying its project, person and visibility alone and a zero AskedAt, since nothing Dispatch
// answered since bounds its age; a key Dispatch last answered it holds no project for is that
// answer again, a *ProjectNotFoundError with a zero AskedAt, until an answer that the project
// exists is kept for anyone, asked at or after it; and every other read fails as with no keeper: a
// project kept hidden from the person, one of which nothing is kept for them since it was made or
// made private, and one whose kept answer cannot be read. A read Dispatch answers is that answer,
// kept as every answer is. The request machine's person rule reads under it
// (requests.Machine.maySee), so while Dispatch does not answer a row is shown and decided by who
// last could; nothing else does, so the token exchange, an enrollment and a release still fail
// while Dispatch does not answer whether a person may see a project. It is a context marker, as
// Fresh is, so an Access that hands the context on keeps the rule.
func OrKept(ctx context.Context) context.Context {
	return context.WithValue(ctx, orKeptKey{}, true)
}

// orKept reports whether ctx is one OrKept made, or derived from one.
func orKept(ctx context.Context) bool {
	marked, _ := ctx.Value(orKeptKey{}).(bool)
	return marked
}

// keepProject writes Dispatch's answer for key, asked at asked, to c's Keeper, apart from ctx and
// bounded by keepTimeout, whichever read fetched it: the project's visibility and whether the
// person may see it, or, where missing, that Dispatch holds no such project. A write that fails
// is logged at WARN, and an answer that hides the project from the person, that no write took, is
// remembered (unkeptProjects) until a write keeps it or a later answer, so a later outage refuses
// the person rather than show them the project on an older kept answer; so is an answer that the
// project exists, that no write took (unkeptExists), so a later outage refuses everyone with no
// answer of their own since rather than read the project as none on an older kept answer that
// Dispatch held no such project; and so is an answer that the project is private, that no write
// took (unkeptPrivate), so a later outage refuses everyone with no answer of their own since
// rather than show them the project on an older kept answer that it was public. A write that takes
// forgets an unkept answer asked no later than its own; an unkept private answer, only a write of
// a private or no-project answer, since a later public answer does not undo that the project was
// private after the person's own answer, as the Keeper reads a kept one.
func (c *Client) keepProject(ctx context.Context, key accessKey, answer Answer, missing bool, asked time.Time) {
	if c.kept == nil {
		return
	}
	standing := store.ProjectStanding{Visibility: answer.Visibility, Visible: answer.Visible, AskedAt: asked}
	if missing {
		standing.Visibility, standing.Visible = "", false
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keepTimeout)
	err := c.kept.keeper.KeepProjectStanding(writeCtx, key.person, key.project, standing)
	cancel()
	c.mu.Lock()
	switch remembered, ok := c.kept.unkeptProjects[key]; {
	case err == nil && ok && !remembered.After(asked):
		delete(c.kept.unkeptProjects, key)
	case err != nil && !missing && !standing.Visible && (!ok || asked.After(remembered)):
		c.kept.unkeptProjects[key] = asked
	}
	switch remembered, ok := c.kept.unkeptExists[key.project]; {
	case err == nil && ok && !remembered.After(asked):
		delete(c.kept.unkeptExists, key.project)
	case err != nil && !missing && (!ok || asked.After(remembered)):
		c.kept.unkeptExists[key.project] = asked
	}
	switch remembered, ok := c.kept.unkeptPrivate[key.project]; {
	case err == nil && ok && !remembered.After(asked) && (missing || standing.Visibility == VisibilityPrivate):
		delete(c.kept.unkeptPrivate, key.project)
	case err != nil && !missing && standing.Visibility == VisibilityPrivate && (!ok || asked.After(remembered)):
		c.kept.unkeptPrivate[key.project] = asked
	}
	c.mu.Unlock()
	if err != nil {
		c.kept.log.Warn(keepProjectFailedMessage, "person", key.person, "project", key.project, "visible", standing.Visible && !missing,
			"asked_at", asked.UTC().Format(time.RFC3339Nano), "error", err)
	}
}

// keptProject is Access's answer for key while Dispatch cannot be asked (cause), under OrKept:
// from the later, by the time it was asked, of the answer the Keeper holds and one this process
// read and could not keep (unkeptProjects), which hides the project; the Keeper is read apart from
// ctx, bounded by keptReadTimeout. A kept answer showing the person the project is served, and a
// kept answer that Dispatch holds no such project is that answer, unless this process read an
// answer that the project exists, asked at or after it, and could not keep it (unkeptExists): that
// is no answer for the person, as the Keeper reads a kept one; and so is a kept public answer
// where this process read an answer that the project is private, asked at or after it, and could
// not keep it (unkeptPrivate). Every other read is cause, wrapped with what was kept, as a client
// with no keeper fails it. Each read is counted in broker_project_visibility_unchecked_total by
// its label, and a served one logged at WARN (KeptProjectMessage), at most once a minute per
// person and project.
func (c *Client) keptProject(ctx context.Context, key accessKey, cause error) (Answer, error) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keptReadTimeout)
	standing, found, readErr := c.kept.keeper.KeptProjectStanding(readCtx, key.person, key.project)
	cancel()
	c.mu.Lock()
	unkept, remembered := c.kept.unkeptProjects[key]
	exists, existsUnkept := c.kept.unkeptExists[key.project]
	private, privateUnkept := c.kept.unkeptPrivate[key.project]
	c.mu.Unlock()
	var kept string
	switch {
	case remembered && (readErr != nil || !found || !unkept.Before(standing.AskedAt)):
		kept = keptNotVisible
	case readErr != nil:
		kept = keptUnreadable
	case !found:
		kept = keptNone
	case standing.Visibility == "" && existsUnkept && !exists.Before(standing.AskedAt):
		kept = keptNone
	case standing.Visibility == "":
		kept = keptMissing
	case standing.Visibility == VisibilityPublic && privateUnkept && !private.Before(standing.AskedAt):
		kept = keptNone
	case !standing.Visible:
		kept = keptNotVisible
	case standing.Visibility == VisibilityPublic:
		kept = keptPublic
	case standing.Visibility == VisibilityPrivate:
		kept = keptVisible
	default:
		kept = keptUnreadable
	}
	if (kept == keptPublic || kept == keptVisible) && c.keptAnswerExpired(standing.AskedAt) {
		return Answer{}, fmt.Errorf("%w; the answer kept for %q and project %s is older than %s; the broker serves a kept visible project for at most %s while Dispatch cannot be asked", cause, key.person, key.project, KeptAnswerTTL, KeptAnswerTTL)
	}
	if noted(c, c.kept.projectNoted, key, kept) {
		c.kept.projectUnchecked.Inc([2]string{"kept", kept})
		switch kept {
		case keptPublic, keptVisible, keptMissing:
			c.kept.log.Warn(KeptProjectMessage, "person", key.person, "project", key.project, "kept", kept,
				"asked_at", standing.AskedAt.UTC().Format(time.RFC3339Nano), "error", cause)
		}
	}
	switch kept {
	case keptPublic, keptVisible:
		return Answer{Project: key.project, Visibility: standing.Visibility, Person: key.person, Visible: true}, nil
	case keptMissing:
		return Answer{}, &ProjectNotFoundError{Project: key.project}
	case keptUnreadable:
		if readErr != nil {
			return Answer{}, fmt.Errorf("%w; the answer kept for %s and project %s could not be read either: %v", cause, key.person, key.project, readErr)
		}
	}
	return Answer{}, fmt.Errorf("%w; the answer kept for %q and project %s is %s, and only one that shows the project is served while Dispatch cannot be asked", cause, key.person, key.project, kept)
}
