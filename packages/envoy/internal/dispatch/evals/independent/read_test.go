package independent

import (
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// TestTheReadingReadsEachOfMainsTablesItNames checks R4: the Legion reading uses
// issues, events, handoffs, and legion_observations from Dispatch's schema.
func TestTheReadingReadsEachOfMainsTablesItNames(t *testing.T) {
	const deleteEvents = `delete from events e using issues i where e.issue_key = i.key and i.project_key = $1`
	for _, test := range []struct {
		name   string
		remove string
		check  func(t *testing.T, before, after Reading)
	}{
		{name: "whole history"},
		{
			name:   "issues",
			remove: `delete from issues where project_key = $1`,
			check: func(t *testing.T, before, after Reading) {
				if before.Issues == 0 || after.Issues != 0 {
					t.Errorf("finished issues before and after deleting issues = %d and %d, want a positive count then zero", before.Issues, after.Issues)
				}
			},
		},
		{
			name:   "events",
			remove: deleteEvents,
			check: func(t *testing.T, before, after Reading) {
				if before.Filings == 0 || after.Filings != 0 {
					t.Errorf("Legion filings before and after deleting events = %d and %d, want a positive count then zero", before.Filings, after.Filings)
				}
			},
		},
		{
			name:   "handoffs",
			remove: `delete from handoffs h using issues i where h.issue_key = i.key and i.project_key = $1`,
			check: func(t *testing.T, before, after Reading) {
				// One closed child has a handoff but no event from a Legion worker.
				if want := before.Issues - 1; after.Issues != want {
					t.Errorf("without handoffs the reading counts %d finished issues, want %d without the child whose only work was a handoff", after.Issues, want)
				}
			},
		},
		{
			name:   "legion_observations",
			remove: `delete from legion_observations where project = $1`,
			check: func(t *testing.T, _, after Reading) {
				if after.FirstClaimObservation != nil || after.FirstSlotObservation != nil {
					t.Errorf("without observations the reading still gives first claim %v and first slot %v, want neither", after.FirstClaimObservation, after.FirstSlotObservation)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			pool := storetest.Open(t).Pool
			now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
			fixture := legiontest.Seed(t, pool, legiontest.Spec{Trees: 1, Now: now})
			window := Window{From: now.AddDate(0, 0, -3), To: now}
			read := func() Reading {
				t.Helper()
				got, err := Read(ctx, pool, fixture.Project, window)
				if err != nil {
					t.Fatalf("read the Legion history: %v", err)
				}
				return got
			}
			whole := read()
			if whole.Issues != legiontest.FinishedPerTree {
				t.Errorf("the whole history has %d finished issues, want %d including the child with only a handoff", whole.Issues, legiontest.FinishedPerTree)
			}
			if len(whole.Measures) != 6 {
				t.Errorf("the whole history gives %d measures, want six", len(whole.Measures))
			}
			for _, slug := range []string{SlugWorkTime, SlugPhaseTime, SlugIdleTime, SlugSlotWait, SlugRework, SlugSplitting} {
				if _, found := whole.Measure(slug); !found {
					t.Errorf("the whole history gives no %s measure", slug)
				}
			}
			if whole.FirstClaimObservation == nil || whole.FirstSlotObservation == nil {
				t.Errorf("the whole history gives first claim %v and first slot %v, want both", whole.FirstClaimObservation, whole.FirstSlotObservation)
			}
			if test.remove == "" {
				return
			}

			before := whole
			if test.name == "issues" {
				// Events reference issues without a cascade. Read again once the
				// events are gone: handoffs alone still qualify the finished issues.
				// Deleting issues then cascades to handoffs.
				if _, err := pool.Exec(ctx, deleteEvents, fixture.Project); err != nil {
					t.Fatalf("delete the project's events before its issues: %v", err)
				}
				before = read()
			}
			if _, err := pool.Exec(ctx, test.remove, fixture.Project); err != nil {
				t.Fatalf("delete the project's rows from %s: %v", test.name, err)
			}
			test.check(t, before, read())
		})
	}
}
