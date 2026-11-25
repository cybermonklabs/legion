package pmdoc

import (
	"github.com/yuin/goldmark/ast"
	gmtext "github.com/yuin/goldmark/text"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// keepAddressesWhole puts back as the text it showed each literal autolink of run, a run of text
// nodes and the literal autolinks between them (textRuns), whose first character stands inside a
// `dispatch://` address of the run's text (runText, text.DispatchSpans). A person's login is an
// email address, and both of the engine's readers of bare addresses start an email or a URL after
// any punctuation - the tokenizer after a `=`, a `;` or a `_`, the transform after a `.` or a `-` -
// so `dispatch://person/john.doe@example.com` would read as the text `dispatch://person/john.` and
// a link to `mailto:doe@example.com`, and no reader would have the address whole to name the
// person by. Every other autolink stays as it was made, an angle autolink and one standing after an
// address among them. It merges no text into the texts beside it, as the engine merges none. Its
// twin is remarkKeepAddressesWhole in packages/proof-editor/src/whole-addresses.ts, which the
// browser editor's engine runs after remark-gfm.
func keepAddressesWhole(run []ast.Node, source []byte) {
	holds := false
	for _, node := range run {
		_, isLink := node.(*ast.AutoLink)
		holds = holds || isLink
	}
	if !holds {
		return
	}
	value, pieces := runText(run, source, false)
	spans := text.DispatchSpans(value)
	span := 0
	for _, piece := range pieces {
		link, isLink := piece.node.(*ast.AutoLink)
		if !isLink {
			continue
		}
		for span < len(spans) && spans[span][1] <= piece.start {
			span++
		}
		if span == len(spans) {
			return
		}
		if spans[span][0] <= piece.start {
			parent := link.Parent()
			parent.ReplaceChild(parent, link, unlinkedText(link, source))
		}
	}
}

// literalAutolink reports whether link is one of the engine's literal autolinks, which the
// tokenizer or the transform read bare, rather than an angle autolink: every bare autolink's
// position is its first byte (bareAutolink), an angle autolink's its `<`.
func literalAutolink(link *ast.AutoLink, source []byte) bool {
	return source[link.Pos()] != '<'
}

// unlinkedText is the text link showed, over its label's bytes in the source. The transform's
// autolink (decodedLabelAttr) showed its label decoded, and comes back as a text, which is read
// decoded; the tokenizer's showed its source as written, and comes back as a raw text, which is
// read as written (parseTextValue), as it showed linked and as the engine shows it.
func unlinkedText(link *ast.AutoLink, source []byte) *ast.Text {
	segment := gmtext.NewSegment(link.Pos(), link.Pos()+len(link.Label(source)))
	if _, decoded := link.AttributeString(decodedLabelAttr); decoded {
		return ast.NewTextSegment(segment)
	}
	return ast.NewRawTextSegment(segment)
}
