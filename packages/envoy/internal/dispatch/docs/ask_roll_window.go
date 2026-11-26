package docs

import (
	"context"
	"reflect"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A person's answer is held once, on the ask's node, and no reader of this build reads it from an
// attribute of an ask block. The release before this one kept a second copy in the block: the
// ask's state, who answered it, when, what they chose and wrote, and, on a copy of an ask on
// another document, the ask and the document it was copied from. Every write keeps only what a
// stored version's readers of that release still read - the state of an ask that is not open, for
// an answered ask who answered and when, and a copy's provenance (keptAskState) - and takes the
// answer's words and selection out of the live document. The text routes take all eight off what
// they serve this build's readers (TextWithAsks). The schema declares them as it always did, so
// the build before compiles the store's schema as it compiles its own; only `invalid` is declared
// `markdown: false`, never reaches stored bytes, and stays in the live tree for the ask card to
// read (G1). A version that differs from the approved one in these attributes alone carries its
// approval (R10, carryVerdict).

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

// keptAskState is the write of an ask block's stored attributes, for the readers of the release
// before that read a stored version: that release's `dispatch request-approval` judges each ask
// block by the `state` the latest version's opener holds (refuseOpenDecisionBlocks), reads an
// opener that holds none as open, and refuses for good a decided block whose opener holds none;
// and its pages draw a stored version's ask from the block, an opener with no state as open. So
// every version keeps, in step with ask - the block's own ask, or the ask of another document it
// is a copy of, whose document's address is copied - the state where the ask is not open
// (answered or resolved, a decline among the resolved), and none where it is open, which both of
// those readers read as open already; for an answered ask who answered and when; and for a copy,
// whatever its state, that ask and its document. That is the block that release wrote where its
// document had no room for the answer but for an open ask's state, and the carry's agreement reads
// either as in step with the ask (askAgreesWithItsNode, copyAgreesWithItsSource). The answer's
// words and selection (answerWords) are left out. It reports whether it changed the block. Every
// write of an ask's state makes it: the answer, resolve and decline routes (KeepAskBlockInStep),
// an issue's close and reopen (WriteIssueAskStates; the reopen takes the state off again) and
// settlement (askReadableWrite), so each decision settles into a version whose opener the release
// before's reader answers as that release did.
//
// kept for one release: stored-ask-state
func keptAskState(ask model.Ask, copied string) func(*pmdoc.Node) bool {
	if ask.State == asks.StateOpen {
		ask.State = ""
	}
	return rollWindowAskWrite(ask, copied, false)
}

// withoutUnkeptAskAttributes takes off node, an ask block, what no version holds - the answer's
// words and selection (answerWords), and an open state, which a
// version keeps of no ask (keptAskState) - and reports whether it held any. It is that write, for
// a block that has an ask (askBlocksWithAnAsk), of a writer that reads no ask's state: a version
// written without settling (storableVersionTree). The rest stays as the live tree holds it, which
// every write of an ask's state keeps in step with it (keptAskState).
//
// kept for one release: stored-ask-state
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
// (askNodeAnswers, read into nodes by ask id): the values keptAskState writes, each one the node
// holds no value of left empty, so the write takes it off the block.
//
// kept for one release: stored-ask-state
func onItsNode(ask model.Ask, nodes map[string]askNodeAnswer) model.Ask {
	held := nodes[ask.ID]
	ask.State, ask.Answer = held.state, nil
	if held.state == asks.StateAnswered {
		at, _ := time.Parse(time.RFC3339Nano, held.answeredAt)
		ask.Answer = &model.AskAnswer{User: held.answeredBy, At: at, Selected: held.selected, Text: held.answer}
	}
	return ask
}

// rollWindowAskWrite is the write of an ask block's stored attributes in step with ask, with the
// answer's words where words: without them it is keptAskState's write, and with them it is the
// block a version of the release before holds, which the carry's agreement reads
// (showsItsAsksState). A value ask does not hold - no state, no answerer, no time, as an ask read
// off a node that holds none of them (onItsNode) - is taken off the block rather than written
// empty.
//
// kept for one release: stored-ask-state
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
// server's, and a version keeps them (keptAskState).
var answerWords = [...]string{"selected", "answer"}

// sameAskAttribute reports whether got, an attribute value an ask block holds, is want: a list
// of strings equals another of the same items in the same order whichever of []string and []any
// holds it, as a tree read from the room holds a list another tree holds as []string. It and
// askStringItems serve the form a version keeps (rollWindowAskWrite), so they go with it.
//
// kept for one release: stored-ask-state
func sameAskAttribute(got, want any) bool {
	gotItems, gotIsItems := askStringItems(got)
	wantItems, wantIsItems := askStringItems(want)
	if gotIsItems || wantIsItems {
		return gotIsItems && wantIsItems && reflect.DeepEqual(gotItems, wantItems)
	}
	return reflect.DeepEqual(got, want)
}

// askStringItems is value as a list of strings, and whether it is one (sameAskAttribute).
//
// kept for one release: stored-ask-state
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
// (storedAskAttributes) off node, an ask block, and reports whether it held any: the write to a
// block whose ask no write knows - one settlement cannot read and no row indexes, a closed issue's
// block no row indexes (unindexedAskWrite, readIssueAskStateWrites), or one a version writes that
// has no ask row and is no copy (storableVersionTree) - whose state is no ask's, a forged state or
// answerer among them where a browser put them on it.
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

// holdsKeptAskAttribute reports whether node, an ask block, holds an attribute a version keeps of
// an ask, which withoutUnkeptAskAttributes leaves on it - a state other than open, an answerer, a
// time or a copy's provenance (keptAskState) - and which a block that has no ask row and is no
// copy of an ask holds none of (askBlocksWithAnAsk).
//
// kept for one release: stored-ask-state
func holdsKeptAskAttribute(node *pmdoc.Node) bool {
	for _, name := range storedAskAttributes() {
		value, held := node.Attrs[name]
		if held && !slices.Contains(answerWords[:], name) && (name != "state" || value != asks.StateOpen) {
			return true
		}
	}
	return false
}

// askBlocksWithAnAsk is the id of each ask block of tree, the document as a write of it read it,
// that has an ask (askBlockAsks): one an ask of document indexes, or a readable block that is a
// copy of an ask on another document of its owner, as the close's final sweep tells them apart
// (readIssueAskStateWrites). Every other ask block of tree has no ask row and is no copy of an
// ask, so it keeps none of the eight (R10), whatever its live tree gives it: the state="resolved" a
// closed issue's settlement or the close's sweep wrote while two builds served, or what a browser
// put on it.
//
// kept for one release: stored-ask-state
func askBlocksWithAnAsk(ctx context.Context, tx pgx.Tx, document string, tree *pmdoc.Node) (map[string]bool, error) {
	rows, sources, err := askBlockAsks(ctx, tx, document, tree)
	if err != nil {
		return nil, err
	}
	withAsk := make(map[string]bool, len(rows)+len(sources))
	for id := range rows {
		withAsk[id] = true
	}
	for id := range sources {
		withAsk[id] = true
	}
	return withAsk, nil
}

// askBlockAsks is the ask each ask block of tree, the document as a write of it read it, has:
// rows, each ask of document that indexes a block (loadAskBlocks), and sources, the ask on another
// document of its owner that each readable block no row indexes is a copy of
// (readCopiedAskSources). The rows are read as the caller's transaction, which holds the document
// owner's lock, reads them, and the owner only where a block no row indexes is left to read the
// copies of. It opens no ask from what it reads, so it takes no copy lock.
//
// kept for one release: stored-ask-state
func askBlockAsks(ctx context.Context, tx pgx.Tx, document string, tree *pmdoc.Node) (map[string]model.Ask, map[string]copiedSource, error) {
	rows, err := loadAskBlocks(ctx, tx, documentAsksOf(document))
	if err != nil {
		return nil, nil, err
	}
	blocks := readableAskBlocks(tree)
	unindexed := unindexedAskBlockIDs(blocks, rows)
	if len(unindexed) == 0 {
		return rows, nil, nil
	}
	owner, err := documentOwner(ctx, tx, document)
	if err != nil {
		return nil, nil, err
	}
	sources, err := readCopiedAskSources(ctx, tx, document, owner, blocks, unindexed)
	if err != nil {
		return nil, nil, err
	}
	return rows, sources, nil
}

// askReadableWrite is settlement's write to an ask block it reads (readAskBlockStrictly), whose
// ask, or the ask it is a copy of on the document whose address is copied, is ask: the block
// parses, so it carries no `invalid` and no old reason shows on it (G1); and of its other server
// attributes it keeps only those a version keeps, in step with ask (keptAskState).
func askReadableWrite(ask model.Ask, copied string) func(*pmdoc.Node) bool {
	// kept for one release: stored-ask-state
	keep := keptAskState(ask, copied)
	return func(node *pmdoc.Node) bool {
		changed := clearAskInvalidAttribute(node)
		return keep(node) || changed
	}
}
