package docs

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A `[^label]` an edit writes is a footnote reference where the document, or the same batch,
// defines that label: a replace whose with ends in `[^1]` in the batch that inserts `[^1]: ...` at
// the end, an insert of a paragraph referring to a definition the document holds, and a replace
// that does the same. A reference no definition names keeps the text main stores, so no text main
// stores is refused (INFRA-4345 R5, from dispatch://INFRA-1134's two cases on GTM-8's proposal,
// where each read back `\[^1]`).
func TestAFootnoteReferenceAnEditWritesIsAFootnote(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, markdown string
		ops            []model.EditOp
		want           string
	}{
		{
			name:     "a replace and the insert of its definition, in one batch",
			markdown: "Intro.\n\nIt happens.\n",
			ops: []model.EditOp{
				{Op: "replace", Find: "It happens.", With: "It happens.[^1]"},
				{Op: "insert", After: "end", Markdown: "[^1]: Each range is a note."},
			},
			want: "Intro.\n\nIt happens.[^1]\n\n[^1]: Each range is a note.\n",
		},
		{
			name:     "an insert of a paragraph referring to the document's definition",
			markdown: "It happens.[^1]\n\n[^1]: Each range is a note.\n",
			ops:      []model.EditOp{{Op: "insert", After: "It happens.", Markdown: "And again.[^1]"}},
			want:     "It happens.[^1]\n\nAnd again.[^1]\n\n[^1]: Each range is a note.\n",
		},
		{
			name:     "a replace referring to the document's definition",
			markdown: "It happens.[^1]\n\nAnd again.\n\n[^1]: Each range is a note.\n",
			ops:      []model.EditOp{{Op: "replace", Find: "And again.", With: "And again.[^1]"}},
			want:     "It happens.[^1]\n\nAnd again.[^1]\n\n[^1]: Each range is a note.\n",
		},
		{
			name:     "a reference no definition names keeps main's text",
			markdown: "It happens.\n",
			ops:      []model.EditOp{{Op: "replace", Find: "It happens.", With: "It happens.[^9]"}},
			want:     "It happens.[^9]\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			batch, err := applyBatch(newBatchTree(tree), test.ops, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(batch.tree)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("the document reads\n%q\nwant\n%q", markdown, test.want)
			}
		})
	}
}
