package docs

import (
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// decisionsPartTitle is the title of a spec's `## Decisions` part, the part its decisions and its
// returned answered asks stand in: a level-2 heading with this
// title, read without regard to case, and every top-level block after it up to the next heading
// of level 2 or above.
const decisionsPartTitle = "Decisions"

// decisionsPartLevel is the level of a `## Decisions` heading.
const decisionsPartLevel = 2

// headingLevel is a heading node's level, 0 for a node that is no heading.
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
func hasDecisionsPart(tree *pmdoc.Node) bool {
	start, _ := decisionsPart(tree)
	return start >= 0
}

// withoutDecisionsPartBlocks is a copy of tree without each top-level block of its `## Decisions`
// part drop names, and without the part's heading where the part then holds nothing else, and
// whether it took anything out. tree itself, and every node it keeps, is left as it was.
func withoutDecisionsPartBlocks(tree *pmdoc.Node, drop func(*pmdoc.Node) bool) (*pmdoc.Node, bool) {
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
	if len(part) > 0 {
		kept = append(kept, tree.Children[start])
		kept = append(kept, part...)
	} else {
		took = true
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
		shown := *node
		shown.Attrs = maps.Clone(node.Attrs)
		if len(kept) == 0 {
			delete(shown.Attrs, shapedAttribute)
		} else {
			shown.Attrs[shapedAttribute] = kept
		}
		return &shown, true
	})
}

// shapedAttribute is the `block[]` attribute of a decision and of an ask that names each spec
// node it shaped.
const shapedAttribute = "shaped"

// shapedPointersOf is the pointers node's `shaped` attribute holds, in their order, as a parse
// leaves them (a list of strings) or as a write sets them (a []string).
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

// decidedAskBlocks is each ask block of a version, as asksAgreeWithTheirNodes read it (shown),
// whose ask holds an answer, by block id: its own document's row, or the source of a copy, read
// answered (asks.StateAnswered).
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
// under its id, and one it holds under that id with the words the person approved - the question,
// the options, multiple and urgency (sameAskWording). A block whose words moved since is not among
// them, so the restore that returns it is a difference no case carries (R7), and neither is a
// block an agent wrote there.
func restoredAskBlocks(approved, version *pmdoc.Node, written map[string]bool) map[string]bool {
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
		if held, approvedHeld := before[id]; approvedHeld && !sameAskWording(held, child) {
			continue
		}
		restored[id] = true
	}
	return restored
}

// sameAskWording reports whether two ask blocks ask the same thing, as settlement reads each
// (parseAskBlock): the same question, options, multiple and urgency.
func sameAskWording(a, b *pmdoc.Node) bool {
	first, err := parseAskBlock(a)
	if err != nil {
		return false
	}
	second, err := parseAskBlock(b)
	return err == nil && first.question == second.question && reflect.DeepEqual(first.options, second.options) &&
		first.multiple == second.multiple && first.urgency == second.urgency
}

// blockMarkdown is node's markdown as a document holding it alone renders it: what a pointer line
// the decisions step takes out of a spec is named by (decisionStepCases.pointerLines), since a
// paragraph's block id is minted afresh by every read of a stored version and its text is the
// paragraph.
func blockMarkdown(node *pmdoc.Node) (string, error) {
	return pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{node}})
}
