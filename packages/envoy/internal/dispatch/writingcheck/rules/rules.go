// This file ports rules of ste-lint.py, from danyuchn/asd-ste100-skill, file
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

// Package rules holds the ASD-STE100 structure rules a writing check applies to each prose
// sentence: the hard rules of upstream ste-lint.py that are true by reading alone. Each rule reads
// one prose.Sentence, so it sees only prose, never code, quotes or addresses, and counts within
// one sentence, never across a line.
package rules

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

// Finding is one rule a sentence breaks. Field is the field the sentence stands in, Rule the
// rule's ID, Text the sentence and Rewrite a short instruction, in ASD-STE100 English, that says
// how to fix it.
type Finding struct {
	Field   string
	Rule    string
	Text    string
	Rewrite string
}

// Rule is one carried rule: its ID, upstream's name for it, and its check, which answers each
// finding the sentence gives, or none.
type Rule struct {
	ID    string
	Check func(sentence prose.Sentence) []Finding
}

// All is the carried rules, in a fixed order.
func All() []Rule {
	return []Rule{
		{ID: "semicolon", Check: semicolon},
		{ID: "phrasal-verb", Check: phrasalVerb},
		{ID: "nominalization", Check: nominalization},
		{ID: "long-sentence", Check: longSentence},
		{ID: "dangling-conjunction", Check: danglingConjunction},
	}
}

// Omission is an upstream rule this package does not carry, and why.
type Omission struct {
	ID     string
	Reason string
}

// LeftOut is each upstream rule left out, with its reason.
var LeftOut = []Omission{
	{ID: "marketing-adjective", Reason: "It is a judgement about meaning, not a rule true by reading alone: " +
		`"robust" is a term of statistics, and "seamless" can name a real joint.`},
	{ID: "synonym-rotation", Reason: "It is a judgement about meaning, not a rule true by reading alone: " +
		`"check" and "verify" can name two different acts.`},
	{ID: "passive-voice", Reason: "It is advisory upstream, not a hard rule: " +
		"the passive is correct where the actor is unknown or not relevant."},
	{ID: "present-perfect", Reason: "It is advisory upstream, not a hard rule: " +
		"the present perfect is correct where the current result is the point."},
}

// MaxWords is the most words one sentence may hold (upstream MAX_WORDS, its cap for
// descriptions).
const MaxWords = 25

func finding(sentence prose.Sentence, rule, rewrite string) Finding {
	return Finding{Field: sentence.Field, Rule: rule, Text: sentence.Text, Rewrite: rewrite}
}

// semicolon is STE Rule 8.1: no semicolons. A sentence gives one finding however many it holds.
func semicolon(sentence prose.Sentence) []Finding {
	if !strings.Contains(sentence.Text, ";") {
		return nil
	}
	return []Finding{finding(sentence, "semicolon", "Write two sentences. Put a full stop where the semicolon is.")}
}

// phrasalVerbs is upstream's closed list of soft phrasal verbs, one family a line, each with the
// one plain verb its rewrite names.
var phrasalVerbs = []struct {
	pattern *regexp.Regexp
	verb    string
}{
	{regexp.MustCompile(`(?i)\b(?:spin(?:ning|s)? up|spun up)\b`), "start"},
	{regexp.MustCompile(`(?i)\breach(?:ing|es|ed)? out\b`), "contact"},
	{regexp.MustCompile(`(?i)\b(?:div(?:e|es|ing|ed) into|dove into)\b`), "examine"},
	{regexp.MustCompile(`(?i)\bkick(?:ing|s|ed)? off\b`), "start"},
	{regexp.MustCompile(`(?i)\bcircl(?:e|es|ing|ed) back\b`), "return"},
	{regexp.MustCompile(`(?i)\btouch(?:ing|es|ed)? base\b`), "talk"},
}

// phrasalVerb gives one finding for each soft phrasal verb of the closed list, in sentence order.
func phrasalVerb(sentence prose.Sentence) []Finding {
	type match struct {
		at      int
		finding Finding
	}
	var matches []match
	for _, family := range phrasalVerbs {
		for _, at := range family.pattern.FindAllStringIndex(sentence.Text, -1) {
			phrase := sentence.Text[at[0]:at[1]]
			matches = append(matches, match{at[0], finding(sentence, "phrasal-verb",
				fmt.Sprintf("Replace %q with one plain verb, for example %q.", phrase, family.verb))})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].at < matches[j].at })
	findings := make([]Finding, 0, len(matches))
	for _, m := range matches {
		findings = append(findings, m.finding)
	}
	return findings
}

// nominalizations is upstream's pattern for an action frozen into a noun: perform, conduct or
// carry out, an article, and a noun of action.
var nominalizations = regexp.MustCompile(`(?i)\b(?:perform|performs|performed|conduct|conducts|conducted|carry out|carries out|carried out)\s+(?:a|an|the)\s+\w+(?:tion|sion|ment|ance|ence|ysis)\b`)

// nominalization gives one finding for each action frozen into a noun.
func nominalization(sentence prose.Sentence) []Finding {
	var findings []Finding
	for _, phrase := range nominalizations.FindAllString(sentence.Text, -1) {
		findings = append(findings, finding(sentence, "nominalization",
			fmt.Sprintf(`Replace %q with the verb of its noun. For example, write "analyze", not "perform an analysis".`, phrase)))
	}
	return findings
}

// longSentence finds a sentence of more than MaxWords words, the words counted as upstream counts
// them, split at white space, in the one sentence. CodeToken and URLToken count as one word each.
func longSentence(sentence prose.Sentence) []Finding {
	words := len(strings.Fields(sentence.Text))
	if words <= MaxWords {
		return nil
	}
	return []Finding{finding(sentence, "long-sentence",
		fmt.Sprintf("Split this sentence into shorter sentences. It has %d words. The limit is %d.", words, MaxWords))}
}

// conjunctionEnd is a text ending in "and" or "or", case aside, with nothing after but white space.
var conjunctionEnd = regexp.MustCompile(`(?i)\b(and|or)\s*$`)

// danglingConjunction finds a list item that ends in "and" or "or": the last sentence of a
// list-item segment, which is the end of the item's own last paragraph. A code span ends no item
// in a conjunction, since it stands as CodeToken ("Combine `left` and `right`").
func danglingConjunction(sentence prose.Sentence) []Finding {
	if sentence.Kind != prose.KindListItem || !sentence.Last {
		return nil
	}
	conjunction := conjunctionEnd.FindStringSubmatch(sentence.Text)
	if conjunction == nil {
		return nil
	}
	return []Finding{finding(sentence, "dangling-conjunction",
		fmt.Sprintf("Do not end a list item with %q. Complete the item, or join it to the next item.", conjunction[1]))}
}
