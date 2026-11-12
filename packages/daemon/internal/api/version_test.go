package api

import (
	"os"
	"regexp"
	"slices"
	"strconv"
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

// entryHeader matches the head of one numbered entry of version.go's doc comment: `// <n>: `.
var entryHeader = regexp.MustCompile(`(?m)^// (\d+): `)

// numberedEntries is every contract version.go carries an entry for, read out of the file
// itself. The test below runs in the package directory, so the file is beside it.
func numberedEntries(t *testing.T) map[int]bool {
	t.Helper()
	source, err := os.ReadFile("version.go")
	if err != nil {
		t.Fatalf("read version.go: %v", err)
	}
	entries := map[int]bool{}
	for _, match := range entryHeader.FindAllStringSubmatch(string(source), -1) {
		number, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("entry header %q: %v", match[0], err)
		}
		entries[number] = true
	}
	if len(entries) == 0 {
		t.Fatal("version.go carries no numbered entry: the pattern this test reads them with no longer matches")
	}
	return entries
}

// SupportedContracts (answers.go) is the list of contracts this daemon describes a shape for,
// and version.go's numbered entries are what say which contracts exist. The two are held
// together in both directions: an entry between the floor and this daemon's own contract that
// the list omits is a contract no answer is shaped for, and a number in the list that no entry
// names is a contract nothing ever shipped under. 47 and 50 are the second case, which is why
// they are in no list.
func TestSupportedContractsAreTheNumberedEntriesFromTheFloorUp(t *testing.T) {
	entries := numberedEntries(t)
	listed := map[int]bool{}
	for _, contract := range SupportedContracts {
		listed[contract] = true
	}
	for contract := ServingFloor; contract <= DaemonAPIVersion; contract++ {
		if entries[contract] && !listed[contract] {
			t.Errorf("version.go has an entry for %d and SupportedContracts leaves it out", contract)
		}
	}
	for _, contract := range SupportedContracts {
		if contract < ServingFloor || contract > DaemonAPIVersion {
			t.Errorf("SupportedContracts names %d, outside the floor %d and this daemon's own contract %d",
				contract, ServingFloor, DaemonAPIVersion)
		}
		if !entries[contract] {
			t.Errorf("SupportedContracts names %d and version.go has no entry for it", contract)
		}
	}
}
