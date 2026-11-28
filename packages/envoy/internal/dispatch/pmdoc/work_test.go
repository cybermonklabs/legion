package pmdoc

import (
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// TestEachWorkCounterCountsWhatItsOperationReaches runs serially so no other test writes to
// the process counters between the two reads for each operation.
func TestEachWorkCounterCountsWhatItsOperationReaches(t *testing.T) {
	document := func(children ...*Node) *Node {
		return &Node{Type: "doc", Children: children}
	}
	paragraph := func(value string) *Node {
		if value == "" {
			return &Node{Type: "paragraph"}
		}
		return &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: value}}}
	}
	parse := func(markdown string, read func() uint64) func(*testing.T) uint64 {
		return func(t *testing.T) uint64 {
			before := read()
			if _, err := ParseRendering(markdown); err != nil {
				t.Fatal(err)
			}
			return read() - before
		}
	}
	update := func(initial, want *Node) func(*testing.T) uint64 {
		return func(t *testing.T) uint64 {
			doc := crdt.New()
			frag := doc.GetXmlFragment("prosemirror")
			write := func(want *Node) {
				var err error
				doc.Transact(func(txn *crdt.Transaction) { err = Update(txn, frag, want) })
				if err != nil {
					t.Fatal(err)
				}
			}
			write(initial)
			before := UpdateWork()
			write(want)
			return UpdateWork() - before
		}
	}

	a, b, c, d, e := paragraph("one"), paragraph("two"), paragraph("three"), paragraph("four"), paragraph("five")
	heading := &Node{Type: "heading", Attrs: Attrs{"level": 1}, Children: b.Children}
	longA, longB, longC := paragraph(strings.Repeat("a", 32)), paragraph(strings.Repeat("b", 32)), paragraph(strings.Repeat("c", 32))
	edited := paragraph(strings.Repeat("b", 16) + "x" + strings.Repeat("b", 15))

	for _, row := range []struct {
		name    string
		run     func(*testing.T) uint64
		want    uint64
		atLeast bool
	}{
		{
			// The declaration walk must reach all 64 bytes after its opener to give up.
			name: "P1 unclosed declaration", run: parse("p <!a"+strings.Repeat("x", 64), ParseWork), want: 64, atLeast: true,
		},
		{
			name: "P2 no tab", run: parse(strings.Repeat("x", 200), ScanWork), want: 200,
		},
		{
			name: "P2 first tab", run: parse(strings.Repeat("x", 10)+"\t"+strings.Repeat("x", 189), ScanWork), want: 11,
		},
		{
			// The closing-bracket search must read all 31 bytes after the first opener.
			name: "R1 unclosed brackets", want: 31, atLeast: true,
			run: func(t *testing.T) uint64 {
				doc := document(paragraph(strings.Repeat("[a", 16)))
				before := RenderWork()
				if _, err := Render(doc); err != nil {
					t.Fatal(err)
				}
				return RenderWork() - before
			},
		},
		{
			// The live list has 2 children. The two equal paragraphs list 1 text each.
			// The inserts build 3 paragraphs and 3 texts, and walk to index 2 (each text goes at 0).
			name: "U1 append", run: update(document(a, b), document(a, b, c, d, e)), want: 2 + (1 + 1) + (3 + 3) + 2,
		},
		{
			// The live list has 3 children. The kept ends list 1 text each. Deleting the middle
			// run walks to index 1 and deletes 1 child. The inserts build 1 heading and 1 text,
			// and walk to index 1 for the heading (its text goes at 0).
			name: "U2 replace", run: update(document(a, b, c), document(a, heading, c)), want: 3 + (1 + 1) + (1 + 1) + (1 + 1) + 1,
		},
		{
			// The live lists have 1 paragraph and 1 text. Neither end of the outer equality
			// check matches, listing the one text twice. Both ends are still paragraphs,
			// so the two equality factors list 1 text each. Emptying the text deletes 32 units.
			name: "U3 empty text", run: update(document(longA), document(paragraph(""))), want: (1 + 1) + (1 + 1) + (1 + 1) + 32,
		},
		{
			// The live list has 5 children. The kept prefix lists 1 text per paragraph.
			// Deleting the tail walks to index 2 and deletes 3 children.
			name: "U4 delete tail", run: update(document(a, b, c, d, e), document(a, b)), want: 5 + (1 + 1) + (2 + 3),
		},
		{
			// The live lists have 3 paragraphs and 1 text. Each outer end lists 1 text from
			// its kept paragraph and 1 from the mismatching middle. Both ends of the remaining
			// run are paragraphs, so the two equality factors list 1 text each. The text delete
			// and insert each walk to index 16 and remove or insert 1 unit there.
			name: "U5 edit text", run: update(document(longA, longB, longC), document(longA, edited, longC)),
			want: (3 + 1) + (1 + 1 + 1 + 1) + (1 + 1) + (16 + 1) + (16 + 1),
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := row.run(t)
			if row.atLeast {
				if got < row.want {
					t.Errorf("work = %d, want at least %d", got, row.want)
				}
			} else if got != row.want {
				t.Errorf("work = %d, want %d", got, row.want)
			}
		})
	}
}
