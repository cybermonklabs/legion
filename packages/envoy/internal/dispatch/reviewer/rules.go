package reviewer

import (
	"cmp"
	"context"
	"embed"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"unicode/utf8"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/modelpace"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/candidates"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/plainwords"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"
)

// RuleSet is one set of rules a call applies (DISPATCH_REVIEWER_RULES).
type RuleSet string

// The rule sets.
const (
	// RuleNoslop is ce-noslop's tests, its pattern catalog and its terminology: skills/plain-words
	// without the five phrases (plainwords).
	RuleNoslop RuleSet = "noslop"
	// RulePhrases is the five phrase shapes skills/plain-words tells agents to stop: the last
	// section of that skill.
	RulePhrases RuleSet = "phrases"
	// RuleReaderWords is each use of a word the reader asked about before, unless the text
	// defines it (Record.ReaderWords).
	RuleReaderWords RuleSet = "reader_words"
	// RuleAskParts is an ask's five parts: the decision, why it is needed now, the facts, a
	// recommendation, and what each option causes. It applies to an ask and a decision block.
	RuleAskParts RuleSet = "ask_parts"
	// RuleStructure is writingcheck's structure rules (writingcheck.EnabledRules), which run
	// before the model call; their findings are an input of the call.
	RuleStructure RuleSet = "structure"
	// RuleDetector is writingcheck's phrase detector, which runs before the model call with its
	// own call of the same model; its findings are an input of the call.
	RuleDetector RuleSet = "detector"
)

// AllRuleSets is every rule set, in the order DISPATCH_REVIEWER_RULES's description names them.
var AllRuleSets = []RuleSet{RuleNoslop, RulePhrases, RuleReaderWords, RuleAskParts, RuleStructure, RuleDetector}

// DefaultRules are the rule sets DISPATCH_REVIEWER_RULES names unset: cmd/dispatch's default,
// and the one cmd/writing-check's harness reads a local Dispatch to run where it sets none.
var DefaultRules = []RuleSet{RuleNoslop, RulePhrases, RuleReaderWords, RuleAskParts}

// phrasesHeading is the heading of skills/plain-words' section of the five phrases.
const phrasesHeading = "## The phrases to stop"

// noslopText and phrasesText are skills/plain-words split into the two rule sets: ce-noslop's
// SKILL.md body with its pattern catalog and its terminology, and the table of the five phrases.
var noslopText, phrasesText = splitSkill(plainwords.Skill)

// splitSkill is skill, SKILL.md after its frontmatter, as the noslop rule set's text, with
// plainwords' two references after it, and the text of the five phrases' section. A skill with
// no such section is noslop's whole.
func splitSkill(skill string) (noslop, phrases string) {
	before, rest, found := strings.Cut(skill, "\n"+phrasesHeading+"\n")
	if !found {
		return strings.TrimSpace(skill) + "\n\n" + plainwords.Patterns + "\n" + plainwords.Terminology, ""
	}
	section, after, more := strings.Cut(rest, "\n## ")
	noslop = strings.TrimSpace(before)
	if more {
		noslop += "\n\n## " + strings.TrimSpace(after)
	}
	return noslop + "\n\n" + strings.TrimSpace(plainwords.Patterns) + "\n\n" + strings.TrimSpace(plainwords.Terminology), strings.TrimSpace(section)
}

// prompts are the templates of every prompt this package sends or writes.
//
//go:embed prompts/*.md
var promptFiles embed.FS

var prompts = template.Must(template.New("prompts").Funcs(template.FuncMap{
	"add": func(a, b int) int { return a + b },
}).ParseFS(promptFiles, "prompts/*.md"))

// render is the template name filled from data.
func render(name string, data any) (string, error) {
	var b strings.Builder
	if err := prompts.ExecuteTemplate(&b, name, data); err != nil {
		return "", fmt.Errorf("reviewer: fill the %s prompt: %w", name, err)
	}
	return strings.TrimSpace(b.String()) + "\n", nil
}

// promptData is what a system prompt is filled from.
type promptData struct {
	// Kind is the text's kind as a reader says it.
	Kind string
	// On holds each rule set the call applies, by name.
	On map[string]bool
	// Ask, Block, Comment and Issue say what the text is, for the rules a rewrite keeps:
	// an ask or a decision block, a decision block, a comment, an issue.
	Ask, Block, Comment, Issue bool
	// Answer says the call reads the writer's answer, and Define that it defines words.
	Answer, Define bool
	// Review says the call is a review, whose rewrite fixes its own findings, not a rewrite for
	// the reader by the rules alone.
	Review bool
	// Noslop and Phrases are the texts of those two rule sets.
	Noslop, Phrases string
}

// kindNames are the kinds as a reader says them.
var kindNames = map[Kind]string{KindDecisionBlock: "decision block"}

// spoken is kind as a reader says it.
func (k Kind) spoken() string { return cmp.Or(kindNames[k], string(k)) }

// applies is the rule sets of rules that apply to a text of kind, each once, in AllRuleSets'
// order: ask_parts only to an ask or a decision block, and only to a call that writes one.
func applies(rules []RuleSet, kind Kind, define bool) []RuleSet {
	var on []RuleSet
	for _, set := range AllRuleSets {
		if !slices.Contains(rules, set) || (set == RuleAskParts && (!kind.asks() || define)) {
			continue
		}
		on = append(on, set)
	}
	return on
}

// prepared is a call's system prompt, its user message and the rule sets it applies, and the
// detector's call where the detector ran.
type prepared struct {
	system, input string
	on            []RuleSet
	checks        []writingcheck.Finding
	calls         []Call
}

// systemPrompt is the data of the system prompt named prompt for a text of kind by rules, and the
// rule sets of rules that apply to it: answer says the call reads the writer's answer, and define
// that it defines words.
func systemPrompt(prompt string, rules []RuleSet, kind Kind, answer, define bool) (promptData, []RuleSet) {
	on := applies(rules, kind, define)
	data := promptData{
		Kind: kind.spoken(), On: map[string]bool{},
		Ask: kind.asks(), Block: kind == KindDecisionBlock, Comment: kind == KindComment, Issue: kind == KindIssue,
		Answer: answer, Define: define, Review: prompt == PurposeReview,
		Noslop: noslopText, Phrases: phrasesText,
	}
	for _, set := range on {
		data.On[string(set)] = true
	}
	return data, on
}

// prepare builds the call named prompt about record: the rule sets of rules that apply, the
// structure rules' and the detector's findings where they are on (the detector's own model call
// among the calls), and the user message with the parts of x the call reads.
func prepare(ctx context.Context, m Model, prompt string, rules []RuleSet, record Record, x extra) (prepared, error) {
	data, on := systemPrompt(prompt, rules, record.Text.Kind, x.answer != "", x.words != nil)
	p := prepared{on: on}
	x.readerWords = data.On[string(RuleReaderWords)]
	x.structureOn, x.detectorOn = data.On[string(RuleStructure)], data.On[string(RuleDetector)]
	if x.structureOn || x.detectorOn {
		x.structure, x.detector, p.calls = runChecks(ctx, m, record.Text, x.structureOn, x.detectorOn)
		p.checks = append(append(p.checks, x.structure...), x.detector...)
	}
	var err error
	if p.system, err = render(prompt, data); err != nil {
		return prepared{}, err
	}
	p.input = record.input(x)
	return p, nil
}

// LeastSystemPromptRunes is how many characters the shortest system prompt has that a call of one
// of purposes (PurposeReview, PurposeRewrite, PurposeDefine, each the name of its prompt) sends by
// rules, for any kind of text (Kinds) and with no answer of the writer's: a review of a text some
// rule set of rules applies to, a rewrite or a definition. It is 0 where purposes make no call by
// rules: a review by rule sets that apply to no text. Every such call is charged at least
// modelpace.Charge of it and the call's reply bound, so cmd/dispatch refuses a model group's
// minute's quota that is not more than that charge of the calls the group makes.
func LeastSystemPromptRunes(rules []RuleSet, purposes ...string) (int, error) {
	least := -1
	for _, kind := range Kinds {
		for _, purpose := range purposes {
			data, on := systemPrompt(purpose, rules, kind, false, purpose == PurposeDefine)
			if purpose == PurposeReview && len(on) == 0 {
				// Review makes no call for a text no rule set applies to.
				continue
			}
			system, err := render(purpose, data)
			if err != nil {
				return 0, err
			}
			if n := utf8.RuneCountInString(system); least < 0 || n < least {
				least = n
			}
		}
	}
	return max(least, 0), nil
}

// runChecks runs writingcheck over text: its structure rules where structure is on, and its
// detector where detector is on, whose reader calls m's model through m's pacer. It answers each
// part's findings and the detector's call, if it made one.
func runChecks(ctx context.Context, m Model, text Text, structure, detector bool) (structureFindings, detectorFindings []writingcheck.Finding, calls []Call) {
	var cfg writingcheck.Config
	if structure {
		cfg.Rules = writingcheck.EnabledRules
	}
	var paced *pacedReader
	if detector {
		paced = &pacedReader{pace: m.Pace, call: Call{Purpose: PurposeDetector, Deployment: m.deployment()}}
		paced.inner = reader.New(reader.Config{
			Model:       m.Model,
			NoEffort:    writingcheck.ReaderNoEffort && m.Model.Endpoint.Kind == agentloop.OpenAIChat,
			Timeout:     cmp.Or(m.Timeout, DefaultTimeout),
			Temperature: m.Temperature,
			Observe: func(c reader.Call) {
				paced.call.Attempts, paced.call.Served, paced.call.Usage, paced.call.Fault = 1, c.Model, c.Usage, c.Fault
			},
		})
		cfg.Detector, cfg.Reader = true, paced
	}
	for _, f := range writingcheck.New(cfg).Check(ctx, fieldsOf(text)).Findings {
		if f.Rule == writingcheck.DetectorRule {
			detectorFindings = append(detectorFindings, f)
		} else {
			structureFindings = append(structureFindings, f)
		}
	}
	if paced != nil && paced.called {
		calls = append(calls, paced.call)
	}
	return structureFindings, detectorFindings, calls
}

// fieldsOf is text's prose as writingcheck reads a write's fields.
func fieldsOf(text Text) []writingcheck.Field {
	fields := []writingcheck.Field{}
	if text.Title != "" {
		fields = append(fields, writingcheck.Field{Name: "title", Text: text.Title})
	}
	fields = append(fields, writingcheck.Field{Name: "body", Text: text.Body})
	for i, option := range text.Options {
		if option.Description != "" {
			fields = append(fields, writingcheck.Field{Name: "options[" + strconv.Itoa(i) + "].description", Text: option.Description})
		}
	}
	return fields
}

// pacedReader is the detector's reader on the caller's share of its deployment's pacer: each read
// waits for the pacer first, charged with the prompt, the candidates and the whole reply's bound.
// It keeps its one call for the review's result.
type pacedReader struct {
	inner  *reader.Reader
	pace   *modelpace.Caller
	call   Call
	called bool
}

func (p *pacedReader) Read(ctx context.Context, cands []candidates.Candidate) ([]reader.Verdict, string) {
	p.called = true
	if p.pace != nil {
		chars := utf8.RuneCountInString(reader.Prompt)
		for _, cand := range cands {
			chars += utf8.RuneCountInString(cand.Sentence.Text) + utf8.RuneCountInString(strings.Join(cand.Before, " ")) + utf8.RuneCountInString(strings.Join(cand.After, " "))
		}
		if err := p.pace.WaitFor(ctx, chars, reader.DefaultMaxOutputTokens); err != nil {
			// The pacer refuses a call it cannot serve in the time left: the call's time ran
			// out before it was made (modelpace.ErrPastDeadline is a deadline).
			p.call.Fault = agentloop.FaultTime
			return nil, reader.OutcomeBudget
		}
	}
	return p.inner.Read(ctx, cands)
}

func (p *pacedReader) Calls() int64 { return p.inner.Calls() }

// OptionWords is options as a model call reads and writes them: each option's label and
// description, with no id, and an empty list where there are none, so JSON shows [] and never
// null. An option's id is the server's, not the text's: the ask's row holds none, the part index
// gives each option one on a read (refs.AskPartIDs), and an edit that rewords an option in its
// place keeps it (itemblocks.Align). So a rewrite keeps an ask's options by their labels, their
// number and their order (Preserve), and no model reads or writes an id; a held ask that posts the
// reviewer's rewrite takes its options so (the api package's withRewrite).
func OptionWords(options []model.AskOption) []model.AskOption {
	words := make([]model.AskOption, len(options))
	for i, option := range options {
		words[i] = model.AskOption{Label: option.Label, Description: option.Description}
	}
	return words
}
