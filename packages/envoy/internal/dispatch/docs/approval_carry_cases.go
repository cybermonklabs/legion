package docs

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

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
// what carryNewestApproval read of the two versions (carryInput): the ask blocks whose asks the
// approver answered since the approval, each ask block of the version whose ask holds an answer,
// the asks the restore returned, and what the decisions step's write added (decisionStepCases);
// approved says whether the tree is the approved version's, A's, or the new one's, N's, since the
// restore and the conversion take out of each only what their write changed in it.
type carryCase struct {
	change  string
	neutral func(tree *pmdoc.Node, input carryInput, approved bool) (*pmdoc.Node, bool)
}

// answerCase is the answer carry (LEGION-573): the ask blocks whose asks the approver of the
// document's newest review answered after that approval (carryInput.approverAnswered,
// approverAnswers), each taken out of both versions whole - a block added since the approval
// among them, and a block whose question was changed while it stood open, before the approver
// answered it. A person who answers a question after approving the document has agreed to what
// its block holds: the question as they answered it, and their answer, which the ask's node holds
// and no document does (R3). A block someone else answered, one the approver answered before the
// approval, and one nobody answered stay differences, and so does every byte outside such a block.
var answerCase = carryCase{
	change: "the approver's answers to its ask blocks, given after the approval",
	neutral: func(tree *pmdoc.Node, input carryInput, _ bool) (*pmdoc.Node, bool) {
		if len(input.approverAnswered) == 0 {
			return tree, false
		}
		return withoutBlocks(tree, func(node *pmdoc.Node) bool {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			return node.Type == "ask" && input.approverAnswered[id]
		})
	},
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
// (askServerAttributeNames), which the first accepted write once the fleet is one build cuts to the
// ones a version keeps (keptAskState), which every change of an ask's state then rewrites, and
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
// the restore's own write added (decisionStepCases.restored), and the heading where it is the
// step's doing (stepsOwnHeading), and out of A only the pointer line that write took out
// (pointerLinesTaken); a block an agent wrote into the part is no part of it, and neither is a
// heading A held, empty or not. Where the approved version held the ask's block already, under
// the id it stands at now, the block is taken out of both versions only where it is byte for byte
// the block the person approved (restoredAskBlocks), its `shaped` pointers among its bytes but for
// what the restore's own write did to them (decisionStepCases.restoredShaped), so a restore that
// returns other words, the same words in other bytes, or a pointer an agent gave the block since,
// than the approved ones is a difference no case carries. It holds for the restore's own write
// alone (decisionStepCases.restore): an agent's write that makes the same change carries nothing
// by it.
//
// kept for one release: decisions-step
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
		restored, restoredTook := withoutDecisionsPartBlocks(tree, input.pointerLinesTaken(approved), !approved && input.stepsOwnHeading())
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
// changed since, and a pointer an agent gave an ask, stay differences. The `## Decisions` heading
// is taken out of N with them where it is the step's doing (stepsOwnHeading) and the part holds
// nothing else; a heading A held stays in both. It holds for the
// conversion's own write alone (decisionStepCases.conversion): an agent's upload that adds the
// same `shaped` pointers to an answered ask supersedes the approval as any other change does.
//
// kept for one release: decisions-step
var conversionCase = carryCase{
	change: "the decisions its Decisions part holds in place of the pointer line to the issue's decisions document, " +
		"and the spec nodes its answered asks shaped, which an approval of a spec does not cover",
	neutral: func(tree *pmdoc.Node, input carryInput, approved bool) (*pmdoc.Node, bool) {
		if !input.steps.conversion {
			return tree, false
		}
		if approved {
			return withoutDecisionsPartBlocks(tree, input.pointerLinesTaken(true), false)
		}
		tree, took := withoutAddedShapedPointers(tree, input.steps.shaped, input.decided)
		converted, convertedTook := withoutDecisionsPartBlocks(tree, func(node *pmdoc.Node) bool {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			return node.Type == decisionType && input.steps.decisions[id]
		}, input.stepsOwnHeading())
		return converted, took || convertedTook
	},
}

// stepsOwnHeading reports whether N's `## Decisions` heading is the decisions step's doing, which
// restoreCase and conversionCase take out of N once its part holds nothing else: A holds no such
// part, and the step's write either added the heading (decisionStepCases.headingAdded) or took out
// the pointer line it stood over (decisionStepCases.pointerLines), the heading and line an agent
// of the release before wrote as one after the approval, which the step's blocks replace. Where A
// holds a `## Decisions` part, empty or not, its heading stays in both versions, so a write that
// leaves N without the heading a person approved is a difference no case carries.
//
// kept for one release: decisions-step
func (input carryInput) stepsOwnHeading() bool {
	return !hasDecisionsPart(input.approved) && (input.steps.headingAdded || len(input.steps.pointerLines) > 0)
}

// carryCases are the cases carryVerdict composes, in the order their reasons are named.
var carryCases = [...]carryCase{answerCase, readingCase, attributeRemovalCase, restoreCase, conversionCase}

// decisionStepCases names the phases of the decisions step whose own write a version is - the
// restore (restoreCase) and the conversion (conversionCase) - and what that write added to the
// document, which is all those two cases take out of a version: the block id of each answered ask
// block the restore wrote (restored), with what it did to the `shaped` pointers of each
// (restoredShaped), of each decision the conversion wrote (decisions), the `shaped` pointers it
// gave an answered ask block the document held already, by that block's id (shaped), whether it
// added the `## Decisions` heading to a document that held no such part (headingAdded), and the
// markdown of each pointer line the write took out of the document's `## Decisions` part
// (pointerLines, blockMarkdown), which is what those cases take out of the approved version. Only
// the step sets it: its version write hands it to snapshotVersion (issueRestore.writeVersion),
// which no route reaches and which carries it on the version's write (versionWrite.steps); every
// other version write leaves it zero, and its carry weighs the restore's and the conversion's
// cases nowhere.
//
// kept for one release: decisions-step
type decisionStepCases struct {
	restore, conversion bool
	restored, decisions map[string]bool
	restoredShaped      map[string]restoredShape
	shaped              map[string][]string
	headingAdded        bool
	pointerLines        []string
}

// any reports whether the version is the decisions step's own write.
//
// kept for one release: decisions-step
func (steps decisionStepCases) any() bool {
	return steps.restore || steps.conversion
}

// carryInput is what carryVerdict decides on: A's tree and the markdown it is stored with, whether
// another renderer version stored that markdown or none stamped it (approvedOtherRenderer,
// pmdoc.OtherRenderer; false for a version this renderer version stored), and N's tree, each as its
// own read gives it, which of N's ask blocks show an ask the approver answered after the
// approval (approverAnswered, approverAnswers), which show an ask that holds an answer among those
// closing with readings (answeredAskBlocks) and among them all (decided), the answered ask blocks
// of N's `## Decisions` part the restore's write added that restoreCase takes out of both
// versions (restoredAskBlocks), and the decisions step's phases whose own write N is, with what
// that write added (steps), the only writes those two cases reach.
// Every ask block of N agrees with its node (asksAgreeWithTheirNodes), which each caller holds
// before it builds one.
type carryInput struct {
	approved              *pmdoc.Node
	approvedText          string
	approvedOtherRenderer bool
	version               *pmdoc.Node
	approverAnswered      map[string]bool
	answered              map[string]bool
	// kept for one release: decisions-step
	decided map[string]bool
	// kept for one release: decisions-step
	restored map[string]bool
	// kept for one release: decisions-step
	steps decisionStepCases
	// pointersLeft is, within one pass of the cases over the two versions (neutral), the pointer
	// lines of the step's write that no case has taken out of A yet (pointerLinesTaken).
	//
	// kept for one release: decisions-step
	pointersLeft *[]string
}

// pointerLinesTaken is what restoreCase and conversionCase take out of the `## Decisions` part of
// one of the two versions beside the blocks the step's write added: of A (approved), each pointer
// line that write took out of the document (decisionStepCases.pointerLines), as the first
// paragraph of the part whose markdown is that line's, once each in a pass of the cases between
// the two of them (pointersLeft), however many paragraphs of A are that line; of N, none, since
// the write left none of them there. A paragraph a person approved that the write did not take
// out - one citing the decisions document among other words or citing a block of it, which the
// restore keeps (isPointerLine), or a second copy of the pointer line - stays in A. The answer is
// a drop for one call of withoutDecisionsPartBlocks, which it walks in order.
//
// kept for one release: decisions-step
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

// withoutBlocks is a copy of tree without each block drop names, wherever it stands, and whether
// it took one out. Each container on the way to one is a fresh node; every other node is tree's
// own, which nothing here changes. The answer case takes the blocks the approver answered out
// through it (answerCase), and the restore's case the blocks the restore wrote (restoreCase).
//
// A container the drop empties - a blockquote, a footnote definition, a typed container or the
// document, which held only blocks drop names - is left holding one empty paragraph, which is what
// both parsers read an empty container as, as WithoutAskBlocks leaves one. The renderer refuses a
// container with no block children (pmdoc.Render), so without it the two trees would not render
// at all, and the carry would fail where an answered block stood alone in its container.
func withoutBlocks(tree *pmdoc.Node, drop func(*pmdoc.Node) bool) (*pmdoc.Node, bool) {
	if tree.Type == "text" || len(tree.Children) == 0 {
		return tree, false
	}
	var children []*pmdoc.Node
	took := false
	for index, child := range tree.Children {
		if drop(child) {
			if children == nil {
				children = slices.Clone(tree.Children[:index])
			}
			took = true
			continue
		}
		next, tookBelow := withoutBlocks(child, drop)
		if tookBelow && children == nil {
			children = slices.Clone(tree.Children[:index])
		}
		if children != nil {
			children = append(children, next)
		}
		took = took || tookBelow
	}
	if !took {
		return tree, false
	}
	if len(children) == 0 {
		children = append(children, &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}})
	}
	copied := *tree
	copied.Children = children
	return &copied, true
}

// carryVerdict is the rule over the two versions, every ask block of N agreeing with its node
// (asksAgreeWithTheirNodes, which its callers hold first): N, input.version, carries the approval
// of A, input.approved, when A's stored text,
// input.approvedText, is what this build renders A's tree to, so that what is compared is the
// bytes the person approved, or a text another renderer version stored or none stamped
// (input.approvedOtherRenderer), which A's tree holds as this build renders it, as the restore
// compares it (restoresApprovedText, holdsRendering), so a renderer change between A and N is no
// difference a case must carry; both trees, taken through every carry case (carryCases), render
// byte for byte the same; at least one case is one the two trees render apart without, since a
// version that equals A without any is the restore's to carry (restoresApprovedText), or a change
// nobody approved; and every block id A holds, but for the readings readingCase took out of it,
// still stands in N (blockIDsStand over the two trees each case left). It answers what the carried
// reason says the change was: the change of each case the two trees render apart without, in
// carryCases' order, joined and closed by carriedReasonEnd. Each case is skipped in turn, beside
// every case before it that the trees rendered the same without, at two renders a case. A case
// that took out of both trees only what stands the same in each - readings A holds and N holds
// unchanged - changed nothing, and is not named. Its error is a refusal of this renderer.
func carryVerdict(input carryInput) (string, bool, error) {
	return carryVerdictOver(input, carryCases[:])
}

// carryVerdictOver is carryVerdict over cases alone, in their order: carryCases for the carry, and
// R10's case alone for the turn of an approval request (changesAskAttributesAlone). A's tree is
// ParseRendering's read of its stored text, so for an A another renderer version stored, A's text
// read and rendered again by this build (readsAs) is the rendering of that tree, which no second
// parse needs to say.
func carryVerdictOver(input carryInput, cases []carryCase) (string, bool, error) {
	approvedText, err := pmdoc.Render(input.approved)
	if err != nil || approvedText != input.approvedText && !input.approvedOtherRenderer {
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
// order but those skipped names, and whether any of those cases took something out of either. The
// pass takes each pointer line of the step's write out of A once, whichever of restoreCase and
// conversionCase takes it (pointersLeft).
func (input carryInput) neutral(cases []carryCase, skipped []bool) (approved, version *pmdoc.Node, took bool) {
	left := slices.Clone(input.steps.pointerLines)
	input.pointersLeft = &left
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
// turn across it (changesAskAttributesAlone): R10's.
var turnCases = []carryCase{attributeRemovalCase}

// changesAskAttributesAlone reports whether the version number of artifactID, written as markdown
// from tree, differs from its stored version earlier in the server-owned attributes of its ask
// blocks alone, by the test R10's case makes for the carry: every ask block of the version agrees
// with its node (asksAgreeWithTheirNodes), and the two texts, earlier's as stored, or as this build
// renders it where another renderer version stored it or none stamped it, and this version's, are
// byte for byte the same once the eight attributes are taken off every ask block of each
// (carryVerdictOver with attributeRemovalCase alone), every block id earlier holds still standing.
// It is MoveApprovalAsk's keep-turn rule beside restoresApprovedText: the version a person's
// answer, resolve or decline writes, or a reopen's settlement, changes no more than that when the
// answer, resolve, decline or reopen is that version's whole change. Text a person was editing in
// the browser and settled into the same version makes this false, and the request goes back to its
// agent. A nil tree is markdown's read-back, parsed here; a version holding no ask block differs in
// no ask attribute. Its reads run where it is called.
func changesAskAttributesAlone(ctx context.Context, tx pgx.Tx, artifactID string, earlier, number int, markdown string, tree *pmdoc.Node) (bool, error) {
	if tree == nil {
		parsed, err := pmdoc.ParseRendering(markdown)
		if err != nil {
			return false, skipUnreadableVersion(artifactID, number, err)
		}
		tree = parsed
	}
	shown, agrees, err := asksAgreeWithTheirNodes(ctx, tx, artifactID, tree)
	if err != nil || !agrees || len(shown.nodes) == 0 {
		return false, err
	}
	approved, approvedText, otherRenderer, readable, err := approvedVersion(ctx, tx, artifactID, earlier)
	if err != nil || !readable {
		return false, err
	}
	_, alone, err := carryVerdictOver(carryInput{approved: approved, approvedText: approvedText, approvedOtherRenderer: otherRenderer, version: tree}, turnCases)
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
