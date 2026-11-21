package legion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// daemonAPIFixture is the path of name in packages/contracts/fixtures/daemon-api, which the Legion
// daemon's own tests write through packages/daemon/internal/testgolden, so its bytes are the
// daemon's wire shape rather than a reading of it.
func daemonAPIFixture(name string) string {
	return filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "daemon-api", name)
}

// TestTheDecodeReadsTheStateDocumentTheDaemonWrote: a whole read's decode reads the daemon's own
// golden of its state document, state.json, which
// `packages/daemon/internal/api/state_golden_test.go` writes from the daemon's populated state.
// dispatch://INFRA-1077's acceptance A4 asks for a test on this side that reads such a document,
// because a Dispatch-side decoder held only to hand-written literals cannot tell when the daemon's
// own shape moves. Every field the Legion board and the four metrics take from a read survives the
// decode — the settings the board carries, the admission block its header and queue are drawn
// from, and each issue's phase and claims — and so does every field a whole read needs to be a
// good read: daemon.project, which must name the setting's project, daemon.stateSeq, the cursor's
// starting point, and the dispatch_url of the settings (fetchWhole); so does daemon.stateRun, which
// a good read takes as the cursor's run (recordWhole). Of the pull request the decode keeps
// nothing, though the golden carries one: cybermonklabs/legion#2539 (main's contract 86) took the
// CI verdict out of the daemon's state and deleted the record its required-check mark was read
// from, so metric 2's CI-wait segment is not measured, a board card carries no CI verdict, and no
// reader is left for the pull request's state.
func TestTheDecodeReadsTheStateDocumentTheDaemonWrote(t *testing.T) {
	body, err := os.ReadFile(daemonAPIFixture("state.json"))
	if err != nil {
		t.Fatalf("read the daemon's state golden: %v", err)
	}
	server := stateServer(t, string(body))

	state, size, err := New(server.URL).State(t.Context())
	if err != nil {
		t.Fatalf("decode the daemon's own state document: %v", err)
	}
	if size != len(body) {
		t.Errorf("read %d bytes of a %d-byte document", size, len(body))
	}
	if len(state.Settings) == 0 {
		t.Fatalf("settings = %d keys, want the daemon's own", len(state.Settings))
	}
	if state.Admission.Cap == nil || *state.Admission.Cap == 0 {
		t.Errorf("admission.cap = 0: the board's header counts the slots in use against it")
	}
	if len(state.Issues) == 0 {
		t.Fatalf("the golden carries no issue, so this test proves nothing")
	}

	// The daemon block a whole read judges: the project it names and the state sequence it serves.
	if state.Daemon == nil || state.Daemon.Project == "" {
		t.Fatalf("daemon = %+v, want the project the daemon serves: a whole read of a state that names none is a failed read", state.Daemon)
	}
	if state.Daemon.StateSeq == nil || *state.Daemon.StateSeq == 0 {
		t.Fatalf("daemon.stateSeq = %v, want the state sequence the state reflects: a whole read of a state without one is no starting point", state.Daemon.StateSeq)
	}
	read := fetchWhole(t.Context(), Settings{Project: state.Daemon.Project, StateURL: server.URL}, time.Now)
	if read.failure != nil || !read.observed || read.stateSeq == nil || *read.stateSeq != *state.Daemon.StateSeq {
		t.Errorf("a whole read of the golden for project %s = failure %v, observed %v, stateSeq %v; want a good read at stateSeq %d",
			state.Daemon.Project, read.failure, read.observed, read.stateSeq, *state.Daemon.StateSeq)
	}
	if state.Daemon.StateRun == "" || read.stateRun == nil || *read.stateRun != state.Daemon.StateRun {
		t.Errorf("daemon.stateRun = %q and the whole read keeps %s, want the run the golden serves, which a good read takes as the cursor's run",
			state.Daemon.StateRun, orNone(read.stateRun))
	}

	// The observations a read of this document writes: the two project rows the board's header
	// and its settings are read from, a phase for each issue, and nothing of the pull request.
	fields := map[string]int{}
	for _, observation := range state.Observations() {
		fields[observation.Field]++
	}
	for _, field := range []string{FieldAdmission, FieldSettings} {
		if fields[field] != 1 {
			t.Errorf("observations of %s = %d, want exactly one project row", field, fields[field])
		}
	}
	if fields[FieldPhase] != len(state.Issues) {
		t.Errorf("phase observations = %d, want one per issue (%d)", fields[FieldPhase], len(state.Issues))
	}
	for field := range fields {
		if strings.HasPrefix(field, "pull_request") || field == "status" {
			t.Errorf("a read of the golden observes %s, want nothing of the issue's status or its pull request: neither the board nor the metrics reads either", field)
		}
	}
}
