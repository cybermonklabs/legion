package replay

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// CapBacktest is the cap backtest of a window (spec R5): its replay under the rule its daemon ran, on
// one branch with its seeds, once at the caps its record shows and once at one flat cap from its
// start to its end (Cap, RunConfig.Cap), and the slots each gives out in each span of one cap its
// daemon served (capSpans) against the record's. Under the recorded caps each span stands within
// gapCapSpan of the record; under the flat cap each span whose cap is not the flat one misses the
// record by more than gapCapSpan, and a span at the flat cap is not read. A backtest that fails
// either, or that reads no span at the flat cap, cannot tell a change of the cap.
type CapBacktest struct {
	Window string         `json:"window"`
	Rule   string         `json:"rule"`
	Cap    int            `json:"cap"`
	Branch BranchAt       `json:"branch"`
	Seeds  []int64        `json:"seeds"`
	Spans  []CapSpanCheck `json:"spans"`
	Pass   bool           `json:"pass"`
}

// CapSpanCheck is one span of one cap in a cap backtest: the span; the slots the record gave out in
// it; the mean over the seeds of the slots the replay at the recorded caps gave out in it and of
// those the replay at the flat cap did, each with its lowest and highest seed's; whether the first
// stands within gapCapSpan of the record (RecordedPass); whether the flat cap's is read, the span's
// cap not being the flat one (FlatRead); and whether it misses the record by more than gapCapSpan
// (FlatPass).
type CapSpanCheck struct {
	CapSpan
	Recorded       int     `json:"recorded"`
	AtRecordedCaps float64 `json:"atRecordedCaps"`
	RecordedLow    float64 `json:"recordedLow"`
	RecordedHigh   float64 `json:"recordedHigh"`
	AtFlatCap      float64 `json:"atFlatCap"`
	FlatLow        float64 `json:"flatLow"`
	FlatHigh       float64 `json:"flatHigh"`
	RecordedPass   bool    `json:"recordedPass"`
	FlatRead       bool    `json:"flatRead"`
	FlatPass       bool    `json:"flatPass"`
}

// gapCapSpan is how far the slots a replay gives out in a span of one cap may stand from the
// record's in the cap backtest (spec R5): 10%, the gap of the slots given out on each day.
var gapCapSpan = gapTenth

// CapBacktest is the cap backtest of the window named window under rule at the flat cap limit,
// read from the runs: the main branch's runs of the part of window under rule, made at the record's
// caps in r and at limit in flat (CapBacktestDir), against the window's record, every scorecard
// read from a line timeline. A record or a run written before it held what the backtest reads (the
// spans of one cap, the moments each slot was given out) is refused, as is a part made at a cap
// other than the one its side of the backtest names.
func (r Runs) CapBacktest(window, rule string, flat Runs, limit int) (CapBacktest, error) {
	rec, err := r.window(window)
	if err != nil {
		return CapBacktest{}, err
	}
	if rec.CapSpans == nil || rec.Scorecard.SlotsGiven == nil {
		return CapBacktest{}, fmt.Errorf("replay: the record of the window %s was written before it held the spans of one cap its daemon served and the moments each slot was given out (capSpans, slotsGiven); run its window again, which writes it anew", window)
	}
	side := func(runs Runs, atCap int) ([]Scorecard, error) {
		part, err := runs.part(window, rule)
		if err != nil {
			return nil, err
		}
		if part.plan.Cap != atCap {
			return nil, fmt.Errorf("replay: the part of %s under %s ran at the cap %d (0 for the record's), and the cap backtest reads it at %d", window, rule, part.plan.Cap, atCap)
		}
		runsOf, ok := part.runs[MainBranchAt.Label()]
		if !ok {
			return nil, fmt.Errorf("replay: the part of %s under %s holds no run of its main branch %s", window, rule, MainBranchAt.Label())
		}
		cards := lineCards(runsOf)
		for _, card := range cards {
			if card.SlotsGiven == nil {
				return nil, fmt.Errorf("replay: a run of %s under %s on %s was read without the moments each slot was given out", window, rule, MainBranchAt.Label())
			}
		}
		return cards, nil
	}
	recorded, err := side(r, 0)
	if err != nil {
		return CapBacktest{}, err
	}
	atFlat, err := side(flat, limit)
	if err != nil {
		return CapBacktest{}, err
	}
	b := CapBacktest{Window: window, Rule: rule, Cap: limit, Branch: MainBranchAt, Seeds: make([]int64, 0, len(recorded)),
		Spans: make([]CapSpanCheck, 0, len(rec.CapSpans)), Pass: true}
	for _, card := range recorded {
		b.Seeds = append(b.Seeds, card.Seed)
	}
	read := 0
	for _, span := range rec.CapSpans {
		c := CapSpanCheck{CapSpan: span, Recorded: slotsIn(rec.Scorecard.SlotsGiven, span), FlatRead: span.Cap != limit}
		c.AtRecordedCaps, c.RecordedLow, c.RecordedHigh = spread(spanCounts(recorded, span))
		c.AtFlatCap, c.FlatLow, c.FlatHigh = spread(spanCounts(atFlat, span))
		c.RecordedPass = gapCapSpan.Within(float64(c.Recorded), c.AtRecordedCaps)
		c.FlatPass = c.FlatRead && !gapCapSpan.Within(float64(c.Recorded), c.AtFlatCap)
		if c.FlatRead {
			read++
		}
		b.Pass = b.Pass && c.RecordedPass && (c.FlatPass || !c.FlatRead)
		b.Spans = append(b.Spans, c)
	}
	b.Pass = b.Pass && read > 0
	return b, nil
}

// slotsIn is how many of given, moments in order, fall in span.
func slotsIn(given []time.Time, span CapSpan) int {
	from := sort.Search(len(given), func(i int) bool { return !given[i].Before(span.From) })
	to := sort.Search(len(given), func(i int) bool { return !given[i].Before(span.To) })
	return to - from
}

// spanCounts is the slots each of cards gave out in span, in their order.
func spanCounts(cards []Scorecard, span CapSpan) []float64 {
	counts := make([]float64, len(cards))
	for i, card := range cards {
		counts[i] = float64(slotsIn(card.SlotsGiven, span))
	}
	return counts
}

// Write prints b as Markdown, its headings at depth: what was replayed at which caps, a row for each
// span of one cap with the record's slots and the replays' at the recorded caps and at the flat cap
// with their tests, each span one slot is more than gapCapSpan of, so that a miss of one slot fails
// it, and whether the backtest passes, or that the tool cannot tell a change of the cap.
func (b CapBacktest) Write(w io.Writer, depth int) error {
	p := &printer{w: w}
	p.line("%s The cap backtest: %s under `%s` at the recorded caps and at one cap of %d", strings.Repeat("#", depth), b.Window, b.Rule, b.Cap)
	p.line("")
	p.line("The window is replayed under the rule its daemon ran, `%s`, on the branch `%s` with the seeds %s, once at the caps its record shows and once at one cap of %d from its start to its end.",
		b.Rule, b.Branch.Label(), seedList(b.Seeds), b.Cap)
	p.line("Each replay's slots given out in each span of one cap the daemon served are read against the record's.")
	p.line("Under the recorded caps each span passes within %s of the record; under the cap of %d each span whose cap is not %d passes where it misses the record by more than %s, and a span at %d is not read.",
		gapCapSpan, b.Cap, b.Cap, gapCapSpan, b.Cap)
	p.line("")
	p.line("| from | to | cap | slots, recorded | at the recorded caps (lowest, highest seed) | test | at the cap of %d (lowest, highest seed) | test |", b.Cap)
	p.line("| --- | --- | --- | --- | --- | --- | --- | --- |")
	var small, failed []string
	for _, s := range b.Spans {
		flat := "not read"
		if s.FlatRead {
			flat = passWord(s.FlatPass)
		}
		p.line("| %s | %s | %d | %d | %s | %s | %s | %s |", spanStamp(s.From), spanStamp(s.To), s.Cap, s.Recorded,
			spreadCell(s.AtRecordedCaps, s.RecordedLow, s.RecordedHigh, len(b.Seeds)), passWord(s.RecordedPass),
			spreadCell(s.AtFlatCap, s.FlatLow, s.FlatHigh, len(b.Seeds)), flat)
		if gapCapSpan.Allowed(float64(s.Recorded)) < 1 {
			small = append(small, spanStamp(s.From)+" to "+spanStamp(s.To))
		}
		if !s.RecordedPass || s.FlatRead && !s.FlatPass {
			failed = append(failed, spanStamp(s.From)+" to "+spanStamp(s.To))
		}
	}
	p.line("")
	if len(small) > 0 {
		p.line("In %d of the %d spans one slot is more than %s of the record's count, so a miss of one slot fails it: %s.", len(small), len(b.Spans),
			gapCapSpan, strings.Join(small, ", "))
	}
	switch {
	case b.Pass:
		p.line("The backtest passes: at the recorded caps each span gives out the record's slots within %s, and at the cap of %d each span it reads misses them by more than %s, so the tool tells a change of the cap.",
			gapCapSpan, b.Cap, gapCapSpan)
	case len(failed) > 0:
		p.line("The backtest fails on the spans %s: the tool cannot tell a change of the cap.", strings.Join(failed, ", "))
	default:
		p.line("The backtest reads no span at the cap of %d, since every span's recorded cap is %d: the tool cannot tell a change of the cap.", b.Cap, b.Cap)
	}
	return p.err
}

// spanStamp is a moment of a span as the cap backtest prints it: in UTC, to the minute.
func spanStamp(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04Z")
}
