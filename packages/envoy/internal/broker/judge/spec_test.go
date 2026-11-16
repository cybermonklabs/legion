// packages/envoy/internal/broker/judge/spec_test.go
// The extract of a spec document the model is shown: which sections it takes, where each of them
// ends, what it does with a document that has neither heading, and the bound on the whole of it.
package judge

import (
	"strings"
	"testing"
)

func TestSpecExtractTakesTheSummaryAndTheRequirements(t *testing.T) {
	for _, c := range []struct {
		name     string
		markdown string
		want     string
	}{
		{
			name: "both sections, each to the next heading of level one or two",
			markdown: "# Title\n\n## Summary\n\nIt judges.\n\n## Problem\n\nPeople wait.\n\n" +
				"## Requirements\n\n**R1.** It never denies.\n\n## Verification\n\nElsewhere.\n",
			want: "## Summary\n\nIt judges.\n\n## Requirements\n\n**R1.** It never denies.",
		},
		{
			name:     "a deeper heading inside a section is more of that section",
			markdown: "## Summary\n\nIt judges.\n\n### How\n\nOnce.\n\n## Design\n\nLater.\n",
			want:     "## Summary\n\nIt judges.\n\n### How\n\nOnce.",
		},
		{
			name:     "a heading inside a fenced block is text",
			markdown: "## Requirements\n\n```md\n## Summary\n\nnot a heading\n```\n\nDone.\n\n# End\n",
			want:     "## Requirements\n\n```md\n## Summary\n\nnot a heading\n```\n\nDone.",
		},
		{
			name:     "the summary alone",
			markdown: "## Summary\n\nIt judges.\n\n## Problem\n\nPeople wait.\n",
			want:     "## Summary\n\nIt judges.",
		},
		{
			name:     "the requirements alone",
			markdown: "## Goals\n\nSome.\n\n## Requirements\n\n**R1.** One.\n",
			want:     "## Requirements\n\n**R1.** One.",
		},
		{
			name:     "the first of a repeated heading",
			markdown: "## Summary\n\nFirst.\n\n## Other\n\nx\n\n## Summary\n\nSecond.\n",
			want:     "## Summary\n\nFirst.",
		},
		{
			name:     "a closed heading, and one in another case",
			markdown: "## SUMMARY ##\n\nIt judges.\n\n## requirements\n\n**R1.** One.\n",
			want:     "## SUMMARY ##\n\nIt judges.\n\n## requirements\n\n**R1.** One.",
		},
		{
			name:     "neither heading is the head of the document",
			markdown: "# Title\n\nSome prose and no sections at all.\n",
			want:     "# Title\n\nSome prose and no sections at all.\n",
		},
		{
			name:     "a level-one Summary is not the section",
			markdown: "# Summary\n\nIt judges.\n",
			want:     "# Summary\n\nIt judges.\n",
		},
		{
			name:     "an empty document",
			markdown: "",
			want:     "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := specExtract(c.markdown); got != c.want {
				t.Errorf("specExtract = %q\nwant %q", got, c.want)
			}
		})
	}
}

// TestSpecExtractIsCutToItsBound: a spec larger than the bound is cut to it, whichever path it
// came down, so what reaches the model is bounded whatever an agent wrote into the document.
func TestSpecExtractIsCutToItsBound(t *testing.T) {
	long := strings.Repeat("é ", specRunes)
	for _, c := range []struct{ name, markdown string }{
		{"the sections", "## Summary\n\n" + long + "\n\n## Requirements\n\n" + long + "\n"},
		{"a document with neither heading", long},
	} {
		t.Run(c.name, func(t *testing.T) {
			if runes := len([]rune(specExtract(c.markdown))); runes != specRunes {
				t.Errorf("the extract is %d runes; want it cut to %d", runes, specRunes)
			}
		})
	}
}
