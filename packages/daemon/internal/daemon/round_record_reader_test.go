package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A round's record keeps who made the read before its replacement: the claim whose process made
// it, or none for the pod's launcher (roundRecordOf). So the line of a replacement that a sweep
// finishes from its record (endRecordedRound, replacementRecord.read) names the read as the
// round's own line names it (workspaceRead.attrs), and never gives a read made through a role as
// the launcher's.
func TestARoundsRecordKeepsWhoMadeItsRead(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 26, 23, 0, 0, 0, time.UTC)
	work := runtime.WorkspaceWork{Head: "c0ffee", Commits: []string{"a1b2c3d"}, Count: 1}
	tester := mustClaimToken(t, podUpdateIssue, claim.RoleTester)
	for _, tc := range []struct {
		name string
		read workspaceRead
		by   string
	}{
		{name: "through a role", read: workspaceRead{round: podUpdateRound, by: viaProcess, claim: tester, at: at, work: work}, by: string(tester)},
		{name: "through the pod's launcher", read: workspaceRead{round: podUpdateRound, by: viaIssue, at: at, work: work}, by: "launcher"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorded, err := roundRecordOf(podUpdateIssue, podUpdatePod, tc.read, podUpdateDrift)
			if err != nil {
				t.Fatalf("the round's record: %v", err)
			}
			var rec replacementRecord
			if err := json.Unmarshal(recorded.Record, &rec); err != nil {
				t.Fatalf("decode the round's record %s: %v", recorded.Record, err)
			}
			read := rec.read(podUpdatePod)
			if read.claim != tc.read.claim || read.round != tc.read.round || read.by != tc.read.by || !read.at.Equal(at) {
				t.Errorf("the read from the record %s = claim %q, round %q, by %v, at %v; want claim %q, round %q, by %v, at %v",
					recorded.Record, read.claim, read.round, read.by, read.at, tc.read.claim, tc.read.round, tc.read.by, at)
			}
			attrs := read.attrs()
			line := make(map[any]any, len(attrs)/2)
			for i := 0; i+1 < len(attrs); i += 2 {
				line[attrs[i]] = attrs[i+1]
			}
			if line["workspaceReadBy"] != tc.by || line["workspaceRead"] != "found work" {
				t.Errorf("the line of the replacement a sweep finishes from the record names the read %v by %v, want found work by %s, as the round's own line names it",
					line["workspaceRead"], line["workspaceReadBy"], tc.by)
			}
		})
	}
}
