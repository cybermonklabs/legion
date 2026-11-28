package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// TestTheCommandPrintsEveryIssueAndCountsThoseReadByAPlanOfOtherSteps is the command's own run
// before k7 runs it against production (k28): over the facts' own history with one tree whose
// issues ran a plan of the steps `implementer reviewer acceptance:owner` (legiontest.Spec's
// PlannedTree), the report names the project and the moment it was given, holds one entry for
// every issue of the history with Legion phase work and for no other, and counts as read by a plan
// whose steps are not the default steps exactly the issues of that tree.
func TestTheCommandPrintsEveryIssueAndCountsThoseReadByAPlanOfOtherSteps(t *testing.T) {
	database := storetest.Open(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: "LEGION", Now: now, PlannedTree: true})

	var out bytes.Buffer
	if err := run(t.Context(), &out, fixture.Project, database.Pool.Config().ConnString(), now.Format(time.RFC3339)); err != nil {
		t.Fatalf("run the command: %v", err)
	}
	var printed report
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil {
		t.Fatalf("read the report the command printed as JSON: %v", err)
	}
	if printed.Project != fixture.Project || !printed.Moment.Equal(now) {
		t.Errorf("the report is of %s at %s, want %s at %s", printed.Project, printed.Moment.Format(time.RFC3339),
			fixture.Project, now.Format(time.RFC3339))
	}
	entries := map[string]bool{}
	for _, entry := range printed.Facts {
		entries[entry.IssueKey] = true
	}
	for _, key := range fixture.PhaseWork {
		if !entries[key] {
			t.Errorf("%s carries Legion phase work and has no entry in the report", key)
		}
	}
	if printed.Issues != len(fixture.PhaseWork) || len(printed.Facts) != len(fixture.PhaseWork) || len(entries) != len(fixture.PhaseWork) {
		t.Errorf("the report counts %d issues and holds %d entries of %d issues, want one entry for each of the %d with Legion phase work",
			printed.Issues, len(printed.Facts), len(entries), len(fixture.PhaseWork))
	}
	if printed.ReadByOtherSteps != len(fixture.Planned) {
		t.Errorf("the report counts %d issues read by a plan whose steps are not the default steps, want the %d of the tree that ran one",
			printed.ReadByOtherSteps, len(fixture.Planned))
	}
	t.Logf("the report of %s at %s holds %d issues, %d of them read by a plan whose steps are not the default steps; the history holds %d with Legion phase work, %d of them in the tree that ran such a plan",
		printed.Project, printed.Moment.Format(time.RFC3339), printed.Issues, printed.ReadByOtherSteps,
		len(fixture.PhaseWork), len(fixture.Planned))
}
