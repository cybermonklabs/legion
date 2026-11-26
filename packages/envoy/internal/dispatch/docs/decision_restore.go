package docs

import (
	"context"
	"errors"
	"fmt"
	"time"

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
