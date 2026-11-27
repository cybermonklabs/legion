package refs

import (
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The heads of stored text that Dispatch sends a reader to draw with its references: a first
// line, an item's excerpt and a block's excerpt. Each is cut where no reference crosses the cut
// (text.HeadRefs), so a mention, an address, a key or a `#id` is never half of itself where it is
// drawn: `CORE-12` of `CORE-1234` would be a working link to another issue. heads_test.go holds
// every other cut of Dispatch text to the reason it is none of these.

// firstLineRunes bounds a first line, its ellipsis included: the bound contracts.OneLineSummary
// cuts a line to.
const firstLineRunes = 160

// excerptRunes bounds the head of an item's text shown beside an edge.
const excerptRunes = 240

// BlockExcerptRunes bounds the containing block a document mention's excerpt shows.
const BlockExcerptRunes = 480

// FirstLine is the line a person reads to tell one text from another: its first line that holds
// more than whitespace (contracts.FirstNonEmptyLine), cut to firstLineRunes runes with `…` as
// contracts.OneLineSummary cuts it, but where no reference crosses the cut; serverURL is the
// deployment whose dashboard URLs are references (text.HeadRefs). It is an Inbox row's subject
// and its reply's first line, and an id prefix candidate's first line.
func FirstLine(body, serverURL string) string {
	return FirstLineWithin(body, serverURL, firstLineRunes)
}

// FirstLineWithin is FirstLine cut to runes runes in place of firstLineRunes, for a surface whose
// own design gives its first line more room: a person's page shows 240 (api/person_nodes.go).
func FirstLineWithin(body, serverURL string, runes int) string {
	return text.HeadRefs(contracts.FirstNonEmptyLine(body), runes, "…", serverURL)
}

// itemExcerpt is the head of an item's text shown beside an edge (Edges): its first excerptRunes
// runes, cut where no reference crosses the cut.
func itemExcerpt(body, serverURL string) string {
	return text.HeadRefs(body, excerptRunes, "", serverURL)
}

// BlockExcerpt is a block's markdown as a mention it holds shows it and as the block's row stores
// it: its first BlockExcerptRunes runes, cut where no reference crosses the cut.
func BlockExcerpt(markdown, serverURL string) string {
	return text.HeadRefs(markdown, BlockExcerptRunes, "", serverURL)
}
