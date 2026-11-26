package daemon

// The Legion reviewer's finding on podreplace.go:191 on #5285 at aae2c628: a round's call that
// yields to a launch's record moves its read onto that record by the one rule every notice of an
// act is told by (tellingRecord): a read that found work over one that failed, a read that failed
// over one that found nothing, and of two reads of one weight the read of the newest record. So, of
// these two records, the one notice of the act names the same commits whichever commits first, the
// yield or the launch's finish; with a third record of the act, a tie can go to another read of the
// same weight (movedRead). Each row writes the launch's record of its delete of the issue's Sandbox
// over the round's pod and the round's record of its replacement by issue, each over a read that
// found a commit of its own, in the row's order, yields the round's record to the launch's, and
// finishes the launch's record: its one notice names the commit of the newer record, and not the
// other's.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestARoundsCallThatYieldsGivesATieToTheNewestRecord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// roundFirst writes the round's record before the launch's, so the launch's is the newer.
		roundFirst bool
		says, not  string
	}{
		{name: "the_round's_record_is_the_newer", says: passedOverCommit, not: launchReadCommit},
		{name: "control:_the_launch's_record_is_the_newer", roundFirst: true, says: launchReadCommit, not: passedOverCommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := podlessKeptVolumeRound(t)
			taken := withPodTaken(rig, "launch-volume-1")
			read := workspaceRead{round: podUpdateRound, by: viaIssue, at: rig.clock.Now(), work: roundUnpushed}
			var round string
			recordRound := func() {
				recorded, ok := rig.s.recordRoundReplacement(rig.ctx, podUpdateIssue, podUpdateRound, podUpdatePod, roundBreakingDrift, read)
				if !ok {
					t.Fatal("record the round's replacement by issue")
				}
				round = recorded.Operation
			}
			if tc.roundFirst {
				recordRound()
			}
			if err := rig.s.recordLaunchTakes(rig.ctx, taken); err != nil {
				t.Fatalf("record the launch's act: %v", err)
			}
			if !tc.roundFirst {
				recordRound()
			}

			finish, err := rig.s.finishReplacement(rig.ctx, round, podUpdateIssue, podUpdatePod, true, func(recordedRead) string { return "the round's own words" })
			if err != nil || !finish.Yielded {
				t.Fatalf("the round's finish = %+v, %v; want it to yield to the launch's record", finish, err)
			}
			standing, err := rig.s.supervisor.store.PodReplacements(rig.ctx, podUpdateProject)
			if err != nil {
				t.Fatal(err)
			}
			if len(standing) != 1 || standing[0].Operation != taken.Operation {
				t.Fatalf("the records standing once the round's call yielded = %+v, want the launch's alone", standing)
			}
			var held replacementRecord
			if err := json.Unmarshal(standing[0].Record, &held); err != nil {
				t.Fatal(err)
			}
			if len(held.Commits) != 1 || held.Commits[0] != tc.says {
				t.Errorf("the launch's record holds the read of commits %v, want the newer record's, %s", held.Commits, tc.says)
			}

			if _, err := rig.s.finishReplacement(rig.ctx, taken.Operation, podUpdateIssue, podUpdatePod, false, launchMessage(taken)); err != nil {
				t.Fatalf("finish the launch's record: %v", err)
			}
			told := rig.architectNotices(record.WorkspaceTakenNotice)
			if len(told) != 1 || !strings.Contains(told[0].Reason, tc.says) || strings.Contains(told[0].Reason, tc.not) {
				t.Fatalf("the architect was told %+v, want one workspace-taken notice naming %s, the newer record's commit, and not %s", told, tc.says, tc.not)
			}
		})
	}
}
