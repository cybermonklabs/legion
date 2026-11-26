// kept.go is what a Client keeps of Dispatch's answers in the broker's database (Keep), and how it
// answers from them while Dispatch cannot be asked: a person's member kind (MemberKind, from
// person_member_kind) and whether a person may see a project (Access under OrKept, from
// project_standing).
package projectaccess

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/metrics"
)

// keepTimeout bounds the write of one answer to the Keeper, and keptReadTimeout the read of the
// kept member kind while Dispatch cannot be asked. Each runs apart from its caller's context, so a
// read whose caller's own bound passed while Dispatch did not answer (RelayedReadBound) is still
// decided by the kept kind, and a caller that hangs up stops no write.
const (
	keepTimeout     = time.Second
	keptReadTimeout = time.Second
)

// keptLogEvery is how often MemberKind counts, per person, a read it answered from the kept member
// kind while Dispatch could not be asked, and logs one it served (KeptFullMessage).
const keptLogEvery = time.Minute

// KeptFullMessage is logged at WARN, at most once a minute per person, when MemberKind answers a
// person full from the member kind kept for them because Dispatch could not be asked, followed by
// person=<the canonical login>, as_of=<the time the kept answer rests on> and error=<why Dispatch
// could not be asked>.
const KeptFullMessage = "broker: Dispatch could not be asked what kind of member a person is; serving them as the full member Dispatch last answered"

// keepFailedMessage is logged at WARN when an answer of the person route could not be kept: while
// Dispatch cannot be asked the person is answered from the answer kept before it, and refused
// where this one named a kind other than full (Client.unkept).
const keepFailedMessage = "broker: Dispatch's answer of what kind of member a person is could not be kept; while Dispatch cannot be asked they are answered from the answer kept before it, and refused where this one is not full"

// Keeper is where a Client keeps each answer of Dispatch it fetches, and reads back the last one,
// so that while Dispatch cannot be asked it answers from what Dispatch last gave (Keep): the
// broker's person_member_kind and project_standing (internal/cipher/store). A write keeps its
// answer unless the row holds a later one: a person answer's kind later by Dispatch's own clock
// (as_of), a project answer asked later, since Dispatch's project answer names no time of its own.
// So every broker process and a restart read the same answer.
type Keeper interface {
	// KeepMemberKind writes the member kind memberKind ("" for none) that Dispatch's answer for
	// person named as of asOf, unless the row holds one Dispatch gave later.
	KeepMemberKind(ctx context.Context, person, memberKind string, asOf time.Time) error
	// KeptMemberKind is the member kind kept for person and the as_of it rests on: "" for an
	// answer that named none, and for one older than the answer of the hold the hold gate keeps
	// for person (person_standing); found is false when no kind is kept.
	KeptMemberKind(ctx context.Context, person string) (kind string, asOf time.Time, found bool, err error)
	// KeepProjectStanding writes Dispatch's answer for person ("" for none) and project, asked at
	// askedAt, unless the row holds one asked later: its visibility and whether person may see
	// the project, or, for visibility "", that Dispatch holds no such project.
	KeepProjectStanding(ctx context.Context, person, project, visibility string, visible bool, askedAt time.Time) error
	// KeptProjectStanding is the last answer kept for person and project, as KeepProjectStanding
	// wrote it, and when it was asked; found is false when no answer is kept.
	KeptProjectStanding(ctx context.Context, person, project string) (standing store.ProjectStanding, found bool, err error)
}

// unkeptKind is an answer's member kind that no write has kept, and the as_of it rests on.
type unkeptKind struct {
	kind string
	asOf time.Time
}

// keptNote is one count of a person's read answered from the kept kind: its label and when.
type keptNote struct {
	kept string
	at   time.Time
}

// Keep makes c keep each answer of Dispatch's person route and project route it fetches in keeper,
// whichever read fetched it, and answer from what it kept there while Dispatch cannot be asked: a
// member-kind read (MemberKind) and a project read made under OrKept (Access). Its WARN lines go to
// log (nil is slog.Default()), and broker_member_kind_unchecked_total and
// broker_project_visibility_unchecked_total are counted on registry (nil counts nothing). It is set
// before c is first used, as UseClock is. A client never given a keeper keeps nothing and fails
// every read Dispatch does not answer.
func (c *Client) Keep(keeper Keeper, log *slog.Logger, registry *metrics.Registry) {
	if log == nil {
		log = slog.Default()
	}
	c.keeper, c.keepLog = keeper, log
	if registry != nil {
		c.keptUnchecked = registry.NewCounter("broker_member_kind_unchecked_total",
			"Member-kind reads the broker answered from the kind it kept for the person because Dispatch could not be asked, at most one a minute per person, by that kind: full, served; restricted, none or unreadable, refused.")
		c.keptProjectUnchecked = registry.NewCounter("broker_project_visibility_unchecked_total",
			"Project reads of the request machine's person rule the broker answered from the answer it kept for the person and project because Dispatch could not be asked, at most one a minute per person and project, by that answer: public, visible or missing, served; not_visible, none or unreadable, refused.")
	}
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

// keptKind is MemberKind's answer for person while Dispatch cannot be asked (cause): MemberFull
// where the last answer kept for them is full, and cause otherwise, wrapped with what was kept, so
// the caller refuses as it refuses with no Keeper (Restricted's *MemberKindUncheckable, in
// uncheckableReason's words). The last answer is the later, by as_of, of the one the Keeper holds
// and one this process read and could not keep (unkept), which is never full; a kept kind older
// than the answer of the hold the Keeper keeps for them reads as none (Keeper.KeptMemberKind). The
// Keeper is read apart from ctx, bounded by keptReadTimeout. Each read is counted in
// broker_member_kind_unchecked_total by its label (keptFull, keptRestricted, keptNone or
// keptUnreadable), and a served one logged at WARN (KeptFullMessage), at most once a minute per
// person.
func (c *Client) keptKind(ctx context.Context, person string, cause error) (string, error) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keptReadTimeout)
	kind, asOf, found, readErr := c.keeper.KeptMemberKind(readCtx, person)
	cancel()
	c.mu.Lock()
	unkept, remembered := c.unkept[person]
	c.mu.Unlock()
	var kept string
	switch {
	case remembered && (readErr != nil || !found || !unkept.asOf.Before(asOf)):
		kept, asOf = keptLabel(unkept.kind), unkept.asOf
	case readErr != nil:
		kept = keptUnreadable
	case !found:
		kept = keptNone
	default:
		kept = keptLabel(kind)
	}
	c.noteKept(person, kept, asOf, cause)
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

// noteKept counts a read of person answered from the kept kind under kept, and logs one served
// (keptFull) at WARN with the as_of it rests on and cause, at most once a minute per person and
// again at once when the label changes.
func (c *Client) noteKept(person, kept string, asOf time.Time, cause error) {
	now := c.now()
	c.mu.Lock()
	last, noted := c.keptNoted[person]
	due := !noted || last.kept != kept || now.Sub(last.at) >= keptLogEvery
	if due {
		if len(c.keptNoted) >= maxCached {
			for other, note := range c.keptNoted {
				if now.Sub(note.at) >= keptLogEvery {
					delete(c.keptNoted, other)
				}
			}
		}
		c.keptNoted[person] = keptNote{kept: kept, at: now}
	}
	c.mu.Unlock()
	if !due {
		return
	}
	c.keptUnchecked.Inc([2]string{"kept", kept})
	if kept == keptFull {
		c.keepLog.Warn(KeptFullMessage, "person", person, "as_of", asOf.UTC().Format(time.RFC3339Nano), "error", cause)
	}
}

// keepPerson writes the member kind of answer, the person route's answer for person, to c's
// Keeper (Keep), apart from ctx and bounded by keepTimeout, whichever read fetched it. An answer
// whose held Dispatch left out, as it does while its own read of the hold fails, is written as any
// other: its kind is Dispatch's answer whatever the hold. An answer with no as_of, from a Dispatch
// older than the hold, cannot be ordered against a kept one, and is not written. A write that fails
// is logged at WARN, and an answer naming a kind other than full that no write took is remembered
// (unkept) until a write keeps it or a later answer, so a later outage answers that person from it
// rather than from an older kept full. A write that takes forgets an unkept answer no later than
// its own.
func (c *Client) keepPerson(ctx context.Context, person string, answer cachedPerson) {
	if c.keeper == nil || answer.asOf.IsZero() {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keepTimeout)
	err := c.keeper.KeepMemberKind(writeCtx, person, answer.kind, answer.asOf)
	cancel()
	c.mu.Lock()
	switch remembered, ok := c.unkept[person]; {
	case err == nil && ok && !remembered.asOf.After(answer.asOf):
		delete(c.unkept, person)
	case err != nil && answer.kind != MemberFull && (!ok || answer.asOf.After(remembered.asOf)):
		c.unkept[person] = unkeptKind{kind: answer.kind, asOf: answer.asOf}
	}
	c.mu.Unlock()
	if err != nil {
		c.keepLog.Warn(keepFailedMessage, "person", person, "member_kind", answer.kind, "as_of", answer.asOf.UTC().Format(time.RFC3339Nano), "error", err)
	}
}

// KeptProjectMessage is logged at WARN, at most once a minute per person and project, when a read
// under OrKept is answered from the answer kept for that person and project because Dispatch could
// not be asked, and that answer shows them the project, followed by person=<the canonical login,
// empty for a read naming no person>, project=<the key>, kept=<public, visible or missing>,
// asked_at=<when the kept answer was asked> and error=<why Dispatch could not be asked>.
const KeptProjectMessage = "broker: Dispatch could not be asked whether a person may see a project; serving on the answer it last gave"

// keepProjectFailedMessage is logged at WARN when an answer of the project route could not be
// kept: while Dispatch cannot be asked the person and project are answered from the answer kept
// before it, and refused where this one hides the project (Client.unkeptProjects).
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
	if c.keeper == nil {
		return
	}
	visibility, visible := answer.Visibility, answer.Visible
	if missing {
		visibility, visible = "", false
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keepTimeout)
	err := c.keeper.KeepProjectStanding(writeCtx, key.person, key.project, visibility, visible, asked)
	cancel()
	c.mu.Lock()
	switch remembered, ok := c.unkeptProjects[key]; {
	case err == nil && ok && !remembered.After(asked):
		delete(c.unkeptProjects, key)
	case err != nil && !missing && !visible && (!ok || asked.After(remembered)):
		c.unkeptProjects[key] = asked
	}
	switch remembered, ok := c.unkeptExists[key.project]; {
	case err == nil && ok && !remembered.After(asked):
		delete(c.unkeptExists, key.project)
	case err != nil && !missing && (!ok || asked.After(remembered)):
		c.unkeptExists[key.project] = asked
	}
	switch remembered, ok := c.unkeptPrivate[key.project]; {
	case err == nil && ok && !remembered.After(asked) && (missing || visibility == VisibilityPrivate):
		delete(c.unkeptPrivate, key.project)
	case err != nil && !missing && visibility == VisibilityPrivate && (!ok || asked.After(remembered)):
		c.unkeptPrivate[key.project] = asked
	}
	c.mu.Unlock()
	if err != nil {
		c.keepLog.Warn(keepProjectFailedMessage, "person", key.person, "project", key.project, "visible", visible && !missing,
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
// with no keeper fails it. Each read is counted in
// broker_project_visibility_unchecked_total by its label, and a served one logged at WARN
// (KeptProjectMessage), at most once a minute per person and project.
func (c *Client) keptProject(ctx context.Context, key accessKey, cause error) (Answer, error) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keptReadTimeout)
	standing, found, readErr := c.keeper.KeptProjectStanding(readCtx, key.person, key.project)
	visibility, visible, askedAt := standing.Visibility, standing.Visible, standing.AskedAt
	cancel()
	c.mu.Lock()
	unkept, remembered := c.unkeptProjects[key]
	exists, existsUnkept := c.unkeptExists[key.project]
	private, privateUnkept := c.unkeptPrivate[key.project]
	c.mu.Unlock()
	var kept string
	switch {
	case remembered && (readErr != nil || !found || !unkept.Before(askedAt)):
		kept, askedAt = keptNotVisible, unkept
	case readErr != nil:
		kept = keptUnreadable
	case !found:
		kept = keptNone
	case visibility == "" && existsUnkept && !exists.Before(askedAt):
		kept = keptNone
	case visibility == "":
		kept = keptMissing
	case visibility == VisibilityPublic && privateUnkept && !private.Before(askedAt):
		kept = keptNone
	case !visible:
		kept = keptNotVisible
	case visibility == VisibilityPublic:
		kept = keptPublic
	case visibility == VisibilityPrivate:
		kept = keptVisible
	default:
		kept = keptUnreadable
	}
	c.noteKeptProject(key, kept, askedAt, cause)
	switch kept {
	case keptPublic, keptVisible:
		return Answer{Project: key.project, Visibility: visibility, Person: key.person, Visible: true}, nil
	case keptMissing:
		return Answer{}, &ProjectNotFoundError{Project: key.project}
	case keptUnreadable:
		if readErr != nil {
			return Answer{}, fmt.Errorf("%w; the answer kept for %s and project %s could not be read either: %v", cause, key.person, key.project, readErr)
		}
	}
	return Answer{}, fmt.Errorf("%w; the answer kept for %q and project %s is %s, and only one that shows the project is served while Dispatch cannot be asked", cause, key.person, key.project, kept)
}

// noteKeptProject counts a read of key answered from the kept answer under kept, and logs one
// served (keptPublic, keptVisible or keptMissing) at WARN with the time it was asked and cause, at
// most once a minute per person and project and again at once when the label changes.
func (c *Client) noteKeptProject(key accessKey, kept string, askedAt time.Time, cause error) {
	now := c.now()
	c.mu.Lock()
	last, noted := c.keptProjectNoted[key]
	due := !noted || last.kept != kept || now.Sub(last.at) >= keptLogEvery
	if due {
		if len(c.keptProjectNoted) >= maxCached {
			for other, note := range c.keptProjectNoted {
				if now.Sub(note.at) >= keptLogEvery {
					delete(c.keptProjectNoted, other)
				}
			}
		}
		c.keptProjectNoted[key] = keptNote{kept: kept, at: now}
	}
	c.mu.Unlock()
	if !due {
		return
	}
	c.keptProjectUnchecked.Inc([2]string{"kept", kept})
	switch kept {
	case keptPublic, keptVisible, keptMissing:
		c.keepLog.Warn(KeptProjectMessage, "person", key.person, "project", key.project, "kept", kept,
			"asked_at", askedAt.UTC().Format(time.RFC3339Nano), "error", cause)
	}
}
