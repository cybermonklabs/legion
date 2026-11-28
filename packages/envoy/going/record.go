package going

// record.go reads the record the bake-off (cmd/going-bakeoff) writes: one entry for each
// decision model it measured, with that model's measurement. A deployment names a model and
// nothing else, and a model with no entry here is no decision model (R13). It also holds R13's
// choice rule (Choose), which the bake-off names the chosen model by and the record's own test
// holds the committed choice to, and the writer of the record's bytes (EncodeRecords).

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

//go:embed decision_models.json
var decisionModelsJSON []byte

// R13's floors for Jev: "Jev chooses the right action on at least 95% of the 117 cases of
// packages/envoy/going/bakeoff/cases.json, and 95% of its answers end in under 500 ms." So a model
// is a candidate only where it chooses the right action on at least JevRightFloor of the cases
// R13 names, all together, pooled over every repeat (RightShare), and its p95 is under
// JevP95Ceiling (MeetsFloors). The floors admit a model and rank nothing: a p95 well under the
// ceiling counts for no more than one just under it. A run that measures fewer than LeastCases of
// R13's cases, or fewer than LeastChartCases chart-worthy ones, is void (cmd/going-bakeoff).
const (
	JevRightFloor   = 0.95
	JevP95Ceiling   = 500 * time.Millisecond // exclusive
	LeastCases      = 80
	LeastChartCases = 20
)

// Record is one measured model and its measurement. Chosen marks the model R13's rule named
// (Choose), or the one a tie-break named where it named none. A run may ask every case more than
// once (Repeats); every accuracy, latency and display rate here then pools every answer of every
// repeat. Every count and rate is of the cases R13 names, the cases R13's rule and check c14 are
// measured on; a case of the case set outside them is kept apart in OutsideCases, which the rule
// and its floors never read.
type Record struct {
	Model    string `json:"model"`
	Endpoint string `json:"endpoint"`
	// MeasuredAt is when the run measured the model, and ReleaseDate the release date the
	// endpoint gave for the model then, both RFC 3339.
	MeasuredAt  string `json:"measured_at"`
	ReleaseDate string `json:"release_date"`
	// The case counts: every case run, those whose words name their action, the rest, the
	// chart-worthy ones, each case counted once however many times it was asked; and the calls
	// that gave no answer, over every repeat.
	Cases            int `json:"cases"`
	NamedCases       int `json:"named_cases"`
	OtherCases       int `json:"other_cases"`
	ChartWorthyCases int `json:"chart_worthy_cases"`
	Unanswered       int `json:"unanswered"`
	// NamedAccuracy and OtherAccuracy are the action question's accuracy on the cases that
	// name their action and on the rest.
	NamedAccuracy float64 `json:"named_accuracy"`
	OtherAccuracy float64 `json:"other_accuracy"`
	// P50MS and P95MS are the latency of one decision call, in milliseconds.
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	// DisplayRates is how the model answered the display question, for each label.
	DisplayRates []DisplayRate `json:"display_rates"`
	// Repeats is how many times the run asked the model every case, and RepeatRights the right
	// choices of each repeat, in the order the repeats ran.
	Repeats      int           `json:"repeats"`
	RepeatRights []RepeatRight `json:"repeat_rights"`
	// TotalSpread is how the total of right choices spread over the repeats.
	TotalSpread Spread `json:"total_spread"`
	// InconsistentCases is every case the model chose right in some repeats and wrong in
	// others.
	InconsistentCases []CaseConsistency `json:"inconsistent_cases"`
	// PairedTests is the paired test of the model against each other model of the run, over
	// the same cases and repeats. It is evidence of how far a lead in right choices can be
	// trusted, recorded beside the choice; it names no model.
	PairedTests []PairedTest `json:"paired_tests"`
	// OutsideCases is each case of the case set outside the cases R13 names, with what the model
	// chose for it over every repeat; none where the set holds no such case.
	OutsideCases []OutsideCase `json:"outside_cases,omitempty"`
	// Chosen marks the model the choice named, and ChosenBy how (ChosenByRightChoices or
	// ChosenByTieBreak). Chooser is who broke the tie and ChosenReason why, on a ChosenByTieBreak
	// entry alone. All three are empty on every entry not chosen.
	Chosen       bool   `json:"chosen"`
	ChosenBy     string `json:"chosen_by,omitempty"`
	Chooser      string `json:"chooser,omitempty"`
	ChosenReason string `json:"chosen_reason,omitempty"`
}

// OutsideCase is one case of the case set outside the cases R13 names (cmd/going-bakeoff's
// outside_r13), with why it is there, its label, and what one model chose for it: Right of Of
// answers chose its label, each choice made with how many answers, and the calls that gave none.
type OutsideCase struct {
	Words      string        `json:"words"`
	Note       string        `json:"note"`
	Action     string        `json:"action"`
	Right      int           `json:"right"`
	Of         int           `json:"of"`
	Chose      []ChoiceCount `json:"chose"`
	Unanswered int           `json:"unanswered"`
}

// ChoiceCount is one action a model chose and how many answers chose it.
type ChoiceCount struct {
	Action string `json:"action"`
	Count  int    `json:"count"`
}

// How the chosen Record was named, which its ChosenBy holds: by the key of R13's rule that named
// it, or by a tie-break where the rule names none. Who broke a tie is the entry's Chooser, apart
// from ChosenBy, which says only how the model was named, so every reader of ChosenBy compares it
// with these two keys.
const (
	// ChosenByRightChoices is the candidate with the most right choices pooled over every
	// repeat, alone at that count; the one model that meets the floors is that too.
	ChosenByRightChoices = "right_choices"
	// ChosenByTieBreak is the candidate a tie-break named where the rule names none, the leading
	// candidates being level on right choices (going-bakeoff --tie-break): the entry's Chooser
	// says who broke the tie, as R13 gives it to the issue's architect, and its ChosenReason why.
	ChosenByTieBreak = "tie_break"
)

// RepeatRight is one repeat's right choices: on the cases that name their action, on the rest,
// and in all.
type RepeatRight struct {
	Named int `json:"named"`
	Other int `json:"other"`
	Total int `json:"total"`
}

// CaseConsistency is a case's right choices over a run's repeats: Right of Of repeats.
type CaseConsistency struct {
	Words string `json:"words"`
	Right int    `json:"right"`
	Of    int    `json:"of"`
}

// DisplayRate is the share of the answers to the cases of one display label the model answered
// one way: Of answers, one for each such case in each repeat, and Count of them were answered
// Answered.
type DisplayRate struct {
	// Label is chart_worthy or not_chart_worthy.
	Label string `json:"label"`
	// Answered is chart, prose, or unanswered for a call that gave no answer.
	Answered string  `json:"answered"`
	Count    int     `json:"count"`
	Of       int     `json:"of"`
	Rate     float64 `json:"rate"`
}

// The labels and answers a DisplayRate names beyond the display question's own choices.
const (
	LabelChartWorthy    = "chart_worthy"
	LabelNotChartWorthy = "not_chart_worthy"
	DisplayUnanswered   = "unanswered"
)

// decisionRecords is the embedded record, read once.
var decisionRecords = sync.OnceValues(func() ([]Record, error) {
	var records []Record
	if err := json.Unmarshal(decisionModelsJSON, &records); err != nil {
		return nil, fmt.Errorf("going: decision_models.json: %w", err)
	}
	return records, nil
})

// DecisionRecord is the entry for the model of that name, and whether the record holds one. A
// record the binary cannot read holds no model, which TestTheRecordReadsBack keeps from
// shipping.
func DecisionRecord(model string) (Record, bool) {
	records, err := decisionRecords()
	if err != nil {
		return Record{}, false
	}
	for _, record := range records {
		if record.Model == model {
			return record, true
		}
	}
	return Record{}, false
}

// Right is the model's right choices pooled over every repeat: the count R13's rule ranks by.
func (r *Record) Right() int {
	right := 0
	for _, counted := range r.RepeatRights {
		right += counted.Total
	}
	return right
}

// RightShare is the share of the model's answers to the cases R13 names, one for each case in
// each repeat, that chose the right action, a call that gave no answer among them: Right of
// Cases times Repeats, and 0 for a record of no answer.
func (r *Record) RightShare() float64 {
	answers := r.Cases * r.Repeats
	if answers == 0 {
		return 0
	}
	return float64(r.Right()) / float64(answers)
}

// MeetsFloors is whether the model is a candidate of R13's rule: it chooses the right action on
// at least JevRightFloor of the cases R13 names, all together (RightShare), and its p95 is under
// JevP95Ceiling. Its accuracies on the cases that name their action and on the rest, each apart,
// and its action fit are no floor, and a candidate without one is ranked as any other (Choose).
func (r *Record) MeetsFloors() bool {
	return r.RightShare() >= JevRightFloor && r.P95MS < float64(JevP95Ceiling.Milliseconds())
}

// PairedAgainst is the model's paired test against the named model, and whether it holds one.
func (r *Record) PairedAgainst(model string) (PairedTest, bool) {
	for _, test := range r.PairedTests {
		if test.Against == model {
			return test, true
		}
	}
	return PairedTest{}, false
}

// ChoiceOutcome is what R13's rule came to on one run's records (Choose).
type ChoiceOutcome string

const (
	// NoCandidate is a run none of whose models meets R13's floors: nothing is ranked, and no
	// model is named.
	NoCandidate ChoiceOutcome = "no_candidate"
	// Named is a run whose one candidate ranked first the rule names (ChosenByRightChoices), or
	// a full tie a tie-break broke (Break, ChosenByTieBreak).
	Named ChoiceOutcome = "named"
	// FullTie is a run whose candidates ranked first are several, level on right choices: the
	// rule names none, and the architect may name one of them (Break).
	FullTie ChoiceOutcome = "full_tie"
)

// Candidate is a model that meets R13's floors, with the count the rule ranks it by: its right
// choices pooled over every repeat.
type Candidate struct {
	Model string
	Right int
}

// Choice is what R13's rule came to over one run's records (Choose), or a tie-break's choice
// where the rule names none (Break), with the counts the rule ranked by, so a reader says what
// the rule came to and ranks nothing again.
type Choice struct {
	// Outcome is what the rule came to, and Named on a tie a tie-break broke.
	Outcome ChoiceOutcome
	// Model is the model named and By how, a ChosenBy value; both are empty unless Outcome is
	// Named. Chooser is who broke the tie and Reason why, on a tie-break's Choice alone.
	Model, By, Chooser, Reason string
	// Ranked is every candidate in the order the rule ranks them: by right choices, and the
	// records' order last, which names nothing. It is empty where no model meets the floors.
	Ranked []Candidate
	// Of is the answers each candidate gave over every repeat, the count its Right is of.
	Of int
	// Level is the candidates ranked first, those with the most right choices, in the records'
	// order. It holds one model where the ranking puts one first, every model of a full tie
	// where it puts several, and nothing where no model meets the floors.
	Level []string
	// Why is, where the rule names no model, why: no model meets the floors, or the models
	// ranked first are a full tie. A tie-break's choice keeps the Why of the tie it broke. It
	// is empty where the rule names a model.
	Why string
}

// Choose is R13's choice rule over one run's records, and what it came to (Outcome) with the
// right choices it ranked the candidates by (Ranked). Every model that meets R13's two floors
// is a candidate (MeetsFloors), ranked by its right choices pooled over every repeat. Where one
// candidate ranks first, it is named (ChosenByRightChoices). Where several rank first, level on
// right choices, the run is a full tie and the rule names none: the architect names one of Level
// (Break). Nothing else decides: the paired test says how far a lead in right choices can be
// trusted and names no model, p95 is a floor and no key, and the records' order names nothing,
// Ranked and Level keeping it only to print by.
func Choose(records []Record) Choice {
	type ranking struct {
		record *Record
		right  int
	}
	var ranked []ranking
	for i := range records {
		if records[i].MeetsFloors() {
			ranked = append(ranked, ranking{&records[i], records[i].Right()})
		}
	}
	if len(ranked) == 0 {
		return Choice{Outcome: NoCandidate, Why: "no model meets R13's floors"}
	}
	slices.SortStableFunc(ranked, func(a, b ranking) int { return b.right - a.right })
	first, most := ranked[0].record, ranked[0].right
	choice := Choice{Ranked: make([]Candidate, len(ranked)), Of: first.Cases * first.Repeats}
	for n, c := range ranked {
		choice.Ranked[n] = Candidate{Model: c.record.Model, Right: c.right}
		if c.right == most {
			choice.Level = append(choice.Level, c.record.Model)
		}
	}
	if len(choice.Level) == 1 {
		choice.Outcome, choice.Model, choice.By = Named, first.Model, ChosenByRightChoices
		return choice
	}
	choice.Outcome = FullTie
	choice.Why = fmt.Sprintf("%s are level at %d of %d right choices pooled over every repeat, so R13's rule names none and the architect names one of them",
		nameList(choice.Level), most, choice.Of)
	return choice
}

// Break is a tie-break's choice of model where the rule names none: chooser names the model, for
// reason. It refuses every Outcome but FullTie - a run the rule decides, a run with no candidate
// - a model outside the full tie, and a chooser or a reason that says nothing, returning c as it
// was: a model is named by hand only among models the measurement cannot tell apart, and the
// record keeps who named it and why.
func (c Choice) Break(model, chooser, reason string) (Choice, error) {
	switch c.Outcome {
	case Named:
		return c, fmt.Errorf("R13's rule names %s (%s), and a tie-break names a model only where the leading candidates are level on right choices", c.Model, c.By)
	case NoCandidate:
		return c, errors.New("no model meets R13's floors, so there is no tie to break")
	case FullTie:
	default:
		return c, fmt.Errorf("R13's rule came to %q, an outcome a tie-break cannot break", c.Outcome)
	}
	switch {
	case !slices.Contains(c.Level, model):
		return c, fmt.Errorf("%q is not one of the level models, %s", model, strings.Join(c.Level, ", "))
	case strings.TrimSpace(chooser) == "":
		return c, fmt.Errorf("naming %s names no chooser, and the record keeps who broke the tie", model)
	case strings.TrimSpace(reason) == "":
		return c, fmt.Errorf("naming %s gives no reason, and the record keeps why the tie was broken so", model)
	}
	c.Outcome, c.Model, c.By, c.Chooser, c.Reason = Named, model, ChosenByTieBreak, chooser, reason
	return c, nil
}

// nameList is the names joined as a sentence lists them.
func nameList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// EncodeRecords is the bytes of the record the bake-off writes: Go's indented JSON, with a final
// newline. The committed file is these bytes exactly (TestTheRecordIsWhatTheBakeOffWrites), and
// biome.json's formatter leaves the file alone, so a run's record is committed as written.
func EncodeRecords(records []Record) ([]byte, error) {
	raw, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
