// This file ports the selftest() cases of ste-lint.py, from danyuchn/asd-ste100-skill, file
// scripts/ste-lint.py at commit 32511c6992ecb5f1971e46a2943f2e6adceedafe, under its MIT licence:
//
// MIT License
//
// Copyright (c) 2026 Dustin Yuchen Teng
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package rules

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

// check is every carried rule's findings over body's prose sentences.
func check(field, body string) []Finding {
	var findings []Finding
	for _, sentence := range prose.ProseSentences(field, body) {
		for _, rule := range All() {
			findings = append(findings, rule.Check(sentence)...)
		}
	}
	return findings
}

// of is the findings of one rule.
func of(findings []Finding, rule string) []Finding {
	var kept []Finding
	for _, f := range findings {
		if f.Rule == rule {
			kept = append(kept, f)
		}
	}
	return kept
}

func ruleIDs(findings []Finding) []string {
	var ids []string
	for _, f := range findings {
		if !slices.Contains(ids, f.Rule) {
			ids = append(ids, f.Rule)
		}
	}
	slices.Sort(ids)
	return ids
}

func texts(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Text)
	}
	return out
}

func TestCarriedAndLeftOutRules(t *testing.T) {
	var carried []string
	for _, rule := range All() {
		carried = append(carried, rule.ID)
	}
	if want := []string{"semicolon", "phrasal-verb", "nominalization", "long-sentence", "dangling-conjunction"}; !reflect.DeepEqual(carried, want) {
		t.Fatalf("All() = %v, want %v", carried, want)
	}
	var left []string
	for _, omission := range LeftOut {
		if omission.Reason == "" {
			t.Errorf("LeftOut %s has no reason", omission.ID)
		}
		if slices.Contains(carried, omission.ID) {
			t.Errorf("LeftOut %s is also carried", omission.ID)
		}
		left = append(left, omission.ID)
	}
	if want := []string{"marketing-adjective", "synonym-rotation", "passive-voice", "present-perfect"}; !reflect.DeepEqual(left, want) {
		t.Fatalf("LeftOut = %v, want %v", left, want)
	}
}

// Upstream's first case: one text breaking each rule once. The carried rules find theirs; the
// marketing adjective, passive and present perfect it also holds are left out and find nothing.
func TestEachCarriedRuleFindsItsBreak(t *testing.T) {
	body := "The panel is removed; spin up the job. Perform an analysis of the seamless log. We have received the report."
	findings := check("body", body)
	if got, want := ruleIDs(findings), []string{"nominalization", "phrasal-verb", "semicolon"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rules found = %v, want %v (findings %+v)", got, want, findings)
	}
	if got := texts(of(findings, "semicolon")); !reflect.DeepEqual(got, []string{"The panel is removed; spin up the job."}) {
		t.Errorf("semicolon texts = %q", got)
	}
	if got := of(findings, "phrasal-verb"); len(got) != 1 || got[0].Rewrite != `Replace "spin up" with one plain verb, for example "start".` {
		t.Errorf("phrasal-verb = %+v", got)
	}
	if got := of(findings, "nominalization"); len(got) != 1 || !strings.HasPrefix(got[0].Rewrite, `Replace "Perform an analysis" with the verb of its noun.`) {
		t.Errorf("nominalization = %+v", got)
	}
}

// Upstream's hedge and compound-tense cases. Hedges are never flagged, and the passive and present
// perfect rules they tested are left out, so none of these sentences gives a finding.
func TestHedgesAndAdvisoryTensesGiveNoFinding(t *testing.T) {
	bodies := []string{
		"The request may have failed. It could be a timeout. The disk might have filled.",
		"The task has run. The job has set the flag. We have begun.",
		"The job may have run.",
		"The task couldn't have run. The task MAY NOT HAVE RUN.",
		"The task has run. We have begun. The flag is set.",
		"The task is gone.",
	}
	for _, modal := range []string{"may", "might", "could", "should", "would", "must"} {
		for _, gap := range []string{" ", "  ", "\t", " not "} {
			bodies = append(bodies, "The task "+modal+gap+"have run.")
		}
	}
	for _, body := range bodies {
		if findings := check("body", body); len(findings) != 0 {
			t.Errorf("check(%q) = %+v, want none", body, findings)
		}
	}
}

// Upstream's synonym-rotation and marketing cases: both rules are left out.
func TestLeftOutJudgementsGiveNoFinding(t *testing.T) {
	for _, body := range []string{
		"Check the config file. Then verify the output. Verify twice.",
		"Check the config. Check the output.",
		"The robust log gives a seamless start.",
	} {
		if findings := check("body", body); len(findings) != 0 {
			t.Errorf("check(%q) = %+v, want none", body, findings)
		}
	}
}

func TestCodeIsNotRead(t *testing.T) {
	for _, body := range []string{
		"```\nx = a; y = b\n```",
		"Set `x = a; y = b` first.",
		"```text\n- code and\n```",
		"~~~\n- code and\n~~~",
		"    - code and",
	} {
		if findings := check("body", body); len(findings) != 0 {
			t.Errorf("check(%q) = %+v, want none", body, findings)
		}
	}
}

func TestFindingCarriesItsField(t *testing.T) {
	findings := check("description", "Close the valve; open the drain.")
	want := []Finding{{
		Field:   "description",
		Rule:    "semicolon",
		Text:    "Close the valve; open the drain.",
		Rewrite: "Write two sentences. Put a full stop where the semicolon is.",
	}}
	if !reflect.DeepEqual(findings, want) {
		t.Fatalf("check = %+v, want %+v", findings, want)
	}
}

func TestPhrasalVerbs(t *testing.T) {
	cases := map[string]string{
		"We are spinning up the server.":  `Replace "spinning up" with one plain verb, for example "start".`,
		"The team spun up two nodes.":     `Replace "spun up" with one plain verb, for example "start".`,
		"Reach out to the owner.":         `Replace "Reach out" with one plain verb, for example "contact".`,
		"She dove into the logs.":         `Replace "dove into" with one plain verb, for example "examine".`,
		"They are diving into the trace.": `Replace "diving into" with one plain verb, for example "examine".`,
		"We kicked off the build.":        `Replace "kicked off" with one plain verb, for example "start".`,
		"Circle back after the test.":     `Replace "Circle back" with one plain verb, for example "return".`,
		"Touch base with the owner.":      `Replace "Touch base" with one plain verb, for example "talk".`,
	}
	for body, want := range cases {
		got := of(check("body", body), "phrasal-verb")
		if len(got) != 1 || got[0].Rewrite != want {
			t.Errorf("check(%q) phrasal-verb = %+v, want one %q", body, got, want)
		}
	}
	got := of(check("body", "Kick off the job, then spin up the node."), "phrasal-verb")
	if len(got) != 2 || !strings.Contains(got[0].Rewrite, `"Kick off"`) || !strings.Contains(got[1].Rewrite, `"spin up"`) {
		t.Errorf("two phrasal verbs = %+v, want both in sentence order", got)
	}
	if got := check("body", "Start the server. Contact the owner."); len(got) != 0 {
		t.Errorf("plain verbs = %+v, want none", got)
	}
}

func TestNominalization(t *testing.T) {
	for _, body := range []string{
		"The team conducted an investigation of the fault.",
		"We carry out the measurement each day.",
		"They performed an assessment of the risk.",
	} {
		if got := of(check("body", body), "nominalization"); len(got) != 1 {
			t.Errorf("check(%q) nominalization = %+v, want one", body, got)
		}
	}
	if got := check("body", "The team investigated the fault."); len(got) != 0 {
		t.Errorf("plain verb = %+v, want none", got)
	}
}

func terms(count int) string {
	words := make([]string, count)
	for index := range words {
		words[index] = fmt.Sprintf("term%d", index+1)
	}
	return strings.Join(words, " ") + "."
}

func TestLongSentence(t *testing.T) {
	got := of(check("body", strings.TrimSpace(strings.Repeat("word ", 30))+"."), "long-sentence")
	if len(got) != 1 || got[0].Rewrite != "Split this sentence into shorter sentences. It has 30 words. The limit is 25." {
		t.Fatalf("30 words = %+v, want one finding", got)
	}
	if got := check("body", terms(25)); len(got) != 0 {
		t.Errorf("25 words = %+v, want none", got)
	}
	// The words are counted in the sentence, not on the line: two short sentences on one line are
	// no finding, and one long sentence wrapped over lines is one.
	if got := check("body", terms(20)+" "+terms(20)); len(got) != 0 {
		t.Errorf("two sentences of 20 words on one line = %+v, want none", got)
	}
	wrapped := strings.Replace(terms(26), " term13 ", "\nterm13 ", 1)
	if got := of(check("body", wrapped), "long-sentence"); len(got) != 1 {
		t.Errorf("one sentence of 26 words over two lines = %+v, want one", got)
	}
	// A code span counts as one word.
	withCode := strings.Replace(terms(26), "term13", "`a b c d e`", 1)
	if got := of(check("body", withCode), "long-sentence"); len(got) != 1 || !strings.Contains(got[0].Rewrite, "26 words") {
		t.Errorf("26 words, one a code span = %+v, want one finding of 26 words", got)
	}
}

// Upstream's table cases: table syntax is layout, not prose, and each cell is read alone.
func TestLongSentenceInTableCells(t *testing.T) {
	for _, table := range []string{
		"| Label | Detail |\n| --- | --- |\n| Clear | " + terms(24) + " |",
		"Label | Detail\n--- | ---\nClear | " + terms(24),
	} {
		if got := check("body", table); len(got) != 0 {
			t.Errorf("check(%q) = %+v, want none", table, got)
		}
	}
	got := of(check("body", "| Label | Detail |\n| --- | --- |\n| Clear | "+terms(26)+" |"), "long-sentence")
	if len(got) != 1 || !strings.Contains(got[0].Rewrite, "It has 26 words.") {
		t.Fatalf("a cell of 26 words = %+v, want one finding of 26 words", got)
	}
}

// Upstream's list cases, read as the parser reads the list: a list item's text ends where the
// item ends, continuation lines and later paragraphs included.
func TestDanglingConjunction(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"every list marker, case and trailing space",
			"- Confirm the target and\n* Record the result OR  \n+ Close the panel\n1. Start the task and\n2) Stop the task OR",
			[]string{"Confirm the target and", "Record the result OR", "Start the task and", "Stop the task OR"}},
		{"a continuation line completes the item", "  - Confirm the target and\n    record the result.", nil},
		{"a continuation line ends the item", "- Confirm the target\n  and", []string{"Confirm the target and"}},
		{"an item in a block quote", "> - Confirm the target and\n> - Record the result or", nil},
		{"code after the item", "- Do this and\n~~~\ncode and\n~~~", []string{"Do this and"}},
		{"one-space marker", " - Start the task and\n   record the result.", nil},
		{"two-space gap", "-  Start the task and\n   record the result.", nil},
		{"three-space marker", "   - Start the task and", []string{"Start the task and"}},
		// Upstream's line regex read `100. ` with a two-space line after it as a closed item; a
		// parser reads that line as a lazy continuation, so the item is closed by a paragraph here.
		{"wide ordered marker", "100. Start the task and\n\nThe disk holds the log.", []string{"Start the task and"}},
		{"punctuation after the conjunction", "- Start the task and.\n- Stop the task or,", nil},
		{"a later paragraph completes the item", "- Start the task and\n\n  record the result.", nil},
		{"parent and nested item", "- Parent item and\n  - Nested item or", []string{"Parent item and", "Nested item or"}},
		{"a paragraph is no list item", "The process may include steps and", nil},
		{"a code span reads as a word", "- Use `and` as a label", nil},
		{"code spans on both sides", "- Combine `left` and `right`", nil},
		{"an earlier sentence of the item", "- Start the task and. Stop it.", nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := of(check("body", test.body), "dangling-conjunction")
			if !reflect.DeepEqual(texts(got), test.want) {
				t.Fatalf("check(%q) = %+v, want texts %q", test.body, got, test.want)
			}
			for _, f := range got {
				if !strings.Contains(f.Rewrite, "Complete the item, or join it to the next item.") {
					t.Errorf("rewrite %q", f.Rewrite)
				}
			}
		})
	}
	// Upstream's regex took a tab after the marker for no list item. CommonMark opens one there,
	// as the Dispatch reader does, so the item is read and its conjunction found.
	if got := of(check("body", "-\tStart the task and"), "dangling-conjunction"); len(got) != 1 ||
		got[0].Rewrite != `Do not end a list item with "and". Complete the item, or join it to the next item.` {
		t.Errorf("tab after the marker = %+v, want one finding", got)
	}
}
