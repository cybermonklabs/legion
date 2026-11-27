package workflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
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
// it does not, and RefusalKind, what refused it (intake.RepositoryRefusalKind): GitHub's answer
// for a repository name, or a resolution that names no <owner>/<name>, none at all included,
// which only setting a repository answers. LeftOut is a refusal the fact's own preparation carries
// (intake.DispatchIssue.RepositoryRefusal, ResolveIssueRepository): a role App GitHub answered is
// not installed on the repository, as on one GitHub does not have, or a name that is no
// <owner>/<name>; Refusal names which.
// Unread is a repository GitHub has not been read for yet (the daemon's ResolveRepository), which
// is not refused but left: RefuseRecording says nothing on it, and the daemon takes it from
// Dispatch's listing once GitHub answers for the repository (its recovery).
type Resolution struct {
	Repo          string
	DefaultBranch string
	GitHubRepo    string
	Refusal       string
	RefusalKind   intake.RepositoryRefusalKind
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
		return Resolution{Refusal: resolvesToNone, RefusalKind: intake.RefusedName}, nil
	}
	if e.cfg.Repositories == nil {
		return Resolution{}, errors.New("the workflow holds no repository state to record an issue's repository from")
	}
	return e.cfg.Repositories.ResolveRepository(resolved)
}

// ResolveIssueRepository is the repository fact's issue is recorded on, or why it is not: the
// refusal fact's own preparation read from GitHub when it carries one
// (intake.DispatchIssue.RepositoryRefusal), and otherwise what the daemon holds in memory
// (ResolveRepository). A preparation carries its refusal on the fact because nothing of it is
// kept: an issue on a repository GitHub refused is read from GitHub again at each preparation, so
// a role App installed since is taken at the next one, with no wait and no restart. It is the one
// resolution that records an issue: admission's roots and orphans (putNewRoot) and the engine's
// children (recordChild). A recorded issue's record following Dispatch onto another repository
// reads the refusal the same way and nothing the daemon holds (correctionResolution).
func (e *Engine) ResolveIssueRepository(fact intake.DispatchIssue) (Resolution, error) {
	if fact.RepositoryRefusal != "" {
		return Resolution{Refusal: fact.RepositoryRefusal, RefusalKind: fact.RepositoryRefusalKind, LeftOut: true}, nil
	}
	return e.ResolveRepository(fact.ResolvedRepo)
}

// correctionResolution is the repository ApplyRepositoryChange moves a recorded issue onto, or why
// it does not: the refusal fact's own preparation carries (intake.DispatchIssue.RepositoryRefusal),
// a resolution that names no repository, or the read that preparation made of GitHub
// (intake.DispatchIssue.RepositoryRead), never one the daemon kept (ResolveRepository, which an
// admission reads). A correction writes a repository Dispatch moved an issue onto, often long
// after any read the daemon kept of it was made, so a kept read would move the record onto a
// repository an App was uninstalled from since, or at a default branch renamed since. A fact whose
// preparation made no such read — a boot listing's, which reads GitHub for no repository, an
// event's or a hand-over's whose preparation's reads GitHub did not answer, or an event prepared
// while its issue had no record or while its record held no repository — is Unread: nothing is
// written and nothing is said, and the daemon's follow reads GitHub again (its followRecorded):
// once a boot is over for the first, at the commit of the apply for the last
// (ApplyRepositoryChange asks it), and for the others until GitHub answers, at the times its
// requestFollowOf gives.
func (e *Engine) correctionResolution(fact intake.DispatchIssue) (Resolution, error) {
	if fact.RepositoryRefusal != "" || fact.ResolvedRepo == "" {
		return e.ResolveIssueRepository(fact)
	}
	read := fact.RepositoryRead
	if read == nil {
		return Resolution{Refusal: "GitHub was not read for " + fact.ResolvedRepo + " at this preparation", Unread: true}, nil
	}
	name := read.Name.String()
	return Resolution{Repo: name, DefaultBranch: read.DefaultBranch, GitHubRepo: name}, nil
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

// setRepositoryHint is the two ways every note that asks for an issue's repository names to set
// the issue's own.
const setRepositoryHint = "the issue header's Repository picker, or dispatch issue-update --repo owner/name"

// RefuseRecording says on the Dispatch issue key why Legion did not record it: its repository,
// resolved, resolves to none, or GitHub does not answer for it as one a role App reaches
// (refused). It says it once per resolution (record.RepoNote), so a repeated event of the issue
// says nothing again, and reports whether it said it now. A child's refusal is also told to the
// architect that owns its parent (refuseChild). An Unread resolution says nothing, to the issue
// or to an architect: the daemon recovers the issue from Dispatch's listing once GitHub answers for its
// repository, with nothing for anyone to move, and it is noted for that recovery
// (Config.NoteUnread), so an issue whose resolution moves off the repository meanwhile (its
// parent's repository, or the project's default, changed) is still recovered.
//
// The two notes are the two refusals that stay. An issue whose repository resolves to none is
// asked for one: its own, or the default repository of its project (Config.Project), in
// Dispatch, which resolves the issue to either. An issue whose repository a role App does not
// reach is told which App and that GitHub answers for the repository neither as installed nor as
// present, since one answer covers a repository that does not exist and one the App is not
// installed on, and nothing of that refusal is kept: installing the App, or correcting the name,
// admits the issue at its next preparation, which is why the note asks for either and for a move
// back to todo.
func (e *Engine) RefuseRecording(ctx context.Context, tx pgx.Tx, key, resolved string, refused Resolution) (bool, error) {
	if refused.Unread {
		if note := e.cfg.NoteUnread; note != nil {
			note(key, resolved)
		}
		return false, nil
	}
	note := "Legion did not admit this issue: " + refused.Refusal + ". Set its repository (" + setRepositoryHint + "), or the default repository of project " +
		e.cfg.Project + ", in Dispatch, and move it to todo again."
	if refused.LeftOut {
		note = "Legion did not admit this issue: " + refused.Refusal + ". GitHub answers for " + resolved +
			" neither as installed nor as present: install the App on it, or correct the repository (" + setRepositoryHint + "), and move the issue to todo again."
	}
	return e.noteOnce(ctx, tx, key, resolved, note)
}

// ApplyRepositoryChange makes a recorded issue's repository follow Dispatch's: fact is the issue
// as the pass read it, whose ResolvedRepo is what Dispatch resolves the issue to now, and the
// repository is Dispatch's own fact, so the daemon moves the record onto it rather than keeping
// what it has. Every pass that reads a recorded issue's resolution calls it, once per issue,
// inside the transaction that pass applies in: a live Dispatch event (Engine.dispatchIssue), the
// listing admission reconciles at boot (its applySummary; the recovery listing takes no recorded
// issue), admission's outbox hand-over (its consumeHandover), and the daemon's follow over
// Dispatch's listing (its followRecorded), which reads an issue again wherever a pass can leave
// its record on another repository than Dispatch resolves (its requestFollow and requestFollowOf
// say who asks for one; this asks for one below). The correction is one statement in that
// transaction (record.Store.CorrectIssueRepository, whose one caller this is): no move to the backlog, no rerun
// and no new generation, so an issue keeps its phase, its generation and whatever is running for
// it while its repository moves.
//
// A fact is applied only to the record it was prepared against. Its preparation stamps it with
// the repository the record held then (intake.DispatchIssue.PreparedAgainst), and while the record
// holds another — compared as every repository name is (sameRepository), so a configured rename's
// other name or a spelling in another case is no move — this writes nothing and says nothing. Two
// reads of Dispatch at one sequence cannot be ordered, and a correction moves no sequence
// (record.Store.CorrectIssueRepository writes none), so a fact prepared before another correction
// moved the record — a hand-over's read against a follow's at the same sequence, or a late event
// against a follow that read Dispatch after the event — would otherwise put the record back on an
// older resolution, and nothing would read the issue again before its next event or the next
// boot. The issue is owed a follow instead, which reads Dispatch and GitHub fresh
// (Config.FollowIssue): it is asked from this transaction's commit hook, so a skip that commits is
// never lost and one rolled back asks nothing, and Config.FollowIssue says what covers a process
// that stops in between. A follow pass's own rows are checked here alike, each prepared against
// the record the pass read before its write.
//
// A fact no preparation stamped against a record's repository (PreparedAgainst empty) was
// prepared while the issue had no record — an event whose issue a listing recorded between the
// event's preparation and this apply — or while its record held no repository
// (record.Issue.Unfilled) — an event whose record the refresher's backfill filled between the
// event's preparation and this apply (the daemon's backfillLater) — or by no preparation at all,
// the decode's own. None of them reads GitHub for a correction
// (intake.DispatchIssue.RepositoryRead) or asks a follow, so where its resolution moves the record
// this writes nothing (Resolution.Unread) and owes the issue a follow from this transaction's
// commit hook, as above: without it the record would keep the repository that listing or that
// backfill wrote until the issue's next event or the next boot. A stamped fact that made no read
// asks no follow here, since the preparation that stamped it asked one already where one is owed
// (the daemon's prepareRecorded and prepareHandover).
//
// The repository is resolved from the fact alone (correctionResolution): the refusal the fact's
// preparation read from GitHub — a role App not installed on it, a repository GitHub does not
// have, a name that is no <owner>/<name> — is the refusal the change waits for, and the record is
// moved onto the name and default branch GitHub answered the fact's own preparation
// (intake.DispatchIssue.RepositoryRead), never at a read the daemon kept from an earlier one; a
// fact whose preparation made no such read writes nothing. A record a correction moves onto a
// repository has GitHub intake hear it and the runtime keep a repository cache of it from the
// correction's commit (Config.HearRepository), as a record that first names it at its recording
// does: without it the issue's own GitHub events on the repository it works now would reach no
// one until the next read of the records.
//
// The record follows Dispatch as far as Legion can take it. A repository Legion cannot work is
// not written onto the record: the record keeps the repository it has, the issue is told once
// why and what a person can do (noteRepositoryChangeWaits), and the change waits on the record
// (record.Store.NoteIssueRepositoryChange) for the issue's next event or the next boot, whose
// preparation reads GitHub again and applies it. Before dispatch://INFRA-4401 (#4493) a mapped
// repository waited for less: the read of Dispatch's repository mapping read both Apps'
// installations again and asked for a pass over every recorded issue once it served the
// repository. That read is gone, and nothing reads a repository no record names again, so every
// waiting change now waits for the issue's next event or the next boot. A resolution that names
// no repository at all is told the same way, and leaves no change waiting: there is no
// repository to change to, so the record's waiting pair is cleared. A resolution GitHub has not
// answered the reads of (Resolution.Unread) is nothing Dispatch has said that the daemon can act
// on, so this writes and says nothing at all, and the daemon's follow reads the repository again
// until GitHub answers.
//
// A record that holds no repository (record.Issue.Unfilled) is the backfill's to fill, and this
// writes nothing on it and says nothing: a correction would fill it without the checks the
// backfill makes, and the stamp of the record a fact was prepared against could fence nothing,
// since a preparation against such a record stamps it empty (PreparedAgainst). The preparations
// read GitHub for no such record (the daemon's correctionMoves). A change this leaves unwritten
// is not lost: an event applied to such a record cannot commit, since the observation it writes
// refuses a record with no repository (record.Store.PutIssue), and it is prepared again; and a
// fact prepared against such a record and applied once the backfill has filled it is a fact no
// preparation stamped, which this follows as the paragraph on such facts says (the daemon's
// backfillLater).
func (e *Engine) ApplyRepositoryChange(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) error {
	if issue.Unfilled() {
		return nil
	}
	resolved := fact.ResolvedRepo
	// The record moved after the fact was prepared: it takes nothing from the fact, and a follow
	// reads the issue fresh once this transaction commits (the doc above says why).
	if fact.PreparedAgainst != "" && !e.sameRepository(fact.PreparedAgainst, issue.Repo) {
		key := issue.Key
		e.followOnCommit(ctx, key)
		e.logOnCommit(ctx, "workflow: a repository correction prepared before the record moved writes nothing; the issue is followed again",
			"issue", key, "prepared", fact.PreparedAgainst, "record", issue.Repo, "resolved", resolved)
		return nil
	}
	// The common pass: Dispatch names the repository the record already holds — the same name, or
	// the other name of a configured rename (sameRepository), so re-pointing every open issue at
	// the repository a transfer moved to corrects none of them. It writes only when a change the
	// record was waiting to make is gone, which is the record saying that nothing waits any longer.
	if e.sameRepository(resolved, issue.Repo) {
		if issue.RepoChange == nil {
			return nil
		}
		return e.store.NoteIssueRepositoryChange(ctx, tx, issue.Key, nil)
	}
	resolution, err := e.correctionResolution(fact)
	if err != nil {
		return err
	}
	if resolution.Unread {
		// A fact no preparation stamped against a record's repository made no read for this
		// correction, and no preparation asked a follow for it: a follow reads the issue fresh once
		// this transaction commits (the doc above says why).
		if fact.PreparedAgainst == "" {
			e.followOnCommit(ctx, issue.Key)
			e.logOnCommit(ctx, "workflow: a repository change prepared while the issue had no record, or a record with no repository, carries no read of GitHub; the issue is followed",
				"issue", issue.Key, "record", issue.Repo, "resolved", resolved)
		}
		return nil
	}
	if resolution.Refusal != "" {
		if err := e.noteRepositoryChangeWaits(ctx, tx, issue, resolved, resolution); err != nil {
			return err
		}
		// A change waits only where Dispatch has named a repository to change to. A resolution
		// that names none is no such repository, so the record's change is cleared rather than
		// written (record.Issue.RepoChange). The issue is told either way.
		var change *record.RepositoryChange
		if resolved != "" {
			change = &record.RepositoryChange{Pending: resolved, Reason: resolution.Refusal}
		}
		// The record already holds what this pass would write, as the arm above reads a change
		// that is gone: every later event of an issue waiting on the same unserved repository,
		// and every boot and recovery listing of it, would otherwise write the row it holds.
		if held := issue.RepoChange; (held == nil && change == nil) || (held != nil && change != nil && *held == *change) {
			return nil
		}
		return e.store.NoteIssueRepositoryChange(ctx, tx, issue.Key, change)
	}
	at := e.now()
	corrected, err := e.store.CorrectIssueRepository(ctx, tx, issue.Key, resolution.Repo, resolution.DefaultBranch, resolution.GitHubRepo, at)
	if err != nil || !corrected {
		return err
	}
	e.HearRepository(ctx, resolution.GitHubRepo)
	e.logOnCommit(ctx, "workflow: an issue's repository follows the one Dispatch resolves it to", "issue", issue.Key,
		"from", issue.Repo, "to", resolution.Repo, "at", at)
	return nil
}

// followOnCommit asks for a follow of the recorded issue key (Config.FollowIssue) from the commit
// hook of the transaction ApplyRepositoryChange writes in, so a skip that commits is never lost and
// one rolled back asks nothing.
func (e *Engine) followOnCommit(ctx context.Context, key string) {
	if follow := e.cfg.FollowIssue; follow != nil {
		store.OnCommit(ctx, func() { follow(key) })
	}
}

// noteRepositoryChangeWaits says on a recorded issue that the repository Dispatch now resolves it
// to, resolved, is not written onto the record: the record keeps the repository it has, for the
// reason refused gives. It says what a person can do, as RefuseRecording does for an issue it does
// not record: set a repository where the resolution names none or a name that is no
// <owner>/<name> (refused.RefusalKind, intake.RefusedName, which the preparation's parse refusal
// carries), since no App can be installed on either, and otherwise install the App refused names
// on it or set another repository: a named repository is refused for what GitHub answers of it
// alone, a role App not installed on it or no such repository. It ends with when Legion moves the
// issue itself: at its next event or at Legion's next boot, the two passes that read GitHub again
// for it, once both Legion Apps are installed on the repository, or, for a resolution that names
// no repository or no <owner>/<name>, once Dispatch resolves the issue to one they are. Since
// dispatch://INFRA-4401 (#4493) removed the read of Dispatch's repository mapping, that is when
// every waiting change is applied, a mapped repository's too. ApplyRepositoryChange reaches it
// for a refused or left-out resolution alone, and it says it once per resolution (noteOnce), so a
// repeated event or listing of the issue says nothing again.
func (e *Engine) noteRepositoryChangeWaits(ctx context.Context, tx pgx.Tx, issue record.Issue, resolved string, refused Resolution) error {
	action := "Set its repository (" + setRepositoryHint + ")."
	moves := " Legion moves the issue at its next event or at Legion's next boot, once Dispatch resolves it to a repository both Legion Apps are installed on."
	if refused.RefusalKind != intake.RefusedName {
		action = "Install it on " + resolved + ", or set another repository (" + setRepositoryHint + ")."
		moves = " Legion moves the issue at its next event or at Legion's next boot, once both Legion Apps are installed on " + resolved + "."
	}
	note := "Legion did not move this issue off " + issue.Repo + ": " + refused.Refusal + ". " + action + moves
	_, err := e.noteOnce(ctx, tx, issue.Key, resolved, note)
	return err
}

// noteOnce posts note on the Dispatch issue key unless the issue's last repository note was the
// same note at the same resolution, and reports whether it posted. The resolutions are compared as
// every repository name is (sameRepository): the daemon's one-time rename pass writes a configured
// pair's `to` over the `from` a note was stored under, while Dispatch still resolves the issue to
// the `from`, and the note it said once is not said again.
func (e *Engine) noteOnce(ctx context.Context, tx pgx.Tx, key, resolved, note string) (bool, error) {
	last, err := e.store.RepoNote(ctx, tx, key)
	if err != nil {
		return false, err
	}
	if last != nil && e.sameRepository(last.ResolvedRepo, resolved) && last.Note == note {
		return false, nil
	}
	if err := e.enqueue(ctx, tx, key, record.MessagePost{Body: note}); err != nil {
		return false, err
	}
	return true, e.store.PutRepoNote(ctx, tx, record.RepoNote{Issue: key, ResolvedRepo: resolved, Note: note, PostedAt: e.now()})
}

// refuseChild says on a child, and to the architect that owns its Dispatch parent, why the child
// was not recorded under root's tree: its repository resolves to none, or GitHub does not answer
// for it as one a role App reaches. The architect is told what the issue is told, since no list of
// the project's repositories is there to choose from: any repository both role Apps are
// installed on records an issue, so what is wrong is the name or the installation. The notice is
// queued on that parent when the record holds it in root's tree, so the owner walk takes it to the
// parent's own architect while that claim holds its role, the one that releases the parent's
// children, and on up the tree once it does not; a parent the record does not hold in the tree has
// no architect of it, and the notice goes on root, the tree's architect.
func (e *Engine) refuseChild(ctx context.Context, tx pgx.Tx, root record.Issue, key, parent, resolved string, refused Resolution) error {
	noted, err := e.RefuseRecording(ctx, tx, key, resolved, refused)
	if err != nil || !noted {
		return err
	}
	on := root.Key
	if parent != "" {
		recorded, err := e.store.Issue(ctx, tx, parent)
		if err != nil {
			return err
		}
		if recorded != nil && recorded.Tree == root.Key {
			on = recorded.Key
		}
	}
	e.logOnCommit(ctx, "workflow: child not recorded; GitHub does not answer for its repository as one Legion's Apps reach", "issue", key, "tree", root.Key, "resolved", resolved, "reason", refused.Refusal)
	return e.notice(ctx, tx, on, record.Notice{Kind: "child-not-recorded", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("%s was not recorded under your tree: %s; the daemon said so on the issue. Set a repository both Legion Apps are installed on, spelled as GitHub spells it (dispatch issue-update --repo owner/name), then release it again.", key, refused.Refusal)})
}
