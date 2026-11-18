package docs

import (
	"context"
	"reflect"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
)

// A person's answer is held once, on the ask's node (dispatch://INFRA-1150 R3), and no attribute
// of an ask block in a document holds it. The release before this one kept a second copy in the
// block: the ask's state, who answered it, when, what they chose and wrote, and, on a copy of an
// ask on another document, the ask and the document it was copied from. A page of that release
// still reads them from the block, so while a process of it serves beside one of this build,
// settlement keeps them in step with the asks row, which both builds write
// (rollWindowAskAttributes). Once processes.Live shows the whole fleet on one build, the fleet
// guard (anotherBuildServes), read at each write, makes settlement take them out of the live
// document instead (withoutStoredAskAttributes), so no version holds them. The schema declares
// them as it always did, so the build before compiles the store's schema as it compiles its own;
// only `invalid` is declared `markdown: false`, never reaches stored bytes, and stays in the live
// tree for the ask card to read (G1). The first version a document writes without them carries
// its approval (R10, carryVerdict).

// anotherBuildServes reports whether processes.Live, read through q, lists a process of a build
// that still reads an ask's state and answer from the attributes of its block: one whose
// registered schema version is below the version at which answers left the markdown
// (answersLeftTheMarkdown). That version is fixed, the one this release introduced: the release
// before this one reads schema version 4 and this release 5, so a process of it makes two builds,
// and so does a process of a build older still, which registers no schema number at all and
// whose row holds the column's default, 0 (processes.Process.PredatesFleetColumns). A process at
// that version or past it reads every answer from the server, as this build does, so to this
// guard the fleet is one build however many commits and schema versions it serves: a later
// release at version 6, which still holds this guard, reads a process of this release as one of
// its own fleet rather than as a reader of block attributes, and writes them back into nobody's
// documents.
//
// The rule reads the schema numbers, never the commits among themselves, for two reasons. This
// process writes documents before it registers - main restores the decided wordings, repairs the
// issues' asks and starts settlement's resumption before joinRegistry - and after a lapsed
// heartbeat drops its row from the live window, and a fleet of base processes alone lists one
// commit then, though the base's pages read every block's attributes. And a later release deployed
// over this one lists two commits while both read answers from the server, so comparing commits
// would open a roll window that serves no page. The decisions step compares commits with its own
// (DecisionStep.anotherBuildLive), since any other build, whatever schema it reads, can write a
// row without its node; this guard is the ask attributes' alone. A database no process registers
// in, as a docs test's is, lists none and holds no other build either.
func anotherBuildServes(ctx context.Context, q processes.Querier) (bool, error) {
	live, err := processes.Live(ctx, q)
	if err != nil {
		return false, err
	}
	left := answersLeftTheMarkdown()
	for _, process := range live {
		if process.SchemaVersion < left {
			return true, nil
		}
	}
	return false, nil
}

// answersLeftTheMarkdown is the schema version from which no build reads an ask's answer from
// its block: the version that introduced the `decision` type, read off the installed schema
// (its since), which is the version this release, the one that took the answer out of the
// markdown (dispatch://INFRA-1150 R3 and R10), introduced. At this release it is this build's own
// version, so the rule reads as R10 and H5 word it - no process below this build's schema version
// - and a later schema keeps that since as it keeps every element's, so the number does not move
// with the build reading it. A manifest that declares no `decision` type - only one a test
// installs in place of this build's - bounds the fleet at its own version.
func answersLeftTheMarkdown() int {
	if versions, declared := (pmdoc.Element{Kind: pmdoc.TypedBlockElement, Name: "decision"}).Versions(); declared {
		return versions.Since
	}
	return pmdoc.SchemaVersion()
}

// storedAskAttributes is the server attributes of an ask block that its markdown carries, read off
// the block schema, which names each once: the seven of the eight a version can store - state,
// answered_by, answered_at, selected, answer, copied_from and copied_from_document - and never
// `invalid`, which no rendering writes (pmdoc.BlockAttributeSchema.InMarkdown).
func storedAskAttributes() []string {
	attributes, _ := pmdoc.BlockTypeAttributes("ask")
	names := make([]string, 0, len(attributes))
	for _, attribute := range attributes {
		if attribute.Server && attribute.InMarkdown() {
			names = append(names, attribute.Name)
		}
	}
	return names
}

// rollWindowAskAttributes is the write that keeps the attributes of an ask block in step with
// ask, its row, while another build serves (twoBuilds), as the release before keeps them: the
// state always; who answered, when, the options chosen (never a nil list, which renders as null
// from one tree and as [] from the room) and the words, for an answered ask; and for a copy, the
// ask and copied, the address of the document it is on. Each other attribute of the seven a
// version can store (storedAskAttributes) is taken off. The write reports whether it changed the
// block, and is made again on the live document (repairLive). On a fleet of one build it is nil:
// settlement then takes the seven out of the block (askReadableWrite).
//
// The options chosen and the words are caller text, and a copy of them in a block grows its
// document as an edit does, so each write that makes that copy weighs it against what one upload
// may hold, as the release before does: the answer route (KeepAskBlockInStep) and settlement
// (withholdAnswersWithoutRoom) write a block whose answer the document has no room for without
// them (rollWindowAskAttributesWithheld), and the ask keeps the answer whole.
//
// kept for one release: roll-window-attributes
func rollWindowAskAttributes(twoBuilds bool, ask model.Ask, copied string) func(*pmdoc.Node) bool {
	if !twoBuilds {
		return nil
	}
	return rollWindowAskWrite(ask, copied, true)
}

// rollWindowAskAttributesWithheld is rollWindowAskAttributes's write for a block whose answer the
// document has no room for: the block says who answered it and when, and carries neither the
// options chosen nor the words (answerWords), as the release before wrote such a block.
func rollWindowAskAttributesWithheld(ask model.Ask, copied string) func(*pmdoc.Node) bool {
	return rollWindowAskWrite(ask, copied, false)
}

// rollWindowAskWrite is the write both of those make, with the answer's words where words.
func rollWindowAskWrite(ask model.Ask, copied string, words bool) func(*pmdoc.Node) bool {
	return func(node *pmdoc.Node) bool {
		desired := pmdoc.Attrs{"state": ask.State}
		if copied != "" {
			desired["copied_from"] = ask.ID
			desired["copied_from_document"] = copied
		}
		if ask.State == asks.StateAnswered && ask.Answer != nil {
			desired["answered_by"] = ask.Answer.User
			desired["answered_at"] = asks.AnsweredAtValue(ask.Answer.At)
			if words {
				desired["selected"] = append([]string{}, ask.Answer.Selected...)
				if ask.Answer.Text != nil {
					desired["answer"] = *ask.Answer.Text
				}
			}
		}
		changed := false
		for _, name := range storedAskAttributes() {
			want, wanted := desired[name]
			got, held := node.Attrs[name]
			switch {
			case wanted && held && sameAskAttribute(got, want):
			case wanted:
				node.Attrs[name] = want
				changed = true
			case held:
				delete(node.Attrs, name)
				changed = true
			}
		}
		return changed
	}
}

// holdsAnswerWords reports whether node, an ask block, carries an answer's words (answerWords).
func holdsAnswerWords(node *pmdoc.Node) bool {
	for _, name := range answerWords {
		if _, held := node.Attrs[name]; held {
			return true
		}
	}
	return false
}

// grownBy is document, the rendering of a document holding the block from, grown by what to, the
// same block with other attributes, adds to the block's own rendering. An answer's words lengthen
// the block's directive line, which carries every attribute quoted, and make no element, so the
// document is measured as it was (answersWithRoom weighs the text routes' answers so too).
func grownBy(document rendering, from, to *pmdoc.Node) (rendering, error) {
	was, err := renderTree(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{from}})
	if err != nil {
		return rendering{}, err
	}
	is, err := renderTree(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{to}})
	if err != nil {
		return rendering{}, err
	}
	return rendering{bytes: document.bytes + len(is) - len(was), measured: document.measured}, nil
}

// answerHasRoom reports whether the document tree, which holds block, has room for block with
// attrs, the attributes an answer's words are written into (rollWindowAskAttributes), by what one
// upload may hold (weigh).
func answerHasRoom(tree, block *pmdoc.Node, attrs pmdoc.Attrs) (bool, error) {
	markdown, err := renderTree(tree)
	if err != nil {
		return false, err
	}
	document := renderingOf(markdown)
	answered := *block
	answered.Attrs = attrs
	grown, err := grownBy(document, block, &answered)
	if err != nil {
		return false, err
	}
	return weigh(document, grown) == nil, nil
}

// withoutIndexes is items but those at the indexes dropped names, in their order, in items' own
// array.
func withoutIndexes[T any](items []T, dropped map[int]bool) []T {
	if len(dropped) == 0 {
		return items
	}
	kept := items[:0]
	for index, item := range items {
		if !dropped[index] {
			kept = append(kept, item)
		}
	}
	return kept
}

// sameAskAttribute reports whether got, an attribute value an ask block holds, is want: a list
// of strings equals another of the same items in the same order whichever of []string and []any
// holds it, as a tree read from the room holds a list another tree holds as []string.
func sameAskAttribute(got, want any) bool {
	gotItems, gotIsItems := askStringItems(got)
	wantItems, wantIsItems := askStringItems(want)
	if gotIsItems || wantIsItems {
		return gotIsItems && wantIsItems && reflect.DeepEqual(gotItems, wantItems)
	}
	return reflect.DeepEqual(got, want)
}

func askStringItems(value any) ([]string, bool) {
	switch items := value.(type) {
	case []string:
		return items, true
	case []any:
		out := make([]string, len(items))
		for index, item := range items {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			out[index] = text
		}
		return out, true
	default:
		return nil, false
	}
}

// withoutStoredAskAttributes takes the seven attributes a version can store
// (storedAskAttributes) off node, an ask block, and reports whether it held any. A block holds
// them once the fleet is one build only where the release before wrote them, or where a browser
// put them on it, a forged state or answerer among them.
func withoutStoredAskAttributes(node *pmdoc.Node) bool {
	changed := false
	for _, name := range storedAskAttributes() {
		if _, held := node.Attrs[name]; held {
			delete(node.Attrs, name)
			changed = true
		}
	}
	return changed
}

// askReadableWrite is settlement's write to an ask block it reads (readAskBlockStrictly), whose
// ask, or the ask it is a copy of on the document whose address is copied, is ask: the block
// parses, so it carries no `invalid` and no old reason shows on it (G1); and its other server
// attributes are kept in step with ask while another build serves (rollWindowAskAttributes), and
// taken out once the fleet is one build (withoutStoredAskAttributes).
func askReadableWrite(twoBuilds bool, ask model.Ask, copied string) func(*pmdoc.Node) bool {
	keep := rollWindowAskAttributes(twoBuilds, ask, copied)
	return func(node *pmdoc.Node) bool {
		changed := clearAskInvalidAttribute(node)
		if keep != nil {
			return keep(node) || changed
		}
		return withoutStoredAskAttributes(node) || changed
	}
}

// askReadableWriteWithheld is askReadableWrite while another build serves, for a block whose
// answer the document has no room for (rollWindowAskAttributesWithheld).
func askReadableWriteWithheld(ask model.Ask, copied string) func(*pmdoc.Node) bool {
	keep := rollWindowAskAttributesWithheld(ask, copied)
	return func(node *pmdoc.Node) bool {
		changed := clearAskInvalidAttribute(node)
		return keep(node) || changed
	}
}
