package workflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Repositories is what the daemon holds in memory of the repositories of its project, which the
// workflow reads, without I/O, when it first records an issue: whether Legion serves the
// repository an issue resolves to, and if so as GitHub names it and at which default branch. The
// daemon reads both before the fact that records the issue is applied (its PrepareRepository), so
// an answer it has not read yet is an error, and the fact is applied again once it has.
type Repositories interface {
	ResolveRepository(resolved string) (Resolution, error)
}

// Resolution is the repository an issue is recorded on: Repo and DefaultBranch when Legion serves
// it, with GitHubRepo, GitHub's spelling of Repo that Envoy's event subjects carry, or Refusal, why
// it does not. LeftOut is a mapped repository Legion leaves out, for a GitHub App not installed on
// it or, with Unread, for now, because GitHub did not answer its installation read; Refusal names
// which. An Unread issue is not refused but left: RefuseRecording says nothing on it, and the
// daemon takes it from Dispatch's listing once GitHub answers for the repository (its recovery).
type Resolution struct {
	Repo          string
	DefaultBranch string
	GitHubRepo    string
	Refusal       string
	LeftOut       bool
	Unread        bool
}

// resolvesToNone is the refusal of an issue whose repository Dispatch resolves to none: neither it
// nor an ancestor names one, and its project has no default.
const resolvesToNone = "its repository resolves to none"

// ResolveRepository is the repository an issue resolving to resolved is recorded on, or why it is
// not recorded. Admission records roots and orphans with it, as the engine records children.
func (e *Engine) ResolveRepository(resolved string) (Resolution, error) {
	if resolved == "" {
		return Resolution{Refusal: resolvesToNone}, nil
	}
	if e.cfg.Repositories == nil {
		return Resolution{}, errors.New("the workflow holds no repository mapping to record an issue's repository from")
	}
	return e.cfg.Repositories.ResolveRepository(resolved)
}

// SettleRetainedRepository is fact with a retained event's missing repository
// (intake.DispatchIssue.ResolvedRepoMissing) settled from the issue's record, recorded: an issue's
// repository is its record's, so the event is applied with it, keeping its own actor and sequence.
// A fact the consumer's preparation did not fill for an issue not yet recorded is refused, so the
// event is retried: nothing records an issue on a repository nobody read.
//
// This settles no repository change and corrects nothing, although the record's repository does
// follow Dispatch's (ApplyRepositoryChange): an event that carries no repository is no statement
// by Dispatch about which repository the issue is on, so there is nothing for the record to
// follow and the record stands.
func SettleRetainedRepository(fact intake.DispatchIssue, recorded *record.Issue) (intake.DispatchIssue, error) {
	if !fact.ResolvedRepoMissing {
		return fact, nil
	}
	if recorded == nil {
		return fact, fmt.Errorf("the event %d of %s predates Dispatch's resolved_repo and %s is not recorded: it is applied once its repository is read from Dispatch",
			fact.Seq, fact.Key, fact.Key)
	}
	fact.ResolvedRepo, fact.ResolvedRepoMissing = recorded.Repo, false
	return fact, nil
}

// setRepositoryHint is the end of every note that asks for an issue's repository.
const setRepositoryHint = "the issue header's Repository picker, or dispatch issue-update --repo owner/name"

// RefuseRecording says on the Dispatch issue key why Legion did not record it: its repository,
// resolved, resolves to none or to one Legion does not serve (refused). It says it once per
// resolution (record.RepoNote), so a repeated event of the issue says nothing again, and reports
// whether it said it now. A child's refusal is also told to its tree's architect (refuseChild). An
// Unread resolution says nothing, to the issue or to an architect: the daemon recovers the issue
// from Dispatch's listing once GitHub answers for its repository, with nothing for anyone to move,
// and it is noted for that recovery (Config.NoteUnread), so an issue whose resolution moves off
// the repository meanwhile (its parent's repository, or the project's default, changed) is still
// recovered.
func (e *Engine) RefuseRecording(ctx context.Context, tx pgx.Tx, key, resolved string, refused Resolution) (bool, error) {
	if refused.Unread {
		if note := e.cfg.NoteUnread; note != nil {
			note(key, resolved)
		}
		return false, nil
	}
	note := "Legion did not admit this issue: " + refused.Refusal + ". Set its repository (" + setRepositoryHint + ") and move it to todo again."
	if refused.LeftOut {
		note = "Legion did not admit this issue: " + refused.Refusal + ". Install it on " + resolved + ", or set another repository (" + setRepositoryHint + "), and move the issue to todo again."
	}
	return e.noteOnce(ctx, tx, key, resolved, note)
}

// ApplyRepositoryChange makes a recorded issue's repository follow Dispatch's: resolved is what
// Dispatch resolves the issue to now, and the repository is Dispatch's own fact, so the daemon
// moves the record onto it rather than keeping what it has. Both passes that read a recorded
// issue's resolution call it — a live Dispatch event (Engine.dispatchIssue) and the boot and
// recovery listing (admission's reconcile) — and the correction is one statement in the fact's own
// transaction: no park, no rerun and no new generation, so an issue keeps its phase, its
// generation and whatever is running for it while its repository moves.
//
// The record follows Dispatch as far as Legion can take it. A repository Legion serves no
// installation of is not written onto the record: the record keeps the repository it has, the
// issue is told once (noteRepositoryChangeWaits), and the change waits on the record
// (record.Store.NoteIssueRepositoryChange) for the pass that finds the repository served, which
// applies it. A resolution that names no repository at all is told the same way, and leaves no
// change waiting: there is no repository to change to, so the record's waiting pair is cleared.
// A resolution GitHub has not answered the installation read of (Resolution.Unread) is nothing
// Dispatch has said that the daemon can act on, so this writes and says nothing at
// all: the daemon takes the issue from Dispatch's listing once GitHub answers for the repository
// (its recovery), and reads the resolution again there.
func (e *Engine) ApplyRepositoryChange(ctx context.Context, tx pgx.Tx, issue record.Issue, resolved string) error {
	// The common pass: Dispatch names the repository the record already holds — the same name, or
	// the other name of a configured rename (sameRepository), so re-pointing every open issue at
	// the repository a transfer moved to corrects none of them. It writes only when a change the
	// record was waiting to make is gone, which is the record saying that nothing waits any longer.
	if e.sameRepository(resolved, issue.Repo) {
		if issue.RepoChangePending == "" {
			return nil
		}
		return e.store.NoteIssueRepositoryChange(ctx, tx, issue.Key, "", "")
	}
	resolution, err := e.ResolveRepository(resolved)
	if err != nil {
		return err
	}
	if resolution.Unread {
		return nil
	}
	if resolution.Refusal != "" {
		if err := e.noteRepositoryChangeWaits(ctx, tx, issue, resolved); err != nil {
			return err
		}
		// A change waits only where Dispatch has named a repository to change to. A resolution
		// that names none is no such repository, so the record's pair is cleared rather than
		// written: it is empty together, as record.Issue says (RepoChangePending). The issue is
		// told either way.
		pending, reason := resolved, resolution.Refusal
		if resolved == "" {
			pending, reason = "", ""
		}
		// The record already holds what this pass would write, as the arm above reads a change
		// that is gone: every later event of an issue waiting on the same unserved repository,
		// and every boot and recovery listing of it, would otherwise write the row it holds.
		if issue.RepoChangePending == pending && issue.RepoChangeReason == reason {
			return nil
		}
		return e.store.NoteIssueRepositoryChange(ctx, tx, issue.Key, pending, reason)
	}
	at := e.now()
	corrected, err := e.store.CorrectIssueRepository(ctx, tx, issue.Key, resolution.Repo, resolution.DefaultBranch, resolution.GitHubRepo, at)
	if err != nil || !corrected {
		return err
	}
	e.logOnCommit(ctx, "workflow: an issue's repository follows the one Dispatch resolves it to", "issue", issue.Key,
		"from", issue.Repo, "to", resolution.Repo, "at", at)
	return nil
}

// noteRepositoryChangeWaits says on a recorded issue that the repository Dispatch now resolves it
// to, resolved, is not written onto the record yet: Legion serves no installation of it, so the
// record keeps the repository it has, and Legion moves the issue itself once it serves that
// repository. ApplyRepositoryChange reaches it for a refused or left-out resolution alone, and it
// says it once per resolution (noteOnce), so a repeated event or listing of the issue says
// nothing again.
func (e *Engine) noteRepositoryChangeWaits(ctx context.Context, tx pgx.Tx, issue record.Issue, resolved string) error {
	var note string
	if resolved == "" {
		note = fmt.Sprintf("Dispatch resolves this issue to no repository, so it keeps %s. Legion moves the issue itself once Dispatch resolves it to a repository Legion serves, with nothing for anyone to do.", issue.Repo)
	} else {
		note = fmt.Sprintf("Legion serves no installation of %s, so this issue keeps %s as its repository. Legion moves the issue to %s itself once it serves that repository, with nothing for anyone to do.", resolved, issue.Repo, resolved)
	}
	_, err := e.noteOnce(ctx, tx, issue.Key, resolved, note)
	return err
}

// noteOnce posts note on the Dispatch issue key unless the issue's last repository note was the
// same note at the same resolution, and reports whether it posted.
func (e *Engine) noteOnce(ctx context.Context, tx pgx.Tx, key, resolved, note string) (bool, error) {
	last, err := e.store.RepoNote(ctx, tx, key)
	if err != nil {
		return false, err
	}
	if last != nil && last.ResolvedRepo == resolved && last.Note == note {
		return false, nil
	}
	if err := e.enqueue(ctx, tx, key, record.MessagePost{Body: note}); err != nil {
		return false, err
	}
	return true, e.store.PutRepoNote(ctx, tx, record.RepoNote{Issue: key, ResolvedRepo: resolved, Note: note, PostedAt: e.now()})
}

// refuseChild says on a child, and to its tree's architect, why the child was not recorded under
// the tree: its repository resolves to none or to one Legion does not serve.
func (e *Engine) refuseChild(ctx context.Context, tx pgx.Tx, root record.Issue, key, resolved string, refused Resolution) error {
	noted, err := e.RefuseRecording(ctx, tx, key, resolved, refused)
	if err != nil || !noted {
		return err
	}
	e.logOnCommit(ctx, "workflow: child not recorded; its repository is not one Legion serves", "issue", key, "tree", root.Key, "resolved", resolved, "reason", refused.Refusal)
	return e.notice(ctx, tx, root.Key, record.Notice{Kind: "child-not-recorded", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("%s was not recorded under your tree: %s; the daemon said so on the issue. Set its repository to one of the project's (dispatch issue-update --repo owner/name), then release it again.", key, refused.Refusal)})
}
