// Package dispatchbody answers whether Dispatch would refuse a body, for a caller outside this
// module. A caller that must know before it writes anything — one whose post is recorded first
// and sent later, where a refusal costs the text rather than the call — asks here and refuses
// its own caller with Dispatch's own answer, which that caller can act on: by sending a shorter
// text, or by taking out the ask block the text holds.
//
// The answer is the server's own. Check splits the body with the splitter behind every Dispatch
// route that takes one (internal/dispatch/itemblocks) and returns its refusal unchanged, so a
// caller refuses exactly what Dispatch refuses rather than approximating it with a reader of its
// own. NulInJSON is the walk Dispatch's input layer reads every JSON request body with for a
// U+0000 (internal/dispatch/text), so a caller that stores a body's strings in PostgreSQL finds
// the string Dispatch would refuse, by the same reading. That splitter and that walk are internal
// packages, unreachable from another module; this package is the one public door onto them, which
// is why it exists at all.
package dispatchbody

import (
	"errors"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// NulInJSON is the first string of the JSON value data that holds U+0000 - a member name or a
// value, in the order data writes them, every copy of a repeated member among them - and its
// place in data: planReason, tasks[0].text, a member's name with each U+0000 written as the six
// characters \u0000, or "body" for a bare string. found is false when no string holds one, and
// when data stops being JSON before one. The caller refuses with its own wording, saying where in
// value the U+0000 stands in its own count, and never sends value back.
func NulInJSON(data []byte) (place, value string, found bool) {
	return text.NulInJSON("", data)
}

// Check answers the first rule Dispatch holds body to that it breaks, or nil when it breaks
// none: an *itemblocks.CountExceeded for a body of more blocks than ItemBlocks, else an
// *itemblocks.CapExceeded naming the first block over BlockUnits, else an
// *itemblocks.DecisionBlock naming the first block that holds an ask block, which
// IsDecisionBlock reads the answer apart by. The refusal is the server's, wording and all, over
// the field name "body", which is what the server's own body routes pass, so a caller's refusal
// of a text reads exactly as Dispatch's refusal of it would.
func Check(body string) error {
	return itemblocks.Check("body", body)
}

// IsDecisionBlock says whether err, an error Check answered, is the refusal of a body for the
// ask block it holds — an `:::ask` block, which the reader draws a decision card for and
// nobody can answer where it stands — rather than the refusal of one for a bound it passes. A
// caller outside this module cannot name that refusal's type, since it belongs to an internal
// package (*itemblocks.DecisionBlock), so it asks here instead; the answer is errors.As over
// Check's own error, so a refusal another error wraps is still read for what it is. A nil error
// is no refusal at all, and reads false.
func IsDecisionBlock(err error) bool {
	var decision *itemblocks.DecisionBlock
	return errors.As(err, &decision)
}

// BlockUnits is the longest one block of a body may be, in UTF-16 code units as Dispatch counts
// them, and ItemBlocks the most blocks one body may hold. They are here for a caller that must
// build a text at the bound rather than ask about one it already holds. Nothing here checks a
// body against them: the server enforces both itself on every route that takes a body, and Check
// reads them through the server's own splitter.
const (
	BlockUnits = contracts.DispatchBlockUnits
	ItemBlocks = contracts.DispatchItemBlocks
)
