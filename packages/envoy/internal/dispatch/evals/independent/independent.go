// Package independent is a second reading of the six Legion measures of the eval registry, written
// from the words of dispatch://INFRA-2916's R6 and D3, and from dispatch://INFRA-1077's "The four
// Legion metrics" and "Rule details", whose rules the other four measures keep, and not from the
// code that serves them:
//
//	The Legion measures are registry rows over one computation: work time to merge, time per
//	phase by role, idle time by role, slot wait, rework, and splitting and follow-ups. Each
//	measure counts the project's issues that finished in the window. Each measure is three series,
//	cut by the stored root flag: all issues, roots alone and children alone.
//
// It is the second side of R6's check (k6), which compares every measure and every one of its
// series, as GET /api/v1/projects/{key}/evals answers them, against a reading of the
// requirement. A check whose two sides are one function can never fail, so this package imports
// neither legion/metrics, legionfacts nor evals (imports_test.go reads its imports): it reads
// `issues`, `events`, `handoffs` and `legion_observations` itself, with statements of its own, and
// works each value out from the words. Where this reading and the eval routes disagree, one of
// them is wrong and the check has found it. legionfacts/independent is the same kind of reading of
// INFRA-2915's per-issue facts, and the rules this one shares with it are read from the same words.
//
// It takes the project and the window as arguments and states both beside every value it reports
// (Reading), because a check whose two sides read different windows proves nothing. cmd prints the
// reading as JSON, which is what a tester re-runs.
package independent

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier is the database connection this reading needs: one that answers a query. A pool, a
// connection or a transaction is one.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Window is the stretch of time one reading measures: the issues whose close falls in [From, To)
// are the ones it counts, and an issue still open runs to To. The eval routes read whole UTC days,
// so a window two sides compare over starts and ends at UTC midnights.
type Window struct {
	From time.Time
	To   time.Time
}

// String is the window as a reader of a reading compares it: two moments, each in UTC.
func (w Window) String() string {
	return w.From.UTC().Format(time.RFC3339) + " to " + w.To.UTC().Format(time.RFC3339)
}

// Reading is one run of this reading: the project and the window it covered, how many issues
// finished in the window and how many filings the project's Legion sessions made in it, the
// moments the project's claim and slot observations begin, and the six measures.
type Reading struct {
	Project               string     `json:"project"`
	WindowFrom            string     `json:"window_from"`
	WindowTo              string     `json:"window_to"`
	Issues                int        `json:"issues"`
	Filings               int        `json:"filings"`
	FirstClaimObservation *time.Time `json:"first_claim_observation"`
	FirstSlotObservation  *time.Time `json:"first_slot_observation"`
	Measures              []Measure  `json:"measures"`
}

// Read answers the six measures of project over window.
func Read(ctx context.Context, db Querier, project string, window Window) (Reading, error) {
	window = Window{From: window.From.UTC(), To: window.To.UTC()}
	held, err := readPopulation(ctx, db, project, window)
	if err != nil {
		return Reading{}, err
	}
	measured := newReading(held)
	m := measuring{window: window, firstClaim: held.firstClaim, firstSlot: held.firstSlot}
	for _, key := range held.keys {
		one := held.issues[key]
		// "An issue finished when its last move to `done` (its close) falls in the window, it is
		// still `done`, and it has Legion phase work": a reopen clears the close.
		if one.closed == nil || one.status != "done" || !one.worked() {
			continue
		}
		m.closes = append(m.closes, *one.closed)
		if !one.closed.Before(window.From) && one.closed.Before(window.To) {
			m.finished = append(m.finished, measured.valuesOf(key))
		}
	}
	slices.SortFunc(m.closes, func(a, b time.Time) int { return a.Compare(b) })
	for _, filed := range held.filings {
		if m.firstFiling == nil || filed.at.Before(*m.firstFiling) {
			m.firstFiling = new(filed.at)
		}
		if filed.at.Before(window.From) || !filed.at.Before(window.To) {
			continue
		}
		m.filed = append(m.filed, classified{class: measured.classOf(filed), root: measured.rootOf(filed.key) == filed.key})
	}
	return Reading{
		Project:               project,
		WindowFrom:            window.From.Format(time.RFC3339),
		WindowTo:              window.To.Format(time.RFC3339),
		Issues:                len(m.finished),
		Filings:               len(m.filed),
		FirstClaimObservation: held.firstClaim,
		FirstSlotObservation:  held.firstSlot,
		Measures:              m.measures(),
	}, nil
}

// Measure is the reading's measure slug, found false for a slug it does not hold.
func (r Reading) Measure(slug string) (Measure, bool) {
	for _, one := range r.Measures {
		if one.Slug == slug {
			return one, true
		}
	}
	return Measure{}, false
}

// Series is the measure's statistics of one series as the eval routes name it (SeriesName), found
// false for a series the measure does not state.
func (m Measure) Series(name string) (Statistics, bool) {
	for _, cut := range m.Cuts {
		if stats, held := cut.Series[name]; held {
			return stats, true
		}
	}
	return nil, false
}
