package docs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A human's answer to a decision block carries his approval forward. He approved the document at
// version A; the version his answer writes, N, differs from A only in its ask blocks, every block
// of N that is not as A held it - added since, answered or re-answered since, or resolved since -
// is one he answered, after his approval, and at least one is: nothing he has not agreed to is in
// N, and something of his is, so Dispatch records his approval of N in his name, in the
// transaction that writes N, rather than leaving his approval stale and asking him to approve
// again what he just decided (LEGION-573). N is compared with A alone: the versions between them
// are read for nothing, so a block that left and came back as A held it is no change of his, and a
// block A did not hold that comes back with an answer he gave before A is his but no action of his
// since, so it carries nothing either.
//
// Who answered what is read from the asks rows of this document, never from N's blocks: a peer on
// the document socket writes a block's attributes as it likes, and settlement writes them back
// from the row only for a block it can read. A block of N that no row of this document backs - a
// forged one, a copy (copied_from, whose row is another document's), or one whose update the room
// has not credited yet - or one settlement could not read (invalid), or a block id N repeats,
// which settlement reads as no block at all, leaves N's decisions unknown, and nothing is carried.
// Nor is anything carried while a block's server attributes are other than settlement's own
// writer (setAskServerAttributes) writes from its row - its state, who answered and when, and the
// answer itself, which the browser shows as the human's decision - save an answer settlement
// withheld (withholdAnswers), or while a decided block asks other wording than its row was decided
// on, which settlement writes back (reconcileAskBlocks): until it does, the block shows a decision
// nobody made, or one under words nobody decided.
//
// The version he approved, A, is held to the same row as N. A block of N is as A held it only
// while A holds a block under its id, read as N's blocks are (askNodes), and that stored block is
// as settlement's own writer leaves it from the row of N's block (blockAsSettlementLeavesIt: the
// whole server-owned set, and a decided row's wording, as for N). A stored block that is not -
// one showing a selection, an answer, a copy's provenance or a question other than its row's,
// approved before settlement wrote the row back over it - is a change of N's, which must then be
// his own answer since his approval, or nothing is carried. The row is the row at N's write, the
// only row there is, and holding A's block to it is exact: a decided row's wording is frozen (the
// edit route's ASK_NOT_OPEN), its selection and text move only with a new answer, which moves
// answered_at, and its state moves only with a resolution, so a stored block that is as the
// current row has it over the whole set was as the row stood when A was written, and one the row
// has moved away from since fails on answered_at or state (LEGION-752). A stored A that held one
// block id twice carries nothing either: its read-back (storedVersionTree) repairs the repeat,
// keeping the id on the first block and minting one for the next, so A shows a block N never had,
// and a block of A that no longer stands in N refuses the carry.
//
// A version whose text is the approved text carries that approval too. The document's newest
// review approves version A; a later version, N, is stored with A's markdown byte for byte - an
// accidental edit undone exactly, or a version a route wrote over a document that changed nothing -
// and each decision block of N is as its row has it (rowBackedBlocks), so no block in it shows
// other than what was decided: a block A showed open that someone answered since comes back in A's
// bytes while its row holds that answer, and carries nothing. Such an N holds nothing its
// reviewer has not agreed to, so Dispatch records that reviewer's approval of N in their name
// and of their review's own kind, in the transaction that writes N, rather than asking them to
// approve A's text again: the newest review of a plan.md is the plan review that approved A, so
// a restored plan version reads approved by that plan review and not by the session whose write
// restored it. Only the newest review carries this way: when it requests changes no approval
// stands, and an older approval was superseded by it.
// A's markdown is compared as it was stored, so a renderer change between A and N leaves N a
// change, as N's diff against A shows. A version whose decision blocks differ from A's does not
// hold A's text, so the answer carry decides it, or the rule below where the blocks it changes
// are questions open again.
//
// A version whose text is the approved text with questions open again carries that approval too.
// An issue's close withdraws every open ask of the issue and writes each one's block resolved, a
// settlement while the issue is closed saves that as a version, and the reopen restores those asks
// and writes their blocks open, which the settlement after the reopen saves as the next version.
// A person who approves the document between the reopen and that settlement - from its header,
// where the review names the latest version - approves the closed issue's version, A, and the next
// version, N, differs from it only in the blocks the reopen wrote open. Before a closed issue's
// document settled, no version came of the close, such an approval reviewed the version from
// before it, and the settlement after the reopen, which rendered that version's text again, wrote
// none, so the approval stood. So N carries the newest review's approval of A when every decision
// block of N is as its row has it (rowBackedBlocks) and N, with each block whose row is open shown
// resolved as the close shows it (withOpenBlocksResolved), renders to A's stored text byte for
// byte, as the restore carries it, in the reviewer's name and of their review's kind. A question
// that A did not hold, or held open, makes that rendering differ from A and carries nothing: a
// question added since the approval is no question the reviewer saw. Every decision block a
// version of the closed issue holds reads answered or resolved: the close withdrew every open ask,
// and a settlement while the issue is closed saves a block with no ask resolved. So a
// block whose row is open in N and that A shows resolved is one the reopen, or a settlement after
// it, opened, since no other write gives a resolved ask back its open state, and the rule carries
// to N only the questions the reopen opened again.

// carryReason is the reason a carried review records, naming the approval it follows.
const carryReason = "Carried forward from the approval of version %d: the answer to a decision block was the whole change since it."

// restoreReason is the reason a review carried to a version restoring the approved text records,
// naming the approval it follows. It opens as carryReason does, which is how the Legion daemon
// tells a carry from a person's own approval (intake.CarriedApprovalReasonPrefix).
const restoreReason = "Carried forward from the approval of version %d: this version's text is that version's, unchanged."

// reopenReason is the reason a review carried to a version holding the approved text with
// questions open again records (reopenedApproval), naming the approval it follows. It opens as
// carryReason does.
const reopenReason = "Carried forward from the approval of version %d: this version's text is that version's, with the questions it showed resolved open again."

// carryInput is what the rule is decided on once H, the version he approved, A, and when he
// approved it are known: the blocks of N by block id, each its row (rowBackedBlocks); the block
// ids of A, each whether its stored block is as that row has it (heldAsItsRow); and the two
// versions cut of their decision blocks (CutDecisionBlocks).
type carryInput struct {
	login            string
	approvedAt       time.Time
	blocks           map[string]model.Ask
	approved         map[string]bool
	approvedCut, cut string
}

// approvedVersion is a version a candidate approved as the rule reads it: its ask nodes as its
// stored markdown holds them, read as N's are (askNodes, each block id once), and its cut. Its
// nodes are held to the rows of N's blocks once those are known (heldAsItsRow): the rule for A
// is the rule for N, over the same rows.
type approvedVersion struct {
	nodes map[string]*pmdoc.Node
	cut   string
}

// carryApproval carries a human's approval to version, just written from rendered, when his answer
// to a decision block is the whole change since the version he approved, when version's text is
// the text of the version the document's newest review approved, or when it is that text with
// questions open again (the rules above). It returns the events it appended - the open approval
// request's ask.answered, when one is open, then artifact.approved - and nothing when the version
// carries no approval. A version he approved that holds an element this build does not know is a
// NewerContentError, which fails the version write (skipUnreadableVersion). It reads rendered.tree
// and never changes it: the tree is the live document's.
func (s *Service) carryApproval(ctx context.Context, tx pgx.Tx, artifactID string, version model.Version, rendered renderedTree) ([]model.Event, error) {
	// Every version write runs this: anything past the newest review is read only once a carry is
	// possible at all, which needs an approval still standing. A newer request for changes than
	// every approval leaves none: the answer carry's candidates each fail on it, and the restore
	// and the reopen carry read the newest review alone.
	newest, err := NewestReview(ctx, tx, artifactID)
	if err != nil || newest == nil || newest.State != "approved" {
		return nil, err
	}
	return s.carryStandingApproval(ctx, tx, artifactID, version.Number, rendered, *newest)
}

// carryStandingApproval is carryApproval once newest, the document's newest review, is known to be
// an approval: the answer carry first, then the restore when the answer carries nothing, and then
// the reopen carry when the restore carries nothing either.
func (s *Service) carryStandingApproval(ctx context.Context, tx pgx.Tx, artifactID string, number int, rendered renderedTree, newest model.ArtifactReview) ([]model.Event, error) {
	review, blocks, err := s.answerCarriedReview(ctx, tx, artifactID, number, rendered)
	if err != nil {
		return nil, err
	}
	if review != nil {
		return writeCarriedReview(ctx, tx, s.events, artifactID, number, *review, fmt.Sprintf(carryReason, review.Version), s.serverURL)
	}
	restored, err := restoreApproval(ctx, tx, s.events, artifactID, number, rendered.markdown, rendered.tree, newest, s.serverURL)
	if err != nil || len(restored) > 0 {
		return restored, err
	}
	return reopenedApproval(ctx, tx, s.events, artifactID, number, rendered, blocks, newest, s.serverURL)
}

// CarryRestoredApproval is the restore alone, for the route that writes a document version itself
// rather than through writeVersionTx - an upload that replaces a document (the api package's
// storeArtifact) - once that version, number, is written with markdown and the open approval
// request has moved to it (MoveApprovalAsk). tree is markdown's read-back, which the caller holds
// when its write left one, and nil otherwise, when markdown is parsed here if the bytes match.
// The answer carry has nothing to read there: an upload's markdown writes no answer, since the
// server takes a block's server-owned attributes off an upload. It returns the events it
// appended, as carryApproval does.
func CarryRestoredApproval(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, number int, markdown string, tree *pmdoc.Node, serverURL string) ([]model.Event, error) {
	newest, err := NewestReview(ctx, tx, artifactID)
	if err != nil || newest == nil || newest.State != "approved" {
		return nil, err
	}
	return restoreApproval(ctx, tx, broker, artifactID, number, markdown, tree, *newest, serverURL)
}

// FollowClosedIssueVersions does, at the reopen of issueKey, what writeVersionTx skips for a
// version that a settlement of a closed issue's document writes (versionWrite.closedIssue): for
// each document of the issue, in id order, it moves the open approval request that names the
// document to the document's latest version (MoveApprovalAsk), and then carries an approval to
// that version (carryStandingApproval), as the version's own write does on an open issue. Before
// a closed issue's document settled, the first settlement after the reopen wrote that text and
// did both. Without them a restored request names the version from before the close, so a
// person's approval from it reviews that version and reads stale, and an approved text that a
// settlement wrote back while the issue was closed stays without its approval. The issue's reopen
// calls it once it has restored the asks the close withdrew, so each restored request is open
// again, and once their events are in, so a request's move and the carry's answer follow its
// ask.opened.
//
// A document whose stored updates are ahead of its latest version (contentChangedSinceVersion) is
// left to the settlement it owes. The reopen's own ask-state write is such an update, and that
// settlement's version holds the text the reopen leaves, which its own write compares with the
// version the request names and with the approved text. A reopen that writes back the text the
// person was asked about then keeps the request with the person, which a move to the closed
// issue's version, whose blocks show the close's resolved states, would not.
//
// The one case this leaves stale is a document whose stored updates past its latest version render
// that version again. Its settlement writes no version, since a settlement versions only a
// rendering that differs from the latest version, so nothing moves the request or runs the carry,
// and the request keeps naming the version from before the close. Such updates come only after
// the closed issue's settlement that wrote the latest version, which stored every update it read.
// So they are a browser's edit and its exact undo, both stored after that settlement and inside
// the close margin, the one window in which the append gate stores a closed issue's browser
// update, so that settlement must also have run inside the margin; or the seed of a rebuild, from
// that version, of a document whose history cannot load. The close's sweep and the ask repair
// write nothing into a document whose blocks already show the states they write.
//
// Nothing is moved or carried twice. MoveApprovalAsk moves nothing when the request already names
// the latest version, as it does after every version written while the request was open, whose
// own write moved it. The carry runs only while the newest review approves a version before the
// latest one: a version whose own write carried an approval, or that this carried at an earlier
// reopen, is already approved. A version whose own write ran the carry and carried nothing is
// refused again, since the rule reads its stored text and the rows as they stand. So the ask
// repair's restore after a reopen, and a later reopen, change nothing more.
//
// The move names the editor that a settlement while the issue was closed credited and kept for the
// document (doc_closed_edit_sources, clearSettlementPending): the edit that moves the request is
// that editor's, saved in that settlement's version, not the reopener's act, and the version's
// own write on an open issue names its editor so. Before a closed issue's document settled, the
// first settlement after the reopen named the same editor, read off the pending row that stood
// through the closed period, so a move that hands the request to its agent wakes the route and
// the claimant as it did then. A document with no kept source, whose closed issue's settlements
// named no editor, is moved in the name of SettlementActor, the actor of a settlement that cannot
// name the editor: such an ask.edited wakes nobody itself (events.Broker.Notify), the reopen's own
// ask.opened in the reopener's name does, and it reaches the request's followers as every
// ask.edited does that is not quiet. The carry's events are in the approver's name, as every
// carry's are.
//
// Once every document is followed, it deletes the kept source of each document of the issue that
// owes no settlement: no settlement left owed by the reopen names it, and a row a later write
// inserts naming no editor of its own would name that old editor (settlementPendingStatement).
// The kept source of a document that owes one stays for that settlement, which deletes it.
//
// It runs in the transaction ctx joined (errUnjoined otherwise), and its caller holds the issue's
// row, as a settlement of each of the issue's documents holds it, so no settlement writes a
// version, or a kept source, between the read of the latest one and the move. It takes no copy
// lock and writes no pending row: the move and the carry write asks rows, reviews and events, and
// the cleanup deletes kept sources alone. It returns the events it appended, in that order, for
// the caller to publish once it commits, after its own.
func (s *Service) FollowClosedIssueVersions(ctx context.Context, issueKey string) ([]model.Event, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return nil, errUnjoined
	}
	rows, err := tx.Query(ctx, issueDocumentEditSources, issueKey)
	if err != nil {
		return nil, fmt.Errorf("list the issue's documents: %w", err)
	}
	documents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (closedEditSource, error) {
		var document closedEditSource
		err := row.Scan(&document.artifactID, &document.lastActor)
		return document, err
	})
	if err != nil {
		return nil, fmt.Errorf("list the issue's documents: %w", err)
	}
	var appended []model.Event
	for _, document := range documents {
		mover := SettlementActor
		if document.lastActor != nil {
			if err := json.Unmarshal(document.lastActor, &mover); err != nil {
				return nil, fmt.Errorf("decode the closed edit source of document %s: %w", document.artifactID, err)
			}
		}
		followed, err := s.followLatestVersion(ctx, tx, document.artifactID, mover)
		if err != nil {
			return nil, fmt.Errorf("follow the latest version of document %s: %w", document.artifactID, err)
		}
		appended = append(appended, followed...)
	}
	if _, err := tx.Exec(ctx, dropSettledClosedEditSources, issueKey); err != nil {
		return nil, fmt.Errorf("delete the closed edit sources of the issue's settled documents: %w", err)
	}
	return appended, nil
}

// closedEditSource is one document of an issue as issueDocumentEditSources lists it: its id and
// the editor a settlement while the issue was closed kept for it, the stored jsonb, nil for none.
type closedEditSource struct {
	artifactID string
	lastActor  []byte
}

// issueDocumentEditSources lists the documents of the issue $1 in id order, as issueDocumentIDs
// does, each with its kept closed edit source (doc_closed_edit_sources), null for none.
const issueDocumentEditSources = `
	select a.id::text, kept.last_actor
	from artifacts a left join doc_closed_edit_sources kept on kept.artifact_id = a.id
	where a.issue_key = $1 and a.kind = 'doc'
	order by a.id`

// dropSettledClosedEditSources deletes the kept closed edit source of each document of the issue
// $1 that owes no settlement (FollowClosedIssueVersions).
const dropSettledClosedEditSources = `
	delete from doc_closed_edit_sources kept
	using artifacts a
	where a.id = kept.artifact_id and a.issue_key = $1 and a.kind = 'doc'
		and not exists (select from doc_settlements_pending p where p.artifact_id = kept.artifact_id)`

// followLatestVersion is FollowClosedIssueVersions for one document, artifactID, moving its
// request in mover's name.
func (s *Service) followLatestVersion(ctx context.Context, tx pgx.Tx, artifactID string, mover model.Actor) ([]model.Event, error) {
	latest, err := latestVersion(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	owed, err := contentChangedSinceVersion(ctx, tx, artifactID, latest.docUpdateVersion)
	if err != nil || owed {
		return nil, err
	}
	moved, err := MoveApprovalAsk(ctx, tx, s.events, artifactID, latest.Version, latest.markdown, mover, s.serverURL)
	if err != nil {
		return nil, err
	}
	newest, err := NewestReview(ctx, tx, artifactID)
	if err != nil || newest == nil || newest.State != "approved" || newest.Version >= latest.Number {
		return moved, err
	}
	// The answer carry reads the version's blocks from its tree, as the version's own write reads
	// the live tree it rendered: the stored text's read-back is that tree.
	tree, err := pmdoc.ParseRendering(latest.markdown)
	if err != nil {
		return moved, skipUnreadableVersion(artifactID, latest.Number, err)
	}
	carried, err := s.carryStandingApproval(ctx, tx, artifactID, latest.Number, renderedTree{tree: tree, markdown: latest.markdown}, *newest)
	if err != nil {
		return nil, err
	}
	return append(moved, carried...), nil
}

// restoreApproval carries newest, the document's newest review and an approval, to version number
// of artifactID when that version's markdown is the text newest approved (restoresVersionText)
// and every decision block it holds is as its row has it, and appends nothing otherwise. The text
// alone is not the approved document: a block the approved version showed open, answered by
// someone since, comes back in the same bytes while its row holds a decision nobody approved,
// which settlement then writes into the next version. So the blocks are read as the answer carry
// reads N's (askNodes, rowBackedBlocks), from tree, the version's own, or from markdown parsed
// when tree is nil; that read is made only once the bytes match.
func restoreApproval(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, number int, markdown string, tree *pmdoc.Node, newest model.ArtifactReview, serverURL string) ([]model.Event, error) {
	restored, err := restoresVersionText(ctx, tx, artifactID, newest.Version, markdown)
	if err != nil || !restored {
		return nil, err
	}
	if tree == nil {
		if tree, err = pmdoc.ParseRendering(markdown); err != nil {
			return nil, skipUnreadableVersion(artifactID, number, err)
		}
	}
	nodes, readable := askNodes(tree)
	if !readable {
		return nil, nil
	}
	if len(nodes) > 0 {
		rows, err := loadAskBlocks(ctx, tx, artifactID)
		if err != nil {
			return nil, err
		}
		if _, backed := rowBackedBlocks(nodes, rows); !backed {
			return nil, nil
		}
	}
	return writeCarriedReview(ctx, tx, broker, artifactID, number, newest, fmt.Sprintf(restoreReason, newest.Version), serverURL)
}

// resolvedStateGrowth is how many bytes longer a decision block renders once its state reads
// resolved rather than open: for a block whose row is open and holds no answer, the state is the
// one server attribute that the close's write changes (setAskServerAttributes), and only its
// value changes.
const resolvedStateGrowth = len("resolved") - len("open")

// reopenedApproval carries newest, the document's newest review and an approval, to version
// number of artifactID, written from rendered, when that version holds a block whose row is open
// and, rendered with every such block shown resolved (withOpenBlocksResolved), is byte for byte
// the text newest approved; it appends nothing otherwise. blocks are the version's blocks each as
// its row, which every one of them is as settlement leaves it (rowBackedBlocks), and nil when one
// is not or the version holds none. Each block shown resolved renders resolvedStateGrowth bytes
// longer, so only a stored text of the size that gives is read, as the restore reads only one of
// the version's own size (restoresVersionText), and the version is rendered again only once one
// is.
func reopenedApproval(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, number int, rendered renderedTree, blocks map[string]model.Ask, newest model.ArtifactReview, serverURL string) ([]model.Event, error) {
	opened := 0
	for _, ask := range blocks {
		if ask.State == "open" {
			opened++
		}
	}
	if opened == 0 {
		return nil, nil
	}
	approved, found, err := storedTextOfSize(ctx, tx, artifactID, newest.Version, len(rendered.markdown)+opened*resolvedStateGrowth)
	if err != nil || !found {
		return nil, err
	}
	closed, err := pmdoc.Render(withOpenBlocksResolved(rendered.tree, blocks))
	if err != nil || closed != approved {
		return nil, err
	}
	return writeCarriedReview(ctx, tx, broker, artifactID, number, newest, fmt.Sprintf(reopenReason, newest.Version), serverURL)
}

// withOpenBlocksResolved is a copy of node's tree in which every ask block whose row in blocks is
// open shows the state the issue's close writes into it (setAskServerAttributes with the row
// resolved), as a version of the closed issue shows it. Each container on the way and each such
// block is a fresh node; every other node is the tree's own, which nothing here changes.
func withOpenBlocksResolved(node *pmdoc.Node, blocks map[string]model.Ask) *pmdoc.Node {
	if node.Type == "ask" {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		ask, found := blocks[id]
		if !found || ask.State != "open" {
			return node
		}
		shown := *node
		shown.Attrs = maps.Clone(node.Attrs)
		ask.State = "resolved"
		setAskServerAttributes(&shown, ask, "", false)
		return &shown
	}
	if node.Type == "text" || len(node.Children) == 0 {
		return node
	}
	copied := *node
	copied.Children = make([]*pmdoc.Node, len(node.Children))
	for index, child := range node.Children {
		copied.Children[index] = withOpenBlocksResolved(child, blocks)
	}
	return &copied
}

// writeCarriedReview writes the carry of review to version number of artifactID with reason: the
// open approval request's answer, Approve in the approver's name, when one is open, then the
// approved review and its artifact.approved. The carry records the kind of the review it
// carries, since it is that review's approval it follows: a plan version restored to the text
// its plan review approved stays approved by that plan review, not by the session that wrote the
// version (ReviewKindPlanReview). A row read from artifact_reviews always names its kind, and a
// review that names none is a person's, as the column's default says. It returns those events in
// that order.
func writeCarriedReview(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, number int, review model.ArtifactReview, reason, serverURL string) ([]model.Event, error) {
	var appended []model.Event
	var askID *string
	open, err := OpenApprovalAsk(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	artifact, err := reviewedArtifact(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	// A request that names one document - every request but one opened over a spec and its
	// issue's Verification document - is answered by this carry, as it always was. One that names
	// two is answered only once both stand approved at the versions it names, which this review
	// settles for this document; the request stays open for the other, and the carried review
	// records the request it was given against either way.
	if open != nil {
		askID = &open.ID
		closes, err := NamedDocumentsApproved(ctx, tx, open.Approval.Documents, artifactID, carriedAtNamedVersion(open, artifactID, number))
		if err != nil {
			return nil, err
		}
		if closes {
			answered, err := answerApprovalAsk(ctx, tx, broker, open, review.Actor, reason, serverURL)
			if err != nil {
				return nil, err
			}
			appended = append(appended, answered)
		}
	}
	_, event, err := WriteReview(ctx, tx, broker, artifact, number, "approved", cmp.Or(review.Kind, ReviewKindPerson), review.Actor, &reason, askID)
	if err != nil {
		return nil, err
	}
	return append(appended, event), nil
}

// answerCarriedReview is the review the answer carry carries to version number of artifactID,
// just written from rendered, or nil, and the version's blocks each as its row (rowBackedBlocks),
// for the reopen carry: a version holding no ask block carries nothing by it and reads nothing
// more, and one whose blocks a row of this document does not each back as settlement leaves it
// leaves its decisions unknown, and the blocks are nil for either.
func (s *Service) answerCarriedReview(ctx context.Context, tx pgx.Tx, artifactID string, number int, rendered renderedTree) (*model.ArtifactReview, map[string]model.Ask, error) {
	nodes, readable := askNodes(rendered.tree)
	if len(nodes) == 0 || !readable {
		return nil, nil, nil
	}
	rows, err := loadAskBlocks(ctx, tx, artifactID)
	if err != nil {
		return nil, nil, err
	}
	blocks, backed := rowBackedBlocks(nodes, rows)
	if !backed {
		return nil, nil, nil
	}
	review, err := s.carriedReview(ctx, tx, artifactID, number, rendered.markdown, blocks)
	return review, blocks, err
}

// restoresVersionText reports whether markdown, the text a new version of artifactID is written
// with, is byte for byte the stored text of its version earlier. A version that stores no
// markdown, a file's, holds no text and compares false. Only a stored text of the same size is
// read, so the write of any other text reads no document (storedTextOfSize).
func restoresVersionText(ctx context.Context, tx pgx.Tx, artifactID string, earlier int, markdown string) (bool, error) {
	stored, found, err := storedTextOfSize(ctx, tx, artifactID, earlier, len(markdown))
	return found && stored == markdown, err
}

// storedTextOfSize is the stored text of version number of artifactID when that text is size
// bytes long, and whether it is; a version that stores no markdown, a file's, has none.
// octet_length reads a stored text's size without decompressing it, so a text of any other size is
// never read.
func storedTextOfSize(ctx context.Context, tx pgx.Tx, artifactID string, number, size int) (string, bool, error) {
	var stored string
	err := tx.QueryRow(ctx, `
		select markdown from artifact_versions
		where artifact_id = $1 and number = $2 and octet_length(markdown) = $3
	`, artifactID, number, size).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read document version %d: %w", number, err)
	}
	return stored, true, nil
}

// carriedAtNamedVersion reports whether the version this carry approves is the version the open
// request names for the carried document. A request that does not name it at all is decided by
// its other documents alone, as it always was.
func carriedAtNamedVersion(open *model.Ask, carried string, version int) bool {
	named := open.Approval.Document(carried)
	return named == nil || named.Version == version
}

// carriedReview is the review version number of artifactID, whose markdown is markdown and whose
// blocks are blocks, each its row, carries, or nil: the one review among the candidates' - each
// person who answered a block of N (answerers) - that stands as an approval (candidateApproval)
// and whose approval the rule carries (carryVerdict, with the approved version's blocks held to
// N's rows by heldAsItsRow). Two candidates whose approved versions straddle a block's absence
// can both pass; should two pass, nothing is carried rather than one guessed.
func (s *Service) carriedReview(ctx context.Context, tx pgx.Tx, artifactID string, number int, markdown string, blocks map[string]model.Ask) (*model.ArtifactReview, error) {
	candidates := answerers(blocks)
	if len(candidates) == 0 {
		return nil, nil
	}
	cutOnce := sync.OnceValues(func() (string, error) { return CutDecisionBlocks(markdown) })
	var carried *model.ArtifactReview
	for _, login := range candidates {
		review, approved, standing, err := s.candidateApproval(ctx, tx, artifactID, login)
		if err != nil {
			return nil, err
		}
		if !standing {
			continue
		}
		cut, err := cutOnce()
		if err != nil {
			return nil, skipUnreadableVersion(artifactID, number, err)
		}
		if !carryVerdict(carryInput{login: login, approvedAt: review.CreatedAt, blocks: blocks, approved: heldAsItsRow(approved.nodes, blocks), approvedCut: approved.cut, cut: cut}) {
			continue
		}
		if carried != nil {
			return nil, nil
		}
		carried = review
	}
	return carried, nil
}

// candidateApproval is login's standing approval of artifactID and the version it approved, A,
// as the rule reads it, and whether he has one: his latest review of the document must be an
// approval with no request for changes by anyone newer than it, and A readable.
func (s *Service) candidateApproval(ctx context.Context, tx pgx.Tx, artifactID, login string) (*model.ArtifactReview, approvedVersion, bool, error) {
	review, err := latestReviewBy(ctx, tx, artifactID, login)
	if err != nil || review == nil || review.State != "approved" {
		return nil, approvedVersion{}, false, err
	}
	requested, err := changesRequestedSince(ctx, tx, *review)
	if err != nil || requested {
		return nil, approvedVersion{}, false, err
	}
	approved, readable, err := s.readApprovedVersion(ctx, tx, artifactID, review.Version)
	if err != nil || !readable {
		return nil, approvedVersion{}, false, err
	}
	return review, approved, true, nil
}

// readApprovedVersion reads the stored version number of artifactID as the rule compares N with
// it (storedVersionTree), and whether it could be read. Its ask nodes are read as N's are
// (askNodes), which refuses a tree holding one block id twice - no measure of what he agreed to,
// as nothing is carried from an N that repeats an id. A stored version's read-back
// (pmdoc.ParseRendering) has already repaired such a repeat before this reads it, keeping the id
// on the first block and minting one for the next, so from a stored version the refusal is never
// reached: A then shows a block under the minted id that N never had, and carryVerdict refuses
// the carry for a block of A that no longer stands. Either way nothing is carried.
func (s *Service) readApprovedVersion(ctx context.Context, tx pgx.Tx, artifactID string, number int) (approvedVersion, bool, error) {
	tree, readable, err := s.storedVersionTree(ctx, tx, artifactID, number)
	if err != nil || !readable {
		return approvedVersion{}, false, err
	}
	nodes, readable := askNodes(tree)
	if !readable {
		return approvedVersion{}, false, nil
	}
	cut, err := renderWithoutAskBlocks(tree)
	if err != nil {
		return approvedVersion{}, false, skipUnreadableVersion(artifactID, number, err)
	}
	return approvedVersion{nodes: nodes, cut: cut}, true, nil
}

// skipUnreadableVersion is carryApproval's answer to a version this parser or renderer refuses. A
// version holding an element this build does not know (pmdoc.ErrUnknownElement) is one a newer
// build wrote while two builds serve one database: this build cannot tell whether the approval
// carries, so the refusal is a NewerContentError naming the version and the element, and the
// version write fails with it - settlement writes no version and leaves the document owed, for a
// build that reads the element (skipUnreadableSettlement). Any other schema refusal is a version
// an earlier release stored, which is not this write's to fail for: the approval is not carried
// and the refusal is logged. Any other error is the write's.
func skipUnreadableVersion(artifactID string, number int, err error) error {
	if errors.Is(err, pmdoc.ErrUnknownElement) {
		return newerContent(fmt.Errorf("version %d: %w", number, err))
	}
	if !errors.Is(err, pmdoc.ErrSchema) {
		return err
	}
	slog.Warn("dispatch: skip carrying an approval over a document version outside the Proof schema",
		"room", artifactID, "version", number, "error", err)
	return nil
}

// askNodes is every ask node tree holds, by block id, wherever it stands: in a blockquote, a list
// item or another typed block; and whether each id is held once. A repeated id is one settlement
// reads no block under (collectAskBlocksForSettlement), so the nodes are not returned, and the
// caller carries nothing; a nil tree holds none. Both versions are read through it: N's live tree
// (carryApproval) and the stored tree of the version he approved, A (readApprovedVersion).
func askNodes(tree *pmdoc.Node) (map[string]*pmdoc.Node, bool) {
	var nodes map[string]*pmdoc.Node
	repeated := false
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if _, repeated = nodes[id]; repeated {
			return false
		}
		if nodes == nil {
			nodes = map[string]*pmdoc.Node{}
		}
		nodes[id] = node
		return true
	})
	if repeated {
		return nil, false
	}
	return nodes, true
}

// rowBackedBlocks is every ask node of N as its row, the row of this document that indexes it
// (rows, by block id), and whether every node is as settlement leaves it from that row
// (blockAsSettlementLeavesIt). A node no row backs leaves N's decisions unknown; a node whose
// server attributes are other than settlement's own writer writes from its row - a copy, a node
// settlement could not read, a forged or stale state, answerer, time, selection or answer - is a
// forgery or a stale write, and the block of a decided row that does not parse or asks other
// wording than the row was decided on shows the decision under words nobody decided. For any of
// them nothing is returned, and the caller carries nothing. An open row's body is free: its
// question is still being written. The rows returned are what the rule reads of N (answerers,
// carryVerdict), and the rows the blocks of A are held to in turn (heldAsItsRow).
func rowBackedBlocks(nodes map[string]*pmdoc.Node, rows map[string]model.Ask) (map[string]model.Ask, bool) {
	blocks := make(map[string]model.Ask, len(nodes))
	for id, node := range nodes {
		ask, backed := rows[id]
		if !backed || !blockAsSettlementLeavesIt(node, ask) {
			return nil, false
		}
		blocks[id] = ask
	}
	return blocks, true
}

// blockAsSettlementLeavesIt reports whether node is as settlement leaves a block of this document
// from its row, ask: its server attributes are what the writer, setAskServerAttributes, writes
// from the row (blockAsItsRowHasIt), with the answer or without it, as settlement leaves out an
// answer the document has no room for; and, for a row that is not open, its wording is what
// reconcileAskBlocks writes back over a decided block - the block parses (parseAskBlock) and asks
// what the row was decided on (askBlock.asks, settlement's own test). An open row's body is free:
// its question is still being written, and the row takes it. Both versions are held to it, each
// node of N against its own row (rowBackedBlocks) and each stored node of A against the row of
// N's block under the same id (heldAsItsRow), so a stored block that passes is as settlement
// would have left it from that row, and one that fails shows something settlement never wrote.
func blockAsSettlementLeavesIt(node *pmdoc.Node, ask model.Ask) bool {
	if !blockAsItsRowHasIt(node, ask) {
		return false
	}
	if ask.State == "open" {
		return true
	}
	block, err := parseAskBlock(node)
	return err == nil && block.asks(model.NewAskEditPrevious(ask))
}

// blockAsItsRowHasIt reports whether node's server attributes (askServerAttributeNames) are what
// settlement's own writer, setAskServerAttributes, writes for ask on a block of this document -
// with the answer, or without it, as settlement leaves out an answer the document has no room for
// (withholdAnswers): the writer, run on a copy of the attributes, changes nothing one way or the
// other. A copy's copied_from, an unreadable block's invalid, and a state, answerer, time,
// selection or answer other than the row's are each a change the writer would make.
func blockAsItsRowHasIt(node *pmdoc.Node, ask model.Ask) bool {
	for _, withholdAnswer := range [...]bool{false, true} {
		clone := &pmdoc.Node{Type: node.Type, Attrs: make(pmdoc.Attrs, len(node.Attrs)+len(askServerAttributeNames))}
		maps.Copy(clone.Attrs, node.Attrs)
		if changed, _ := setAskServerAttributes(clone, ask, "", withholdAnswer); !changed {
			return true
		}
	}
	return false
}

// heldAsItsRow is, for each block id of A (approved, its stored ask nodes), whether the stored
// block is as the row of N's block under that id has it (blockAsSettlementLeavesIt over blocks,
// N's rows): the blocks of A the rule counts as unchanged since his approval. The row at N's write
// is the only row there is, and holding A's stored block to it is exact rather than approximate: a
// decided row's wording is frozen (the edit route's ASK_NOT_OPEN, which only an open ask passes),
// its selection and text move only with a new answer, which moves answered_at, and its state
// moves only with a resolution, so a stored block that is as the current row has it over the
// whole server-owned set was as the row stood when A was written, and one the row has moved away
// from since fails on answered_at or state. An id of A that no block of N has a row for is false,
// and the writer is not run for it (no empty row reaches setAskServerAttributes); it decides no
// carry either way: either N holds a block under the id, and rowBackedBlocks refused N before
// this ran, or N does not, and carryVerdict refuses for a block of A that no longer stands.
func heldAsItsRow(approved map[string]*pmdoc.Node, blocks map[string]model.Ask) map[string]bool {
	held := make(map[string]bool, len(approved))
	for id, node := range approved {
		row, backed := blocks[id]
		held[id] = backed && blockAsSettlementLeavesIt(node, row)
	}
	return held
}

// answerers is everyone who answered a block of N, blocks, each its row, by canonical login
// (model.CanonicalLogin), each once, sorted: the candidates whose approval N may carry.
func answerers(blocks map[string]model.Ask) []string {
	var logins []string
	for _, block := range blocks {
		if block.State != "answered" || block.Answer == nil {
			continue
		}
		if login := model.CanonicalLogin(block.Answer.User); !slices.Contains(logins, login) {
			logins = append(logins, login)
		}
	}
	slices.Sort(logins)
	return logins
}

// carryVerdict is the rule over the rows alone: N carries H's approval of A when the two cut to
// the same text, every block of N that is not as A held it is answered by H after his approval, at
// least one block of N is not as A held it, and every block of A still stands in N. A block of N
// is as A held it when A holds a block under its id and that stored block is as the row has it
// (input.approved, heldAsItsRow): content he approved, whoever answered it before his approval,
// which stays out of the test. Every other block of N is a change of N's - one added since A, one
// answered or re-answered since by anyone, one resolved since, which is nobody's answer, or one
// whose stored block in A is not as its row has it - and must be H's own answer, given after his
// approval (the row's answered_at after the approval's time). So a block A showed other than its
// row carries when H himself answered it again after approving, his later answer being his action
// since whatever A showed, and carries nothing otherwise. A version whose every block A held as
// its row has it holds nothing new of his and carries nothing; and a block A did not hold, back
// with an answer he gave before his approval, is his but no action of his since, so it carries
// nothing either.
//
// Each of those answers of his must also have been one he could have approved with: made at a
// browser, where he himself acted (no act), or by an agent of his under a credential holding the
// approve scope (model.ScopeApprove). An approval is his to settle, and a credential he narrowed
// to the person scope alone cannot settle what he settles; without this test an agent that may
// not approve could carry an approval of his simply by answering a block, which is the one thing
// the narrowing was meant to stop. The scopes read are the act's own, written when the answer
// was made, so narrowing a token now cannot un-approve what it approved then.
func carryVerdict(input carryInput) bool {
	if input.approvedCut != input.cut {
		return false
	}
	changed := false
	for id, block := range input.blocks {
		if input.approved[id] {
			continue
		}
		if block.State != "answered" || block.Answer == nil || model.CanonicalLogin(block.Answer.User) != input.login || !block.Answer.At.After(input.approvedAt) {
			return false
		}
		if !block.Answer.Act.Holds(model.ScopeApprove) {
			return false
		}
		changed = true
	}
	if !changed {
		return false
	}
	for id := range input.approved {
		if _, held := input.blocks[id]; !held {
			return false
		}
	}
	return true
}

// storedVersionTree is the tree of the stored version number of artifactID, and whether that
// version could be read: a version outside the Proof schema is logged and not readable, a number
// below 1, before the document's first version, is no version and not readable either, and a
// version holding an element this build does not know is an error (skipUnreadableVersion).
func (s *Service) storedVersionTree(ctx context.Context, tx pgx.Tx, artifactID string, number int) (tree *pmdoc.Node, readable bool, err error) {
	if number < 1 {
		return nil, false, nil
	}
	var markdown string
	if err := tx.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1 and number = $2`, artifactID, number).Scan(&markdown); err != nil {
		return nil, false, fmt.Errorf("read document version %d: %w", number, err)
	}
	tree, err = pmdoc.ParseRendering(markdown)
	if err != nil {
		return nil, false, skipUnreadableVersion(artifactID, number, err)
	}
	return tree, true, nil
}

// answerApprovalAsk answers the open approval request ask with Approve in actor's name, giving
// reason as the answer's text, as a human's answer through the answer route is written, and
// appends its ask.answered event on the ask's owner. The write names the open state it answers
// from: a request that is no longer open is an error, not a second answer. The answer's time is
// the transaction's now(), as the answer route's is: each process's host keeps its own clock, and
// this one is the created_at of the ask.answered event appended here and of the review the carry
// writes beside it, and the time the answer's part is written at (IndexAskPartsCounted below).
func answerApprovalAsk(ctx context.Context, tx pgx.Tx, broker *events.Broker, ask *model.Ask, actor model.Actor, reason, serverURL string) (model.Event, error) {
	var answeredAt time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&answeredAt); err != nil {
		return model.Event{}, fmt.Errorf("read the approval answer's time: %w", err)
	}
	answer := model.AskAnswer{User: actor.ID, Selected: []string{ApprovalOptionApprove}, Text: &reason, At: answeredAt.UTC()}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return model.Event{}, fmt.Errorf("encode approval answer: %w", err)
	}
	answered, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1 and state = 'open'`, ask.ID, encoded)
	if err != nil {
		return model.Event{}, fmt.Errorf("answer approval ask: %w", err)
	}
	if answered.RowsAffected() != 1 {
		return model.Event{}, fmt.Errorf("answer approval ask %s: it is no longer open", ask.ID)
	}
	ask.State = "answered"
	ask.Answer = &answer
	// The answer is a part of the request, so its text reaches the graph with it, and the
	// ask.answered below names what it moved.
	changes, err := refs.IndexAskPartsCounted(ctx, tx, refs.NewReader(ctx, tx, "ask", ask.ID, serverURL),
		*ask, answer.At, model.ReferenceChanges{})
	if err != nil {
		return model.Event{}, fmt.Errorf("index the parts of approval ask %s: %w", ask.ID, err)
	}
	payload, err := events.AskEventPayload(ctx, tx, *ask, changes)
	if err != nil {
		return model.Event{}, err
	}
	event, err := broker.Append(ctx, tx, model.Event{
		IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID,
		Type: "ask.answered", Actor: actor,
		Payload: model.AskAnsweredEventPayload{AskEventPayload: payload},
	})
	if err != nil {
		return model.Event{}, fmt.Errorf("append approval ask answer: %w", err)
	}
	// The answer's part edges are new rows of this transaction, so they take the event's id and
	// a ?since= read lists them.
	if err := refs.Stamp(ctx, tx, "ask", ask.ID, event.ID); err != nil {
		return model.Event{}, fmt.Errorf("stamp approval ask answer references: %w", err)
	}
	return event, nil
}

// CutDecisionBlocks is markdown rendered again with every ask block left out, wherever it stands,
// so two versions that differ only in their decision blocks cut to the same text. Both sides of
// the comparison go through it, so a version an earlier renderer stored compares as today's
// renderer writes it. A container the cut empties - a blockquote or a callout that held only
// decision blocks - is left holding one empty paragraph, which is what both parsers read an empty
// container as.
func CutDecisionBlocks(markdown string) (string, error) {
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return "", err
	}
	return renderWithoutAskBlocks(tree)
}

// renderWithoutAskBlocks renders a copy of tree with its ask blocks left out (withoutAskBlocks).
func renderWithoutAskBlocks(tree *pmdoc.Node) (string, error) {
	return pmdoc.Render(withoutAskBlocks(tree))
}

// withoutAskBlocks is a copy of node's tree with every ask block left out. Each container on the
// way is a fresh node with its own children; the nodes below the last container - text, inline
// nodes, leaves - are the parsed tree's own, which nothing here changes.
func withoutAskBlocks(node *pmdoc.Node) *pmdoc.Node {
	if node.Type == "text" || len(node.Children) == 0 {
		return node
	}
	copied := *node
	copied.Children = make([]*pmdoc.Node, 0, len(node.Children))
	for _, child := range node.Children {
		if child.Type == "ask" {
			continue
		}
		copied.Children = append(copied.Children, withoutAskBlocks(child))
	}
	if len(copied.Children) == 0 {
		copied.Children = append(copied.Children, &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}})
	}
	return &copied
}
