// rework_test.go holds the tests of metric 3, rework: review rounds, send-backs, and the
// implementing and testing hours after an issue's first review.
package metrics

import (
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestReviewRoundsAndSendBacksComeFromThePhaseTimeline: metric 3 over an observed issue that was
// reviewed twice and sent back once, with the implementing and testing hours after its first
// review.
func TestReviewRoundsAndSendBacksComeFromThePhaseTimeline(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(6)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(1), "testing").
		observeString(legion.FieldPhase, hour(2), "reviewing").
		observeString(legion.FieldPhase, hour(3), "implementing").
		observeString(legion.FieldPhase, hour(4), "testing").
		observeString(legion.FieldPhase, hour(5), "reviewing").
		observeString(legion.FieldPhase, hour(6), "done")
	w := windowOf(fixture(hour(-24), hour(24), issue), ScopeAll)
	row := w.fact(t, "INFRA-1")
	if row.ReviewRounds != 2 || !row.SentBack {
		t.Fatalf("the issue has %d review rounds and sent_back=%t, want two rounds and a send-back", row.ReviewRounds, row.SentBack)
	}
	if row.ImplementingHoursAfterFirstReview != 1 || row.TestingHoursAfterFirstReview != 1 {
		t.Fatalf("after the first review: %g implementing and %g testing hours, want one of each",
			row.ImplementingHoursAfterFirstReview, row.TestingHoursAfterFirstReview)
	}
	rework := w.rework(t)
	if rounds, share := medianOf(rework.ReviewRounds), valueOf(rework.SentBack.Share); rounds != 2 || share != 1 {
		t.Fatalf("median rounds %g and sent-back share %g, want 2 and 1", rounds, share)
	}
}

// TestASendBackThatWaitedOnChildrenIsCounted: a review that requests changes while a Dispatch child
// of the issue is open moves it into `awaiting_children` first, and the return from there brings it
// to implementing (dispatch://INFRA-3320, record.ReturnStep in packages/daemon/internal/record).
// The move from reviewing to implementing across that wait is the send-back.
func TestASendBackThatWaitedOnChildrenIsCounted(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(8)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(1), "testing").
		observeString(legion.FieldPhase, hour(2), "reviewing").
		// The reviewer requested changes while a child of the issue was open.
		observeString(legion.FieldPhase, hour(3), "awaiting_children").
		observeString(legion.FieldPhase, hour(5), "implementing").
		observeString(legion.FieldPhase, hour(6), "testing").
		observeString(legion.FieldPhase, hour(7), "reviewing").
		observeString(legion.FieldPhase, hour(8), "done")
	w := windowOf(fixture(hour(-24), hour(24), issue), ScopeAll)
	if row := w.fact(t, "INFRA-1"); !row.SentBack {
		t.Fatalf("sent_back = %t, want the move from reviewing at hour 2 to implementing at hour 5, across the wait on children, counted", row.SentBack)
	}
	if share := valueOf(w.rework(t).SentBack.Share); share != 1 {
		t.Fatalf("sent-back share = %g, want 1", share)
	}
}

// TestAReviewStillRunningAtTheFirstObservationIsOneRound: review rounds are "entries into
// `reviewing`, or into status `needs_review` before observations" (the spec's metric 3). The issue
// below entered `needs_review` before the sampler's first read, and that read found it still in
// review: one entry, the status's, and the observed stretch that carries the same review on is no
// second one.
func TestAReviewStillRunningAtTheFirstObservationIsOneRound(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(5)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(1), "testing").
		statusAt(hour(2), "needs_review").
		observeString(legion.FieldPhase, hour(3), "reviewing").
		observeString(legion.FieldPhase, hour(4), "accepting")
	row := windowOf(fixture(hour(-24), hour(24), issue), ScopeAll).fact(t, "INFRA-1")
	if rounds := row.ReviewRounds; rounds != 1 {
		t.Fatalf("the issue has %d review rounds, want the one its needs_review opened at hour 2, which the sampler saw go on from hour 3", rounds)
	}
}

// TestASendBackOutOfRetroIsCountedOnTheObservedTimeline: metric 3 counts "a move from `testing`,
// `reviewing`, `accepting` `retro`, `merging` or `awaiting_merge` to `implementing`". A backward
// move (`request_backward_move`, the daemon workflow's TriggerBackward rows) sends the issue back
// out of any of them, as a head that started conflicting with its base did before
// cybermonklabs/legion#4464, so the observed half counts a move out of retro, as the status half
// counts the `in_progress` written over a `retro`.
func TestASendBackOutOfRetroIsCountedOnTheObservedTimeline(t *testing.T) {
	fromRetro := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(6)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(1), "testing").
		observeString(legion.FieldPhase, hour(2), "reviewing").
		observeString(legion.FieldPhase, hour(3), "retro").
		// The head started conflicting with its base during retro.
		observeString(legion.FieldPhase, hour(4), "implementing").
		observeString(legion.FieldPhase, hour(6), "done")
	straight := newIssue("INFRA-2").worked("implementer", hour(0)).closedAt(hour(6)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(1), "testing").
		observeString(legion.FieldPhase, hour(2), "reviewing").
		observeString(legion.FieldPhase, hour(3), "retro").
		observeString(legion.FieldPhase, hour(6), "done")

	w := windowOf(fixture(hour(-24), hour(24), fromRetro, straight), ScopeAll)

	for _, key := range w.finished {
		want := key == "INFRA-1"
		if sent := w.fact(t, key).SentBack; sent != want {
			t.Errorf("%s sent_back = %t, want %t: only the issue sent back out of retro was sent back",
				key, sent, want)
		}
	}
	if share := valueOf(w.rework(t).SentBack.Share); share != 0.5 {
		t.Errorf("sent-back share = %g, want 0.5", share)
	}
}

// TestAMoveFromTestingBackToImplementingIsASendBackOnTheObservedTimeline: metric 3's share sent
// back counts "a move from `testing`, `reviewing`, `accepting` `retro`, `merging` or
// `awaiting_merge` to `implementing`". A change its tester fails goes from testing straight back
// to implementing, with no review between.
func TestAMoveFromTestingBackToImplementingIsASendBackOnTheObservedTimeline(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(5)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(1), "testing").
		// The tester failed the change.
		observeString(legion.FieldPhase, hour(2), "implementing").
		observeString(legion.FieldPhase, hour(3), "testing")
	if row := windowOf(fixture(hour(-24), hour(24), issue), ScopeAll).fact(t, "INFRA-1"); !row.SentBack {
		t.Fatalf("sent_back = %t, want the move from testing back to implementing at hour 2 counted", row.SentBack)
	}
}

// TestAMoveOutOfEachSendBackPhaseToImplementingIsASendBack: metric 3's share sent back counts "a
// move from `testing`, `reviewing`, `accepting` `retro`, `merging` or `awaiting_merge` to
// `implementing`" (the spec's metric 3), and a backward move (`request_backward_move`, the daemon
// workflow's TriggerBackward rows) sends the issue back out of any of them, as a head that started
// conflicting with its base did before cybermonklabs/legion#4464. Each of the six phases is a row
// of its own, observed for the hour between two stretches of implementing, so a phase the rule
// leaves out fails its own row.
func TestAMoveOutOfEachSendBackPhaseToImplementingIsASendBack(t *testing.T) {
	for _, phase := range []string{"testing", "reviewing", "accepting", "retro", "merging", "awaiting_merge"} {
		t.Run(phase, func(t *testing.T) {
			issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(3)).
				observeString(legion.FieldPhase, hour(0), "implementing").
				observeString(legion.FieldPhase, hour(1), phase).
				observeString(legion.FieldPhase, hour(2), "implementing")
			if row := windowOf(fixture(hour(-24), hour(24), issue), ScopeAll).fact(t, "INFRA-1"); !row.SentBack {
				t.Fatalf("sent_back = %t, want the move from %s back to implementing at hour 2 counted", row.SentBack, phase)
			}
		})
	}
}

// TestASendBackWhereTheEventLogMeetsTheObservationsIsCounted: a send-back is read off the status
// history before the issue's first observation and off the observed timeline from it on
// (sentBackOnce), and the move can fall between the two. The issue below is in `testing` by its
// status at hour 1, and the sampler's first read finds it implementing at hour 2, with no
// `in_progress` before that read in its status history: the move from the event log's last phase
// into the first observed one is the send-back, and the status half has none to count.
func TestASendBackWhereTheEventLogMeetsTheObservationsIsCounted(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(4)).
		statusAt(hour(1), "testing").
		observeString(legion.FieldPhase, hour(2), "implementing")
	population := fixture(hour(-24), hour(24), issue)
	want := []string{"implementing 0-1", "testing 1-2", "implementing 2-4"}
	if got := timelineOf(population, issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("the timeline is %v, want %v: the event log's phases up to the first observation at hour 2 and the daemon's from it on",
			got, want)
	}
	if row := windowOf(population, ScopeAll).fact(t, "INFRA-1"); !row.SentBack {
		t.Fatalf("sent_back = %t, want the move from the event log's testing into the implementing first observed at hour 2 counted",
			row.SentBack)
	}
}

// TestBeforeObservationsReviewRoundsAreEntriesIntoNeedsReview: an issue the sampler never saw
// counts its review rounds from the status the workflow set, and its send-back from the same
// statuses: the move out of `needs_review` into `in_progress` at hour 3 is the reviewer asking
// for changes, which no timeline drawn from first events alone can hold.
func TestBeforeObservationsReviewRoundsAreEntriesIntoNeedsReview(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(8)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(2), "needs_review").
		statusAt(hour(3), "in_progress").
		statusAt(hour(5), "needs_review").
		statusAt(hour(8), "done")
	row := windowOf(fixture(hour(-24), hour(24), issue), ScopeAll).fact(t, "INFRA-1")
	if row.ReviewRounds != 2 || !row.SentBack {
		t.Fatalf("the issue has %d review rounds and sent_back=%t, want two rounds and the one send-back its statuses record", row.ReviewRounds, row.SentBack)
	}
}
