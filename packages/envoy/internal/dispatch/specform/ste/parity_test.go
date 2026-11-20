package ste

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// corpusLines reads testdata/corpus.jsonl: one JSON string per line.
func corpusLines(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/corpus.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(string(raw), "\n")
	if rows[len(rows)-1] == "" {
		rows = rows[:len(rows)-1]
	}
	texts := make([]string, len(rows))
	for index, row := range rows {
		if err := json.Unmarshal([]byte(row), &texts[index]); err != nil {
			t.Fatalf("corpus line %d: %v", index+1, err)
		}
	}
	if len(texts) == 0 {
		t.Fatal("the corpus holds no line, so it compares nothing")
	}
	return texts
}

// multiset is findings as sorted "rule\x00match" keys, so two lists with the same findings in
// another order compare equal.
func multiset(findings []Finding) []string {
	keys := make([]string, len(findings))
	for index, finding := range findings {
		keys[index] = finding.Rule + "\x00" + finding.Match
	}
	slices.Sort(keys)
	return keys
}

// TestPortMatchesSteLintOnTheCorpus runs the pinned Python program (testdata/ste-lint.py, through
// testdata/run_corpus.py) over every corpus line and holds the port's hard findings to its, with
// no tolerance: the same findings, and in the same order. Without python3 it fails, since a
// skipped comparison would read as a passed one.
func TestPortMatchesSteLintOnTheCorpus(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is not on PATH, so the port cannot be compared with ste-lint: %v", err)
	}
	texts := corpusLines(t)
	command := exec.Command(python, "testdata/run_corpus.py", "testdata/corpus.jsonl")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		t.Fatalf("run_corpus.py: %v\n%s", err, stderr.String())
	}
	answers := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(answers) != len(texts) {
		t.Fatalf("run_corpus.py answered %d lines for %d corpus lines", len(answers), len(texts))
	}
	var differ []string
	for index, text := range texts {
		var rows []struct{ Rule, Match string }
		if err := json.Unmarshal([]byte(answers[index]), &rows); err != nil {
			t.Fatalf("run_corpus.py line %d: %v", index+1, err)
		}
		want := make([]Finding, len(rows))
		for row, finding := range rows {
			want[row] = Finding{Rule: finding.Rule, Match: finding.Match}
		}
		got := Lint(text)
		switch {
		case !slices.Equal(multiset(got), multiset(want)):
			differ = append(differ, fmt.Sprintf("corpus line %d %q: ste-lint %v, port %v", index+1, text, want, got))
		case !sameFindings(got, want):
			differ = append(differ, fmt.Sprintf("corpus line %d %q: ste-lint's order %v, port's %v", index+1, text, want, got))
		}
	}
	if len(differ) > 0 {
		t.Fatalf("%d of %d corpus lines differ; the first: %s", len(differ), len(texts), differ[0])
	}
}

// TestCorpusFiresEveryHardRule holds the corpus to lines that can fail the comparison: each of
// the seven hard rules fires on at least one line, and at least one line has no finding.
func TestCorpusFiresEveryHardRule(t *testing.T) {
	fired := map[string]int{}
	clean := 0
	for _, text := range corpusLines(t) {
		found := Lint(text)
		if len(found) == 0 {
			clean++
		}
		for _, finding := range found {
			fired[finding.Rule]++
		}
	}
	for _, rule := range []string{"semicolon", "phrasal-verb", "marketing-adjective", "nominalization", "long-sentence", "synonym-rotation", "dangling-conjunction"} {
		if fired[rule] == 0 {
			t.Errorf("no corpus line fires %s, so the comparison proves nothing about it", rule)
		}
	}
	if clean == 0 {
		t.Error("every corpus line has a finding, so no line shows a text that passes")
	}
	if len(fired) != 7 {
		t.Errorf("the corpus fires %d rules (%v), want the seven hard rules", len(fired), fired)
	}
}
