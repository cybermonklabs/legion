package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// closeWaitState is a state holding an issue in verifying whose close waits for a reported defect
// and a read of its checks, an issue in merging that holds a reported defect, and an issue at work
// that carries neither.
func closeWaitState() State {
	merged := time.Date(2026, 11, 27, 4, 0, 0, 0, time.UTC)
	reported := merged.Add(10 * time.Minute)
	return State{
		Issues: map[string]Issue{
			"LEGION-208": {Key: "LEGION-208", Phase: phase.Verifying, Status: "testing", Workers: map[claim.Role]PhaseView{},
				CloseWait: &CloseWaitView{Since: &merged, NextChanges: 0,
					Defects: []CloseWaitDefectView{{ID: "3", Kind: "defect", Role: "merger", Text: "the retry never stops",
						Head: "4d2f6a0c8e1b3d5f7a9c0e2b4d6f8a1c3e5b7d90", Since: reported}},
					Unread: &CloseWaitUnreadView{Cause: "no read of the checks since the merge", Since: merged}}},
			"LEGION-209": {Key: "LEGION-209", Phase: phase.Merging, Status: "retro", Workers: map[claim.Role]PhaseView{},
				CloseWait: &CloseWaitView{Defects: []CloseWaitDefectView{{ID: "4", Kind: "defect", Role: "implementer",
					Text: "the log names no issue", Head: "4d2f6a0c8e1b3d5f7a9c0e2b4d6f8a1c3e5b7d90", Since: reported}}}},
			"LEGION-210": {Key: "LEGION-210", Phase: phase.Implementing, Status: "in_progress", Workers: map[claim.Role]PhaseView{}},
		},
	}
}

// servedIssue is what a state body carries of one issue that the defect-calls entry moves: its
// phase, and its closeWait as the wire holds it.
type servedIssue struct {
	Phase     string          `json:"phase"`
	CloseWait json.RawMessage `json:"closeWait"`
}

// servedIssues is each issue of a state body, by key.
func servedIssues(t *testing.T, body []byte) map[string]servedIssue {
	t.Helper()
	var served struct {
		Issues map[string]servedIssue `json:"issues"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	return served.Issues
}

// A caller below the defect-calls entry knows neither `verifying`, which every plugin before it
// parses `phase` with a strict enumeration that lacks, nor `closeWait`: the entry's value strip
// serves it the phase as `merging`, the phase a merged issue waited in before (shaping.go's
// withoutUnadmittedPhases, phaseStandIn), and its band serves every issue without `closeWait`
// (withoutCloseWait). That holds for a caller stating a contract below the entry, for one from a
// pod whose claims record one, and on the record route as on the state. A caller at the entry's
// contract, and one stating nothing from no pod, reads both as the source holds them, and the
// source is left as it was.
func TestAnOlderContractSeesMergingAndNoCloseWait(t *testing.T) {
	state := closeWaitState()
	const operator = "192.0.2.1:41000"
	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		stated int
		phase  phase.Phase
		wait   bool
	}{
		{name: "a caller stating the contract just below", remote: operator, claims: podClaims(DaemonAPIVersion()),
			stated: entryNumber(defectCallsContract) - 1, phase: phase.Merging},
		{name: "a pod at pauseContract", remote: statePodAddr + ":41000", claims: podClaims(pauseContract), phase: phase.Merging},
		{name: "a caller stating the contract", remote: operator, claims: podClaims(pauseContract),
			stated: entryNumber(defectCallsContract), phase: phase.Verifying, wait: true},
		{name: "a caller in no pod stating nothing", remote: operator, claims: podClaims(pauseContract), phase: phase.Verifying, wait: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := stateServer(t, state, tc.claims)
			issues := servedIssues(t, stateStating(t, handler, tc.remote, tc.stated))
			if got := issues["LEGION-208"].Phase; got != string(tc.phase) {
				t.Errorf("LEGION-208's phase is served as %q, want %q", got, tc.phase)
			}
			if got := issues["LEGION-209"].Phase; got != string(phase.Merging) {
				t.Errorf("LEGION-209's phase is served as %q, want merging unchanged", got)
			}
			if got := issues["LEGION-210"].Phase; got != string(phase.Implementing) {
				t.Errorf("LEGION-210's phase is served as %q, want implementing unchanged", got)
			}
			for _, key := range []string{"LEGION-208", "LEGION-209"} {
				if carried := len(issues[key].CloseWait) > 0; carried != tc.wait {
					t.Errorf("%s carries closeWait = %v, want %v: %s", key, carried, tc.wait, issues[key].CloseWait)
				}
			}
			if len(issues["LEGION-210"].CloseWait) > 0 {
				t.Errorf("LEGION-210 carries closeWait %s, want none", issues["LEGION-210"].CloseWait)
			}

			recorded := asked(t, handler, tc.remote, "/legion/v1/issues/LEGION-208/record", tc.stated)
			if recorded.Code != http.StatusOK {
				t.Fatalf("the record of LEGION-208 = %d, want 200; body %s", recorded.Code, recorded.Body)
			}
			var issue servedIssue
			if err := json.Unmarshal(recorded.Body.Bytes(), &issue); err != nil {
				t.Fatalf("decode the record %s: %v", recorded.Body, err)
			}
			if issue.Phase != string(tc.phase) || (len(issue.CloseWait) > 0) != tc.wait {
				t.Errorf("the record of LEGION-208 serves phase %q and closeWait %s, want %q and closeWait %v", issue.Phase, issue.CloseWait, tc.phase, tc.wait)
			}
		})
	}

	if got := state.Issues["LEGION-208"]; got.Phase != phase.Verifying || got.CloseWait == nil {
		t.Errorf("the source issue = %s with closeWait %v, want verifying with its wait left as it was", got.Phase, got.CloseWait)
	}
}
