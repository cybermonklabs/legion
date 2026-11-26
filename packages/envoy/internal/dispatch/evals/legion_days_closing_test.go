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
// (metrics.ProjectFactsClosed, grown to that clock time), with no input in between, for the day
// before that midnight and the day after it. That the facts so grown are the computation run at
// that clock time is metrics' own test (TestFactsClosedAtAnyMomentAreTheComputationAtThatMoment).
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
	days := []time.Time{midnight.AddDate(0, 0, -1), midnight}
	counted, compared := 0, 0
	for _, moment := range []time.Time{midnight.Add(-time.Minute), midnight.Add(time.Minute)} {
		population, err := metrics.ReadProject(ctx, pool, project, moment)
		if err != nil {
			t.Fatalf("read the population at %s: %v", moment, err)
		}
		facts := map[string]metrics.IssueFacts{}
		for key, closed := range metrics.ProjectFactsClosed(population, nil) {
			facts[key] = closed.FactsAt(population.To)
		}
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
			computed := LegionDay{Project: project, Day: stored.Day, filed: stored.filed, repos: stored.repos}
			for key, fact := range facts {
				if fact.ClosedAt != nil && !fact.ClosedAt.Before(day) && fact.ClosedAt.Before(day.AddDate(0, 0, 1)) {
					computed.closed = append(computed.closed, factsRow(project, key, fact))
				}
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
			counted += len(fromStored.Members)
			t.Logf("at %s, %s: %d issues counted, %d rows, the computation answering %d open issues that reach no day",
				moment.UTC(), day.Format(time.DateOnly), len(fromStored.Members), len(fromStored.Rows), open)
		}
	}
	if counted == 0 {
		t.Fatalf("no day of %v counts an issue, so the rows compared are empty: this test examined nothing", days)
	}
	t.Logf("%d days compared on both sides of %s, %d counted issues among them", compared, midnight.UTC(), counted)
}

// factsRow is the computation's facts of one issue as the stored row holds them, which embeds
// them (legionfacts.Row).
func factsRow(project, key string, fact metrics.IssueFacts) legionfacts.Row {
	return legionfacts.Row{Project: project, Issue: key, ClosedFacts: metrics.ClosedFacts{IssueFacts: fact}}
}
