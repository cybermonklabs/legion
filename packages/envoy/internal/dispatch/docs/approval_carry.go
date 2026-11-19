package docs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
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

// A person's answer is held once, on the ask's node, so answering an
// ask changes no byte of a document and supersedes no approval of it. What changes the bytes is
// the answer's old second copy leaving the document: the server-owned attributes of every ask
// block (askServerAttributeNames), which the release before wrote into every version and this
// build stops writing once the whole fleet runs it (rollWindowAskAttributes). That removal rewrites
// bytes a person approved, on blocks nobody edited, on the first accepted write of every document
// holding an ask block, open or answered, since `state="open"` is written on every open ask. So
// the version that drops them carries the approval (R10), and so does a version written while two
// builds serve, whose attributes the roll window keeps in step with an answer given since.
//
// The approval carried is the document's newest review, an approval of version A; the version
// written is N. N carries it when every difference between the two falls under one of the carry
// cases (carryCases), each of which takes out of a tree what it may change - the cases compose, so
// one version that holds several of them carries where its every difference is one of theirs -
// and nothing else differs: A's stored text and N's rendering, each taken through every case, are
// byte for byte the same (carryVerdict). The comparison is against the bytes the person approved,
// which an earlier release stored: A's stored markdown must be what this build renders A's tree to
// (approvedVersion), so a renderer change between A and N is a difference no case carries. Every
// ask block of N must also agree with its node (asksAgreeWithTheirNodes: backed by a row of this document, read
// by settlement, showing no answer state other than its row's and no wording other than a decided
// row's), since a block that shows a decision nobody made, or one under words nobody decided,
// carries nothing however its bytes compare. And every block id A holds must still stand in N,
// but for the readings a case takes out.
//
// The cases are R2's, R10's, R7's and R9's. A reading never changes the decision it reads, so the
// readings of a version's decisions and of its answered asks, added, edited or removed, are no
// change to anything its reviewer approved (readingCase): the version a session writes when it
// adds its reading of a person's decision still reads approved, at the version it wrote. The
// server-owned ask attributes are R10's (attributeRemovalCase). The answered asks the restore
// returns to the spec's `## Decisions` part, in place of the part's pointer line, are R7's
// (restoreCase): the restore changes where an ask's block stands and not its words. The decisions
// the conversion of an issue's decisions document writes into that part, and the links it gives
// an answered ask as `shaped` pointers, are R9's (conversionCase): a decision and its ties are
// outside what a spec's approval covers. Those two hold for the
// decisions step's own write alone (decisionStepCases), and take out only what that write added:
// an agent's upload or edit that makes the same change carries nothing by them, and neither does
// an agent's change the step's version keeps - a decision whose words it rewrote, a pointer it gave
// an ask, a block it wrote into the part.
//
// A version whose text is the approved text carries that approval too, byte for byte and before
// any case is weighed (restoresApprovedText). The document's newest review approves version A; a
// later version, N, is stored with A's markdown byte for byte - an accidental edit undone exactly,
// or a version a route wrote over a document that changed nothing - and each ask block of N agrees
// with its node, so no block in it shows other than what was decided. Such an N holds nothing its
// reviewer has not agreed to, so Dispatch records that reviewer's approval of N in their name and
// of their review's own kind, in the transaction that writes N, rather than asking them to approve
// A's text again: the newest review of a plan.md is the plan review that approved A, so a restored
// plan version reads approved by that plan review and not by the session whose write restored it.
// Only the newest review carries: when it requests changes no approval stands, and an older
// approval was superseded by it.

// restoreReason is the reason a review carried to a version restoring the approved text records,
// naming the version of the approval it stands for (approvalOrigin), where that version's text is
// the text restored. It opens as every carried reason does, which is how the Legion daemon tells a
// carry from a person's own approval (intake.CarriedApprovalReasonPrefix).
const restoreReason = "Carried forward from the approval of version %d: this version's text is that version's, unchanged."

// restoreCarriedReason is restoreReason where the approval restored is itself a carry: it names the
// version the person approved (approvalOrigin), then the version whose text is restored, the one
// that approval was carried to (restoredReason).
const restoreCarriedReason = "Carried forward from the approval of version %d: this version's text is version %d's, unchanged, which that approval was carried to."

// carriedReasonPrefix opens the reason of every review a carry case records, naming the version
// of the approval it stands for (approvalOrigin): the daemon's intake.CarriedApprovalReasonPrefix
// and the version. The changes of the cases that carried follow it, joined, and carriedReasonEnd
// closes it.
const carriedReasonPrefix = "Carried forward from the approval of version %d: "

// carriedReasonEnd closes a carried reason after the changes it names (carryVerdict).
const carriedReasonEnd = ", were the whole change since it."

// askServerAttributeNames are the attributes of an ask block the release before kept in agreement
// with its ask row, which a stored version may hold: `invalid` never reaches stored bytes
// (pmdoc.BlockAttributeSchema.InMarkdown), and the other seven do wherever that release wrote
// them. It is the one place the eight are spelled, and the carry its one reader
// (attributeRemovalCase): it compares against stored versions, which are never rewritten, so it
// stays after the fleet stops writing them.
var askServerAttributeNames = [...]string{"state", "answered_by", "answered_at", "selected", "answer", "invalid", "copied_from", "copied_from_document"}

// carryCase is one kind of difference between an approved version and a later one that carries
// the approval: what its reason says the change was, and neutral, a copy of a tree with what the
// case may change taken out of it, with whether it took anything out. Two versions that differ by
// such changes alone are the same once both are taken through it. neutral reads the tree beside
// what carryNewestApproval read of the two versions (carryInput): each ask block of the version
// whose ask holds an answer, the asks the restore returned, and what the decisions step's write
// added (decisionStepCases); approved says whether the tree is the approved version's, A's, or the
// new one's, N's, since the restore and the conversion take out of each only what their write
// changed in it.
type carryCase struct {
	change  string
	neutral func(tree *pmdoc.Node, input carryInput, approved bool) (*pmdoc.Node, bool)
}

// readingCase is R2's: the readings a version's decisions and its answered asks close with,
// added, edited or removed. A reading never changes the decision it reads, so writing, editing or
// removing one never stales a person's approval of the document that holds the decision, and the
// version it is written in reads approved. A reading in an ask that holds no answer reads no
// decision, and stays a difference of its own: the write paths refuse one, and settlement flags
// its ask block, which then agrees with no node.
var readingCase = carryCase{
	change: "the readings of its decisions and of its answered asks, which change no decision",
	neutral: func(tree *pmdoc.Node, input carryInput, _ bool) (*pmdoc.Node, bool) {
		return withoutReadings(tree, input.answered)
	},
}

// attributeRemovalCase is R10's: the server-owned attributes of every ask block
// (askServerAttributeNames), which the first accepted write once the fleet is one build drops, and
// which a write while two builds serve keeps in step with an answer given since.
var attributeRemovalCase = carryCase{
	change: "the server-owned attributes of its ask blocks, which hold no answer",
	neutral: func(tree *pmdoc.Node, _ carryInput, _ bool) (*pmdoc.Node, bool) {
		return withoutAskServerAttributes(tree)
	},
}

// restoreCase is R7's (D1): the ask blocks the restore writes into the spec's `## Decisions` part
// - each answered ask it returns, and each ask block of the decisions document it moves, whatever
// its ask's state - in place of its pointer line, with the `## Decisions` heading it adds to a
// spec holding no such part, a block under a new id included. It takes out of N only the blocks
// the restore's own write added (decisionStepCases.restored), and out of A only the pointer line
// that write took out (pointerLinesTaken); a block an agent wrote into the part is no part of it.
// Where the approved version held the ask's block already, under the id it stands at now, the
// block is taken out of both versions only where its words are the words the person approved
// (restoredAskBlocks), so a restore that returns other words than the approved ones is a
// difference no case carries. It holds for the restore's own write alone
// (decisionStepCases.restore): an agent's write that makes the same change carries nothing by it.
var restoreCase = carryCase{
	change: "the ask blocks its Decisions part holds again, which changed where they stand and not their words",
	neutral: func(tree *pmdoc.Node, input carryInput, approved bool) (*pmdoc.Node, bool) {
		if !input.steps.restore {
			return tree, false
		}
		tree, took := withoutBlocks(tree, func(node *pmdoc.Node) bool {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			return node.Type == "ask" && input.restored[id]
		})
		restored, restoredTook := withoutDecisionsPartBlocks(tree, input.pointerLinesTaken(approved))
		return restored, took || restoredTook
	},
}

// conversionCase is R9's (D6, E2): the decision blocks of the spec's `## Decisions` part, which the
// conversion of the issue's decisions document writes in place of the part's pointer line to that
// document, and the `shaped` pointers an answered ask's restored or present block takes from the
// section that restated it. A decision, and an answered ask's ties, are outside what an approval
// of a spec covers; the readings the conversion writes inside answered
// asks are readingCase's. It takes out of N only the decisions and the pointers the conversion's
// own write added (decisionStepCases.decisions, decisionStepCases.shaped), and out of A only the
// pointer line that write took out (pointerLinesTaken): a decision A held, whose words an agent
// changed since, and a pointer an agent gave an ask, stay differences. A `## Decisions` heading the
// part holds nothing else under is taken out with them, since the conversion adds it to a spec
// that holds no such part. It holds for the conversion's own write alone
// (decisionStepCases.conversion): an agent's upload that adds the same `shaped` pointers to an
// answered ask supersedes the approval as any other change does.
var conversionCase = carryCase{
	change: "the decisions its Decisions part holds in place of the pointer line to the issue's decisions document, " +
		"and the spec nodes its answered asks shaped, which an approval of a spec does not cover",
	neutral: func(tree *pmdoc.Node, input carryInput, approved bool) (*pmdoc.Node, bool) {
		if !input.steps.conversion {
			return tree, false
		}
		if approved {
			return withoutDecisionsPartBlocks(tree, input.pointerLinesTaken(true))
		}
		tree, took := withoutAddedShapedPointers(tree, input.steps.shaped, input.decided)
		converted, convertedTook := withoutDecisionsPartBlocks(tree, func(node *pmdoc.Node) bool {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			return node.Type == decisionType && input.steps.decisions[id]
		})
		return converted, took || convertedTook
	},
}

// carryCases are the cases carryVerdict composes, in the order their reasons are named.
var carryCases = [...]carryCase{readingCase, attributeRemovalCase, restoreCase, conversionCase}

// decisionStepCases names the phases of the decisions step whose own write a version is - the
// restore (restoreCase) and the conversion (conversionCase) - and what that write added to the
// document, which is all those two cases take out of a version: the block id of each answered ask
// block the restore wrote (restored), of each decision the conversion wrote (decisions), the
// `shaped` pointers it gave an answered ask block the document held already, by that block's id
// (shaped), and the markdown of each pointer line the write took out of the document's
// `## Decisions` part (pointerLines, blockMarkdown), which is what those cases take out of the
// approved version. Only the step sets it, through a mark on its write's context no route can put
// there (withDecisionStepWrite), and SnapshotVersion carries it on the version's write
// (versionWrite.steps); every other version write leaves it zero, and its carry weighs the reading
// case and the attribute-removal case alone.
type decisionStepCases struct {
	restore, conversion bool
	restored, decisions map[string]bool
	shaped              map[string][]string
	pointerLines        []string
}

// any reports whether the version is the decisions step's own write.
func (steps decisionStepCases) any() bool {
	return steps.restore || steps.conversion
}

// decisionStepWriteKey marks a context as the decisions step's own write (withDecisionStepWrite).
type decisionStepWriteKey struct{}

// withDecisionStepWrite marks ctx as the decisions step's own write, of the phases steps names and
// with what it added.
func withDecisionStepWrite(ctx context.Context, steps decisionStepCases) context.Context {
	return context.WithValue(ctx, decisionStepWriteKey{}, steps)
}

// decisionStepCasesOf is the phases ctx's mark names, and what their write added, zero for a
// context the step did not mark.
func decisionStepCasesOf(ctx context.Context) decisionStepCases {
	steps, _ := ctx.Value(decisionStepWriteKey{}).(decisionStepCases)
	return steps
}

// carryInput is what carryVerdict decides on: A's tree and the markdown it is stored with, and
// N's tree, each as its own read gives it, whether every ask block of N agrees with its node
// (asksAgreeWithTheirNodes), which of N's ask blocks show an ask that holds an answer among those
// closing with readings (answeredAskBlocks) and among them all (decided), the answered ask blocks
// of N's `## Decisions` part the restore's write added that restoreCase takes out of both
// versions (restoredAskBlocks), and the decisions step's phases whose own write N is, with what
// that write added (steps), the only writes those two cases reach.
type carryInput struct {
	approved     *pmdoc.Node
	approvedText string
	version      *pmdoc.Node
	agrees       bool
	answered     map[string]bool
	decided      map[string]bool
	restored     map[string]bool
	steps        decisionStepCases
	// pointersLeft is, within one pass of the cases over the two versions (neutral), the pointer
	// lines of the step's write that no case has taken out of A yet (pointerLinesTaken).
	pointersLeft *[]string
}

// pointerLinesTaken is what restoreCase and conversionCase take out of the `## Decisions` part of
// one of the two versions beside the blocks the step's write added: of A (approved), each pointer
// line that write took out of the document (decisionStepCases.pointerLines), as the first
// paragraph of the part whose markdown is that line's, once each in a pass of the cases between
// the two of them (pointersLeft), however many paragraphs of A are that line; of N, none, since
// the write left none of them there. A paragraph a person approved that the write did not take
// out - one citing a block of the decisions document, which the restore keeps, or a second copy of
// the pointer line - stays in A. The answer is a drop for one call of withoutDecisionsPartBlocks,
// which it walks in order.
func (input carryInput) pointerLinesTaken(approved bool) func(*pmdoc.Node) bool {
	if !approved || input.pointersLeft == nil || len(*input.pointersLeft) == 0 {
		return func(*pmdoc.Node) bool { return false }
	}
	left := input.pointersLeft
	return func(node *pmdoc.Node) bool {
		if node.Type != "paragraph" || len(*left) == 0 {
			return false
		}
		markdown, err := blockMarkdown(node)
		if err != nil {
			return false
		}
		index := slices.Index(*left, markdown)
		if index < 0 {
			return false
		}
		*left = slices.Delete(*left, index, index+1)
		return true
	}
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
// and never changes it: the tree is the live document's. steps names the decisions step's phases
// whose own write the version is (versionWrite.steps), whose cases alone it weighs beside the
// reading case and the attribute-removal case.
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
func (s *Service) carryApproval(ctx context.Context, tx pgx.Tx, artifactID string, version model.Version, rendered renderedTree, steps decisionStepCases, sink func(model.Event)) error {
	if err := refuseSecondCarry(ctx, tx, artifactID); err != nil {
		return err
	}
	// Every version write runs this: anything past the newest review is read only once a carry is
	// possible at all, which needs an approval still standing.
	newest, err := NewestReview(ctx, tx, artifactID)
	if err != nil || newest == nil || newest.State != "approved" {
		return err
	}
	return s.carryStandingApproval(ctx, tx, artifactID, version.Number, rendered, *newest, steps, sink)
}

// carryStandingApproval is carryApproval once newest, the document's newest review, is known to be
// an approval (carryNewestApproval). It hands sink the events it appends, as carryApproval does.
func (s *Service) carryStandingApproval(ctx context.Context, tx pgx.Tx, artifactID string, number int, rendered renderedTree, newest model.ArtifactReview, steps decisionStepCases, sink func(model.Event)) error {
	return carryNewestApproval(ctx, tx, s.events, artifactID, number, rendered.markdown, rendered.tree, newest, s.serverURL, steps, sink)
}

// CarryUploadedApproval is carryApproval for the route that writes a document version itself
// rather than through writeVersionTx - an upload that replaces a document (the api package's
// storeArtifact) - once that version, number, is queued with markdown (Ledger.QueueVersion) and
// the open approval request has moved to it (MoveApprovalAsk). tree is markdown's read-back, which
// the caller holds when its write left one, and nil otherwise, when markdown is parsed here. The
// upload runs the same rules as every other version write but the decisions step's: an upload
// that adds, edits or removes the readings of its decisions and answered asks, or that drops the
// server-owned attributes of an ask block, as every upload does, carries its approval where those
// are the whole change, and an upload that makes the change of the restore or of the conversion
// carries nothing by their cases (restoreCase, conversionCase), which no route can reach. Its
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
	return carryNewestApproval(ctx, tx, broker, artifactID, number, markdown, tree, *newest, serverURL, decisionStepCases{}, eventSink(ctx))
}

// carryNewestApproval carries newest, the document's newest review and an approval of version A,
// to version number of artifactID, written as markdown from tree: when every ask block of the
// version agrees with its node (asksAgreeWithTheirNodes), and then either the version's text is
// A's stored text byte for byte (restoresApprovedText), or every difference from it is one of the
// carry cases (carryVerdict). It appends nothing otherwise, and hands sink what it appends, as
// carryApproval does. A is read only for a version holding an ask block or a decision: every case
// is a difference in one of those blocks, and a version holding neither either holds A's text or
// holds none of A's decisions and ask blocks, which still have to stand. steps names the decisions
// step's phases whose own write the version is, and what that write added, whose cases
// (restoreCase, conversionCase) it weighs beside the other two; zero for every other write.
func carryNewestApproval(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, number int, markdown string, tree *pmdoc.Node, newest model.ArtifactReview, serverURL string, steps decisionStepCases, sink func(model.Event)) error {
	shown, agrees, err := asksAgreeWithTheirNodes(ctx, tx, artifactID, tree)
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
	if len(shown.nodes) == 0 && !holdsDecision(tree) {
		return nil
	}
	approved, approvedText, readable, err := approvedVersion(ctx, tx, artifactID, newest.Version)
	if err != nil || !readable {
		return err
	}
	answered, err := answeredAskBlocks(ctx, tx, shown, approved)
	if err != nil {
		return err
	}
	input := carryInput{
		approved: approved, approvedText: approvedText, version: tree, agrees: agrees, answered: answered,
		decided: decidedAskBlocks(shown), steps: steps,
	}
	if steps.restore {
		input.restored = restoredAskBlocks(approved, tree, steps.restored)
	}
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
// whose text is A's stored text byte for byte: restoreReason where newest is the person's own
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

// versionAsks is what asksAgreeWithTheirNodes read of a version's ask blocks: each one by block id
// (askNodes), the asks of the document that index them (loadAskBlocks), and, of each block no
// such ask indexes, the ask of another document it is a copy of (readCopiedAskSources), by block
// id. The reading case reads through the last two whether the ask a block shows holds an answer
// (answeredAskBlocks), as settlement does.
type versionAsks struct {
	nodes   map[string]*pmdoc.Node
	rows    map[string]model.Ask
	sources map[string]copiedSource
}

// asksAgreeWithTheirNodes is every ask block of a version, tree, by block id (askNodes), and
// whether each agrees with its node: a block the row of artifactID indexes agrees with that row,
// the projection of its node (askAgreesWithItsNode), and a block no row of this document indexes
// is a copy of an ask on another document of its owner (readCopiedAskSources, settlement's own
// classification) that agrees with its source (copyAgreesWithItsSource). A version holding no ask
// block agrees; one holding a block id twice, which settlement reads no block under, or a block
// neither indexed nor a copy - one forged on the socket, or one whose update the room has not
// credited yet - does not, and leaves its decisions unknown to every carry rule. The rows are read
// only for a version holding an ask block, and the sources only for one holding a block no row
// indexes; the carry opens no ask from what it reads, so it takes no copy lock. What it read of
// the blocks is answered beside the verdict (versionAsks).
func asksAgreeWithTheirNodes(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node) (versionAsks, bool, error) {
	nodes, readable := askNodes(tree)
	if !readable || len(nodes) == 0 {
		return versionAsks{}, readable, nil
	}
	rows, err := loadAskBlocks(ctx, tx, documentAsksOf(artifactID))
	if err != nil {
		return versionAsks{}, false, err
	}
	var copies []askBlock
	var unindexed []string
	for id, node := range nodes {
		if ask, indexed := rows[id]; indexed {
			if !askAgreesWithItsNode(node, ask) {
				return versionAsks{}, false, nil
			}
			continue
		}
		block, err := parseAskBlock(node)
		if err != nil {
			return versionAsks{}, false, nil
		}
		copies, unindexed = append(copies, block), append(unindexed, id)
	}
	if len(copies) == 0 {
		return versionAsks{nodes: nodes, rows: rows}, true, nil
	}
	owner, err := documentOwner(ctx, tx, artifactID)
	if err != nil {
		return versionAsks{}, false, err
	}
	sources, err := readCopiedAskSources(ctx, tx, artifactID, owner, copies, unindexed)
	if err != nil {
		return versionAsks{}, false, err
	}
	for _, block := range copies {
		source, copied := sources[block.id]
		if !copied || !copyAgreesWithItsSource(block.node, source) {
			return versionAsks{}, false, nil
		}
	}
	return versionAsks{nodes: nodes, rows: rows, sources: sources}, true, nil
}

// answeredAskBlocks is each ask block of a version, read as asksAgreeWithTheirNodes read it
// (shown), whose ask holds an answer, by block id, among those that close with a reading in that
// version or in approved, the version its reviewer approved: the blocks whose readings read a
// person's decision (readingCase). Whether a block's ask holds an answer is settlement's own
// decision (readingAnswers), over the ask its own document's row indexes or, for a copy, its
// source. A version none of whose ask blocks closes with a reading, in either version, reads no
// ask.
func answeredAskBlocks(ctx context.Context, tx pgx.Tx, shown versionAsks, approved *pmdoc.Node) (map[string]bool, error) {
	var before map[string]*pmdoc.Node
	read := false
	var ids []string
	for id, node := range shown.nodes {
		if !closesWithReadings(node) {
			if !read {
				before, _ = askNodes(approved)
				read = true
			}
			if held, approvedHeld := before[id]; !approvedHeld || !closesWithReadings(held) {
				continue
			}
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	answers, err := readingAnswers(ctx, tx, ids, shown.rows, shown.sources)
	if err != nil {
		return nil, err
	}
	answered := make(map[string]bool, len(answers))
	for id, answer := range answers {
		if answer.answered {
			answered[id] = true
		}
	}
	return answered, nil
}

// closesWithReadings reports whether node, a decision or an ask block, closes with a block its
// type admits after its own content: a reading (pmdoc.TypedContent).
func closesWithReadings(node *pmdoc.Node) bool {
	_, admitted := pmdoc.TypedContent(node)
	return len(admitted) > 0
}

// holdsDecision reports whether tree holds a decision block anywhere.
func holdsDecision(tree *pmdoc.Node) bool {
	held := false
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		held = held || node.Type == decisionType
		return !held
	})
	return held
}

// copyAgreesWithItsSource reports whether node, a copy of source's ask on another document,
// agrees with it: settlement reads it (no `invalid`), and it shows no state of the source other
// than the source's - none at all, or what the roll window keeps in step with the source, its
// provenance among it (inStepWithTheRollWindow with the source's document). Its wording is the
// source's by the classification that found it.
func copyAgreesWithItsSource(node *pmdoc.Node, source copiedSource) bool {
	if _, flagged := node.Attrs["invalid"]; flagged {
		return false
	}
	return !withoutStoredAskAttributes(attributesOf(node)) || inStepWithTheRollWindow(node, source.ask, source.document)
}

// askAgreesWithItsNode reports whether node, an ask block of a version, agrees with ask, the row of
// its document that indexes it, which is the projection of the ask's node: settlement reads it (it
// carries no `invalid`); it shows no answer state other than the row's - none at all, as every
// block of a version written once the fleet is one build, or what the roll window keeps in step
// with the row (inStepWithTheRollWindow), as a block of a version written while two builds serve -
// so a forged or stale state, answerer, time, selection or answer, or a copy's provenance on a
// block of this document's own, disagrees; and, for a row that is not open, it asks the wording
// the row was decided on (askBlock.asks, settlement's own test), which settlement writes back over
// a decided block. An open row's body is free: its question is still being written, and the row
// takes it.
func askAgreesWithItsNode(node *pmdoc.Node, ask model.Ask) bool {
	if _, flagged := node.Attrs["invalid"]; flagged {
		return false
	}
	if withoutStoredAskAttributes(attributesOf(node)) && !inStepWithTheRollWindow(node, ask, "") {
		return false
	}
	if ask.State == asks.StateOpen {
		return true
	}
	block, err := parseAskBlock(node)
	return err == nil && block.asks(model.NewAskEditPrevious(ask))
}

// inStepWithTheRollWindow reports whether node, an ask block, holds what the roll window keeps in
// step with ask, a copy's provenance among it where copied names the source's document, as one of
// its two writes leaves it, so that neither changes the block: the write with the answer's words
// (rollWindowAskAttributes), or the one without them (rollWindowAskAttributesWithheld), which the
// answer route and settlement write into a block whose answer the document has no room for, as
// the release before writes such a block. Only a block both would change shows a state the row
// does not hold.
func inStepWithTheRollWindow(node *pmdoc.Node, ask model.Ask, copied string) bool {
	return !rollWindowAskAttributes(true, ask, copied)(attributesOf(node)) ||
		!rollWindowAskAttributesWithheld(ask, copied)(attributesOf(node))
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
// (askServerAttributeNames) taken off, and whether any block held one. Each container on the way
// and each such block is a fresh node; every other node is the tree's own, which nothing here
// changes (withAskBlocksChanged).
func withoutAskServerAttributes(tree *pmdoc.Node) (*pmdoc.Node, bool) {
	return withAskBlocksChanged(tree, func(node *pmdoc.Node) (*pmdoc.Node, bool) {
		var stripped pmdoc.Attrs
		for _, name := range askServerAttributeNames {
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

// withoutReadings is readingCase's neutral: a copy of tree with the readings left out that each
// decision, and each ask block answered names, closes with (pmdoc.TypedContent), and whether it
// left any out. A reading in an ask block whose ask holds no answer stays. As with
// withoutAskServerAttributes, each container on the way and each block it left a reading out of is
// a fresh node, and every other node is the tree's own, which nothing here changes.
func withoutReadings(tree *pmdoc.Node, answered map[string]bool) (*pmdoc.Node, bool) {
	if tree.Type == "text" || len(tree.Children) == 0 {
		return tree, false
	}
	children, took := tree.Children, false
	if id, _ := tree.Attrs[pmdoc.BlockIDAttr].(string); tree.Type == decisionType || tree.Type == "ask" && answered[id] {
		if content, admitted := pmdoc.TypedContent(tree); len(admitted) > 0 {
			children, took = slices.Clip(content), true
		}
	}
	var below []*pmdoc.Node
	for index, child := range children {
		next, tookBelow := withoutReadings(child, answered)
		if !tookBelow {
			continue
		}
		if below == nil {
			below = slices.Clone(children)
		}
		below[index] = next
	}
	if below != nil {
		children, took = below, true
	}
	if !took {
		return tree, false
	}
	copied := *tree
	copied.Children = children
	return &copied, true
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
	// advice makes later in this transaction (supersededApprovalAdvice) takes it off the ledger.
	// The carry makes no such read itself: it runs at most once for a document in a transaction
	// (refuseSecondCarry).
	recordPendingReview(ctx, tx, artifactID, model.ArtifactReview{
		ArtifactID: artifactID,
		Version:    number,
		State:      "approved",
		Kind:       kind,
		Actor:      review.Actor,
		Reason:     &reason,
		AskID:      askID,
	})
	return nil
}

// restoresApprovedText reports whether markdown, the text a new version of artifactID is written
// with, is byte for byte the stored text of its version earlier, the one a person approved. A
// version that stores no markdown, a file's, holds no text and compares false. The text compared
// against it is the caller's own, which it holds already - the version it just wrote or queued -
// so only the earlier version is read, and that version is a stored one by construction: the
// version of a review NewestReview read from artifact_reviews, or the version an open approval
// request names for a settlement's move (MoveApprovalAsk). octet_length reads a stored text's size
// without decompressing it, so only a stored text of the same size is read, and the write of any
// other text reads no document.
func restoresApprovedText(ctx context.Context, tx pgx.Tx, artifactID string, earlier int, markdown string) (bool, error) {
	var stored string
	err := tx.QueryRow(ctx, `
		select markdown from artifact_versions
		where artifact_id = $1 and number = $2 and octet_length(markdown) = $3
	`, artifactID, earlier, len(markdown)).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read document version %d: %w", earlier, err)
	}
	return stored == markdown, nil
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

// carryVerdict is the rule over the two versions: N, input.version, carries the approval of A,
// input.approved, when every ask block of N agrees with its node (input.agrees); A's stored text,
// input.approvedText, is what this build renders A's tree to, so that what is compared is the
// bytes the person approved, and a renderer change between A and N is a difference no case
// carries; both trees, taken through every carry case (carryCases), render byte for byte the
// same; at least one case is one the two trees render apart without, since a version that equals
// A without any is the restore's to carry (restoresApprovedText), or a change nobody approved; and
// every block id A holds, but for the readings readingCase took out of it, still stands in N
// (blockIDsStand over the two trees each case left). It answers what the carried reason says the
// change was: the change of each case the two trees render apart without, in carryCases' order,
// joined and closed by carriedReasonEnd. Each case is skipped in turn, beside every case before it
// that the trees rendered the same without, at two renders a case. A case that took out of both
// trees only what stands the same in each - readings A holds and N holds unchanged - changed
// nothing, and is not named. Its error is a refusal of this renderer.
func carryVerdict(input carryInput) (string, bool, error) {
	if !input.agrees {
		return "", false, nil
	}
	approvedText, err := pmdoc.Render(input.approved)
	if err != nil || approvedText != input.approvedText {
		return "", false, err
	}
	// skipped names the cases a comparison is made without: none, for the verdict itself.
	skipped := make([]bool, len(carryCases))
	approved, version, took := input.neutral(skipped)
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
	for index, carried := range carryCases {
		skipped[index] = true
		approvedWithout, versionWithout, _ := input.neutral(skipped)
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

// neutral is input's two trees, A's and N's, each taken through every carry case in carryCases'
// order but those skipped names, and whether any of those cases took something out of either. The
// pass takes each pointer line of the step's write out of A once, whichever of restoreCase and
// conversionCase takes it (pointersLeft).
func (input carryInput) neutral(skipped []bool) (approved, version *pmdoc.Node, took bool) {
	left := slices.Clone(input.steps.pointerLines)
	input.pointersLeft = &left
	approved, version = input.approved, input.version
	for index, carried := range carryCases {
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

// approvedVersion is the tree of the stored version number of artifactID and the markdown it is
// stored with, and whether that version could be read: a version outside the Proof schema is
// logged and not readable, a number below 1, before the document's first version, is no version
// and not readable either, and a version holding an element this build does not know is an error
// (skipUnreadableVersion).
func approvedVersion(ctx context.Context, tx pgx.Tx, artifactID string, number int) (*pmdoc.Node, string, bool, error) {
	if number < 1 {
		return nil, "", false, nil
	}
	var markdown string
	if err := tx.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1 and number = $2`, artifactID, number).Scan(&markdown); err != nil {
		return nil, "", false, fmt.Errorf("read document version %d: %w", number, err)
	}
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return nil, "", false, skipUnreadableVersion(artifactID, number, err)
	}
	return tree, markdown, true, nil
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
	// of its own.
	changes, parts, err := planAskPartsCounted(ctx, tx, refs.NewReader(ctx, tx, "ask", ask.ID, serverURL),
		answered, answer.At, model.ReferenceChanges{})
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
		if err := settled.Queue(ctx, b); err != nil {
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
		if err := graphstore.QueueCaused(ctx, b, settled.Held, graphstore.LatestEvent); err != nil {
			return fmt.Errorf("state the answer of approval ask %s: %w", ask.ID, err)
		}
		ask.State, ask.Answer, ask.Resolution = answered.State, answered.Answer, answered.Resolution
		return nil
	})
}
