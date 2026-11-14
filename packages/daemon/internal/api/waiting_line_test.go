package api

import (
	"encoding/json"
	"fmt"
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
// the line does not name. Each member carries the place the projection gave it and the line's
// own fields beside it: the answered root stands in the returned part, with the moment its work
// came back and the row it kept through the wait, and the two roots that have never started
// stand in the starting part, with the ranks that placed them there and no row at all.
func waitingLineState() State {
	admittedAt := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	answeredAt := admittedAt.Add(-time.Hour)
	holds, waits := true, false
	line := []string{"LEGION-40", "LEGION-20", "LEGION-30"}
	members := map[string]WaitingLineView{
		"LEGION-40": {Part: "returned", Kind: "answered", PartPlace: 1, PartLength: 1,
			HoldsSlot: &holds, Since: &answeredAt},
		"LEGION-20": {Part: "starting", Kind: "queued_root", PartPlace: 1, PartLength: 2,
			HoldsSlot: &waits, RootRank: "C", OwnRank: "C", RootKey: "LEGION-20"},
		"LEGION-30": {Part: "starting", Kind: "queued_root", PartPlace: 2, PartLength: 2,
			HoldsSlot: &waits, RootRank: "E", OwnRank: "E", RootKey: "LEGION-30"},
	}
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
		member := members[key]
		member.Place, member.Length = i+1, len(line)
		issues[key] = Issue{Key: key, Phase: stands, Status: "todo", WaitingLine: &member}
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
	places := make(map[string]WaitingLineView)
	for key, raw := range issueKeyServed(t, body, WaitingLineField) {
		var place WaitingLineView
		if err := json.Unmarshal(raw, &place); err != nil {
			t.Fatalf("decode the %s of %s, %s: %v", WaitingLineField, key, raw, err)
		}
		places[key] = place
	}
	return places
}

// issueKeyServed is the raw JSON of one key of every issue of a state body that carries it, by
// issue key: the one read behind every test of this package that asks what reached the wire
// (waitingLinesServed above, waitingLineFieldsServed and whysServed). An issue without the key
// is in no entry, because whether the key is on the wire at all is the question each of them
// puts.
func issueKeyServed(t *testing.T, body []byte, name string) map[string]json.RawMessage {
	t.Helper()
	var state struct {
		Issues map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	served := make(map[string]json.RawMessage, len(state.Issues))
	for key, issue := range state.Issues {
		if raw, carried := issue[name]; carried {
			served[key] = raw
		}
	}
	return served
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
	const addedWaitingLine = 58
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
// the number twice — a headline "Daemon API contract 58:" and then "a caller from a pod below
// 58 is served the state without the field" — and a reader of the first form alone passes a
// text whose headline was corrected and whose boundary sentence was not. A file that names no
// such pair is passed over, so a later text in another file is covered the moment it names the
// two together.
//
// Two contracts draw a boundary beside this field now, so the window says which one a sentence
// states: a window that also names `partPlace` is held to whyAndLineFieldsContract, and every
// other window to waitingLineContract as before. `partPlace` is the marker because it is a
// member only the later contract adds — the earlier one serves `place` and `length` and nothing
// else, and the strip between them takes `partPlace` away with the rest
// (withoutWhyAndLineFields) — so a sentence naming it states the later boundary and no other.
// Every sentence that states that boundary carries the marker, in this tree's prose as in
// server.go's own comment on the constant, which writes "no `partPlace` and no other field of
// this contract" into the window for exactly this reason: a sentence that forgets the marker is
// read as one about the waiting line's first contract and refused for saying the wrong number,
// which is the refusal wanted. A second marker is not the answer when that happens; the
// sentence naming the member it is about is.
//
// What it does not catch: a number more than 220 bytes from the field's name. A passage that
// states the boundary a paragraph away from the word `waitingLine` is outside every window this
// reads, and it is read by nothing else here: this scan names no declaration whose comment the
// scanner would read whole (contractScan.declarations), as the quiet time's does. The window is
// where the sentences that state the boundary fall today, and no count of bytes is right for
// every layout a later text may take.
//
// What to do when it fires on a sentence that is true — a passage that names another contract
// beside the field, as one about two boundaries at once does: correct that sentence, or widen
// the window until it takes in the word that marks the other contract (`retired`, `stranded`),
// which is what makes this pass such a passage over. Changing the number is never the answer:
// the number is the daemon's own, and the test above is what holds it. This is honest machinery
// for the renumber that already left five sentences behind, not a reader of every text that
// names a contract.
func TestTheShippedTextsNameTheWaitingLinesOwnContract(t *testing.T) {
	checked := scanContractTexts(t, contractScan{
		fields:         []string{WaitingLineField},
		without:        []string{"retired", "stranded"},
		contract:       waitingLineContract,
		marked:         "partPlace",
		markedContract: whyAndLineFieldsContract,
		files: []string{
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
		},
	}, func(name, said, near string, want int) {
		t.Errorf("%s says `%s` of the %s, want the number %d, the contract that added what the sentence states: %q",
			name, said, WaitingLineField, want, near)
	})
	if checked == 0 {
		t.Fatalf("no shipped text names a contract beside %s; this test proves nothing", WaitingLineField)
	}
}

// contractScan is one field's reading of the shipped texts: which windows are that field's, which
// of its declarations have their comments read whole, and the number each of them must name.
type contractScan struct {
	// fields are the field names that mark a window as this field's. A window naming any one of
	// them is read, which is what a measure carried by two keys needs.
	fields []string
	// without are the words that give a window away as a neighbouring field's, whose own
	// contract the same files discuss beside this one. A window naming any one of them is passed
	// over.
	without []string
	// files are the shipped texts read, each one a path from the repository root.
	files []string
	// declarations are the places this field is declared under a comment of its own that states
	// the contract it shipped at. Such a comment may state it farther from the field's name than
	// any window reaches, or name the field in a form no window looks for, so each is read whole:
	// every contract number in it is held to the contract below, and a comment that names none
	// fails the test, since it is where the field's own contract is stated.
	declarations []contractDeclaration
	// contract is the number every window of this field, and every number in its declarations'
	// comments, must name, unless the window or the comment names the marker below.
	contract int
	// marked is the word that gives a window away as stating a later boundary than this field's
	// own, and markedContract the number such a window must name instead. One field draws two
	// boundaries once a later contract adds members to it, and the marker is a member only that
	// later contract serves, so a window naming it states the later number and every other
	// window the field's own. A declaration's comment is read by the same rule, with the whole
	// comment as the text the marker is looked for in. An empty marker leaves every window and
	// every comment held to contract.
	marked         string
	markedContract int
}

// contractDeclaration is one place a field is declared under a comment of its own: file, a path
// from the repository root, and declares, the text the declaring line begins with, indent aside.
// That line is the one line of the file beginning with declares, which is how the comment read
// is the one above it (declarationComment).
type contractDeclaration struct{ file, declares string }

// contractNumbers finds a contract number in the two forms the scanner reads one in: `contract
// <N>`, and the bare `below <N>` a sentence writes once the passage has already said "contract".
// It is the scanner's one regex, for its windows and its declarations' comments alike, and a test
// that renumbers a comment in memory finds the numbers to change by it, so that the numbers it
// changes are the ones the scanner reads.
var contractNumbers = regexp.MustCompile(`(?:contract|below) (\d+)`)

// scanContractTexts reads each file the scan names and holds two readings of it to the scan's
// contract: every window of this field, and the whole comment above each of its declarations. It
// calls wrong for each number that names another contract than the one its window or comment
// states, with the file, the phrase that names it and the window or the comment around it. It
// answers how many numbers it held, one per window of the field and one per number in a
// declaration's comment, so that a caller can refuse a scan that proved nothing. A file it cannot
// read fails the test at once, as does a declaration it cannot find under a comment, and a
// declaration whose comment names no contract fails it too.
//
// It is the one scanner, and both contract tests call it with their own values: a copy of it
// per field is how one of them would come to read a text the other passes over, and a second
// reader of the declarations' comments beside it is how a comment would come to be read by
// another rule than the windows. The window is 220 bytes either side of the number, which is
// where the sentences that state a boundary fall, and both forms of the number are read —
// `contract <N>`, and the bare `below <N>` a sentence writes once the passage has already said
// "contract" — because a passage commonly writes its number twice and a reader of the first form
// alone passes a text whose headline was corrected and whose boundary sentence was not. wrong is
// told the number the window or the comment should have named, which is the field's own or the
// marker's, so the caller's sentence names the boundary the text states rather than one of the
// two always.
//
// The scanner is the reading of the files (readContractTexts) and the reading of their texts
// (scanContractTextsIn) together; a test that changes a text in memory hands the second its
// changed text, so that what refuses the change is this same reading.
func scanContractTexts(t *testing.T, scan contractScan, wrong func(name, said, near string, want int)) int {
	t.Helper()
	checked, _ := scanContractTextsIn(t, scan, readContractTexts(t, scan), wrong)
	return checked
}

// readContractTexts is the text of each file the scan names, its files and its declarations'
// alike, by the file's path from the repository root; a file both name is read once. A file it
// cannot read fails the test at once.
func readContractTexts(t *testing.T, scan contractScan) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "..")
	texts := make(map[string]string, len(scan.files)+len(scan.declarations))
	read := func(name string) {
		if _, done := texts[name]; done {
			return
		}
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		texts[name] = string(body)
	}
	for _, name := range scan.files {
		read(name)
	}
	for _, place := range scan.declarations {
		read(place.file)
	}
	return texts
}

// scanContractTextsIn is the scanner's reading (scanContractTexts) of the texts it is handed,
// by file, rather than of the files on disk: every file the scan names, of its files and its
// declarations', must have a text there, and one that has none fails the test at once. Beside
// the count it answers where it found each declaration's comment — comments[i] is the byte span
// [start, end) of the comment above scan.declarations[i] in that file's text — so that a caller
// renumbering a comment changes the comment this read, found once, rather than finding it again
// by a reading of its own. A declaration it cannot find under a comment fails the test at once,
// and one whose comment names no contract fails it too.
func scanContractTextsIn(t *testing.T, scan contractScan, texts map[string]string,
	wrong func(name, said, near string, want int)) (checked int, comments [][2]int) {
	t.Helper()
	const window = 220
	names := func(near string, words []string) bool {
		return slices.ContainsFunc(words, func(word string) bool { return strings.Contains(near, word) })
	}
	textOf := func(name string) string {
		text, handed := texts[name]
		if !handed {
			t.Fatalf("the scan reads %s and was handed no text of it", name)
		}
		return text
	}
	// hold holds the number found at `at` in text to the contract near states, near being the
	// window around the number or the comment it stands in. It is the one rule that tells the
	// two boundaries apart, for windows and comments alike: a near naming the marker states the
	// contract that added it, and every other near the field's own.
	hold := func(name, text string, at []int, near string) {
		checked++
		holds := scan.contract
		if scan.marked != "" && strings.Contains(near, scan.marked) {
			holds = scan.markedContract
		}
		if said := text[at[2]:at[3]]; said != strconv.Itoa(holds) {
			wrong(name, strings.TrimSpace(text[at[0]:at[1]]), strings.TrimSpace(near), holds)
		}
	}
	for _, name := range scan.files {
		text := textOf(name)
		for _, at := range contractNumbers.FindAllStringSubmatchIndex(text, -1) {
			near := text[max(0, at[0]-window):min(len(text), at[1]+window)]
			if !names(near, scan.fields) || names(near, scan.without) {
				continue
			}
			hold(name, text, at, near)
		}
	}
	comments = make([][2]int, 0, len(scan.declarations))
	for _, place := range scan.declarations {
		text := textOf(place.file)
		start, end, err := declarationComment(text, place.declares)
		if err != nil {
			t.Fatalf("%s: %v", place.file, err)
		}
		comments = append(comments, [2]int{start, end})
		comment := text[start:end]
		found := contractNumbers.FindAllStringSubmatchIndex(comment, -1)
		if len(found) == 0 {
			t.Errorf("the comment above %s's %q names no contract: it is where the field's own contract is stated",
				place.file, place.declares)
		}
		for _, at := range found {
			hold(place.file, comment, at, comment)
		}
	}
	return checked, comments
}

// declarationComment is the comment that documents a field where text declares it, as the byte
// span [start, end) of text: the run of comment lines — a `//` line, or a `/**`, `*` or `*/`
// line of a block — right above the one line that, indent aside, begins with declares. A text
// declaring it on no line or on two, or under no comment, is an error rather than nothing to
// read: the comment is what the scanner holds to the field's number, and one it cannot find is
// one it would pass.
func declarationComment(text, declares string) (start, end int, err error) {
	lines := strings.SplitAfter(text, "\n")
	offsets := make([]int, len(lines)+1)
	declaration := -1
	for i, line := range lines {
		offsets[i+1] = offsets[i] + len(line)
		if !strings.HasPrefix(strings.TrimSpace(line), declares) {
			continue
		}
		if declaration >= 0 {
			return 0, 0, fmt.Errorf("lines %d and %d both begin with %q: the scanner reads the comment above the one line that declares the field",
				declaration+1, i+1, declares)
		}
		declaration = i
	}
	if declaration < 0 {
		return 0, 0, fmt.Errorf("no line begins with %q: the scanner reads the comment above the line that declares the field", declares)
	}
	first := declaration
	for first > 0 {
		above := strings.TrimSpace(lines[first-1])
		if !strings.HasPrefix(above, "//") && !strings.HasPrefix(above, "/*") && !strings.HasPrefix(above, "*") {
			break
		}
		first--
	}
	if first == declaration {
		return 0, 0, fmt.Errorf("line %d, which begins with %q, stands under no comment: that comment is where the field's contract is stated",
			declaration+1, declares)
	}
	return offsets[first], offsets[declaration], nil
}
