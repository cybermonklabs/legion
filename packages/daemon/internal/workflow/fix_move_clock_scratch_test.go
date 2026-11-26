package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A scratch reproduction, never merged: a fix ends at Y with commits of its own, and the way after
// it starts the tester with its copy moved to Y. The tester passes, and the next start, the
// reviewer's, must move nothing: the tester's start already moved the copy, and the tester may have
// committed over Y since. copyMoveAfterFix decides that from the step starts' clock alone, so with
// an engine clock that does not advance, or that steps back, the tester's start is not "after" the
// fix's end and the reviewer's start moves the copy to Y again. The control: a clock that advances.
func TestScratchTheStartAfterTheWaysStartMovesNothingWhateverTheClock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clock func(*Engine)
	}{
		{"a clock that does not advance", func(*Engine) {}},
		{"a clock that steps back at each read", steppingBackClock},
		{"control: a clock that advances", tickingClock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixAtTheEnd(t, returnEngine())
			tc.clock(r.engine)
			r.fixEnds(headTwo)
			r.assertWay(2, phase.Testing, claim.RoleTester, headTwo, 1, retroTesting)
			fix := r.latestFix()
			var testerAt time.Time
			if err := r.pool.QueryRow(t.Context(), `select started_at from step_starts where issue = $1 and task = 'a' and kind = $2 and place = 2`,
				"LEGION-208", string(record.PlaceStep)).Scan(&testerAt); err != nil {
				t.Fatalf("read the tester's start: %v", err)
			}
			t.Logf("the fix ended at %s at %s; the tester's start at the way is stored at %s (after the end: %t)",
				fix.EndHead, fix.EndedAt.UTC(), testerAt.UTC(), testerAt.After(*fix.EndedAt))
			entry := roleRowOf(t, r.pool, r.engine, "LEGION-208", claim.RoleReviewer).Entry

			r.move(TriggerTesterPassed, "", record.PhaseRow{})

			starts := r.startsOf(claim.RoleReviewer)
			if len(starts) == 0 || starts[len(starts)-1].Phase != phase.Reviewing || starts[len(starts)-1].Entry != entry+1 {
				t.Fatalf("the reviewer's starts after the tester's pass = %+v, want the latest in reviewing of entry %d", starts, entry+1)
			}
			got := starts[len(starts)-1].MoveTo
			t.Logf("the reviewer's start of entry %d after the tester's pass moves the copy to %q", entry+1, got)
			if got != "" {
				t.Fatalf("the reviewer's start after the tester's pass moves the task's copy to %q, want no move: the tester's start at the way already moved it there, and the tester may have committed over it since", got)
			}
		})
	}
}

// steppingBackClock makes engine's clock step back a second at each read, from an hour after
// stepClock, so each write the test makes through it is earlier than the one before, and later
// than every write made before it.
func steppingBackClock(engine *Engine) {
	at := stepClock.Add(time.Hour)
	engine.cfg.Clock = func() time.Time {
		at = at.Add(-time.Second)
		return at
	}
}
