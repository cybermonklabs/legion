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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
// review approves version A; a later version, N, is stored with A's text - an accidental edit
// undone exactly, or a version a route wrote over a document that changed nothing - and each
// decision block of N is as its row has it (rowBackedBlocks), so no block in it shows other than
// what was decided: a block A showed open that someone answered since comes back in A's text
// while its row holds that answer, and carries nothing. Such an N holds nothing its reviewer has
// not agreed to, so Dispatch records that reviewer's approval of N in their name and of their
// review's own kind, in the transaction that writes N, rather than asking them to approve A's
// text again: the newest review of a plan.md is the plan review that approved A, so a restored
// plan version reads approved by that plan review and not by the session whose write restored
// it. Only the newest review carries this way: when it requests changes no approval stands, and
// an older approval was superseded by it.
// N holds A's text when N's markdown is A's stored markdown byte for byte. An A that another
// renderer version stored, or that no renderer version stamped (pmdoc.OtherRenderer), is compared
// as this build renders it: a renderer change (pmdoc.RendererVersion) leaves every version stored
// before it in its renderer's spelling of its tree, so N also holds A's text when A's markdown,
// read and rendered again by this build, is N's (readsAs), though N's diff against A shows the
// spelling the change made. An A this renderer version wrote is compared byte for byte alone. A
// version whose decision blocks differ from A's does not hold A's text, so the answer carry
// decides it, or the rule below where the blocks it changes are questions open again.
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
// block of N is as its row has it (rowBackedBlocks); N, with each block whose row is open shown
// resolved as the close shows it (withOpenBlocksResolved), renders A's text, as the restore
// compares it (restoresVersionText: A's stored markdown byte for byte, or, for an A another
// renderer version stored or none stamped, as this build renders it); and every one of those open
// rows is an ask the close withdrew before A was written and the reopen restored after it, which
// nothing has moved since (reopenRestoredSince, read from the asks' own events). It is carried in
// the reviewer's name and of their review's kind.
//
// So the rule carries over exactly the questions the close withdrew and the reopen opened again:
// each was open when the issue closed, in the version such an approval reviewed before a closed
// issue's document settled. A block A shows resolved for any other reason is a question nobody
// approved open, though the settlement after the reopen opens it, and N then carries nothing:
// a block the closed issue's settlement saved resolved with no ask - typed inside the close
// margin, written by a process that had not heard the close, or left unindexed by a settlement
// before the close that stopped short - whose ask that settlement opens new; and the block of an
// ask a settlement retracted, back inside the close margin, which that settlement restores. A
// question that A did not hold, or held open, makes the rendering differ from A and carries
// nothing either.

// CarriedReviewReasonPrefix opens the reason of every review Dispatch carries forward to a later
// version, and is what tells such a review from one a person wrote on the version it names: the
// number that follows it is the version the person approved. The Legion daemon reads the same
// words from its own module (intake.CarriedApprovalReasonPrefix), which no import can share.
const CarriedReviewReasonPrefix = "Carried forward from the approval of version "

// carryReason is the reason a carried review records, naming the approval it follows.
const carryReason = CarriedReviewReasonPrefix + "%d: the answer to a decision block was the whole change since it."

// restoreReason is the reason a review carried to a version restoring the approved text records,
// naming the approval it follows.
const restoreReason = CarriedReviewReasonPrefix + "%d: this version's text is that version's, unchanged."

// CarriedFromVersion reads the version a carried review's reason names, out of the sentence
// carryReason and restoreReason write, and reports false for every reason a person wrote. It
// stands beside the two sentences because a reader that took a carried review for a person's
// own would quote Dispatch's own words back as the reviewer's, and nothing would report it: the
// writer and its reader are changed together here. A reason that opens with the prefix but
// names no number there is read as a person's, since the number is all such a line would say.
// The Legion daemon reads the same sentence in its own module, which no import can share.
func CarriedFromVersion(reason string) (int, bool) {
	rest, found := strings.CutPrefix(reason, CarriedReviewReasonPrefix)
	if !found {
		return 0, false
	}
	digits, _, found := strings.Cut(rest, ":")
	if !found {
		return 0, false
	}
	version, err := strconv.Atoi(digits)
	if err != nil || version < 1 {
		return 0, false
	}
	return version, true
}

// reopenReason is the reason a review carried to a version holding the approved text with
// questions open again records (reopenedApproval), naming the approval it follows. Like the two
// above it, it is written from CarriedReviewReasonPrefix rather than spelling that opening
// itself, so the sentence and its reader (CarriedFromVersion) cannot drift apart.
const reopenReason = CarriedReviewReasonPrefix + "%d: this version's text is that version's, with the questions it showed resolved open again."

// FormGate is the form gate of The Spec (R49) as a carry asks it: whether the approval the carry
// would write in the person's name - of document, at its version, answering request, the approval
// request open on it, or nil when none is - breaks the form. An approval that answers a request
// covers each document the request names, so the gate judges each of them, as it judges the
// person's own Approve of that request. The API package's form is the one implementation
// (api.SpecForm, whose gate api.NewSpecForm sets): one function gives the documents an approval
// covers and judges each, and it reads nothing more for a document that no root of The Spec
// covers. A carry writes no approval in the person's name, and answers no open request with
// Approve, when the gate finds a break: the person's own Approve then gets the gate's refusal.
type FormGate func(ctx context.Context, tx pgx.Tx, request *model.AskApproval, document model.AskApprovalDocument) (bool, error)

// SetFormGate is the gate each carry of this service's version writes asks before it writes an
// approval (writeCarriedReview). A service built without one carries no approval at all. Its one
// caller is api.NewSpecForm: the server's boot sets the gate before its restoration settles a
// document, and each backfill command sets it on the service it writes through, so each of their
// settlements carries an approval through the same gate.
func (s *Service) SetFormGate(gate FormGate) {
	s.formGate.Store(&gate)
}

// gate is the form gate SetFormGate set, or nil, with which no carry writes anything.
func (s *Service) gate() FormGate {
	if gate := s.formGate.Load(); gate != nil {
		return *gate
	}
	return nil
}

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

// errApprovalCarriedTwice is the refusal a carry returns when the transaction it runs in has
// carried an approval for the same document already (refuseSecondCarry). One carry for one
// document in one transaction is what the carry's two stored reads are right on: it reads the
// document's newest review (NewestReview) and the approved version's text (restoresVersionText)
// from the tables, and a carry earlier in the same transaction has queued a newer review of that
// document and a newer version of it than either read can see, so a second carry would decide on
// a review this transaction has already moved past and record an approval in a person's name
// from it, with nothing to show for the mistake. The ledger is built to take several versions of
// one artifact in one transaction (Ledger.NextVersion), so nothing but this refusal holds the
// rule.
//
// A route that trips it is a programming error, not a user's: no caller can provoke it, and the
// refusal fails the write rather than reporting anything to a person. A route that must write
// two versions of one document in one transaction has to give the carry a read of that
// transaction's queued review (Ledger.PendingReview) and of its queued version's text
// (latestVersion) in place of the two stored reads before it can carry twice.
var errApprovalCarriedTwice = errors.New("a transaction carries one document's approval at most once")

// refuseSecondCarry refuses the carry when the ledger queueing tx's writes already holds a
// pending review of artifactID (Ledger.PendingReview): writeCarriedReview alone records one
// (recordPendingReview), so its presence means this transaction has carried an approval for this
// document already (errApprovalCarriedTwice). A transaction no ledger queues holds no pending
// review and passes.
func refuseSecondCarry(ctx context.Context, tx pgx.Tx, artifactID string) error {
	if _, carried := queueFor(ctx, tx).PendingReview(artifactID); carried {
		return fmt.Errorf("carry the approval of document %s twice in one transaction: %w", artifactID, errApprovalCarriedTwice)
	}
	return nil
}

// carryApproval carries a human's approval to version, just written from rendered, when his answer
// to a decision block is the whole change since the version he approved, when version's text is
// the text of the version the document's newest review approved, or when it is that text with
// questions open again (the rules above). It hands sink the events it appends - the open approval
// request's ask.answered, when one is open and this carry leaves every document it names approved
// (NamedDocumentsApproved), then artifact.approved - and appends nothing when the version carries
// no approval. A version he approved that holds an element this build does not know is a
// NewerContentError, which fails the version write (skipUnreadableVersion). It reads rendered.tree
// and never changes it: the tree is the live document's. Every read and every rule run where it is
// called: the newest review (NewestReview), the ask rows (loadAskBlocks), the stored text of the
// approved version (restoresVersionText), the asks' events the reopen carry reads
// (reopenRestoredSince), the request (openApprovalAsk) and the reviews of the other documents the
// request names (NamedDocumentsApproved). The newest review, the approved version's text and
// those other reviews are stored reads, which are right only under the rule refuseSecondCarry
// holds, that one transaction carries one document's approval at most once; the ask rows, the
// request and the asks' events are read as this transaction's own queued writes leave them. The
// answer and the review are written through write, so a transaction whose writes are queued runs
// them in its write phase, after the version they carry to.
func (s *Service) carryApproval(ctx context.Context, tx pgx.Tx, artifactID string, version model.Version, rendered renderedTree, sink func(model.Event)) error {
	if err := refuseSecondCarry(ctx, tx, artifactID); err != nil {
		return err
	}
	// Every version write runs this: anything past the newest review is read only once a carry is
	// possible at all, which needs an approval still standing. A newer request for changes than
	// every approval leaves none: the answer carry's candidates each fail on it, and the restore
	// and the reopen carry read the newest review alone.
	newest, err := NewestReview(ctx, tx, artifactID)
	if err != nil || newest == nil || newest.State != "approved" {
		return err
	}
	return s.carryStandingApproval(ctx, tx, artifactID, version.Number, rendered, *newest, sink)
}

// carryStandingApproval is carryApproval once newest, the document's newest review, is known to be
// an approval: the answer carry first, then the restore when the answer carries nothing, and then
// the reopen carry when the restore carries nothing either. The version's blocks, each as its row,
// are read once for all three (versionBlocks): a version whose blocks are not each as settlement
// leaves them from a row of this document carries nothing by any of them. It hands sink the
// events it appends, as carryApproval does.
func (s *Service) carryStandingApproval(ctx context.Context, tx pgx.Tx, artifactID string, number int, rendered renderedTree, newest model.ArtifactReview, sink func(model.Event)) error {
	blocks, known, err := versionBlocks(ctx, tx, artifactID, rendered.tree)
	if err != nil || !known {
		return err
	}
	if len(blocks) > 0 {
		review, err := s.carriedReview(ctx, tx, artifactID, number, rendered.markdown, blocks)
		if err != nil {
			return err
		}
		if review != nil {
			return writeCarriedReview(ctx, tx, s.events, s.gate(), artifactID, number, *review, fmt.Sprintf(carryReason, review.Version), s.serverURL, sink)
		}
	}
	restored, err := restoresVersionText(ctx, tx, artifactID, newest.Version, rendered.markdown)
	if err != nil {
		return err
	}
	if restored {
		return writeCarriedReview(ctx, tx, s.events, s.gate(), artifactID, number, newest, fmt.Sprintf(restoreReason, newest.Version), s.serverURL, sink)
	}
	return reopenedApproval(ctx, tx, s.events, s.gate(), artifactID, number, rendered.tree, blocks, newest, s.serverURL, sink)
}

// CarryRestoredApproval is the restore alone, for the route that writes a document version itself
// rather than through writeVersionTx - an upload that replaces a document (the api package's
// storeArtifact) - once that version, number, is queued with markdown (Ledger.QueueVersion) and
// the open approval request has moved to it (MoveApprovalAsk). tree is markdown's read-back,
// which the caller holds when its write left one, and nil otherwise, when markdown is parsed
// here if it holds the approved text (restoresVersionText).
// The answer carry has nothing to read there: an upload's markdown writes no answer, since the
// server takes a block's server-owned attributes off an upload. The version's blocks are read
// only once the text matches: the text alone is not the approved document, since a block the
// approved version showed open, answered by someone since, comes back in the same text while its
// row holds a decision nobody approved, which settlement then writes into the next version; the
// rows are the rows this transaction's own queued writes leave (loadAskBlocks). Its reads run
// where it is called, the route's read phase, and its writes are queued there, as carryApproval's
// are; the events it appends reach the ledger of the transaction it runs in (eventSink), which
// publishes them after its Commit, beside the version move's own. gate judges the version's form,
// as every carry's (writeCarriedReview).
func CarryRestoredApproval(ctx context.Context, tx pgx.Tx, broker *events.Broker, gate FormGate, artifactID string, number int, markdown string, tree *pmdoc.Node, serverURL string) error {
	if err := refuseSecondCarry(ctx, tx, artifactID); err != nil {
		return err
	}
	newest, err := NewestReview(ctx, tx, artifactID)
	if err != nil || newest == nil || newest.State != "approved" {
		return err
	}
	restored, err := restoresVersionText(ctx, tx, artifactID, newest.Version, markdown)
	if err != nil || !restored {
		return err
	}
	if tree == nil {
		if tree, err = pmdoc.ParseRendering(markdown); err != nil {
			return skipUnreadableVersion(artifactID, number, err)
		}
	}
	if _, known, err := versionBlocks(ctx, tx, artifactID, tree); err != nil || !known {
		return err
	}
	return writeCarriedReview(ctx, tx, broker, gate, artifactID, number, *newest, fmt.Sprintf(restoreReason, newest.Version), serverURL, eventSink(ctx))
}

// versionBlocks is every ask block of a version, tree, as its row, the row of artifactID that
// indexes it (askNodes, rowBackedBlocks), and whether those blocks are known: a version holding no
// ask block holds none and is known, and one holding a block id twice, or a block no row of this
// document backs as settlement leaves it, is not, and leaves its decisions unknown to every carry
// rule. The rows are read only for a version holding an ask block, as this transaction's own
// queued writes leave them (loadAskBlocks).
func versionBlocks(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node) (map[string]model.Ask, bool, error) {
	nodes, readable := askNodes(tree)
	if !readable || len(nodes) == 0 {
		return nil, readable, nil
	}
	rows, err := loadAskBlocks(ctx, tx, artifactID)
	if err != nil {
		return nil, false, err
	}
	blocks, backed := rowBackedBlocks(nodes, rows)
	return blocks, backed, nil
}

// reopenedApproval carries newest, the document's newest review and an approval of version A, to
// version number of artifactID, whose tree is tree and whose blocks, each as its row, are blocks
// (versionBlocks), when that version holds a block whose row is open, each of those open rows is
// an ask the close withdrew before A was written and the reopen restored after it
// (reopenRestoredSince), and the version, rendered with every such block shown resolved
// (withOpenBlocksResolved), holds A's stored text as the restore compares it
// (restoresVersionText); it appends nothing otherwise, and hands sink what it appends, as
// carryApproval does. The rows are decided first, from the asks' events: a version write whose
// open blocks hold any other ask renders nothing more.
func reopenedApproval(ctx context.Context, tx pgx.Tx, broker *events.Broker, gate FormGate, artifactID string, number int, tree *pmdoc.Node, blocks map[string]model.Ask, newest model.ArtifactReview, serverURL string, sink func(model.Event)) error {
	var opened []string
	for _, ask := range blocks {
		if ask.State == "open" {
			opened = append(opened, ask.ID)
		}
	}
	if len(opened) == 0 {
		return nil
	}
	if restored, err := reopenRestoredSince(ctx, tx, artifactID, newest.Version, opened); err != nil || !restored {
		return err
	}
	closed, err := pmdoc.Render(withOpenBlocksResolved(tree, blocks))
	if err != nil {
		return err
	}
	if rendersApproved, err := restoresVersionText(ctx, tx, artifactID, newest.Version, closed); err != nil || !rendersApproved {
		return err
	}
	return writeCarriedReview(ctx, tx, broker, gate, artifactID, number, newest, fmt.Sprintf(reopenReason, newest.Version), serverURL, sink)
}

// reopenRestoredSince reports whether every ask of asks, the open rows of a version's blocks, is
// one an issue's close withdrew before version approved of artifactID was written and the
// issue's reopen restored after it, with nothing else moving the ask's state: of the ask's state
// events (ask.opened, ask.answered, ask.resolved), the last before the artifact.version that
// recorded that version is an ask.resolved of kind withdrawn (model.AskResolutionWithdrawn), which
// only a close writes, and the one after it is the ask.opened that opened it again, which for a
// withdrawn ask only the reopen's restore appends (api.recordRestoredAsksTx), at the reopen or the
// ask repair's restore of one. A settlement's ask fails it: the ask it opens for a block that had
// none has no event before that version, and the ask it restores had an ask.resolved of kind
// retracted. The events are the issue's, and every transaction appending one takes the issue's
// owner lock first and holds it until it ends (events.lockEventOwner), so their ids follow the
// order those transactions committed in. A version no artifact.version recorded, and the document
// of no issue, restore nothing.
//
// An ask whose restore this transaction has queued (restoredAsksOf, the reopen's follow,
// FollowClosedIssueVersions) has its ask.opened queued too, after the restore and before
// anything the follow queues, and the events table holds none of it until the write phase has
// run: that ask.opened is counted as the one after the version, beside the stored events.
func reopenRestoredSince(ctx context.Context, tx pgx.Tx, artifactID string, approved int, asks []string) (bool, error) {
	var restored int
	if err := tx.QueryRow(ctx, `
		with recorded as (
			select max(e.id) as id
			from artifacts a join events e on e.issue_key = a.issue_key
			where a.id = $1::uuid and e.type = 'artifact.version'
				and e.payload->>'artifact_id' = $2 and e.payload->'version'->>'number' = $3
		),
		moves as (
			-- The type list is the partial index events_ask_payload_id's, which serves the read;
			-- an edit moves no state.
			select e.payload->>'id' as ask, e.id, e.type, e.payload->'resolution'->>'kind' as kind
			from events e
			where e.type in ('ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited') and e.type <> 'ask.edited'
				and e.payload->>'id' = any($4::text[])
		)
		select count(*)
		from unnest($4::text[]) as opened(ask), recorded
		where (
			select m.type = 'ask.resolved' and m.kind = '`+model.AskResolutionWithdrawn+`'
			from moves m where m.ask = opened.ask and m.id < recorded.id
			order by m.id desc limit 1
		) and (
			coalesce((
				select array_agg(m.type order by m.id)
				from moves m where m.ask = opened.ask and m.id > recorded.id
			), '{}'::text[])
			|| case when opened.ask = any($5::text[]) then array['ask.opened'] else '{}'::text[] end
		) = array['ask.opened']
	`, artifactID, artifactID, strconv.Itoa(approved), asks, restoredAsksOf(ctx, tx)).Scan(&restored); err != nil {
		return false, fmt.Errorf("read whether the reopen restored the open asks of document version %d: %w", approved, err)
	}
	return restored == len(asks), nil
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
// open approval request's answer, Approve in the approver's name, when one is open and this carry
// leaves every document it names approved, then the approved review and its artifact.approved. It
// hands sink those events in that order, once the write phase has appended them. The carry records
// the kind of the review it carries, since it is that review's approval it follows: a plan version
// restored to the text its plan review approved stays approved by that plan review, not by the
// session that wrote the version (ReviewKindPlanReview). A row read from artifact_reviews always
// names its kind, and a review that names none is a person's, as the column's default says. It
// holds no document service: every write it makes is a package function over tx and the broker,
// which is what the restore's own caller outside this package holds (CarryRestoredApproval).
// Every kind of carry writes here, so the form gate judges the approval first - this version, and
// each other document of the open request the carried review is given against - and an approval
// that breaks the form carries nothing, and nothing is written or queued in anyone's name. With
// no gate, nothing is carried either.
func writeCarriedReview(ctx context.Context, tx pgx.Tx, broker *events.Broker, gate FormGate, artifactID string, number int, review model.ArtifactReview, reason, serverURL string, sink func(model.Event)) error {
	if gate == nil {
		return nil
	}
	var askID *string
	// The request as the version write's own move left it (MoveApprovalAsk), which names this
	// version already.
	open, err := openApprovalAsk(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	var request *model.AskApproval
	if open != nil {
		request = open.Approval
	}
	if breaks, err := gate(ctx, tx, request, model.AskApprovalDocument{ArtifactID: artifactID, Version: number}); err != nil || breaks {
		return err
	}
	artifact, err := reviewedArtifact(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	// A request that names one document - every request but one opened over a spec and its
	// issue's Verification document - is answered by this carry, as it always was. One that names
	// two is answered only once both stand approved at the versions it names, which this review
	// settles for this document; the request stays open for the other, and the carried review
	// records the request it was given against either way. The gate above judged both.
	if open != nil {
		// This document's own review, the one this carry queues, is never read back: the carry
		// hands its verdict over (carriedAtNamedVersion), which is what known is for. Every
		// other document the request names is read as this transaction's queued writes leave its
		// reviews: a review of one of them stands queued and unwritten where a carry of that
		// document ran earlier in this transaction - the reopen's follow carries each document
		// of the issue in one transaction (FollowClosedIssueVersions) - since writeCarriedReview
		// is the one writer that records a review on the ledger (recordPendingReview,
		// Ledger.PendingReview), and the table answers for every other document. A version write
		// carries once, for its own document: a copied ask's other documents are settled in
		// transactions of their own (SettleCopiesOf marks them owed), and an answer route's
		// reviews of the other named documents are queued after the block write, which writes no
		// version.
		closes, err := namedDocumentsApproved(ctx, tx, open.Approval.Documents, artifactID, carriedAtNamedVersion(open, artifactID, number), queueFor(ctx, tx))
		if err != nil {
			return err
		}
		if closes {
			if err := answerApprovalAsk(ctx, tx, broker, artifactID, open, review.Actor, reason, serverURL, sink); err != nil {
				return err
			}
		}
		askID = &open.ID
	}
	// The carried review keeps the kind of the review it follows, which QueueReview writes into
	// the row; a review read from a row that names no kind is a person's.
	kind := cmp.Or(review.Kind, ReviewKindPerson)
	if err := write(ctx, tx, func(b *store.WriteBatch) error {
		return QueueReview(b, broker, artifact, number, "approved", kind, review.Actor, &reason, askID, func(_ model.ArtifactReview, event model.Event) {
			sink(event)
		})
	}); err != nil {
		return err
	}
	// The carry's own approval is this document's newest review from here on, and no read of the
	// table sees it until the write phase runs, so the read of the newest review that the write
	// advice makes later in this transaction (supersededApprovalAdvice, the standing review a
	// question's advice names) takes it off the ledger. The carry makes no such read itself: it
	// runs at most once for a document in a transaction (refuseSecondCarry).
	//
	// The ledger's review is dated with tx's now() (recordPendingReview, Ledger.Now), and the row
	// QueueReview inserts is dated by its column's default, now() (artifact_reviews.created_at,
	// migration 0014_artifact_reviews). Postgres holds now() at the transaction's start for the
	// whole transaction, and the insert runs in tx, so the two are one value: a reader before the
	// write phase names the carry at the time the stored row carries.
	return recordPendingReview(ctx, tx, artifactID, model.ArtifactReview{
		ArtifactID: artifactID,
		Version:    number,
		State:      "approved",
		Kind:       kind,
		Actor:      review.Actor,
		Reason:     &reason,
		AskID:      askID,
	})
}

// restoresVersionText reports whether markdown, the text a new version of artifactID is written
// with, or the text a version rendered with its open blocks shown resolved (reopenedApproval),
// holds the stored text of its version earlier, by the rule a latest version holds a rendering
// (holdsRendering): byte for byte, or, where another renderer version stored that text or none
// stamped it (pmdoc.OtherRenderer), as stored text this build reads and renders again as markdown
// (readsAs), so a renderer change between the two versions leaves the text held. A version that
// stores no markdown, a file's, holds no text and compares false. The text compared against it is
// the caller's own, which it holds already - the version it just wrote or queued, or a rendering
// of it - so only the earlier version is read, and that version is a stored one by construction:
// the version of a review NewestReview read from artifact_reviews, or the version an open approval
// request names for a settlement's move (moveApprovalAsk). One read takes the stored text's
// stamp, and the text itself only where its size is markdown's (octet_length, which reads a
// stored text's size without decompressing it) or another renderer version stored it or none
// stamped it: a version this renderer version wrote is read only for a text of its size, so the
// write of any other text reads no document, and the version of another renderer version is
// parsed and rendered only once its bytes are not markdown's. It writes nothing, so a
// transaction whose writes are queued runs it in its read phase, and that parse and render hold
// no transaction id.
func restoresVersionText(ctx context.Context, tx pgx.Tx, artifactID string, earlier int, markdown string) (bool, error) {
	var stored *string
	var stamp *int
	err := tx.QueryRow(ctx, `
		select case when octet_length(markdown) = $3 or renderer_version is distinct from $4 then markdown end, renderer_version
		from artifact_versions
		where artifact_id = $1 and number = $2 and markdown is not null
	`, artifactID, earlier, len(markdown), pmdoc.RendererVersion).Scan(&stored, &stamp)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read document version %d: %w", earlier, err)
	}
	return stored != nil && holdsRendering(*stored, stamp, markdown), nil
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

// answerApprovalAsk answers the open approval request ask of artifactID with Approve in actor's
// name, giving reason as the answer's text, as a human's answer through the answer route is
// written, and hands sink the ask.answered event it appends on the ask's owner. The write names the
// open state it answers from: a request that is no longer open is an error, not a second answer,
// which the update's callback returns, so the write phase stops there. The answer's time is the
// transaction's now() (transactionNow), as the answer route's is: each process's host keeps its
// own clock, and this one is the created_at of the ask.answered event appended here and of the
// review the carry writes beside it, and the time the answer's part is written at
// (planAskPartsCounted).
// The update, the parts, the append and the stamp are one step (write); the event carries the
// row as the step leaves it, after the move this version write queued before it, which set the
// request's edited_at (RewriteApprovalAsk).
func answerApprovalAsk(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, ask *model.Ask, actor model.Actor, reason, serverURL string, sink func(model.Event)) error {
	answeredAt, err := transactionNow(ctx, tx)
	if err != nil {
		return err
	}
	answer := model.AskAnswer{User: actor.ID, Selected: []string{ApprovalOptionApprove}, Text: &reason, At: answeredAt.UTC()}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return fmt.Errorf("encode approval answer: %w", err)
	}
	// The row the update below leaves: the event carries it and the parts are read from it, so a
	// later rewording of the same request in this transaction reaches neither.
	answered := *ask
	answered.State = "answered"
	answered.Answer = &answer
	// The answer is a part of the request, so its text reaches the graph with it, and the
	// ask.answered below names what it moved. The parts are read through a reader of the ask's
	// own (refs.NewReader), planned here and written in the step below, after the update whose
	// row they describe. A version write that moved the request first has planned its parts
	// already, and this plan runs against what that one leaves, so the answer takes a revision
	// of its own. The answer holds the parts alone, so the step reconciles the question's
	// references after them too and settles the forms queue there (questionReconciled false).
	changes, parts, err := planAskPartsCounted(ctx, tx, refs.NewReader(ctx, tx, ask.Author, "ask", ask.ID, serverURL),
		answered, answer.At, false, model.ReferenceChanges{})
	if err != nil {
		return fmt.Errorf("index the parts of approval ask %s: %w", ask.ID, err)
	}
	// The request as this transaction's queued writes leave it, which is answered and no longer
	// the agent's to hand back: the move recorded the row it left (recordPendingApproval,
	// MoveApprovalAsk), and a reader of the ledger between this answer and the write phase - the
	// write's own report of the request its version moved (api.movedApprovalRequestAdvice) -
	// must see this answer rather than that move.
	recordPendingApproval(ctx, tx, artifactID, &answered)
	return write(ctx, tx, func(b *store.WriteBatch) error {
		b.Queue("answer approval ask", `update asks set state = 'answered', answer = $2 where id = $1 and state = 'open'`, ask.ID, encoded).Exec(func(tag pgconn.CommandTag) error {
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("answer approval ask %s: it is no longer open", ask.ID)
			}
			ask.State = answered.State
			ask.Answer = answered.Answer
			return nil
		})
		if err := parts(b); err != nil {
			return fmt.Errorf("index the parts of approval ask %s: %w", ask.ID, err)
		}
		// The ask's thread_logins are read by the event's insert, after the answer's update.
		if err := broker.QueueAppendWithLogins(b, model.Event{
			IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID,
			Type: "ask.answered", Actor: actor,
			Payload: model.AskAnsweredEventPayload{AskEventPayload: model.NewAskEventPayload(answered, changes)},
		}, events.ThreadLoginsOf(ask.ID), sink); err != nil {
			return fmt.Errorf("append approval ask answer: %w", err)
		}
		// The answer's part edges are new rows of this transaction, so they take the id of the
		// event queued just above and a ?since= read lists them.
		refs.QueueStamp(b, "ask", ask.ID)
		return nil
	})
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

// renderWithoutAskBlocks renders a copy of tree with its ask blocks left out (WithoutAskBlocks).
func renderWithoutAskBlocks(tree *pmdoc.Node) (string, error) {
	return pmdoc.Render(WithoutAskBlocks(tree))
}

// WithoutAskBlocks is a copy of node's tree with every ask block left out: the tree
// CutDecisionBlocks renders, for a reader that goes on to read the cut tree itself and not only
// its text, as the change set of two versions does (GET /api/v1/artifacts/{id}/changes). Each
// container on the way is a fresh node with its own children; the nodes below the last
// container - text, inline nodes, leaves - are the parsed tree's own, which nothing here changes.
func WithoutAskBlocks(node *pmdoc.Node) *pmdoc.Node {
	if node.Type == "text" || len(node.Children) == 0 {
		return node
	}
	copied := *node
	copied.Children = make([]*pmdoc.Node, 0, len(node.Children))
	for _, child := range node.Children {
		if child.Type == "ask" {
			continue
		}
		copied.Children = append(copied.Children, WithoutAskBlocks(child))
	}
	if len(copied.Children) == 0 {
		copied.Children = append(copied.Children, &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}})
	}
	return &copied
}
