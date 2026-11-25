package replay

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// filedAfterLeaveWindow is cancelledQuestionWindow with one more recorded act: at 11:00, five
// minutes into ACME-2's planning, its planner, whose session Dispatch names under the title
// `Legion planner · ACME-2`, files ACME-3 under it in triage. A person closes ACME-2 at 11:05.
func filedAfterLeaveWindow(t *testing.T) Window {
	t.Helper()
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	cancelledQuestionRecord(b)
	return b.build()
}

// The recorded events of an issue's work that a replay never opened enter when the issue leaves
// the workflow, as the events of work the replay passed over or cancelled enter (placeUnrun), so a
// child the record files is filed in every replay (assumption 17). The test replays
// filedAfterLeaveWindow under keep-past-review, where ACME-2 is promoted at 11:00, 45 minutes after
// the record: the person's close at 11:05 comes while its architect still writes the design, so its
// planning, where the record files ACME-3, never opens. ACME-3's filing enters at the close, 11:05,
// once. Left with the planning that never opened, it never entered and the replay never filed
// ACME-3; under main, the rule the record ran, it enters at 11:00 as recorded.
func TestTheEventsOfWorkThatNeverOpenedEnterWhenTheIssueLeaves(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		rule string
		at   time.Time
	}{{"main", clock(11, 0)}, {"keep-past-review", clock(11, 5)}} {
		t.Run(c.rule, func(t *testing.T) {
			t.Parallel()
			var copied *shadow
			cfg := RunConfig{Window: filedAfterLeaveWindow(t), Rule: replayedRule(t, c.rule), Branch: MainBranch, Seed: 1, Slowdown: 1,
				PGDSN: replayDSN(t), Out: t.TempDir()}
			cfg.atStart = func(s *shadow) { copied = s }
			if _, err := Run(context.Background(), cfg); err != nil {
				t.Fatalf("replay filedAfterLeaveWindow under %s: %v", c.rule, err)
			}
			if applied, want := eventsApplied(t, copied, "ACME-3", "issue.created"), []time.Time{c.at}; !slices.EqualFunc(applied, want, time.Time.Equal) {
				t.Errorf("the copy of Dispatch holds the filing of ACME-3 applied at %s, want once, at %s", describeTimes(applied), describeTimes(want))
			}
		})
	}
}

// cancelledQuestionRecord writes cancelledQuestionWindow's record into b, with ACME-2's planner's
// filing of ACME-3 at 11:00.
func cancelledQuestionRecord(b *windowBuilder) {
	b.t.Helper()
	recordCancelledQuestion(b, func() {
		planner := recordedSession("ACME-2", claim.RolePlanner, "00000000-0000-4000-8000-0000000000c7")
		b.filedBy(clock(11, 0), "ACME-3", filing{Title: "Ship the ACME-3 change", Parent: "ACME-2", Tree: "ACME-2", Status: "triage", Rank: "a3"},
			planner)
	})
}
