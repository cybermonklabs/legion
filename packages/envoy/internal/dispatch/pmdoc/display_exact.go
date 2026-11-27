package pmdoc

import "github.com/sjawhar/envoy/internal/dispatch/bytelru"

// DisplayExact reports whether Parse takes markdown, the text whose SHA-256 is sum (TextSum),
// whole, with no fallback of a block or of the whole text, so a writer's editor may take that
// markdown. It says nothing more of the display tree: in DisplayLatest the tree still resets the
// attributes the server owns (ReadForDisplay), so an answered ask in it draws open, and a tree is
// a thing to draw, never a text to write back. ReadForDisplay never refuses, and where Parse
// would, the tree it draws is not the text: a block Parse refuses is one paragraph of its literal
// characters, a block of HTML a paragraph holding it as inline HTML, a directive the schema
// refuses its fence lines, and a document past the element limit, nested past maxInlineNesting
// or one whose read panicked is literal text throughout (literalDocument). An editor filled from
// such a tree and written back writes that reading over the text: `# Why` comes back `\# Why`. A
// caller that lets a reader edit a stored text reads this beside its tree, and fills a writer's
// editor from the markdown only where it is true.
//
// The text is read as ReadForDisplay reads it, with line feeds alone (LineFeeds), and no id is
// minted, since the verdict is all this keeps. The verdict is kept by sum (displayExactCache),
// whichever way the text's tree reached the answer - read here, held in the display cache, or
// copied from the stored trees - so a text whose verdict this process holds is not read again,
// and a list read again parses nothing. Two callers asking at once about a text not yet held each
// read it; a caller that may ask so - api's display tree reader, whose workers and requests run
// at once - joins its asks of one text into one (judgedDisplayExact). An empty text has no tree
// to judge, and answers true.
func DisplayExact(sum [32]byte, markdown string) bool {
	if markdown == "" {
		return true
	}
	if exact, held := HeldDisplayExact(sum); held {
		return exact
	}
	verdict := displayExactNo
	if _, err := parseUnstamped(LineFeeds(markdown), fromStart, true, NewWriteBudget()); err == nil {
		verdict = displayExactYes
	}
	displayExactCache.PutSize(sum, verdict, displayExactEntryBytes)
	return verdict[0] == displayExactYes[0]
}

// HeldDisplayExact is the verdict of DisplayExact this process already holds of the text whose
// SHA-256 is sum, and whether it holds one at all, read without the text and without a parse. A
// caller asks this before it spends a worker on a text, as it asks HeldDisplayTree before it
// reads a tree, and it marks the verdict used, as DisplayExact's own lookup does.
func HeldDisplayExact(sum [32]byte) (exact, held bool) {
	verdict, held := displayExactCache.Get(sum)
	return held && verdict[0] == displayExactYes[0], held
}

// displayExactYes and displayExactNo are the two verdicts the cache keeps, each one shared byte:
// a cache value is never written to.
var (
	displayExactYes = []byte{1}
	displayExactNo  = []byte{0}
)

// displayExactEntryBytes is what one verdict is charged: its sum, its byte, and the map entry and
// list element that hold it, rounded up.
const displayExactEntryBytes = 128

// displayExactCacheBytes bounds the verdicts DisplayExact keeps: 8,192 texts at
// displayExactEntryBytes each, far more than the texts a dashboard asks it about - a priority's
// description - and nothing beside the display trees themselves (DisplayCacheBytes).
const displayExactCacheBytes = 1 << 20

// displayExactCache is the verdicts of DisplayExact by the sum of the text judged. A verdict is
// a property of the text its key hashes, so none goes stale, and one leaves only to make room
// for another.
var displayExactCache = bytelru.New[[32]byte, []byte](displayExactCacheBytes)
