package api

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// quietTimeState is a state whose two issues of one live tree carry that tree's quiet time — the
// root, which holds a slot, and a child of it past its own review, which holds none — beside an
// issue of a tree that has left the workflow, which carries neither key. The pair is the tree's,
// so the two members carry the same two values, which is what a reader of either of them reads.
func quietTimeState() State {
	movedAt := time.Date(2026, 11, 9, 7, 30, 0, 0, time.UTC)
	quiet := 2.5
	member := func(key string, stands phase.Phase, slot *SlotView) Issue {
		return Issue{Key: key, Tree: "LEGION-10", Phase: stands, Status: "in_progress", Slot: slot,
			TreeMovedAt: &movedAt, QuietHours: &quiet}
	}
	admittedAt := time.Date(2026, 11, 9, 5, 0, 0, 0, time.UTC)
	return State{
		Admission: Admission{Cap: 2, Active: []string{"LEGION-10"}, Trees: []string{"LEGION-10"}},
		Issues: map[string]Issue{
			"LEGION-10": member("LEGION-10", phase.Implementing, &SlotView{Index: 0, AdmittedAt: admittedAt}),
			"LEGION-11": member("LEGION-11", phase.Retro, nil),
			"LEGION-20": {Key: "LEGION-20", Tree: "LEGION-20", Phase: phase.Done, Status: "done"},
		},
	}
}

// quietTimesServed is the tree quiet time of every issue of a state body that carries one, by
// issue key: the raw movement and the raw hours, read by name (TreeMovedAtField, QuietHoursField).
// An issue carrying neither key is in no entry, because whether the keys are on the wire at all
// is what the strip is read by: a reader below quietTimeContract is to be served neither, whether
// its own schema would refuse a key it does not name or quietly drop it. An issue carrying one of
// the two alone is an entry all the same, so that half a measure is seen rather than passed over.
func quietTimesServed(t *testing.T, body []byte) map[string][2]json.RawMessage {
	t.Helper()
	var state struct {
		Issues map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	served := make(map[string][2]json.RawMessage, len(state.Issues))
	for key, issue := range state.Issues {
		movedAt, carriesMovedAt := issue[TreeMovedAtField]
		hours, carriesHours := issue[QuietHoursField]
		if !carriesMovedAt && !carriesHours {
			continue
		}
		served[key] = [2]json.RawMessage{movedAt, hours}
	}
	return served
}

// A plugin below quietTimeContract is served no issue carrying `treeMovedAt` or `quietHours`:
// one built before sjawhar/legion#2788 reads the state with a strict schema and would refuse the
// whole body for a key it does not name, and one built after strips the keys, so the strip is
// what keeps them from a caller that did not ask for them. The route authenticates nobody, so it
// knows that plugin by the pod the request came from: a caller whose pod's claims record a
// contract below this one is served neither key on any issue, and a caller whose pod records
// this contract is served both on every issue of the live tree — the root and the child past its
// review alike — in the same reading of the same state. Every key added since contract 46 is
// served to that second caller today, so what the first is served short of is this change's two
// and nothing else.
func TestAPodBelowTheQuietTimeContractIsServedNeitherKey(t *testing.T) {
	state := quietTimeState()

	behind := stateFrom(t, state, statePodAddr+":4100", podClaims(quietTimeContract-1))
	if served := quietTimesServed(t, behind); len(served) != 0 {
		t.Errorf("the pod below %d is served the quiet times %v, want none: the keys are not its contract's",
			quietTimeContract, served)
	}

	at := stateFrom(t, state, statePodAddr+":4100", podClaims(quietTimeContract))
	served := quietTimesServed(t, at)
	for _, key := range []string{"LEGION-10", "LEGION-11"} {
		pair, carried := served[key]
		if !carried {
			t.Fatalf("the pod at %d is served no %s and no %s for %s, want its tree's measure: %s",
				quietTimeContract, TreeMovedAtField, QuietHoursField, key, at)
		}
		if string(pair[0]) != `"2026-11-09T07:30:00Z"` || string(pair[1]) != "2.5" {
			t.Errorf("%s is served %s %s and %s %s, want its tree's movement and its 2.5 hours",
				key, TreeMovedAtField, pair[0], QuietHoursField, pair[1])
		}
	}
	if pair, carried := served["LEGION-20"]; carried {
		t.Errorf("LEGION-20, whose tree has left the workflow, is served %v, want neither key", pair)
	}
}

// quietTimeContract is the contract whose entry in version.go added the two fields, and this
// daemon's own contract is never below it: a caller at this contract is served the measure.
func TestQuietTimeContractIsTheContractThatAddedTheFields(t *testing.T) {
	const addedQuietTime = 111
	if quietTimeContract != addedQuietTime {
		t.Fatalf("quietTimeContract = %d, want %d, the contract whose entry in version.go added each issue's measure of its tree's quiet time",
			quietTimeContract, addedQuietTime)
	}
	if quietTimeContract > DaemonAPIVersion {
		t.Fatalf("quietTimeContract = %d, above DaemonAPIVersion %d: no caller could be served the fields",
			quietTimeContract, DaemonAPIVersion)
	}
}

// Every shipped text that names a contract number beside the tree quiet time names the one the
// daemon declares for it. A renumber moves the constant, which the test above holds, and leaves
// the prose saying the boundary the old number drew: a reader then takes a daemon below the old
// number to serve no measure, where the daemon below the new one serves none. The waiting line
// before this change was renumbered twice and left five such sentences behind, which is why the
// guard is here rather than in a reviewer's eye
// (TestTheShippedTextsNameTheWaitingLinesOwnContract, which this mirrors for its own field).
//
// A text names this field's contract when `contract <N>`, or the bare `below <N>` a sentence
// writes once the passage has already said "contract", stands within 220 bytes of
// TreeMovedAtField or QuietHoursField, and that window names none of the three neighbouring
// fields whose own contracts these same files discuss beside this one — the waiting line, the
// memory draw and the gated steps. The window is where the sentences that state the boundary
// fall, and the exclusions are what let a field declared under a neighbour's paragraph be read
// without reading that paragraph's number as this one's. Both forms are read because a passage
// commonly writes the number twice — a headline "Daemon API contract 100:" and then "a caller
// from a pod below 100 is served the state without the keys" — and a reader of the first form
// alone passes a text whose headline was corrected and whose boundary sentence was not. A file
// that names no such pair is passed over, so a later text in another file is covered the moment
// it names the two together.
//
// What the window does not catch: a number more than 220 bytes from either field's name. A
// passage that states the boundary a paragraph away from the words `treeMovedAt` and
// `quietHours` is outside every window this reads, and no count of bytes is right for every
// layout a later text may take. The two comments that document the fields where they are
// declared are such passages: Issue's in state.go names the fields `TreeMovedAt` and
// `QuietHours`, which no window looks for, and the schema's in legion-api.ts states its boundary
// first more than 220 bytes above its `treeMovedAt` key. So the scan names the two as its
// declarations, and the scanner reads each of those comments whole beside the windows: every
// contract number it names must be this one, and it must name one. The cases after the scan
// hold that reading to each number those comments name: each is put back in memory as another
// contract's, in the comment the scan itself found, and the scanner, handed that one text and
// that one declaration alone so that no window refuses the number in the comment's place, must
// refuse it.
//
// What to do when it fires on a sentence that is true — a passage that names another contract
// beside these fields: correct that sentence, or widen the exclusions until they take in the
// word that marks the other contract, which is what makes this pass such a passage over; a
// field's own comment names no other contract, so one that does is the sentence to correct.
// Changing the number is never the answer: the number is the daemon's own, and the test above is
// what holds it.
func TestTheShippedTextsNameTheQuietTimesOwnContract(t *testing.T) {
	scan := contractScan{
		fields:   []string{TreeMovedAtField, QuietHoursField},
		without:  []string{WaitingLineField, "memoryGroup", "gatedSteps"},
		contract: quietTimeContract,
		files: []string{
			"packages/daemon/internal/api/server.go",
			"packages/daemon/internal/api/state.go",
			"packages/daemon/internal/projection/project.go",
			"packages/daemon/internal/projection/project_test.go",
			"packages/contracts/src/legion-api.ts",
		},
		// The two places the tree quiet time is declared under a comment of its own that states
		// the contract it shipped at: Issue's two fields in state.go and the contract schema's two
		// keys in legion-api.ts, each found by the line declaring the first of the pair there.
		declarations: []contractDeclaration{
			{"packages/daemon/internal/api/state.go", "TreeMovedAt "},
			{"packages/contracts/src/legion-api.ts", TreeMovedAtField + ":"},
		},
	}
	texts := readContractTexts(t, scan)
	checked, comments := scanContractTextsIn(t, scan, texts, func(name, said, near string, want int) {
		t.Errorf("%s says `%s` of the tree quiet time, want the number %d, the contract that added it: %q",
			name, said, want, near)
	})
	if checked == 0 {
		t.Fatalf("no shipped text names a contract beside %s or %s, or above their declarations; this test proves nothing",
			TreeMovedAtField, QuietHoursField)
	}

	// stale is the number a renumber that missed a comment would leave in it: 109 today, and
	// never this contract.
	const stale = quietTimeContract - 2
	for i, place := range scan.declarations {
		// Each case hands the scanner its renumbered text and this declaration alone, so the
		// refusal it must make is the comment's reading's, which no window can make in its place.
		alone := scan
		alone.files, alone.declarations = nil, []contractDeclaration{place}
		text, start, end := texts[place.file], comments[i][0], comments[i][1]
		for _, at := range contractNumbers.FindAllStringSubmatchIndex(text[start:end], -1) {
			said := text[start+at[0]:start+at[2]] + strconv.Itoa(stale)
			renumbered := text[:start+at[2]] + strconv.Itoa(stale) + text[start+at[3]:]
			line := strings.Count(text[:start+at[0]], "\n") + 1
			t.Run(fmt.Sprintf("%s saying %s on line %d", filepath.Base(place.file), said, line), func(t *testing.T) {
				var refused []string
				scanContractTextsIn(t, alone, map[string]string{place.file: renumbered}, func(_, phrase, _ string, _ int) {
					refused = append(refused, phrase)
				})
				if !slices.Contains(refused, said) {
					t.Errorf("%s with `%s` in its comment on the tree quiet time has %v refused, want that phrase among them",
						place.file, said, refused)
				}
			})
		}
	}
}
