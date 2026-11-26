package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// AskColumns selects one ask row, aliased a, in the order ScanAsk reads it. Its block artifact is
// the document holding the ask's block (refs.AskBlockDocumentSQL): for an ask of an issue room,
// whose row is keyed by the room's tree, the document of the fragment that holds the block. Every
// write into the block reaches that document's fragment through it, and every read names that
// document.
var AskColumns = `a.id::text, a.issue_key, a.artifact_id::text, a.block_id, (` + refs.AskBlockDocumentSQL("a") + `)::text, a.author, a.question, a.options, a.multiple, a.urgency,
		a.anchor, a.state, a.answer, a.resolution, a.created_at, a.edited_at, a.kind, a.approval, a.asked_of, a.asked_of_by, a.asked_of_at`

// ScanAsk decodes one AskColumns row. extra receives, in order, any columns the query
// selects after AskColumns.
func ScanAsk(row pgx.Row, extra ...any) (model.Ask, error) {
	var ask model.Ask
	var author, options, anchor, answer, resolution, approval []byte
	var editedAt *time.Time
	targets := append([]any{
		&ask.ID, &ask.IssueKey, &ask.ArtifactID, &ask.BlockID, &ask.BlockArtifactID, &author, &ask.Question, &options, &ask.Multiple, &ask.Urgency,
		&anchor, &ask.State, &answer, &resolution, &ask.CreatedAt, &editedAt, &ask.Kind, &approval, &ask.AskedOf, &ask.AskedOfBy, &ask.AskedOfAt,
	}, extra...)
	if err := row.Scan(targets...); err != nil {
		return model.Ask{}, err
	}
	if err := json.Unmarshal(author, &ask.Author); err != nil {
		return model.Ask{}, fmt.Errorf("decode ask author: %w", err)
	}
	if err := json.Unmarshal(options, &ask.Options); err != nil {
		return model.Ask{}, fmt.Errorf("decode ask options: %w", err)
	}
	if len(anchor) > 0 {
		var value model.Anchor
		if err := model.DecodeAnchor(anchor, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask anchor: %w", err)
		}
		ask.Anchor = &value
	}
	if len(answer) > 0 {
		var value model.AskAnswer
		if err := json.Unmarshal(answer, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask answer: %w", err)
		}
		ask.Answer = &value
	}
	if len(resolution) > 0 {
		var value model.AskResolution
		if err := json.Unmarshal(resolution, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask resolution: %w", err)
		}
		ask.Resolution = &value
	}
	if len(approval) > 0 {
		var value model.AskApproval
		if err := json.Unmarshal(approval, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask approval: %w", err)
		}
		// Every stored approval carries its documents list: the migration backfilled it and the
		// asks_normalize_approval_documents trigger writes it for a row a release that knows
		// only the four lead fields inserts during a rolling deploy.
		ask.Approval = &value
	}
	ask.EditedAt = askTimestamp(editedAt)
	return ask, nil
}

// WriteAskResolution closes an open ask the caller has locked as retracted, resolved or
// declined: it records the kind, the person's reason and who closed it, and returns the ask as it
// now stands. A decline (model.AskResolutionDeclined) is the one kind that carries words of
// Dispatch's own, so this function writes model.DeclineDispatchWords under that kind and leaves
// DispatchWords empty under every other: no caller can put the words elsewhere. It is the one
// place that says what resolving an ask stores; each caller writes a block ask's own state and
// appends the ask.resolved event its path owns.
//
// The resolution is decided here and written through the one writer of an ask's answer state
// (asks.PlanSettle, asks.Settled.Queue): on the ask's node, then on the row. The write is a write
// step (write): queued on ctx's ledger when the caller joined one, so it runs in the
// transaction's write phase in the order the caller made it. What it states on the node waits in
// held for the ask.resolved event the caller appends after it, which the caller hands to
// graphstore.QueueCaused once it has queued that event (graphstore.Caused where nothing queues).
// A caller that plans in its read phase and queues later plans the same close with
// PlanAskResolution instead.
func WriteAskResolution(ctx context.Context, tx pgx.Tx, ask model.Ask, kind, reason string, actor model.Actor, held *graphstore.Uncaused) (model.Ask, error) {
	settled, ask, err := PlanAskResolution(ctx, tx, ask, kind, reason, actor, held)
	if err != nil {
		return model.Ask{}, err
	}
	if err := write(ctx, tx, settled.Queue); err != nil {
		return model.Ask{}, fmt.Errorf("write ask resolution: %w", err)
	}
	return ask, nil
}

// PlanAskResolution decides the close of ask as WriteAskResolution writes it - the kind, the
// reason, who closed it, and the Dispatch words a decline alone carries - and plans it through the
// one writer of an ask's answer state (planSettle: from the ledger joined to ctx where one queues
// tx's writes, which records the plan as the ask's latest), writing nothing. It answers the
// planned write, which the caller queues where it makes the write (asks.Settled.Queue), and the
// ask as that write leaves it. What the write states on the ask's node waits in held, as
// WriteAskResolution's does. It is for a transaction that reads before its first write: the
// close of an issue plans the withdrawal of each open ask before its own update, and queues each
// after it.
func PlanAskResolution(ctx context.Context, tx pgx.Tx, ask model.Ask, kind, reason string, actor model.Actor, held *graphstore.Uncaused) (asks.Settled, model.Ask, error) {
	resolution := model.AskResolution{Kind: kind, Reason: reason, Actor: actor, At: time.Now().UTC()}
	if kind == model.AskResolutionDeclined {
		resolution.DispatchWords = model.DeclineDispatchWords
	}
	change := asks.Resolved(resolution)
	change.Who, change.At, change.Held = actor, resolution.At, held
	settled, err := planSettle(ctx, tx, ask.ID, change)
	if err != nil {
		return asks.Settled{}, model.Ask{}, fmt.Errorf("write ask resolution: %w", err)
	}
	ask.State, ask.Answer, ask.Resolution = settled.Ask.State, settled.Ask.Answer, settled.Ask.Resolution
	return settled, ask, nil
}

// WriteRestoredAsks writes back open each of restored, asks an issue's close withdrew whose rows
// its reopen restores: each row gets state open with no answer and no resolution and keeps
// everything else. Each is written through the one writer of an ask's answer state, in actor's
// name at at (asks.PlanSettle with asks.Reopened, asks.Settled.Queue), as a write step (write),
// queued on ctx's ledger when the caller joined one; what it states on the node of restored[i]
// waits in held[i] for that ask's ask.opened, which the caller appends after it and hands it to
// graphstore.QueueCaused once it has queued that event (graphstore.Caused where nothing queues).
// held holds one write for each of restored. Each block ask's row is recorded as it will stand
// (recordAskBlock), so a read of the block asks or of a copy's source later in the transaction -
// the reopen's write of the asks' blocks and copies (WriteIssueAskStates), which classifies them
// by the rows - reads it open, as it would once the update had run. Every restored ask is recorded
// too (Ledger.restored), for the reopen's other reads of the asks restored: its mark of the
// documents whose blocks wait for it, and its follow of the versions written while the issue was
// closed (MarkReopenOwedAskDocuments, FollowClosedIssueVersions), which read an approval request
// and the asks' events.
func WriteRestoredAsks(ctx context.Context, tx pgx.Tx, restored []model.Ask, actor model.Actor, at time.Time, held []graphstore.Uncaused) error {
	if len(restored) == 0 {
		return nil
	}
	if len(held) != len(restored) {
		return fmt.Errorf("restore %d withdrawn asks with %d held writes: one is held for each", len(restored), len(held))
	}
	ids := make([]string, len(restored))
	for index, ask := range restored {
		ids[index] = ask.ID
		change := asks.Reopened()
		change.Who, change.At, change.Held = actor, at, &held[index]
		settled, err := planSettle(ctx, tx, ask.ID, change)
		if err != nil {
			return fmt.Errorf("restore withdrawn ask: %w", err)
		}
		if err := write(ctx, tx, settled.Queue); err != nil {
			return fmt.Errorf("restore withdrawn ask: %w", err)
		}
	}
	for _, ask := range restored {
		if ask.BlockArtifactID != nil {
			recordAskBlock(ctx, tx, *ask.BlockArtifactID, ask)
		}
	}
	if ledger := queueFor(ctx, tx); ledger != nil {
		ledger.restored = append(ledger.restored, ids...)
	}
	return nil
}

// restoredAsksOf is the ids of the asks whose restore the ledger queueing tx's writes holds
// (WriteRestoredAsks): none where no ledger queues them, since the restore then ran at once.
func restoredAsksOf(ctx context.Context, tx pgx.Tx) []string {
	if ledger := queueFor(ctx, tx); ledger != nil {
		return ledger.restored
	}
	return nil
}

// askTimestamp formats a nullable ask timestamp as the wire's RFC 3339 UTC text, nil for nil.
func askTimestamp(value *time.Time) *string {
	if value == nil {
		return nil
	}
	return new(value.UTC().Format(time.RFC3339Nano))
}
