package docs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A person's answer is held once, on the ask's node, and the text a person approves is the
// document's words and blocks. What changes the bytes beside them is the answer's old second copy:
// the server-owned attributes of every ask block (askServerAttributeNames), which the release
// before wrote into every version. While two builds serve, the roll window keeps all of them in
// step with each ask (rollWindowAskAttributes); once the whole fleet runs this build, every write
// takes the answer's words and selection off, and an open ask's state, and keeps the state of an
// ask that is not open, its answerer and time and a copy's provenance (keptAskState), and every
// change of an ask's state rewrites those. Each rewrites bytes a person approved, on blocks nobody
// edited: the first accepted write once the fleet is one build does so on every document holding
// an ask block, open or answered, whose stored opener the release before wrote, since `state="open"` is
// written on every open ask. So a version whose only change from the approved one is in those
// attributes carries the approval (R5).
//
// The approval carried is the document's newest review, an approval of version A; the version
// written is N. N carries it when every difference between the two falls under one of the carry
// cases (carryCases), each of which takes out of a tree what it may change - the cases compose, so
// one version that holds several of them carries where its every difference is one of theirs -
// and nothing else differs: A's stored text and N's rendering, each taken through every case, are
// byte for byte the same (carryVerdict). The comparison is against the bytes the person approved,
// which an earlier release stored, held to A's tree by the rule a version holds a rendering by
// (holdsRendering): where this renderer version stored A, A's stored markdown must be what this
// build renders A's tree to (approvedVersion), so a difference in the bytes beside the cases is one
// no case carries; where another renderer version stored A, or none stamped it
// (pmdoc.OtherRenderer), A is compared as this build renders it, since a renderer change
// (pmdoc.RendererVersion) leaves every version stored before it in its renderer's spelling of its
// tree, which N's diff against A shows and no case need carry. Every
// ask block of N must also agree with its node (asksAgreeWithTheirNodes: backed by a row of this document, read
// by settlement, showing no answer state other than its row's and no wording other than a decided
// row's), since a block that shows a decision nobody made, or one under words nobody decided,
// carries nothing however its bytes compare. And every block id A holds must still stand in N.
//
// The server-owned ask attributes are R5's (attributeRemovalCase).
//
// A version whose text is the approved text carries that approval too, before any case is weighed
// (restoresApprovedText). The document's newest review approves version A; a later version, N, is
// stored with A's text - an accidental edit undone exactly, or a version a route wrote over a
// document that changed nothing - and each ask block of N agrees with its node, so no block in it
// shows other than what was decided. Such an N holds nothing its reviewer has not agreed to, so
// Dispatch records that reviewer's approval of N in their name and of their review's own kind, in
// the transaction that writes N, rather than asking them to approve A's text again: the newest
// review of a plan.md is the plan review that approved A, so a restored plan version reads approved
// by that plan review and not by the session whose write restored it. Only the newest review
// carries: when it requests changes no approval stands, and an older approval was superseded by it.
// N holds A's text when N's markdown is A's stored markdown byte for byte. An A that another
// renderer version stored, or that no renderer version stamped (pmdoc.OtherRenderer), is compared
// as this build renders it: a renderer change (pmdoc.RendererVersion) leaves every version stored
// before it in its renderer's spelling of its tree, so N also holds A's text when A's markdown,
// read and rendered again by this build, is N's (readsAs), though N's diff against A shows the
// spelling the change made. An A this renderer version wrote is compared byte for byte alone.

// CarriedReviewReasonPrefix opens the reason of every review Dispatch carries forward to a later
// version, and is what tells such a review from one a person wrote on the version it names: the
// number that follows it is the version the person approved. The Legion daemon reads the same
// words from its own module (intake.CarriedApprovalReasonPrefix), which no import can share.
const CarriedReviewReasonPrefix = "Carried forward from the approval of version "

// restoreReason is the reason a review carried to a version restoring the approved text records,
// naming the version of the approval it stands for (approvalOrigin), where that version's text is
// the text restored. Like every carried reason, it is written from CarriedReviewReasonPrefix
// rather than spelling that opening itself, so the sentence and its reader (CarriedFromVersion)
// cannot drift apart.
const restoreReason = CarriedReviewReasonPrefix + "%d: this version's text is that version's, unchanged."

// restoreCarriedReason is restoreReason where the approval restored is itself a carry: it names the
// version the person approved (approvalOrigin), then the version whose text is restored, the one
// that approval was carried to (restoredReason).
const restoreCarriedReason = CarriedReviewReasonPrefix + "%d: this version's text is version %d's, unchanged, which that approval was carried to."

// CarriedFromVersion reads the version a carried review's reason names, out of the sentence every
// carry writes (carriedReasonPrefix, restoreReason, restoreCarriedReason), and reports false for
// every reason a person wrote. It stands beside those sentences because a reader that took a
// carried review for a person's own would quote Dispatch's own words back as the reviewer's, and
// nothing would report it: the writer and its reader are changed together here. A reason that
// opens with the prefix but names no number there is read as a person's, since the number is all
// such a line would say. The Legion daemon reads the same sentence in its own module, which no
// import can share.
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

// carriedReasonPrefix opens the reason of every review a carry case records, naming the version
// of the approval it stands for (approvalOrigin): CarriedReviewReasonPrefix and the version. The
// changes of the cases that carried follow it, joined, and carriedReasonEnd closes it.
const carriedReasonPrefix = CarriedReviewReasonPrefix + "%d: "

// carriedReasonEnd closes a carried reason after the changes it names (carryVerdict).
const carriedReasonEnd = ", were the whole change since it."

// askServerAttributeNames are the attributes of an ask block the release before kept in agreement
// with its ask row, which a stored version may hold: `invalid` never reaches stored bytes
// (pmdoc.BlockAttributeSchema.InMarkdown), and the other seven do wherever that release wrote
// them. It is the one place the eight are spelled. Outside the tests two read it, each through
// withoutAskServerAttributes: the carry (attributeRemovalCase), which compares stored versions, and
// the text routes (TextWithAsks, through servedAskTree), which take the eight off every tree they
// render for a reader of the asks member. A stored version is never rewritten, so both still meet
// the eight after the fleet stops writing them, and the list stays.
var askServerAttributeNames = [...]string{"state", "answered_by", "answered_at", "selected", "answer", "invalid", "copied_from", "copied_from_document"}

// carryCase is one kind of difference between an approved version and a later one that carries
// the approval: what its reason says the change was, and neutral, a copy of a tree with what the
// case may change taken out of it, with whether it took anything out. Two versions that differ by
// such changes alone are the same once both are taken through it. neutral reads the tree beside
// what carryNewestApproval read of the two versions (carryInput); approved says whether the tree
// is the approved version's, A's, or the new one's, N's.
type carryCase struct {
	change  string
	neutral func(tree *pmdoc.Node, input carryInput, approved bool) (*pmdoc.Node, bool)
}

// attributeRemovalCase is R5's: the server-owned attributes of every ask block
// (askServerAttributeNames), which the first accepted write once the fleet is one build cuts to the
// ones a version keeps (keptAskState), which every change of an ask's state then rewrites, and
// which a write while two builds serve keeps in step with an answer given since.
var attributeRemovalCase = carryCase{
	change: "the server-owned attributes of its ask blocks, which hold no answer",
	neutral: func(tree *pmdoc.Node, _ carryInput, _ bool) (*pmdoc.Node, bool) {
		return withoutAskServerAttributes(tree)
	},
}

// carryCases are the cases carryVerdict composes, in the order their reasons are named.
var carryCases = [...]carryCase{attributeRemovalCase}

// carryInput is what carryVerdict decides on: A's tree, the markdown it is stored with and whether
// another renderer version stored that markdown or none stamped it (pmdoc.OtherRenderer), and N's
// tree, each as its own read gives it. Every ask block of N agrees with its node
// (asksAgreeWithTheirNodes), which each caller holds before it builds one.
type carryInput struct {
	approved              *pmdoc.Node
	approvedText          string
	approvedOtherRenderer bool
	version               *pmdoc.Node
}

// errApprovalCarriedTwice is the refusal a carry returns when the transaction it runs in has
// carried an approval for the same document already (refuseSecondCarry). One carry for one
// document in one transaction is what the carry's stored reads are right on: it reads the
// document's newest review (NewestReview), the approved version's text and tree
// (restoresApprovedText, approvedVersion) and the approval a carried review names
// (approvalOrigin) from the tables, and a carry earlier in the same transaction has queued a newer
// review of that document and a newer version of it than any of those reads can see, so a second
// carry would decide on a review this transaction has already moved past and record an approval
// in a person's name from it, with nothing to show for the mistake. The ledger is built to take
// several versions of one artifact in one transaction (Ledger.NextVersion), so nothing but this
// refusal holds the rule.
//
// A route that trips it is a programming error, not a user's: no caller can provoke it, and the
// refusal fails the write rather than reporting anything to a person. A route that must write
// two versions of one document in one transaction has to give the carry a read of that
// transaction's queued review (Ledger.PendingReview) and of its queued version's text
// (latestVersion) in place of the stored reads before it can carry twice.
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

// carryApproval carries the document's newest approval to version, just written from rendered,
// when version's text is the approved text, or when every difference from it is one of the carry
// cases (the rules above). It hands sink the events it appends - the open approval request's
// ask.answered, when one is open and this carry leaves every document it names approved
// (NamedDocumentsApproved), then artifact.approved - and appends nothing when the version carries
// no approval. A version approved that holds an element this build does not know is a
// NewerContentError, which fails the version write (skipUnreadableVersion). It reads rendered.tree
// and never changes it: the tree is the live document's.
//
// Every read and every rule run where it is called, the transaction's read phase: the newest
// review, the ask rows and the copies' sources the version's blocks are held to
// (asksAgreeWithTheirNodes), the approved version's text and tree, the approval a carried review
// names, and the request and the reviews of the other documents it names (writeCarriedReview).
// The newest review, the approved version and the approval a carry names are stored reads, which
// are right only under the rule refuseSecondCarry holds, that one transaction carries one
// document's approval at most once; the ask rows and the request are read as this transaction's
// own queued writes leave them (loadAskBlocks, openApprovalAsk). The answer and the review are
// written through write, so a transaction whose writes are queued runs them in its write phase,
// after the version they carry to.
func (s *Service) carryApproval(ctx context.Context, tx pgx.Tx, artifactID string, version model.Version, rendered renderedTree, sink func(model.Event)) error {
	if err := refuseSecondCarry(ctx, tx, artifactID); err != nil {
		return err
	}
	// Every version write runs this: anything past the newest review is read only once a carry is
	// possible at all, which needs an approval still standing.
	newest, err := NewestReview(ctx, tx, artifactID)
	if err != nil || newest == nil || newest.State != "approved" {
		return err
	}
	return s.carryStandingApproval(ctx, tx, artifactID, version.Number, rendered, *newest, sink)
}

// carryStandingApproval is carryApproval once newest, the document's newest review, is known to be
// an approval (carryNewestApproval). It hands sink the events it appends, as carryApproval does.
func (s *Service) carryStandingApproval(ctx context.Context, tx pgx.Tx, artifactID string, number int, rendered renderedTree, newest model.ArtifactReview, sink func(model.Event)) error {
	return carryNewestApproval(ctx, tx, s.events, artifactID, number, rendered.markdown, rendered.tree, newest, s.serverURL, sink)
}

// CarryUploadedApproval is carryApproval for the route that writes a document version itself
// rather than through writeVersionTx - an upload that replaces a document (the api package's
// storeArtifact) - once that version, number, is queued with markdown (Ledger.QueueVersion) and
// the open approval request has moved to it (MoveApprovalAsk). tree is markdown's read-back, which
// the caller holds when its write left one, and nil otherwise, when markdown is parsed here. The
// upload runs the same rules as every other version write: an upload that drops the server-owned
// attributes of an ask block, as every upload does, carries its approval where those are the
// whole change, and an upload of the approved text carries it whether this renderer version or
// another stored that text (restoresApprovedText). Its
// reads run where it is called, the route's read phase, and its writes are queued there, as
// carryApproval's are; the events it appends reach the ledger of the transaction it runs in
// (eventSink), which publishes them after its Commit, beside the version move's own.
func CarryUploadedApproval(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, number int, markdown string, tree *pmdoc.Node, serverURL string) error {
	if err := refuseSecondCarry(ctx, tx, artifactID); err != nil {
		return err
	}
	newest, err := NewestReview(ctx, tx, artifactID)
	if err != nil || newest == nil || newest.State != "approved" {
		return err
	}
	if tree == nil {
		if tree, err = pmdoc.ParseRendering(markdown); err != nil {
			return skipUnreadableVersion(artifactID, number, err)
		}
	}
	return carryNewestApproval(ctx, tx, broker, artifactID, number, markdown, tree, *newest, serverURL, eventSink(ctx))
}

// carryNewestApproval carries newest, the document's newest review and an approval of version A,
// to version number of artifactID, written as markdown from tree: when every ask block of the
// version agrees with its node (asksAgreeWithTheirNodes), and then either the version's text is
// A's stored text (restoresApprovedText: byte for byte, or, for an A another renderer version
// stored or none stamped, as this build renders it), or every difference from it is one of the
// carry cases (carryVerdict). It appends nothing otherwise, and hands sink what it appends, as
// carryApproval does. A is read only for a version holding an ask block: every case is a
// difference in one, and a version holding none either holds A's text or holds none of A's ask
// blocks, which still have to stand.
func carryNewestApproval(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, number int, markdown string, tree *pmdoc.Node, newest model.ArtifactReview, serverURL string, sink func(model.Event)) error {
	held, agrees, err := asksAgreeWithTheirNodes(ctx, tx, artifactID, tree)
	if err != nil || !agrees {
		return err
	}
	restored, err := restoresApprovedText(ctx, tx, artifactID, newest.Version, markdown)
	if err != nil {
		return err
	}
	if restored {
		reason, err := restoredReason(ctx, tx, artifactID, newest)
		if err != nil {
			return err
		}
		return writeCarriedReview(ctx, tx, broker, artifactID, number, newest, reason, serverURL, sink)
	}
	if !held {
		return nil
	}
	input, readable, err := approvedVersion(ctx, tx, artifactID, newest.Version)
	if err != nil || !readable {
		return err
	}
	input.version = tree
	change, carried, err := carryVerdict(input)
	if err != nil {
		return skipUnreadableVersion(artifactID, number, err)
	}
	if !carried {
		return nil
	}
	origin, err := approvalOrigin(ctx, tx, artifactID, newest)
	if err != nil {
		return err
	}
	return writeCarriedReview(ctx, tx, broker, artifactID, number, newest, fmt.Sprintf(carriedReasonPrefix, origin)+change, serverURL, sink)
}

// restoredReason is the reason of the carry of newest, the approval of version A, to a version
// that holds A's stored text (restoresApprovedText): restoreReason where newest is the person's own
// approval of A, and restoreCarriedReason where newest is a carry to A of the approval of an
// earlier version (approvalOrigin), whose text the restored version need not hold.
func restoredReason(ctx context.Context, tx pgx.Tx, artifactID string, newest model.ArtifactReview) (string, error) {
	origin, err := approvalOrigin(ctx, tx, artifactID, newest)
	if err != nil || origin == newest.Version {
		return fmt.Sprintf(restoreReason, newest.Version), err
	}
	return fmt.Sprintf(restoreCarriedReason, origin, newest.Version), nil
}

// approvalOrigin is the version of the approval newest, an approval the document carries to a
// later version, stands for, which the carried reason names: newest's own version where a person,
// or a plan review, gave it; and where newest is itself a carry, the version its reason names. So
// a carry of a carried approval names the version the person approved however many carries stand
// between, as c-attribute-removal reads it ("The carried review records the version the person
// approved"). A carry is told by the shape Dispatch writes it in (writeCarriedReview): a reason
// that opens as every carried reason does (carriedReasonPrefix), naming a version before newest's
// own, at which artifactID holds an approval by newest's own actor. A reason a person or an agent
// wrote in that opening for a version nobody approved, or one after newest's own, names nothing,
// and newest's own version stands. The comparison itself is with newest's own version, whose text
// the carry stands on (restoresApprovedText, approvedVersion). The Legion daemon reads a carried
// reason's opening alone (intake.CarriedApprovalReasonPrefix), never its number.
func approvalOrigin(ctx context.Context, tx pgx.Tx, artifactID string, newest model.ArtifactReview) (int, error) {
	var origin int
	if newest.Reason == nil {
		return newest.Version, nil
	}
	if _, err := fmt.Sscanf(*newest.Reason, carriedReasonPrefix, &origin); err != nil || origin < 1 || origin >= newest.Version {
		return newest.Version, nil
	}
	var approved bool
	if err := tx.QueryRow(ctx, `
		select exists (
			select 1 from artifact_reviews
			where artifact_id = $1 and version = $2 and state = 'approved'
			  and actor->>'kind' = $3 and actor->>'id' = $4
		)
	`, artifactID, origin, newest.Actor.Kind, newest.Actor.ID).Scan(&approved); err != nil {
		return 0, fmt.Errorf("read the approval a carried review names: %w", err)
	}
	if !approved {
		return newest.Version, nil
	}
	return origin, nil
}

// asksAgreeWithTheirNodes reports whether tree, a version, holds an ask block (held), and whether
// each agrees with its node: a block the row of artifactID indexes agrees with that row and with
// its ask's node as the transaction reads it (askAgreesWithItsNode, askNodeAnswers), or with the
// row alone where the row names no node, its answer's only record (onItsNode), and a block no row
// of this document indexes is a copy of an ask on another document of its owner (readBlockAsks,
// settlement's own classification) that agrees with its source and its source's node, or its
// source's row where that row names none (copyAgreesWithItsSource). A version holding no ask block
// agrees; one holding a block id twice, which settlement reads no block under (askNodes), or a
// block neither indexed nor a copy - one forged on the socket, or one whose update the room has not
// credited yet - does not, and leaves its decisions unknown to every carry rule. The rows, the
// sources and their nodes are read only for a version holding an ask block, and the owner only for
// one holding a block no row indexes; the carry opens no ask from what it reads, so it takes no
// copy lock.
func asksAgreeWithTheirNodes(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node) (held, agrees bool, err error) {
	nodes, readable := askNodes(tree)
	if !readable || len(nodes) == 0 {
		return false, readable, nil
	}
	blocks := make([]askBlock, 0, len(nodes))
	for _, node := range nodes {
		if block, err := parseAskBlock(node); err == nil {
			blocks = append(blocks, block)
		}
	}
	read, err := readBlockAsks(ctx, tx, artifactID, nil, blocks, readCopiedAskSources)
	if err != nil {
		return false, false, err
	}
	// kept for one release: stored-ask-state
	answers, err := askNodeAnswers(ctx, tx, read.askIDs())
	if err != nil {
		return false, false, err
	}
	for id, node := range nodes {
		if ask, indexed := read.rows[id]; indexed {
			if !askAgreesWithItsNode(node, ask, onItsNode(ask, answers)) {
				return true, false, nil
			}
			continue
		}
		// A block that does not parse is no copy (readBlockAsks reads none for it).
		source, copied := read.sources[id]
		if !copied || !copyAgreesWithItsSource(node, source, onItsNode(source.ask, answers)) {
			return true, false, nil
		}
	}
	return true, true, nil
}

// copyAgreesWithItsSource reports whether node, a copy of source's ask on another document,
// agrees with it: settlement reads it (no `invalid`), and it shows no state of the source other
// than the source's, as its row or its node (onNode) holds it (showsItsAsksState with the source's
// document). Its wording is the source's by the classification that found it.
func copyAgreesWithItsSource(node *pmdoc.Node, source copiedSource, onNode model.Ask) bool {
	if _, flagged := node.Attrs["invalid"]; flagged {
		return false
	}
	return showsItsAsksState(node, source.ask, onNode, source.document)
}

// askAgreesWithItsNode reports whether node, an ask block of a version, agrees with ask, the row of
// its document that indexes it, and with onNode, that row with its state and answer as the ask's
// node holds them (onItsNode): settlement reads it (it carries no `invalid`); it shows no answer
// state other than the row's or the node's (showsItsAsksState), so a forged or stale state,
// answerer, time, selection or answer, or a copy's provenance on a block of this document's own,
// disagrees; and, for a row that is not open, it asks the wording the row was decided on
// (askBlock.asks, settlement's own test), which settlement writes back over a decided block. An
// open row's body is free: its question is still being written, and the row takes it.
func askAgreesWithItsNode(node *pmdoc.Node, ask, onNode model.Ask) bool {
	if _, flagged := node.Attrs["invalid"]; flagged {
		return false
	}
	if !showsItsAsksState(node, ask, onNode, "") {
		return false
	}
	if ask.State == asks.StateOpen {
		return true
	}
	block, err := parseAskBlock(node)
	return err == nil && block.asks(model.NewAskEditPrevious(ask))
}

// showsItsAsksState reports whether node, an ask block of a version, shows no answer state other
// than ask's - its own ask's row, or where copied names the source's document, the source's, its
// provenance among it - as one of the writes that put them there leaves it, so that it changes
// nothing: none of the seven attributes a version can store, as a version an upload wrote holds;
// the ones a version keeps once the fleet is one build (keptAskState), written from onNode, the
// ask as its node holds it (onItsNode), as every write on a fleet of one build writes them - which
// differs from the row only while the decisions step's catch-up has not yet stated on the node what
// a process of the release before wrote into the row, when such a write keeps the row's instead
// (keptFromItsNode), the next form for an ask that is not open; the roll window's write without the
// answer's words (rollWindowAskAttributesWithheld, the same form as keptAskState) from the row; or
// the roll window's write with them (rollWindowAskAttributes) from the row, as a version written
// while two builds serve, or by the release before, holds. Only a block every one of them would
// change shows a state neither the row nor the node holds. Its first form is R5's, which stays;
// each other goes with the kept thing that writes it.
func showsItsAsksState(node *pmdoc.Node, ask, onNode model.Ask, copied string) bool {
	return !withoutStoredAskAttributes(attributesOf(node)) ||
		// kept for one release: stored-ask-state
		!keptAskState(onNode, copied)(attributesOf(node)) ||
		// kept for one release: roll-window-attributes
		!rollWindowAskAttributesWithheld(ask, copied)(attributesOf(node)) ||
		// kept for one release: roll-window-attributes
		!rollWindowAskAttributes(true, ask, copied)(attributesOf(node))
}

// attributesOf is a node of node's type holding a copy of its attributes alone, for a write to an
// ask block's attributes to be tried on without changing node.
func attributesOf(node *pmdoc.Node) *pmdoc.Node {
	attributes := make(pmdoc.Attrs, len(node.Attrs))
	maps.Copy(attributes, node.Attrs)
	return &pmdoc.Node{Type: node.Type, Attrs: attributes}
}

// withoutAskServerAttributes is attributeRemovalCase's neutral, and the tree the text routes
// render (TextWithAsks): a copy of tree with every ask block's server-owned attributes
// (askServerAttributeNames) taken off, and whether any block held one (withoutAskAttributes).
func withoutAskServerAttributes(tree *pmdoc.Node) (*pmdoc.Node, bool) {
	return withoutAskAttributes(tree, askServerAttributeNames[:])
}

// withoutAskAttributes is a copy of tree with each attribute names names taken off every ask
// block, and whether any block held one. Each container on the way and each such block is a fresh
// node; every other node is the tree's own, which nothing here changes (withAskBlocksChanged).
func withoutAskAttributes(tree *pmdoc.Node, names []string) (*pmdoc.Node, bool) {
	return withAskBlocksChanged(tree, func(node *pmdoc.Node) (*pmdoc.Node, bool) {
		var stripped pmdoc.Attrs
		for _, name := range names {
			if _, held := node.Attrs[name]; held {
				if stripped == nil {
					stripped = maps.Clone(node.Attrs)
				}
				delete(stripped, name)
			}
		}
		if stripped == nil {
			return node, false
		}
		shown := *node
		shown.Attrs = stripped
		return &shown, true
	})
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
// which is what the upload's own caller outside this package holds (CarryUploadedApproval).
func writeCarriedReview(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, number int, review model.ArtifactReview, reason, serverURL string, sink func(model.Event)) error {
	var askID *string
	// The request as the version write's own move left it (MoveApprovalAsk), which names this
	// version already.
	open, err := openApprovalAsk(ctx, tx, artifactID)
	if err != nil {
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
	// records the request it was given against either way.
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

// restoresApprovedText reports whether markdown, the text a new version of artifactID is written
// with, holds the stored text of its version earlier, the one a person approved, by the rule a
// latest version holds a rendering (holdsRendering): byte for byte, or, where another renderer
// version stored that text or none stamped it (pmdoc.OtherRenderer), as stored text this build
// reads and renders again as markdown (readsAs), so a renderer change between the two versions
// leaves the text held. A version that stores no markdown, a file's, holds no text and compares
// false. The text compared against it is the caller's own, which it holds already - the version it
// just wrote or queued - so only the earlier version is read, and that version is a stored one by
// construction: the version of a review NewestReview read from artifact_reviews, or the version an
// open approval request names for a settlement's move (MoveApprovalAsk). One read takes the stored
// text's stamp, and the text itself only where its size is markdown's (octet_length, which reads a
// stored text's size without decompressing it) or another renderer version stored it or none
// stamped it: a version this renderer version wrote is read only for a text of its size, so the
// write of any other text reads no document, and the version of another renderer version is
// parsed and rendered only once its bytes are not markdown's. It writes nothing, so a transaction
// whose writes are queued runs it in its read phase, and that parse and render hold no
// transaction id.
func restoresApprovedText(ctx context.Context, tx pgx.Tx, artifactID string, earlier int, markdown string) (bool, error) {
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
// caller carries nothing; a nil tree holds none. A version's tree is read through it
// (asksAgreeWithTheirNodes).
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

// carryVerdict is the rule over the two versions, every ask block of N agreeing with its node
// (asksAgreeWithTheirNodes, which its callers hold first): N, input.version, carries the approval
// of A, input.approved, when A's stored text, input.approvedText, holds what this build renders
// A's tree to by the rule a version holds a rendering by (holdsRendering) - byte for byte where
// this renderer version stored it, so that what is compared is the bytes the person approved, and
// as this build renders it where another renderer version stored it or none stamped it
// (input.approvedOtherRenderer), since a renderer change leaves every version stored before it in
// its renderer's spelling of its tree, which is no difference a case need carry; both trees, taken
// through every carry case (carryCases), render byte for byte the same; at least one case is one
// the two trees render apart without, since a version that equals A without any is the restore's
// to carry (restoresApprovedText), or a change nobody approved; and every block id A holds still
// stands in N (blockIDsStand over the two trees each case left). It answers what the carried
// reason says the change was: the change of each case the two trees render apart without, in
// carryCases' order, joined and closed by carriedReasonEnd. Each case is skipped in turn, beside
// every case before it that the trees rendered the same without, at two renders a case. A case
// that took out of both trees only what stands the same in each changed nothing, and is not named.
// Its error is a refusal of this renderer.
func carryVerdict(input carryInput) (string, bool, error) {
	return carryVerdictOver(input, carryCases[:])
}

// carryVerdictOver is carryVerdict over cases alone, in their order: carryCases for the carry, and
// R5's case alone for the turn of an approval request (changesAskAttributesAlone).
func carryVerdictOver(input carryInput, cases []carryCase) (string, bool, error) {
	// A's tree is read from A's stored text, so where another renderer version stored that text or
	// none stamped it, what this build renders the tree to is that text as this build reads and
	// renders it again (readsAs), which holdsRendering takes as held: only a text this renderer
	// version stored is held to its rendering byte for byte, and no second read of it is made.
	approvedText, err := pmdoc.Render(input.approved)
	if err != nil || !input.approvedOtherRenderer && approvedText != input.approvedText {
		return "", false, err
	}
	// skipped names the cases a comparison is made without: none, for the verdict itself.
	skipped := make([]bool, len(cases))
	approved, version, took := input.neutral(cases, skipped)
	if !took {
		return "", false, nil
	}
	if same, err := renderSame(approved, version); err != nil || !same {
		return "", false, err
	}
	if !blockIDsStand(approved, version) {
		return "", false, nil
	}
	var changes []string
	for index, carried := range cases {
		skipped[index] = true
		approvedWithout, versionWithout, _ := input.neutral(cases, skipped)
		same, err := renderSame(approvedWithout, versionWithout)
		if err != nil {
			return "", false, err
		}
		if !same {
			skipped[index] = false
			changes = append(changes, carried.change)
		}
	}
	if len(changes) == 0 {
		return "", false, nil
	}
	return strings.Join(changes, ", and ") + carriedReasonEnd, true, nil
}

// neutral is input's two trees, A's and N's, each taken through every case of cases in their
// order but those skipped names, and whether any of those cases took something out of either.
func (input carryInput) neutral(cases []carryCase, skipped []bool) (approved, version *pmdoc.Node, took bool) {
	approved, version = input.approved, input.version
	for index, carried := range cases {
		if skipped[index] {
			continue
		}
		var tookApproved, tookVersion bool
		approved, tookApproved = carried.neutral(approved, input, true)
		version, tookVersion = carried.neutral(version, input, false)
		took = took || tookApproved || tookVersion
	}
	return approved, version, took
}

// turnCases is the one case a version's change may be for an approval request to keep the person's
// turn across it (changesAskAttributesAlone): R5's.
var turnCases = []carryCase{attributeRemovalCase}

// changesAskAttributesAlone reports whether the version number of artifactID, written as markdown
// from tree, differs from its stored version earlier in the server-owned attributes of its ask
// blocks alone, by the test R5's case makes for the carry: every ask block of the version agrees
// with its node (asksAgreeWithTheirNodes), and the two texts, earlier's as stored - held to what
// this build renders it to as the carry holds it (carryVerdictOver: byte for byte where this
// renderer version stored it, and as this build renders it where another renderer version stored
// it or none stamped it) - and the version's, are byte for byte the same once the eight attributes
// are taken off every ask block of each (carryVerdictOver with attributeRemovalCase alone), every
// block id earlier holds still standing. It is MoveApprovalAsk's keep-turn rule beside
// restoresApprovedText: the version a person's answer, resolve or decline writes, or a reopen's
// settlement, changes no more than that when the answer, resolve, decline or reopen is that
// version's whole change. Text a person was editing in the browser and settled into the same
// version makes this false, and the request goes back to its agent. A nil tree is markdown's
// read-back, parsed here; a version holding no ask block differs in no ask attribute. Its reads
// run where it is called.
func changesAskAttributesAlone(ctx context.Context, tx pgx.Tx, artifactID string, earlier, number int, markdown string, tree *pmdoc.Node) (bool, error) {
	if tree == nil {
		parsed, err := pmdoc.ParseRendering(markdown)
		if err != nil {
			return false, skipUnreadableVersion(artifactID, number, err)
		}
		tree = parsed
	}
	held, agrees, err := asksAgreeWithTheirNodes(ctx, tx, artifactID, tree)
	if err != nil || !agrees || !held {
		return false, err
	}
	input, readable, err := approvedVersion(ctx, tx, artifactID, earlier)
	if err != nil || !readable {
		return false, err
	}
	input.version = tree
	_, alone, err := carryVerdictOver(input, turnCases)
	if err != nil {
		return false, skipUnreadableVersion(artifactID, number, err)
	}
	return alone, nil
}

// renderSame reports whether the trees a and b render byte for byte the same.
func renderSame(a, b *pmdoc.Node) (bool, error) {
	aText, err := pmdoc.Render(a)
	if err != nil {
		return false, err
	}
	bText, err := pmdoc.Render(b)
	return err == nil && aText == bText, err
}

// blockIDsStand reports whether every typed block id approved holds - the ids a stored version
// writes, its other blocks' being minted afresh by every read - is a typed block's id in version
// too. A stored version's read-back repairs an id it holds twice by minting the second block a
// fresh one (pmdoc.ParseRendering), which no later version holds, so such a version carries
// nothing.
func blockIDsStand(approved, version *pmdoc.Node) bool {
	standing := typedBlockIDs(version)
	stands := true
	pmdoc.Walk(approved, func(node *pmdoc.Node) bool {
		if _, typed := pmdoc.BlockTypeAttributes(node.Type); typed {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			if _, held := standing[id]; !held {
				stands = false
			}
		}
		return stands
	})
	return stands
}

// typedBlockIDs is the id of every typed block of tree.
func typedBlockIDs(tree *pmdoc.Node) map[string]struct{} {
	ids := map[string]struct{}{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if _, typed := pmdoc.BlockTypeAttributes(node.Type); typed {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			ids[id] = struct{}{}
		}
		return true
	})
	return ids
}

// approvedVersion is the stored version number of artifactID as carryVerdict reads it (carryInput,
// all but the version written): its tree, the markdown it is stored with and whether another
// renderer version stored that markdown or none stamped it (renderer_version, pmdoc.OtherRenderer);
// and whether that version could be read: a version outside the Proof schema is logged and not
// readable, a number below 1, before the document's first version, is no version and not readable
// either, and a version holding an element this build does not know is an error
// (skipUnreadableVersion).
func approvedVersion(ctx context.Context, q Queryer, artifactID string, number int) (carryInput, bool, error) {
	if number < 1 {
		return carryInput{}, false, nil
	}
	var markdown string
	var stamp *int
	if err := q.QueryRow(ctx, `select markdown, renderer_version from artifact_versions where artifact_id = $1 and number = $2`, artifactID, number).Scan(&markdown, &stamp); err != nil {
		return carryInput{}, false, fmt.Errorf("read document version %d: %w", number, err)
	}
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return carryInput{}, false, skipUnreadableVersion(artifactID, number, err)
	}
	return carryInput{approved: tree, approvedText: markdown, approvedOtherRenderer: pmdoc.OtherRenderer(stamp)}, true, nil
}

// answerApprovalAsk answers the open approval request ask of artifactID with Approve in actor's
// name, giving reason as the answer's text, as a human's answer through the answer route is
// written, and hands sink the ask.answered event it appends on the ask's owner. The answer goes
// through the one writer of an ask's answer state (asks.PlanSettle, asks.Settled.Queue), on the
// ask's node and then its row, and what it states on the node is appended under that
// ask.answered, right after it (graphstore.QueueCaused). The write names the open state it
// answers from: a request that is no longer open is an error, not a second answer, which the
// update's guard returns (asks.ErrStateMoved), so the write phase stops there. The answer's time
// is the transaction's now() (transactionNow), as the answer route's is: each process's host
// keeps its own clock, and this one is the created_at of the ask.answered event appended here and
// of the review the carry writes beside it, and the time the answer's part is written at
// (planAskPartsCounted).
// The update, the parts, the append, the stamp and the answer's facts are one step (write); the
// event carries the row as the step leaves it, after the move this version write queued before
// it, which set the request's edited_at (RewriteApprovalAsk).
func answerApprovalAsk(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, ask *model.Ask, actor model.Actor, reason, serverURL string, sink func(model.Event)) error {
	answeredAt, err := transactionNow(ctx, tx)
	if err != nil {
		return err
	}
	answer := model.AskAnswer{User: actor.ID, Selected: []string{ApprovalOptionApprove}, Text: &reason, At: answeredAt.UTC()}
	var held graphstore.Uncaused
	change := asks.Answered(answer)
	change.From, change.Who, change.At, change.Held = asks.StateOpen, actor, answer.At, &held
	settled, err := planSettle(ctx, tx, ask.ID, change)
	if err != nil {
		return fmt.Errorf("answer approval ask: %w", err)
	}
	// The row the update below leaves: the event carries it and the parts are read from it, so a
	// later rewording of the same request in this transaction reaches neither.
	answered := *ask
	answered.State, answered.Answer, answered.Resolution = settled.Ask.State, settled.Ask.Answer, settled.Ask.Resolution
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
		if err := settled.Queue(b); err != nil {
			return fmt.Errorf("answer approval ask: %w", err)
		}
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
		// What the answer states on the ask's node is appended under that same event, which no
		// statement between them appends another of (graphstore.LatestEvent).
		graphstore.QueueCaused(b, settled.Held, graphstore.LatestEvent)
		ask.State, ask.Answer, ask.Resolution = answered.State, answered.Answer, answered.Resolution
		return nil
	})
}

// WithoutAskBlocks is a copy of node's tree with every ask block left out, wherever it stands, for
// a reader that compares two versions apart from their ask blocks, as the change set of two
// versions does (GET /api/v1/artifacts/{id}/changes). A container the cut empties - a blockquote
// or a callout that held only ask blocks - is left holding one empty paragraph, which is what both
// parsers read an empty container as. Each container on the way is a fresh node with its own
// children; the nodes below the last container - text, inline nodes, leaves - are the parsed
// tree's own, which nothing here changes.
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
