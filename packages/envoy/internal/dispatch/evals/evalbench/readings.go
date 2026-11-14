package evalbench

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// INFRA's logged evals and their readings. The storage review that measured the registry's reads
// (dispatch://INFRA-979/artifact/review-storage-md, §9, as review-storage-measured-md §1 corrected
// it) spread 2,800,000 readings over 730 days, 20 evals and five row kinds, with the eval and the
// row kind drawn from separate counters so every eval holds every kind; here the 20 evals are
// INFRA's own, so one project's list read folds all of them.
//
// Each eval is registered through the register write, then its target changed by a second
// register write, so each has two versions and two series: `value`, its headline, and
// `baseline`, a control series folded by its last value. Its readings are written through the
// readings write, one batch an eval a day in day order, as an agent logging each day's results
// writes them, so the table is in measured_at order as a live one is and every invariant of the
// write holds as it does for any caller: superseded_at set on a corrected reading in the
// correction's transaction, and the eval's day rows (eval_days) kept by increment in that same
// transaction. A batch after the eval's first corrects one reading of the batch before it, so
// the store holds corrections of every eval through the whole history.

// RowKinds are the row kinds every eval's readings are spread over.
var RowKinds = []string{"day", "issue", "pull_request", "deploy", "job"}

// loggedEval is one of INFRA's logged evals: its slug, name, unit and direction, its headline
// series' fold, where its values sit and the target its second version sets.
type loggedEval struct {
	slug, name, unit, direction, agg string
	mean, spread, target             float64
}

// loggedEvals are INFRA's 20 logged evals.
var loggedEvals = []loggedEval{
	{"review-latency", "Review latency", "minutes", evals.DirectionLowerBetter, evals.AggQuantile, 45, 20, 60},
	{"build-minutes", "Build minutes", "minutes", evals.DirectionLowerBetter, evals.AggQuantile, 12, 4, 15},
	{"test-flake-rate", "Test flake rate", "ratio", evals.DirectionLowerBetter, evals.AggWeightedMean, 0.03, 0.01, 0.02},
	{"deploy-lead", "Deploy lead time", "minutes", evals.DirectionLowerBetter, evals.AggQuantile, 90, 40, 120},
	{"queue-depth", "Queue depth", "issues", evals.DirectionLowerBetter, evals.AggLast, 40, 10, 30},
	{"agent-cost", "Agent cost", "usd", evals.DirectionLowerBetter, evals.AggSum, 8, 3, 1500},
	{"retry-count", "Retries", "retries", evals.DirectionLowerBetter, evals.AggSum, 2, 1, 200},
	{"page-weight", "Page weight", "kb", evals.DirectionLowerBetter, evals.AggMax, 280, 40, 300},
	{"cold-start", "Cold start", "seconds", evals.DirectionLowerBetter, evals.AggQuantile, 4, 1.5, 5},
	{"merge-conflicts", "Merge conflicts", "conflicts", evals.DirectionLowerBetter, evals.AggSum, 1, 0.5, 100},
	{"spec-revisions", "Spec revisions", "revisions", evals.DirectionLowerBetter, evals.AggQuantile, 3, 1, 4},
	{"handoff-gap", "Handoff gap", "hours", evals.DirectionLowerBetter, evals.AggQuantile, 2, 1, 3},
	{"ci-queue", "CI queue wait", "minutes", evals.DirectionLowerBetter, evals.AggQuantile, 6, 3, 8},
	{"answer-quality", "Answer quality", "score", evals.DirectionHigherBetter, evals.AggWeightedMean, 0.8, 0.1, 0.85},
	{"coverage", "Coverage", "ratio", evals.DirectionHigherBetter, evals.AggLast, 0.75, 0.05, 0.8},
	{"doc-freshness", "Doc freshness", "days", evals.DirectionLowerBetter, evals.AggQuantile, 20, 8, 30},
	{"token-usage", "Token usage", "ktokens", evals.DirectionLowerBetter, evals.AggSum, 300, 90, 20000},
	{"incident-minutes", "Incident minutes", "minutes", evals.DirectionLowerBetter, evals.AggMax, 25, 15, 30},
	{"slot-utilisation", "Slot utilisation", "ratio", evals.DirectionHigherBetter, evals.AggWeightedMean, 0.7, 0.1, 0.8},
	{"rollback-time", "Rollback time", "minutes", evals.DirectionLowerBetter, evals.AggQuantile, 10, 5, 15},
}

// LoggedSlugs are the slugs of INFRA's logged evals, in registration order.
func LoggedSlugs() []string {
	slugs := make([]string, len(loggedEvals))
	for i, eval := range loggedEvals {
		slugs[i] = eval.slug
	}
	return slugs
}

// readingBody is one row of a readings write, as a caller sends it.
type readingBody struct {
	Series     string         `json:"series"`
	RowKind    string         `json:"row_kind"`
	RowKey     string         `json:"row_key"`
	Value      *float64       `json:"value"`
	Reason     string         `json:"value_absent_reason,omitempty"`
	Unit       string         `json:"unit,omitempty"`
	N          *int64         `json:"n,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	MeasuredAt time.Time      `json:"measured_at"`
	Source     string         `json:"source"`
	Note       string         `json:"note,omitempty"`
	Replaces   string         `json:"replaces,omitempty"`
}

// writeReadings registers INFRA's logged evals and writes their readings.
func writeReadings(ctx context.Context, database *store.Store, spec Spec, history infraHistory, logf func(string, ...any)) error {
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker, Settle: time.Hour})
	defer func() {
		if err := documents.Shutdown(context.WithoutCancel(ctx)); err != nil {
			logf("the document service did not stop cleanly: %v", err)
		}
	}()
	registry := evals.New(database, documents, broker)
	for _, eval := range loggedEvals {
		if err := register(ctx, registry, eval); err != nil {
			return err
		}
	}
	rng := newRand(spec.Seed, "readings")
	total := spec.scaled(readings, len(loggedEvals)*readingDays)
	batches := len(loggedEvals) * readingDays
	start := spec.Now.Add(-readingDays * 24 * time.Hour)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	// The runs the history holds at this scale and rate, whose ids a deploy or job reading names.
	deploys, checks := spec.dailyRuns()
	runs := int64(runDays * (deploys + checks))
	previous := make([][]evals.Reading, len(loggedEvals))
	written := 0
	for day := range readingDays {
		from := start.AddDate(0, 0, day)
		for e, eval := range loggedEvals {
			batch := day*len(loggedEvals) + e
			count := total / batches
			if batch < total%batches {
				count++
			}
			rows := make([]json.RawMessage, 0, count)
			// One reading of the batch before corrects it, which leaves that one superseded.
			if len(previous[e]) > 0 && count > 1 {
				replaced := previous[e][rng.IntN(len(previous[e]))]
				if replaced.Value != nil {
					value := *replaced.Value * (0.9 + 0.2*rng.Float64())
					row, err := json.Marshal(readingBody{Series: replaced.Series, RowKind: replaced.RowKind, RowKey: replaced.RowKey,
						Value: &value, Unit: replaced.Unit, N: replaced.N, Attributes: replaced.Attributes,
						MeasuredAt: replaced.MeasuredAt, Source: "evalbench", Note: "correction", Replaces: replaced.ID})
					if err != nil {
						return fmt.Errorf("encode a correction of %s: %w", eval.slug, err)
					}
					rows = append(rows, row)
				}
			}
			for index := len(rows); index < count; index++ {
				row, err := json.Marshal(drawReading(rng, eval, history, runs, from, index, count))
				if err != nil {
					return fmt.Errorf("encode a reading of %s: %w", eval.slug, err)
				}
				rows = append(rows, row)
			}
			stored, err := registry.WriteReadings(ctx, evals.Batch{Project: Project, Slug: eval.slug, Rows: rows, Caller: actor, BuiltBy: spec.BuiltBy})
			if err != nil {
				return fmt.Errorf("write the readings of %s on %s: %w", eval.slug, from.Format(time.DateOnly), err)
			}
			previous[e] = stored.Readings
			written += len(stored.Readings)
		}
		if (day+1)%73 == 0 {
			logf("readings: %d of %d days, %d readings", day+1, readingDays, written)
		}
	}
	return nil
}

// register writes the eval's first version, with its two series and its method, and then its
// second, which sets its target.
func register(ctx context.Context, registry *evals.Store, eval loggedEval) error {
	first := evals.Registration{
		Project: Project, Slug: eval.slug, Caller: actor,
		Name:      evals.Set(eval.name),
		Question:  evals.Set("How does " + eval.name + " move from one window to the next?"),
		Decision:  evals.Set("A window past the target sends the owner to look at its rows."),
		Unit:      evals.Set(eval.unit),
		Direction: evals.Set(eval.direction),
		DocText:   evals.Set("An agent logs " + eval.name + " each day; the number is the fold of the window's readings."),
		Series: []evals.SeriesChange{
			{Series: "value", Unit: evals.Set(eval.unit), Role: evals.Set(evals.RoleHeadline), IsPrimary: evals.Set(true), Listed: evals.Set(true), Agg: evals.Set(eval.agg)},
			{Series: "baseline", Unit: evals.Set(eval.unit), Role: evals.Set(evals.RoleControl), IsPrimary: evals.Set(false), Listed: evals.Set(true), Agg: evals.Set(evals.AggLast)},
		},
	}
	if _, err := registry.Register(ctx, first); err != nil {
		return fmt.Errorf("register %s: %w", eval.slug, err)
	}
	target := eval.target
	if _, err := registry.Register(ctx, evals.Registration{
		Project: Project, Slug: eval.slug, Caller: actor,
		Target: evals.Set(&target), TargetInclusive: evals.Set(true),
	}); err != nil {
		return fmt.Errorf("set the target of %s: %w", eval.slug, err)
	}
	return nil
}

// drawReading is reading index of count of one eval's batch on the day from: its row kind turns
// with the index, its row key names a row of that kind (an INFRA issue, a pull request, one of the
// history's runs, whose ids are firstRunID up to firstRunID+runs, a job of one of them, or the
// day), its time falls in the day in index order, and one in a hundred has no value and says why.
func drawReading(rng *rand.Rand, eval loggedEval, history infraHistory, runs int64, from time.Time, index, count int) readingBody {
	kind := RowKinds[(index+from.YearDay())%len(RowKinds)]
	body := readingBody{Series: "value", RowKind: kind, Unit: eval.unit, Source: "evalbench",
		MeasuredAt: from.Add(time.Duration((float64(index) + rng.Float64()) / float64(count) * float64(24*time.Hour)))}
	if rng.Float64() < 0.1 {
		body.Series = "baseline"
	}
	repo := Repos[rng.IntN(len(Repos))]
	switch kind {
	case "day":
		body.RowKey = from.Format(time.DateOnly)
	case "issue":
		issue := history.issues[rng.IntN(len(history.issues))]
		body.RowKey = issue.key
		if issue.repo != "" {
			body.Attributes = map[string]any{"repo": issue.repo}
		}
	case "pull_request":
		body.RowKey = repo + "#" + strconv.Itoa(1+rng.IntN(4000))
		body.Attributes = map[string]any{"repo": repo}
	case "deploy":
		body.RowKey = strconv.FormatInt(firstRunID+rng.Int64N(runs), 10)
		body.Attributes = map[string]any{"repo": Repos[0]}
	default:
		body.RowKey = strconv.FormatInt(firstRunID+rng.Int64N(runs), 10) + "/" + checkStages[rng.IntN(len(checkStages))].name
		body.Attributes = map[string]any{"repo": Repos[0]}
	}
	if rng.Float64() < 0.01 {
		body.Reason = "not_measured"
		return body
	}
	value := math.Max(0, eval.mean+eval.spread*rng.NormFloat64())
	body.Value = &value
	if rng.Float64() < 0.3 {
		body.N = new(int64(1 + rng.IntN(50)))
	}
	return body
}
