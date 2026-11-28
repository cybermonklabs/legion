package pmdoc

import (
	"slices"
	"testing"
)

// QuestionBlocks reads an ask block only where the parser opens one, and each case holds its want
// to Parse of its markdown too. A backslash in a footnote's label escapes the `]` after it, as the
// renderer writes a label that holds one (escapeFootnoteLabel), so a block or a
// fence written on that label's line opens there, and the definition's later blocks stand four
// columns in, as the renderer writes them. A label of a form feed is a label: goldmark's white
// space is the space, the tab and the line endings alone. A line four columns or more past its
// containers opens no block: it is indented code. A later line of a paragraph there, an inline
// code span's among them, can open no block either, but a write refuses such a line, so no
// version holds one. After a front-matter opener that no line closes, no typed block opens at the
// document's level (frontmatterAttempt).
func TestQuestionBlocksReadTheAskBlocksPmdocReads(t *testing.T) {
	for _, test := range []struct {
		name, markdown string
		want           []string
	}{
		{
			name:     "a block opened on the line of a footnote label that holds an escaped bracket",
			markdown: "See.[^a\\]b]\n\n[^a\\]b]: :::ask{#noted}\n    Which?\n    :::\n",
			want:     []string{"noted"},
		},
		{
			name:     "a fence opened on the line of a footnote label that holds an escaped bracket",
			markdown: "See.[^a\\]b]\n\n[^a\\]b]: ```\n    :::ask{#fenced}\n    ```\n\n:::ask{#after}\nWhich?\n:::\n",
			want:     []string{"after"},
		},
		{
			name:     "a later block of a footnote four columns in, after a fence the label's line opened",
			markdown: "See.[^a\\]b]\n\n[^a\\]b]: ```\n    code\n    ```\n\n    :::ask{#later}\n    Which?\n    :::\n",
			want:     []string{"later"},
		},
		{
			name:     "a later block of a footnote four columns in",
			markdown: "See.[^n]\n\n[^n]: Note.\n\n    :::ask{#plain}\n    Which?\n    :::\n",
			want:     []string{"plain"},
		},
		{
			name:     "a block opened on the line of a footnote label that holds only a form feed",
			markdown: "See.[^\f]\n\n[^\f]: :::ask{#x}\n    Which?\n    :::\n",
			want:     []string{"x"},
		},
		{
			name:     "a line of indented code",
			markdown: "Intro.\n\n    :::ask{#indented}\n    Which?\n    :::\n",
		},
		{
			name:     "a line four columns past a quote's marker or a list item's",
			markdown: ">     :::ask{#quoted}\n\n-     :::ask{#listed}\n",
		},
		{
			name:     "a line after a front-matter opener that no line closes",
			markdown: "---\n:::ask{#unclosed}\nWhich?\n:::\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := Parse(test.markdown)
			if err != nil {
				t.Fatalf("pmdoc refuses the markdown: %v", err)
			}
			var parsed []string
			Walk(tree, func(node *Node) bool {
				if id, _ := node.Attrs[BlockIDAttr].(string); node.Type == "ask" {
					parsed = append(parsed, id)
				}
				return true
			})
			if !slices.Equal(parsed, test.want) {
				t.Fatalf("pmdoc reads the ask blocks %q, want %q", parsed, test.want)
			}
			var scanned []string
			for _, block := range QuestionBlocks(test.markdown) {
				scanned = append(scanned, block.ID)
			}
			if !slices.Equal(scanned, test.want) {
				t.Errorf("QuestionBlocks reads the ask blocks %q, want %q as pmdoc reads them", scanned, test.want)
			}
		})
	}
}
