package reviewer

import (
	"context"
	"regexp"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
)

// Finding is one place where a text breaks a rule: the rule set, the words of the text, why they
// break it, and the words to write in their place.
type Finding struct {
	RuleSet RuleSet `json:"rule_set"`
	Quote   string  `json:"quote"`
	Why     string  `json:"why"`
	Fix     string  `json:"fix"`
}

// DroppedFinding is a finding of the reply that the review dropped because its quote is not in
// the text: its rule set, and its quote as the model wrote it. The quote is kept to show what
// the model quoted: words of the record, or words of the text that it changed.
type DroppedFinding struct {
	RuleSet RuleSet `json:"rule_set"`
	Quote   string  `json:"quote"`
}

// Reviewed is what one review answered.
type Reviewed struct {
	// Findings are the places where the text breaks a rule; none where the text passed.
	Findings []Finding
	// Dropped are the findings of the reply whose quote is not in the text, in the reply's
	// order. The review drops such a finding: its fix has no words of the text to stand in place
	// of. A review whose every finding is dropped found nothing.
	Dropped []DroppedFinding
	// Rewrite is the text with each finding fixed, nil where the review found nothing or the
	// model wrote none.
	Rewrite *Text
	// NotApplied is the rule of what a rewrite keeps that Rewrite breaks (Preserve), nil where
	// it keeps them all. A rewrite that breaks one is not applied, so it is not posted as
	// rewritten; the writer still gets it with the findings.
	NotApplied *NotApplied
	// Checks are the findings of writingcheck's structure rules and detector, which the model
	// read as inputs (the structure and detector rule sets).
	Checks []writingcheck.Finding
	// Calls are the model calls the review made, the detector's first.
	Calls []Call
}

// reviewReply is a review's reply as its schema has it.
type reviewReply struct {
	Findings []Finding `json:"findings"`
	Rewrite  *Text     `json:"rewrite"`
}

// Review reviews record's text by rules (DISPATCH_REVIEWER_RULES): one model call that returns
// the findings and a rewrite, after the structure rules and the detector where they are on. A
// text no rule set applies to passes with no call. A finding whose quote is not in the text is
// dropped (Reviewed.Dropped). A model call that gives no usable answer is a *ModelError, and the
// Reviewed then still carries the calls made.
func Review(ctx context.Context, m Model, rules []RuleSet, record Record) (Reviewed, error) {
	p, err := prepare(ctx, m, PurposeReview, rules, record, extra{})
	if err != nil {
		return Reviewed{}, err
	}
	reviewed := Reviewed{Checks: p.checks, Calls: p.calls}
	if len(p.on) == 0 {
		return reviewed, nil
	}
	reply, c, err := call[reviewReply](ctx, m, PurposeReview, p.system, p.input, reviewSchema(record.Text.Kind, p.on, m.strict()), nil)
	reviewed.Calls = append(reviewed.Calls, c)
	if err != nil {
		return reviewed, err
	}
	form := quoteForm
	if record.Text.Kind.asks() {
		form = askQuoteForm
	}
	text := form(readerFields(record.Text))
	for _, finding := range reply.Findings {
		finding.Quote, finding.Why, finding.Fix = unembedded(finding.Quote), unembedded(finding.Why), unembedded(finding.Fix)
		if quote := form(finding.Quote); quote == "" || !strings.Contains(text, quote) {
			reviewed.Dropped = append(reviewed.Dropped, DroppedFinding{RuleSet: finding.RuleSet, Quote: finding.Quote})
			continue
		}
		reviewed.Findings = append(reviewed.Findings, finding)
	}
	if len(reviewed.Findings) > 0 && reply.Rewrite != nil {
		rewrite := ofKind(*reply.Rewrite, record.Text.Kind)
		reviewed.Rewrite = &rewrite
		reviewed.NotApplied = Preserve(record, "", rewrite)
	}
	return reviewed, nil
}

// quoteMarks are the marks a quote is looked up without: Markdown's emphasis and code marks, and
// curly quotes, which stand as straight ones.
var quoteMarks = strings.NewReplacer("*", "", "`", "", "\u2019", "'", "\u2018", "'", "\u201c", "\"", "\u201d", "\"")

// quoteForm is text as a finding's quote is looked up in it, as the harness's quote check reads
// both (writing-check's claude.QuoteForm): lower-cased, without quoteMarks, and with each run of
// white space one space.
func quoteForm(text string) string {
	return strings.Join(strings.Fields(quoteMarks.Replace(strings.ToLower(text))), " ")
}

// optionSyntax is the syntax that the model's input (textPart's JSON) or an ask block's line puts
// around and between an option's label and description: the keys "label" and "description" with
// their colon, quote marks with the backslash that escapes one, colons, commas, braces and
// brackets.
var optionSyntax = regexp.MustCompile(`"(?:label|description)"\s*:|\\?"|[:,{}\[\]]`)

// askQuoteForm is an ask's text, or a quote of it, as a finding's quote is looked up in it:
// quoteForm, with each match of optionSyntax one space. The model reads an option as JSON and may
// quote it so, `"Send it","description":"I post the draft"`, or as an ask block writes it,
// `Send it: I post the draft`. Both become `send it i post the draft`. readerFields puts each
// option's label and its description side by side, so both quotes are in the ask. A quote that
// holds a word the ask does not hold is not.
func askQuoteForm(text string) string {
	return strings.Join(strings.Fields(optionSyntax.ReplaceAllString(quoteForm(text), " ")), " ")
}

// ofKind is a reply's text as a text of kind: its kind set, only the fields the kind has, and each
// field's words as the text holds them (unembedded).
func ofKind(text Text, kind Kind) Text {
	text = text.unembedded()
	text.Kind = kind
	if kind != KindIssue {
		text.Title = ""
	}
	if !kind.asks() {
		text.Options, text.Multiple = nil, false
	}
	return text
}
