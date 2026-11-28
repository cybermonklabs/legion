package docs

import (
	"fmt"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// refuseControlCharacters is the one refusal every document write path calls before storing a
// tree: any block id or non-server typed-block directive attribute that tree introduces over
// prior holding a control character (C0 U+0000 to U+001F, C1 U+0080 to U+009F, U+2028 or U+2029)
// is refused with pmdoc.ErrControlCharacter naming the block and the field. Every HTTP write
// route maps that sentinel to 400 INVALID_MARKDOWN.
func refuseControlCharacters(tree, prior *pmdoc.Node) error {
	return pmdoc.ControlCharacters(tree, prior)
}

// stampSettlementBlockIDs is stampBlockIDs for a room's settlement: in addition to missing,
// U+0000 and duplicate ids, any block id holding a control character (pmdoc.ValidWrittenBlockID)
// is replaced with a freshly minted id so a browser's CRDT update cannot carry one into a settled
// version. Kept beside settlement rather than in service.go so reads and historical backfills keep
// storedBlockID unchanged. keepers names, for an id two fragments of an issue room hold, the
// fragment that keeps it (roomIDKeepers, keepPriorHolders); every other id stays with its first
// holder in the room's order.
func stampSettlementBlockIDs(doc *crdt.Doc, layout roomLayout, origin any, keepers map[string]int) (*pmdoc.Node, []int, int, []stampedAsk, error) {
	minted := 0
	var stamped []stampedAsk
	tree, counts, err := rewriteRoom(doc, layout, origin, func(live *pmdoc.Node, counts []int) bool {
		before := askBlockOrder(live)
		keepPriorHolders(live, counts, keepers)
		minted = pmdoc.EnsureSettlementBlockIDsCount(live)
		if minted > 0 && len(before) > 0 {
			stamped = stampedAskBlocks(before, askBlockOrder(live))
		}
		return minted > 0
	})
	return tree, counts, minted, stamped, err
}

// reconcileControlCharacters repairs any non-server typed-block directive attribute in tree that
// holds a control character (pmdoc.IsControlCharacter): a browser's CRDT update is never refused
// at the socket, so settlement repairs what it leaves here, as reconcileCICheckBlocks repairs a
// broken ci_check. Each offending attribute is reset to its written default (or deleted when it
// has none) in tree and in the live room via repairLive, and one block.invalid event names the
// block, its type, the field and the control character.
func (r *settlementReconciliation) reconcileControlCharacters(
	artifactID string,
	owner artifactOwner,
	tree *pmdoc.Node,
	actor model.Actor,
) {
	if tree == nil {
		return
	}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		attrs, typed := pmdoc.BlockTypeAttributes(node.Type)
		if !typed {
			return true
		}
		blockID, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if blockID == "" {
			return true
		}
		for _, attr := range attrs {
			if attr.Server {
				continue
			}
			value, exists := node.Attrs[attr.Name]
			if !exists {
				continue
			}
			char, bad := firstAttrControlCharacter(value)
			if !bad {
				continue
			}
			attrName := attr.Name
			defaultVal, hasDefault := attr.Default, attr.Default != nil
			if r.repair(node, blockID, repairControlCharacterAttr(attrName, value, defaultVal, hasDefault)) {
				r.events = append(r.events, documentAskEvent(owner, artifactID, "block.invalid", actor, model.BlockInvalidEventPayload{
					BlockID:     blockID,
					Reason:      fmt.Sprintf("%s block %q field %s holds control character U+%04X", node.Type, blockID, attrName, char),
					DisturbedBy: actor,
					BlockType:   node.Type,
				}))
			}
		}
		return true
	})
}

// firstAttrControlCharacter returns the first control character in a typed-block attribute value,
// whether string or list of strings.
func firstAttrControlCharacter(value any) (rune, bool) {
	switch current := value.(type) {
	case string:
		return pmdoc.FirstControlCharacter(current)
	case []string:
		for _, item := range current {
			if char, bad := pmdoc.FirstControlCharacter(item); bad {
				return char, true
			}
		}
	case []any:
		for _, item := range current {
			if text, ok := item.(string); ok {
				if char, bad := pmdoc.FirstControlCharacter(text); bad {
					return char, true
				}
			}
		}
	}
	return 0, false
}

// repairControlCharacterAttr resets name on node when it still holds the offending value (so a
// peer's newer edit is left to its own settlement), restoring the schema's default or deleting
// the attribute when it has none.
func repairControlCharacterAttr(name string, judged, defaultVal any, hasDefault bool) func(*pmdoc.Node) bool {
	return func(node *pmdoc.Node) bool {
		current, exists := node.Attrs[name]
		if !exists {
			return false
		}
		if _, bad := firstAttrControlCharacter(current); !bad {
			return false
		}
		// Only repair if the offending value is still the same kind/value we judged, or still
		// holds a control character (a concurrent edit leaving another control character is still
		// invalid; leaving the check on HasControlCharacter ensures settlement never stores one).
		_ = judged
		if hasDefault {
			node.Attrs[name] = defaultVal
		} else {
			delete(node.Attrs, name)
		}
		return true
	}
}
