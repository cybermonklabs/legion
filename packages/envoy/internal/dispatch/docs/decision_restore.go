package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The decisions step's third phase: for each issue, in one
// transaction, the restore of every answered ask an agent moved out of the issue's spec (R6, R7),
// then the conversion of every section of the issue's decisions document (R9), and the retirement
// of that document once it holds no section (R11). Both are the server's own writes, made in
// DecisionStepActor's name: never an agent's, so the refusal of an agent's write that drops an
// answered ask's block (refuseDroppedAnsweredAsks) is not theirs to meet. They write each document
// through its live document (applyLive) and its version through SnapshotVersion, under the
// owner-verified mark (withOwnerVerified), which a closed issue's spec takes as a settlement's
// version does: an issue is restored and converted whether it is open or closed.

// DecisionStepActor is the actor the decisions step's third phase writes as: the versions it
// writes, their events, and the facts it appends.
//
// kept for one release: decisions-step
var DecisionStepActor = model.Actor{Kind: "system", ID: "dispatch-decisions-step"}

// DecisionIssue is what the third phase did for one issue, or, under a dry run, would have done.
//
// kept for one release: decisions-step
type DecisionIssue struct {
	Issue string
	// Restored is each ask block the restore wrote into the spec, in answer-time order: each
	// answered ask it returned, and each ask block of the decisions document it moved, whatever its
	// ask's state, one no row names among them (RestoredAsk.Ask empty).
	Restored []RestoredAsk
	// Declined is each answered ask the restore neither returned nor moved because a person
	// removed its block: from the spec, for an ask of the main shape, the version that left it out
	// being theirs (removalOf); or from the decisions document, for an ask the restore would move.
	// That removal is the one its node's removed_by holds (nodeRemoval), or one the document's room
	// holds that no settlement has versioned yet, which the version of that document the pass
	// writes states (holdDecisionsRoomRemovals, writeDecisionsDocument).
	Declined []DeclinedAsk
	// Decisions, Readings, Dropped, Removed and Left count the sections of the issue's decisions
	// document by what became of each: a decision block, a reading under an ask, a section R9's
	// plain-answer rule deleted, a section deleted because the answered ask it cites is one a
	// person removed from its spec, and a section the pass leaves where it is until its next pass:
	// one whose body holds an ask block the document's block index does not hold yet, or whose
	// decision cites a version of the document that holds the section and none does yet.
	Decisions, Readings, Dropped, Removed, Left int
	// Retired is whether the decisions document was retired (RetireDocument).
	Retired bool
	// SpecVersion and DecisionsVersion are the versions the phase wrote of each, 0 for none.
	SpecVersion, DecisionsVersion int
	// OtherVersions is how many other documents of the issue the conversion wrote a version of:
	// each one holding an answered ask block a converted section restated (writePresentDocuments).
	OtherVersions int
	// SpecApproval is what became of the spec's approval: "carried" where its version carries the
	// approval, "superseded" where the spec's newest review approved a version and the phase's
	// version does not carry it (newestApproval), and "" where neither.
	SpecApproval string
	// Superseded is each approval the phase superseded, with the asks whose restore did it.
	Superseded []DecisionSupersededApproval
}

// RestoredAsk is one ask block the restore wrote: its ask's id, "" for a block of the decisions
// document no row names, its shape (graphcensus.RestoreReturns, RestoreMoves), the address of the
// block it moved from where it moved (a block of the decisions document, which a moved_to tuple
// now names), "" where it returns under the spec's own address, and the address of the block it
// stands at now, with whether that block took a new id. Read is, for an ask of the main shape,
// what the restore read of the version that left its block out of the spec (restoreReadNoPerson,
// restoreReadNeverHeld, restoreReadNoAuthors), and "" for an ask the restore moves.
//
// kept for one release: decisions-step
type RestoredAsk struct {
	Ask, Shape, From, To, Read string
	NewID                      bool
}

// What the restore read of the version that left an answered ask's block out of its spec, where
// it returns the ask: that version's authors name no person and no agent acting with the person
// scope; no version of the spec ever held the block; or that version names no author.
const (
	restoreReadNoPerson  = "no_person"
	restoreReadNeverHeld = "never_held"
	restoreReadNoAuthors = "no_authors"
)

// DeclinedAsk is one answered ask the restore declined to write into its spec: its id, the person
// who removed its block, and a version. For an ask of the main shape it is the person's version of
// the spec that left the block out. For an ask the restore would move it is the person's version
// of the decisions document that left the block out, 0 where the event that recorded the removal
// names no version. For a deletion in the decisions document's room that no settlement had
// versioned, it is the version of that document the pass wrote, which states the removal
// (writeDecisionsDocument).
//
// kept for one release: decisions-step
type DeclinedAsk struct {
	Ask, Person string
	Version     int
}

// DecisionSupersededApproval is an approval the phase superseded: the document's address, the
// approval, and the asks the restore wrote whose versions superseded it, each as the ask's node
// (dispatch://KEY/ask/<id>) and its block's address.
//
// kept for one release: decisions-step
type DecisionSupersededApproval struct {
	Document string
	Approval model.SupersededApproval
	Asks     []string
	Nodes    []string
}

// issueDocument is one document of an issue the phase writes: its id, its ref_key (the store's
// address of it), its reference (dispatch://…) and its name.
//
// kept for one release: decisions-step
type issueDocument struct {
	id, refKey, address, name string
}

// readIssueDocument reads the issue's document query answers, by id, in tx.
//
// kept for one release: decisions-step
func readIssueDocument(ctx context.Context, tx pgx.Tx, query string, args ...any) (issueDocument, bool, error) {
	document, err := scanIssueDocument(tx.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return issueDocument{}, false, nil
	}
	if err != nil {
		return issueDocument{}, false, err
	}
	return document, true, nil
}

// scanIssueDocument scans one row of issueDocumentColumns.
//
// kept for one release: decisions-step
func scanIssueDocument(row pgx.Row) (issueDocument, error) {
	var document issueDocument
	var issueKey, session *string
	var project, slug, kind string
	var primary bool
	if err := row.Scan(&document.id, &document.refKey, &issueKey, &project, &session, &slug, &kind, &primary, &document.name); err != nil {
		return issueDocument{}, err
	}
	document.address = refs.ArtifactRef(issueKey, project, session, slug, kind, primary)
	return document, nil
}

// issueDocumentColumns are the columns scanIssueDocument scans, off artifacts a.
//
// kept for one release: decisions-step
const issueDocumentColumns = `a.id::text, a.ref_key, a.issue_key, coalesce(a.project_key, ''), a.session_id, a.slug, a.kind, a.is_primary, a.name`

// restoredBlock is one ask block the restore writes into the spec: the work the finder read
// (graphcensus.ReadRestoreWork), or, for a block of the decisions document no work item moves, the
// work planDocumentMoves makes of it; the ask's row, none for a block no row names (rowless),
// which is written as the document held it (moved), since no row holds its question to build it
// from or names it to rebind, or, where the spec holds an ask block under its id already (joins),
// not written at all, its address moving to that block; the id its block takes, the address it
// moves from (from: for the second shape the address the finder read, and for the main shape the
// address of a copy of the block the decisions document holds, "" where it holds none), the block
// of a document of the issue it moves out of where that document still holds it (moved), for the
// main shape the block as the spec's last version holding it held it (held, removalOf), the spec
// blocks it shaped, the readings it holds, for the main shape what the restore read of the
// version that left the block out (RestoredAsk.Read), and what the write of its block did to the
// `shaped` pointers beside the block's own (shape: restoredPointers, movedBlockOf).
//
// kept for one release: decisions-step
type restoredBlock struct {
	work     graphcensus.RestoreWork
	row      model.Ask
	rowless  bool
	joins    bool
	id       string
	from     string
	moved    *pmdoc.Node
	held     *pmdoc.Node
	shaped   []string
	readings []*pmdoc.Node
	read     string
	shape    restoredShape
}

// declinedAsk is one answered ask the restore declines to write into the spec (removalOf,
// nodeRemoval): the person who removed its block, the version that left it out, the time of the
// removal and the event that recorded it, under which the pass states the removal (writeFacts).
//
// kept for one release: decisions-step
type declinedAsk struct {
	ask, person string
	version     int
	at          time.Time
	event       int64
}

// address is the block's address in the spec once restored.
//
// kept for one release: decisions-step
func (r *restoredBlock) address(spec issueDocument) string {
	return refs.BlockAddress(spec.refKey, r.id)
}

// sectionFate is what becomes of one section of the decisions document.
//
// kept for one release: decisions-step
type sectionFate int

// kept for one release: decisions-step
const (
	// fateUntouched is a section this pass does not convert: one a restore alone leaves for the
	// conversion.
	fateUntouched sectionFate = iota
	// fateDecision is a section that becomes a decision block: one that cites no answered ask, one
	// under `## Replaced`, or one the conversion could not place under one ask.
	fateDecision
	// fateReading is a section that cites an answered ask: its body becomes a reading in that
	// ask's block.
	fateReading
	// fateDropped is a section that cites an answered ask and whose body is that ask's answer and
	// nothing more, or nothing at all beside the ask's own block: R9 deletes it.
	fateDropped
	// fateRemovedAsk is a section that cites an answered ask a person removed from its spec (its
	// node holds an asserted removed_by): R9 deletes it, because the person removed the decision it
	// reads, and its heading moves to the ask itself.
	fateRemovedAsk
	// fateLeft is a section the pass leaves where it is, and counts, until its next pass: one
	// whose body holds an ask block the document's block index does not hold yet, or whose
	// decision cites the version of the document that holds the section and none does yet.
	fateLeft
)

// convertedSection is one section of the decisions document and what becomes of it: its fate,
// the ask it cites (fateReading, fateDropped, fateRemovedAsk) or, for the decision of a section
// under `## Replaced`, the answered ask that decision settles (its from_ask), the block it becomes
// (fateDecision: the decision, under decisionID), the author of its newest version, and the Keep a
// person recorded on its heading.
//
// kept for one release: decisions-step
type convertedSection struct {
	section    decisionSection
	fate       sectionFate
	ask        string
	decision   *pmdoc.Node
	decisionID string
	reading    *pmdoc.Node
	author     model.Actor
	keep       *sectionKeep
	links      []string
	// askDocument and askBlock are where the ask's block stands once the pass is written: the
	// spec for an ask it restores, else the document of the issue that holds it already.
	askDocument issueDocument
	askBlock    string
}

// sectionKeep is a person's standing Keep of a section's heading (decision_keeps), and whether
// the sum it recorded is the section's sum as it stands. keeper is the actor the Keep's own
// decision.kept event recorded, which the kept_by and kept_text_sum the conversion carries it onto
// are written by: a row derived from an earlier write carries that write's actor, and so its
// Proven mark (model.Actor.Proven). A Keep no such event names that person for is the person alone,
// with no mark, since nothing read says a credential proved them.
//
// kept for one release: decisions-step
type sectionKeep struct {
	keptBy  string
	keeper  model.Actor
	keptAt  time.Time
	matches bool
}

// issueRestore is the third phase's work on one issue, in the transaction it holds.
//
// kept for one release: decisions-step
type issueRestore struct {
	service   *Service
	tx        pgx.Tx
	issue     string
	convert   bool
	at        time.Time
	spec      issueDocument
	decisions *issueDocument
	rows      map[string]model.Ask
	// answered is each ask of rows that holds an answer as a reading reads it: its row answered,
	// and its node too (askAnswers, as refuseReadingsWithoutAnswers reads it), so the pass writes
	// no reading into the block of an ask a writer of the release before answered on its row alone.
	answered map[string]bool
	// specBlocks is every block id the spec's block index holds.
	specBlocks map[string]bool
	// documents is every document of the issue that stands, by id, and askBlocks the block id of
	// each ask block each one holds, as its block index holds it: where an answered ask a section
	// restates stands when the restore does not write it (askBlockOf).
	documents map[string]issueDocument
	askBlocks map[string]map[string]bool
	restores  []*restoredBlock
	// declined is each answered ask the pass neither returns nor moves, a person having removed its
	// block (removalOf, nodeRemoval), and removed every ask of the issue a person removed: one whose
	// node holds an asserted removed_by, one the pass declines, or one of roomRemovals.
	declined []*declinedAsk
	removed  map[string]bool
	// roomRemovals is each answered ask whose block a person's browser deleted from the decisions
	// document's room before a settlement versioned the deletion, with that person, by ask id
	// (holdDecisionsRoomRemovals). The transaction's ledger holds each of these removals, and the
	// version of the document the pass writes states it (writeDecisionsDocument, removalsOf), so
	// none of them is in declined.
	roomRemovals map[string]string
	// present is the shaped pointers and the readings this pass writes into an answered ask block
	// a document of the issue holds already, by the document's id.
	present  map[string]*presentEdits
	sections []*convertedSection
	// sectionsLeft is how many sections the decisions document holds once this pass has written
	// it, and decisionsHoldAsks whether it still holds an ask block then, in a section or outside
	// every one: an ask block this pass does not move - one the document's block index does not
	// hold yet, which planDocumentMoves reads the blocks it moves from - which keeps the document
	// from its retirement (R6). decisionsDropped is each block id this pass takes out of the
	// decisions document (planSections), which a `shaped` pointer to it no longer reaches there
	// (movedPointer).
	sectionsLeft      int
	decisionsHoldAsks bool
	decisionsDropped  map[string]bool
	// indexedVersion is the version of the decisions document its block index stands at, 0 where
	// it owes one, and indexedBlocks each block that index holds (refs.BlockChanges): a section
	// whose heading it holds stands in that version, which a decision leaving part of the section
	// out cites (sectionVersion).
	indexedVersion int
	indexedBlocks  map[string]refs.BlockChange
	// pointerLine is the markdown of the pointer line the spec's write took out of its
	// `## Decisions` part (blockMarkdown), "" for none, headingAdded whether that write added the
	// part's heading to a spec that held no such part, and shapedAdded each `shaped` pointer the
	// pass's writes gave an answered ask block a document held already, by the document's id and
	// then the block's: with the blocks the spec's write added, what each version names as the
	// step's own write (stepCasesFor).
	pointerLine  string
	headingAdded bool
	shapedAdded  map[string]map[string][]string
	// readingsWritten is the block id of each reading the pass's writes put in an ask block a
	// document held already, by the document's id (writePresent): a section converted into a
	// reading whose reading is not among them was written nowhere (refuseUnwrittenReadings).
	readingsWritten map[string]map[string]bool
	result          *DecisionIssue
}

// presentEdits is what this pass writes into the answered ask blocks one document holds already:
// the spec blocks each one shaped and the readings it takes, by block id.
//
// kept for one release: decisions-step
type presentEdits struct {
	shaped   map[string][]string
	readings map[string][]*pmdoc.Node
}

// presentIn is the edits this pass writes into document's answered ask blocks.
//
// kept for one release: decisions-step
func (r *issueRestore) presentIn(document string) *presentEdits {
	edits := r.present[document]
	if edits == nil {
		edits = &presentEdits{shaped: map[string][]string{}, readings: map[string][]*pmdoc.Node{}}
		r.present[document] = edits
	}
	return edits
}

// errBlockIndexOwed is restoreIssueDecisions' answer for an issue a document of which still owes
// its block index (document_block_versions.version null): the pass writes nothing of the issue and
// leaves it for a later pass, once the block-index reconciliation has indexed the document
// (RestoreAndConvert).
//
// kept for one release: decisions-step
var errBlockIndexOwed = errors.New("docs: a document of the issue owes its block index, so the decisions step leaves the issue for a later pass")

// restoreIssueDecisions runs the third phase on issue in tx, which ctx joined with its writes run
// at once (joinAtOnce), so each read below sees every write made before it: the restore of work,
// the answered asks and the decisions document's ask blocks the finder read for it, and, where
// convert, the conversion of its decisions document and that document's retirement once it holds
// no section and no ask block. Under the issue's owner lock, it first answers errBlockIndexOwed,
// with nothing written, where any document of the issue that stands owes its block index: that
// document's document_blocks rows are not current, the finder leaves a spec that owes its index
// out, and the pass reads from those rows where each block of every document of the issue stands
// (readState, askBlockOf), so it would read an answered ask whose block such a document holds, or
// whose block left the spec, as standing in no document. Before it plans the restore, it holds
// each removal of an answered ask that a person's browser made in the decisions document's room
// and that no settlement has versioned yet (holdDecisionsRoomRemovals): the pass moves no such ask,
// and reports it declined. It writes nothing that it does not report, and its caller commits or
// rolls back: a pass that would take a section out of the decisions document without writing the
// reading it became fails (refuseUnwrittenReadings).
//
// kept for one release: decisions-step
func (s *Service) restoreIssueDecisions(ctx context.Context, tx pgx.Tx, issue string, work []graphcensus.RestoreWork, convert bool) (DecisionIssue, error) {
	result := DecisionIssue{Issue: issue}
	// The step's own write, which alone the restore's and the conversion's carry cases reach
	// (restoreCase, conversionCase): every version this pass writes is written as the step's own,
	// with what this pass added to that document (writeVersion, stepCasesFor).
	ctx = withOwnerVerified(ctx)
	r := &issueRestore{
		service: s, tx: tx, issue: issue, convert: convert, result: &result, present: map[string]*presentEdits{},
		shapedAdded: map[string]map[string][]string{}, readingsWritten: map[string]map[string]bool{},
	}
	if err := tx.QueryRow(ctx, `select now()`).Scan(&r.at); err != nil {
		return result, fmt.Errorf("read the time of the restore of %s: %w", issue, err)
	}
	spec, found, err := readIssueDocument(ctx, tx, `
		select `+issueDocumentColumns+` from artifacts a
		where a.issue_key = $1 and a.is_primary and a.kind = 'doc' and a.retired_at is null
	`, issue)
	if err != nil {
		return result, fmt.Errorf("read the spec of %s: %w", issue, err)
	}
	if !found {
		return result, fmt.Errorf("issue %s holds no spec to restore its answered asks to", issue)
	}
	r.spec = spec
	decisions, found, err := readIssueDocument(ctx, tx, `select `+issueDocumentColumns+` `+refs.DecisionsDocumentOf("$1"), issue)
	if err != nil {
		return result, fmt.Errorf("read the decisions document of %s: %w", issue, err)
	}
	if found {
		r.decisions = &decisions
	}
	// The issue's owner lock first, as every writer of its documents takes it (lockArtifactOwner),
	// then the rows of its asks, then each document's live write: the decisions document's first,
	// which the read of its room's removals opens (holdDecisionsRoomRemovals), then the spec's and
	// each other document's.
	if _, _, err := lockArtifactOwner(ctx, tx, spec.id); err != nil {
		return result, err
	}
	var owing string
	if err := tx.QueryRow(ctx, `
		select coalesce((select a.ref_key from artifacts a
		                 join document_block_versions owed on owed.artifact_id = a.id and owed.version is null
		                 where a.issue_key = $1 and a.kind = 'doc' and a.retired_at is null
		                 order by a.created_at, a.id limit 1), '')
	`, issue).Scan(&owing); err != nil {
		return result, fmt.Errorf("read whether the documents of %s owe their block index: %w", issue, err)
	}
	if owing != "" {
		return result, fmt.Errorf("%w: %s owes its block index", errBlockIndexOwed, owing)
	}
	if err := r.readState(ctx); err != nil {
		return result, err
	}
	if r.decisions != nil {
		if err := r.holdDecisionsRoomRemovals(ctx); err != nil {
			return result, err
		}
	}
	if err := r.planRestores(ctx, work); err != nil {
		return result, err
	}
	specApproval, err := newestApproval(ctx, tx, spec.id)
	if err != nil {
		return result, err
	}
	var decisionsApproval *model.SupersededApproval
	if r.decisions != nil {
		if decisionsApproval, err = standingApproval(ctx, tx, r.decisions.id); err != nil {
			return result, err
		}
	}
	if err := r.moveRows(ctx); err != nil {
		return result, err
	}
	var cause int64 = graphstore.NoEvent
	if r.decisions != nil {
		event, err := r.writeDecisionsDocument(ctx)
		if err != nil {
			return result, err
		}
		cause = event
	}
	if event, err := r.writeSpec(ctx); err != nil {
		return result, err
	} else if event != graphstore.NoEvent {
		cause = event
	}
	if err := r.writePresentDocuments(ctx); err != nil {
		return result, err
	}
	if err := r.refuseUnwrittenReadings(); err != nil {
		return result, err
	}
	if err := r.writeFacts(ctx, cause); err != nil {
		return result, err
	}
	if err := r.reportApprovals(ctx, specApproval, decisionsApproval); err != nil {
		return result, err
	}
	if r.convert && r.decisions != nil && r.sectionsLeft == 0 && !r.decisionsHoldAsks {
		retirement, err := RetireDocument(ctx, tx, r.decisions.id, DecisionStepActor,
			"The decisions step converted every section of the issue's decisions document into the spec's decisions and readings, and moved every ask block it held into the spec.")
		if err != nil {
			return result, err
		}
		eventSink(ctx)(retirement.Event)
		if retirement.SupersededApproval != nil {
			result.Superseded = append(result.Superseded, DecisionSupersededApproval{
				Document: r.decisions.address, Approval: *retirement.SupersededApproval,
			})
		}
		result.Retired = true
	}
	return result, nil
}

// readState reads, in the issue's transaction, the rows of every question ask of the issue,
// locked as a writer of an ask's row takes it (`for no key update`), which of them hold an answer
// on row and node (answered), which of them a person removed from the document its row names
// (removed: their node's newest removed_by asserts), every document of the issue that stands, and
// the blocks their block indexes hold: every block of the spec, and every ask block of each.
//
// kept for one release: decisions-step
func (r *issueRestore) readState(ctx context.Context) error {
	rows, err := r.tx.Query(ctx, `
		select `+AskColumns+` from asks a
		where a.issue_key = $1 and a.kind = 'question'
		order by a.id
		for no key update
	`, r.issue)
	if err != nil {
		return fmt.Errorf("read the asks of %s: %w", r.issue, err)
	}
	r.rows = map[string]model.Ask{}
	for rows.Next() {
		ask, err := ScanAsk(rows)
		if err != nil {
			rows.Close()
			return fmt.Errorf("read an ask of %s: %w", r.issue, err)
		}
		r.rows[ask.ID] = ask
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the asks of %s: %w", r.issue, err)
	}
	removed, err := r.tx.Query(ctx, `
		select k.id::text from asks k
		where k.issue_key = $1 and k.kind = 'question' and `+graphcensus.RemovedByAPerson, r.issue)
	if err != nil {
		return fmt.Errorf("read the asks a person removed of %s: %w", r.issue, err)
	}
	removedIDs, err := pgx.CollectRows(removed, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the asks a person removed of %s: %w", r.issue, err)
	}
	r.removed = make(map[string]bool, len(removedIDs))
	for _, id := range removedIDs {
		r.removed[id] = true
	}
	var answeredRows []string
	for id, ask := range r.rows {
		if ask.State == asks.StateAnswered {
			answeredRows = append(answeredRows, id)
		}
	}
	nodes, err := askAnswers(ctx, r.tx, answeredRows)
	if err != nil {
		return err
	}
	r.answered = map[string]bool{}
	for id, answer := range nodes {
		r.answered[id] = answer.answered
	}
	documents, err := r.tx.Query(ctx, `
		select `+issueDocumentColumns+` from artifacts a
		where a.issue_key = $1 and a.kind = 'doc' and a.retired_at is null
		order by a.created_at, a.id
	`, r.issue)
	if err != nil {
		return fmt.Errorf("read the documents of %s: %w", r.issue, err)
	}
	r.documents = map[string]issueDocument{}
	for documents.Next() {
		document, err := scanIssueDocument(documents)
		if err != nil {
			documents.Close()
			return fmt.Errorf("read a document of %s: %w", r.issue, err)
		}
		r.documents[document.id] = document
	}
	documents.Close()
	if err := documents.Err(); err != nil {
		return fmt.Errorf("read the documents of %s: %w", r.issue, err)
	}
	blocks, err := r.tx.Query(ctx, `
		select b.artifact_id::text, b.block_id, b.type from document_blocks b
		join artifacts a on a.id = b.artifact_id
		where a.issue_key = $1 and a.kind = 'doc' and a.retired_at is null and (b.artifact_id = $2::uuid or b.type = 'ask')
	`, r.issue, r.spec.id)
	if err != nil {
		return fmt.Errorf("read the blocks of the documents of %s: %w", r.issue, err)
	}
	r.specBlocks, r.askBlocks = map[string]bool{}, map[string]map[string]bool{}
	for blocks.Next() {
		var artifact, id, typ string
		if err := blocks.Scan(&artifact, &id, &typ); err != nil {
			blocks.Close()
			return fmt.Errorf("read a block of the documents of %s: %w", r.issue, err)
		}
		if artifact == r.spec.id {
			r.specBlocks[id] = true
		}
		if typ == "ask" {
			if r.askBlocks[artifact] == nil {
				r.askBlocks[artifact] = map[string]bool{}
			}
			r.askBlocks[artifact][id] = true
		}
	}
	blocks.Close()
	return blocks.Err()
}

// holdDecisionsRoomRemovals holds the removals of answered asks that a person's browser made in
// the decisions document's room and that no settlement has versioned yet, before the pass plans
// what it moves (planRestores). The room's settlement waits a settle delay after the last edit.
// Until it runs, no version records such a deletion, the ask's node holds no removed_by, and the
// block index still holds the block, so the finder reads the ask as work of the second shape. The
// read is an operation of the pass's own live write of the document that changes nothing
// (applyLive): it reads the room as that write's version reads it (liveWrite.readTree,
// roomReadTree), and it opens the decisions document's live write before the spec's, in the
// issue's lock order. With the room's pending authors (pendingRoomAuthors), it holds each such
// removal on the transaction's ledger as the person those authors name, as every writer that
// versions a room in a settlement's place holds it (holdRoomRemovals). Each held ask whose node
// holds no removal yet goes in roomRemovals and in removed: the pass moves no block of it
// (planRestores, planDocumentMoves), its row keeps naming the decisions document, and the
// conversion reads it as removed (planSection). The version of the document the pass writes
// states the removal on the ask's node (writeDecisionsDocument, removalsOf). Authors that name no
// person hold nothing, so an agent's drop still moves. An ask whose node holds a removal already
// is declined with that removal (nodeRemoval).
//
// kept for one release: decisions-step
func (r *issueRestore) holdDecisionsRoomRemovals(ctx context.Context) error {
	var live *pmdoc.Node
	err := r.service.applyLive(ctx, r.decisions.id, DecisionStepActor, func(_ *crdt.Doc, current pmdoc.ValidDocument, _ func(func(*crdt.Transaction))) error {
		live = current.Node()
		return nil
	})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return err
	}
	authors, err := pendingRoomAuthors(ctx, r.tx, r.decisions.id, joinedLiveWrite(ctx, r.decisions.id))
	if err != nil {
		return err
	}
	if err := holdRoomRemovals(ctx, r.tx, r.decisions.id, live, authors); err != nil {
		return err
	}
	r.roomRemovals = map[string]string{}
	ledger := ledgerFrom(ctx)
	if ledger == nil {
		return nil
	}
	for ask, removal := range ledger.removals[r.decisions.id] {
		if !r.removed[ask] {
			r.roomRemovals[ask] = removal.person
			r.removed[ask] = true
		}
	}
	return nil
}

// planRestores turns the finder's work into the blocks the restore writes, each item read again
// under the issue's lock, since the finder read the whole pass's work before it took any: each
// ask still as the finder read it once its row is locked - its block where the finder saw it, and,
// for a returning ask, answered - keeps its block id unless a block of the spec holds that id as
// the spec's block index holds it now, when it takes a fresh one no document of the issue holds
// (R6's collision rule). An ask block of the decisions document moves whatever its ask's state
// (D1), and every one the document's block index holds moves, one no work item names among them
// (planDocumentMoves). A work item whose row moved since the finder read it is left: its block, if
// the document still holds it, moves as planDocumentMoves reads it under the lock. So is a
// returning ask whose block the spec holds again, which a person put back: a restore of it would
// write a second block of that ask and leave the person's with no row. An answered ask a person
// removed is never written into the spec, in either shape. One whose node holds an asserted
// removed_by since the finder read it is passed over: a returning ask a person took out of the
// spec, and an ask a person deleted from the decisions document while the step ran, which is
// declined with the removal its node holds (nodeRemoval), its row left naming the block it left.
// So is an ask whose block a person's browser deleted from the decisions document's room before a
// settlement versioned the deletion (roomRemovals): its row keeps naming the block it left, and
// writeDecisionsDocument declines it once the version it writes states the removal. A returning
// ask the version that left its block out shows a person removed is declined too (removalOf).
// writeFacts records each other decline on its node, and the result reports every decline. An
// ask the restore moves out of the decisions document moves from the address the finder read
// (RestoreWork.Address), which its moved_to and its node's relations name whether or not the
// document still holds the block (writeFacts). The read of who wrote the version that left a
// block out (removalOf) is the main shape's alone: a block the decisions document holds moves
// whoever put it there, since that document retires and the block has nowhere else to stand.
//
// kept for one release: decisions-step
func (r *issueRestore) planRestores(ctx context.Context, work []graphcensus.RestoreWork) error {
	for _, item := range work {
		row, held := r.rows[item.Ask]
		if !held || row.BlockID == nil || *row.BlockID != item.BlockID || row.BlockArtifactID == nil ||
			item.Shape == graphcensus.RestoreReturns && row.State != asks.StateAnswered {
			slog.Warn("dispatch: the decisions step leaves an ask whose row moved since it was read to its next pass",
				"issue", r.issue, "ask", item.Ask)
			continue
		}
		restore := &restoredBlock{work: item, row: row, id: item.BlockID}
		switch item.Shape {
		case graphcensus.RestoreReturns:
			if *row.BlockArtifactID != r.spec.id {
				continue
			}
			if r.askBlocks[r.spec.id][item.BlockID] {
				slog.Warn("dispatch: the decisions step leaves an ask whose block its spec holds again since it was read",
					"issue", r.issue, "ask", item.Ask)
				continue
			}
			if r.removed[item.Ask] {
				continue
			}
			declined, read, held, err := r.removalOf(ctx, item.Ask, item.BlockID)
			if err != nil {
				return err
			}
			if declined != nil {
				r.decline(declined)
				continue
			}
			restore.read, restore.held = read, held
		case graphcensus.RestoreMoves:
			if r.decisions == nil || *row.BlockArtifactID != r.decisions.id {
				continue
			}
			if r.removed[item.Ask] {
				if _, held := r.roomRemovals[item.Ask]; held {
					continue
				}
				declined, err := r.nodeRemoval(ctx, item.Ask)
				if err != nil {
					return err
				}
				r.decline(declined)
				continue
			}
			restore.from = item.Address
		}
		if r.specBlocks[item.BlockID] || r.specRowHolds(item.BlockID, row.ID) {
			id, err := r.freshBlockID(ctx)
			if err != nil {
				return err
			}
			restore.id = id
		}
		r.restores = append(r.restores, restore)
	}
	if r.decisions != nil {
		if err := r.planDocumentMoves(ctx); err != nil {
			return err
		}
	}
	// Answer-time order, as the restored blocks stand in the spec's `## Decisions` part.
	sort.SliceStable(r.restores, func(a, b int) bool {
		return answeredAt(r.restores[a].row).Before(answeredAt(r.restores[b].row))
	})
	return nil
}

// planDocumentMoves adds a move of the second shape for each ask block the decisions document's
// block index holds (document_blocks of type ask, read under the issue's lock: readState) that no
// restore of this pass takes already (movesBlock): R9's first further kind, a section whose body
// still holds an ask block, has every such block moved first. A block whose row moved since the
// finder read it moves as the block of the row that names it now, from its address in the
// document; a block no row names moves as a block with no row to rebind (rowless), written as the
// document's live tree holds it, from its address there, where the tree still holds it
// (planSections). Each keeps its block id unless the spec holds it, when it takes a fresh one (R6's
// collision rule). A block no row names whose id the spec holds as an ask block - a copy of the
// spec's ask, which has no ask of its own - joins that block instead: no block is written, since
// under a fresh id the spec's next settlement would open it as a new ask beside the one it copies,
// and its address takes its one moved_to to the spec's block, which answers the live ask. A block
// whose row names an ask a person removed (removed) does not move: its ask is never written into
// the spec (planRestores), and a deletion of its block in the room that no settlement has
// versioned is among those (holdDecisionsRoomRemovals).
//
// kept for one release: decisions-step
func (r *issueRestore) planDocumentMoves(ctx context.Context) error {
	for _, id := range slices.Sorted(maps.Keys(r.askBlocks[r.decisions.id])) {
		if r.movesBlock(id) {
			continue
		}
		row, named := r.decisionsRow(id)
		if named && r.removed[row.ID] {
			continue
		}
		restore := &restoredBlock{id: id, work: graphcensus.RestoreWork{
			Shape: graphcensus.RestoreMoves, Issue: r.issue, BlockID: id, Spec: r.spec.refKey,
		}}
		if named {
			restore.row, restore.work.Ask = row, row.ID
			restore.from = refs.BlockAddress(r.decisions.refKey, id)
			restore.work.Address = restore.from
		} else {
			restore.rowless = true
		}
		switch {
		case restore.rowless && r.askBlocks[r.spec.id][id]:
			restore.joins = true
		case r.specBlocks[id] || r.specRowHolds(id, restore.row.ID):
			fresh, err := r.freshBlockID(ctx)
			if err != nil {
				return err
			}
			restore.id = fresh
		}
		r.restores = append(r.restores, restore)
	}
	return nil
}

// movesBlock reports whether a restore of this pass takes the decisions document's block id
// already: a move of the second shape, or a returning ask whose copy the document holds under
// that id, which no row of the document names (planSections).
//
// kept for one release: decisions-step
func (r *issueRestore) movesBlock(id string) bool {
	for _, restore := range r.restores {
		if restore.work.BlockID == id && (restore.work.Shape == graphcensus.RestoreMoves || !r.ownedByDecisions(id)) {
			return true
		}
	}
	return false
}

// decisionsRow is the row of the ask of the issue that names block id of the decisions document.
//
// kept for one release: decisions-step
func (r *issueRestore) decisionsRow(id string) (model.Ask, bool) {
	for _, row := range r.rows {
		if row.BlockArtifactID != nil && *row.BlockArtifactID == r.decisions.id && row.BlockID != nil && *row.BlockID == id {
			return row, true
		}
	}
	return model.Ask{}, false
}

// removalOf reads who took ask's block, blockID, out of the spec (readSpecRemoval). Where that is a
// person it answers the decline, with the version that left the block out, its time and the event
// that recorded it (versionEventID), which the decline records on the ask's node. Otherwise it
// answers what it read and the block as the newest version holding it held it, and the ask is
// restored.
//
// kept for one release: decisions-step
func (r *issueRestore) removalOf(ctx context.Context, ask, blockID string) (*declinedAsk, string, *pmdoc.Node, error) {
	removal, err := readSpecRemoval(ctx, r.tx, r.issue, r.spec.id, r.rows[ask], blockID)
	if err != nil {
		return nil, "", nil, err
	}
	if removal.person == "" {
		return nil, removal.read, removal.held, nil
	}
	event, err := versionEventID(ctx, r.tx, artifactOwner{IssueKey: &r.issue}, r.spec.id, removal.version)
	if err != nil {
		return nil, "", nil, err
	}
	return &declinedAsk{ask: ask, person: removal.person, version: removal.version, at: removal.at, event: event}, "", nil, nil
}

// decline records declined, an answered ask the pass does not write into the spec because a
// person removed its block: writeFacts records the removal on its node, the conversion reads the
// ask as removed (planSection), and the result, which the step logs, reports it.
//
// kept for one release: decisions-step
func (r *issueRestore) decline(declined *declinedAsk) {
	r.declined = append(r.declined, declined)
	r.removed[declined.ask] = true
	r.result.Declined = append(r.result.Declined, DeclinedAsk{Ask: declined.ask, Person: declined.person, Version: declined.version})
}

// nodeRemoval is the decline of ask, an ask of the second shape whose node holds an asserted
// removed_by (readState's removed): a person deleted its block from the decisions document since
// the finder read it. It is that removal as the node's newest removed_by states it - the person,
// the time and the event, the artifact.version of the person's version that left the block out -
// with that version's number, 0 where the event names none. writeFacts states the removal again
// under that event, which appends nothing: the store's repeat key (graph_tuples_once) holds it
// already, so the removal stands as the person's write stated it.
//
// kept for one release: decisions-step
func (r *issueRestore) nodeRemoval(ctx context.Context, ask string) (*declinedAsk, error) {
	declined := &declinedAsk{ask: ask}
	if err := r.tx.QueryRow(ctx, `
		select newest.to_id, newest.at, newest.source_event, coalesce((e.payload->'version'->>'number')::int, 0)
		from (
			select t.to_id, t.at, t.source_event from graph_tuples t
			where t.from_kind = $1 and t.from_id = $2 and t.kind = $3 and t.to_kind <> $4
			order by t.seq desc limit 1
		) newest
		left join events e on e.id = newest.source_event and e.type = 'artifact.version'
	`, graphstore.KindAsk, ask, graphstore.TupleRemovedBy, graphstore.KindValue).Scan(
		&declined.person, &declined.at, &declined.event, &declined.version); err != nil {
		return nil, fmt.Errorf("read the removal the node of ask %s holds: %w", ask, err)
	}
	return declined, nil
}

// specRemoval is who took an ask's block out of its spec, as readSpecRemoval reads it: the person
// whose version left it out, that version and its time, person "" where no person did; and
// otherwise what the read found (read, one of the restoreRead reasons) and the block as the newest
// version holding it held it (held), nil where none did.
type specRemoval struct {
	person  string
	version int
	at      time.Time
	read    string
	held    *pmdoc.Node
}

// readSpecRemoval reads, through q, who took ask's block, blockID, out of the spec specID of
// issue: the version that left it out, the lowest above the newest whose stored tree holds an ask
// block with that id, read newest first through the stored-version reader the approval carry
// reads (approvedVersion), a version it cannot read holding none. Where that version is a
// person's (versionPerson) it names that person, the version and its time. Otherwise it names what
// it read, and the ask is restored: that version's authors name no person and no agent acting
// with the person scope, no version of the spec held the block, or that version names no author;
// with the block as the newest version holding it held it, nil where none did, whose bytes the
// restore writes back (restoredAskBlock). A version a person's own write wrote is the person's
// whatever other authors it lists, and one a session wrote with no act of a person - every
// agent's drop the restore exists to undo, the release before's bearer writes among them - is no
// person's.
//
// The ask's answerer is no remover in the one version that first holds her answer: the version
// written after the ask's answered_at whose previous version was written before it. The release
// before wrote an answer into its block in the live document in the answerer's name, so the first
// version after the answer credits her whatever else it holds, an agent's drop of the block among
// it (answerersVersion). In every later version she counts as any person does. This build's own
// settlement records a person's removal of an answered ask's block as the version is written
// (holdRoomRemovals), and so does every write that versions the room in a settlement's place - an
// upload, a snapshot, a named version - so a version this build wrote with the block gone, its
// answerer among the room's authors, has its ask declined before this reads it (readState's
// removed).
//
// It is the one test of a person's removal, which the restore reads (removalOf) and the store
// census's dangling counter reads its returning set by (ReturnDeclined). It writes nothing.
func readSpecRemoval(ctx context.Context, q Queryer, issue, specID string, ask model.Ask, blockID string) (specRemoval, error) {
	rows, err := q.Query(ctx, `select number from artifact_versions where artifact_id = $1::uuid order by number desc`, specID)
	if err != nil {
		return specRemoval{}, fmt.Errorf("read the versions of the spec of %s: %w", issue, err)
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return specRemoval{}, fmt.Errorf("read the versions of the spec of %s: %w", issue, err)
	}
	removing, holder := 0, 0
	var held *pmdoc.Node
	for _, number := range numbers {
		tree, _, readable, err := approvedVersion(ctx, q, specID, number)
		if err != nil {
			return specRemoval{}, err
		}
		if readable {
			if held = askBlockIn(tree, blockID); held != nil {
				holder = number
				break
			}
		}
		removing = number
	}
	switch {
	case holder == 0:
		return specRemoval{read: restoreReadNeverHeld}, nil
	case removing == 0:
		// The newest version holds the block: no version left it out.
		return specRemoval{read: restoreReadNoPerson, held: held}, nil
	}
	var authors []model.Actor
	var at, previousAt time.Time
	if err := q.QueryRow(ctx, `
		select v.authors, v.created_at, previous.created_at
		from artifact_versions v, artifact_versions previous
		where v.artifact_id = $1::uuid and v.number = $2 and previous.artifact_id = $1::uuid and previous.number = $3
	`, specID, removing, holder).Scan(&authors, &at, &previousAt); err != nil {
		return specRemoval{}, fmt.Errorf("read the authors of version %d of the spec of %s: %w", removing, issue, err)
	}
	if len(authors) == 0 {
		return specRemoval{read: restoreReadNoAuthors, held: held}, nil
	}
	person, _ := versionPerson(authors, answerersVersion(ask, at, previousAt))
	if person == "" {
		return specRemoval{read: restoreReadNoPerson, held: held}, nil
	}
	return specRemoval{person: person, version: removing, at: at}, nil
}

// ReturnDeclined answers whether the restore declines to write back the ask of work, a return of
// the main shape (graphcensus.RestoreReturns): whether a person took its block out of the spec, as
// the restore reads it before it writes one back (readSpecRemoval, removalOf). It reads the ask's
// row and the spec work names through q and writes nothing, so the store census runs it in its
// read-only snapshot (graphcensus.ReturnDeclined), and the dangling counter's returning set holds
// only the asks the restore writes back.
func ReturnDeclined(ctx context.Context, q graphstore.Queryer, work graphcensus.RestoreWork) (bool, error) {
	var specID string
	ask, err := ScanAsk(q.QueryRow(ctx, `
		select `+AskColumns+`, spec.id::text from asks a
		join artifacts spec on spec.ref_key = $2
		where a.id = $1::uuid`, work.Ask, work.Spec), &specID)
	if err != nil {
		return false, fmt.Errorf("read the ask %s the restore returns to %s: %w", work.Ask, work.Spec, err)
	}
	removal, err := readSpecRemoval(ctx, q, work.Issue, specID, ask, work.BlockID)
	if err != nil {
		return false, err
	}
	return removal.person != "", nil
}

// answerersVersion is the login of ask's answerer where a version written at at, whose previous
// version was written at previousAt, is the one that first holds the answer - written after the
// answer's time, its previous version before it - and "" otherwise (removalOf). Its one caller,
// readSpecRemoval, serves the store census's dangling counter as well as the restore
// (ReturnDeclined).
func answerersVersion(ask model.Ask, at, previousAt time.Time) string {
	if ask.Answer == nil || !at.After(ask.Answer.At) || !previousAt.Before(ask.Answer.At) {
		return ""
	}
	return model.CanonicalLogin(ask.Answer.User)
}

// versionPerson is the person a version's authors name, "" where they name none, and the author
// that names them: the first author that is a person (kind user, a browser's person, or an agent
// acting for one through a route that takes the person scope) or an agent whose recorded act
// holds the person scope (a broker-issued token its person scoped), named as that author's person
// (personOf), other than excluded, the answerer in the version that first holds her answer
// (answerersVersion).
func versionPerson(authors []model.Actor, excluded string) (string, model.Actor) {
	for _, author := range authors {
		if author.Kind != "user" && (author.Act == nil || !slices.Contains(author.Act.Scopes, model.ScopePerson)) {
			continue
		}
		if login := personOf(author); login != "" && login != excluded && graphstore.AddressFits(login) {
			return login, author
		}
	}
	return "", model.Actor{}
}

// askBlockIn is the ask block tree holds under id, nil where it holds none.
func askBlockIn(tree *pmdoc.Node, id string) *pmdoc.Node {
	var held *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if blockID, _ := node.Attrs[pmdoc.BlockIDAttr].(string); node.Type == "ask" && blockID == id {
			held = node
		}
		return held == nil
	})
	return held
}

// specRowHolds reports whether the row of an ask of the issue other than ask names the spec's
// block id, which asks_block_id_unique keeps to one row: an ask a person declined or a settlement
// retracted keeps its row naming the block it left, so the restored ask takes a fresh id there as
// it does where a live block of the spec holds the id (R6).
//
// kept for one release: decisions-step
func (r *issueRestore) specRowHolds(id, ask string) bool {
	for _, row := range r.rows {
		if row.ID != ask && row.BlockArtifactID != nil && *row.BlockArtifactID == r.spec.id && row.BlockID != nil && *row.BlockID == id {
			return true
		}
	}
	return false
}

// answeredAt is when ask was answered, the zero time for an ask its row holds no answer of.
//
// kept for one release: decisions-step
func answeredAt(ask model.Ask) time.Time {
	if ask.Answer == nil {
		return time.Time{}
	}
	return ask.Answer.At
}

// freshBlockID is a block id no document of the issue holds, as a block or as an ask's row.
//
// kept for one release: decisions-step
func (r *issueRestore) freshBlockID(ctx context.Context) (string, error) {
	for {
		id := uuid.NewString()
		var held bool
		if err := r.tx.QueryRow(ctx, `
			select exists (
				select 1 from document_blocks b join artifacts a on a.id = b.artifact_id
				where a.issue_key = $1 and b.block_id = $2
			) or exists (select 1 from asks k where k.issue_key = $1 and k.block_id = $2)
		`, r.issue, id).Scan(&held); err != nil {
			return "", fmt.Errorf("read whether a document of %s holds a block id: %w", r.issue, err)
		}
		if !held {
			return id, nil
		}
	}
}

// moveRows points each restored ask's row at its block in the spec, before either document is
// written, so the spec's write reads the ask it holds as the spec's own (refuseReadingsWithoutAnswers,
// the typed-node pass) and the decisions document's no longer names it. A block no row names has
// none to point.
//
// kept for one release: decisions-step
func (r *issueRestore) moveRows(ctx context.Context) error {
	for _, restore := range r.restores {
		if restore.rowless || *restore.row.BlockID == restore.id && *restore.row.BlockArtifactID == r.spec.id {
			continue
		}
		if _, err := r.tx.Exec(ctx, `update asks set block_id = $2, block_artifact_id = $3::uuid where id = $1::uuid`,
			restore.row.ID, restore.id, r.spec.id); err != nil {
			return fmt.Errorf("point ask %s at its block in the spec of %s: %w", restore.row.ID, r.issue, err)
		}
		restore.row.BlockID, restore.row.BlockArtifactID = new(restore.id), new(r.spec.id)
		r.rows[restore.row.ID] = restore.row
	}
	return nil
}

// restoreOf is the restore of ask, nil where this pass restores none.
//
// kept for one release: decisions-step
func (r *issueRestore) restoreOf(ask string) *restoredBlock {
	for _, restore := range r.restores {
		if !restore.rowless && restore.row.ID == ask {
			return restore
		}
	}
	return nil
}

// askBlockOf is the document that holds ask's block and the block's id, and whether it has one:
// the spec, for an ask this pass restores; else the document of the issue its row names, where
// that document's block index holds the block as an ask block (R9: the ask's "restored or present"
// block). The decisions document is none of them: an ask its row names there is the restore's,
// which moves it into the spec whatever its state.
//
// kept for one release: decisions-step
func (r *issueRestore) askBlockOf(ask string) (issueDocument, string, bool) {
	if restore := r.restoreOf(ask); restore != nil {
		return r.spec, restore.id, true
	}
	row, held := r.rows[ask]
	if !held || row.State != asks.StateAnswered || row.BlockID == nil || row.BlockArtifactID == nil ||
		r.decisions != nil && *row.BlockArtifactID == r.decisions.id || !r.askBlocks[*row.BlockArtifactID][*row.BlockID] {
		return issueDocument{}, "", false
	}
	document, stands := r.documents[*row.BlockArtifactID]
	if !stands {
		return issueDocument{}, "", false
	}
	return document, *row.BlockID, true
}

// writeDecisionsDocument writes the decisions document: it takes out of it each ask block the
// restore moves (shape (b), and a copy of an ask of the main shape that the document holds) and
// each section the pass converts, as this pass reads the document's sections (planSections), and
// writes its version. The same write takes every attribute a version can store off each ask block
// the document keeps that has no ask row and is no copy of an ask (withoutAsklessAskAttributes),
// so its version holds none of the eight there (R10): the phase runs on a fleet of one build alone
// (restoreIssue). It records whether the document still holds an ask block once those are taken
// out (decisionsHoldAsks), which keeps it from its retirement. Where the pass holds a person's
// removal from the document's room (roomRemovals), it writes the version even where it takes no
// block out. That version states each such removal on the ask's node (removalsOf), so a conversion
// that then retires the document never retires it with the deletion unstated. It reports each such
// ask declined, to the person and at that version. A pass that holds such a removal and writes no
// version fails, so no pass commits the removal unstated. It answers the event of that version,
// NoEvent where it wrote none.
//
// kept for one release: decisions-step
func (r *issueRestore) writeDecisionsDocument(ctx context.Context) (int64, error) {
	var drop map[string]bool
	dropped := func(node *pmdoc.Node) bool {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		return drop[id]
	}
	err := r.service.applyLive(ctx, r.decisions.id, DecisionStepActor, func(doc *crdt.Doc, current pmdoc.ValidDocument, _ func(func(*crdt.Transaction))) error {
		tree := current.Node()
		if tree == nil {
			return fmt.Errorf("the decisions document of %s is outside the Proof schema, so the decisions step cannot read its sections", r.issue)
		}
		var err error
		if drop, err = r.planSections(ctx, tree); err != nil {
			return err
		}
		r.decisionsDropped = drop
		left, _ := withoutBlocks(tree, dropped)
		r.decisionsHoldAsks = pmdoc.HoldsAskBlock(left)
		if len(drop) == 0 {
			return nil
		}
		// kept for one release: stored-ask-state
		withAsk, err := askBlocksWithAnAsk(ctx, r.tx, r.decisions.id, tree)
		if err != nil {
			return err
		}
		_, err = rewriteLive(doc, nil, func(live *pmdoc.Node) bool {
			next, took := withoutBlocks(live, dropped)
			if !took {
				return false
			}
			live.Children = next.Children
			withoutAsklessAskAttributes(live, withAsk)
			return true
		})
		return err
	})
	if errors.Is(err, websocket.ErrNoChanges) || err == nil && len(drop) == 0 {
		if len(r.roomRemovals) == 0 {
			return graphstore.NoEvent, nil
		}
	} else if err != nil {
		return graphstore.NoEvent, err
	}
	event, err := r.writeVersion(ctx, *r.decisions, &r.result.DecisionsVersion)
	if err != nil || len(r.roomRemovals) == 0 {
		return event, err
	}
	if event == graphstore.NoEvent {
		return graphstore.NoEvent, fmt.Errorf("the decisions step holds a person's removal of an answered ask from the decisions document of %s, and wrote no version of that document to state it", r.issue)
	}
	for _, ask := range slices.Sorted(maps.Keys(r.roomRemovals)) {
		r.result.Declined = append(r.result.Declined, DeclinedAsk{Ask: ask, Person: r.roomRemovals[ask], Version: r.result.DecisionsVersion})
	}
	return event, nil
}

// ownedByDecisions reports whether an ask names a block of the decisions document under id as its
// own: an ask this pass moves out of it, or one whose row names that document.
//
// kept for one release: decisions-step
func (r *issueRestore) ownedByDecisions(id string) bool {
	for _, restore := range r.restores {
		if restore.work.Shape == graphcensus.RestoreMoves && restore.work.BlockID == id {
			return true
		}
	}
	_, named := r.decisionsRow(id)
	return named
}

// planSections reads tree, the decisions document as its live write holds it, into what this
// pass does with it, and answers the block ids the document loses: each ask block the restore
// moves into the spec, and every block of each section the pass converts.
//
// kept for one release: decisions-step
func (r *issueRestore) planSections(ctx context.Context, tree *pmdoc.Node) (map[string]bool, error) {
	drop := map[string]bool{}
	movedBy := map[string]*restoredBlock{}
	for _, restore := range r.restores {
		if restore.work.Shape == graphcensus.RestoreReturns && r.ownedByDecisions(restore.work.BlockID) {
			// The block the document holds under that id is an ask of its own, which its own
			// work item moves; it is no copy of this one.
			continue
		}
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			if node.Type != "ask" || id != restore.work.BlockID || restore.moved != nil {
				return true
			}
			// The ask's own block of the second shape, or a copy of an ask of the main shape the
			// document holds under that ask's block id: either moves into the spec.
			restore.moved = node
			if restore.from == "" {
				restore.from = refs.BlockAddress(r.decisions.refKey, id)
			}
			movedBy[id] = restore
			drop[id] = true
			return false
		})
	}
	// A block no row names moves only where the live tree still holds it: nothing else holds what
	// to write.
	r.restores = slices.DeleteFunc(r.restores, func(restore *restoredBlock) bool {
		return restore.rowless && restore.moved == nil
	})
	sections := decisionSectionsOf(tree)
	ties, err := readSectionTies(ctx, r.tx, r.decisions.id, r.spec.id)
	if err != nil {
		return nil, err
	}
	if r.indexedBlocks, err = refs.BlockChanges(ctx, r.tx, r.decisions.id); err != nil {
		return nil, err
	}
	var indexed *int
	if err := r.tx.QueryRow(ctx, `select version from document_block_versions where artifact_id = $1::uuid`,
		r.decisions.id).Scan(&indexed); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read the version the block index of the decisions document of %s stands at: %w", r.issue, err)
	}
	if indexed != nil {
		r.indexedVersion = *indexed
	}
	authors, err := r.sectionAuthors(ctx, sections)
	if err != nil {
		return nil, err
	}
	keeps, err := r.sectionKeeps(ctx, tree, sections)
	if err != nil {
		return nil, err
	}
	for index, section := range sections {
		converted := &convertedSection{section: section, author: authors[index], keep: keeps[section.headingID()]}
		r.sections = append(r.sections, converted)
		if err := r.planSection(ctx, converted, ties, movedBy); err != nil {
			return nil, err
		}
		switch converted.fate {
		case fateUntouched, fateLeft:
			r.sectionsLeft++
			if converted.fate == fateLeft {
				r.result.Left++
			}
			continue
		case fateDecision:
			r.result.Decisions++
		case fateReading:
			r.result.Readings++
		case fateDropped:
			r.result.Dropped++
		case fateRemovedAsk:
			r.result.Removed++
		}
		for _, id := range section.blockIDs() {
			drop[id] = true
		}
	}
	return drop, nil
}

// planSection decides what becomes of one section (R9), and records the spec blocks it links as
// the `shaped` pointers of the ask it restates. It is the one place that orders R9's rules.
//
// Every ask block the section holds, wherever in its body, moves into the spec first (the second
// shape of the restore, which moves every ask block of the document: planDocumentMoves), and the
// rest of the section converts by the rules below. The asks a section is about are the asks whose
// blocks it held and the answered asks it cites. A section that cites one answered ask a person
// removed from its spec (removed: its node holds an asserted removed_by, or this pass declined to
// restore it) is deleted by R9's rule for it (fateRemovedAsk): the person removed the decision it
// reads, so no block takes its links or its words, which stay in the decisions document's
// versions, and its heading moves to the ask itself (writeFacts). A section under `## Replaced`
// otherwise becomes a decision with replaced="true" whichever ask it is about (planReplaced): that
// rule comes before the reading rule. A section about one answered ask restates it
// (planRestating): that ask's block - the one the restore writes into the spec, or the one a
// document of the issue holds already (askBlockOf) - takes the section's links as `shaped`
// pointers, and the section's body becomes a reading in it; a body that is the ask's answer and
// nothing more, or nothing at all, is dropped, and writes no reading (the plain-answer rule). A
// section about no answered ask becomes a decision, and one that held one ask block holding no
// answer and nothing else is dropped, its heading moving to that block.
//
// R9's further kinds each become a decision that waits for a person's review (planUnplaced): a
// section about more than one answered ask, under `## Replaced` or not, one whose answered ask has
// its block in no document of the issue (an ask of the issue no block holds), and one whose body a
// reading cannot hold (a reading holds paragraphs alone, and only in an answered ask: a reading
// reads an answer).
//
// A section whose body, the blocks that move taken out, still holds an ask block - one the
// document's block index does not hold yet, which no move takes - is left where it is: a
// conversion of it would take that block out of the document and write it nowhere (R6, R8). The
// next pass, once the index holds the block, moves it and converts the section.
//
// A restore alone converts only a section that holds a block it moves, so no section is left
// holding nothing but its heading, and it writes no decision, since only the conversion's case
// carries an approval across one (conversionCase); it still gives a restored answered ask the
// links of every section that restates it, as R6 says. It converts a section into a reading only
// of an ask it restores, whose block takes the section's links beside it: a section about an ask a
// document holds already is left, with its links, for the conversion, which gives them to that
// block (planRestating).
//
// kept for one release: decisions-step
func (r *issueRestore) planSection(ctx context.Context, converted *convertedSection, ties sectionTies, movedBy map[string]*restoredBlock) error {
	section := converted.section
	moving := func(node *pmdoc.Node) bool {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		_, moves := movedBy[id]
		return moves && node.Type == "ask"
	}
	var heldBlocks []*restoredBlock
	var held []string
	for _, block := range section.body {
		pmdoc.Walk(block, func(node *pmdoc.Node) bool {
			if moving(node) {
				id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
				heldBlocks = append(heldBlocks, movedBy[id])
				if !movedBy[id].rowless {
					held = append(held, movedBy[id].row.ID)
				}
			}
			return true
		})
	}
	body := section.body
	if len(heldBlocks) > 0 {
		left, _ := withoutBlocks(&pmdoc.Node{Type: "doc", Children: section.body}, moving)
		body = left.Children
	}
	if slices.ContainsFunc(body, pmdoc.HoldsAskBlock) {
		converted.fate = r.leftUnlessRestoring(len(heldBlocks) > 0)
		return nil
	}
	converted.links = tiesOf(section, ties.spec)
	about := slices.Clone(held)
	for _, ask := range tiesOf(section, ties.asks) {
		if row, found := r.rows[ask]; found && row.State == asks.StateAnswered {
			about = appendUnique(about, ask)
		}
	}
	var answered []string
	for _, ask := range about {
		if r.rows[ask].State == asks.StateAnswered {
			answered = append(answered, ask)
		}
	}
	unplaced := unplacedSection{asks: about, held: len(heldBlocks) > 0, mentions: ties.asks}
	switch {
	case len(answered) > 1:
		unplaced.cause = unplacedAsks
	case len(answered) == 1 && !slices.Contains(held, answered[0]) && r.removed[answered[0]]:
		converted.fate = r.leftUnlessRestoring(unplaced.held)
		if r.convert {
			converted.fate, converted.ask = fateRemovedAsk, answered[0]
		}
		return nil
	case section.replaced:
		return r.planReplaced(ctx, converted, answered, body, unplaced.held)
	case len(answered) == 1:
		return r.planRestating(ctx, converted, answered[0], body, unplaced)
	case len(heldBlocks) == 0:
		if !r.convert {
			return nil
		}
		return r.planDecision(ctx, converted, body, nil)
	case len(heldBlocks) == 1 && len(body) == 0:
		converted.fate, converted.ask = fateDropped, heldBlocks[0].row.ID
		converted.askDocument, converted.askBlock = r.spec, heldBlocks[0].id
		return nil
	default:
		unplaced.cause = unplacedBody
	}
	return r.planUnplaced(ctx, converted, body, unplaced)
}

// planRestating decides what becomes of converted, a section about one answered ask, ask, that no
// rule before the reading rule took (planSection), and whose body, the blocks that move taken out,
// is body.
//
// kept for one release: decisions-step
func (r *issueRestore) planRestating(ctx context.Context, converted *convertedSection, ask string, body []*pmdoc.Node, unplaced unplacedSection) error {
	document, blockID, stands := r.askBlockOf(ask)
	if !stands {
		unplaced.cause = unplacedNoBlock
		return r.planUnplaced(ctx, converted, body, unplaced)
	}
	answered := r.answered[ask]
	fate := fateReading
	switch {
	case len(body) == 0 || answered && plainAnswer(body, r.rows[ask]):
		fate = fateDropped
	case !answered || !allParagraphs(body):
		unplaced.cause = unplacedBody
		return r.planUnplaced(ctx, converted, body, unplaced)
	}
	// An ask that holds no answer takes no pointer: what an answer shaped is the answer's.
	restore := r.restoreOf(ask)
	switch {
	case !answered:
	case restore != nil:
		restore.shaped = appendUnique(restore.shaped, converted.links...)
	case r.convert:
		edits := r.presentIn(document.id)
		edits.shaped[blockID] = appendUnique(edits.shaped[blockID], converted.links...)
	}
	// A restore alone gives a section's links to no block it does not write: they are the
	// conversion's to give to the ask's present block, so a section about an ask this pass does not
	// restore keeps its links and its words where they stand for the conversion, even where it held
	// a block that moves.
	if !r.convert && restore == nil {
		converted.fate = r.leftUnlessRestoring(unplaced.held)
		return nil
	}
	if !r.convert && !unplaced.held {
		return nil
	}
	converted.fate, converted.ask = fate, ask
	converted.askDocument, converted.askBlock = document, blockID
	if fate != fateReading {
		return nil
	}
	reading, err := readingOf(body)
	if err != nil {
		return err
	}
	converted.reading = reading
	if restore != nil {
		restore.readings = append(restore.readings, reading)
	} else {
		edits := r.presentIn(document.id)
		edits.readings[blockID] = append(edits.readings[blockID], reading)
	}
	return nil
}

// planReplaced makes converted, a section under `## Replaced` about at most one answered ask
// (answered), a decision with replaced="true" (planDecision), whatever its body holds: R9's rule
// for such a section comes before its reading rule. Its decision keeps the section's heading block
// id and takes its links as `shaped`, as every section that becomes a decision does, and settles
// the one answered ask it is about, where it is about one, as its from_ask. It writes no reading
// under that ask and gives the ask none of the section's links, and its heading moves to the
// decision, not to the ask (writeFacts). A restore alone writes no decision, and leaves the section
// to the conversion.
//
// kept for one release: decisions-step
func (r *issueRestore) planReplaced(ctx context.Context, converted *convertedSection, answered []string, body []*pmdoc.Node, held bool) error {
	if !r.convert {
		converted.fate = r.leftUnlessRestoring(held)
		return nil
	}
	if len(answered) == 1 {
		converted.ask = answered[0]
	}
	return r.planDecision(ctx, converted, body, nil)
}

// unplacedSection is a section the conversion could not place under one ask: why (cause), the
// asks it is about - the asks whose blocks it held and the answered asks it cites - whether it held
// an ask block, which moved into the spec, and the asks each block of the decisions document cites
// (sectionTies.asks).
//
// kept for one release: decisions-step
type unplacedSection struct {
	cause    string
	asks     []string
	held     bool
	mentions map[string][]string
}

// Why the conversion could not place a section under one ask (unplacedReason).
//
// kept for one release: decisions-step
const (
	unplacedAsks    = "it cites more than one answered ask"
	unplacedNoBlock = "the answered ask it cites has its block in no document of the issue"
	unplacedBody    = "its body is no reading one answered ask can hold"
)

// unplacedReason is the uncertainty_reason of the decision a section becomes that the conversion
// could not place under one ask for cause.
//
// kept for one release: decisions-step
func unplacedReason(cause string) string {
	return "The conversion of the decisions document could not place this section under one ask: " + cause +
		". It waits for a person's review."
}

// planUnplaced makes converted, a section the conversion could not place under one ask, a decision
// that waits for a person's review (planDecision). A restore alone writes no decision, and leaves
// the section to the conversion.
//
// kept for one release: decisions-step
func (r *issueRestore) planUnplaced(ctx context.Context, converted *convertedSection, body []*pmdoc.Node, unplaced unplacedSection) error {
	if !r.convert {
		converted.fate = r.leftUnlessRestoring(unplaced.held)
		return nil
	}
	return r.planDecision(ctx, converted, body, &unplaced)
}

// leftUnlessRestoring is fateLeft for a section the conversion reads, and fateUntouched for one a
// restore alone reads and that held no block it moves.
//
// kept for one release: decisions-step
func (r *issueRestore) leftUnlessRestoring(holds bool) sectionFate {
	if !r.convert && !holds {
		return fateUntouched
	}
	return fateLeft
}

// planDecision makes converted a decision block of the spec's `## Decisions` part: its words are
// the section's body (body) with its heading as the first paragraph; a section under
// `## Replaced` is replaced="true", and from_ask names the answered ask its decision settles
// (converted.ask, planReplaced), where it settles one; its links are its shaped pointers. It keeps
// the section's heading block id where the spec holds no block under it. A section that cites no
// answered ask, or one under `## Replaced` (unplaced nil), takes its `Unsure:` paragraph as
// uncertainty="high", with the rest of it as uncertainty_reason. One the conversion could not
// place under one ask takes uncertainty="high" with the reason why (unplacedReason), keeps any
// `Unsure:` paragraph among its words, and closes with a paragraph citing each ask it is about
// that its words do not cite (uncitedAsks); it takes no from_ask, since it settles no one ask.
//
// A block of the body a decision cannot hold is left out of it (holdableWords), and the decision
// then ends with a citation of the section in the decisions document's last version that held it
// (sectionVersion), whose text stays in that version once the document retires. An ask block the
// section held is no such part: it stands in the spec. Where no version holds the section yet, the
// section is left for the next pass.
//
// kept for one release: decisions-step
func (r *issueRestore) planDecision(ctx context.Context, converted *convertedSection, body []*pmdoc.Node, unplaced *unplacedSection) error {
	section := converted.section
	id := section.headingID()
	if id == "" || r.specBlocks[id] || r.restoreHolds(id) {
		var err error
		if id, err = r.freshBlockID(ctx); err != nil {
			return err
		}
	}
	attrs := pmdoc.Attrs{pmdoc.BlockIDAttr: id}
	words := body
	if unplaced != nil {
		attrs["uncertainty"], attrs["uncertainty_reason"] = "high", unplacedReason(unplaced.cause)
	} else if unsure, reason := section.unsure(); unsure != nil {
		words = slices.DeleteFunc(slices.Clone(body), func(block *pmdoc.Node) bool { return block == unsure })
		attrs["uncertainty"] = "high"
		if reason != "" {
			attrs["uncertainty_reason"] = reason
		}
	}
	if section.replaced {
		attrs["replaced"] = true
	}
	if converted.ask != "" {
		attrs["from_ask"] = refs.AskRef(text.Ref{IssueKey: r.issue, ID: converted.ask})
	}
	if len(converted.links) > 0 {
		attrs[shapedAttribute] = r.specPointers(converted.links)
	}
	heading := &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}, Children: section.heading.Children}
	decision, err := parsedTypedBlock(decisionNode(attrs, heading, words), decisionType)
	leftOut := false
	if err != nil {
		words, leftOut = holdableWords(attrs, heading, words)
	}
	var closing []*pmdoc.Node
	if unplaced != nil {
		if uncited := r.uncitedAsks(section, words, *unplaced); len(uncited) > 0 {
			closing = append(closing, textParagraph("The asks this section cites: "+strings.Join(uncited, ", ")+"."))
		}
	}
	if leftOut {
		version := r.sectionVersion(section)
		if version == 0 {
			converted.fate = fateLeft
			return nil
		}
		closing = append(closing, textParagraph("The section as the decisions document held it: "+
			refs.BlockRef(r.decisions.address, section.headingID())+"@v"+strconv.Itoa(version)+"."))
	}
	if len(closing) > 0 {
		decision, err = parsedTypedBlock(decisionNode(attrs, heading, append(slices.Clone(words), closing...)), decisionType)
	}
	if err != nil {
		return fmt.Errorf("write the section %q of the decisions document of %s as a decision: %w", section.title, r.issue, err)
	}
	converted.fate, converted.decision, converted.decisionID = fateDecision, decision, id
	return nil
}

// decisionNode is a decision block of attrs holding heading and then words.
//
// kept for one release: decisions-step
func decisionNode(attrs pmdoc.Attrs, heading *pmdoc.Node, words []*pmdoc.Node) *pmdoc.Node {
	return &pmdoc.Node{Type: decisionType, Attrs: attrs, Children: append([]*pmdoc.Node{heading}, words...)}
}

// holdableWords is words without each block a decision block of attrs cannot hold after heading,
// as the document's own markdown pipeline reads one (parsedTypedBlock), and whether it left one
// out.
//
// kept for one release: decisions-step
func holdableWords(attrs pmdoc.Attrs, heading *pmdoc.Node, words []*pmdoc.Node) ([]*pmdoc.Node, bool) {
	var kept []*pmdoc.Node
	for _, block := range words {
		if _, err := parsedTypedBlock(decisionNode(attrs, heading, []*pmdoc.Node{block}), decisionType); err == nil {
			kept = append(kept, block)
		}
	}
	return kept, len(kept) < len(words)
}

// textParagraph is a paragraph holding words as plain text.
//
// kept for one release: decisions-step
func textParagraph(words string) *pmdoc.Node {
	return &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}, Children: []*pmdoc.Node{{Type: "text", Text: words}}}
}

// uncitedAsks is the address (dispatch://KEY/ask/<id>) of each ask unplaced is about that the
// section's heading and the blocks of words do not cite, as the decisions document's block index
// read their citations (unplacedSection.mentions): an ask whose block the section held, and one
// cited only in a block the decision leaves out.
//
// kept for one release: decisions-step
func (r *issueRestore) uncitedAsks(section decisionSection, words []*pmdoc.Node, unplaced unplacedSection) []string {
	cited := map[string]bool{}
	cite := func(id string) {
		for _, ask := range unplaced.mentions[id] {
			cited[ask] = true
		}
	}
	cite(section.headingID())
	for _, block := range words {
		pmdoc.Walk(block, func(node *pmdoc.Node) bool {
			if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
				cite(id)
			}
			return true
		})
	}
	var addresses []string
	for _, ask := range unplaced.asks {
		if !cited[ask] {
			addresses = append(addresses, refs.AskRef(text.Ref{IssueKey: r.issue, ID: ask}))
		}
	}
	return addresses
}

// sectionVersion is the decisions document's last version that holds section, as its block index
// reads it: the version the index stands at where it holds the section's heading, and 0 where the
// index holds no such version.
//
// kept for one release: decisions-step
func (r *issueRestore) sectionVersion(section decisionSection) int {
	if _, held := r.indexedBlocks[section.headingID()]; !held || section.headingID() == "" {
		return 0
	}
	return r.indexedVersion
}

// restoreHolds reports whether a block this pass restores takes id.
//
// kept for one release: decisions-step
func (r *issueRestore) restoreHolds(id string) bool {
	for _, restore := range r.restores {
		if restore.id == id {
			return true
		}
	}
	return false
}

// specPointers is the `shaped` value naming each spec block of ids, as a `block[]` attribute
// writes it (pmdoc.ParseBlockPointerValue): canonical block references, each once.
//
// kept for one release: decisions-step
func (r *issueRestore) specPointers(ids []string) []string {
	pointers := make([]string, 0, len(ids))
	for _, id := range ids {
		pointers = appendUnique(pointers, refs.BlockRef(r.spec.address, id))
	}
	return pointers
}

// appendUnique appends each of values to list that it does not hold yet.
//
// kept for one release: decisions-step
func appendUnique(list []string, values ...string) []string {
	for _, value := range values {
		if !slices.Contains(list, value) {
			list = append(list, value)
		}
	}
	return list
}

// plainAnswer reports whether body is ask's answer and nothing more (R9's plain-answer rule): its
// text, and the answer's, are the same once whitespace is collapsed and case folded. The answer is
// the person's words, or, where they wrote none, the options they chose.
//
// kept for one release: decisions-step
func plainAnswer(body []*pmdoc.Node, ask model.Ask) bool {
	if ask.Answer == nil {
		return false
	}
	answer := strings.Join(ask.Answer.Selected, ", ")
	if ask.Answer.Text != nil && strings.TrimSpace(*ask.Answer.Text) != "" {
		answer = *ask.Answer.Text
	}
	parts := make([]string, len(body))
	for index, block := range body {
		parts[index] = pmdoc.TextContent(block)
	}
	normalized := normalizedWords(strings.Join(parts, " "))
	return normalized != "" && normalized == normalizedWords(answer)
}

// normalizedWords is value with its whitespace collapsed to single spaces, trimmed, and its case
// folded.
//
// kept for one release: decisions-step
func normalizedWords(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

// allParagraphs reports whether every block of body is a paragraph, which is all a reading holds.
//
// kept for one release: decisions-step
func allParagraphs(body []*pmdoc.Node) bool {
	for _, block := range body {
		if block.Type != "paragraph" {
			return false
		}
	}
	return true
}

// readingOf is a reading block holding body's paragraphs, under a fresh block id, as the document's
// own markdown pipeline makes it. A reading stands only at the end of a decision or an ask, so it is
// parsed at the end of a decision that holds it and nothing else of weight, and taken out of it.
//
// kept for one release: decisions-step
func readingOf(body []*pmdoc.Node) (*pmdoc.Node, error) {
	reading := &pmdoc.Node{
		Type: readingType, Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: uuid.NewString()}, Children: slices.Clone(body),
	}
	holder, err := parsedTypedBlock(&pmdoc.Node{
		Type: decisionType, Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: uuid.NewString()},
		Children: []*pmdoc.Node{{Type: "paragraph", Attrs: pmdoc.Attrs{}, Children: []*pmdoc.Node{{Type: "text", Text: "holder"}}}, reading},
	}, decisionType)
	if err != nil {
		return nil, err
	}
	_, readings := pmdoc.TypedContent(holder)
	if len(readings) != 1 || readings[0].Type != readingType {
		return nil, errors.New("the reading does not read back as one reading block")
	}
	return readings[0], nil
}

// readingType is the type of an agent's reading block.
//
// kept for one release: decisions-step
const readingType = "reading"

// parsedTypedBlock is node, a typed block of typ built here, as the document's own markdown
// pipeline makes it (parsedBlockBody): rendered and read back, so it carries the attributes and
// the inner block ids a parse gives rather than ones a hand-built node invents, with its own id,
// which its markdown writes, kept.
//
// kept for one release: decisions-step
func parsedTypedBlock(node *pmdoc.Node, typ string) (*pmdoc.Node, error) {
	parsed, refusal, err := parsedBlockBody([]*pmdoc.Node{node}, pmdoc.NewWriteBudget())
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return nil, errors.New(refusal)
	}
	if len(parsed) != 1 || parsed[0].Type != typ {
		return nil, fmt.Errorf("the %s block does not read back as one %s block", typ, typ)
	}
	return parsed[0], nil
}

// sectionAuthors is the author of each section's newest version: the first author
// artifact_versions.authors names on the newest version of the decisions document that changed
// the section's text, which is the greatest version the block index records as changing a block of
// the section (indexedBlocks, refs.BlockChanges), as the Decisions read of the release before dated
// a section. A section whose blocks the index records no change of, or whose version names no
// author, has none.
//
// kept for one release: decisions-step
func (r *issueRestore) sectionAuthors(ctx context.Context, sections []decisionSection) ([]model.Actor, error) {
	changes := r.indexedBlocks
	versions := make([]int, len(sections))
	wanted := map[int]bool{}
	for index, section := range sections {
		versions[index] = sectionChangedVersion(changes, section.blockIDs())
		if versions[index] > 0 {
			wanted[versions[index]] = true
		}
	}
	authors := make([]model.Actor, len(sections))
	if len(wanted) == 0 {
		return authors, nil
	}
	rows, err := r.tx.Query(ctx, `
		select number, authors from artifact_versions where artifact_id = $1::uuid and number = any($2::int[])
	`, r.decisions.id, slices.Sorted(maps.Keys(wanted)))
	if err != nil {
		return nil, fmt.Errorf("read the authors of the decisions document of %s: %w", r.issue, err)
	}
	firsts := map[int]model.Actor{}
	for rows.Next() {
		var number int
		var listed []model.Actor
		if err := rows.Scan(&number, &listed); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the authors of the decisions document of %s: %w", r.issue, err)
		}
		if len(listed) > 0 {
			firsts[number] = listed[0]
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the authors of the decisions document of %s: %w", r.issue, err)
	}
	for index := range sections {
		authors[index] = firsts[versions[index]]
	}
	return authors, nil
}

// sectionKeeps is each standing Keep a person recorded on a heading of the decisions document
// (decision_keeps, which Keep writes for such a heading until the document is retired, F3), by
// heading block id, with whether the sum it recorded is the section's text sum as it stands: the
// sum the release before's Decisions read served, over the section's markdown from its heading to
// its last block (decisionread.TextSum), which the Keep posts back. Each carries the actor of the
// newest decision.kept the issue holds for its heading, the event Keep appended in the transaction
// that wrote the row (sectionKeep.keeper).
//
// kept for one release: decisions-step
func (r *issueRestore) sectionKeeps(ctx context.Context, tree *pmdoc.Node, sections []decisionSection) (map[string]*sectionKeep, error) {
	rows, err := r.tx.Query(ctx, `
		select k.block_id, k.text_sha256, k.kept_by, k.kept_at, kept.actor
		from decision_keeps k
		left join lateral (
			select e.actor from events e
			where e.issue_key = $2 and e.type = 'decision.kept'
			  and e.payload->>'artifact_id' = k.artifact_id::text and e.payload->>'block_id' = k.block_id
			order by e.id desc
			limit 1
		) kept on true
		where k.artifact_id = $1::uuid
	`, r.decisions.id, r.issue)
	if err != nil {
		return nil, fmt.Errorf("read the Keeps of the decisions document of %s: %w", r.issue, err)
	}
	type recorded struct {
		sum, keptBy string
		keeper      model.Actor
		keptAt      time.Time
	}
	held := map[string]recorded{}
	for rows.Next() {
		var block string
		var keep recorded
		var actor *model.Actor
		if err := rows.Scan(&block, &keep.sum, &keep.keptBy, &keep.keptAt, &actor); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read a Keep of the decisions document of %s: %w", r.issue, err)
		}
		keep.keeper = model.Actor{Kind: "user", ID: model.CanonicalLogin(keep.keptBy)}
		if actor != nil && actor.Kind == "user" && model.CanonicalLogin(actor.ID) == keep.keeper.ID {
			keep.keeper = *actor
		}
		held[block] = keep
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the Keeps of the decisions document of %s: %w", r.issue, err)
	}
	keeps := map[string]*sectionKeep{}
	if len(held) == 0 {
		return keeps, nil
	}
	markdown, offsets, err := renderTreeWithOffsets(tree)
	if err != nil {
		return nil, err
	}
	ranges := blockRanges(offsets)
	for _, section := range sections {
		keep, kept := held[section.headingID()]
		if !kept {
			continue
		}
		text, found := sectionText(markdown, ranges, section.blockIDs())
		matches := found && decisionread.TextSum(text) == keep.sum
		keeps[section.headingID()] = &sectionKeep{keptBy: keep.keptBy, keeper: keep.keeper, keptAt: keep.keptAt, matches: matches}
	}
	return keeps, nil
}

// writeSpec writes into the spec what the pass restores and converts, in one live write and one
// version: each restored ask block, in answer-time order, then each converted section's decision,
// in the spec's `## Decisions` part, in place of its one pointer line to the decisions document -
// the part's first paragraph whose one content is the citation of the document itself
// (isPointerLine), every other paragraph kept, one citing the document among other words or
// citing a section of it among them - or after the part's last block where it holds none, and
// under a `## Decisions` heading the write adds where the spec holds no such part; and, in each
// answered ask block it holds already, the shaped pointers and the readings the conversion gives
// it. It records the pointer line it took out (pointerLine) and whether it added the heading
// (headingAdded), which its version names as the step's own. On a fleet of one build - which the
// phase runs on alone - the same write cuts every ask block of the spec to the attributes a
// version keeps (writePresent), as the first accepted write of the document does (R10), a block
// that has no ask row and is no copy of an ask to none of them, and gives each restored answered
// ask's block those of its row (keptAskState). It answers the event of the version it wrote,
// NoEvent where the spec did not change.
//
// kept for one release: decisions-step
func (r *issueRestore) writeSpec(ctx context.Context) (int64, error) {
	// A restored ask's block keeps the state its node holds (keptAskState, onItsNode).
	// kept for one release: stored-ask-state
	var ids []string
	for _, restore := range r.restores {
		if !restore.joins && !restore.rowless {
			ids = append(ids, restore.row.ID)
		}
	}
	nodes, err := askNodeAnswers(ctx, r.tx, ids)
	if err != nil {
		return graphstore.NoEvent, err
	}
	var added []*pmdoc.Node
	for _, restore := range r.restores {
		if restore.joins {
			// A copy of an ask block the spec holds: nothing to write (planDocumentMoves).
			continue
		}
		var block *pmdoc.Node
		if restore.rowless {
			block, err = r.movedBlockOf(ctx, restore)
		} else {
			block, err = r.restoredAskBlock(ctx, restore, nodes)
		}
		if err != nil {
			return graphstore.NoEvent, err
		}
		added = append(added, block)
		r.result.Restored = append(r.result.Restored, RestoredAsk{
			Ask: restore.row.ID, Shape: restore.work.Shape, From: restore.from,
			To: restore.address(r.spec), Read: restore.read, NewID: restore.id != restore.work.BlockID,
		})
	}
	for _, converted := range r.sections {
		if converted.fate == fateDecision {
			added = append(added, converted.decision)
		}
	}
	pointers := map[string]bool{}
	if r.decisions != nil {
		pointers[r.decisions.refKey] = true
	}
	var refused error
	var pointerLine string
	var headingAdded bool
	err = r.service.applyLive(ctx, r.spec.id, DecisionStepActor, func(doc *crdt.Doc, current pmdoc.ValidDocument, _ func(func(*crdt.Transaction))) error {
		if current.Node() == nil {
			return fmt.Errorf("the spec of %s is outside the Proof schema, so the decisions step cannot write into it", r.issue)
		}
		// kept for one release: stored-ask-state
		withAsk, err := askBlocksWithAnAsk(ctx, r.tx, r.spec.id, current.Node())
		if err != nil {
			return err
		}
		_, err = rewriteLive(doc, nil, func(live *pmdoc.Node) bool {
			for _, block := range added {
				id, _ := block.Attrs[pmdoc.BlockIDAttr].(string)
				if blockIDHeld(live, id) {
					refused = fmt.Errorf("the spec of %s holds block %s already, which the decisions step writes a block under", r.issue, id)
					return false
				}
			}
			changed := r.writePresent(r.spec.id, live, r.present[r.spec.id], withAsk)
			pointerLine, headingAdded = "", false
			if len(added) > 0 {
				headingAdded = !hasDecisionsPart(live)
				var line *pmdoc.Node
				live.Children, line = withDecisionsPartBlocks(live.Children, added, func(node *pmdoc.Node) bool {
					return isPointerLine(node, pointers, r.service.serverURL)
				})
				if line != nil {
					markdown, err := blockMarkdown(line)
					if err != nil {
						refused = fmt.Errorf("render the pointer line the decisions step takes out of the spec of %s: %w", r.issue, err)
						return false
					}
					pointerLine = markdown
				}
				changed = true
			}
			return changed
		})
		return err
	})
	if refused != nil {
		return graphstore.NoEvent, refused
	}
	if errors.Is(err, websocket.ErrNoChanges) {
		return graphstore.NoEvent, nil
	}
	if err != nil {
		return graphstore.NoEvent, err
	}
	r.pointerLine, r.headingAdded = pointerLine, headingAdded
	r.creditSectionAuthors(ctx, r.spec.id)
	return r.writeVersion(ctx, r.spec, &r.result.SpecVersion)
}

// creditSectionAuthors credits each reading and each decision this pass writes into document to
// the author of the section it came from (sectionAuthors), on the live write that wrote it, before
// its version is written: the typed-node pass gives a reading's node, and a decision's, the author
// of the write that introduced its block (refs.DecisionFacts, blockAuthors), and this write is the
// step's, which proves nobody, so the block carries its section's author forward as a stamp's
// rename carries an ask block's (liveWrite.carriedAskAuthors). A section whose author is unknown
// credits nobody, and its node takes no author.
//
// kept for one release: decisions-step
func (r *issueRestore) creditSectionAuthors(ctx context.Context, document string) {
	write := ledgerFrom(ctx).liveWriteFor(document)
	if write == nil {
		return
	}
	authors := map[string]model.Actor{}
	for _, converted := range r.sections {
		if converted.author.Kind == "" && converted.author.ID == "" {
			continue
		}
		switch {
		case converted.fate == fateDecision && document == r.spec.id:
			authors[converted.decisionID] = converted.author
		case converted.fate == fateReading && converted.askDocument.id == document:
			id, _ := converted.reading.Attrs[pmdoc.BlockIDAttr].(string)
			authors[id] = converted.author
		}
	}
	mergeInto(&write.carriedAskAuthors, authors)
}

// writePresent writes edits - the shaped pointers and the readings the conversion gives the
// answered ask blocks a document holds already - into live, the tree of document as its live
// write holds it, and takes the answer's words and selection, which no version holds on a fleet
// of one build - the one the phase runs on - off every ask block of it that has an ask (withAsk,
// askBlocksWithAnAsk: withoutUnkeptAskAttributes), leaving the state, answerer, time and a copy's
// provenance settlement keeps in step with each ask, and every attribute a version can store off
// each other ask block, which has no ask row and is no copy of an ask and keeps none of the eight
// (R10, withoutStoredAskAttributes). It records each pointer it gave a block that
// the block did not hold (shapedAdded), which the version names as the step's own, and each
// reading it wrote (readingsWritten), and reports whether it changed the tree. It visits every
// ask block of the tree: pmdoc.Walk stops at the first node its visit answers false for, so the
// visit answers true at an ask block too, and the blocks after the first are written as well.
//
// kept for one release: decisions-step
func (r *issueRestore) writePresent(document string, live *pmdoc.Node, edits *presentEdits, withAsk map[string]bool) bool {
	changed := false
	added := map[string][]string{}
	written := map[string]bool{}
	pmdoc.Walk(live, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if edits != nil {
			if shaped := edits.shaped[id]; len(shaped) > 0 {
				held := shapedPointersOf(node)
				var given []string
				for _, pointer := range r.specPointers(shaped) {
					if !slices.Contains(held, pointer) {
						given = append(given, pointer)
					}
				}
				if len(given) > 0 {
					node.Attrs[shapedAttribute] = append(held, given...)
					added[id] = given
					changed = true
				}
			}
			if readings := edits.readings[id]; len(readings) > 0 {
				node.Children = append(node.Children, readings...)
				for _, reading := range readings {
					readingID, _ := reading.Attrs[pmdoc.BlockIDAttr].(string)
					written[readingID] = true
				}
				changed = true
			}
		}
		// The strip of a block that has no ask row and is no copy of an ask is R10's and stays;
		// the lighter strip of a block that has an ask goes with the form a version keeps.
		strip := withoutStoredAskAttributes
		// kept for one release: stored-ask-state
		if withAsk[id] {
			strip = withoutUnkeptAskAttributes
		}
		if strip(node) {
			changed = true
		}
		return true
	})
	r.shapedAdded[document] = added
	r.readingsWritten[document] = written
	return changed
}

// withoutAsklessAskAttributes takes every attribute a version can store off each ask block of tree
// that withAsk does not name (askBlocksWithAnAsk) - one that has no ask row and is no copy of an
// ask, which keeps none of the eight (R10) - and reports whether it changed one. Its one caller is
// the decisions step's write of the decisions document (writeDecisionsDocument), so it goes with
// the step; the blocks withAsk names are passed over only while a version keeps a form of each
// ask, and that goes earlier.
//
// kept for one release: decisions-step
func withoutAsklessAskAttributes(tree *pmdoc.Node, withAsk map[string]bool) bool {
	changed := false
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		// kept for one release: stored-ask-state
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); withAsk[id] {
			return true
		}
		if withoutStoredAskAttributes(node) {
			changed = true
		}
		return true
	})
	return changed
}

// refuseUnwrittenReadings fails the pass where a section it converts into a reading of an ask
// block a document held already has that reading in no block the pass wrote (readingsWritten):
// the decisions document's write has taken the section out by then, and the moved_to the pass
// would state from its heading names a block that holds none of its words. The failure rolls the
// issue's transaction back, so the section stays where it stood, and no moved_to names it. A
// reading of an ask the pass restores is in the block restoredAskBlock builds, which holds it.
//
// kept for one release: decisions-step
func (r *issueRestore) refuseUnwrittenReadings() error {
	for _, converted := range r.sections {
		if converted.fate != fateReading || r.restoreOf(converted.ask) != nil {
			continue
		}
		id, _ := converted.reading.Attrs[pmdoc.BlockIDAttr].(string)
		if !r.readingsWritten[converted.askDocument.id][id] {
			return fmt.Errorf("the decisions step wrote the reading of the section %q of %s into no block: %s holds no ask block %s to take it, so it converts nothing of %s this pass",
				converted.section.title, r.decisions.address, converted.askDocument.address, converted.askBlock, r.issue)
		}
	}
	return nil
}

// writePresentDocuments writes, into each document of the issue other than the spec that holds an
// answered ask block a converted section restated, the shaped pointers and the readings the
// conversion gives that block (writePresent), and its version, in document id order. R9 gives a
// section's links and reading to the ask's present block wherever it stands in the issue.
//
// kept for one release: decisions-step
func (r *issueRestore) writePresentDocuments(ctx context.Context) error {
	for _, id := range slices.Sorted(maps.Keys(r.present)) {
		if id == r.spec.id {
			continue
		}
		document := r.documents[id]
		edits := r.present[id]
		err := r.service.applyLive(ctx, id, DecisionStepActor, func(doc *crdt.Doc, current pmdoc.ValidDocument, _ func(func(*crdt.Transaction))) error {
			if current.Node() == nil {
				return fmt.Errorf("%s is outside the Proof schema, so the decisions step cannot write into it", document.address)
			}
			// kept for one release: stored-ask-state
			withAsk, err := askBlocksWithAnAsk(ctx, r.tx, id, current.Node())
			if err != nil {
				return err
			}
			_, err = rewriteLive(doc, nil, func(live *pmdoc.Node) bool {
				return r.writePresent(id, live, edits, withAsk)
			})
			return err
		})
		if errors.Is(err, websocket.ErrNoChanges) {
			continue
		}
		if err != nil {
			return err
		}
		var number int
		r.creditSectionAuthors(ctx, id)
		if _, err := r.writeVersion(ctx, document, &number); err != nil {
			return err
		}
		if number > 0 {
			r.result.OtherVersions++
		}
	}
	return nil
}

// blockIDHeld reports whether tree holds a block under id.
//
// kept for one release: decisions-step
func blockIDHeld(tree *pmdoc.Node, id string) bool {
	held := false
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if blockID, _ := node.Attrs[pmdoc.BlockIDAttr].(string); blockID == id {
			held = true
		}
		return !held
	})
	return held
}

// withDecisionsPartBlocks is children, a document's top-level blocks, with blocks put in its
// `## Decisions` part in place of the part's first pointer line (pointer), which it answers, every
// other block of the part kept, and after its last block where it holds none; and, where the
// document holds no such part, at its end under a `## Decisions` heading added for them. It
// answers nil for the pointer line where it took none out.
//
// kept for one release: decisions-step
func withDecisionsPartBlocks(children, blocks []*pmdoc.Node, pointer func(*pmdoc.Node) bool) ([]*pmdoc.Node, *pmdoc.Node) {
	start, end := decisionsPart(&pmdoc.Node{Type: "doc", Children: children})
	if start < 0 {
		heading := &pmdoc.Node{Type: "heading", Attrs: pmdoc.Attrs{"level": decisionsPartLevel},
			Children: []*pmdoc.Node{{Type: "text", Text: decisionsPartTitle}}}
		return append(append(slices.Clone(children), heading), blocks...), nil
	}
	next := slices.Clone(children[:start+1])
	var replaced *pmdoc.Node
	for _, child := range children[start+1 : end] {
		if replaced == nil && pointer(child) {
			replaced = child
			next = append(next, blocks...)
			continue
		}
		next = append(next, child)
	}
	if replaced == nil {
		next = append(next, blocks...)
	}
	return append(next, children[end:]...), replaced
}

// restoredAskBlock is the block restore writes into the spec for an ask its row names. It is the
// block it moves, byte for byte, with every link, code span and sub-list its question and options
// hold - for the main shape the block as the spec's last version holding it held it (held), for
// the second shape, or where the spec never held it, the block as the decisions document holds it
// (moved) - where that block reads back as the wording the ask's row holds (askBlock.asks), so
// the restore returns what the person read (R7); and otherwise the ask's question and options as
// its row holds them. Either way it takes urgency and multiple from the row and shows no answer,
// which its node holds (R6): its opening line keeps the row's state, answerer and time, as every
// version on a fleet of one build - the one the phase runs on - keeps them for the release before's
// readers (keptAskState), and none of the other server-owned attributes. Its `shaped` pointers are
// the block's own and the spec blocks the sections that restate it link (restoredPointers), and it
// holds the readings the block of the decisions document held and the ones the conversion writes.
// It reads back as the row's wording, or is refused.
//
// kept for one release: decisions-step
func (r *issueRestore) restoredAskBlock(ctx context.Context, restore *restoredBlock, nodes map[string]askNodeAnswer) (*pmdoc.Node, error) {
	decided := model.NewAskEditPrevious(restore.row)
	attrs := pmdoc.Attrs{pmdoc.BlockIDAttr: restore.id, "urgency": decided.Urgency, "multiple": decided.Multiple}
	shaped, shape, err := r.restoredPointers(ctx, restore)
	if err != nil {
		return nil, err
	}
	restore.shape = shape
	if len(shaped) > 0 {
		attrs[shapedAttribute] = shaped
	}
	var readings []*pmdoc.Node
	if restore.moved != nil {
		_, readings = pmdoc.TypedContent(restore.moved)
	}
	readings = append(slices.Clone(readings), restore.readings...)
	kept := func(block *pmdoc.Node) *pmdoc.Node {
		// kept for one release: stored-ask-state
		keptAskState(onItsNode(restore.row, nodes), "")(block)
		return block
	}
	source := restore.held
	if restore.work.Shape == graphcensus.RestoreMoves || source == nil {
		source = restore.moved
	}
	if source != nil {
		content, _ := pmdoc.TypedContent(source)
		// A block whose bytes the parse refuses, or that asks other words than the row's, is
		// built from the row below instead.
		if block, asks, err := askBlockFrom(attrs, content, readings, decided); err == nil && asks {
			return kept(block), nil
		}
	}
	budget := pmdoc.NewWriteBudget()
	content, err := questionBlocks(normalizeAskQuestion(decided.Question), budget)
	if err != nil {
		return nil, fmt.Errorf("write the question of ask %s: %w", restore.row.ID, err)
	}
	if list, err := editedOptionList(nil, normalizeAskOptions(decided.Options), budget); err != nil {
		return nil, fmt.Errorf("write the options of ask %s: %w", restore.row.ID, err)
	} else if list != nil {
		content = append(content, list)
	}
	block, asks, err := askBlockFrom(attrs, content, readings, decided)
	if err != nil {
		return nil, fmt.Errorf("write the block of ask %s: %w", restore.row.ID, err)
	}
	if !asks {
		return nil, fmt.Errorf("the restored block of ask %s does not read back as the wording its row holds", restore.row.ID)
	}
	return kept(block), nil
}

// askBlockFrom is the ask block of attrs holding content and then readings, as the document's own
// markdown pipeline makes it (parsedTypedBlock), and whether it asks what decided does
// (askBlock.asks).
//
// kept for one release: decisions-step
func askBlockFrom(attrs pmdoc.Attrs, content, readings []*pmdoc.Node, decided model.AskEditPrevious) (*pmdoc.Node, bool, error) {
	children := append(slices.Clone(content), readings...)
	block, err := parsedTypedBlock(&pmdoc.Node{Type: "ask", Attrs: maps.Clone(attrs), Children: children}, "ask")
	if err != nil {
		return nil, false, err
	}
	read, err := parseAskBlock(block)
	return block, err == nil && read.asks(decided), nil
}

// restoredPointers is the `shaped` value of restore's block, R6's "its `shaped` tuples stay
// intact": the pointers the block held - as the spec last held it (held) and as the decisions
// document holds it (moved) - each as it reads once the pass is written (movedPointer), then the
// spec blocks the sections that restate its ask link (specPointers), each once; and what the pass
// did to the pointers the spec last held the block with (restoredShape) - each it rewrote, and
// each other pointer it gives the block - which restoreCase leaves out of its comparison with the
// block the person approved.
//
// kept for one release: decisions-step
func (r *issueRestore) restoredPointers(ctx context.Context, restore *restoredBlock) ([]string, restoredShape, error) {
	var pointers []string
	var shape restoredShape
	if restore.held != nil {
		for _, pointer := range shapedPointersOf(restore.held) {
			to, stands, err := r.movedPointer(ctx, pointer)
			if err != nil {
				return nil, restoredShape{}, err
			}
			if !stands {
				to = ""
			}
			if to != pointer {
				if shape.moved == nil {
					shape.moved = map[string]string{}
				}
				shape.moved[pointer] = to
			}
			if to != "" {
				pointers = appendUnique(pointers, to)
			}
		}
	}
	own := len(pointers)
	if restore.moved != nil {
		moved, err := r.movedPointers(ctx, shapedPointersOf(restore.moved))
		if err != nil {
			return nil, restoredShape{}, err
		}
		pointers = appendUnique(pointers, moved...)
	}
	pointers = appendUnique(pointers, r.specPointers(restore.shaped)...)
	shape.added = pointers[own:len(pointers):len(pointers)]
	return pointers, shape, nil
}

// movedPointers is pointers, the `shaped` values of a block the restore moves, each as it reads
// once the pass is written (movedPointer), without each that then names no block.
//
// kept for one release: decisions-step
func (r *issueRestore) movedPointers(ctx context.Context, pointers []string) ([]string, error) {
	moved := make([]string, 0, len(pointers))
	for _, pointer := range pointers {
		to, stands, err := r.movedPointer(ctx, pointer)
		if err != nil {
			return nil, err
		}
		if stands {
			moved = appendUnique(moved, to)
		}
	}
	return moved, nil
}

// movedPointer is pointer, a `shaped` value of a block the restore moves, as it reads once the
// pass is written, and whether it names a block then. A pointer to a block of the decisions
// document that this pass moves names the block it moves to: the ask block the restore writes
// into the spec, or the decision or the ask block a section it converts becomes, as writeFacts'
// moved_to tuples state it (passMove). One to any other block this pass takes out of that
// document names none, nor does one whose section's moved_to reaches an ask, which no `shaped`
// value names. One to a block the document no longer holds names the block its moved_to tuples
// reach (graphstore.MovedTo), and none where they reach no block. Every other pointer, one to a
// block the decisions document still holds among them, reads as it is.
//
// kept for one release: decisions-step
func (r *issueRestore) movedPointer(ctx context.Context, pointer string) (string, bool, error) {
	if r.decisions == nil {
		return pointer, true, nil
	}
	cited := text.Extract(pointer, r.service.serverURL)
	if len(cited) != 1 || cited[0].Kind != "block" {
		return pointer, true, nil
	}
	address := text.TargetID(cited[0])
	id, ofDecisions := strings.CutPrefix(address, r.decisions.refKey+"#")
	if !ofDecisions {
		return pointer, true, nil
	}
	if to, taken := r.passMove(address, id); taken {
		return to, to != "", nil
	}
	reached, ok, err := graphstore.MovedTo(ctx, r.tx, address)
	if err != nil || !ok || reached.Ask != "" {
		return "", false, err
	}
	if reached.Address == address {
		return pointer, true, nil
	}
	to, stands := r.blockPointer(reached.Address)
	return to, stands, nil
}

// passMove is where this pass moves the block of the decisions document at address, the store's
// address of its block id, named as a `shaped` value names it ("" for a section's heading whose
// moved_to reaches an ask, or a block of a converted section that no moved_to leaves), and whether
// this pass takes that block out of the document at all (decisionsDropped).
//
// kept for one release: decisions-step
func (r *issueRestore) passMove(address, id string) (string, bool) {
	for _, restore := range r.restores {
		if restore.from == address {
			return refs.BlockRef(r.spec.address, restore.id), true
		}
	}
	for _, converted := range r.sections {
		if refs.BlockAddress(r.decisions.refKey, converted.section.headingID()) != address {
			continue
		}
		switch converted.fate {
		case fateDecision:
			return refs.BlockRef(r.spec.address, converted.decisionID), true
		case fateReading, fateDropped:
			return refs.BlockRef(converted.askDocument.address, converted.askBlock), true
		}
	}
	return "", r.decisionsDropped[id]
}

// blockPointer is the `shaped` value naming the block at address, a store address
// (refs.BlockAddress), and whether address is one of a document of the issue that stands.
//
// kept for one release: decisions-step
func (r *issueRestore) blockPointer(address string) (string, bool) {
	refKey, id, found := strings.Cut(address, "#")
	if !found {
		return "", false
	}
	for _, document := range r.documents {
		if document.refKey == refKey {
			return refs.BlockRef(document.address, id), true
		}
	}
	return "", false
}

// movedBlockOf is the block the restore writes into the spec for restore, a block of the decisions
// document no row names: the block as that document's live tree holds it, under the id it takes,
// its `shaped` pointers each as it reads once the pass is written (movedPointers), read back as
// the document's own markdown pipeline makes it. It holds what a version of the spec on a fleet of
// one build - the one the phase runs on - stores of it, as the spec's settlement and a version of
// the spec read the ask of the block once it stands there (askBlockAsks). A copy of an ask another
// document of the issue holds takes the form a version keeps of its source - the state where that
// is not open, the answerer and time, and copied_from and copied_from_document - as the source's
// node holds it when the step runs (keptFromItsNode), and holds no answer's words or selection and
// no open state (keptAskState). The copy's own attributes are not that form: they show the source
// as the copy's last settlement read it, and an answer or a resolution of the source arms the
// copy's next settlement for after its commit (SettleCopiesOf), which the step can run before. A
// block the spec's own row names loses the answer's words and selection and an open state
// (withoutUnkeptAskAttributes). A block that is neither loses every attribute a version can store
// (withoutStoredAskAttributes, R10): no ask's state stands behind them, and the settlement after
// the write opens its ask and writes that state. No row holds its question to build it from, and
// none names it to rebind.
//
// kept for one release: decisions-step
func (r *issueRestore) movedBlockOf(ctx context.Context, restore *restoredBlock) (*pmdoc.Node, error) {
	node := &pmdoc.Node{Type: "ask", Attrs: maps.Clone(restore.moved.Attrs), Children: restore.moved.Children}
	node.Attrs[pmdoc.BlockIDAttr] = restore.id
	if held := shapedPointersOf(restore.moved); len(held) > 0 {
		pointers, err := r.movedPointers(ctx, held)
		if err != nil {
			return nil, err
		}
		delete(node.Attrs, shapedAttribute)
		if len(pointers) > 0 {
			node.Attrs[shapedAttribute] = pointers
		}
		// The spec held no block of it: every pointer it carries in is the pass's own.
		restore.shape = restoredShape{added: pointers}
	}
	// The strip of a block that is no copy is R10's and stays; the source's form a copy takes goes
	// with the form a version keeps.
	// kept for one release: stored-ask-state
	rows, sources, err := askBlockAsks(ctx, r.tx, r.spec.id, &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{node}})
	if err != nil {
		return nil, err
	}
	_, indexed := rows[restore.id]
	source, copied := sources[restore.id]
	switch {
	// kept for one release: stored-ask-state
	case copied:
		nodes, err := askNodeAnswers(ctx, r.tx, []string{source.ask.ID})
		if err != nil {
			return nil, err
		}
		keptAskState(keptFromItsNode(source.ask, nodes), source.document)(node)
	// kept for one release: stored-ask-state
	case indexed:
		withoutUnkeptAskAttributes(node)
	default:
		withoutStoredAskAttributes(node)
	}
	return movedAskBlock(node, restore)
}

// movedAskBlock is node, the block movedBlockOf builds for restore, read back as the document's
// own markdown pipeline makes it.
//
// kept for one release: decisions-step
func movedAskBlock(node *pmdoc.Node, restore *restoredBlock) (*pmdoc.Node, error) {
	block, err := parsedTypedBlock(node, "ask")
	if err != nil {
		return nil, fmt.Errorf("write the ask block %s of the decisions document, which no row names, into the spec: %w", restore.work.BlockID, err)
	}
	return block, nil
}

// writeVersion writes the version of document the pass's live write left (snapshotVersion, as
// SnapshotVersion writes it), with what the pass added to document (stepCasesFor) as the
// decisions step's own write, and its artifact.version event, under which the version's block
// index states its facts (graphstore.Caused) and its references are stamped, as every version a
// route writes is. It records the version's number in *number and answers the event, NoEvent
// where no version was written.
//
// kept for one release: decisions-step
func (r *issueRestore) writeVersion(ctx context.Context, document issueDocument, number *int) (int64, error) {
	written, err := r.service.snapshotVersion(ctx, document.id, DecisionStepActor, false, r.stepCasesFor(document.id))
	if err != nil {
		return graphstore.NoEvent, err
	}
	if !written.Wrote {
		return graphstore.NoEvent, nil
	}
	*number = written.Version.Number
	event, err := r.service.events.Append(ctx, r.tx, model.Event{
		IssueKey: new(r.issue), Type: "artifact.version", Actor: DecisionStepActor,
		Payload: ArtifactVersionEventPayload(document.id, document.name, written.Version, nil, written.Changes),
	})
	if err != nil {
		return graphstore.NoEvent, fmt.Errorf("append the version event of %s: %w", document.address, err)
	}
	if err := refs.Stamp(ctx, r.tx, "artifact", document.id, event.ID); err != nil {
		return graphstore.NoEvent, err
	}
	if err := graphstore.Caused(ctx, r.tx, written.Blocks, event.ID); err != nil {
		return graphstore.NoEvent, err
	}
	eventSink(ctx)(event)
	return event.ID, nil
}

// stepCasesFor is what the version of document this pass writes names as the decisions step's
// own write (decisionStepCases, which writeVersion hands snapshotVersion): the
// restore's phase, the conversion's where it converts, and what the pass added to document - for
// the spec, the ask blocks the restore wrote, the decisions the conversion wrote, the
// `## Decisions` heading where it added one and the pointer line it took out; for any document,
// the `shaped` pointers it gave the answered ask blocks the document held already. The restore's
// and the conversion's carry cases take out only those.
//
// kept for one release: decisions-step
func (r *issueRestore) stepCasesFor(document string) decisionStepCases {
	steps := decisionStepCases{restore: true, conversion: r.convert, shaped: r.shapedAdded[document]}
	if document != r.spec.id {
		return steps
	}
	steps.restored, steps.decisions, steps.headingAdded = map[string]bool{}, map[string]bool{}, r.headingAdded
	steps.restoredShaped = map[string]restoredShape{}
	for _, restore := range r.restores {
		if !restore.joins {
			steps.restored[restore.id] = true
			steps.restoredShaped[restore.id] = restore.shape
		}
	}
	for _, converted := range r.sections {
		if converted.fate == fateDecision {
			steps.decisions[converted.decisionID] = true
		}
	}
	if r.pointerLine != "" {
		steps.pointerLines = []string{r.pointerLine}
	}
	return steps
}

// writeFacts appends, under cause, the facts of the pass the documents' versions state none of:
// for each ask the restore moved from the decisions document, its node's relations to the
// documents, now the spec's (graphstore.AskClaims), and one moved_to tuple from the block it left
// to the block it stands at; for each section the conversion took out, one moved_to tuple from its
// heading's address to the decision it became or the ask block it restates (R8), or, for a section
// that cites an answered ask a person removed, to that ask's own node; and for each decision whose
// section a person kept, the Keep on its node - a kept_by tuple, and a kept_text_sum value where
// the Keep's sum is the section's as it stands (F3) - with the decision_keeps row that projects
// it, as Keep writes one. A section a restore alone took out writes its moved_to too. For each
// answered ask the restore declined (removalOf, nodeRemoval), it appends one removed_by from the
// ask's node to the person who removed its block, with that person's node, under the event that
// recorded the person's version and at its time, so the finder leaves a returning ask out from
// then on; for a move it declined, that is the removal its node holds, which the append restates
// under its own event and so leaves as it stands. It appends nothing for an ask of roomRemovals:
// the version of the decisions document that the pass wrote states it (writeDecisionsDocument).
//
// kept for one release: decisions-step
func (r *issueRestore) writeFacts(ctx context.Context, cause int64) error {
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	var claims []graphstore.Claim
	moved := func(from, to string) {
		tuples = append(tuples, graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: from, Kind: graphstore.TupleMovedTo,
			ToKind: refs.BlockNodeKind, ToID: to, At: r.at, Who: DecisionStepActor, SourceEvent: cause, Asserted: true,
		})
	}
	// One write states each node once: a person a Keep names and a person who removed an ask may
	// be one.
	people := map[string]bool{}
	person := func(login string, at time.Time, event int64) {
		if !people[login] {
			people[login] = true
			nodes = append(nodes, graphstore.PersonNode(login, DecisionStepActor, at, event))
		}
	}
	for _, declined := range r.declined {
		person(declined.person, declined.at, declined.event)
		tuples = append(tuples, graphstore.Tuple{
			FromKind: graphstore.KindAsk, FromID: declined.ask, Kind: graphstore.TupleRemovedBy,
			ToKind: graphstore.KindPerson, ToID: declined.person,
			At: declined.at, Who: DecisionStepActor, SourceEvent: declined.event, Asserted: true,
		})
	}
	for _, restore := range r.restores {
		if restore.from == "" {
			continue
		}
		moved(restore.from, restore.address(r.spec))
		if restore.work.Shape == graphcensus.RestoreMoves && !restore.rowless {
			places, err := graphstore.ReadAskPlaces(ctx, r.tx, &r.spec.id, nil, &r.spec.id)
			if err != nil {
				return err
			}
			claims = append(claims, graphstore.AskClaims(restore.row.ID, places, DecisionStepActor, r.at, cause)...)
		}
	}
	type keepRow struct {
		block, sum, keptBy string
		keptAt             time.Time
	}
	var keepRows []keepRow
	for _, converted := range r.sections {
		heading := refs.BlockAddress(r.decisions.refKey, converted.section.headingID())
		switch converted.fate {
		case fateDecision:
			address := refs.BlockAddress(r.spec.refKey, converted.decisionID)
			moved(heading, address)
			if converted.keep == nil || converted.keep.keptBy == "" {
				continue
			}
			words, err := refs.DecisionWords(converted.decision)
			if err != nil {
				return err
			}
			sum := decisionread.TextSum(words)
			login := model.CanonicalLogin(converted.keep.keptBy)
			keeper := converted.keep.keeper
			person(login, r.at, cause)
			tuples = append(tuples, graphstore.Tuple{
				FromKind: refs.BlockNodeKind, FromID: address, Kind: refs.TupleKeptBy,
				ToKind: graphstore.KindPerson, ToID: login, At: converted.keep.keptAt, Who: keeper, SourceEvent: cause, Asserted: true,
			})
			keptSum := converted.keep.sumOr(sum)
			if converted.keep.matches {
				tuples = append(tuples, graphstore.Tuple{
					FromKind: refs.BlockNodeKind, FromID: address, Kind: refs.ValueKeptTextSum,
					ToKind: graphstore.KindValue, ToID: sum, At: converted.keep.keptAt, Who: keeper, SourceEvent: cause, Asserted: true,
				})
			}
			keepRows = append(keepRows, keepRow{block: converted.decisionID, sum: keptSum, keptBy: login, keptAt: converted.keep.keptAt})
		case fateReading, fateDropped:
			moved(heading, refs.BlockAddress(converted.askDocument.refKey, converted.askBlock))
		case fateRemovedAsk:
			tuples = append(tuples, graphstore.Tuple{
				FromKind: refs.BlockNodeKind, FromID: heading, Kind: graphstore.TupleMovedTo,
				ToKind: graphstore.KindAsk, ToID: converted.ask, At: r.at, Who: DecisionStepActor, SourceEvent: cause, Asserted: true,
			})
		}
	}
	if len(nodes)+len(tuples)+len(claims) > 0 {
		arms, params, err := graphstore.WriteArms(1, nodes, tuples, claims)
		if err != nil {
			return err
		}
		if _, err := r.tx.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
			return fmt.Errorf("state the moves, the removals and the Keeps of the restore of %s: %w", r.issue, err)
		}
	}
	for _, keep := range keepRows {
		if _, err := r.tx.Exec(ctx, `
			insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by, kept_at)
			values ($1::uuid, $2, $3, $4, $5, $6)
			on conflict (artifact_id, block_id) do update
				set text_sha256 = excluded.text_sha256, changed_version = excluded.changed_version,
					kept_by = excluded.kept_by, kept_at = excluded.kept_at
		`, r.spec.id, keep.block, keep.sum, r.result.SpecVersion, keep.keptBy, keep.keptAt); err != nil {
			return fmt.Errorf("project the Keep of a converted decision of %s: %w", r.issue, err)
		}
	}
	return nil
}

// sumOr is the sum the Keep's projection row holds once its decision is a node: the decision's own
// text sum where the Keep was of the section as it stands, and nothing it can match otherwise -
// the empty sum, which no decision's text has - so the decision reads waiting for review.
//
// kept for one release: decisions-step
func (keep *sectionKeep) sumOr(sum string) string {
	if keep.matches {
		return sum
	}
	return ""
}

// newestApproval is the spec's newest review where it is an approval, of whatever version, or nil:
// the approval the restore's version can carry (carryNewestApproval reads the same review), which
// an agent's move of an ask out of the spec left behind its own version, and which the restore
// carries back or supersedes (R7).
func newestApproval(ctx context.Context, tx pgx.Tx, artifactID string) (*model.SupersededApproval, error) {
	review, err := NewestReview(ctx, tx, artifactID)
	if err != nil || review == nil || review.State != "approved" {
		return nil, err
	}
	return &model.SupersededApproval{ReviewID: review.ID, ApprovedVersion: review.Version, ApprovedBy: review.Actor, Kind: review.Kind}, nil
}

// reportApprovals records what the pass did to each document's approval: the spec's approval -
// its newest review where that is an approval (newestApproval) - carried to the version the pass
// wrote, or, where the version does not carry it, superseded, with the asks whose restore wrote
// that version, each as its node and its block; and the decisions document's approval, which
// stood at its latest version and which the version the pass wrote of it superseded, with the
// asks it moved out.
//
// kept for one release: decisions-step
func (r *issueRestore) reportApprovals(ctx context.Context, spec, decisions *model.SupersededApproval) error {
	askRefs := make([]string, 0, len(r.restores))
	nodes := make([]string, 0, len(r.restores))
	var movedRefs, movedNodes []string
	for _, restore := range r.restores {
		if restore.rowless {
			continue
		}
		ask := refs.AskRef(text.Ref{IssueKey: r.issue, ID: restore.row.ID})
		askRefs = append(askRefs, ask)
		nodes = append(nodes, refs.BlockRef(r.spec.address, restore.id))
		if restore.from != "" {
			movedRefs = append(movedRefs, ask)
			movedNodes = append(movedNodes, refs.BlockRef(r.decisions.address, restore.work.BlockID))
		}
	}
	if r.result.SpecVersion > 0 {
		newest, err := NewestReview(ctx, r.tx, r.spec.id)
		if err != nil {
			return err
		}
		switch {
		case newest != nil && newest.State == "approved" && newest.Version == r.result.SpecVersion:
			r.result.SpecApproval = "carried"
		case spec != nil:
			r.result.SpecApproval = "superseded"
			r.result.Superseded = append(r.result.Superseded, DecisionSupersededApproval{
				Document: r.spec.address, Approval: *spec, Asks: askRefs, Nodes: nodes,
			})
		}
	}
	if r.result.DecisionsVersion > 0 && decisions != nil {
		r.result.Superseded = append(r.result.Superseded, DecisionSupersededApproval{
			Document: r.decisions.address, Approval: *decisions, Asks: movedRefs, Nodes: movedNodes,
		})
	}
	return nil
}
