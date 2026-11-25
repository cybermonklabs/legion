package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// requestShapesFixture records the field paths of every request body a plugin sends, one record
// per route and numbered contract, from the contract that wrote the file to L
// (LastNumberedContract). It is the request half of the gate whose answer half is
// contractShapesFixture, and it is forward-only: the records start at the contract that wrote the
// file, and no earlier contract is recorded. The records are frozen: no `-update` writes them
// again, and the record of a contract above L is L's with the routes of each entry's fixture at or
// below it applied in the build's order (entry_fixtures_test.go's buildRecords), the fixture
// `-update` writes for an entry that has none. A later contract whose request type no longer reads
// a recorded field fails TestEveryRecordedRequestFieldIsReadByName by the route, the field and the
// contract, unless requests.go's AcceptedByName declares the field. At this daemon's own contract
// the record of each route equals the type's field paths less the fields AcceptedByName declares
// for it, so a field the type reads that neither the record holds nor a row declares fails there
// too. A record of a route that this daemon reads no body at fails the same test, unless
// requests.go's RemovedRoutes declares the route removed at a contract above the record's.
//
// A field path is written as a key path of an answer is (keyPathRule): the dotted path from the
// body's root, with a map's key and a list's index written as `*`.
const requestShapesFixture = "request-shapes.json"

// requestShape is one record of requestShapesFixture: the field paths one contract's plugin may
// send to one route.
type requestShape struct {
	Contract   int      `json:"contract"`
	Route      string   `json:"route"`
	FieldPaths []string `json:"fieldPaths"`
}

// frozenPluginRequests is every route of the numbered entries that decodes a body a plugin sends,
// with the Go type its handler decodes that body into. The grants route takes the claim form and
// the controller form in one type, and the controller's two recoveries share one type, so each of
// those is one row. The register route decodes its body leniently (readRegisterBody) and refuses
// no member at all, and it is recorded all the same: a field it stops reading is a field an older
// plugin's registration means something by. A route an entry adds is a row of that entry's test
// file (entryTestRows.requests), which pluginRequests adds after these.
//
// The census in TestMain holds each row to the type the handler decodes in fact
// (requestDecodedProblems), and TestEveryPostRouteIsARequestRowOrSaysWhyNot holds the table to
// the routes the server registers.
var frozenPluginRequests = map[string]reflect.Type{
	"POST /legion/v1/claims/register":         reflect.TypeFor[claim.RegisterRequest](),
	"POST /legion/v1/claims/ready":            reflect.TypeFor[claim.ReadyRequest](),
	"POST /legion/v1/claims/ready-for-update": reflect.TypeFor[claim.ReadyForUpdateRequest](),
	"POST /legion/v1/claims/exit":             reflect.TypeFor[claim.ExitRequest](),
	"POST /legion/v1/claims/turn-outcome":     reflect.TypeFor[claim.TurnOutcomeRequest](),
	"POST /legion/v1/claims/activity":         reflect.TypeFor[claim.ActivityRequest](),
	"POST /legion/v1/claims/release-records":  reflect.TypeFor[ReleaseRecordsRequest](),
	"POST /legion/v1/grants":                  reflect.TypeFor[GrantRequest](),
	"POST /legion/v1/handoff/complete":        reflect.TypeFor[HandoffCompleteRequest](),
	"POST /legion/v1/issues/status":           reflect.TypeFor[IssueStatusRequest](),
	"POST /legion/v1/claims/fresh-session":    reflect.TypeFor[ClaimRequest](),
	"POST /legion/v1/claims/compact":          reflect.TypeFor[ClaimRequest](),
	"POST /legion/v1/gates/register":          reflect.TypeFor[GateRegisterRequest](),
	"POST /legion/v1/plans/register":          reflect.TypeFor[PlanRegisterRequest](),
	"POST /legion/v1/tasks/stop":              reflect.TypeFor[TaskStopRequest](),
	"POST /legion/v1/waves/release":           reflect.TypeFor[WaveReleaseRequest](),
	"POST /legion/v1/phase/backward":          reflect.TypeFor[PhaseBackwardRequest](),
	"POST /legion/v1/phase/retry":             reflect.TypeFor[PhaseRetryRequest](),
	"POST /legion/v1/signoff":                 reflect.TypeFor[SignOffRequest](),
	"POST /legion/v1/roots/close":             reflect.TypeFor[RootCloseRequest](),
	"POST /legion/v1/children/rerun":          reflect.TypeFor[ChildRequest](),
	"POST /legion/v1/project/pause":           reflect.TypeFor[ProjectPauseRequest](),
	"POST /legion/v1/project/resume":          reflect.TypeFor[ProjectResumeRequest](),
	"POST /legion/v1/intake/replay":           reflect.TypeFor[IntakeReplayRequest](),
	"POST /legion/v1/intake/discard":          reflect.TypeFor[IntakeDiscardRequest](),
	"POST /legion/v1/project/p0-bound":        reflect.TypeFor[ProjectP0BoundRequest](),
	"POST /legion/v1/project/p0-bound/clear":  reflect.TypeFor[ProjectP0BoundClearRequest](),
}

// pluginRequests is every route that decodes a body a plugin sends, with the type its handler
// decodes that body into: the frozen rows, then each entry's rows in the build's order, less every
// route an entry retires (RemovedRoutes). A retired route answers every call with its refusal and
// reads no body, and the record of the retiring entry's number holds no request of it, so a revert
// that retires a route an earlier entry or a numbered one added leaves the frozen rows as they are.
func pluginRequests() map[string]reflect.Type {
	requests := maps.Clone(frozenPluginRequests)
	for _, rows := range entriesTestRows() {
		maps.Copy(requests, rows.requests)
	}
	for route := range RemovedRoutes() {
		delete(requests, route)
	}
	return requests
}

// sameBuild is why a route whose body the `legion` command sends needs no record: the command is
// built from the commit the daemon is built from, so its request and the daemon that reads it are
// never of two contracts.
const sameBuild = "the `legion` command sends this body, and it is built from the daemon's own commit, so no older build sends it"

// routesWithNoPluginBody is every POST route the server registers that pluginRequests holds no
// row for and that decodes a body, with why no plugin's body reaches it.
var routesWithNoPluginBody = map[string]string{
	"POST /legion/v1/controller/secret":                      sameBuild + " (`legion controller start`)",
	"POST /legion/v1/controller/github-credential":           sameBuild + " (`legion controller start`)",
	"POST /legion/v1/operator/claims":                        sameBuild + " (`legion claims spawn`)",
	"POST /legion/v1/operator/claims/{token}/deliver":        sameBuild + " (`legion claims deliver`)",
	"POST /legion/v1/operator/claims/{token}/retry":          sameBuild + " (`legion claims retry`)",
	"POST /legion/v1/operator/claims/{token}/forget-session": sameBuild + " (`legion claims forget-session`)",
	"POST /legion/v1/operator/project/pause":                 sameBuild + " (`legion project pause`)",
	"POST /legion/v1/operator/project/resume":                sameBuild + " (`legion project resume`)",
	"POST /legion/v1/operator/intake/replay":                 sameBuild + " (`legion intake replay`)",
	"POST /legion/v1/operator/intake/discard":                sameBuild + " (`legion intake discard`)",
	"POST /legion/v1/operator/project/p0-bound":              sameBuild + " (`legion project set-p0-bound`)",
	"POST /legion/v1/operator/project/p0-bound/clear":        sameBuild + " (`legion project clear-p0-bound`)",
}

// routesReadingNoBody is every POST route the server registers that decodes no body at all, so
// no body needs a record. The census of the bodies this run decoded holds each such route to it
// (requestDecodedProblems): a route of this set that decodes a body is a problem.
var routesReadingNoBody = map[string]bool{
	"POST /legion/v1/operator/claims/{token}/suspend":       true,
	"POST /legion/v1/operator/claims/{token}/resume":        true,
	"POST /legion/v1/operator/claims/{token}/stop":          true,
	"POST /legion/v1/operator/claims/{token}/compact":       true,
	"POST /legion/v1/operator/claims/{token}/release-names": true,
	"POST /legion/v1/operator/claims/{token}/close":         true,
	// The route park_child called, which answers every call with its refusal (RemovedRoutes).
	"POST /legion/v1/children/park": true,
}

// requestFieldPaths is every field path typ declares, read from its json tags as an answer's key
// paths are (walkDeclaredType), sorted.
func requestFieldPaths(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	shape := declaredShape{paths: map[string]declaredField{}, maps: map[string]bool{}, opaque: map[string]bool{}}
	walkDeclaredType(t, typ, "", declaredField{}, shape, nil)
	return slices.Sorted(maps.Keys(shape.paths))
}

// walkedRequests is the field paths of every request type of pluginRequests, by route. Each path
// is one the type reads by name, AcceptedByName's fields among them.
func walkedRequests(t *testing.T) map[string][]string {
	t.Helper()
	requests := pluginRequests()
	walked := make(map[string][]string, len(requests))
	for route, typ := range requests {
		walked[route] = requestFieldPaths(t, typ)
	}
	return walked
}

// acceptedRow is declared's row for field of route, and false where it holds none.
func acceptedRow(declared []AcceptedField, route, field string) (AcceptedField, bool) {
	at := slices.IndexFunc(declared, func(row AcceptedField) bool { return row.Route == route && row.Field == field })
	if at < 0 {
		return AcceptedField{}, false
	}
	return declared[at], true
}

// acceptedLeft is whether declared holds a row for field of route that left at or below
// contract: a field the request of that contract does not carry, although the type still reads
// it by name.
func acceptedLeft(declared []AcceptedField, route, field string, contract int) bool {
	row, isDeclared := acceptedRow(declared, route, field)
	return isDeclared && row.LeftAt <= contract
}

// requestShapesAt is the records of contract: for each route, the field paths its type reads
// with every field declared to have left at or below contract taken out, since that contract's
// request does not carry it.
func requestShapesAt(walked map[string][]string, declared []AcceptedField, contract int) []requestShape {
	records := make([]requestShape, 0, len(walked))
	for _, route := range slices.Sorted(maps.Keys(walked)) {
		paths := slices.DeleteFunc(slices.Clone(walked[route]), func(path string) bool {
			return acceptedLeft(declared, route, path, contract)
		})
		records = append(records, requestShape{Contract: contract, Route: route, FieldPaths: paths})
	}
	return records
}

// sortedRequestShapes is records in the fixture's own order: by contract, then by route.
func sortedRequestShapes(records []requestShape) []requestShape {
	sorted := slices.Clone(records)
	slices.SortFunc(sorted, func(a, b requestShape) int {
		if a.Contract != b.Contract {
			return a.Contract - b.Contract
		}
		return strings.Compare(a.Route, b.Route)
	})
	return sorted
}

// readRequestShapes is the frozen fixture as it stands, and false where no file is there.
func readRequestShapes(t *testing.T) ([]requestShape, bool) {
	t.Helper()
	encoded, err := os.ReadFile(goldenPath(requestShapesFixture))
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read %s: %v", requestShapesFixture, err)
	}
	var records []requestShape
	if err := json.Unmarshal(encoded, &records); err != nil {
		t.Fatalf("decode %s: %v", requestShapesFixture, err)
	}
	return records, true
}

// requestShapeProblems is every way the request types this daemon reads fail the records on
// file, at current, the contract the daemon declares:
//
//   - a recorded field that the request type does not read by name and that declared holds no
//     row for;
//   - a recorded field that a row of declared says left at or below the record's contract;
//   - a field the request type reads by name that the route's record of current does not hold
//     and that declared holds no row for: the record of the daemon's own contract is what
//     `-update` writes into the fixture of the entry that is that contract (requestShapesAt), the
//     type's field paths less the declared ones, and a run without the flag holds it to exactly
//     that, as the answers' records are held to what the daemon serves;
//   - a recorded route that no row of pluginRequests reads, unless a row of removed declares the
//     route removed at a contract above the record's;
//   - a row of declared whose field the route's request type does not read by name, so the body
//     the row is written for is refused as an unknown member;
//   - a row of declared whose route is no route of pluginRequests, or whose LeftAt is not a
//     contract above the floor and at or below current, or which says nothing of what ignoring
//     the field means;
//   - a route of removed whose contract is not one above the floor and at or below current.
func requestShapeProblems(onFile []requestShape, walked map[string][]string, declared []AcceptedField, removed map[string]int, current int) []string {
	problems := []string{}
	for _, record := range onFile {
		read, routed := walked[record.Route]
		if !routed {
			switch leftAt, isRemoved := removed[record.Route]; {
			case !isRemoved:
				problems = append(problems, fmt.Sprintf(
					"contract %d sends a body to %s, and no route of this daemon reads one there: a plugin of that contract is answered \"no route\"",
					record.Contract, record.Route))
			case leftAt <= record.Contract:
				problems = append(problems, fmt.Sprintf(
					"contract %d sends a body to %s, and requests.go's RemovedRoutes says the route left at %d, at or below that contract: correct its contract",
					record.Contract, record.Route, leftAt))
			}
			continue
		}
		for _, field := range record.FieldPaths {
			row, isDeclared := acceptedRow(declared, record.Route, field)
			switch {
			case isDeclared && row.LeftAt <= record.Contract:
				problems = append(problems, fmt.Sprintf(
					"%s: `%s` is a field of contract %d's request, and requests.go's AcceptedByName says it left at %d, at or below that contract: correct the row's LeftAt",
					record.Route, field, record.Contract, row.LeftAt))
			case slices.Contains(read, field), isDeclared:
				// Read by name, or declared: a declared field the type no longer reads is the
				// row's own problem, reported below.
			default:
				problems = append(problems, fmt.Sprintf(
					"%s: `%s` is a field of contract %d's request, and this daemon's request type no longer reads it, so readBody refuses every plugin of contract %d that sends it. Keep the field in the type and declare it in requests.go's AcceptedByName, with the contract it left at and what ignoring it means, or refuse its value with a sentence of its own where ignoring it would change what the call means.",
					record.Route, field, record.Contract, record.Contract))
			}
		}
		if record.Contract != current {
			continue
		}
		for _, field := range read {
			if _, isDeclared := acceptedRow(declared, record.Route, field); isDeclared || slices.Contains(record.FieldPaths, field) {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s: `%s` is a field this daemon's request type reads by name, and the record of contract %d does not hold it, and requests.go's AcceptedByName declares no row for it. If it is a field of contract %d's request, it is a contract change: add an entry (scripts/daemon-contract-entry.sh) and run `go test ./internal/api/ -update`, which writes its fixture (`-update-entry=<id>` writes an entry's fixture again). If the type reads it only so that an older plugin's body is read, declare it in AcceptedByName, with the contract it left at and what ignoring it means.",
				record.Route, field, current, current))
		}
	}
	for _, row := range declared {
		read, routed := walked[row.Route]
		switch {
		case !routed:
			problems = append(problems, fmt.Sprintf(
				"requests.go's AcceptedByName declares `%s` on %s, which is no route a plugin's body reaches", row.Field, row.Route))
		case !slices.Contains(read, row.Field):
			problems = append(problems, fmt.Sprintf(
				"%s: requests.go's AcceptedByName declares `%s` accepted by name, and the request type does not read it by name, so readBody refuses the body the row is written for as an unknown member",
				row.Route, row.Field))
		}
		if row.LeftAt <= ServingFloor || row.LeftAt > current {
			problems = append(problems, fmt.Sprintf(
				"%s: requests.go's AcceptedByName says `%s` left at %d, which is no contract above the floor %d and at or below this daemon's %d",
				row.Route, row.Field, row.LeftAt, ServingFloor, current))
		}
		if strings.TrimSpace(row.Ignoring) == "" {
			problems = append(problems, fmt.Sprintf(
				"%s: requests.go's AcceptedByName declares `%s` and does not say what ignoring it means", row.Route, row.Field))
		}
	}
	for _, route := range slices.Sorted(maps.Keys(removed)) {
		if leftAt := removed[route]; leftAt <= ServingFloor || leftAt > current {
			problems = append(problems, fmt.Sprintf(
				"requests.go's RemovedRoutes says %s left at %d, which is no contract above the floor %d and at or below this daemon's %d",
				route, leftAt, ServingFloor, current))
		}
	}
	return problems
}

// The frozen fixture itself, held to its own order and format. No flag writes it: the record of a
// contract above L is an entry's, in that entry's fixture (TestEntryFixtures), and
// TestEveryRecordedRequestFieldIsReadByName holds each record to the types.
func TestRequestShapesFixture(t *testing.T) {
	onFile, written := readRequestShapes(t)
	if !written {
		t.Fatalf("%s is missing: it holds the frozen records of the numbered entries, which no flag writes", requestShapesFixture)
	}
	frozenGolden(t, requestShapesFixture, sortedRequestShapes(onFile))
}

// Every field a recorded contract's plugin sends is a field this daemon's request type reads by
// name, or a row of AcceptedByName says why it no longer needs to be, and every row of that
// table is a field the type does read by name. The records are this build's: the frozen ones and
// those the entry fixtures compose (requestRecordsOfBuild). The record of this daemon's own
// contract is among them, and each of its routes is held to equal the type's field paths less the
// fields AcceptedByName declares for the route: a request type that loses a field before
// `-update` is red here, and so is one that reads a field its record does not hold and no row
// declares.
func TestEveryRecordedRequestFieldIsReadByName(t *testing.T) {
	if rewritingFixtures() {
		t.Skip("the fixtures are being rewritten; run the suite again without -update")
	}
	for _, problem := range recordedRequestProblems(t, packageEntryFixtures) {
		t.Error(problem)
	}
}

// recordedRequestProblems is every problem of this build's request records, its entry fixtures
// read from dir, against the case above: requestShapeProblems, and each route of pluginRequests
// that the record of this daemon's own contract does not hold.
func recordedRequestProblems(t *testing.T, dir string) []string {
	t.Helper()
	onFile := requestRecordsOfBuild(t, dir)
	problems := requestShapeProblems(onFile, walkedRequests(t), AcceptedByName(), RemovedRoutes(), DaemonAPIVersion())
	routes := map[string]bool{}
	for _, record := range onFile {
		if record.Contract == DaemonAPIVersion() {
			routes[record.Route] = true
		}
	}
	for _, route := range slices.Sorted(maps.Keys(pluginRequests())) {
		if !routes[route] {
			problems = append(problems, fmt.Sprintf("%s has no record at contract %d: an entry that adds the route writes it into its fixture, with `go test ./internal/api/ -update`", route, DaemonAPIVersion()))
		}
	}
	return problems
}

// withScratchRequestEntry is records, this build's, with the record of a scratch entry at next
// whose fixture `-update` writes from walked and declared: what the types read at next
// (requestShapesAt) less the record of this daemon's own contract, composed over that record as
// buildRecords composes every entry's.
func withScratchRequestEntry(t *testing.T, records []requestShape, walked map[string][]string, declared []AcceptedField, next int) []requestShape {
	t.Helper()
	before := contractRecords{Contract: DaemonAPIVersion()}
	for _, record := range records {
		if record.Contract == DaemonAPIVersion() {
			before.Requests = append(before.Requests, record)
		}
	}
	fixture, err := entryFixtureOf(before, contractRecords{Contract: next, Requests: requestShapesAt(walked, declared, next)})
	if err != nil {
		t.Fatalf("write the scratch entry's fixture: %v", err)
	}
	composed, err := composeEntries(before, []numberedFixture{{name: "scratch", number: next, fixture: fixture}})
	if err != nil {
		t.Fatalf("compose the scratch entry's record: %v", err)
	}
	return append(slices.Clone(records), composed[0].Requests...)
}

// What the request gate catches, shown by scratch changes to copies of the walked types and the
// declarations. A change of what a plugin sends is a new entry, a scratch one here, whose number
// is next, one above this daemon's own: the records of this build stay where they are, the types
// and the table are read as that entry's, and the scratch entry's record is the one `-update`
// writes into its fixture (withScratchRequestEntry).
func TestTheRequestGateCatchesARemovedField(t *testing.T) {
	if rewritingFixtures() {
		t.Skip("the fixtures are being rewritten; run the suite again without -update")
	}
	onFile := requestRecordsOfBuild(t, packageEntryFixtures)
	const route, field = "POST /legion/v1/roots/close", "reason"
	next := DaemonAPIVersion() + 1
	walked := walkedRequests(t)
	if !slices.Contains(walked[route], field) {
		t.Fatalf("%s reads no `%s`; removing it proves nothing", route, field)
	}
	without := maps.Clone(walked)
	without[route] = slices.DeleteFunc(slices.Clone(walked[route]), func(path string) bool { return path == field })

	t.Run("a field leaves a request type with no declaration", func(t *testing.T) {
		// The records of every contract stay, and the scratch entry's fixture writes the record of
		// its own number beside them without the field, so the removal is a problem of every
		// recorded contract whose request holds the field: one problem each, each naming its own
		// contract, this daemon's own among them, and none at the scratch entry's.
		var holding []int
		for _, record := range onFile {
			if record.Route == route && slices.Contains(record.FieldPaths, field) {
				holding = append(holding, record.Contract)
			}
		}
		if !slices.Contains(holding, DaemonAPIVersion()) {
			t.Fatalf("the record of contract %d holds no `%s` at %s; removing it proves nothing", DaemonAPIVersion(), field, route)
		}
		problems := requestShapeProblems(withScratchRequestEntry(t, onFile, without, AcceptedByName(), next), without, AcceptedByName(), RemovedRoutes(), next)
		if len(problems) != len(holding) {
			t.Fatalf("the gate reported %d problems, want one naming the field for each recorded contract whose request holds it, %v:\n%s",
				len(problems), holding, strings.Join(problems, "\n"))
		}
		// The gate reads the records in their order, and so does holding.
		for i, contract := range holding {
			t.Logf("the gate reported: %s", problems[i])
			for _, want := range []string{route, "`" + field + "`", fmt.Sprintf("contract %d", contract)} {
				if !strings.Contains(problems[i], want) {
					t.Errorf("the problem reads %q, want it to name %s", problems[i], want)
				}
			}
		}
		own := fmt.Sprintf("contract %d", DaemonAPIVersion())
		if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, own) }) {
			t.Errorf("no problem names %s, this daemon's own contract:\n%s", own, strings.Join(problems, "\n"))
		}
	})

	t.Run("a field leaves the contract and the type reads it by name with a declaration", func(t *testing.T) {
		declared := append(slices.Clone(AcceptedByName()), AcceptedField{Route: route, Field: field, LeftAt: next, Ignoring: "a scratch row"})
		if problems := requestShapeProblems(withScratchRequestEntry(t, onFile, walked, declared, next), walked, declared, RemovedRoutes(), next); len(problems) != 0 {
			t.Errorf("the gate reported problems for a declared field the type still reads:\n%s", strings.Join(problems, "\n"))
		}
		for _, record := range requestShapesAt(walked, declared, next) {
			if record.Route == route && slices.Contains(record.FieldPaths, field) {
				t.Errorf("the record of contract %d carries `%s`, which the declaration says left at %d", next, field, next)
			}
		}
	})

	t.Run("a declared field the type does not read by name", func(t *testing.T) {
		declared := append(slices.Clone(AcceptedByName()), AcceptedField{Route: route, Field: field, LeftAt: next, Ignoring: "a scratch row"})
		problems := requestShapeProblems(onFile, without, declared, RemovedRoutes(), next)
		if len(problems) != 1 || !strings.Contains(problems[0], "does not read it by name") {
			t.Fatalf("the gate reported %q, want the one problem saying the type does not read `%s` by name", problems, field)
		}
		t.Logf("the gate reported: %s", problems[0])
	})
}

// What the request gate catches at this daemon's own contract, shown by scratch changes to
// copies of the walked types and the declarations: a field the type reads that the record of
// this contract does not hold is either a field of this contract, which `-update` records in its
// entry's fixture, or a field read for an older plugin alone, which a row of AcceptedByName
// declares. With neither, the gate names the field, so the row that says why a field is still read
// cannot vanish unseen.
func TestTheRequestGateCatchesAReadFieldItsRecordDoesNotHold(t *testing.T) {
	if rewritingFixtures() {
		t.Skip("the fixtures are being rewritten; run the suite again without -update")
	}
	onFile := requestRecordsOfBuild(t, packageEntryFixtures)
	walked := walkedRequests(t)
	wantNamed := func(t *testing.T, problems []string, route, field string) {
		t.Helper()
		if len(problems) != 1 {
			t.Fatalf("the gate reported %d problems, want the one naming the field:\n%s", len(problems), strings.Join(problems, "\n"))
		}
		t.Logf("the gate reported: %s", problems[0])
		for _, want := range []string{route, "`" + field + "`", fmt.Sprintf("contract %d", DaemonAPIVersion()), "-update", "AcceptedByName"} {
			if !strings.Contains(problems[0], want) {
				t.Errorf("the problem reads %q, want it to name %s", problems[0], want)
			}
		}
	}

	t.Run("a field read for an older plugin loses its declaration", func(t *testing.T) {
		const route, field = "POST /legion/v1/handoff/complete", "ready"
		if _, isDeclared := acceptedRow(AcceptedByName(), route, field); !isDeclared {
			t.Fatalf("requests.go's AcceptedByName declares no `%s` on %s; deleting the row proves nothing", field, route)
		}
		declared := slices.DeleteFunc(slices.Clone(AcceptedByName()), func(row AcceptedField) bool { return row.Route == route && row.Field == field })
		wantNamed(t, requestShapeProblems(onFile, walked, declared, RemovedRoutes(), DaemonAPIVersion()), route, field)
	})

	t.Run("a field of this contract the record does not hold yet", func(t *testing.T) {
		const route, field = "POST /legion/v1/roots/close", "scratchField"
		if slices.Contains(walked[route], field) {
			t.Fatalf("%s already reads `%s`; adding it proves nothing", route, field)
		}
		with := maps.Clone(walked)
		with[route] = slices.Sorted(slices.Values(append(slices.Clone(walked[route]), field)))
		wantNamed(t, requestShapeProblems(onFile, with, AcceptedByName(), RemovedRoutes(), DaemonAPIVersion()), route, field)
	})
}

// What the request gate catches of a removed route, shown by scratch copies of RemovedRoutes. The
// park route is the route that table names: records of this build hold its body, and no row of
// pluginRequests reads one there. Without its entry, each such record is a plugin answered "no
// route"; an entry whose contract is at or below a recorded contract is wrong for that record;
// and an entry whose contract is above this daemon's own would let this daemon's own record carry
// the route.
func TestTheRequestGateCatchesARemovedRouteItDoesNotDeclare(t *testing.T) {
	if rewritingFixtures() {
		t.Skip("the fixtures are being rewritten; run the suite again without -update")
	}
	onFile := requestRecordsOfBuild(t, packageEntryFixtures)
	const route = "POST /legion/v1/children/park"
	if _, isRemoved := RemovedRoutes()[route]; !isRemoved {
		t.Fatalf("requests.go's RemovedRoutes declares no %s; deleting its entry proves nothing", route)
	}
	var holding []int
	for _, record := range onFile {
		if record.Route == route {
			holding = append(holding, record.Contract)
		}
	}
	if len(holding) == 0 {
		t.Fatalf("no record of this build holds %s; removing its entry proves nothing", route)
	}
	walked := walkedRequests(t)

	t.Run("the removal is not declared", func(t *testing.T) {
		removed := maps.Clone(RemovedRoutes())
		delete(removed, route)
		problems := requestShapeProblems(onFile, walked, AcceptedByName(), removed, DaemonAPIVersion())
		if len(problems) != len(holding) {
			t.Fatalf("the gate reported %d problems, want one for each recorded contract that sends %s a body, %v:\n%s",
				len(problems), route, holding, strings.Join(problems, "\n"))
		}
		// The gate reads the records in their order, and so does holding.
		for i, contract := range holding {
			t.Logf("the gate reported: %s", problems[i])
			for _, want := range []string{route, fmt.Sprintf("contract %d", contract), `"no route"`} {
				if !strings.Contains(problems[i], want) {
					t.Errorf("the problem reads %q, want it to name %s", problems[i], want)
				}
			}
		}
	})

	t.Run("the entry says the route left at a recorded contract", func(t *testing.T) {
		last := holding[len(holding)-1]
		removed := map[string]int{route: last}
		problems := requestShapeProblems(onFile, walked, AcceptedByName(), removed, DaemonAPIVersion())
		if len(problems) != 1 {
			t.Fatalf("the gate reported %d problems, want the one naming the record of contract %d:\n%s", len(problems), last, strings.Join(problems, "\n"))
		}
		t.Logf("the gate reported: %s", problems[0])
		for _, want := range []string{route, fmt.Sprintf("contract %d", last), fmt.Sprintf("left at %d", last), "correct its contract"} {
			if !strings.Contains(problems[0], want) {
				t.Errorf("the problem reads %q, want it to name %s", problems[0], want)
			}
		}
	})

	t.Run("the entry names a contract above this daemon's own", func(t *testing.T) {
		above := DaemonAPIVersion() + 1
		removed := map[string]int{route: above}
		problems := requestShapeProblems(onFile, walked, AcceptedByName(), removed, DaemonAPIVersion())
		if len(problems) != 1 {
			t.Fatalf("the gate reported %d problems, want the one naming the entry's contract %d:\n%s", len(problems), above, strings.Join(problems, "\n"))
		}
		t.Logf("the gate reported: %s", problems[0])
		for _, want := range []string{route, fmt.Sprintf("left at %d", above), "no contract above the floor"} {
			if !strings.Contains(problems[0], want) {
				t.Errorf("the problem reads %q, want it to name %s", problems[0], want)
			}
		}
	})
}

// Every POST route the server registers is a row of pluginRequests, says why no plugin's body
// reaches it, or reads no body, and every route the three tables name is a registered route. A
// route added tomorrow that reads a plugin's body is red here until its type is recorded.
func TestEveryPostRouteIsARequestRowOrSaysWhyNot(t *testing.T) {
	requests := pluginRequests()
	registered := map[string]bool{}
	for _, pattern := range registeredPatterns(t) {
		registered[pattern] = true
		if !strings.HasPrefix(pattern, "POST ") {
			continue
		}
		var in []string
		if _, plugin := requests[pattern]; plugin {
			in = append(in, "pluginRequests")
		}
		if _, other := routesWithNoPluginBody[pattern]; other {
			in = append(in, "routesWithNoPluginBody")
		}
		if routesReadingNoBody[pattern] {
			in = append(in, "routesReadingNoBody")
		}
		if len(in) != 1 {
			t.Errorf("%s is in %v of pluginRequests, routesWithNoPluginBody and routesReadingNoBody, want exactly one", pattern, in)
		}
	}
	for _, route := range slices.Concat(slices.Collect(maps.Keys(requests)), slices.Collect(maps.Keys(routesWithNoPluginBody)),
		slices.Collect(maps.Keys(routesReadingNoBody))) {
		if !registered[route] {
			t.Errorf("%s is no route NewServer registers", route)
		}
	}
	// A removed route answers every call with its refusal and reads no body, so no body an older
	// plugin sends it is refused as an unknown member.
	for route := range RemovedRoutes() {
		if !routesReadingNoBody[route] {
			t.Errorf("requests.go's RemovedRoutes declares %s removed, and routesReadingNoBody does not hold it: a removed route reads no body", route)
		}
	}
}

// requestDecodedPair is one body a route of this run decoded: the route's pattern and the type.
type requestDecodedPair struct {
	pattern string
	into    reflect.Type
}

// requestsDecoded is every pair the tests of this package have made, read once the run ends
// (requestDecodedProblems, called by TestMain), as answersWritten is.
var requestsDecoded = struct {
	mu    sync.Mutex
	pairs map[requestDecodedPair]bool
}{pairs: map[requestDecodedPair]bool{}}

func init() {
	requestDecoded = func(pattern string, into reflect.Type) {
		requestsDecoded.mu.Lock()
		defer requestsDecoded.mu.Unlock()
		requestsDecoded.pairs[requestDecodedPair{pattern: pattern, into: into}] = true
	}
}

// requestDecodedProblems is every body this run decoded into a type its route's row of
// pluginRequests does not name, every body decoded on a route of routesReadingNoBody, and every
// body decoded on a route no table names. A decode with no pattern is a handler a test called
// directly, which names no route to hold.
func requestDecodedProblems() []string {
	requestsDecoded.mu.Lock()
	defer requestsDecoded.mu.Unlock()
	problems := []string{}
	for pair := range requestsDecoded.pairs {
		if pair.pattern == "" {
			continue
		}
		if routesReadingNoBody[pair.pattern] {
			problems = append(problems, fmt.Sprintf(
				"%s decoded a body into %s, and routesReadingNoBody says the route reads no body: correct the table, or the route", pair.pattern, pair.into))
			continue
		}
		if _, other := routesWithNoPluginBody[pair.pattern]; other {
			continue
		}
		want, plugin := pluginRequests()[pair.pattern]
		if !plugin {
			problems = append(problems, fmt.Sprintf(
				"%s decoded a body into %s, and none of pluginRequests, routesWithNoPluginBody and routesReadingNoBody names the route", pair.pattern, pair.into))
			continue
		}
		if pair.into != want {
			problems = append(problems, fmt.Sprintf(
				"%s decoded a body into %s, and pluginRequests names %s: the request gate walks a type the route does not read",
				pair.pattern, pair.into, want))
		}
	}
	slices.Sort(problems)
	return problems
}
