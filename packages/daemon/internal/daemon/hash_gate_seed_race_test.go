package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A person's approval of the latest version of a gated document opens its gate in hash mode (R3 of
// dispatch://INFRA-6065, "A review"), even where a gate seed read Dispatch while that approval's
// write was still open. Dispatch stamps an event with its transaction's start (events.created_at
// defaults to now()), so the approval's event can carry a time before the seed's own read time
// although its write commits after the read, which therefore did not show it; the read's verdict,
// marked by the daemon's clock alone, must not then outrank the approval, in either order the two
// facts reach the row. A read cannot have seen an event whose sequence is above every sequence of
// the issue it could have seen, so the approval is the newer of the two.
func TestHashGateSeedReadDoesNotOutrankALaterApproval(t *testing.T) {
	t.Parallel()
	closedAt := time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC)
	seedAt := closedAt.Add(time.Hour)
	// The person's approval of version 8: its transaction began a millisecond before the seed
	// read Dispatch and committed after it, as event 30 of the issue.
	approval := intake.DispatchArtifact{Key: seedRoot, ArtifactID: seedDoc, Kind: intake.DispatchArtifactApproved, Version: 8,
		Mark: record.EventMark{Seq: 30, At: seedAt.Add(-time.Millisecond)}, Actor: seedReviewer}
	closedRow := func() record.DesignGate {
		changes := record.EventMark{Seq: 20, At: closedAt}
		return record.DesignGate{Issue: seedRoot, Step: record.DesignStepSpec, ArtifactID: seedDoc, LatestVersion: 8, ApprovedVersion: new(7),
			ApprovedMark: record.EventMark{Seq: 10, At: closedAt.Add(-time.Hour)},
			Verdict:      &record.GateVerdict{Version: 8, Current: false, Mark: changes}, ToldOpen: new(false)}
	}
	// What the seed read: the approval of 7, not standing at 8, no digest owed, the person's review
	// of 8 not yet committed.
	staleBody := func(t *testing.T) []byte {
		return hashGateBody(t, "artifact.json", seedRoot, reviewed(10, closedAt.Add(-time.Hour), func(approval map[string]any) {
			versions(7, 8)(approval)
			approval["state"] = "stale"
			approval["current_sha256"] = strings.Repeat("c", 64)
		}))
	}
	for _, tc := range []struct {
		name          string
		approvalFirst bool
	}{
		{name: "the seed's read applies first, then the approval", approvalFirst: false},
		{name: "the approval applies first, then the seed's read", approvalFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newSeedRig(t)
			r.clock = seedAt
			r.put(closedRow())
			r.client.answer(seedDoc, staleBody(t))
			live := func() {
				if _, err := intake.ApplyFact(r.ctx, r.pool, "dispatch", "approval-30", approval, r.runner.handlers...); err != nil {
					t.Fatalf("apply the person's approval of version 8: %v", err)
				}
			}
			if tc.approvalFirst {
				live()
				r.seed(seedRoot, 8)
			} else {
				r.seed(seedRoot, 8)
				live()
			}
			gate := r.gate(seedRoot)
			if gate.ApprovedVersion == nil || *gate.ApprovedVersion != 8 || !record.GateOpen(gate) {
				t.Fatalf("the gate after the person's approval of its latest version 8 = %+v (verdict %+v), want it open on that approval", gate, gate.Verdict)
			}
		})
	}
}
