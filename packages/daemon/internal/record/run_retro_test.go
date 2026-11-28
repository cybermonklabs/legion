package record

import (
	"testing"
	"time"
)

// A run's row tells three things apart that the close and every fence behind it read
// (INFRA-5602's P1): a run whose retro never started, a retro that started inside the run before
// its issue closed, which the run had and which is no retro after a close (a retro that passed,
// one that moved the work back, one stopped at its bound, whichever step the row came from), and
// the retro Legion runs after the close, which is open from its opening to its end. Only the
// last is a retro anything waits for, and only the second and the last count as the run's retro:
// the issue that closes after a retro of its run gets none.
func TestARunRetroSaysWhatTheRunHadAndWhetherItsRetroAfterTheCloseIsOpen(t *testing.T) {
	started := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	retro := started.Add(time.Hour)
	opened := started.Add(2 * time.Hour)
	bound := retro.Add(24 * time.Hour)
	ended := opened.Add(30 * time.Minute)
	for _, tc := range []struct {
		name                         string
		row                          RunRetro
		retroHad, afterClose, isOpen bool
	}{
		{
			name: "a run that started and has had no retro",
			row:  RunRetro{Issue: "LEGION-208", Generation: 2, Tree: "LEGION-200", StartedAt: started},
		},
		{
			name:     "a retro that started inside the run before the close",
			row:      RunRetro{Issue: "LEGION-208", Generation: 2, Tree: "LEGION-200", StartedAt: started, RetroStartedAt: &retro},
			retroHad: true,
		},
		{
			name: "a retro opened after the close, not yet reported",
			row: RunRetro{Issue: "LEGION-208", Generation: 2, Tree: "LEGION-200", StartedAt: started, RetroStartedAt: &retro,
				OpenedAt: &opened, BoundAt: &bound, Agent: "legion-legion-legion-208-implementer"},
			retroHad: true, afterClose: true, isOpen: true,
		},
		{
			name: "a retro after the close that its report ended",
			row: RunRetro{Issue: "LEGION-208", Generation: 2, Tree: "LEGION-200", StartedAt: started, RetroStartedAt: &retro,
				OpenedAt: &opened, BoundAt: &bound, Agent: "legion-legion-legion-208-implementer",
				EndedAt: &ended, Outcome: RetroOutcomePassed, EndedBy: RetroEndReport},
			retroHad: true, afterClose: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.RetroHad(); got != tc.retroHad {
				t.Errorf("RetroHad() = %t, want %t", got, tc.retroHad)
			}
			if got := tc.row.AfterClose(); got != tc.afterClose {
				t.Errorf("AfterClose() = %t, want %t", got, tc.afterClose)
			}
			if got := tc.row.Open(); got != tc.isOpen {
				t.Errorf("Open() = %t, want %t", got, tc.isOpen)
			}
		})
	}
}
