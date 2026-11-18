package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
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

// The decisions step's third phase (dispatch://INFRA-1150, "The step"): for each issue, in one
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
var DecisionStepActor = model.Actor{Kind: "system", ID: "dispatch-decisions-step"}

// DecisionIssue is what the third phase did for one issue, or, under a dry run, would have done.
type DecisionIssue struct {
	Issue string
	// Restored is each answered ask the restore returned to the spec, in answer-time order.
	Restored []RestoredAsk
	// Decisions, Readings, Dropped and Left count the sections of the issue's decisions document by
	// what became of each: a decision block, a reading under an ask, a section a rule of R9
	// deleted, and a section no rule converts, which stays where it is.
	Decisions, Readings, Dropped, Left int
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

// RestoredAsk is one answered ask the restore returned: its id, its shape
// (graphcensus.RestoreReturns, RestoreMoves), the address of the block it moved from where it
// moved (a block of the decisions document, which a moved_to tuple now names), "" where it
// returns under the spec's own address, and the address of the block it stands at now, with
// whether that block took a new id.
type RestoredAsk struct {
	Ask, Shape, From, To string
	NewID                bool
}

// DecisionSupersededApproval is an approval the phase superseded: the document's address, the
// approval, and the asks the restore wrote whose versions superseded it, each as the ask's node
// (dispatch://KEY/ask/<id>) and its block's address.
type DecisionSupersededApproval struct {
	Document string
	Approval model.SupersededApproval
	Asks     []string
	Nodes    []string
}

// issueDocument is one document of an issue the phase writes: its id, its ref_key (the store's
// address of it), its reference (dispatch://…) and its name.
type issueDocument struct {
	id, refKey, address, name string
}

// readIssueDocument reads the issue's document query answers, by id, in tx.
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
const issueDocumentColumns = `a.id::text, a.ref_key, a.issue_key, coalesce(a.project_key, ''), a.session_id, a.slug, a.kind, a.is_primary, a.name`

// restoredBlock is one answered ask the restore writes into the spec: the work the finder read
// (graphcensus.ReadRestoreWork), the ask's row, the id its block takes, the address it moves from
// (from: for the second shape the address the finder read, and for the main shape the address of
// a copy of the block the decisions document holds, "" where it holds none), the block of a
// document of the issue it moves out of where that document still holds it (moved), the spec
// blocks it shaped, and the readings it holds.
type restoredBlock struct {
	work     graphcensus.RestoreWork
	row      model.Ask
	id       string
	from     string
	moved    *pmdoc.Node
	shaped   []string
	readings []*pmdoc.Node
}

// address is the block's address in the spec once restored.
func (r *restoredBlock) address(spec issueDocument) string {
	return storeBlockAddress(spec.refKey, r.id)
}

// storeBlockAddress is the store's address of block id of the document whose ref_key is refKey,
// as the block index keys it: a U+0000 reads as U+FFFD, as the rendering writes it.
func storeBlockAddress(refKey, id string) string {
	return refKey + "#" + strings.ReplaceAll(id, "\x00", "\uFFFD")
}

// sectionFate is what becomes of one section of the decisions document.
type sectionFate int

const (
	// fateUntouched is a section this pass does not convert: one a restore alone leaves for the
	// conversion.
	fateUntouched sectionFate = iota
	// fateDecision is a section that cites no answered ask: it becomes a decision block.
	fateDecision
	// fateReading is a section that cites an answered ask: its body becomes a reading in that
	// ask's block.
	fateReading
	// fateDropped is a section that cites an answered ask and whose body is that ask's answer and
	// nothing more, or nothing at all beside the ask's own block: R9 deletes it.
	fateDropped
	// fateLeft is a section no rule converts: it stays, and is counted.
	fateLeft
)

// convertedSection is one section of the decisions document and what becomes of it: its fate,
// the ask it cites (fateReading, fateDropped), the block it becomes (fateDecision: the decision,
// under decisionID), the author of its newest version, and the Keep a person recorded on its
// heading.
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
type sectionKeep struct {
	keptBy  string
	keeper  model.Actor
	keptAt  time.Time
	matches bool
}

// issueRestore is the third phase's work on one issue, in the transaction it holds.
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
	// present is the shaped pointers and the readings this pass writes into an answered ask block
	// a document of the issue holds already, by the document's id.
	present  map[string]*presentEdits
	sections []*convertedSection
	// sectionsLeft is how many sections the decisions document holds once this pass has written
	// it, and decisionsHoldAsks whether it still holds an ask block then, in a section or outside
	// every one: an ask block this pass does not move - one whose row moved since the finder read
	// it, or one no row names - which keeps the document from its retirement (R6).
	sectionsLeft      int
	decisionsHoldAsks bool
	// pointerLine is the markdown of the pointer line the spec's write took out of its
	// `## Decisions` part (blockMarkdown), "" for none, and shapedAdded each `shaped` pointer
	// the pass's writes gave an answered ask block a document held already, by the document's id
	// and then the block's: with the blocks the spec's write added, what each version's mark names
	// (stepCasesFor).
	pointerLine string
	shapedAdded map[string]map[string][]string
	// readingsWritten is the block id of each reading the pass's writes put in an ask block a
	// document held already, by the document's id (writePresent): a section converted into a
	// reading whose reading is not among them was written nowhere (refuseUnwrittenReadings).
	readingsWritten map[string]map[string]bool
	result          *DecisionIssue
}

// presentEdits is what this pass writes into the answered ask blocks one document holds already:
// the spec blocks each one shaped and the readings it takes, by block id.
type presentEdits struct {
	shaped   map[string][]string
	readings map[string][]*pmdoc.Node
}

// presentIn is the edits this pass writes into document's answered ask blocks.
func (r *issueRestore) presentIn(document string) *presentEdits {
	edits := r.present[document]
	if edits == nil {
		edits = &presentEdits{shaped: map[string][]string{}, readings: map[string][]*pmdoc.Node{}}
		r.present[document] = edits
	}
	return edits
}

// restoreIssueDecisions runs the third phase on issue in tx, which ctx joined (Service.Join):
// the restore of work, the answered asks and the decisions document's ask blocks the finder read
// for it, and, where convert, the conversion of its decisions document and that document's
// retirement once it holds no section and no ask block. It writes nothing that it does not report,
// and its caller commits or rolls back: a pass that would take a section out of the decisions
// document without writing the reading it became fails (refuseUnwrittenReadings).
func (s *Service) restoreIssueDecisions(ctx context.Context, tx pgx.Tx, issue string, work []graphcensus.RestoreWork, convert bool) (DecisionIssue, error) {
	result := DecisionIssue{Issue: issue}
	// The step's own write, which alone the restore's and the conversion's carry cases reach
	// (restoreCase, conversionCase): every version this pass writes carries the step's mark, with
	// what this pass added to that document (writeVersion, stepCasesFor).
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
	decisions, found, err := readIssueDocument(ctx, tx, `
		select `+issueDocumentColumns+` from decisions_documents() d join artifacts a on a.id = d.id
		where d.issue_key = $1 order by a.created_at, a.id limit 1
	`, issue)
	if err != nil {
		return result, fmt.Errorf("read the decisions document of %s: %w", issue, err)
	}
	if found {
		r.decisions = &decisions
	}
	// The issue's row first, as every writer of its documents takes it (lockArtifactOwner), then
	// the rows of its asks, then each document's live write.
	if _, _, err := lockArtifactOwner(ctx, tx, spec.id); err != nil {
		return result, err
	}
	if err := r.readState(ctx); err != nil {
		return result, err
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
		collectEvent(ctx, retirement.Event)
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
// locked as a writer of an ask's row takes it (`for no key update`), and which of them hold an
// answer on row and node (answered), every document of the issue that stands, and the blocks
// their block indexes hold: every block of the spec, and every ask block of each.
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

// planRestores turns the finder's work into the blocks the restore writes, each item read again
// under the issue's lock, since the finder read the whole pass's work before it took any: each
// ask still as the finder read it once its row is locked - its block where the finder saw it, and,
// for a returning ask, answered - keeps its block id unless a block of the spec holds that id as
// the spec's block index holds it now, when it takes a fresh one no document of the issue holds
// (R6's collision rule). An ask block of the decisions document moves whatever its ask's state
// (D1). A work item whose row moved since the finder read it is left to the next pass, and so is a
// returning ask whose block the spec holds again, which a person put back: a restore of it would
// write a second block of that ask and leave the person's with no row. An ask the restore moves
// out of the decisions document moves from the address the finder read (RestoreWork.Address),
// which its moved_to and its node's relations name whether or not the document still holds the
// block (writeFacts).
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
		case graphcensus.RestoreMoves:
			if r.decisions == nil || *row.BlockArtifactID != r.decisions.id {
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
	// Answer-time order, as the restored blocks stand in the spec's `## Decisions` part.
	sort.SliceStable(r.restores, func(a, b int) bool {
		return answeredAt(r.restores[a].row).Before(answeredAt(r.restores[b].row))
	})
	return nil
}

// specRowHolds reports whether the row of an ask of the issue other than ask names the spec's
// block id, which asks_block_id_unique keeps to one row: an ask a person declined or a settlement
// retracted keeps its row naming the block it left, so the restored ask takes a fresh id there as
// it does where a live block of the spec holds the id (R6).
func (r *issueRestore) specRowHolds(id, ask string) bool {
	for _, row := range r.rows {
		if row.ID != ask && row.BlockArtifactID != nil && *row.BlockArtifactID == r.spec.id && row.BlockID != nil && *row.BlockID == id {
			return true
		}
	}
	return false
}

// answeredAt is when ask was answered, the zero time for an ask its row holds no answer of.
func answeredAt(ask model.Ask) time.Time {
	if ask.Answer == nil {
		return time.Time{}
	}
	return ask.Answer.At
}

// freshBlockID is a block id no document of the issue holds, as a block or as an ask's row.
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
// the typed-node pass) and the decisions document's no longer names it.
func (r *issueRestore) moveRows(ctx context.Context) error {
	for _, restore := range r.restores {
		if *restore.row.BlockID == restore.id && *restore.row.BlockArtifactID == r.spec.id {
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
func (r *issueRestore) restoreOf(ask string) *restoredBlock {
	for _, restore := range r.restores {
		if restore.row.ID == ask {
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
// writes its version. It records whether the document still holds an ask block once those are
// taken out (decisionsHoldAsks), which keeps it from its retirement. It answers the event of that
// version, NoEvent where it wrote none.
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
		left, _ := withoutBlocks(tree, dropped)
		r.decisionsHoldAsks = pmdoc.HoldsAskBlock(left)
		if len(drop) == 0 {
			return nil
		}
		_, err = rewriteLive(doc, nil, func(live *pmdoc.Node) bool {
			next, took := withoutBlocks(live, dropped)
			if !took {
				return false
			}
			live.Children = next.Children
			return true
		})
		return err
	})
	if errors.Is(err, websocket.ErrNoChanges) || err == nil && len(drop) == 0 {
		return graphstore.NoEvent, nil
	}
	if err != nil {
		return graphstore.NoEvent, err
	}
	return r.writeVersion(ctx, *r.decisions, &r.result.DecisionsVersion)
}

// ownedByDecisions reports whether an ask names a block of the decisions document under id as its
// own: an ask this pass moves out of it, or one whose row names that document.
func (r *issueRestore) ownedByDecisions(id string) bool {
	for _, restore := range r.restores {
		if restore.work.Shape == graphcensus.RestoreMoves && restore.work.BlockID == id {
			return true
		}
	}
	for _, row := range r.rows {
		if row.BlockArtifactID != nil && *row.BlockArtifactID == r.decisions.id && row.BlockID != nil && *row.BlockID == id {
			return true
		}
	}
	return false
}

// planSections reads tree, the decisions document as its live write holds it, into what this
// pass does with it, and answers the block ids the document loses: each ask block the restore
// moves into the spec, and every block of each section the pass converts.
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
				restore.from = storeBlockAddress(r.decisions.refKey, id)
			}
			movedBy[id] = restore
			drop[id] = true
			return false
		})
	}
	sections := decisionSectionsOf(tree)
	ties, err := readSectionTies(ctx, r.tx, r.decisions.id, r.spec.id)
	if err != nil {
		return nil, err
	}
	authors, err := r.sectionAuthors(ctx, tree, sections)
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
		}
		for _, id := range section.blockIDs() {
			drop[id] = true
		}
	}
	return drop, nil
}

// planSection decides what becomes of one section (R9), and records the spec blocks it links as
// the `shaped` pointers of the ask it restates.
//
// The ask a section restates is the ask whose block it holds, which the restore moves whatever the
// ask's state (D1), or else the one answered ask of the issue it cites. A section that restates one
// and that this pass converts gives that ask's block - the one the restore writes into the spec,
// or the one a document of the issue holds already (askBlockOf) - its links as `shaped` pointers
// where the ask is answered, and its body, the ask's moved block taken out, becomes a reading in
// that block; a body that is the ask's answer and nothing more, or nothing at all, is dropped, and
// writes no reading (the plain-answer rule). A section that cites no answered ask becomes a
// decision. A section that restates two asks, or whose body a reading cannot hold (a reading holds
// paragraphs alone, and only in an answered ask: a reading reads an answer), is left where it is:
// no rule converts it (plan v15, "A section that no rule converts"); so is one whose ask has a
// block in no document of the issue, which nothing could hold its reading; and so is one whose
// body, the blocks the restore moves taken out, still holds an ask block, wherever in it - one
// whose row moved since the finder read it, or one no row names: a conversion of the section would
// take that block out of the document and write it nowhere (R6, R8).
//
// A restore alone converts only a section that holds a block it moves, so no section is left
// holding nothing but its heading; it still gives a restored answered ask the links of every
// section that restates it, as R6 says.
func (r *issueRestore) planSection(ctx context.Context, converted *convertedSection, ties sectionTies, movedBy map[string]*restoredBlock) error {
	section := converted.section
	var held []string
	body := make([]*pmdoc.Node, 0, len(section.body))
	for _, block := range section.body {
		id, _ := block.Attrs[pmdoc.BlockIDAttr].(string)
		if restore, moves := movedBy[id]; moves && block.Type == "ask" {
			held = append(held, restore.row.ID)
			continue
		}
		body = append(body, block)
	}
	if slices.ContainsFunc(body, pmdoc.HoldsAskBlock) {
		converted.fate = r.leftUnlessRestoring(held)
		return nil
	}
	var cited []string
	for _, ask := range tiesOf(section, ties.asks) {
		if row, held := r.rows[ask]; held && row.State == asks.StateAnswered {
			cited = append(cited, ask)
		}
	}
	links := tiesOf(section, ties.spec)
	converted.links = links
	var ask string
	switch {
	case len(held) == 1:
		ask = held[0]
	case len(held) > 1 || len(cited) > 1:
		converted.fate = r.leftUnlessRestoring(held)
		return nil
	case len(cited) == 1:
		ask = cited[0]
	}
	if ask == "" {
		if !r.convert {
			return nil
		}
		return r.planDecision(ctx, converted, body)
	}
	converted.ask = ask
	document, blockID, stands := r.askBlockOf(ask)
	if !stands {
		converted.fate = r.leftUnlessRestoring(held)
		return nil
	}
	converted.askDocument, converted.askBlock = document, blockID
	// An ask that holds no answer takes no pointer: what an answer shaped is the answer's.
	answered := r.answered[ask]
	switch restore := r.restoreOf(ask); {
	case !answered:
	case restore != nil:
		restore.shaped = appendUnique(restore.shaped, links...)
	case r.convert:
		edits := r.presentIn(document.id)
		edits.shaped[blockID] = appendUnique(edits.shaped[blockID], links...)
	}
	if !r.convert && len(held) == 0 {
		return nil
	}
	switch {
	case len(body) == 0 || answered && plainAnswer(body, r.rows[ask]):
		converted.fate = fateDropped
	case answered && allParagraphs(body):
		reading, err := readingOf(body)
		if err != nil {
			return err
		}
		converted.fate, converted.reading = fateReading, reading
		if restore := r.restoreOf(ask); restore != nil {
			restore.readings = append(restore.readings, reading)
		} else {
			edits := r.presentIn(document.id)
			edits.readings[blockID] = append(edits.readings[blockID], reading)
		}
	default:
		converted.fate = r.leftUnlessRestoring(held)
	}
	return nil
}

// leftUnlessRestoring is fateLeft for a section the conversion reads, and fateUntouched for one a
// restore alone reads and holds no block it moves.
func (r *issueRestore) leftUnlessRestoring(held []string) sectionFate {
	if !r.convert && len(held) == 0 {
		return fateUntouched
	}
	return fateLeft
}

// planDecision makes converted, a section that cites no answered ask, a decision block of the
// spec's `## Decisions` part: its words are the section's markdown with its heading as the first
// paragraph; an `Unsure:` paragraph becomes uncertainty="high", with the rest of it as
// uncertainty_reason; a section under `## Replaced` is replaced="true"; its links are its shaped
// pointers. It keeps the section's heading block id where the spec holds no block under it.
func (r *issueRestore) planDecision(ctx context.Context, converted *convertedSection, body []*pmdoc.Node) error {
	section := converted.section
	id := section.headingID()
	if id == "" || r.specBlocks[id] || r.restoreHolds(id) {
		var err error
		if id, err = r.freshBlockID(ctx); err != nil {
			return err
		}
	}
	attrs := pmdoc.Attrs{pmdoc.BlockIDAttr: id}
	unsure, reason := section.unsure()
	words := []*pmdoc.Node{{Type: "paragraph", Attrs: pmdoc.Attrs{}, Children: section.heading.Children}}
	for _, block := range body {
		if block != unsure {
			words = append(words, block)
		}
	}
	if unsure != nil {
		attrs["uncertainty"] = "high"
		if reason != "" {
			attrs["uncertainty_reason"] = reason
		}
	}
	if section.replaced {
		attrs["replaced"] = true
	}
	if len(converted.links) > 0 {
		attrs[shapedAttribute] = r.specPointers(converted.links)
	}
	decision, err := parsedTypedBlock(&pmdoc.Node{Type: decisionType, Attrs: attrs, Children: words}, decisionType)
	if err != nil {
		return fmt.Errorf("write the section %q of the decisions document of %s as a decision: %w", section.title, r.issue, err)
	}
	converted.fate, converted.decision, converted.decisionID = fateDecision, decision, id
	return nil
}

// restoreHolds reports whether a block this pass restores takes id.
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
func (r *issueRestore) specPointers(ids []string) []string {
	pointers := make([]string, 0, len(ids))
	for _, id := range ids {
		pointers = appendUnique(pointers, refs.BlockRef(r.spec.address, id))
	}
	return pointers
}

// appendUnique appends each of values to list that it does not hold yet.
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
func normalizedWords(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

// allParagraphs reports whether every block of body is a paragraph, which is all a reading holds.
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

// readingType is the type of an agent's reading block (dispatch://INFRA-1150 R2).
const readingType = "reading"

// parsedTypedBlock is node, a typed block of typ built here, as the document's own markdown
// pipeline makes it (parsedBlockBody): rendered and read back, so it carries the attributes and
// the inner block ids a parse gives rather than ones a hand-built node invents, with its own id,
// which its markdown writes, kept.
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
// the section (refs.BlockChanges), as the Decisions read of the release before dated a section.
// A section whose blocks the index records no change of, or whose version names no author, has
// none.
func (r *issueRestore) sectionAuthors(ctx context.Context, tree *pmdoc.Node, sections []decisionSection) ([]model.Actor, error) {
	changes, err := refs.BlockChanges(ctx, r.tx, r.decisions.id)
	if err != nil {
		return nil, err
	}
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
// the part's first paragraph that cites the document itself (citesDocument), every other
// paragraph kept, one citing a section of the document among them - or after the part's last
// block where it holds none, and under a `## Decisions` heading the write adds where the spec
// holds no such part; and, in each answered ask block it holds already, the shaped pointers and
// the readings the conversion gives it. It records the pointer line it took out (pointerLine),
// which its version's mark names. On a fleet of one build - which the phase runs on alone - the
// same write takes the server-owned attributes out of every ask block of the spec
// (withoutStoredAskAttributes), as the first accepted write of the document does (R10). It
// answers the event of the version it wrote, NoEvent where the spec did not change.
func (r *issueRestore) writeSpec(ctx context.Context) (int64, error) {
	var added []*pmdoc.Node
	for _, restore := range r.restores {
		block, err := r.restoredAskBlock(restore)
		if err != nil {
			return graphstore.NoEvent, err
		}
		added = append(added, block)
		r.result.Restored = append(r.result.Restored, RestoredAsk{
			Ask: restore.row.ID, Shape: restore.work.Shape, From: restore.from,
			To: restore.address(r.spec), NewID: restore.id != restore.work.BlockID,
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
	twoBuilds, err := anotherBuildServes(ctx, r.tx)
	if err != nil {
		return graphstore.NoEvent, err
	}
	var refused error
	var pointerLine string
	err = r.service.applyLive(ctx, r.spec.id, DecisionStepActor, func(doc *crdt.Doc, current pmdoc.ValidDocument, _ func(func(*crdt.Transaction))) error {
		if current.Node() == nil {
			return fmt.Errorf("the spec of %s is outside the Proof schema, so the decisions step cannot write into it", r.issue)
		}
		_, err := rewriteLive(doc, nil, func(live *pmdoc.Node) bool {
			for _, block := range added {
				id, _ := block.Attrs[pmdoc.BlockIDAttr].(string)
				if blockIDHeld(live, id) {
					refused = fmt.Errorf("the spec of %s holds block %s already, which the decisions step writes a block under", r.issue, id)
					return false
				}
			}
			changed := r.writePresent(r.spec.id, live, r.present[r.spec.id], twoBuilds)
			pointerLine = ""
			if len(added) > 0 {
				var line *pmdoc.Node
				live.Children, line = withDecisionsPartBlocks(live.Children, added, func(node *pmdoc.Node) bool {
					return citesDocument(node, pointers, r.service.serverURL)
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
	r.pointerLine = pointerLine
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
// write holds it, and, on a fleet of one build (twoBuilds false), takes the server-owned
// attributes out of every ask block of it (withoutStoredAskAttributes). It records each pointer it
// gave a block that the block did not hold (shapedAdded), which the version's mark names, and each
// reading it wrote (readingsWritten), and reports whether it changed the tree. It visits every ask
// block of the tree: pmdoc.Walk stops at the first node its visit answers false for, so the visit
// answers true at an ask block too, and the blocks after the first are written as well.
func (r *issueRestore) writePresent(document string, live *pmdoc.Node, edits *presentEdits, twoBuilds bool) bool {
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
		if !twoBuilds && withoutStoredAskAttributes(node) {
			changed = true
		}
		return true
	})
	r.shapedAdded[document] = added
	r.readingsWritten[document] = written
	return changed
}

// refuseUnwrittenReadings fails the pass where a section it converts into a reading of an ask
// block a document held already has that reading in no block the pass wrote (readingsWritten):
// the decisions document's write has taken the section out by then, and the moved_to the pass
// would state from its heading names a block that holds none of its words. The failure rolls the
// issue's transaction back, so the section stays where it stood, and no moved_to names it. A
// reading of an ask the pass restores is in the block restoredAskBlock builds, which holds it.
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
func (r *issueRestore) writePresentDocuments(ctx context.Context) error {
	twoBuilds, err := anotherBuildServes(ctx, r.tx)
	if err != nil {
		return err
	}
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
			_, err := rewriteLive(doc, nil, func(live *pmdoc.Node) bool {
				return r.writePresent(id, live, edits, twoBuilds)
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

// restoredAskBlock is the block restore writes into the spec, built from the ask's row - its
// question and options as the row holds them, urgency and multiple - and its node, which holds
// the answer: the block shows no answer (R6). It takes the spec blocks the sections that restate
// it link as its `shaped` pointers, and holds the readings the block it moves held and the ones
// the conversion writes. It reads back as the row's wording (askBlock.asks), or is refused.
func (r *issueRestore) restoredAskBlock(restore *restoredBlock) (*pmdoc.Node, error) {
	budget := pmdoc.NewWriteBudget()
	decided := model.NewAskEditPrevious(restore.row)
	children, err := questionBlocks(normalizeAskQuestion(decided.Question), budget)
	if err != nil {
		return nil, fmt.Errorf("write the question of ask %s: %w", restore.row.ID, err)
	}
	if list, err := editedOptionList(nil, normalizeAskOptions(decided.Options), budget); err != nil {
		return nil, fmt.Errorf("write the options of ask %s: %w", restore.row.ID, err)
	} else if list != nil {
		children = append(children, list)
	}
	if restore.moved != nil {
		_, held := pmdoc.TypedContent(restore.moved)
		children = append(children, held...)
	}
	children = append(children, restore.readings...)
	attrs := pmdoc.Attrs{pmdoc.BlockIDAttr: restore.id, "urgency": decided.Urgency, "multiple": decided.Multiple}
	if len(restore.shaped) > 0 {
		attrs[shapedAttribute] = r.specPointers(restore.shaped)
	}
	block, err := parsedTypedBlock(&pmdoc.Node{Type: "ask", Attrs: attrs, Children: children}, "ask")
	if err != nil {
		return nil, fmt.Errorf("write the block of ask %s: %w", restore.row.ID, err)
	}
	read, err := parseAskBlock(block)
	if err != nil || !read.asks(decided) {
		return nil, fmt.Errorf("the restored block of ask %s does not read back as the wording its row holds", restore.row.ID)
	}
	return block, nil
}

// writeVersion writes the version of document the pass's live write left (SnapshotVersion), under
// the step's mark naming what the pass added to document (stepCasesFor), and its artifact.version
// event, under which the version's block index states its facts (graphstore.Caused) and its
// references are stamped, as every version a route writes is. It records the version's number in
// *number and answers the event, NoEvent where no version was written.
func (r *issueRestore) writeVersion(ctx context.Context, document issueDocument, number *int) (int64, error) {
	written, err := r.service.SnapshotVersion(withDecisionStepWrite(ctx, r.stepCasesFor(document.id)), document.id, DecisionStepActor)
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
	collectEvent(ctx, event)
	return event.ID, nil
}

// stepCasesFor is the mark of the version of document this pass writes (decisionStepCases): the
// restore's phase, the conversion's where it converts, and what the pass added to document - for
// the spec, the ask blocks the restore wrote, the decisions the conversion wrote and the pointer
// line it took out; for any document, the `shaped` pointers it gave the answered ask blocks the
// document held already. The restore's and the conversion's carry cases take out only those.
func (r *issueRestore) stepCasesFor(document string) decisionStepCases {
	steps := decisionStepCases{restore: true, conversion: r.convert, shaped: r.shapedAdded[document]}
	if document != r.spec.id {
		return steps
	}
	steps.restored, steps.decisions = map[string]bool{}, map[string]bool{}
	for _, restore := range r.restores {
		steps.restored[restore.id] = true
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
// heading's address to the decision it became or the ask block it restates (R8); and for each
// decision whose section a person kept, the Keep on its node - a kept_by tuple, and a
// kept_text_sum value where the Keep's sum is the section's as it stands (F3) - with the
// decision_keeps row that projects it, as Keep writes one. A section a restore alone took out
// writes its moved_to too.
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
	for _, restore := range r.restores {
		if restore.from == "" {
			continue
		}
		moved(restore.from, restore.address(r.spec))
		if restore.work.Shape == graphcensus.RestoreMoves {
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
		heading := storeBlockAddress(r.decisions.refKey, converted.section.headingID())
		switch converted.fate {
		case fateDecision:
			address := storeBlockAddress(r.spec.refKey, converted.decisionID)
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
			nodes = append(nodes, graphstore.PersonNode(login, DecisionStepActor, r.at, cause))
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
			moved(heading, storeBlockAddress(converted.askDocument.refKey, converted.askBlock))
		}
	}
	if len(nodes)+len(tuples)+len(claims) > 0 {
		arms, params, err := graphstore.WriteArms(1, nodes, tuples, claims)
		if err != nil {
			return err
		}
		if _, err := r.tx.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
			return fmt.Errorf("state the moves and the Keeps of the restore of %s: %w", r.issue, err)
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
func (r *issueRestore) reportApprovals(ctx context.Context, spec, decisions *model.SupersededApproval) error {
	askRefs := make([]string, 0, len(r.restores))
	nodes := make([]string, 0, len(r.restores))
	var movedRefs, movedNodes []string
	for _, restore := range r.restores {
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
