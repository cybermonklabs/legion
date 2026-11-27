package legion

//ci:reads packages/contracts/fixtures/daemon-api/state.json os.ReadFile (daemonAPIFixture, ../../../../contracts/fixtures/daemon-api): the state document the daemon's own internal/daemon/state_fixtures_test.go writes, which a whole read's decode must read
//ci:reads packages/contracts/fixtures/daemon-api/observed-state.json os.ReadFile (daemonAPIFixture, ../../../../contracts/fixtures/daemon-api): the observed view the daemon's own internal/api/state_golden_test.go writes from state.json, which a whole read's decode must read
//ci:reads packages/contracts/fixtures/daemon-api/state-narrowed-repositories.json TestTheDecodeOfADaemonBelowTheEntryHasNoP0Holders os.ReadFile (narrowedStatePath, ../../../../contracts/fixtures/daemon-api): the state the daemon's own internal/api/state_golden_test.go writes for a caller below the P0 start's entry, which a whole read's decode must read to no number of P0 holders

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// daemonAPIFixture is the path of name in packages/contracts/fixtures/daemon-api, which the Legion
// daemon's own tests write: internal/daemon's state_fixtures_test.go builds the daemon's state
// source over a seeded database and writes state.json from what the state route serves from it
// (TestTheStateFixturesAreTheDaemonsCompositionOfTheirSeeds), and internal/api's goldens write
// their views of it, so its bytes are the daemon's wire shape rather than a reading of it.
// dispatch://INFRA-1077's acceptance A4 asks for a test on this side that reads such a document,
// because a Dispatch-side decoder held only to hand-written literals cannot tell when the
// daemon's own shape moves.
func daemonAPIFixture(name string) string {
	return filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "daemon-api", name)
}

// narrowedStatePath is the state the Legion daemon serves a caller at contract 199, below the P0
// start's entry: `packages/daemon/internal/api/state_golden_test.go`'s
// TestStateNarrowedRepositoriesGolden asks the daemon's own route for it and writes it here, so
// these bytes are what the daemon's shaping leaves out for such a caller rather than a reading of
// it.
func narrowedStatePath() string {
	return daemonAPIFixture("state-narrowed-repositories.json")
}

// goldenAdmission is the admission block of a state document, key by key as the daemon wrote it,
// so a test reads what the golden carries without going through the decode it is testing.
func goldenAdmission(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var document struct {
		Admission map[string]json.RawMessage `json:"admission"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("read the golden's admission block: %v", err)
	}
	if document.Admission == nil {
		t.Fatal("the golden carries no admission block, so this test proves nothing")
	}
	return document.Admission
}

// TestTheDecodeReadsTheStateDocumentTheDaemonWrote: a whole read's decode reads the daemon's own
// goldens of its state document: state.json, the whole state, which
// `packages/daemon/internal/daemon`'s TestTheStateFixturesAreTheDaemonsCompositionOfTheirSeeds
// composes over a seeded database and which a daemon of an earlier build serves to a whole read,
// and observed-state.json, the observed view (`?view=observed`) a whole read asks for
// (stateView), which `packages/daemon/internal/api`'s TestObservedStateGolden writes from
// state.json. dispatch://INFRA-1077's acceptance A4 asks for a test
// on this side that reads such a document, because a Dispatch-side decoder held only to
// hand-written literals cannot tell when the daemon's own shape moves. Of each golden, every field
// the Legion board and the four metrics take from a read survives the decode — the settings the
// board carries, the admission block its header and queue are drawn from, the number of P0
// holders its header copies, and each issue's phase and claims — and so does every field a whole
// read needs to be a good read: daemon.project, which must name the setting's project,
// daemon.stateSeq, the cursor's starting point, and the dispatch_url of the settings
// (fetchWhole); so does daemon.stateRun, which a good read takes as the cursor's run
// (recordWhole). Of the pull request the decode keeps nothing, though state.json carries one:
// cybermonklabs/legion#2539 (main's contract 86) took the CI verdict out of the daemon's state
// and deleted the record its required-check mark was read from, so metric 2's CI-wait segment is
// not measured, a board card carries no CI verdict, and no reader is left for the pull request's
// state.
func TestTheDecodeReadsTheStateDocumentTheDaemonWrote(t *testing.T) {
	for _, golden := range []string{"state.json", "observed-state.json"} {
		t.Run(golden, func(t *testing.T) {
			body, err := os.ReadFile(daemonAPIFixture(golden))
			if err != nil {
				t.Fatalf("read the daemon's state golden: %v", err)
			}
			server := stateServer(t, string(body))

			state, size, err := New(server.URL).State(t.Context())
			if err != nil {
				t.Fatalf("decode the daemon's own state document: %v", err)
			}
			if int64(size) != int64(len(body)) {
				t.Errorf("read %d bytes of a %d-byte document", size, len(body))
			}
			if len(state.Settings) == 0 {
				t.Fatalf("settings = %d keys, want the daemon's own", len(state.Settings))
			}
			if state.Admission.Cap == nil || *state.Admission.Cap == 0 {
				t.Errorf("admission.cap = 0: the board's header counts the slots in use against it")
			}
			// The daemon's number of P0 holders, which the board's header gives beside the cap
			// (dispatch://INFRA-5636): the decode keeps the golden's own value.
			raw, carried := goldenAdmission(t, body)["p0Holders"]
			if !carried {
				t.Fatal("the golden's admission carries no p0Holders, so the P0 holders' decode proves nothing")
			}
			var held int
			if err := json.Unmarshal(raw, &held); err != nil {
				t.Fatalf("the golden's admission.p0Holders %s is no number: %v", raw, err)
			}
			if state.Admission.P0Holders == nil || *state.Admission.P0Holders != held {
				t.Errorf("admission.p0Holders decodes to %v, want the golden's %d", state.Admission.P0Holders, held)
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
			read := fetchWhole(t.Context(), Settings{Project: state.Daemon.Project, StateURL: server.URL}, time.Now(), time.Now)
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
		})
	}
}

// TestTheDecodeOfADaemonBelowTheEntryHasNoP0Holders: the daemon's own body for a caller below the
// P0 start's entry carries no `admission.p0Holders`, as no state of a daemon before that entry
// does, and the decode reads it, and the read records it, as no number rather than 0, so the
// board's header gives null for it (dispatch://INFRA-5636). The rest of the block still decodes,
// so what is absent is the number alone.
func TestTheDecodeOfADaemonBelowTheEntryHasNoP0Holders(t *testing.T) {
	body, err := os.ReadFile(narrowedStatePath())
	if err != nil {
		t.Fatalf("read the daemon's narrowed state golden: %v", err)
	}
	if _, carried := goldenAdmission(t, body)["p0Holders"]; carried {
		t.Fatal("the narrowed golden's admission carries p0Holders, so it is no body of a caller below the P0 start's entry and this test proves nothing")
	}
	server := stateServer(t, string(body))

	state, _, err := New(server.URL).State(t.Context())
	if err != nil {
		t.Fatalf("decode the daemon's narrowed state document: %v", err)
	}
	if state.Admission == nil || state.Admission.Cap == nil || len(state.Admission.Active) == 0 {
		t.Fatalf("the admission block decodes to %+v, want the daemon's cap and slot holders", state.Admission)
	}
	if state.Admission.P0Holders != nil {
		t.Errorf("admission.p0Holders decodes to %d, want none: the daemon sent no number", *state.Admission.P0Holders)
	}
	for _, observation := range state.Observations() {
		if observation.IssueKey != AdmissionKey || observation.Field != FieldAdmission {
			continue
		}
		if recorded := AdmissionOf(observation.Value); recorded == nil || recorded.P0Holders != nil {
			t.Errorf("the read records the admission block %s, want it with no number of P0 holders", observation.Value)
		}
		return
	}
	t.Fatal("the read records no admission block, so this test proves nothing")
}
