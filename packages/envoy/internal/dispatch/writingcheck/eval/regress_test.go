package eval_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/candidates"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

// regressCase is one line of regress-cases.jsonl, the reader's development cases: a body
// written for the test and the verdict each listed sentence of it must get. The writing-check
// command's regress subcommand reads the same shape.
type regressCase struct {
	ID     string `json:"id"`
	Body   string `json:"body"`
	Expect []struct {
		Sentence string `json:"sentence"`
		Vacuous  *bool  `json:"vacuous"`
	} `json:"expect"`
}

// TestRegressCasesAreCandidates checks that every development case parses, and that every
// sentence it expects a verdict for is a candidate of the code stage, so the reader reads it.
func TestRegressCasesAreCandidates(t *testing.T) {
	data, err := os.ReadFile("regress-cases.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	var classes [2]int
	lines := bufio.NewScanner(bytes.NewReader(data))
	lines.Buffer(nil, 1<<20)
	for n := 1; lines.Scan(); n++ {
		dec := json.NewDecoder(bytes.NewReader(lines.Bytes()))
		dec.DisallowUnknownFields()
		var c regressCase
		if err := dec.Decode(&c); err != nil {
			t.Errorf("line %d: %v", n, err)
			continue
		}
		if c.ID == "" || c.Body == "" || len(c.Expect) == 0 {
			t.Errorf("line %d: a case needs an id, a body and at least one expected verdict", n)
			continue
		}
		if ids[c.ID] {
			t.Errorf("line %d: id %s is not unique", n, c.ID)
		}
		ids[c.ID] = true
		found := candidates.Find(prose.ProseSentences("body", c.Body), candidates.Options{})
		for _, e := range c.Expect {
			if e.Vacuous == nil {
				t.Errorf("case %s: %q has no expected verdict", c.ID, e.Sentence)
				continue
			}
			if *e.Vacuous {
				classes[0]++
			} else {
				classes[1]++
			}
			if !slices.ContainsFunc(found, func(k candidates.Candidate) bool { return k.Sentence.Text == e.Sentence }) {
				t.Errorf("case %s: %q is not a candidate of the code stage", c.ID, e.Sentence)
			}
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) < 60 || classes[0] == 0 || classes[1] == 0 {
		t.Errorf("%d cases, %d vacuous and %d not vacuous sentences: want at least 60 cases and both classes", len(ids), classes[0], classes[1])
	}
}
