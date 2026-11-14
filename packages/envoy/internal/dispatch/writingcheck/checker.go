// Package writingcheck is the writing check: it reads the prose of a write's text fields, applies
// the structure rules to every sentence, and, with the detector on, has the model reader judge
// the sentences the code stage picks as candidates for a vacuous verb phrase.
//
// The structure rules always run. The reader is called once per check, over the candidates of
// every field together, and only when there is at least one. A reader fault never refuses a
// write: the check then reports the fault's outcome and the structure rules' findings alone.
package writingcheck

import (
	"context"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/candidates"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/rules"
)

// The outcomes of a check besides the reader's own fault outcomes (reader.Outcome*).
const (
	// OutcomeOff is a check that is turned off and read nothing.
	OutcomeOff = "off"
	// OutcomeRefused is a check with at least one finding.
	OutcomeRefused = "refused"
	// OutcomeClean is a check with no finding and no fault.
	OutcomeClean = "clean"
	// OutcomeNoModel is a check whose detector is on with no reader to call.
	OutcomeNoModel = reader.OutcomeNoModel
)

// detectorRewrite is a vacuous finding's fix where the reader gave none.
const detectorRewrite = "Say the fact the verb phrase stands for: the value, the holder, the date or the consequence."

// Field is one text field of a write: Name is the field's JSON path in the request, Text its
// Markdown.
type Field struct{ Name, Text string }

// Finding is one rule a sentence breaks: the field it stands in, the rule's id, the sentence,
// and a short instruction that says how to fix it.
type Finding struct{ Field, Rule, Text, Rewrite string }

// Reader is the model reader the detector calls; *reader.Reader is one.
type Reader interface {
	Read(ctx context.Context, cands []candidates.Candidate) ([]reader.Verdict, string)
	Calls() int64
}

// Config is what a Checker checks.
type Config struct {
	// Enabled turns the check on; a check turned off reads nothing.
	Enabled bool
	// Rules is the ids of the structure rules to apply, the bundle's EnabledRules; an id
	// package rules does not carry applies nothing.
	Rules []string
	// RulesOff is rule ids not to apply although Rules names them. DetectorRule here turns
	// the detector off.
	RulesOff []string
	// Detector turns the detector on.
	Detector bool
	// Reader is the model reader; nil (a nil *reader.Reader included) is no model.
	Reader Reader
}

// Result is what one check found. Outcome is OutcomeRefused where Findings holds any, else
// OutcomeClean, OutcomeNoModel or the reader's fault outcome. Candidates counts the code stage's
// candidates, and ReaderCalled is whether the reader was asked to read them.
type Result struct {
	Findings     []Finding
	Outcome      string
	Candidates   int
	ReaderCalled bool
}

// Checker checks the fields of a write. It is safe for concurrent use.
type Checker struct {
	enabled  bool
	rules    []rules.Rule
	detector bool
	reader   Reader
}

// New is a Checker over cfg.
func New(cfg Config) *Checker {
	c := &Checker{
		enabled:  cfg.Enabled,
		detector: cfg.Detector && !slices.Contains(cfg.RulesOff, DetectorRule),
		reader:   cfg.Reader,
	}
	if r, ok := cfg.Reader.(*reader.Reader); ok && r == nil {
		c.reader = nil
	}
	for _, rule := range rules.All() {
		if slices.Contains(cfg.Rules, rule.ID) && !slices.Contains(cfg.RulesOff, rule.ID) {
			c.rules = append(c.rules, rule)
		}
	}
	return c
}

// Check reads every field's prose and answers what it found.
func (c *Checker) Check(ctx context.Context, fields []Field) Result {
	if !c.enabled {
		return Result{Outcome: OutcomeOff}
	}
	var (
		result    Result
		sentences []prose.Sentence
	)
	for _, field := range fields {
		sentences = append(sentences, prose.ProseSentences(field.Name, field.Text)...)
	}
	for _, sentence := range sentences {
		for _, rule := range c.rules {
			for _, f := range rule.Check(sentence) {
				result.Findings = append(result.Findings, Finding(f))
			}
		}
	}
	outcome := OutcomeClean
	switch {
	case !c.detector:
	case c.reader == nil:
		outcome = OutcomeNoModel
	default:
		cands := candidates.Find(sentences, candidates.Options{})
		result.Candidates = len(cands)
		if len(cands) == 0 {
			break
		}
		verdicts, readOutcome := c.reader.Read(ctx, cands)
		result.ReaderCalled = true
		if readOutcome != reader.OutcomeRead {
			outcome = readOutcome
		}
		byID := make(map[string]candidates.Candidate, len(cands))
		for _, cand := range cands {
			byID[cand.ID] = cand
		}
		for _, v := range verdicts {
			cand, ok := byID[v.ID]
			if !ok || !v.Vacuous {
				continue
			}
			rewrite := v.Rewrite
			if rewrite == "" {
				rewrite = detectorRewrite
			}
			result.Findings = append(result.Findings, Finding{
				Field: cand.Sentence.Field, Rule: DetectorRule, Text: cand.Sentence.Text, Rewrite: rewrite,
			})
		}
	}
	if len(result.Findings) > 0 {
		outcome = OutcomeRefused
	}
	result.Outcome = outcome
	return result
}
