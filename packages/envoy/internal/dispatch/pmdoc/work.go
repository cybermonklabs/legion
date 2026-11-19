package pmdoc

import "sync/atomic"

// parseWork, renderWork and updateWork count what ParseWork, RenderWork and UpdateWork answer.
var parseWork, renderWork, updateWork atomic.Uint64

// ParseWork is how many steps this process's reads of markdown have taken since it started. A
// step is what a read does once for each piece of its input when its cost is linear, and again
// for each piece it reads again when it is not:
//
//   - each line goldmark's block parse advances past (lastLineColumns), the blank lines it skips
//     included, and each byte of each line an inline raw HTML walk reads looking for its closer
//     (inlineRawHTMLParser), the writer's scan's walks among them (rawHTMLScan);
//   - each container the nesting bound walks up over as a block opens (opensPastBound);
//   - each character of a delimiter run the inline parse scans (emphasisParser, tildeParser), and
//     each run its pairing visits (delimiterList);
//   - each character a bare address's readers read (readLiteralAddress, literalEmail,
//     literalDomains, literalURL's path, and the transform's nextURLPattern, urlDomain, emailRun
//     and findURLs);
//   - each node unescapeTablePipes walks, each event the reading of list spreads steps over
//     (readSpreads), and each footnote definition keyed for, and each reference looked up in, the
//     map that resolves a reference up to case (footnoteDefinitionsByKey).
//
// A test reads it before and after a call and holds the difference to a count linear in the
// call's input, which no load on the machine moves: the parse costs that once grew with the
// square of a body's length (linear_test.go, heavy_bodies_test.go) grew in these steps.
func ParseWork() uint64 { return parseWork.Load() }

// RenderWork is how many steps this process's renders have taken since it started, beside the
// reads of what they wrote, which ParseWork counts: each character a forward scan of a text reads
// to find its next closer or the bounds of its delimiter run (forwardScan), each character of a
// footnote label folded to ask whether a reference to it is defined (footnoteLabelSet.refersTo),
// each line the writer's raw HTML scan takes into its block (rawHTMLScan), and each inline node
// and each line of a text the writing of line breaks walks (writtenLineBreaks).
func RenderWork() uint64 { return renderWork.Load() }

// UpdateWork is how many fragment items this process's writes of a tree into a document have
// reached since it started: for each fragment operation Update makes, the items ygo walks over to
// the index it is given and the items it deletes or inserts there, and the items each list of a
// fragment's children holds. ygo walks a fragment from its start to each index it is given, so a
// write that inserted a fragment's children one index at a time, or walked the fragment again for
// each replaced child, reached a number of items that grew with the square of the children
// (linear_test.go).
func UpdateWork() uint64 { return updateWork.Load() }
