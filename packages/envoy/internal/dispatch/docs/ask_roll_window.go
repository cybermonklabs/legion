package docs

import (
	"context"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
)

// A person's answer is held once, on the ask's node, and no reader of this build reads it from an
// attribute of an ask block. The release before this one kept a second copy in the block: the
// ask's state, who answered it, when, what they chose and wrote, and, on a copy of an ask on
// another document, the ask and the document it was copied from. A page of that release still
// reads them from the block, so while a process of it serves beside one of this build, settlement
// keeps them in step with the asks row, which both builds write (rollWindowAskAttributes). Once
// processes.Live shows the whole fleet on one build, the fleet guard (anotherBuildServes), read at
// each write, makes every write keep only what a stored version's readers of that release still
// read - the state of an ask that is not open, for an answered ask who answered and when, and a copy's provenance
// (keptAskState) - and take the answer's words and selection out of the live document. The text
// routes take all eight off what they serve this build's readers (TextWithAsks). The schema
// declares them as it always did, so the build before compiles the store's schema as it compiles
// its own; only `invalid` is declared `markdown: false`, never reaches stored bytes, and stays in
// the live tree for the ask card to read (G1). A version that differs from the approved one in
// these attributes alone carries its approval (R10, carryVerdict).

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
//
// kept for one release: roll-window-attributes
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
// markdown, introduced. At this release it is this build's own
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
// `invalid`, which no rendering writes (pmdoc.BlockAttributeSchema.InMarkdown). It depends on the
// installed manifest alone, so it is read once for each manifest installed (storedAskNames) and
// every caller shares the list, which none writes.
func storedAskAttributes() []string {
	schema, _ := pmdoc.BlockTypeAttributes("ask")
	if read := storedAskNames.Load(); read != nil && sameAttributeSchema(read.schema, schema) {
		return read.names
	}
	names := make([]string, 0, len(schema))
	for _, attribute := range schema {
		if attribute.Server && attribute.InMarkdown() {
			names = append(names, attribute.Name)
		}
	}
	storedAskNames.Store(&storedAskNamesRead{schema: schema, names: names})
	return names
}

// storedAskNamesRead is storedAskAttributes's answer for one installed manifest: the ask type's
// attribute schema as that manifest holds it, and the names read off it.
type storedAskNamesRead struct {
	schema pmdoc.BlockAttributes
	names  []string
}

// storedAskNames holds storedAskAttributes's last read. A manifest installed since (pmdoc.Install,
// or one a test installs) is compiled whole into new slices, so its ask schema is another slice
// and the read is made again for it.
var storedAskNames atomic.Pointer[storedAskNamesRead]

// sameAttributeSchema reports whether a and b are one slice of one compiled manifest.
func sameAttributeSchema(a, b pmdoc.BlockAttributes) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// keptAskState is the write of an ask block's stored attributes once the fleet is one build, for
// the readers of the release before that read a stored version: that release's
// `dispatch request-approval` judges each ask block by the `state` the latest version's opener
// holds (refuseOpenDecisionBlocks), reads an opener that holds none as open, and refuses for good
// a decided block whose opener holds none; and its pages draw a stored version's ask from the
// block, an opener with no state as open. So every version written once the fleet is one build
// keeps, in step with ask - the block's own ask, or the ask of another document it is a copy of,
// whose document's address is copied - the state where the ask is not open (answered or resolved,
// a decline among the resolved), and none where it is open, which both of those readers read as
// open already; for an answered ask who answered and when; and for a copy, whatever its state,
// that ask and its document. That is the block that release wrote where its document had no room
// for the answer (rollWindowAskAttributesWithheld) but for an open ask's state, and the carry's
// agreement reads either as in step with the ask (askAgreesWithItsNode, copyAgreesWithItsSource).
// The answer's words and selection (answerWords) are left out. It reports whether it changed the
// block. Every write of an ask's state makes it on a fleet of one build as it makes
// rollWindowAskAttributes while two builds serve: the answer, resolve and decline routes
// (KeepAskBlockInStep), an issue's close and reopen (WriteIssueAskStates; the reopen takes the
// state off again) and settlement (askReadableWrite), so each decision settles into a version
// whose opener the release before's reader answers as that release did.
//
// kept for one release: stored-ask-state
func keptAskState(ask model.Ask, copied string) func(*pmdoc.Node) bool {
	if ask.State == asks.StateOpen {
		ask.State = ""
	}
	return rollWindowAskWrite(ask, copied, false)
}

// withoutUnkeptAskAttributes takes off node, an ask block, what no version written once the fleet
// is one build holds - the answer's words and selection (answerWords), and an open state, which a
// version keeps of no ask (keptAskState) - and reports whether it held any. It is that write for a
// writer that reads no ask row (storableVersionTree, the decisions step's writePresent): the rest
// stays as the live tree holds it, which every write of an ask's state keeps in step with it
// (keptAskState).
func withoutUnkeptAskAttributes(node *pmdoc.Node) bool {
	changed := false
	for _, name := range answerWords {
		if _, held := node.Attrs[name]; held {
			delete(node.Attrs, name)
			changed = true
		}
	}
	if node.Attrs["state"] == asks.StateOpen {
		delete(node.Attrs, "state")
		changed = true
	}
	return changed
}

// onItsNode is ask, a row of the asks table, with its state and answer as its node holds them
// (askNodeAnswers, read into nodes by ask id): the values keptAskState writes once the fleet is one
// build, each one the node holds no value of left empty, so the write takes it off the block.
func onItsNode(ask model.Ask, nodes map[string]askNodeAnswer) model.Ask {
	held := nodes[ask.ID]
	ask.State, ask.Answer = held.state, nil
	if held.state == asks.StateAnswered {
		at, _ := time.Parse(time.RFC3339Nano, held.answeredAt)
		ask.Answer = &model.AskAnswer{User: held.answeredBy, At: at, Selected: held.selected, Text: held.answer}
	}
	return ask
}

// rollWindowAskAttributes is the write that keeps the attributes of an ask block in step with
// ask, its row, while another build serves (twoBuilds), as the release before keeps them: the
// state always; who answered, when, the options chosen (never a nil list, which renders as null
// from one tree and as [] from the room) and the words, for an answered ask; and for a copy, the
// ask and copied, the address of the document it is on. Each other attribute of the seven a
// version can store (storedAskAttributes) is taken off. The write reports whether it changed the
// block, and is made again on the live document (repairLive). On a fleet of one build it is nil:
// each write then keeps only what a version keeps (keptAskState).
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

// rollWindowAskWrite is the write both of those make, with the answer's words where words. A value
// ask does not hold - no state, no answerer, no time, as an ask read off a node that holds none of
// them (onItsNode) - is taken off the block rather than written empty.
func rollWindowAskWrite(ask model.Ask, copied string, words bool) func(*pmdoc.Node) bool {
	return func(node *pmdoc.Node) bool {
		desired := pmdoc.Attrs{}
		if ask.State != "" {
			desired["state"] = ask.State
		}
		if copied != "" {
			desired["copied_from"] = ask.ID
			desired["copied_from_document"] = copied
		}
		if ask.State == asks.StateAnswered && ask.Answer != nil {
			if ask.Answer.User != "" {
				desired["answered_by"] = ask.Answer.User
			}
			if !ask.Answer.At.IsZero() {
				desired["answered_at"] = asks.AnsweredAtValue(ask.Answer.At)
			}
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

// answerWords are the attributes of an ask block that carry an answer's caller text: the options
// it selects, whose labels are the asker's text, and its words. Who answered and when are the
// server's, and stay on a block whose answer the document has no room for
// (rollWindowAskAttributesWithheld).
var answerWords = [...]string{"selected", "answer"}

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
// document is measured as it was.
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
// (storedAskAttributes) off node, an ask block, and reports whether it held any: the write once
// the fleet is one build to a block whose ask no write knows - one settlement cannot read and no
// row indexes, or one the decisions step moves that no row names - whose state is no ask's, a
// forged state or answerer among them where a browser put them on it.
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
// once the fleet is one build only those a version keeps (keptAskState).
func askReadableWrite(twoBuilds bool, ask model.Ask, copied string) func(*pmdoc.Node) bool {
	keep := rollWindowAskAttributes(twoBuilds, ask, copied)
	if keep == nil {
		keep = keptAskState(ask, copied)
	}
	return func(node *pmdoc.Node) bool {
		changed := clearAskInvalidAttribute(node)
		return keep(node) || changed
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
