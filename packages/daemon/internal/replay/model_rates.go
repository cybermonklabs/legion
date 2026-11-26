package replay

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// The rates the load models rest on (spec R11): each model's signal in each range of 20 issues in
// flight that a window records, beside the model (LoadBand), and R11's test across two records of
// the rate each model rests on (CompareRecords): the tokens for each minute of an issue in flight,
// the kills for each hour of an issue in flight from a start the caller gives, and, for each
// repository whose queue has at least 50 exits from its head in each record, those exits for each
// hour in which the queue held a pull request. A model that misses this test gives no limit, as
// one that misses the daily test gives none.

// loadBandWidth is the width of a range of issues in flight that the rates by load are read in.
const loadBandWidth = 20

// acrossExitsFloor is the exits from the head of a repository's queue that R11's test across two
// records needs in each record to test that queue's rate.
const acrossExitsFloor = 50

// The reasons of a rate, or a model, that the test across two records cannot test.
const (
	reasonNoRate        = "the rate has no reading on one of the two records, which holds no record of its signal or no exposure to it"
	reasonNoTestedQueue = "no repository's queue has 50 exits from its head in each record"
)

// LoadBand is a model's signal in one range [From, To) of the issues in flight that the window
// records: for the merge queue's model, of one repository's queue; the hours the window spent in
// that range where it holds the model's record; the count of the signal there (the tokens model's
// uncached input tokens, the kill model's kills, the merge queue's exits from its head) and its
// exposure (the issue-minutes in flight, the issue-hours in flight, the hours in which the queue
// held a pull request), with the rate of the first over the second, none where the exposure is
// none. The tokens model reads a minute's range from its average issues in flight, the others the
// issues in flight at each moment.
type LoadBand struct {
	Repository string   `json:"repository,omitempty"`
	From       int      `json:"from"`
	To         int      `json:"to"`
	Hours      float64  `json:"hours"`
	Count      float64  `json:"count"`
	Exposure   float64  `json:"exposure"`
	Rate       *float64 `json:"rate"`
}

// bands is a table of LoadBand by repository and range.
type bands map[bandKey]*LoadBand

// bandKey is a LoadBand's repository and the start of its range.
type bandKey struct {
	repo string
	from int
}

// at is the band of repo of the range that holds issues in flight.
func (b bands) at(repo string, issues float64) *LoadBand {
	from := int(math.Floor(issues/loadBandWidth)) * loadBandWidth
	key := bandKey{repo: repo, from: from}
	if b[key] == nil {
		b[key] = &LoadBand{Repository: repo, From: from, To: from + loadBandWidth}
	}
	return b[key]
}

// list is b's bands in the order of their repository and range, each with its rate.
func (b bands) list() []LoadBand {
	list := make([]LoadBand, 0, len(b))
	for _, key := range slices.SortedFunc(maps.Keys(b), func(x, y bandKey) int {
		if c := strings.Compare(x.repo, y.repo); c != 0 {
			return c
		}
		return x.from - y.from
	}) {
		band := *b[key]
		if band.Exposure > 0 {
			rate := band.Count / band.Exposure
			band.Rate = &rate
		}
		list = append(list, band)
	}
	return list
}

// tokenMinutes calls each for each minute of the provider's record in the window's span, in order,
// with its line and its average issues in flight over the whole minute in the record.
func (r *modelRecord) tokenMinutes(each func(line ProviderMinute, issues float64)) {
	for start := r.w.From.Truncate(time.Minute); start.Before(r.w.To); start = start.Add(time.Minute) {
		if line, held := r.minutes[start.Unix()]; held {
			each(line, r.flight.hours(start, start.Add(time.Minute))*60)
		}
	}
}

// tokenBands is the tokens model's rates by load: each minute of the provider's record in the span
// in the range of its average issues in flight.
func (r *modelRecord) tokenBands() []LoadBand {
	b := bands{}
	r.tokenMinutes(func(line ProviderMinute, issues float64) {
		band := b.at("", issues)
		band.Hours += time.Minute.Hours()
		band.Count += float64(line.Uncached)
		band.Exposure += issues
	})
	return b.list()
}

// killBands is the kill model's rates by load over record, the parts of the span in which the
// window holds the record of the kills: each kill in the range of the issues in flight at its
// moment.
func (r *modelRecord) killBands(record []Span) []LoadBand {
	b := bands{}
	recorded, _ := r.killMoments()
	for _, part := range record {
		r.flight.pieces(part.From, part.To, func(from, to time.Time, count int) {
			band := b.at("", float64(count))
			band.Hours += to.Sub(from).Hours()
			band.Exposure += float64(count) * to.Sub(from).Hours()
		})
		for _, kill := range momentsWithin(recorded, part) {
			b.at("", float64(r.flight.at(kill))).Count++
		}
	}
	return b.list()
}

// queueBands is the merge queue's model's rates by load over record, the parts of the span in which
// the window holds the queue's record, for each repository: each exit from the head of its queue
// in the range of the issues in flight at its moment, and the hours in which its queue held a pull
// request in the range of the issues in flight then.
func (r *modelRecord) queueBands(record []Span) []LoadBand {
	b := bands{}
	for _, q := range r.queues {
		for _, part := range record {
			r.flight.pieces(part.From, part.To, func(from, to time.Time, count int) {
				band := b.at(q.repo, float64(count))
				band.Hours += to.Sub(from).Hours()
				for _, busy := range q.busy {
					until := busy.To
					if until.IsZero() {
						until = to
					}
					if start, end := maxTime(busy.From, from), minTime(until, to); end.After(start) {
						band.Exposure += end.Sub(start).Hours()
					}
				}
			})
			for _, pull := range q.pulls {
				if !pull.early && !pull.left.IsZero() && part.Holds(pull.left) {
					b.at(q.repo, float64(r.flight.at(pull.left))).Count++
				}
			}
		}
	}
	return b.list()
}

// RateReading is the rate a load model rests on, read on one record: its count and its exposure,
// with the rate of the first over the second, none where the exposure is none.
type RateReading struct {
	Count    float64  `json:"count"`
	Exposure float64  `json:"exposure"`
	Rate     *float64 `json:"rate"`
}

// rateOf is count over exposure as a RateReading.
func rateOf(count, exposure float64) RateReading {
	reading := RateReading{Count: count, Exposure: exposure}
	if exposure > 0 {
		rate := count / exposure
		reading.Rate = &rate
	}
	return reading
}

// tokensRate is the rate the tokens model rests on: the uncached input tokens of the provider's
// record in the span for each minute of an issue in flight, over its minutes with an issue in
// flight.
func (r *modelRecord) tokensRate() RateReading {
	var tokens, issueMinutes float64
	r.tokenMinutes(func(line ProviderMinute, issues float64) {
		if issues > 0 {
			tokens, issueMinutes = tokens+float64(line.Uncached), issueMinutes+issues
		}
	})
	return rateOf(tokens, issueMinutes)
}

// killsRate is the rate the kill model rests on, counted from from: the kills where the window
// holds the record of the kills in its span from from on, for each hour of an issue in flight
// there.
func (r *modelRecord) killsRate(from time.Time) RateReading {
	recorded, _ := r.killMoments()
	kills, hours := 0, 0.0
	for _, part := range clipSpans(r.w.Signals[SignalKills], Span{From: maxTime(r.w.From, from), To: r.w.To}) {
		kills += countWithin(recorded, part)
		hours += r.flight.hours(part.From, part.To)
	}
	return rateOf(float64(kills), hours)
}

// exitRate is the rate the merge queue's model rests on for q: its exits from the head for each
// hour in which it held a pull request.
func (q *recordedQueue) exitRate() RateReading {
	return rateOf(float64(q.headExits), q.busyHours)
}

// AcrossRecords is R11's test across two records (CompareRecords): the other record's project and
// span, the moment the kills are counted from on each record, each rate tested with its reading on
// each record, and each repository whose queue has no test.
type AcrossRecords struct {
	Project   string          `json:"project"`
	From      time.Time       `json:"from"`
	To        time.Time       `json:"to"`
	KillsFrom time.Time       `json:"killsFrom"`
	Rates     []AcrossRate    `json:"rates"`
	Untested  []UntestedQueue `json:"untested"`
}

// AcrossRate is one rate a load model rests on under R11's test across two records: the model, the
// rate's name and, for the merge queue's model, the repository; its reading on the window and on
// the other record; the ratio of the first to the second; and the test, which passes where the
// window's rate is within 30% of the other record's (gapThirty), with its reason where it gives no
// test.
type AcrossRate struct {
	Model      string      `json:"model"`
	Rate       string      `json:"rate"`
	Repository string      `json:"repository,omitempty"`
	Window     RateReading `json:"window"`
	Against    RateReading `json:"against"`
	Ratio      *float64    `json:"ratio"`
	Verdict    string      `json:"verdict"`
	Reason     string      `json:"reason,omitempty"`
}

// UntestedQueue is a repository whose queue has fewer than 50 exits from its head in one of the
// two records, so R11's test across records does not test its rate: its exits on each.
type UntestedQueue struct {
	Repository   string `json:"repository"`
	WindowExits  int    `json:"windowExits"`
	AgainstExits int    `json:"againstExits"`
}

// CompareRecords is window's models with R11's test across two records against the models of
// against, the kills counted from killsFrom on each record: each model's rate on window within
// 30% of the same rate on against. A model passes where each rate it rests on that the test tests
// passes, fails where one fails, and gives no test where none is tested; it gives a limit only
// where it passes this test and R11's daily test. It refuses models that hold no window's record.
func CompareRecords(window, against LoadModels, killsFrom time.Time) (LoadModels, error) {
	if window.fit == nil || against.fit == nil {
		return LoadModels{}, errNoModelRecord
	}
	a, b := window.fit, against.fit
	across := &AcrossRecords{Project: against.Project, From: against.From, To: against.To, KillsFrom: killsFrom, Rates: []AcrossRate{},
		Untested: []UntestedQueue{}}
	out := window
	out.Models, out.Across = slices.Clone(window.Models), across
	for i := range out.Models {
		model := &out.Models[i]
		var rates []AcrossRate
		switch model.Name {
		case ModelTokens:
			rates = []AcrossRate{acrossRate(ModelTokens, paramTokensRate, "", a.tokensRate(), b.tokensRate())}
		case ModelMemory:
			rates = []AcrossRate{acrossRate(ModelMemory, paramKillRate, "", a.killsRate(killsFrom), b.killsRate(killsFrom))}
		case ModelQueue:
			rates, across.Untested = queueRates(a, b)
		}
		across.Rates = append(across.Rates, rates...)
		model.judgeAcross(rates)
	}
	return out, nil
}

// acrossRate is the test of one rate, read as window on the window and as against on the other
// record.
func acrossRate(model, name, repository string, window, against RateReading) AcrossRate {
	rate := AcrossRate{Model: model, Rate: name, Repository: repository, Window: window, Against: against}
	if window.Rate == nil || against.Rate == nil {
		rate.Verdict, rate.Reason = VerdictNoTest, reasonNoRate
		return rate
	}
	if *against.Rate != 0 {
		ratio := *window.Rate / *against.Rate
		rate.Ratio = &ratio
	}
	rate.Verdict = passWord(gapThirty.Within(*against.Rate, *window.Rate))
	return rate
}

// queueRates is the test of each repository's rate of exits from the head, in the order of its
// name, for each repository whose queue has at least acrossExitsFloor exits from its head in each
// record, and each other repository of either record, untested.
func queueRates(window, against *modelRecord) ([]AcrossRate, []UntestedQueue) {
	byKey := func(r *modelRecord) map[string]*recordedQueue {
		queues := make(map[string]*recordedQueue, len(r.queues))
		for _, q := range r.queues {
			queues[q.key] = q
		}
		return queues
	}
	ours, theirs := byKey(window), byKey(against)
	keys := slices.Sorted(maps.Keys(ours))
	for key := range theirs {
		if ours[key] == nil {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	rates, untested := []AcrossRate{}, []UntestedQueue{}
	for _, key := range keys {
		q, other := ours[key], theirs[key]
		name, exits, otherExits := key, 0, 0
		if other != nil {
			name, otherExits = other.repo, other.headExits
		}
		if q != nil {
			name, exits = q.repo, q.headExits
		}
		if exits < acrossExitsFloor || otherExits < acrossExitsFloor {
			untested = append(untested, UntestedQueue{Repository: name, WindowExits: exits, AgainstExits: otherExits})
			continue
		}
		rates = append(rates, acrossRate(ModelQueue, paramExitRate, name, q.exitRate(), other.exitRate()))
	}
	return rates, untested
}

// judgeAcross gives m its verdict under R11's test across two records from the tests of the rates
// it rests on (CompareRecords), and settles whether it gives a limit.
func (m *LoadModel) judgeAcross(rates []AcrossRate) {
	m.Across, m.AcrossReason = VerdictNoTest, reasonNoTestedQueue
	for _, rate := range rates {
		switch {
		case rate.Verdict == VerdictFails:
			m.Across, m.AcrossReason = VerdictFails, ""
		case rate.Verdict == VerdictPasses && m.Across != VerdictFails:
			m.Across, m.AcrossReason = VerdictPasses, ""
		case rate.Verdict == VerdictNoTest && m.Across == VerdictNoTest:
			m.AcrossReason = rate.Reason
		}
	}
	m.settle()
}

// write prints a's section (LoadModels.Write): the other record, the start of the kills, each rate
// on each record with their ratio and the test, and each repository with no test.
func (a AcrossRecords) write(p *printer) {
	p.line("")
	p.line("## R11's test across two records")
	p.line("")
	p.line("The other record is the window of %s from %s to %s, and the kills are counted from %s on each record.",
		a.Project, stamp(a.From), stamp(a.To), stamp(a.KillsFrom))
	p.line("A rate passes where its reading on this window is within %s of its reading on the other record.", gapThirty)
	p.line("")
	p.line("| model | rate | repository | this window | the other record | ratio | test |")
	p.line("| --- | --- | --- | --- | --- | --- | --- |")
	for _, rate := range a.Rates {
		repository, ratio, test := "–", "–", rate.Verdict
		if rate.Repository != "" {
			repository = rate.Repository
		}
		if rate.Ratio != nil {
			ratio = formatNumber(*rate.Ratio)
		}
		if rate.Reason != "" {
			test += ": " + rate.Reason
		}
		p.line("| %s | %s | %s | %s | %s | %s | %s |", rate.Model, rate.Rate, repository, rateCell(rate.Window), rateCell(rate.Against), ratio, test)
	}
	p.line("")
	if len(a.Untested) == 0 {
		p.line("No repository's queue is left untested.")
		return
	}
	p.line("The repositories whose queue has fewer than %d exits from its head in a record, which have no test:", acrossExitsFloor)
	p.line("")
	for _, queue := range a.Untested {
		p.line("- %s: %d exits from its head on this window, %d on the other record", queue.Repository, queue.WindowExits, queue.AgainstExits)
	}
}

// rateCell is reading as the test's table prints it: the rate, with its count over its exposure.
func rateCell(reading RateReading) string {
	if reading.Rate == nil {
		return fmt.Sprintf("– (%s over %s)", formatNumber(reading.Count), formatNumber(reading.Exposure))
	}
	return fmt.Sprintf("%s (%s over %s)", formatNumber(*reading.Rate), formatNumber(reading.Count), formatNumber(reading.Exposure))
}

// writeLoadBands prints model's rates by load (LoadModels.Write).
func writeLoadBands(p *printer, model LoadModel) {
	if len(model.Load) == 0 {
		p.line("The window records no range of issues in flight for this model.")
		return
	}
	p.line("The signal's rate in each range of %d issues in flight that the window records:", loadBandWidth)
	p.line("")
	head := map[string]string{
		ModelTokens: "| issues in flight | hours | uncached input tokens | issue-minutes in flight | tokens for each minute of an issue in flight |",
		ModelMemory: "| issues in flight | hours | kills | issue-hours in flight | kills for each hour of an issue in flight |",
		ModelQueue: "| repository | issues in flight | hours | exits from the head | hours in which the queue held a pull request | " +
			"exits for each such hour |",
	}[model.Name]
	p.line("%s", head)
	rule := "| --- | --- | --- | --- | --- |"
	if model.Name == ModelQueue {
		rule += " --- |"
	}
	p.line("%s", rule)
	for _, band := range model.Load {
		row := fmt.Sprintf("| [%d, %d) | %s | %s | %s | %s |", band.From, band.To, formatNumber(band.Hours), formatNumber(band.Count),
			formatNumber(band.Exposure), optionalNumber(band.Rate))
		if model.Name == ModelQueue {
			row = "| " + band.Repository + " " + row
		}
		p.line("%s", row)
	}
}
