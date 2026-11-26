package daemon

import (
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The fences of a start are one judgment (startFenceOf), which the outbox's run of the row logs from and
// the judgment again under the machine's lock reads (stepStands), so neither can hold a fence the
// other lacks. It names the first fence a row fails, in the outbox's order: the generation, then
// the phase and the step's role (record.Issue.HoldsQueued), then the entry. It reads the issue's
// phases rows for the entry fence alone, so a row that fails an earlier fence, a start of no entry
// and a root architect's start, which names no phase, read none; a failed read is the row's error.
func TestAStartRowNamesTheFirstFenceItFailsAndReadsThePhasesRowsForTheEntryAlone(t *testing.T) {
	t.Parallel()
	checking := record.Issue{Key: "LEGION-208", Phase: phase.Step, Generation: 2, Running: []record.RunningStep{
		{Task: plan.DefaultTask, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 1}, Role: "checker", Phase: phase.Step}}}
	rows := []record.PhaseRow{{Issue: "LEGION-208", Role: "checker", Entry: 5}}
	for _, tc := range []struct {
		name      string
		issue     record.Issue
		payload   record.SuperviseRequest
		want      startFence
		readsRows bool
	}{
		{"a row of an earlier generation", checking,
			record.SuperviseRequest{Op: "start", Role: "checker", Phase: phase.Step, Entry: 5, Generation: 1}, fenceGeneration, false},
		{"a start of a phase the issue left", checking,
			record.SuperviseRequest{Op: "start", Role: claim.RoleTester, Phase: phase.Testing, Entry: 5, Generation: 2}, fenceStep, false},
		{"a start of another step's role at the phase step", checking,
			record.SuperviseRequest{Op: "start", Role: "linter", Phase: phase.Step, Entry: 5, Generation: 2}, fenceStep, false},
		{"a start of an entry its role has left", checking,
			record.SuperviseRequest{Op: "start", Role: "checker", Phase: phase.Step, Entry: 4, Generation: 2}, fenceEntry, true},
		{"a start of the entry its role is on", checking,
			record.SuperviseRequest{Op: "start", Role: "checker", Phase: phase.Step, Entry: 5, Generation: 2}, fenceHolds, true},
		{"a start of no entry", checking,
			record.SuperviseRequest{Op: "start", Role: "checker", Phase: phase.Step, Generation: 2}, fenceHolds, false},
		{"a root architect's start, which names no phase", checking,
			record.SuperviseRequest{Op: "start", Role: claim.RoleArchitect, Entry: 1, Generation: 2}, fenceHolds, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			read := 0
			got, err := startFenceOf(tc.issue, tc.payload, func() ([]record.PhaseRow, error) {
				read++
				return rows, nil
			})
			if err != nil || got != tc.want {
				t.Fatalf("the fence of %+v = %d, %v; want %d", tc.payload, got, err, tc.want)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.readsRows]; read != want {
				t.Fatalf("the judgment read the phases rows %d times, want %d", read, want)
			}
		})
	}
	refused := errors.New("the store is gone")
	if _, err := startFenceOf(checking, record.SuperviseRequest{Op: "start", Role: "checker", Phase: phase.Step, Entry: 5, Generation: 2},
		func() ([]record.PhaseRow, error) { return nil, refused }); !errors.Is(err, refused) {
		t.Fatalf("the fence over a failed read of the phases rows = %v, want that read's error", err)
	}
}
