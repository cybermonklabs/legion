package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ApprovalDocumentsJSONPath is the artifact ids of every document an approval names, over the
// ask aliased a: the expression asks_open_approval_documents indexes, so `?` over it finds the
// open request one document is named by and `?|` the requests naming any of several, wherever
// each stands in the list.
const ApprovalDocumentsJSONPath = `jsonb_path_query_array(a.approval, '$.documents[*].artifact_id')`

// OpenApprovalAsk returns the open approval ask naming artifactID, or nil. A request that names
// two documents is found by either of them. The caller holds the document owner's lock, which
// every writer of an approval ask takes first (a request, a review, a version move or carry), so
// it observes the same version the review and approval routes use and the row holds still until
// the caller's transaction ends; the read takes no row lock.
func OpenApprovalAsk(ctx context.Context, tx pgx.Tx, artifactID string) (*model.Ask, error) {
	return OpenApprovalAskForDocuments(ctx, tx, []string{artifactID})
}

// OpenApprovalAskForDocuments returns the newest open approval ask naming any of artifactIDs, or
// nil; like OpenApprovalAsk it takes no row lock, the caller holding the document owner's lock. A
// request about to name several documents is found by any one of them, so a request opened over
// an issue's Verification document alone is the one a later request over the spec and that
// document renews: two open requests naming one document would ask a person twice about it and
// strand whichever they did not answer.
func OpenApprovalAskForDocuments(ctx context.Context, tx pgx.Tx, artifactIDs []string) (*model.Ask, error) {
	return openApprovalAskAmong(ctx, tx, artifactIDs, nil)
}

// openApprovalAskAmong is OpenApprovalAskForDocuments with each ask of restored, whose restore the
// caller's transaction has queued and not yet written (WriteRestoredAsks), read as that restore
// leaves it: open, with no answer and no resolution.
func openApprovalAskAmong(ctx context.Context, tx pgx.Tx, artifactIDs, restored []string) (*model.Ask, error) {
	state := `a.state = 'open'`
	args := []any{artifactIDs}
	if len(restored) > 0 {
		state = `(a.state = 'open' or a.id = any($2::uuid[]))`
		args = append(args, restored)
	}
	row := tx.QueryRow(ctx, `
		select `+AskColumns+`
		from asks a
		where a.kind = 'approval' and `+state+` and `+ApprovalDocumentsJSONPath+` ?| $1::text[]
		order by a.created_at desc, a.id desc
		limit 1
	`, args...)
	ask, err := ScanAsk(row)
	if err == nil {
		if ask.State != "open" {
			ask.State, ask.Answer, ask.Resolution = "open", nil, nil
		}
		return &ask, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return nil, fmt.Errorf("read open approval ask: %w", err)
}

// openApprovalAsk is OpenApprovalAsk as this transaction's own writes left it: the row a version
// write of this transaction moved to its version (MoveApprovalAsk), edited_at included, else the
// stored one, where a request whose restore this transaction queued reads open (the reopen's
// follow, FollowClosedIssueVersions). A request naming several documents is held under each of
// them (recordPendingApproval), so whichever document a later read finds it by - the carry that
// follows the move, or the follow's move of the request's next document - reads the one row. A
// request this transaction's carry has answered (answerApprovalAsk, which records the answered
// row) is open no longer, so this answers none: the carry's answer is the one write that closes
// the request, and refuseSecondCarry fails every later version write of that document in the same
// transaction, so no reader that goes on to commit finds the entry either way.
func openApprovalAsk(ctx context.Context, tx pgx.Tx, artifactID string) (*model.Ask, error) {
	if ledger := queueFor(ctx, tx); ledger != nil {
		if ask := ledger.approvals[artifactID]; ask != nil {
			if ask.State != "open" {
				return nil, nil
			}
			return ask, nil
		}
	}
	return openApprovalAskAmong(ctx, tx, []string{artifactID}, restoredAsksOf(ctx, tx))
}

// MoveApprovalAsk advances the open approval ask naming the document to version, a settled version
// of it whose text is markdown: the version its caller just wrote, or, at a reopen, the latest
// version a settlement wrote while the issue was closed (FollowClosedIssueVersions). A request
// that names two documents moves whichever of them was written. mover is the actor whose edit
// moved the request: the caller knows it even when the version credits more than one author. The
// row and its thread stay open; requested_version records that the agent must hand it back before
// a human sees it in Waiting on you again. Only the first move since the request was opened or
// handed back wakes anyone, even when a thread reply already left it waiting on its agent; a later
// one, while a document's requested_version is already below the version it named, is quiet
// (model.AskEditEventPayload.Quiet, the rule RewriteApprovalAsk also applies).
//
// bySettlement says a settlement wrote version: the settlement's own version write
// (versionWrite.settlement), or the reopen's follow of a version written while the issue was
// closed, which a settlement or a comment anchor's snapshot wrote, and which no stored version
// tells apart (FollowClosedIssueVersions). Only then, when the document waits on the human (its
// requested_version is the version the request names) and markdown is byte for byte the stored
// text of that version (restoresVersionText), does its requested_version move with its version,
// quietly: a settlement saves what the room already holds, such as the blocks a close and a reopen
// wrote back, so the text the person was asked about did not change and the request keeps its
// turn as if it had not moved, waking nobody. A version a route writes - a named version, an
// upload, an edit or a summary edit - moves the request to its agent whatever text it stores, as
// every move did before the rule: the Legion daemon answers an agent's gate wait with such a
// version's artifact.version, telling the agent the request is back on its turn, which this keeps
// true.
//
// It runs in a transaction a caller joined, as every other document operation does, and in the
// one a settlement or the block-id backfill owns, which no caller joins (Ledger): the version
// write every document version goes through (writeVersionTx) runs in both. A context holding no
// ledger at all is refused by the rewording it makes (rewriteApprovalAsk, errUnjoined). Its reads
// - the request, as this transaction's own writes left it (openApprovalAsk), and the stored text
// the keep-turn rule compares - run where it is called; its writes reach the transaction's write
// queue and the event it appends reaches its ledger (eventSink), which publishes it after its
// Commit. Where its writes are queued, the ledger records the request as moved
// (recordPendingApproval), so the carry that follows in the same version write reads the request
// as this move leaves it.
func MoveApprovalAsk(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, version model.Version, markdown string, mover model.Actor, bySettlement bool, serverURL string) error {
	return moveApprovalAsk(ctx, tx, broker, artifactID, version, markdown, mover, nil, bySettlement, serverURL)
}

// moveApprovalAsk is MoveApprovalAsk with keptEditor, the editor a settlement on the document's
// closed issue kept, set by a settlement's version that adds none of that editor's text and so
// names SettlementActor as mover (versionWrite.keptEditor). A request that names a version before
// the one this version follows was moved by none of the versions between, so each was written
// while the issue was closed (versionWrite.closedIssue), and its move names keptEditor, as the
// reopen's move to such a version does (FollowClosedIssueVersions). The rule takes those versions
// to hold that editor's text; no stored version says whose text each added, so a version the close
// alone produced, or one a settlement wrote for another edit, is read the same way. A request that
// names the version this one follows moves over this version's text alone, in mover's name.
//
// The version the keep-turn rule reads is a stored one: the request names a version written
// before this transaction, since the one transaction that moves a request with bySettlement set
// writes at most one version of the document (a settlement's) or none (the reopen's follow).
func moveApprovalAsk(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, version model.Version, markdown string, mover model.Actor, keptEditor *model.Actor, bySettlement bool, serverURL string) error {
	ask, err := openApprovalAsk(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	if ask == nil {
		return nil
	}
	moved := ask.Approval.Document(artifactID)
	if moved == nil || moved.Version == version.Number {
		return nil
	}
	if keptEditor != nil && moved.Version < version.Number-1 {
		mover = *keptEditor
	}
	summary, err := ApprovalAskSummary(*ask)
	if err != nil {
		return err
	}
	keepsTurn := false
	if bySettlement && moved.RequestedVersion == moved.Version {
		if keepsTurn, err = restoresVersionText(ctx, tx, artifactID, moved.Version, markdown); err != nil {
			return err
		}
	}
	documents := slices.Clone(ask.Approval.Documents)
	for index := range documents {
		if documents[index].ArtifactID == artifactID {
			documents[index].Version = version.Number
			if keepsTurn {
				documents[index].RequestedVersion = version.Number
			}
		}
	}
	quiet := keepsTurn || ApprovalAwaitsHandBack(ask.Approval.Documents)
	if err := rewriteApprovalAsk(ctx, tx, broker, ask, mover, documents, summary, serverURL, quiet); err != nil {
		return err
	}
	recordPendingApproval(ctx, tx, artifactID, ask)
	return nil
}

// RewriteApprovalAsk rewords one open approval row to name documents with summary, indexes its
// new question and the parts that question is the first of, stamps edited_at, and records its
// ask.edited event, through the rewording a document version move (MoveApprovalAsk) also takes
// (rewriteApprovalAsk). A version move, a request with a new summary and a request that changes
// which documents it covers all reword; none hands the request back, so each document's
// requested_version stays as it was. The one exception is a settlement's version move that stores
// byte for byte the text of the version the person was asked about, which moves that document's
// requested_version with its version, quietly (MoveApprovalAsk). A move of a request that an
// earlier version already moved since it was opened or handed back, a document's
// requested_version below the version it named, is recorded quiet
// (model.AskEditEventPayload.Quiet): it changes only that version, so like a human's unnamed version
// it wakes nobody. The event it appends goes to the ledger of the transaction it runs in, as every
// document operation's does (eventSink), which publishes it after its Commit; no caller reads it
// back, since where the writes are queued the append has not run when this returns.
//
// It refuses a context holding no ledger at all (errUnjoined), the one context in which the
// rewording would be told to nobody: there the update runs at once while the event reaches no
// ledger to publish it, leaving the row reworded and the request's readers none the wiser. A
// settlement's ledger and the block-id backfill's are in the context of the writes they own, so
// this refuses neither.
//
// The new question, the approval it encodes, the references it cites, the parts it leaves and
// its edited_at are worked out here; the update, the references' writes, the parts' writes, the
// append and the stamp are one step (write).
// edited_at is clock_timestamp(), read here rather than in the update, so the event's copy of the
// row carries it before the write phase runs, and so does ask, which a later step of the same
// transaction queues from (carryApproval's answer).
func RewriteApprovalAsk(
	ctx context.Context,
	tx pgx.Tx,
	broker *events.Broker,
	ask *model.Ask,
	actor model.Actor,
	documents []model.AskApprovalDocument,
	summary, serverURL string,
) error {
	quiet := approvalMoves(ask.Approval.Documents, documents) && ApprovalAwaitsHandBack(ask.Approval.Documents)
	return rewriteApprovalAsk(ctx, tx, broker, ask, actor, documents, summary, serverURL, quiet)
}

// rewriteApprovalAsk is the rewording RewriteApprovalAsk and MoveApprovalAsk share. Each caller
// decides quiet, which marks its ask.edited (model.AskEditEventPayload.Quiet), by its own rule.
func rewriteApprovalAsk(
	ctx context.Context,
	tx pgx.Tx,
	broker *events.Broker,
	ask *model.Ask,
	actor model.Actor,
	documents []model.AskApprovalDocument,
	summary, serverURL string,
	quiet bool,
) error {
	if ledgerFrom(ctx) == nil {
		return errUnjoined
	}
	sink := eventSink(ctx)
	previous := model.NewAskEditPrevious(*ask)
	ask.Question = ApprovalQuestion(documents, summary)
	// A fresh approval rather than the old one changed in place: the event below keeps its own
	// copy of the row, which a later rewording of the same request in this transaction must not
	// reach before the event is appended.
	reworded := *ask.Approval
	reworded.Documents = documents
	ask.Approval = &reworded
	approval, err := EncodeApproval(ask.Approval)
	if err != nil {
		return err
	}
	reader := refs.NewReader(ctx, tx, ask.Author, "ask", ask.ID, serverURL)
	changes, index, err := planEditedAskReferences(ctx, tx, reader, ask.ID, previous.Question, ask.Question)
	if err != nil {
		return fmt.Errorf("index approval ask: %w", err)
	}
	var editedAt time.Time
	if err := tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&editedAt); err != nil {
		return fmt.Errorf("read approval ask edit time: %w", err)
	}
	ask.EditedAt = askTimestamp(&editedAt)
	row := *ask
	// The rewording is a revision of the request's question part, read through the one reader,
	// which its ask.edited names beside the request's own references. The parts are planned from
	// the row the update below writes, which the read of the index does not show yet, and written
	// in that same step; the row carries the edited_at that update sets, so the plan marks the
	// request edited without reading it back. The question's reconciliation is queued before the
	// parts, so the parts' step settles the forms queue (questionReconciled).
	changes, parts, err := planAskPartsCounted(ctx, tx, reader, row, editedAt, true, changes)
	if err != nil {
		return fmt.Errorf("index the parts of approval ask %s: %w", ask.ID, err)
	}
	payload := model.NewAskEditEventPayload(row, previous, actor, changes)
	payload.Quiet = quiet
	return write(ctx, tx, func(b *store.WriteBatch) error {
		b.Queue("rewrite approval ask", `
			update asks
			set question = $2, approval = $3, edited_at = $4
			where id = $1
		`, ask.ID, row.Question, approval, editedAt)
		if err := index(b); err != nil {
			return fmt.Errorf("index approval ask: %w", err)
		}
		if err := parts(b); err != nil {
			return fmt.Errorf("index the parts of approval ask %s: %w", ask.ID, err)
		}
		if err := broker.QueueAppend(b, model.Event{
			IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID,
			Type: "ask.edited", Actor: actor, Payload: payload,
		}, sink); err != nil {
			return fmt.Errorf("append approval ask edit: %w", err)
		}
		refs.QueueStamp(b, "ask", ask.ID)
		return nil
	})
}

// EncodeApproval is an approval ask's approval as asks.approval stores it.
func EncodeApproval(approval *model.AskApproval) ([]byte, error) {
	encoded, err := json.Marshal(approval)
	if err != nil {
		return nil, fmt.Errorf("encode approval ask: %w", err)
	}
	return encoded, nil
}

// ApprovalQuestion renders the server-owned question prefix - every document the request names,
// each with the version it names - with its optional summary.
func ApprovalQuestion(documents []model.AskApprovalDocument, summary string) string {
	named := make([]string, 0, len(documents))
	for _, document := range documents {
		named = append(named, fmt.Sprintf("%s (version %d)", document.Name, document.Version))
	}
	question := "Approve " + joinWithAnd(named) + "?"
	if summary == "" {
		return question
	}
	return question + " " + summary
}

// joinWithAnd lists parts as English does: "a", "a and b", "a, b and c".
func joinWithAnd(parts []string) string {
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// ApprovalAskSummary parses the summary after the server-owned approval question prefix. A
// question naming the request's first document alone is read too: that is what the release before
// the one that grew a request to two documents writes when a version move rewords such a request
// during a rolling deploy.
func ApprovalAskSummary(ask model.Ask) (string, error) {
	candidates := [][]model.AskApprovalDocument{ask.Approval.Documents}
	// The lead alone is a second candidate only when the request names more than one document:
	// over one it is the same list, and over none there is nothing to slice.
	if len(ask.Approval.Documents) > 1 {
		candidates = append(candidates, ask.Approval.Documents[:1])
	}
	for _, documents := range candidates {
		prefix := ApprovalQuestion(documents, "")
		if ask.Question == prefix {
			return "", nil
		}
		if summary, ok := strings.CutPrefix(ask.Question, prefix+" "); ok && summary != "" {
			return summary, nil
		}
	}
	return "", fmt.Errorf("approval ask %s has a question not written by the approval route", ask.ID)
}

// approvalMoves reports whether rewriting an approval's documents as next moves any document to
// another version.
func approvalMoves(current, next []model.AskApprovalDocument) bool {
	for _, document := range next {
		for _, before := range current {
			if before.ArtifactID == document.ArtifactID && before.Version != document.Version {
				return true
			}
		}
	}
	return false
}

// ApprovalAwaitsHandBack reports whether a version already moved one of the documents past the
// version its agent last handed to the human, which is the state in which a further move is
// quiet, and the state a version write's own answer reports to the agent that wrote it
// (api.movedApprovalRequestAdvice).
//
// It is the Go twin of the SQL approvalMoved in packages/envoy/internal/dispatch/api/
// ask_rows.go, which asks the same of a stored row rather than of an ask already read, and also
// of an approval as a build before requests held their documents wrote it, which only a read at a
// past time meets. The two are the server's only places `requested_version < version` is
// spelled, so nobody spells it a third time here: a reader looking for the rule finds one of them
// and the other named beside it. The client reads the same rule on its side, of the documents a
// report hands it (packages/envoy-client/src/dispatch-execute.ts).
func ApprovalAwaitsHandBack(documents []model.AskApprovalDocument) bool {
	for _, document := range documents {
		if document.RequestedVersion < document.Version {
			return true
		}
	}
	return false
}
