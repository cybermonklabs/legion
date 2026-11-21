package specform

import (
	"bufio"
	"encoding/json"
	"maps"
	"os"
	"testing"
)

// The judge reads the rules that read more than one sentence from what it reads once of each
// sentence (wholeText), and that reading is ste-lint's reading of the whole line (lintedWhole): for
// each line of ste-lint's corpus and of the rows below that ste-lint reads as one line, the
// findings of synonym-rotation and long-sentence that wholeText reads, its verbs and whether it is
// a list item that ends in "and" or "or" are ste-lint's. The rows hold what joins or splits a piece
// of ste-lint across two sentences, and what moves the first place of a verb: a stop before a
// closing quote or bracket, an initialism, code font that holds a stop and a space, a verb of a
// group in code font and in a name, and list items. A line that ste-lint reads as more than one,
// as the corpus's lines with \v or U+2028 are, the judge reads whole (wholeText.lines).
func TestTheWholeTextReadsAsSteLintReadsTheLine(t *testing.T) {
	lines := []string{
		`The guide says "Run it." The job copies the report to deploy@build.example.com: each night after the tests of the merge pass on main.`,
		`The guide says (run it.) Then the job copies each report of the night to the host of the build and keeps a copy of it for a week or two.`,
		`Dispatch keeps it in the U.S. Then the job copies each report of the night to the host of the build and keeps a copy for a week or two at most.`,
		"Dispatch reads `one. two` and Web.Config at boot. It logs each read of the file and each write of the file for the team of the night build today.",
		"Dispatch checks Foo.Validate at boot. It validated the file.",
		"Dispatch checks `Foo.Validate` at boot. It validated the file. It confirms each record and verifies it.",
		"- Dispatch reads Web.Config. It keeps the record and",
		"1. Dispatch keeps the record. It checks it or",
		`He said "Stop." "Go." "Wait." Then each of the twenty words of this long piece runs on and on past every closing quote of the line today.`,
	}
	corpus, err := os.Open("ste/testdata/corpus.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	scanner := bufio.NewScanner(corpus)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var line string
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	read, joined, several := 0, 0, 0
	for _, text := range lines {
		line := oneLine(text)
		whole := newWholeText(line, nil, nil)
		if len(whole.spans) == 0 {
			continue
		}
		if whole.lines {
			several++
			continue
		}
		whole.load()
		read++
		want, verbs := lintedWhole(line, nil)
		dangles := false
		for key := range want {
			if key.rule == "ste100-dangling-conjunction" {
				dangles = true
				delete(want, key)
			}
		}
		if !maps.Equal(whole.before, want) || whole.verbs != verbs || whole.dangles != dangles {
			t.Errorf("%q: wholeText reads %v, verbs %b and dangles %v; ste-lint reads %v, verbs %b and dangles %v",
				line, whole.before, whole.verbs, whole.dangles, want, verbs, dangles)
		}
		for _, spread := range whole.spreads[:len(whole.spreads)-1] {
			if spread.Open {
				joined++
			}
		}
	}
	t.Logf("read %d lines by their sentences, with %d sentences that a piece runs on from, and %d whole", read, joined, several)
	if read < 700 || joined == 0 || several == 0 {
		t.Errorf("read %d lines by their sentences, %d sentences that a piece runs on from and %d lines whole: the test reads too little", read, joined, several)
	}
}
