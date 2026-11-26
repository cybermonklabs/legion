package evals

import (
	"cmp"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// TestTheLegionDayRowsOfTheStoredRowsAreTheComputationsOnBothSidesOfAMidnight is condition (c) of
// R21's property: the Legion day rows built from the stored rows (ReadLegionDay, which closes each
// row at its day's end) equal the day rows built from the computation's facts read at a clock
// time a minute before and a minute after the first UTC midnight past the history's end
// (metrics.ProjectFactsClosed, grown to that clock time), for the last two completed days with
// closes. The filings come from the same population's events, not the stored filings table.
// That the facts so grown are the computation run at that clock time is metrics' own test
// (TestFactsClosedAtAnyMomentAreTheComputationAtThatMoment).
//
// A Legion day row counts an issue on the day it closes alone, so an open issue reaches no day
// row: the open issues' facts grow across the midnight, and neither day's rows move with them.
// The test holds that too: every issue a day counts is closed, and the open issues the
// computation answers are counted on no day.
func TestTheLegionDayRowsOfTheStoredRowsAreTheComputationsOnBothSidesOfAMidnight(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx, pool := t.Context(), r.database.Pool
	now := time.Date(2026, 11, 11, 21, 0, 0, 0, time.UTC)
	fixture := legiontest.Seed(t, pool, legiontest.Spec{
		Project: "LEGION", Trees: 6, Now: now, Queued: true, Parked: true, Merged: true, Filings: true,
	})
	project := fixture.Project
	rows, filings, err := legionfacts.ComputeProject(ctx, pool, project, now)
	if err != nil {
		t.Fatalf("compute the facts of %s: %v", project, err)
	}
	if _, err := legionfacts.WriteProject(ctx, pool, legionfacts.ProjectWrite{Project: project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts of %s: %v", project, err)
	}
	if err := legionfacts.WriteFilings(ctx, pool, legionfacts.FilingsWrite{Project: project, Filings: filings, Whole: true}); err != nil {
		t.Fatalf("write the filings of %s: %v", project, err)
	}
	since, err := r.registry.ReadLegionSince(ctx, project)
	if err != nil {
		t.Fatalf("read the first observations of %s: %v", project, err)
	}

	midnight := measures.Midnight(now).AddDate(0, 0, 1)
	days := []time.Time{midnight.AddDate(0, 0, -2), midnight.AddDate(0, 0, -1)}
	compared := 0
	for _, moment := range []time.Time{midnight.Add(-time.Minute), midnight.Add(time.Minute)} {
		population, err := metrics.ReadProject(ctx, pool, project, moment)
		if err != nil {
			t.Fatalf("read the population at %s: %v", moment, err)
		}
		facts := map[string]metrics.IssueFacts{}
		for key, closed := range metrics.ProjectFactsClosed(population, nil) {
			facts[key] = closed.FactsAt(population.To)
		}
		filings := metrics.ProjectFilings(population)
		open := 0
		for _, fact := range facts {
			if fact.ClosedAt == nil {
				open++
			}
		}
		if open == 0 {
			t.Fatalf("the computation at %s answers no open issue, so nothing grows across the midnight: this test examined nothing", moment)
		}
		for _, day := range days {
			stored, err := r.registry.ReadLegionDay(ctx, project, day)
			if err != nil {
				t.Fatalf("read the Legion day %s: %v", day, err)
			}
			for _, row := range stored.closed {
				if row.ClosedAt == nil {
					t.Errorf("the stored rows of %s count the open issue %s", day.Format(time.DateOnly), row.Issue)
				}
			}
			computed := LegionDay{Project: project, Day: day}
			var keys []string
			for _, filing := range filings {
				if measures.Midnight(filing.At).Equal(day) {
					computed.filed = append(computed.filed, legionfacts.Filing{Project: project, IssueFiling: filing})
					keys = append(keys, filing.Key)
				}
			}
			for key, fact := range facts {
				if fact.ClosedAt != nil && !fact.ClosedAt.Before(day) && fact.ClosedAt.Before(day.AddDate(0, 0, 1)) {
					computed.closed = append(computed.closed, factsRow(project, key, fact))
					keys = append(keys, key)
				}
			}
			if len(computed.closed) == 0 || len(computed.filed) == 0 {
				t.Fatalf("%s at %s: fixture has %d closes and %d filings; each comparison needs both", day, moment, len(computed.closed), len(computed.filed))
			}
			computed.repos, err = legionfacts.ReadRepos(ctx, pool, keys)
			if err != nil {
				t.Fatalf("resolve repositories for computed input on %s: %v", day, err)
			}
			slices.SortFunc(computed.closed, func(a, b legionfacts.Row) int {
				if order := b.ClosedAt.Compare(*a.ClosedAt); order != 0 {
					return order
				}
				return cmp.Compare(a.Issue, b.Issue)
			})
			fromStored, fromComputed := stored.Rows(since), computed.Rows(since)
			if !reflect.DeepEqual(fromStored, fromComputed) {
				t.Errorf("at %s the Legion rows of %s built from the stored rows differ from those built from the computation:\n\tstored   %+v\n\tcomputed %+v",
					moment.UTC(), day.Format(time.DateOnly), fromStored, fromComputed)
			}
			compared++
			t.Logf("at %s, %s: %d issues counted, %d rows, the computation answering %d open issues that reach no day",
				moment.UTC(), day.Format(time.DateOnly), len(fromStored.Members), len(fromStored.Rows), open)
		}
	}
	t.Logf("%d nonempty day comparisons on both sides of %s", compared, midnight.UTC())
}

// factsRow is the computation's facts of one issue as the stored row holds them, which embeds
// them (legionfacts.Row).
func factsRow(project, key string, fact metrics.IssueFacts) legionfacts.Row {
	return legionfacts.Row{Project: project, Issue: key, ClosedFacts: metrics.ClosedFacts{IssueFacts: fact}}
}
