// check_states_former_heads_test.go holds the settler to its rule for the former heads of an open
// pull request (check_states_former_heads.go, formerHeadsOf): a head the pull request has left keeps its states,
// and a result, a retraction, a change of a check's words or a settled CI run there publishes the
// new state there, while that pull request is open, since the tester may have tested that head
// while the pull request moved on; the head's rows go once the pull request is closed or merged.
// The rig is check_states_test.go's.
//
// TestAFormerHeadOfAnOpenPullRequestKeepsItsStates writes the events its two moves published to
// packages/contracts/fixtures/dispatch-events/check-states-after-a-move.json, which the daemon's
// testing move replays (workflow's TestAResultAtTheTestedHeadAfterAMoveDecidesTheMove): the daemon
// is held to what Dispatch publishes after a move, and not to a sequence a test wrote by hand.
package api

//ci:reads packages/contracts/fixtures/dispatch-events/check-states-after-a-move.json TestAFormerHeadOfAnOpenPullRequestKeepsItsStates compares the events the settler published with the committed fixture, which it writes again under UPDATE_GOLDEN=1

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// afterAMoveFixturePath is the fixture the daemon's testing move replays: each case's events, as
// Dispatch published them.
var afterAMoveFixturePath = filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "dispatch-events", "check-states-after-a-move.json")

// afterAMoveEvent is one check.state_changed event as the fixture keeps it: each field of the
// payload the daemon reads, and none that differs between two runs (the Verification document's
// id, the results' ids).
type afterAMoveEvent struct {
	Check        string   `json:"check"`
	Head         string   `json:"head"`
	PullRequests []int    `json:"pull_requests"`
	Checks       []string `json:"checks"`
	State        string   `json:"state"`
	Reason       string   `json:"reason"`
	Roles        []string `json:"roles"`
	CICase       string   `json:"ci_case"`
	Serial       int64    `json:"serial"`
}

// afterAMoveFixture is the fixture's whole shape: the events of each case, oldest first.
type afterAMoveFixture struct {
	FailAfterTheMove   []afterAMoveEvent `json:"fail_after_the_move"`
	PassesAfterTheMove []afterAMoveEvent `json:"passes_after_the_move"`
}

// keepEvents appends events to kept as the fixture keeps them.
func keepEvents(kept *[]afterAMoveEvent, events []checkStatesEvent) {
	for _, event := range events {
		*kept = append(*kept, afterAMoveEvent{
			Check: event.check(), Head: event.Payload.Head, PullRequests: event.Payload.PullRequests,
			Checks: event.Payload.Checks, State: event.state(), Reason: event.Payload.Reason,
			Roles: event.Payload.Roles, CICase: event.Payload.CICase, Serial: event.Payload.Serial,
		})
	}
}

// settleKeeping is settleOwed, with the events it published appended to kept before the caller
// judges them, so the fixture holds what Dispatch published whatever the judgement says.
func settleKeeping(t *testing.T, rig *checkStatesRig, kept *[]afterAMoveEvent) []checkStatesEvent {
	t.Helper()
	events := settleOwed(t, rig)
	keepEvents(kept, events)
	return events
}

// wantNoRowAt fails the test where the settler still holds a row of check at head.
func wantNoRowAt(t *testing.T, what string, rig *checkStatesRig, head string, checks ...string) {
	t.Helper()
	for _, check := range checks {
		if _, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, head, check); held {
			t.Fatalf("%s: the state of %q at %s is still published", what, check, head[:6])
		}
	}
}

// TestAFormerHeadOfAnOpenPullRequestKeepsItsStates: #70 moves from H1 to H2 while the tester tests
// H1, and the tester's results at H1 still publish its states there.
//
// In the first case the implementer passes k1 and k2 at H1, #70 moves to H2, and the tester
// records a fail of k2 at H1: one event, naming #70, k2 at H1 unknown for pass_and_fail, the state
// of a commit holding a pass and a fail, as at an open head. Its retraction publishes k2 at H1
// back to yes. Once #70 is closed no state of H1 or H2 is published any more,
// and a later result at H1 publishes nothing. In the second case nobody passed before the move,
// and the tester's passes of k1 and k2 at H1 after it publish yes at H1.
//
// `UPDATE_GOLDEN=1 go test ./internal/dispatch/api/ -run TestAFormerHeadOfAnOpenPullRequestKeepsItsStates`
// writes the events of the two cases up to the tester's results to the fixture, before any
// judgement of them, so the fixture is what Dispatch published; the run without it fails where
// they differ from the fixture.
func TestAFormerHeadOfAnOpenPullRequestKeepsItsStates(t *testing.T) {
	var published afterAMoveFixture
	t.Run("a fail at the tested head after a move", func(t *testing.T) {
		kept := &published.FailAfterTheMove
		rig := newCheckStatesRig(t, checkStatesTwoChecks)
		upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
		wantEvents(t, "#70 opens at H1", settleKeeping(t, rig, kept),
			stateWant{"k1", eachHeadH1, "no", "no_result"}, stateWant{"k2", eachHeadH1, "no", "no_result"})
		eachHeadResult(t, rig.handler, rig.issue, "k1", "implementer", "pass", eachHeadH1)
		eachHeadResult(t, rig.handler, rig.issue, "k2", "implementer", "pass", eachHeadH1)
		wantEvents(t, "the implementer's passes at H1", settleKeeping(t, rig, kept),
			stateWant{"k1", eachHeadH1, "yes", ""}, stateWant{"k2", eachHeadH1, "yes", ""})
		upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
		wantEvents(t, "#70 moves to H2", settleKeeping(t, rig, kept),
			stateWant{"k1", eachHeadH2, "no", "no_result"}, stateWant{"k2", eachHeadH2, "no", "no_result"})

		fail := eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "fail", eachHeadH1)
		events := settleKeeping(t, rig, kept)
		wantEvents(t, "the tester's fail of k2 at H1 after the move", events, stateWant{"k2", eachHeadH1, "unknown", "pass_and_fail"})
		if !slices.Equal(events[0].Payload.PullRequests, []int{70}) || !slices.Equal(events[0].Payload.Checks, []string{"k1", "k2"}) {
			t.Fatalf("the state of k2 at H1 names the pull requests %v and the checks %v, want [70] and [k1 k2]",
				events[0].Payload.PullRequests, events[0].Payload.Checks)
		}
		wantPublishedIsComputed(t, "the tester's fail of k2 at H1", rig, "k2", eachHeadH1)

		if retracted := retractAsPerson(t, rig.handler, fail, "The run was against the wrong build."); retracted.Code != http.StatusCreated {
			t.Fatalf("retract the tester's fail: status=%d body=%s", retracted.Code, retracted.Body.String())
		}
		wantEvents(t, "the tester's fail retracted", settleOwed(t, rig), stateWant{"k2", eachHeadH1, "yes", ""})

		writeHead(t, rig.database, ciJobRepository, 70, eachHeadH2, "closed", rig.issue)
		if events := settleOwed(t, rig); len(events) != 0 {
			t.Fatalf("#70 closed: the settler published %v, want nothing", eventLines(events))
		}
		wantNoRowAt(t, "#70 closed", rig, eachHeadH1, "", "k1", "k2")
		wantNoRowAt(t, "#70 closed", rig, eachHeadH2, "", "k1", "k2")
		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", eachHeadH1)
		if events := settleOwed(t, rig); len(events) != 0 {
			t.Fatalf("a fail at H1 after #70 closed: the settler published %v, want nothing", eventLines(events))
		}
		wantNoRowAt(t, "a fail at H1 after #70 closed", rig, eachHeadH1, "", "k1")
	})
	t.Run("passes at the tested head after a move", func(t *testing.T) {
		kept := &published.PassesAfterTheMove
		rig := newCheckStatesRig(t, checkStatesTwoChecks)
		upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
		wantEvents(t, "#70 opens at H1", settleKeeping(t, rig, kept),
			stateWant{"k1", eachHeadH1, "no", "no_result"}, stateWant{"k2", eachHeadH1, "no", "no_result"})
		upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
		wantEvents(t, "#70 moves to H2", settleKeeping(t, rig, kept),
			stateWant{"k1", eachHeadH2, "no", "no_result"}, stateWant{"k2", eachHeadH2, "no", "no_result"})

		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
		eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "pass", eachHeadH1)
		events := settleKeeping(t, rig, kept)
		wantEvents(t, "the tester's passes at H1 after the move", events,
			stateWant{"k1", eachHeadH1, "yes", ""}, stateWant{"k2", eachHeadH1, "yes", ""})
		for _, event := range events {
			if !slices.Equal(event.Payload.Roles, []string{"tester"}) {
				t.Fatalf("the state %s names the roles %v, want [tester]", event.line(), event.Payload.Roles)
			}
		}
	})

	encoded, err := json.MarshalIndent(published, "", "  ")
	if err != nil {
		t.Fatalf("encode the published events: %v", err)
	}
	encoded = append(encoded, '\n')
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(afterAMoveFixturePath, encoded, 0o644); err != nil {
			t.Fatalf("write %s: %v", afterAMoveFixturePath, err)
		}
		return
	}
	want, err := os.ReadFile(afterAMoveFixturePath)
	if err != nil {
		t.Fatalf("read %s (run with UPDATE_GOLDEN=1 to write it): %v", afterAMoveFixturePath, err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("the events Dispatch published differ from %s, which the daemon replays (run with UPDATE_GOLDEN=1 to write them):\n%s\nwant\n%s",
			afterAMoveFixturePath, encoded, want)
	}
}

// TestAFormerHeadTakesEachKindOfChange: while #70 is open at H2, a change of a check's words
// publishes the new state at H1 as at H2, and k3, a check CI proves, keeps the state of its run at
// H1 across the move (its run there is still read) and takes yes from CI once that run concludes.
// A merge of #70 ends H1 as a close does.
func TestAFormerHeadTakesEachKindOfChange(t *testing.T) {
	t.Run("words moved", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesTwoChecks)
		upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
		settleOwed(t, rig)
		upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH2)
		settleOwed(t, rig)
		uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "verification.md",
			checkStatesBlock("k1", ciCheckOrdinary, "The server records a result against a check, named by its block id.")+"\n"+
				checkStatesBlock("k2", ciCheckOrdinary, "The server answers a read of the results."))
		wantEventSet(t, "k1's words moved", settleOwed(t, rig),
			stateWant{"k1", eachHeadH1, "no", "no_result"}, stateWant{"k1", eachHeadH2, "no", "no_result"})
	})
	t.Run("CI settled", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesWithCI)
		rig.github.listings.set(eachHeadH1, pendingRun(901, ciJobName, "in_progress"))
		rig.github.listings.set(eachHeadH2, pendingRun(902, ciJobName, "in_progress"))
		upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
		started := eventsOfCheck(settleOwed(t, rig), "k3", eachHeadH1)
		if len(started) != 1 || started[0].state() != "no" || started[0].Payload.CICase != ciCaseNotCompleted {
			t.Fatalf("k3 at H1 while its run goes on: %v, want one event of no with ci_case %s", eventLines(started), ciCaseNotCompleted)
		}
		upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
		moved := settleOwed(t, rig)
		wantEventSet(t, "#70 moves to H2", moved,
			stateWant{"k1", eachHeadH2, "no", "no_result"}, stateWant{"k2", eachHeadH2, "no", "no_result"},
			stateWant{"k3", eachHeadH2, "no", "no_result"})

		rig.github.listings.set(eachHeadH1, completedRun(901, ciJobName, "success"))
		if _, err := rig.database.Pool.Exec(context.Background(), `select check_states_owe($1)`, rig.issue); err != nil {
			t.Fatalf("mark %s owed as the settlement of a pull request's checks does: %v", rig.issue, err)
		}
		events := settleOwed(t, rig)
		wantEvents(t, "k3's run at H1 concluded after the move", events, stateWant{"k3", eachHeadH1, "yes", ""})
		if !slices.Equal(events[0].Payload.Roles, []string{"ci"}) || events[0].Payload.CICase != "success" ||
			!slices.Equal(events[0].Payload.PullRequests, []int{70}) {
			t.Fatalf("k3 at H1 once its run passed: roles %v, ci_case %q, pull requests %v, want [ci], success and [70]",
				events[0].Payload.Roles, events[0].Payload.CICase, events[0].Payload.PullRequests)
		}
	})
	t.Run("a merge", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesTwoChecks)
		upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
		settleOwed(t, rig)
		upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
		settleOwed(t, rig)
		writeHead(t, rig.database, ciJobRepository, 70, eachHeadH2, "merged", rig.issue)
		if events := settleOwed(t, rig); len(events) != 0 {
			t.Fatalf("#70 merged: the settler published %v, want nothing", eventLines(events))
		}
		wantNoRowAt(t, "#70 merged", rig, eachHeadH1, "", "k1", "k2")
		wantNoRowAt(t, "#70 merged", rig, eachHeadH2, "", "k1", "k2")
	})
}

// TestAHeadStaysAFormerHeadOfEachPullRequestThatWasAtIt: #71 opens at H1, and #70 opens at H1
// after it, which changes no state and publishes nothing. #70 moves to H2, and #71 closes: H1
// stays a former head of #70, since the settler named #70 at H1 when it came, and the tester's fail
// of k1 at H1 publishes there, naming #70 alone.
func TestAHeadStaysAFormerHeadOfEachPullRequestThatWasAtIt(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	upsertHead(t, rig, 71, eachHeadH1, rig.issue, "A change")
	wantEvents(t, "#71 opens at H1", settleOwed(t, rig),
		stateWant{"k1", eachHeadH1, "no", "no_result"}, stateWant{"k2", eachHeadH1, "no", "no_result"})
	upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
	if events := settleOwed(t, rig); len(events) != 0 {
		t.Fatalf("#70 opens at H1, where #71 is: the settler published %v, want nothing", eventLines(events))
	}
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	wantEvents(t, "#70 moves to H2", settleOwed(t, rig),
		stateWant{"k1", eachHeadH2, "no", "no_result"}, stateWant{"k2", eachHeadH2, "no", "no_result"})
	writeHead(t, rig.database, ciJobRepository, 71, eachHeadH1, "closed", rig.issue)
	if events := settleOwed(t, rig); len(events) != 0 {
		t.Fatalf("#71 closed: the settler published %v, want nothing", eventLines(events))
	}

	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", eachHeadH1)
	events := settleOwed(t, rig)
	wantEvents(t, "the tester's fail of k1 at H1", events, stateWant{"k1", eachHeadH1, "no", "fail"})
	if !slices.Equal(events[0].Payload.PullRequests, []int{70}) {
		t.Fatalf("the state of k1 at H1 names the pull requests %v, want [70]", events[0].Payload.PullRequests)
	}
}
