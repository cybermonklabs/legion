package docs

import (
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// decisionsPartTitle is the title of a spec's `## Decisions` part, the part its decisions and its
// returned answered asks stand in: a level-2 heading with this
// title, read without regard to case, and every top-level block after it up to the next heading
// of level 2 or above.
//
// kept for one release: decisions-step
const decisionsPartTitle = "Decisions"

// decisionsPartLevel is the level of a `## Decisions` heading.
//
// kept for one release: decisions-step
const decisionsPartLevel = 2

// headingLevel is a heading node's level, 0 for a node that is no heading.
//
// kept for one release: decisions-step
func headingLevel(node *pmdoc.Node) int {
	if node.Type != "heading" {
		return 0
	}
	switch level := node.Attrs["level"].(type) {
	case int:
		return level
	case int64:
		return int(level)
	case float64:
		return int(level)
	}
	return 1
}

// decisionsPart is where tree's `## Decisions` part stands among its top-level children: the
// index of its heading and the index after its last block, or -1 and -1 where tree holds none. The
// first such heading is the part.
//
// kept for one release: decisions-step
func decisionsPart(tree *pmdoc.Node) (int, int) {
	if tree == nil {
		return -1, -1
	}
	for index, child := range tree.Children {
		if headingLevel(child) != decisionsPartLevel ||
			!strings.EqualFold(strings.TrimSpace(pmdoc.TextContent(child)), decisionsPartTitle) {
			continue
		}
		end := index + 1
		for end < len(tree.Children) {
			if level := headingLevel(tree.Children[end]); level > 0 && level <= decisionsPartLevel {
				break
			}
			end++
		}
		return index, end
	}
	return -1, -1
}

// hasDecisionsPart reports whether tree holds a `## Decisions` part.
//
// kept for one release: decisions-step
func hasDecisionsPart(tree *pmdoc.Node) bool {
	start, _ := decisionsPart(tree)
	return start >= 0
}

// withoutDecisionsPartBlocks is a copy of tree without each top-level block of its `## Decisions`
// part drop names, and, where stepsHeading - the heading is the decisions step's doing
// (carryInput.stepsOwnHeading) - without the heading too once the part holds nothing else, and
// whether it took anything out. Every other heading stays whatever the part holds: an empty part a
// person approved is the person's, and a write that deletes it changes what they approved. tree
// itself, and every node it keeps, is left as it was.
//
// kept for one release: decisions-step
func withoutDecisionsPartBlocks(tree *pmdoc.Node, drop func(*pmdoc.Node) bool, stepsHeading bool) (*pmdoc.Node, bool) {
	start, end := decisionsPart(tree)
	if start < 0 {
		return tree, false
	}
	kept := make([]*pmdoc.Node, 0, len(tree.Children))
	kept = append(kept, tree.Children[:start]...)
	part := make([]*pmdoc.Node, 0, end-start-1)
	for _, child := range tree.Children[start+1 : end] {
		if !drop(child) {
			part = append(part, child)
		}
	}
	took := len(part) < end-start-1
	if stepsHeading && len(part) == 0 {
		took = true
	} else {
		kept = append(kept, tree.Children[start])
		kept = append(kept, part...)
	}
	kept = append(kept, tree.Children[end:]...)
	if !took {
		return tree, false
	}
	copied := *tree
	copied.Children = kept
	return &copied, true
}

// withoutBlocks is a copy of tree without each block drop names, wherever it stands, and whether
// it took one out. Each container on the way to one is a fresh node; every other node is tree's
// own, which nothing here changes.
//
// kept for one release: decisions-step
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
	copied := *tree
	copied.Children = children
	return &copied, true
}

// withoutAddedShapedPointers is a copy of tree with the `shaped` pointers added names taken off
// the ask block each names by block id, an ask holding an answer (decided), every other pointer
// of that block kept, and whether it took one off: the pointers the conversion's write gave an
// answered ask block a document held already (decisionStepCases.shaped). A block left holding no
// pointer loses the attribute, which it held none of before that write.
//
// kept for one release: decisions-step
func withoutAddedShapedPointers(tree *pmdoc.Node, added map[string][]string, decided map[string]bool) (*pmdoc.Node, bool) {
	if len(added) == 0 {
		return tree, false
	}
	return withAskBlocksChanged(tree, func(node *pmdoc.Node) (*pmdoc.Node, bool) {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		taken := added[id]
		if len(taken) == 0 || !decided[id] {
			return node, false
		}
		held := shapedPointersOf(node)
		kept := make([]string, 0, len(held))
		for _, pointer := range held {
			if !slices.Contains(taken, pointer) {
				kept = append(kept, pointer)
			}
		}
		if len(kept) == len(held) {
			return node, false
		}
		return withShapedPointers(node, kept), true
	})
}

// shapedAttribute is the `block[]` attribute of a decision and of an ask that names each spec
// node it shaped.
//
// kept for one release: decisions-step
const shapedAttribute = "shaped"

// shapedPointersOf is the pointers node's `shaped` attribute holds, in their order, as a parse
// leaves them (a list of strings) or as a write sets them (a []string).
//
// kept for one release: decisions-step
func shapedPointersOf(node *pmdoc.Node) []string {
	switch held := node.Attrs[shapedAttribute].(type) {
	case []string:
		return slices.Clone(held)
	case []any:
		pointers := make([]string, 0, len(held))
		for _, value := range held {
			if pointer, isString := value.(string); isString {
				pointers = append(pointers, pointer)
			}
		}
		return pointers
	}
	return nil
}

// withShapedPointers is a copy of node, an ask block, holding pointers as its `shaped` value, and
// no such attribute where pointers is empty. node itself is left as it was.
//
// kept for one release: decisions-step
func withShapedPointers(node *pmdoc.Node, pointers []string) *pmdoc.Node {
	shown := *node
	shown.Attrs = maps.Clone(node.Attrs)
	if len(pointers) == 0 {
		delete(shown.Attrs, shapedAttribute)
	} else {
		shown.Attrs[shapedAttribute] = pointers
	}
	return &shown
}

// restoredShape is what the restore's write did to the `shaped` pointers of one ask block it wrote
// into the spec (decisionStepCases.restoredShaped), beside the pointers the block held as the spec
// last held it (restoredBlock.held), which the write keeps as each reads once the pass is written
// (restoredPointers): added is each pointer it gave the block that the block held none of - a
// pointer of the decisions document's block of its ask, moved with it, and a spec block a section
// that restates its ask links - and moved is each pointer the block held that it rewrote, to where
// that pointer reads then, "" where it names no block then (movedPointer). They are all of the
// block's pointers restoreCase leaves out of its comparison with the block the person approved
// (restoredAskBlocks): a pointer an agent gave the block, or took off it, since the approval is a
// difference no case carries.
//
// kept for one release: decisions-step
type restoredShape struct {
	added []string
	moved map[string]string
}

// approvedBlock is node, the block the approved version holds under the restored block's id, with
// each of its pointers the restore's write rewrote on the block it wrote (moved) read as rewritten,
// left out where rewritten to no block, and every other pointer as it stands.
//
// kept for one release: decisions-step
func (shape restoredShape) approvedBlock(node *pmdoc.Node) *pmdoc.Node {
	held := shapedPointersOf(node)
	if len(shape.moved) == 0 || len(held) == 0 {
		return node
	}
	pointers := make([]string, 0, len(held))
	for _, pointer := range held {
		if to, moved := shape.moved[pointer]; moved {
			pointer = to
		}
		if pointer != "" {
			pointers = appendUnique(pointers, pointer)
		}
	}
	return withShapedPointers(node, pointers)
}

// writtenBlock is node, the block the restore's write wrote, without the pointers that write added
// (added), every other pointer kept in its order.
//
// kept for one release: decisions-step
func (shape restoredShape) writtenBlock(node *pmdoc.Node) *pmdoc.Node {
	if len(shape.added) == 0 {
		return node
	}
	// shapedPointersOf answers a list of its own, which the cut may reorder in place.
	held := shapedPointersOf(node)
	count := len(held)
	kept := slices.DeleteFunc(held, func(pointer string) bool { return slices.Contains(shape.added, pointer) })
	if len(kept) == count {
		return node
	}
	return withShapedPointers(node, kept)
}

// decidedAskBlocks is each ask block of a version, as asksAgreeWithTheirNodes read it (shown),
// whose ask holds an answer, by block id: its own document's row, or the source of a copy, read
// answered (asks.StateAnswered).
//
// kept for one release: decisions-step
func decidedAskBlocks(shown versionAsks) map[string]bool {
	decided := map[string]bool{}
	for id := range shown.nodes {
		if row, indexed := shown.rows[id]; indexed {
			decided[id] = row.State == asks.StateAnswered
			continue
		}
		if source, copied := shown.sources[id]; copied {
			decided[id] = source.ask.State == asks.StateAnswered
		}
	}
	return decided
}

// restoredAskBlocks is each ask block that stands in version's `## Decisions` part, that the
// restore's write added (written, decisionStepCases.restored), and that restoreCase takes out of
// both versions, whatever its ask's state - an answered ask returned, or an ask block of the
// decisions document moved, open ones among them (D1): one the approved version does not hold
// under its id, and one it holds under that id byte for byte as the person approved it
// (sameAskBytes) - its question and options with every link, code span and sub-list they hold,
// multiple and urgency, and its `shaped` pointers but for what the restore's own write did to
// them (shaped, restoredShape). A block whose words, bytes or pointers moved since is not among
// them, so the restore that returns it is a difference no case carries (R7) and the restore
// reports the approval superseded, and neither is a block an agent wrote there.
//
// kept for one release: decisions-step
func restoredAskBlocks(approved, version *pmdoc.Node, written map[string]bool, shaped map[string]restoredShape) map[string]bool {
	start, end := decisionsPart(version)
	if start < 0 {
		return nil
	}
	before, _ := askNodes(approved)
	restored := map[string]bool{}
	for _, child := range version.Children[start+1 : end] {
		id, _ := child.Attrs[pmdoc.BlockIDAttr].(string)
		if child.Type != "ask" || !written[id] {
			continue
		}
		if held, approvedHeld := before[id]; approvedHeld && !sameAskBytes(shaped[id].approvedBlock(held), shaped[id].writtenBlock(child)) {
			continue
		}
		restored[id] = true
	}
	return restored
}

// sameAskBytes reports whether two ask blocks render byte for byte the same once each is cut to
// what a person approves of an ask (askBytes).
//
// kept for one release: decisions-step
func sameAskBytes(a, b *pmdoc.Node) bool {
	first, err := askBytes(a)
	if err != nil {
		return false
	}
	second, err := askBytes(b)
	return err == nil && first == second
}

// askBytes is node, an ask block, as a document holding it alone renders it, without what other
// cases weigh: the server-owned attributes (askServerAttributeNames, attributeRemovalCase's) and
// the readings it closes with (readingCase's). What is left is what the person approved of it -
// its question and options, each link, code span and sub-list among them, urgency, multiple and
// its `shaped` pointers - under its id; the pointers the restore's own write changed are taken
// off before (restoredShape).
//
// kept for one release: decisions-step
func askBytes(node *pmdoc.Node) (string, error) {
	attrs := maps.Clone(node.Attrs)
	for _, name := range askServerAttributeNames {
		delete(attrs, name)
	}
	content, _ := pmdoc.TypedContent(node)
	return blockMarkdown(&pmdoc.Node{Type: node.Type, Attrs: attrs, Children: content})
}

// blockMarkdown is node's markdown as a document holding it alone renders it: what a pointer line
// the decisions step takes out of a spec is named by (decisionStepCases.pointerLines), since a
// paragraph's block id is minted afresh by every read of a stored version and its text is the
// paragraph.
//
// kept for one release: decisions-step
func blockMarkdown(node *pmdoc.Node) (string, error) {
	return pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{node}})
}

// pointerLineLead is the words a spec's pointer line to its issue's decisions document holds
// before the citation, case folded and its whitespace collapsed, and nothing else: none, `see`,
// `see the decisions in` (or `at`, or with no `the`), or `the decisions are in` (or `live`, or
// `at`, or with no `the`), as the release before's spec skill had an agent write the line ("The
// decisions are in" and the decisions document's dispatch:// address), and `the decisions this
// spec rests on are in`, the line the tests and the e2e seeds write for it. A paragraph whose
// words before the citation say anything more, such as "The decisions the board rejected twice
// are in", is a person's prose, which the step keeps.
//
// kept for one release: decisions-step
var pointerLineLead = regexp.MustCompile(`^(see|see (the )?decisions (in|at)|(the )?decisions( this spec rests on)? (are|live) (in|at))?$`)

// isPointerLine reports whether node is the pointer line of a spec's `## Decisions` part to the
// issue's decisions document, one of documents by ref_key: a paragraph whose one content is the
// citation of the document itself (a reference to one of its blocks is not one), read from the
// paragraph's markdown as every reader of a citation reads it (text.Extract). That citation is the
// paragraph's one reference and its last word, with nothing after it but a full stop and nothing
// before it but a lead naming the decisions (pointerLineLead). Every other paragraph is a person's
// prose, which the step keeps: one that cites the document among other words, or that cites a
// section of it.
//
// kept for one release: decisions-step
func isPointerLine(node *pmdoc.Node, documents map[string]bool, serverURL string) bool {
	if node.Type != "paragraph" || len(documents) == 0 {
		return false
	}
	markdown, err := blockMarkdown(node)
	if err != nil {
		return false
	}
	cited := text.Extract(markdown, serverURL)
	if len(cited) != 1 || cited[0].Kind != "artifact" || !documents[text.TargetID(cited[0])] {
		return false
	}
	words := strings.Fields(strings.TrimSuffix(strings.TrimSpace(markdown), "."))
	if len(words) == 0 || len(text.Extract(words[len(words)-1], serverURL)) != 1 {
		return false
	}
	return pointerLineLead.MatchString(strings.ToLower(strings.Join(words[:len(words)-1], " ")))
}
