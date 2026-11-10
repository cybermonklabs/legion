package daemon

import (
	"slices"
	"testing"
)

// The one line a stop may log is the claim write a cancelled decision left owed, and the judgement
// that says so is stopComplaints. These pin both halves of it: what it refuses, and what it
// permits. The permission is the only way this suite can be made quieter about a stop, so a later
// edit that widens it fails here rather than passing every green run.
func TestWhatAStopMayLog(t *testing.T) {
	t.Parallel()
	const (
		stopping  = `{"level":"INFO","msg":"legion daemon stopping","project":"T","claims":2,"deciding":["legion-t-legion-9-architect (supervise.RequestSuspend)"]}`
		owedTrue  = `{"level":"WARN","msg":"supervise: a claim write the database did not take","claim":"legion-t-legion-10-architect","store":"PutClaim","error":"context canceled","owed":true}`
		owedFalse = `{"level":"WARN","msg":"supervise: a claim write the database did not take","claim":"legion-t-legion-10-architect","store":"PutClaim","error":"context canceled","owed":false}`
		repaired  = `{"level":"INFO","msg":"supervise: a claim write the database did not take was made","claim":"legion-t-legion-10-architect"}`
		otherWarn = `{"level":"WARN","msg":"supervise: a claim write the database did not take was made","owed":true}`
		anError   = `{"level":"ERROR","msg":"legion daemon stop timed out"}`
	)
	for _, tc := range []struct {
		name    string
		window  []string
		refused []string
		owed    int
	}{
		{"nothing at all", []string{stopping}, nil, 0},
		{"the owed write the cancel caught", []string{stopping, owedTrue}, nil, 1},
		{"a repair that failed again", []string{stopping, owedFalse}, []string{owedFalse}, 0},
		{"an owed write of a claim the halt did not name", []string{stopping, owedTrue, owedTrue}, nil, 2},
		{"an owed write with no stopping line in the window", []string{owedTrue}, nil, 1},
		{"a repair that landed, at Info", []string{stopping, owedTrue, repaired}, nil, 1},
		{"the made line at Warn, which is not the owed write", []string{stopping, otherWarn}, []string{otherWarn}, 0},
		{"an error of the stop itself", []string{stopping, anError}, []string{anError}, 0},
		{"both, the permitted one no cover for the other", []string{stopping, owedTrue, owedFalse}, []string{owedFalse}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			window := joinLines(tc.window)
			if refused := stopComplaints(window); !slices.Equal(refused, tc.refused) {
				t.Errorf("stopComplaints refused %q, want %q", refused, tc.refused)
			}
			if owed := owedStopWrites(window); len(owed) != tc.owed {
				t.Errorf("owedStopWrites found %d permitted lines, want %d", len(owed), tc.owed)
			}
		})
	}
}

// decidingAtHalt reads the halt's own list, and answers nothing rather than failing where the
// window has no stopping line or one it cannot read: it is reported beside a permitted line rather
// than deciding anything.
func TestWhatTheHaltSaysItWasDeciding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, window string
		want         []string
	}{
		{"the halt names what it cancelled", `{"msg":"legion daemon stopping","deciding":["a (supervise.RequestSuspend)","b (supervise.Retire)"]}`, []string{"a (supervise.RequestSuspend)", "b (supervise.Retire)"}},
		{"the halt cancelled nothing", `{"msg":"legion daemon stopping","deciding":[]}`, nil},
		{"no stopping line in the window", `{"level":"WARN","msg":"something else"}`, nil},
		{"a stopping line that is not JSON", `legion daemon stopping "msg":"legion daemon stopping"`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := decidingAtHalt(tc.window); !slices.Equal(got, tc.want) {
				t.Errorf("decidingAtHalt read %q, want %q", got, tc.want)
			}
		})
	}
}

// joinLines is a log window built from its lines, as a syncBuffer holds one.
func joinLines(lines []string) string {
	window := ""
	for _, line := range lines {
		window += line + "\n"
	}
	return window
}
