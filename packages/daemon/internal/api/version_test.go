package api

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The contract number the boot gate requires of the installed plugin is written where both sides
// read it: `packages/pi-legion/src/daemon-api-version.test.ts` pins the plugin manifest's
// `legion.daemonApiVersion` to this fixture, so a bump on one side alone fails a test.
func TestDaemonAPIVersionGolden(t *testing.T) {
	golden(t, "version.json", map[string]int{"daemonApiVersion": DaemonAPIVersion})
}

// The closed set of causes a claim can fail under is two hand-kept lists — the constants of
// `supervise.FailureCause` and the `LEGION_FAILURE_CAUSES` enum of
// `packages/contracts/src/legion-api.ts`, which `failure.cause` and `pendingCause` are read
// through — and a word added to the Go list alone would make every strict plugin refuse the
// whole state body. So the Go roster is written where the contracts suite reads it, as the
// contract number above is: this golden, which `legion-api.test.ts` holds equal to that enum,
// word for word and in order. A cause added to supervise leaves this fixture stale, and the
// fixture written again (`go test ./internal/api/ -update`) then fails the contracts case until
// the enum names the word too.
//
// The seven words are written out here beside it, so a constant renamed on the Go side — which
// would otherwise rewrite the fixture with nobody reading the diff — fails here by its own name.
func TestFailureCausesGolden(t *testing.T) {
	want := []supervise.FailureCause{"launch_refused", "died", "never_registered", "stale_plugin",
		"prompt_retires_exhausted", "provider_error", "unrecorded"}
	if !slices.Equal(supervise.FailureCauses, want) {
		t.Fatalf("supervise.FailureCauses = %q, want the wire words %q", supervise.FailureCauses, want)
	}
	golden(t, "failure-causes.json", map[string][]supervise.FailureCause{"causes": supervise.FailureCauses})
}

// supervise.PodUpdateContract is the contract that introduced ready_for_update, and supervise
// cannot read DaemonAPIVersion: internal/api imports supervise, not the other way round. This is
// the one place the two meet, so it pins the number rather than bounding it.
//
// A bound is not enough, and this test learned that the hard way: it read `<= DaemonAPIVersion`,
// a merge moved this change's own contract from 45 to 46, and the constant stayed at 45 under a
// green test. Every worker of the released 45 plugin was then judged able to answer an op its
// plugin does not have, so no turn's end counted as an answer and the stale lane opened a plugin
// round in place of a relaunch: every round of the first roll would have stalled. Above the
// daemon's contract is the other way to be wrong, where no claim could answer at all.
//
// So the number is written here, with the entry of api/version.go that introduced the op. A
// later contract bump changes neither: the op was introduced once, and this constant names when.
func TestPodUpdateContractIsTheContractThatIntroducedTheOp(t *testing.T) {
	const introducedReadyForUpdate = 46
	if supervise.PodUpdateContract != introducedReadyForUpdate {
		t.Fatalf("supervise.PodUpdateContract = %d, want %d, the contract whose entry in version.go introduced ready_for_update",
			supervise.PodUpdateContract, introducedReadyForUpdate)
	}
	if supervise.PodUpdateContract > DaemonAPIVersion {
		t.Fatalf("supervise.PodUpdateContract = %d, above DaemonAPIVersion %d: no claim could answer an update round",
			supervise.PodUpdateContract, DaemonAPIVersion)
	}
}
