package docs

import (
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// errJoinsTwoTables is a reject whose insert runs from a cell of one table into a cell of
// another: the browser editor's reject joins the two into one table, which Splice does not, so
// the reject is refused rather than stored otherwise than the browser would store it.
var errJoinsTwoTables = errors.New("the insert runs from one table into the next, and removing it would join the two tables")

// rejectedInsert is tree with the text of the insert suggestion id deleted span by span
// (pmdoc.MarkSpans), last first so the earlier spans keep their positions, each with nothing
// spliced over it. Runs that meet across a block boundary are one span, as the browser editor's
// reject deletes them, so the blocks join: that undoes the split an insert made, and a table the
// span cuts is padded to its width afterwards (pmdoc.PadTables), as the browser's table plugin pads
// it. Text without the mark between two runs ends a span, so it is kept, where the browser's reject
// deletes it with them. A removal the document cannot hold, one the schema refuses or the renderer
// cannot write, is refused (rejectSpliceRefusal).
func rejectedInsert(tree *pmdoc.Node, id string) (*pmdoc.Node, error) {
	budget := pmdoc.NewWriteBudget()
	spans := pmdoc.MarkSpans(tree, string(MarkSuggestion), id)
	nothing := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{Type: "paragraph"}}}
	next := tree
	for index := len(spans) - 1; index >= 0; index-- {
		span := spans[index]
		if joinsTwo(next, span, "table") {
			return nil, rejectSpliceRefusal(errJoinsTwoTables)
		}
		valid, err := pmdoc.ValidateDocument(next, "Splice")
		if err != nil {
			return nil, rejectSpliceRefusal(err)
		}
		spliced, err := valid.Splice(span, nothing)
		if err != nil {
			return nil, rejectSpliceRefusal(err)
		}
		found, err := matchedBlocks(valid, span)
		if err != nil {
			return nil, rejectSpliceRefusal(err)
		}
		if next, err = padCutTables(spliced, found, budget); err != nil {
			return nil, rejectSpliceRefusal(err)
		}
		if err := next.Validate(); err != nil {
			return nil, rejectSpliceRefusal(err)
		}
	}
	if _, err := pmdoc.Render(next); err != nil {
		return nil, rejectSpliceRefusal(err)
	}
	return next, nil
}

// AnchorMark is one anchor mark of a document as an ask's or a comment's anchor names it: the
// mark's type (MarkAsk, MarkComment, MarkSuggestion) and its id.
type AnchorMark struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// TakenAnchorMarks is the anchor marks - an ask's, a comment's, another suggestion's - on the text
// the reject of an insert suggestion took out of the document (rejectedInsert), as
// takenAnchorMarks reads them off the tree before the removal and the tree it left. Removed is
// each mark the removal took out whole: it lay wholly on the inserted words, so every row
// anchored on it quotes those words alone. Cut is each mark that stands after the removal: it
// wrapped document text too, and that text is all it wraps now. A reject that removes no text
// takes no mark, and neither does an accept.
type TakenAnchorMarks struct {
	Removed []AnchorMark `json:"removed,omitempty"`
	Cut     []AnchorMark `json:"cut,omitempty"`
}

// takenAnchorMarks is the anchor marks on the text the reject of insert suggestion id takes out of
// tree, leaving next (rejectedInsert): every anchor mark but the insert's own that a text run
// carrying the insert's mark carries - those runs are the text the removal takes, span by span
// (pmdoc.MarkSpans) - each once, in tree order, Removed where next no longer holds it and Cut
// where it does.
func takenAnchorMarks(tree, next *pmdoc.Node, id string) TakenAnchorMarks {
	own := pmdoc.MarkRef{Type: string(MarkSuggestion), ID: id}
	var marks []pmdoc.MarkRef
	seen := map[pmdoc.MarkRef]bool{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "text" || !pmdoc.NodeHasMarkID(node, own.Type, own.ID) {
			return true
		}
		for _, mark := range node.Marks {
			markID, _ := mark.Attrs["id"].(string)
			ref := pmdoc.MarkRef{Type: mark.Type, ID: markID}
			if markID == "" || anchorKinds[mark.Type] == "" || ref == own || seen[ref] {
				continue
			}
			seen[ref] = true
			marks = append(marks, ref)
		}
		return true
	})
	var taken TakenAnchorMarks
	stands := pmdoc.FindMarks(next, marks)
	for _, ref := range marks {
		if _, cut := stands[ref]; cut {
			taken.Cut = append(taken.Cut, AnchorMark(ref))
		} else {
			taken.Removed = append(taken.Removed, AnchorMark(ref))
		}
	}
	return taken
}

// markSet is marks as a set of mark identities.
func markSet(marks []AnchorMark) map[pmdoc.MarkRef]bool {
	set := make(map[pmdoc.MarkRef]bool, len(marks))
	for _, mark := range marks {
		set[pmdoc.MarkRef(mark)] = true
	}
	return set
}

// joinsTwo reports whether span runs from inside one node of nodeType into another: from one
// table into the next, or, for "table_row", from one body row into another (a header row is a
// table_header_row, so a range from it into the first body row is not one).
func joinsTwo(tree *pmdoc.Node, span pmdoc.Range, nodeType string) bool {
	holder := func(position int) *pmdoc.Node {
		at, _ := pmdoc.ContainingTextblock(tree, position)
		for _, ancestor := range at.Ancestors {
			if ancestor.Type == nodeType {
				return ancestor
			}
		}
		return nil
	}
	from, to := holder(span.From), holder(span.To)
	return from != nil && to != nil && from != to
}

// rejectSpliceRefusal words every refusal of a reject's removal, which leaves the document as it
// was and the suggestion open: the person rejecting cannot change the text, so each names what they
// can do, accept the suggestion or edit the document themselves. An insert that runs into an ask or
// callout from the text before it would join the two and leave the ask or callout empty, which the
// browser editor drops; one that runs from one table into the next would join the tables
// (errJoinsTwoTables); and any other removal leaves a shape the document cannot hold, one the
// schema refuses or the renderer cannot write.
func rejectSpliceRefusal(err error) error {
	const advice = "accept the suggestion, or remove the text by editing the document"
	switch {
	case errors.Is(err, pmdoc.ErrJoinEmptiesTypedBlock):
		return &ErrInvalidOp{Field: "anchor", Reason: "the insert runs into an ask or callout from the text before it, and " +
			"removing it would join the two and leave the ask or callout empty; " + advice}
	case errors.Is(err, errJoinsTwoTables):
		return &ErrInvalidOp{Field: "anchor", Reason: err.Error() + "; " + advice}
	case errors.Is(err, pmdoc.ErrSchema):
		return &ErrInvalidOp{Field: "anchor", Reason: fmt.Sprintf("removing the insert would leave a shape the document "+
			"cannot hold (%v); %s", err, advice)}
	}
	return err
}
