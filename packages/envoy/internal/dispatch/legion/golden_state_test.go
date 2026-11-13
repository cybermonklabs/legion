package legion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenStatePath is the state document the Legion daemon's own code writes:
// `packages/daemon/internal/api/state_golden_test.go` builds the daemon's populated state and
// writes it here (`golden(t, "state.json", populatedState(t))`), so these bytes are the daemon's
// wire shape rather than a reading of it. dispatch://INFRA-1077's acceptance A4 asks for a test
// on this side that reads such a document, because a Dispatch-side decoder held only to
// hand-written literals cannot tell when the daemon's own shape moves.
func goldenStatePath() string {
	return filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "daemon-api", "state.json")
}

// TestTheDecodeReadsTheStateDocumentTheDaemonWrote: the sampler's decode reads the daemon's own
// golden, and every field the Legion view and the four metrics take from a read survives it — the
// settings the settings control shows with the source of each (the spec's contract 4), the
// admission block the view's header and queue draw, and each issue's phase and claims. Of the
// pull request the decode keeps nothing, though the golden carries one: cybermonklabs/legion#2539
// (main's contract 86) took the CI verdict out of the daemon's state and deleted the record its
// required-check mark was read from, so metric 2's CI-wait segment is not measured, the Legion
// card shows no CI verdict, and no reader is left for the pull request's state.
func TestTheDecodeReadsTheStateDocumentTheDaemonWrote(t *testing.T) {
	body, err := os.ReadFile(goldenStatePath())
	if err != nil {
		t.Fatalf("read the daemon's state golden: %v", err)
	}
	server := stateServer(t, string(body))

	state, size, err := New(server.URL, "").State(t.Context())
	if err != nil {
		t.Fatalf("decode the daemon's own state document: %v", err)
	}
	if size != len(body) {
		t.Errorf("read %d bytes of a %d-byte document", size, len(body))
	}
	if len(state.Settings) == 0 || len(state.SettingSources) == 0 {
		t.Fatalf("settings = %d keys, settingSources = %d keys, want the daemon's own of each",
			len(state.Settings), len(state.SettingSources))
	}
	for key := range state.Settings {
		if _, named := state.SettingSources[key]; !named {
			t.Errorf("setting %q has no entry in settingSources: contract 4 gives every key its source", key)
		}
	}
	if state.Admission.Cap == nil || *state.Admission.Cap == 0 {
		t.Errorf("admission.cap = 0: the view's header draws the slots in use against it")
	}
	if len(state.Issues) == 0 {
		t.Fatalf("the golden carries no issue, so this test proves nothing")
	}

	// The observations a read of this document writes: the three project rows the view's header
	// and its settings control read, a phase for each issue, and nothing of the pull request.
	fields := map[string]int{}
	for _, observation := range state.Observations() {
		fields[observation.Field]++
	}
	for _, field := range []string{FieldAdmission, FieldSettings, FieldSettingSources} {
		if fields[field] != 1 {
			t.Errorf("observations of %s = %d, want exactly one project row", field, fields[field])
		}
	}
	if fields[FieldPhase] != len(state.Issues) {
		t.Errorf("phase observations = %d, want one per issue (%d)", fields[FieldPhase], len(state.Issues))
	}
	for field := range fields {
		if strings.HasPrefix(field, "pull_request") {
			t.Errorf("a read of the golden observes %s, want nothing of the pull request: neither the board nor the metrics reads it", field)
		}
	}
}
