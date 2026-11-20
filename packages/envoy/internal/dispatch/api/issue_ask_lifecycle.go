package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// withdrawnAsk is the SQL predicate, on asks a, that holds for an ask a close withdrew
// (model.AskResolutionWithdrawn).
const withdrawnAsk = `a.state = 'resolved' and a.resolution->>'kind' = '` + model.AskResolutionWithdrawn + `'`

// withdrawnReason is the reason recorded on an ask the close of issueKey withdrew.
func withdrawnReason(issueKey string) string {
	return fmt.Sprintf("Withdrawn by the close of %s.", issueKey)
}

// settleIssueBeforeClose runs, before a close of issueKey opens its transaction, the settlement
// each document of the issue owes, so every ask block written into them is an ask, credited to its
// author, for the close to withdraw with a record. The reason is the asks: a block credited after
// the withdrawal would be an open ask on a closed issue that nothing withdraws, so a settlement
// after the close credits none. It saves such a block resolved with no ask, and the block becomes
// an ask once the issue reopens. It holds no transaction or lock, so it cannot meet the close's. A
// settlement that stops short is run again a few times within a short budget
// (docs.SettleIssueDocuments): one that met updates the room had not stored yet, one a write
// superseded (another process's append among them, which the settlement catches its room up with
// first), and one whose room held a stored history that was replaced under it, which is reloaded
// at the head, each settle on a later attempt. A settlement still short after those is logged and
// the close proceeds; its blocks are not asks yet, and the close's sweep marks them resolved
// (sweepClosedIssueAsks). For an issue already closed, or none, this settles nothing first: its
// close is refused, and the resumption settles what a closed issue's documents owe
// (docs.Service.RunSettlementResumption).
//
// ctx must not end when the client goes away (outlivingTheClient): a settlement runs to its end
// whether or not the client waits for the close, as the settlement it stands in for would. On a
// keyed request it still ends at the keyed run's bound (keyedRunLimit). issueSettleBudget
// (docs.SettleIssueDocuments) bounds only the waits for the rooms' durable appends and whether a
// retry starts, not a settlement already running.
func (s *server) settleIssueBeforeClose(ctx context.Context, issueKey string) {
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return
	}
	if _, open, err := issueStatus(ctx, issueKey, pooledOwnerRead(s.deps.Store.Pool)); err != nil || !open {
		return
	}
	if err := s.deps.Docs.SettleIssueDocuments(ctx, issueKey); err != nil {
		slog.Warn("dispatch: close an issue whose documents did not all settle first", "issue", issueKey, "error", err)
	}
}

// The close withdraws every open ask of its issue and the reopen restores them, in two halves
// each, so their document work runs in the transaction's read phase, before its first write
// (INFRA-636). The first half (prepareWithdrawal, prepareRestoration) reads the asks and writes
// their blocks and copies into the issue's documents (docs.WriteIssueAskStates), whose writes queue
// on ctx's ledger; the second (recordWithdrawnAsksTx, recordRestoredAsksTx) queues each ask's row
// write and event after them, so in the write phase every document write comes before the first
// ask event, as the close and the reopen order them. Each event's thread_logins is read by its own
// insert, after its ask's write (events.ThreadLoginsOf). The reopen's second half also does its
// follow of the versions a settlement wrote while the issue was closed, whose reads and renders
// are document work, so a reopen runs both halves in its read phase, one after the other.
//
// ctx is joined to tx (Docs.Join) and ledger is its ledger; the caller holds the issue's owner
// lock and has locked every event owner it appends to (LockOwners) before calling, and commits
// through the ledger and publishes the returned events once it has.

// prepareWithdrawal is the close's first half: it reads every open ask of issueKey and writes the
// blocks showing them in the issue's documents, and their copies, resolved, while the rows still
// read open (docs.WriteIssueAskStates classifies copies by the rows as they stand). Unindexed
// blocks are the close's final sweep's to mark (sweepClosedIssueAsks). It answers the open asks,
// oldest id first, for recordWithdrawnAsksTx.
func (s *server) prepareWithdrawal(ctx context.Context, tx pgx.Tx, issueKey string, actor model.Actor) ([]model.Ask, error) {
	open, shown, err := s.readOpenIssueAsks(ctx, tx, issueKey)
	if err != nil {
		return nil, err
	}
	if _, err := s.deps.Docs.WriteIssueAskStates(ctx, issueKey, shown, actor, false); err != nil {
		return nil, err
	}
	return open, nil
}

// readOpenIssueAsks reads every open ask of issueKey, oldest id first, in tx, which the caller holds
// the issue's owner lock in, and answers them beside the same asks as their blocks show them once
// withdrawn: resolved. WriteIssueAskStates writes nothing from a resolution. The rows are read
// plainly: every writer of an issue's asks takes the issue's owner lock first (store.LockOwner),
// so none moves them while the caller holds it.
func (s *server) readOpenIssueAsks(ctx context.Context, tx pgx.Tx, issueKey string) ([]model.Ask, []model.Ask, error) {
	open, err := s.readIssueAsks(ctx, tx, `
		select `+askRowColumns+`
		`+askRowFrom+`
		where a.issue_key = $1 and a.state = 'open'
		order by a.id
	`, issueKey)
	if err != nil {
		return nil, nil, err
	}
	shown := make([]model.Ask, len(open))
	for index, ask := range open {
		ask.State = "resolved"
		shown[index] = ask
	}
	return open, shown, nil
}

// recordWithdrawnAsksTx is the close's second half: it records each of open, the open asks of
// issueKey readOpenIssueAsks read, resolved with kind withdrawn, naming actor, the closer, and
// queues one ask.resolved event for each, whose actor is the closer and whose owner is the issue.
// It answers the asks as withdrawn and the events, filled in once Commit has run the write phase.
func (s *server) recordWithdrawnAsksTx(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, issueKey string, open []model.Ask, actor model.Actor) ([]model.Ask, []*model.Event, error) {
	reason := withdrawnReason(issueKey)
	withdrawn := make([]model.Ask, 0, len(open))
	appended := make([]*model.Event, 0, len(open))
	for _, ask := range open {
		ask, err := docs.WriteAskResolution(ctx, tx, ask, model.AskResolutionWithdrawn, reason, actor)
		if err != nil {
			return nil, nil, err
		}
		event, err := s.queueLoggedEvent(ledger, issueOwner(issueKey).event(
			"ask.resolved", actor, model.NewAskEventPayload(ask, model.ReferenceChanges{}),
		), events.ThreadLoginsOf(ask.ID), "", "")
		if err != nil {
			return nil, nil, err
		}
		withdrawn = append(withdrawn, ask)
		appended = append(appended, event)
	}
	return withdrawn, appended, nil
}

// prepareRestoration is the reopen's first half: it reads every ask of issueKey a close withdrew
// and writes each back open, with no answer and no resolution, keeping everything else (an
// approval request its id, approval and hand-back, so its turn is the one it had)
// (docs.WriteRestoredAsks), then writes the blocks showing them and their copies open, reading the
// rows as those writes leave them. It answers the restored asks, oldest id first, as the reopen
// restored them, for recordRestoredAsksTx.
//
// An approval request is judged first, at the versions it names, by the form of The Spec
// (specFormOf, spec_form.go): one a text of whose covered documents breaks the form is not opened
// again. It is recorded resolved, with kind resolved and the findings as its reason
// (specform.ActRestore), naming actor, and recordRestoredAsksTx queues one ask.resolved event for
// it after the ask.opened events. It has no block, so no document write changes, and the
// resolution takes it out of withdrawnAsk, so no later restore or repair finds it again. Its
// author reads the findings. It answers those requests too, oldest id first, beside the restored
// asks.
//
// It also marks owing a settlement each document of the issue whose ask blocks a settlement after
// the reopen has to reconcile (docs.MarkReopenOwedAskDocuments), reading the rows as the restore
// leaves them, open: one that holds an ask block no settlement has indexed, which a settlement
// while the issue was closed saved resolved with no ask, recording its author; the block document
// of each open block ask, among them a restored ask whose block left that document while the
// issue was closed, which the ask-state write found no block of and so did not write; and one
// holding the block of an ask a settlement retracted, which came back while the issue was closed
// and stayed retracted. The settlement this mark leaves owed opens the first kind's ask, credited
// to its recorded author, retracts a restored ask whose block left, and restores the retracted
// ask. Its pending rows are queued after the ask-state write and before the first event, and no
// copy lock comes after them (docs.lockProjectCopies).
func (s *server) prepareRestoration(ctx context.Context, tx pgx.Tx, issueKey string, actor model.Actor) (restored, leftResolved []model.Ask, err error) {
	withdrawn, err := s.readIssueAsks(ctx, tx, `
		select `+askRowColumns+`
		`+askRowFrom+`
		where a.issue_key = $1 and `+withdrawnAsk+`
		order by a.id
	`, issueKey)
	if err != nil {
		return nil, nil, err
	}
	type brokenRequest struct {
		ask    model.Ask
		reason string
	}
	var broken []brokenRequest
	reopening := make([]model.Ask, 0, len(withdrawn))
	for _, ask := range withdrawn {
		if ask.Kind == "approval" && ask.Approval != nil {
			form, err := s.specFormOf(ctx, tx, ask.Approval.Documents, specform.ActRestore)
			if err != nil {
				return nil, nil, err
			}
			if refusal := form.refusal(specform.ActRestore); refusal != nil {
				broken = append(broken, brokenRequest{ask: ask, reason: refusal.Error()})
				continue
			}
		}
		reopening = append(reopening, ask)
	}
	restored = make([]model.Ask, 0, len(reopening))
	for _, ask := range reopening {
		ask.State = "open"
		ask.Answer = nil
		ask.Resolution = nil
		restored = append(restored, ask)
	}
	if err := docs.WriteRestoredAsks(ctx, tx, restored); err != nil {
		return nil, nil, err
	}
	leftResolved = make([]model.Ask, 0, len(broken))
	for _, request := range broken {
		ask, err := docs.WriteAskResolution(ctx, tx, request.ask, specFormRestoreResolution, request.reason, actor)
		if err != nil {
			return nil, nil, err
		}
		leftResolved = append(leftResolved, ask)
	}
	if _, err := s.deps.Docs.WriteIssueAskStates(ctx, issueKey, restored, actor, false); err != nil {
		return nil, nil, err
	}
	if err := s.deps.Docs.MarkReopenOwedAskDocuments(ctx, issueKey); err != nil {
		return nil, nil, err
	}
	return restored, leftResolved, nil
}

// recordRestoredAsksTx is the reopen's second half: it indexes the parts of each of restored, the
// asks prepareRestoration wrote back open, and queues one ask.opened event for each, whose actor
// is actor, the reopener, as settlement's own restoration of an ask appends. Then it queues one
// ask.resolved event, whose actor is actor too, for each of leftResolved, the approval requests
// prepareRestoration left resolved for the form of The Spec, so a request is never seen to reopen
// before its resolution.
//
// Once those events are queued, it does for each document of the issue what a settlement while
// the issue was closed skipped (docs.Service.FollowClosedIssueVersions): it moves the open
// approval request that names the document to the document's latest version, in the name of the
// editor a settlement while the issue was closed credited, and carries an approval to that
// version, as the version's own write does on an open issue. A document with updates its latest
// version does not hold, this reopen's ask-state write among them, is left to the settlement that
// saves them, whose version's own write does both. The move and the carry are queued after the
// ask.opened events and after the ask.resolved events of the requests left resolved, so a
// restored request reopens before it moves or a carry answers it, and a request left resolved is
// not moved, and they take no copy lock and write no pending row. It then forgets the editor a
// settlement while the issue was closed kept for each document that owes no settlement
// (doc_closed_edit_sources); a document that owes one keeps it until that settlement deletes it
// (docs.clearSettlementPending). The follow reads the versions, the requests and the asks' events
// and renders each version it carries over, so its caller runs this in the transaction's read
// phase; the events it appends reach the ledger, which publishes them as Commit ends, ahead of the
// ones this answers.
//
// It answers the ask.opened and ask.resolved events, filled in once Commit has run the write
// phase.
func (s *server) recordRestoredAsksTx(ctx context.Context, ledger *docs.Ledger, issueKey string, restored, leftResolved []model.Ask, actor model.Actor) ([]*model.Event, error) {
	appended := make([]*model.Event, 0, len(restored)+len(leftResolved))
	restoredAt := time.Now().UTC()
	for _, ask := range restored {
		// The restore took each ask's answer off, which removes its answer part: the index is
		// queued after the row write that took it off (prepareRestoration), and the ask.opened
		// names what that moved and stamps the part rows the index leaves with its own id, so a
		// ?since= read lists them.
		changes, err := s.queueAskParts(ctx, ledger, ask, nil, restoredAt, model.ReferenceChanges{})
		if err != nil {
			return nil, err
		}
		event, err := s.queueLoggedEvent(ledger, issueOwner(issueKey).event(
			"ask.opened", actor, model.NewAskEventPayload(ask, changes),
		), events.ThreadLoginsOf(ask.ID), "ask", ask.ID)
		if err != nil {
			return nil, err
		}
		appended = append(appended, event)
	}
	for _, ask := range leftResolved {
		event, err := s.queueLoggedEvent(ledger, issueOwner(issueKey).event(
			"ask.resolved", actor, model.NewAskEventPayload(ask, model.ReferenceChanges{}),
		), events.ThreadLoginsOf(ask.ID), "", "")
		if err != nil {
			return nil, err
		}
		appended = append(appended, event)
	}
	if err := s.deps.Docs.FollowClosedIssueVersions(ctx, issueKey); err != nil {
		return nil, err
	}
	return appended, nil
}

// specFormRestoreResolution is the resolution kind of an approval request a restore leaves
// resolved for the form of The Spec: resolved, not withdrawn, so withdrawnAsk no longer holds for it.
const specFormRestoreResolution = "resolved"

// sweepClosedIssueAsks is the close's last pass over its issue's documents, run once the close
// has committed. The commit's notification (migration 20261101181500's trigger) closes the issue's
// rooms on every process, so a room stops taking a browser's ask block within moments of it; the
// sweep catches the blocks written before then. In a transaction of its own, holding the issue's
// owner lock, it writes the blocks of withdrawn - the asks the close withdrew - and of every
// other resolved block ask of the issue, and their copies, again, from their rows as they now
// stand, writes the copies of every answered block ask of the issue from its row with the answer
// left out (docs.WriteIssueAskStates), and writes resolved every readable ask block no settlement
// has indexed: one a browser wrote into the room after the close forked it, or one whose
// settlement failed before the close. Each such block is logged. An issue reopened in the meantime
// is left alone. The sweep never fails the close: an error is logged. A block a room stores after
// the sweep, before its process has heard of the close, leaves its document owing a settlement,
// which runs on the closed issue: it saves the block resolved and opens no ask for it. The
// repair's interval pass sweeps a closed issue's document that still owes a settlement when it
// runs (RunIssueAskRepair).
//
// ctx must not end when the client goes away (outlivingTheClient), and no transaction is open.
func (s *server) sweepClosedIssueAsks(ctx context.Context, issueKey string, withdrawn []model.Ask, actor model.Actor) {
	if err := s.sweepClosedIssueAsksTx(ctx, issueKey, withdrawn, actor); err != nil {
		slog.Error("dispatch: sweep the ask blocks of a closed issue", "issue", issueKey, "error", err)
	}
}

func (s *server) sweepClosedIssueAsksTx(ctx context.Context, issueKey string, withdrawn []model.Ask, actor model.Actor) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := s.deps.Docs.Join(ctx, tx)
	defer ledger.Discard()
	var closed bool
	// The issue's owner lock, then its read in a statement of its own (store.LockOwnerAndRead). The
	// sweep's document writes are queued for its commit: it writes nothing else.
	if err := store.LockOwnerAndRead(ctx, tx, store.IssueOwner(issueKey), `
		select closed_at is not null from issues where key = $1
	`, []any{issueKey}, &closed); err != nil {
		return fmt.Errorf("lock the closed issue: %w", err)
	}
	if !closed {
		return nil
	}
	ids := make([]string, len(withdrawn))
	for index, ask := range withdrawn {
		ids[index] = ask.ID
	}
	if _, err := s.sweepIssueAskBlocksTx(documentCtx, tx, issueKey, ids, nil, actor); err != nil {
		return err
	}
	return ledger.Commit(ctx)
}

// sweepIssueAskBlocksTx is the close's final sweep of issueKey's documents (sweepClosedIssueAsks)
// in tx, which the caller holds the issue's owner lock in and ctx is joined to: it writes the blocks of
// the asks withdrawnIDs names, the asks the close withdrew (none when nil), and of every other
// resolved or answered block ask of the issue, and their copies, from their rows as they stand,
// and the blocks of withdrawing, open asks as their blocks show them once withdrawn
// (readOpenIssueAsks), which a repair records withdrawn after this, all in one write of each
// document (docs.WriteIssueAskStates). It marks resolved every readable ask block no settlement
// has indexed, which WriteIssueAskStates logs, and answers them for a caller that counts them.
func (s *server) sweepIssueAskBlocksTx(ctx context.Context, tx pgx.Tx, issueKey string, withdrawnIDs []string, withdrawing []model.Ask, actor model.Actor) ([]docs.BlockRef, error) {
	// The rows as they stand under the issue's owner lock: a reopen, an answer and a second close
	// between the close's commit and this lock leave them other than the close wrote them. Every
	// other resolved or answered block ask's blocks are written from its row too: an ask resolved
	// or answered after the close's settlement ran and before its transaction took the issue's lock
	// marked its copies owed a settlement, which runs only after the close has committed, so the
	// sweep writes those copies its state now, in the one write it makes of each document. An
	// answered ask's own block is the answer route's to write, and WriteIssueAskStates leaves it as
	// it is. A nil withdrawnIDs is a null array, which matches no id. withdrawing are open rows,
	// which the query does not select.
	current, err := s.readIssueAsks(ctx, tx, `
		select `+askRowColumns+`
		`+askRowFrom+`
		where a.issue_key = $1 and (a.id = any($2::uuid[]) or (a.state in ('resolved', 'answered') and a.block_id is not null))
		order by a.id
	`, issueKey, withdrawnIDs)
	if err != nil {
		return nil, err
	}
	return s.deps.Docs.WriteIssueAskStates(ctx, issueKey, append(current, withdrawing...), actor, true)
}

// readIssueAsks reads the asks query selects (askRowColumns from askRowFrom), with their opening
// event ids, as the ask.* event payloads carry them.
func (s *server) readIssueAsks(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]model.Ask, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the issue's asks: %w", err)
	}
	asks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Ask, error) {
		return scanAskRow(row)
	})
	if err != nil {
		return nil, fmt.Errorf("read the issue's asks: %w", err)
	}
	pointers := make([]*model.Ask, len(asks))
	for index := range asks {
		pointers[index] = &asks[index]
	}
	if err := s.attachOpenedEventIDs(ctx, tx, pointers); err != nil {
		return nil, err
	}
	return asks, nil
}
