package workflow

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// ClearGeneration empties the facts one generation of issue owns (record.Store.ClearGeneration):
// a merged or closed pull request, the last generation's reading of a still-open one, its design
// gates, and each role's handoff, review rounds, verdict and summary. If the clear deletes gate
// rows of issue, the empty set of design-gate documents is queued for it, due at now
// (publishEmptied). issue is the record as tx holds it.
//
// It and ClearTreeGeneration are the only callers that clear a generation: one issue's, for an
// adopted member and a re-entered child, or a whole tree's, for a re-admitted root. The record
// owns the statements, and each takes the one forget as an input it requires (forgetSuperseded),
// which it runs over every decision kept on a cleared issue and every changes request the outbox
// holds on one while the issue's gates still stand, so each one that is no longer the person's
// word is deleted before the clear deletes the gate that records what took its place. Asked after
// the clear, a gate that is gone or new would make the decision read as the person's word again: a
// person who requested changes and then approved the latest version as their hand-over would send
// the new architect, or the controller, to revise the spec they approved. That holds as much for a
// request still queued, for the tree's architect while its pod is released or relaunching or for
// the controller while no session holds its claim, as for a kept one: its delivery asks the gates
// as they stand when it runs (the daemon's readNoticeTree and controllerNotice).
func (e *Engine) ClearGeneration(ctx context.Context, tx pgx.Tx, issue record.Issue, now time.Time) error {
	emptied, err := e.store.ClearGeneration(ctx, tx, issue, e.forgetSuperseded)
	if err != nil {
		return err
	}
	if err := e.publishEmptied(ctx, tx, emptied, now); err != nil {
		return fmt.Errorf("clear the generation of %s: %w", issue.Key, err)
	}
	return nil
}

// ClearTreeGeneration empties them for every issue of tree, and forgets first as ClearGeneration
// says. A root set back to todo starts a new generation of the whole tree, and its children
// carried the old one's pull request, gate and handoffs into it — a child re-entered at the new
// generation read a handoff of the last.
func (e *Engine) ClearTreeGeneration(ctx context.Context, tx pgx.Tx, tree string, now time.Time) error {
	emptied, err := e.store.ClearTreeGeneration(ctx, tx, tree, e.forgetSuperseded)
	if err != nil {
		return err
	}
	if err := e.publishEmptied(ctx, tx, emptied, now); err != nil {
		return fmt.Errorf("clear the generation of tree %s: %w", tree, err)
	}
	return nil
}

// forgetSuperseded is the forget a clear is handed (record.KeptForget), over the engine's store
// and gated steps: ForgetSuperseded of owed, the decisions kept on the cleared issues, and
// dropSupersededChanges of queued, the changes requests the outbox holds on them. A clear names
// nothing to an architect, so the gates ForgetSuperseded read for a task's text are dropped.
func (e *Engine) forgetSuperseded(ctx context.Context, tx pgx.Tx, tree string, owed record.KeptDecisions,
	queued []record.QueuedChanges) error {
	if _, err := ForgetSuperseded(ctx, tx, e.store, e.cfg.Steps, tree, owed); err != nil {
		return err
	}
	return e.dropSupersededChanges(ctx, tx, tree, queued)
}

// dropSupersededChanges deletes in tx each changes request of queued, on issues of tree, that the
// person's approval followed (Store.DropQueuedChanges), asked as its delivery would ask it
// (NoticeSuperseded) of the gates of the issue it was made on, read in one read, and writes one
// line for each once tx commits, naming the approved version. A request no approval followed
// stays, and is delivered as it would have been, asked again then of the gates as they stand. A
// runner that holds the lease of a row it deletes reads the row gone in the snapshot it asks the
// gates in, and finishes it unpublished (the daemon's stillOwed, which its readNoticeTree and
// controllerNotice read); one already past that read publishes, as it would publish a request
// whose approval committed after its read.
func (e *Engine) dropSupersededChanges(ctx context.Context, tx pgx.Tx, tree string, queued []record.QueuedChanges) error {
	if len(queued) == 0 {
		return nil
	}
	var issues []string
	for _, request := range queued {
		if !slices.Contains(issues, request.Issue) {
			issues = append(issues, request.Issue)
		}
	}
	gates, err := e.store.Gates(ctx, tx, issues)
	if err != nil {
		return err
	}
	var spent []int64
	for _, request := range queued {
		superseded := NoticeSuperseded(gates[request.Issue], e.cfg.Steps, request.Issue, request.Notice,
			record.DecisionDesignChangesRequested)
		if superseded == nil {
			continue
		}
		spent = append(spent, request.Row)
		e.logOnCommit(ctx, "workflow: a queued changes request finished unpublished at its generation's clear: "+
			"the person's approval followed it", "row", request.Row, "kind", request.Kind,
			"issue", request.Issue, "root", tree, "ref", request.Notice.Ref,
			"version", request.Notice.Version, "approved", superseded.By.Version)
	}
	if len(spent) == 0 {
		return nil
	}
	return e.store.DropQueuedChanges(ctx, tx, spent)
}

// publishEmptied queues in tx, due at now, one publication of the empty set of design-gate
// documents for each issue a clear emptied the gates of (record.Store.ClearGeneration answers
// them): those gates recorded a document until the clear, and Dispatch holds the set they
// recorded until something tells it otherwise. It is queued through the engine's own store,
// which in the daemon wakes the issue's board lane once the clear commits (projectRecords), so
// the publication is sent then rather than at the lane's next sweep. The empty set carries no
// entry, so nothing in it says whether a row holds a tree (record.DesignGateDocument.Holds), and
// it needs none of the deployment's steps to build it.
func (e *Engine) publishEmptied(ctx context.Context, tx pgx.Tx, emptied []string, now time.Time) error {
	for _, issue := range emptied {
		if err := e.store.EnqueueDesignGates(ctx, tx, record.DesignGatesPublish{Issue: issue, Gates: record.GateDocuments(nil, nil)}, now); err != nil {
			return err
		}
	}
	return nil
}
