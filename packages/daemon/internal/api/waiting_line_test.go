package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// waitingLineState is a state whose admission line holds three members — a root a person has
// answered, a root whose first phase has never started, and a member a Dispatch link blocks,
// which stands in the line like any other — beside an issue holding a slot and a closed issue
// the line does not name. Each member carries the place the projection gave it.
func waitingLineState() State {
	admittedAt := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	line := []string{"LEGION-40", "LEGION-20", "LEGION-30"}
	issues := map[string]Issue{
		"LEGION-10": {Key: "LEGION-10", Phase: phase.Implementing, Status: "in_progress",
			Slot: &SlotView{Index: 0, AdmittedAt: admittedAt}},
		"LEGION-50": {Key: "LEGION-50", Phase: phase.Done, Status: "done"},
	}
	for i, key := range line {
		stands := phase.Admitted
		if key == "LEGION-40" {
			stands = phase.AwaitingPerson
		}
		issues[key] = Issue{Key: key, Phase: stands, Status: "todo",
			WaitingLine: &WaitingLineView{Place: i + 1, Length: len(line)}}
	}
	return State{
		Admission: Admission{Cap: 2, Active: []string{"LEGION-10"}, Waiting: line,
			Blocked: []BlockedIssue{{Issue: "LEGION-30", BlockedBy: []string{"LEGION-99"}}}},
		Issues: issues,
	}
}

// waitingLinesServed is the place of every issue of a state body that carries one, by issue key.
// The key is read by name (WaitingLineField) and an issue without it is in no entry, because
// whether the key is on the wire at all is what the strip is read by: a reader below
// waitingLineContract is to be served no such key, whether its own schema would refuse one it
// does not name or quietly drop it.
func waitingLinesServed(t *testing.T, body []byte) map[string]WaitingLineView {
	t.Helper()
	var state struct {
		Issues map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	places := make(map[string]WaitingLineView, len(state.Issues))
	for key, issue := range state.Issues {
		raw, carried := issue[WaitingLineField]
		if !carried {
			continue
		}
		var place WaitingLineView
		if err := json.Unmarshal(raw, &place); err != nil {
			t.Fatalf("decode the %s of %s, %s: %v", WaitingLineField, key, raw, err)
		}
		places[key] = place
	}
	return places
}

// The state route serves each issue of `admission.waiting` its own place in that line and the
// line's length: the places run 1 to the length in the order the list is published in, and no two
// issues stand at the same one. The member a link blocks stands in the line like the answered
// root and the queued root, and what holds it is `admission.blocked` beside this. An issue
// holding a slot and an issue the line does not name are served no `waitingLine` key at all.
func TestTheWaitingLinePlaceIsServedOnEachIssue(t *testing.T) {
	state := waitingLineState()
	body := stateFrom(t, state, "10.1.24.9:4100", podClaims(DaemonAPIVersion))
	line, served := state.Admission.Waiting, waitingLinesServed(t, body)

	if blocked := state.Admission.Blocked; len(blocked) != 1 || blocked[0].Issue != "LEGION-30" ||
		!slices.Contains(line, "LEGION-30") {
		t.Fatalf("the line %v and the blocked %#v must hold LEGION-30 together; this test proves nothing", line, blocked)
	}
	stands := make(map[int]string, len(line))
	for i, key := range line {
		place, carried := served[key]
		if !carried {
			t.Fatalf("%s stands at %d in %v and is served no %s: %s", key, i+1, line, WaitingLineField, body)
		}
		if place.Place != i+1 || place.Length != len(line) {
			t.Errorf("%s's %s = %+v, want place %d of %d, its place in %v", key, WaitingLineField, place, i+1, len(line), line)
		}
		if taken, twice := stands[place.Place]; twice {
			t.Errorf("%s and %s are both served place %d of %v", taken, key, place.Place, line)
		}
		stands[place.Place] = key
	}
	for _, key := range []string{"LEGION-10", "LEGION-50"} {
		if place, carried := served[key]; carried {
			t.Errorf("%s is served %s %+v, want no such key: the line does not name it", key, WaitingLineField, place)
		}
	}
}

// A plugin below waitingLineContract is served no issue carrying `waitingLine`: one built
// before sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole
// body for a key it does not name, and one built after strips the key, so the strip is what
// keeps the field from a caller that did not ask for it. The route authenticates nobody, so
// it knows that plugin by the pod the request came from: a caller whose pod's claims record a
// contract below this one is served no issue carrying the field, and a caller whose pod
// records this contract is served it on every member of the line.
func TestTheWaitingLinePlaceIsStrippedBelowItsContract(t *testing.T) {
	state := waitingLineState()
	behind := stateFrom(t, state, statePodAddr+":4100", podClaims(waitingLineContract-1))
	if served := waitingLinesServed(t, behind); len(served) != 0 {
		t.Errorf("the pod below %d is served the places %+v, want none: the field is not its contract's",
			waitingLineContract, served)
	}

	at := stateFrom(t, state, statePodAddr+":4100", podClaims(waitingLineContract))
	served := waitingLinesServed(t, at)
	for _, key := range state.Admission.Waiting {
		if _, carried := served[key]; !carried {
			t.Errorf("the pod at %d is served no %s for %s, want its place: %s",
				waitingLineContract, WaitingLineField, key, at)
		}
	}
}

// waitingLineContract is the contract whose entry in version.go added the field, and this
// daemon's own contract is never below it: a caller at this contract is served the place.
func TestWaitingLineContractIsTheContractThatAddedTheField(t *testing.T) {
	const addedWaitingLine = 57
	if waitingLineContract != addedWaitingLine {
		t.Fatalf("waitingLineContract = %d, want %d, the contract whose entry in version.go added each waiting issue's place in the line",
			waitingLineContract, addedWaitingLine)
	}
	if waitingLineContract > DaemonAPIVersion {
		t.Fatalf("waitingLineContract = %d, above DaemonAPIVersion %d: no caller could be served the field",
			waitingLineContract, DaemonAPIVersion)
	}
}

// Every shipped text that names a contract number beside the waiting line names the one the
// daemon declares for it. A renumber moves the constants, which the test above holds, and leaves
// the prose saying the boundary the old number drew: a reader then takes a daemon below the old
// number to serve no place, where the daemon below the new one serves none. This change was
// renumbered twice, and the second renumber left five such sentences behind, so the guard is
// here rather than in a reviewer's eye.
//
// A text names this field's contract when `contract <N>`, or the bare `below <N>` a sentence
// writes once the passage has already said "contract", stands within 220 bytes of
// WaitingLineField, and that window names neither the retired stranded state nor the stranded
// report, which is the other contract these same files discuss; the window is where the
// sentences that state the boundary fall. Both forms are read because a passage commonly writes
// the number twice — a headline "Daemon API contract 57:" and then "a caller from a pod below
// 57 is served the state without the field" — and a reader of the first form alone passes a
// text whose headline was corrected and whose boundary sentence was not. A file that names no
// such pair is passed over, so a later text in another file is covered the moment it names the
// two together.
func TestTheShippedTextsNameTheWaitingLinesOwnContract(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	named := regexp.MustCompile(`(?:contract|below) (\d+)`)
	const window = 220
	checked := 0
	for _, name := range []string{
		"packages/daemon/cmd/legion/main.go",
		"packages/daemon/cmd/legion/main_test.go",
		"packages/daemon/internal/api/server.go",
		"packages/daemon/internal/api/state.go",
		"packages/daemon/internal/projection/project.go",
		"packages/daemon/internal/projection/project_test.go",
		"packages/contracts/src/legion-api.ts",
		"packages/pi-legion/AGENTS.md",
		"docs/site/src/content/docs/legion/running-legion.md",
		"docs/site/src/content/docs/legion/troubleshooting.md",
		"packages/contracts/.changes/unreleased/pr2989-Added-20261108-074056.yaml",
		"packages/pi-legion/.changes/unreleased/pr2989-Changed-20261108-074108.yaml",
		"packages/claude-envoy/.changes/unreleased/pr2989-Changed-20261108-074111.yaml",
	} {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(body)
		for _, at := range named.FindAllStringSubmatchIndex(text, -1) {
			near := text[max(0, at[0]-window):min(len(text), at[1]+window)]
			if !strings.Contains(near, WaitingLineField) ||
				strings.Contains(near, "retired") || strings.Contains(near, "stranded") {
				continue
			}
			checked++
			said := text[at[2]:at[3]]
			if want := strconv.Itoa(waitingLineContract); said != want {
				t.Errorf("%s says `%s` of the %s, want the number %d, the contract that added it: %q",
					name, strings.TrimSpace(text[at[0]:at[1]]), WaitingLineField, waitingLineContract,
					strings.TrimSpace(near))
			}
		}
	}
	if checked == 0 {
		t.Fatalf("no shipped text names a contract beside %s; this test proves nothing", WaitingLineField)
	}
}
