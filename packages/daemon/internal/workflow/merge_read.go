package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// RequestInstructionLabel is in each text that tells an owner that a request for new work started
// the change it owns (D17): the root architect's start (ArchitectStartTask), the next-change notice
// and the start of a child's own architect. A reader tells the request's instruction by it.
const RequestInstructionLabel = "a request for new work started this change"

// requestInstruction is what the owner of a change a request started is told (D17, "The
// instruction"), in each of its three places. Each sentence holds at most 25 words and no
// semicolon. No program holds the change's first phase for it: the instruction is the whole
// mechanism, which a measurement of c-reopened-count may later show to need more.
const requestInstruction = "This issue's work merged, and " + RequestInstructionLabel + ". " +
	"Read what the request asks: the comment that begins `Reopened after an early close:`, or else the reason that whoever asked wrote on the issue. " +
	"Before this change ends, write that work as checks of the Verification document that run after the merge. " +
	"A request of the controller or of an architect ends with no pull request when you withdraw it with evidence that the work is done. " +
	"A request of a person ends with no pull request only when that person sets the issue to `done`, so ask them. " +
	"This change's first phase does not wait for that edit."

// Request is one request for new work on an issue (D17): who asked (record.RequesterPerson,
// claim.RoleController or claim.RoleArchitect), the text its request row keeps, and the person
// whose event or grant asked, empty where no person's did. An unattributed request counts as a
// person's for who asked, and confirms nothing: only Person names a person.
type Request struct {
	Requester claim.Role
	Text      string
	Person    string
}

// RequestOf is the request fact makes, a move of its issue to todo: who asked is the route's own
// (intake.DispatchIssue.Requester) for a route's own write, and a person's for every other event,
// a boot listing's among them. Its text names the event that made it, and who wrote it. The daemon
// never reads the comment a controller writes beside its request: the owner reads it.
func RequestOf(fact intake.DispatchIssue) Request {
	requester := fact.Requester
	if requester == "" {
		requester = record.RequesterPerson
	}
	by := string(requester)
	switch {
	case fact.PersonActor != "":
		by += " " + fact.PersonActor
	case fact.ActorSession != "":
		by += " through session " + fact.ActorSession
	}
	event := fact.Type
	if event == "" {
		event = "the listing"
	}
	return Request{Requester: requester, Person: fact.PersonActor,
		Text: fmt.Sprintf("%s set %s to %s (%s, Dispatch sequence %d)", by, fact.Key, fact.Status, event, fact.Seq)}
}

// RecordedRequest is the request an issue's own record shows and no event of this transaction
// brings: a stranded child's accepted todo at its root's promotion, or a merge read's apply where
// the row it rereads is gone. It names no actor, so it counts as a person's (D17).
func RecordedRequest(issue record.Issue) Request {
	return Request{Requester: record.RequesterPerson,
		Text: fmt.Sprintf("the record shows %s at todo since Dispatch sequence %d", issue.Key, issue.LastDispatchSeq)}
}

// RequestRoute is what a door does with a request for new work on an issue (RouteRequest).
type RequestRoute int

const (
	// RequestRunsToday is today's path: the issue is no closed issue a request reaches, a person
	// confirmed that its work never merged, or it was parked and never closed.
	RequestRunsToday RequestRoute = iota
	// RequestStartsChange is one next change of the issue (StartNextChange), its merge known.
	RequestStartsChange
	// RequestWaits is a request that waits for its merge read: nothing starts and nothing clears.
	RequestWaits
)

// RouteRequest is the record's decision on a request for new work on issue, made under the fact
// lock before any clear (D17, "The merge, read first from the record"). An issue in verifying has
// its merge on its row, and starts one next change. An issue at done reads its merge history
// (record.Store.MergeHistoryOf): positive evidence (ANSWERED) saves the merge it knows and starts
// one next change, and never reads candidates, but where that merge lacks its base or its final
// head one read of its pull request gives them first (record.MergeReadStageMetadata). Without
// positive evidence (PARTIAL, NONE) a person's confirmation that the work never merged is taken
// (ConsumeNeverMergedConfirmation) and the request runs as today. Otherwise a closed issue's
// request waits for the merge read, which the door queues here, while one that only a park left
// at done runs as today: a closed issue whose work never merged is what a missing merge cannot
// prove (closedHistory). Every other issue runs as today.
//
// It decides alone, so every door asks one rule: the doors place the issue, and StartNextChange's
// one guard decides whether a change may start.
func (e *Engine) RouteRequest(ctx context.Context, tx pgx.Tx, issue record.Issue, req Request) (RequestRoute, error) {
	switch issue.Phase {
	case phase.Verifying:
		return RequestStartsChange, nil
	case phase.Done:
	default:
		return RequestRunsToday, nil
	}
	pending, err := e.store.PendingMergeRead(ctx, tx, issue.Key)
	if err != nil {
		return RequestRunsToday, err
	}
	history, err := e.store.MergeHistoryOf(ctx, tx, issue.Key)
	if err != nil {
		return RequestRunsToday, err
	}
	if history.Class == record.MergeHistoryAnswered {
		if history.MissingMetadata {
			return RequestWaits, e.waitForRead(ctx, tx, issue, req, pending)
		}
		if !history.Evidence.Snapshot {
			if _, err := e.store.PutMergeSnapshot(ctx, tx, issue.Key, history.Evidence.Merge); err != nil {
				return RequestRunsToday, err
			}
		}
		if pending != nil {
			if _, err := e.store.CancelMergeRead(ctx, tx, issue.Key); err != nil {
				return RequestRunsToday, err
			}
		}
		return RequestStartsChange, nil
	}
	confirmed, err := e.store.ConsumeNeverMergedConfirmation(ctx, tx, issue.Key)
	if err != nil {
		return RequestRunsToday, err
	}
	if confirmed || !closedHistory(issue, pending) {
		if pending != nil {
			if _, err := e.store.CancelMergeRead(ctx, tx, issue.Key); err != nil {
				return RequestRunsToday, err
			}
		}
		e.logOnCommit(ctx, "workflow: a request on work that never merged runs as today", "issue", issue.Key, "class", history.Class,
			"confirmed", confirmed)
		return RequestRunsToday, nil
	}
	return RequestWaits, e.waitForRead(ctx, tx, issue, req, pending)
}

// closedHistory says whether issue, at done, closed: its record's own status is done, or its
// request already waits for a merge read (pending), which only a closed issue's request queues.
// A park leaves an issue at done with the status it was parked at, and a non-person's move of a
// closed issue out of the workflow keeps the status done (closedHistoryMove), so a closed issue
// stays closed for every request but a person's confirmed one.
func closedHistory(issue record.Issue, pending *record.PendingMergeRead) bool {
	return pending != nil || issue.Status == record.DoneStatus
}

// waitForRead queues the merge read of a request that waits for it (record.MergeRead), where none
// stands: one row stands for an issue at a time, and a later request reaches the standing row
// alone (pendingRequestEvent).
func (e *Engine) waitForRead(ctx context.Context, tx pgx.Tx, issue record.Issue, req Request, pending *record.PendingMergeRead) error {
	if pending != nil {
		return nil
	}
	queued, err := e.store.QueueMergeRead(ctx, tx, record.MergeRead{Issue: issue.Key, Generation: issue.Generation,
		Requester: req.Requester, Request: req.Text}, e.now())
	if err != nil {
		return err
	}
	if queued {
		e.logOnCommit(ctx, "workflow: a request on a closed issue waits for the read of its merge", "issue", issue.Key,
			"generation", issue.Generation, "requester", req.Requester)
	}
	return nil
}

// requestObserved is issue with fact's observation on it (observedIssue) and the record's status
// todo: the request taken, which a change then starts from, or which waits for its merge read
// with the status the read's apply keys on.
func (e *Engine) requestObserved(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) (record.Issue, error) {
	observed, err := e.observedIssue(ctx, tx, issue, fact)
	if err != nil {
		return issue, err
	}
	observed.Status = "todo"
	return observed, nil
}

// requestDoor is the engine's door of a request for new work (D17): a todo on an issue in
// verifying, and on a child at done of a tree that is live. It decides by RouteRequest and places
// the issue where it stands: a change starts in place, a request that waits records the
// observation with the status todo, and today's path is the caller's (ReenterChild). A root in
// verifying whose tree lingers, and a child of a tree that is not live, are admission's to place,
// so it answers false for them.
func (e *Engine) requestDoor(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) (bool, error) {
	root := claim.IsTreeRoot(issue.Key, issue.Tree)
	switch {
	case issue.Phase == phase.Verifying && root && issue.Lingers():
		return false, nil
	case issue.Phase == phase.Done && root:
		return false, nil
	case issue.Phase == phase.Done:
		tree, err := e.store.Issue(ctx, tx, issue.Tree)
		if err != nil || tree == nil {
			return false, err
		}
		live, err := record.TreeLive(ctx, e.store, tx, *tree, e.cfg.Steps)
		if err != nil || !live {
			return false, err
		}
	case issue.Phase != phase.Verifying:
		return false, nil
	}
	req := RequestOf(fact)
	route, err := e.RouteRequest(ctx, tx, issue, req)
	if err != nil || route == RequestRunsToday {
		return false, err
	}
	observed, err := e.requestObserved(ctx, tx, issue, fact)
	if err != nil {
		return false, err
	}
	if err := e.WriteIssue(ctx, tx, observed); err != nil {
		return false, err
	}
	if route == RequestWaits {
		return true, nil
	}
	return true, e.StartRequestedChange(ctx, tx, observed.Key, req, ChangeInPlace(observed))
}

// StartRequestedChange starts the next change of key that request asked for at place, from the
// record as it stands now, its merge snapshot saved: the one call every door makes once the
// record's decision (RouteRequest) or a merge read's answer says the work merged.
func (e *Engine) StartRequestedChange(ctx context.Context, tx pgx.Tx, key string, request Request, place ChangePlace) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return err
	}
	place.Request = &request
	return e.StartNextChange(ctx, tx, *issue, nil, place)
}

// pendingRequestEvent is a newer Dispatch event of an issue whose request waits for its merge
// read (record.Store.PendingMergeRead). Any such event asks one read of the issue's links, since a
// person ends the wait by linking the merged pull request. A later request by a person, a move to
// todo the route made for them or a status move of theirs, makes the pending request the
// person's: its asker and its text replace the row's. A later request of a session, the
// controller's or an architect's, changes neither, nor does an edit, an echo or the listing.
func (e *Engine) pendingRequestEvent(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) error {
	refreshed, err := e.store.RefreshMergeRead(ctx, tx, issue.Key, e.now())
	if err != nil || !refreshed {
		return err
	}
	moved := fact.StatusMove || fact.Status != issue.DispatchStatus
	if fact.Status != "todo" || !moved || fact.PersonActor == "" {
		return nil
	}
	req := RequestOf(fact)
	taken, err := e.store.RequestMergeRead(ctx, tx, issue.Key, req.Requester, req.Text)
	if err != nil || !taken {
		return err
	}
	e.logOnCommit(ctx, "workflow: a person's later request takes over the request that waits for its merge read", "issue", issue.Key,
		"person", fact.PersonActor)
	return nil
}

// closedHistoryMove takes a move of issue, at done, to backlog, icebox, triage or done (D17, "A
// person's confirmation is separate from merge evidence"). The move ends a request that waits for
// its merge read. Where the issue closed (closedHistory) and its record holds no proof of its
// merge, only a person's backlog moves it as today, and it records that person's confirmation
// that the work never merged (record.Store.ConfirmNeverMerged): the next todo takes today's rerun.
// Any other move of it, by a controller, an architect, a session, the boot listing or an event of
// no actor, keeps the record's own status done with the observation, so the next request is again
// a closed issue's, and answers true: nothing leaves the workflow again. A done move, and every
// move of an issue whose merge is known or that never closed, goes on as today.
func (e *Engine) closedHistoryMove(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) (bool, error) {
	pending, err := e.store.PendingMergeRead(ctx, tx, issue.Key)
	if err != nil {
		return false, err
	}
	if pending != nil {
		if _, err := e.store.CancelMergeRead(ctx, tx, issue.Key); err != nil {
			return false, err
		}
		e.logOnCommit(ctx, "workflow: a move out of todo ends the request that waited for its merge read", "issue", issue.Key,
			"status", fact.Status, "requester", pending.Read.Requester)
	}
	if !closedHistory(issue, pending) || fact.Status == record.DoneStatus {
		return false, nil
	}
	history, err := e.store.MergeHistoryOf(ctx, tx, issue.Key)
	if err != nil || history.Class == record.MergeHistoryAnswered {
		return false, err
	}
	if fact.PersonActor != "" && fact.Status == "backlog" {
		e.logOnCommit(ctx, "workflow: a person confirms that the work of a closed issue never merged", "issue", issue.Key,
			"person", fact.PersonActor, "class", history.Class)
		return false, e.store.ConfirmNeverMerged(ctx, tx, issue.Key)
	}
	kept, err := e.observedIssue(ctx, tx, issue, fact)
	if err != nil {
		return false, err
	}
	kept.Status = record.DoneStatus
	e.logOnCommit(ctx, "workflow: the record keeps a closed issue closed: only a person's backlog confirms that its work never merged",
		"issue", issue.Key, "status", fact.Status, "class", history.Class)
	return true, e.WriteIssue(ctx, tx, kept)
}

// MergeReadRequest is the standing request a merge read's fact answers (intake.MergeRead), read
// again under the fact lock: the issue as its record holds it and who asked and what, from the
// standing merge_read row as it stands now, never from the executor's copy (D17, "The merge read",
// step 4). It answers false where the fact applies to nothing: the record holds another generation,
// the issue is no longer at done with the status todo, or no row of the fact's generation stands.
func (e *Engine) MergeReadRequest(ctx context.Context, tx pgx.Tx, fact intake.MergeRead) (record.Issue, Request, bool, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || issue.Generation != fact.Generation || issue.Phase != phase.Done || issue.Status != "todo" {
		return record.Issue{}, Request{}, false, err
	}
	pending, err := e.store.PendingMergeRead(ctx, tx, fact.Issue)
	if err != nil || pending == nil || pending.Read.Generation != fact.Generation {
		return record.Issue{}, Request{}, false, err
	}
	return *issue, Request{Requester: pending.Read.Requester, Text: pending.Read.Request}, true, nil
}

// mergeRead applies the merge read's answer (intake.MergeRead) under the fact lock, where its
// request still stands (MergeReadRequest): it saves the merge through the snapshot's one writer
// (record.Store.PutMergeSnapshot), which keeps a later merge that stands already, and places the
// issue. The engine places a child of a live tree, where its change starts in place with the
// request as the row stands now, and the row ends. Admission, which runs after this handler,
// places a root, an orphan and a child of a tree that does not run, from the same row.
func (e *Engine) mergeRead(ctx context.Context, tx pgx.Tx, fact intake.MergeRead) (intake.Result, error) {
	issue, req, standing, err := e.MergeReadRequest(ctx, tx, fact)
	if err != nil || !standing {
		return intake.Result{}, err
	}
	if _, err := e.store.PutMergeSnapshot(ctx, tx, issue.Key, fact.Merge); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: the merge read found the merge of a closed issue", "issue", issue.Key, "number", fact.Merge.Number,
		"requester", req.Requester)
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		return intake.Result{}, nil
	}
	tree, err := e.store.Issue(ctx, tx, issue.Tree)
	if err != nil || tree == nil {
		return intake.Result{}, err
	}
	live, err := record.TreeLive(ctx, e.store, tx, *tree, e.cfg.Steps)
	if err != nil || !live {
		return intake.Result{}, err
	}
	if _, err := e.store.CancelMergeRead(ctx, tx, issue.Key); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.StartRequestedChange(ctx, tx, issue.Key, req, ChangeInPlace(issue))
}

// mergeUnread applies the merge read's report that it found no candidate that answers
// (intake.MergeUnread): while the request still waits on the row of that generation, and its
// controller was never told of it, the controller is told now (tellMergeUnread). The daily turn
// tells it again once each UTC day after (tellEachMergeUnread).
func (e *Engine) mergeUnread(ctx context.Context, tx pgx.Tx, fact intake.MergeUnread) (intake.Result, error) {
	pending, err := e.store.PendingMergeRead(ctx, tx, fact.Issue)
	if err != nil || pending == nil || pending.Read.Generation != fact.Generation || !pending.Read.ToldAt.IsZero() {
		return intake.Result{}, err
	}
	return intake.Result{}, e.tellMergeUnread(ctx, tx, *pending, fact.Cause)
}

// tellEachMergeUnread is the daily turn of merge-unread: at the first controller tick of each UTC
// day, the controller is told once of each request whose merge read ended unread or stopped and
// that it was not told of yet that day. It sends no read and queues none: the row's progress and
// its due time stay as the read left them.
func (e *Engine) tellEachMergeUnread(ctx context.Context, tx pgx.Tx) error {
	reads, err := e.store.UnreadMergeReads(ctx, tx, e.cfg.Project)
	if err != nil {
		return err
	}
	today := e.now().UTC().Format(time.DateOnly)
	for _, pending := range reads {
		if !pending.Read.ToldAt.IsZero() && pending.Read.ToldAt.UTC().Format(time.DateOnly) == today {
			continue
		}
		cause := MergeUnreadCause(pending.Read.Progress)
		if err := e.tellMergeUnread(ctx, tx, pending, cause); err != nil {
			return err
		}
	}
	return nil
}

// tellMergeUnread tells the project's controller (merge-unread) that Legion cannot tell whether the
// work of pending's issue merged, and why, and marks the row told now. A person ends the wait: a
// link of the merged pull request on the issue, or a backlog and then a todo that confirm the work
// never merged. Each sentence holds at most 25 words and no semicolon.
func (e *Engine) tellMergeUnread(ctx context.Context, tx pgx.Tx, pending record.PendingMergeRead, cause string) error {
	reason := fmt.Sprintf("Legion cannot tell whether the work of %s merged, so its request starts nothing. ", pending.Read.Issue) +
		fmt.Sprintf("The read waits on this: %s. ", cause) +
		"Ask a person to link the merged pull request on the issue. " +
		"Or ask a person to set the issue to backlog, then todo, which confirms the work never merged."
	if err := e.enqueue(ctx, tx, pending.Read.Issue, record.ControllerNotice{Kind: record.MergeUnreadNotice, Reason: reason}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the controller is told of a request whose merge Legion cannot tell", "issue", pending.Read.Issue,
		"generation", pending.Read.Generation)
	return e.store.MarkMergeUnreadTold(ctx, tx, pending.ID, e.now())
}

// MergeUnreadCause is what a merge read that ended with no answer waits on, as merge-unread names
// it: the stage that failed three times and its last failure, or a search that found no pull
// request naming the issue with a merge. The outbox's executor names its MergeUnread fact's cause
// by it too, so the notice at once and the daily one say the same.
func MergeUnreadCause(progress record.MergeReadProgress) string {
	if progress.State == record.MergeReadStateStopped {
		return "its " + progress.Stage + " stage failed three times, the last with " + progress.Cause
	}
	return "no pull request it reads names the issue with a merge"
}
