package pmdoc

//ci:reads packages/envoy/internal/dispatch/pmdoc/testdata/ask-block-scanner.json `packages/envoy/internal/dispatch/pmdoc/question_blocks_test.go#TestQuestionBlocksReadTheAskBlocksPmdocReads` reads it at testdata/ask-block-scanner.json: the corpus of the two block scanners, which `packages/contracts/src/dispatch-block-ref.test.ts` reads as well

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// askBlockScannerCase is one row of testdata/ask-block-scanner.json, the one corpus of the two
// hand-written block scanners: this package's QuestionBlocks and packages/contracts'
// readAskBlocks (dispatch-block-reader.ts), whose test reads the same file. A row is a document
// (Lines, joined by line feeds), the ids of the ask blocks that open in it (IDs), and the rule it
// holds (Why). Pmdoc says whether Parse holds the row too: the rows it parses without refusing the
// document (a block with a body and a closing fence) are held to the ask blocks it reads, and the
// others, openers with no body that a scanner reads and the parser refuses in a write, to the
// scanners alone.
type askBlockScannerCase struct {
	Name  string   `json:"name"`
	Why   string   `json:"why"`
	Pmdoc bool     `json:"pmdoc"`
	Lines []string `json:"lines"`
	IDs   []string `json:"ids"`
}

// QuestionBlocks reads an ask block only where the parser opens one, and each row of the shared
// corpus holds its ids to Parse too where the row says the parser reads it. A backslash in a
// footnote's label escapes the `]` after it, as the renderer writes a label that holds one
// (escapeFootnoteLabel), so a block or a fence written on that label's line opens there, and the
// definition's later blocks stand four columns in, as the renderer writes them. A label of a form
// feed is a label: goldmark's white space is the space, the tab and the line endings alone. A line
// four columns or more past its containers opens no block: it is indented code. A later line of a
// paragraph there, an inline code span's among them, can open no block either, but a write
// refuses such a line, so no version holds one. After a front-matter opener that no line closes,
// no typed block opens at the document's level (frontmatterAttempt).
func TestQuestionBlocksReadTheAskBlocksPmdocReads(t *testing.T) {
	raw, err := os.ReadFile("testdata/ask-block-scanner.json")
	if err != nil {
		t.Fatalf("read the shared corpus: %v", err)
	}
	var corpus []askBlockScannerCase
	if err := json.Unmarshal(raw, &corpus); err != nil || len(corpus) == 0 {
		t.Fatalf("parse the shared corpus: %v (%d rows)", err, len(corpus))
	}
	for _, test := range corpus {
		t.Run(test.Name, func(t *testing.T) {
			markdown := strings.Join(test.Lines, "\n")
			if test.Pmdoc {
				tree, err := Parse(markdown)
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
				if !slices.Equal(parsed, test.IDs) {
					t.Fatalf("pmdoc reads the ask blocks %q, want %q", parsed, test.IDs)
				}
			}
			var scanned []string
			for _, block := range QuestionBlocks(markdown) {
				scanned = append(scanned, block.ID)
			}
			if !slices.Equal(scanned, test.IDs) {
				t.Errorf("QuestionBlocks reads the ask blocks %q, want %q (%s)", scanned, test.IDs, test.Why)
			}
		})
	}
}
