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
// per route and contract, from this daemon's own contract on. It is the request half of the
// gate whose answer half is contractShapesFixture, and it is forward-only: the records start at
// the contract that wrote the file, and no earlier contract is recorded. A later contract whose
// request type no longer reads a recorded field fails TestEveryRecordedRequestFieldIsReadByName
// by the route, the field and the contract, unless requests.go's AcceptedByName declares the
// field. At this daemon's own contract the record of each route equals the type's field paths
// less the fields AcceptedByName declares for it, so a field the type reads that neither the
// record holds nor a row declares fails there too.
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

// pluginRequests is every route that decodes a body a plugin sends, with the Go type its handler
// decodes that body into. The grants route takes the claim form and the controller form in one
// type, and the controller's two recoveries share one type, so each of those is one row. The
// register route decodes its body leniently (readRegisterBody) and refuses no member at all,
// and it is recorded all the same: a field it stops reading is a field an older plugin's
// registration means something by.
//
// The census in TestMain holds each row to the type the handler decodes in fact
// (requestDecodedProblems), and TestEveryPostRouteIsARequestRowOrSaysWhyNot holds the table to
// the routes the server registers.
var pluginRequests = map[string]reflect.Type{
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
	"POST /legion/v1/waves/release":           reflect.TypeFor[WaveReleaseRequest](),
	"POST /legion/v1/phase/backward":          reflect.TypeFor[PhaseBackwardRequest](),
	"POST /legion/v1/phase/retry":             reflect.TypeFor[PhaseRetryRequest](),
	"POST /legion/v1/signoff":                 reflect.TypeFor[SignOffRequest](),
	"POST /legion/v1/roots/close":             reflect.TypeFor[RootCloseRequest](),
	"POST /legion/v1/children/park":           reflect.TypeFor[ChildRequest](),
	"POST /legion/v1/children/rerun":          reflect.TypeFor[ChildRequest](),
}

// sameBuild is why a route whose body the `legion` command sends needs no record: the command is
// built from the commit the daemon is built from, so its request and the daemon that reads it are
// never of two contracts.
const sameBuild = "the `legion` command sends this body, and it is built from the daemon's own commit, so no older build sends it"

// readsNoBody is why a route that decodes no body needs no record. The census of the bodies this
// run decoded holds such a route to it (requestDecodedProblems).
const readsNoBody = "the route reads no body"

// routesWithNoPluginBody is every POST route the server registers that pluginRequests holds no
// row for, with why no plugin's body reaches it.
var routesWithNoPluginBody = map[string]string{
	"POST /legion/v1/controller/secret":                      sameBuild + " (`legion controller start`)",
	"POST /legion/v1/controller/github-credential":           sameBuild + " (`legion controller start`)",
	"POST /legion/v1/operator/claims":                        sameBuild + " (`legion claims spawn`)",
	"POST /legion/v1/operator/claims/{token}/deliver":        sameBuild + " (`legion claims deliver`)",
	"POST /legion/v1/operator/claims/{token}/retry":          sameBuild + " (`legion claims retry`)",
	"POST /legion/v1/operator/claims/{token}/forget-session": sameBuild + " (`legion claims forget-session`)",
	"POST /legion/v1/operator/claims/{token}/suspend":        readsNoBody,
	"POST /legion/v1/operator/claims/{token}/resume":         readsNoBody,
	"POST /legion/v1/operator/claims/{token}/stop":           readsNoBody,
	"POST /legion/v1/operator/claims/{token}/compact":        readsNoBody,
	"POST /legion/v1/operator/claims/{token}/close":          readsNoBody,
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
	walked := make(map[string][]string, len(pluginRequests))
	for route, typ := range pluginRequests {
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

// readRequestShapes is the fixture as it stands, and false where no file is there yet.
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

// updatedRequestShapes is what the `-update` path writes: the records of this daemon's own
// contract as its request types read now, and every other record exactly as the fixture holds
// it, which is updatedRecords' rule for the answers. A record of an earlier contract is what a
// plugin of that contract sends, and no later run of `-update` rewrites it.
func updatedRequestShapes(onFile, fresh []requestShape) []requestShape {
	records := make([]requestShape, 0, len(onFile)+len(fresh))
	for _, record := range onFile {
		if record.Contract != DaemonAPIVersion {
			records = append(records, record)
		}
	}
	return sortedRequestShapes(append(records, fresh...))
}

// requestShapeProblems is every way the request types this daemon reads fail the records on
// file, at current, the contract the daemon declares:
//
//   - a recorded field that the request type does not read by name and that declared holds no
//     row for;
//   - a recorded field that a row of declared says left at or below the record's contract;
//   - a field the request type reads by name that the route's record of current does not hold
//     and that declared holds no row for: the record of the daemon's own contract is what
//     `-update` writes (requestShapesAt), the type's field paths less the declared ones, and a
//     run without the flag holds it to exactly that, as the answers' records are held to what
//     the daemon serves;
//   - a recorded route that no row of pluginRequests reads;
//   - a row of declared whose field the route's request type does not read by name, so the body
//     the row is written for is refused as an unknown member;
//   - a row of declared whose route is no route of pluginRequests, or whose LeftAt is not a
//     contract above the floor and at or below current, or which says nothing of what ignoring
//     the field means.
func requestShapeProblems(onFile []requestShape, walked map[string][]string, declared []AcceptedField, current int) []string {
	problems := []string{}
	for _, record := range onFile {
		read, routed := walked[record.Route]
		if !routed {
			problems = append(problems, fmt.Sprintf(
				"contract %d sends a body to %s, and no route of this daemon reads one there: a plugin of that contract is answered \"no route\"",
				record.Contract, record.Route))
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
				"%s: `%s` is a field this daemon's request type reads by name, and the record of contract %d in %s does not hold it, and requests.go's AcceptedByName declares no row for it. If it is a field of contract %d's request, record it: run `go test ./internal/api/ -update`. If the type reads it only so that an older plugin's body is read, declare it in AcceptedByName, with the contract it left at and what ignoring it means.",
				record.Route, field, current, requestShapesFixture, current))
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
	return problems
}

// The fixture itself. `-update` writes this daemon's own contract's records and keeps every
// other, as updatedRequestShapes says; a run without the flag holds the file to its own order
// and format, and TestEveryRecordedRequestFieldIsReadByName holds each record to the types.
func TestRequestShapesFixture(t *testing.T) {
	onFile, written := readRequestShapes(t)
	if !*updateGolden {
		if !written {
			t.Fatalf("%s is not written yet: run `go test ./internal/api/ -update` once to write it", requestShapesFixture)
		}
		golden(t, requestShapesFixture, sortedRequestShapes(onFile))
		return
	}
	golden(t, requestShapesFixture, updatedRequestShapes(onFile, requestShapesAt(walkedRequests(t), AcceptedByName, DaemonAPIVersion)))
}

// Every field a recorded contract's plugin sends is a field this daemon's request type reads by
// name, or a row of AcceptedByName says why it no longer needs to be, and every row of that
// table is a field the type does read by name. The records of this daemon's own contract are on
// file too, and each is held to equal the type's field paths less the fields AcceptedByName
// declares for the route: a request type that loses a field before `-update` is red here, and so
// is one that reads a field its record does not hold and no row declares.
func TestEveryRecordedRequestFieldIsReadByName(t *testing.T) {
	if *updateGolden {
		t.Skip("the fixture is being rewritten; run the suite again without -update")
	}
	onFile, written := readRequestShapes(t)
	if !written {
		t.Fatalf("%s is not written yet: run `go test ./internal/api/ -update` once to write it", requestShapesFixture)
	}
	for _, problem := range requestShapeProblems(onFile, walkedRequests(t), AcceptedByName, DaemonAPIVersion) {
		t.Error(problem)
	}
	routes := map[string]bool{}
	for _, record := range onFile {
		if record.Contract == DaemonAPIVersion {
			routes[record.Route] = true
		}
	}
	for _, route := range slices.Sorted(maps.Keys(pluginRequests)) {
		if !routes[route] {
			t.Errorf("%s has no record at contract %d in %s: run `go test ./internal/api/ -update`", route, DaemonAPIVersion, requestShapesFixture)
		}
	}
}

// What the request gate catches, shown by scratch changes to copies of the walked types and the
// declarations. A later contract is current+1: the records on file stay where they are, as
// `-update` leaves them, and the types and the table are read as that contract's.
func TestTheRequestGateCatchesARemovedField(t *testing.T) {
	if *updateGolden {
		t.Skip("the fixture is being rewritten; run the suite again without -update")
	}
	onFile, written := readRequestShapes(t)
	if !written {
		t.Fatalf("%s is not written yet: run `go test ./internal/api/ -update` once to write it", requestShapesFixture)
	}
	const route, field = "POST /legion/v1/roots/close", "reason"
	next := DaemonAPIVersion + 1
	walked := walkedRequests(t)
	if !slices.Contains(walked[route], field) {
		t.Fatalf("%s reads no `%s`; removing it proves nothing", route, field)
	}
	without := maps.Clone(walked)
	without[route] = slices.DeleteFunc(slices.Clone(walked[route]), func(path string) bool { return path == field })

	t.Run("a field leaves a request type with no declaration", func(t *testing.T) {
		// The fixture keeps the record of every contract, and `-update` adds the record of each
		// later contract beside it, so the removal is a problem of every recorded contract whose
		// request holds the field: one problem each, each naming its own contract. One of them
		// names this daemon's own contract, the record `-update` writes.
		var holding []int
		for _, record := range onFile {
			if record.Route == route && slices.Contains(record.FieldPaths, field) {
				holding = append(holding, record.Contract)
			}
		}
		if !slices.Contains(holding, DaemonAPIVersion) {
			t.Fatalf("the record of contract %d in %s holds no `%s` at %s; removing it proves nothing", DaemonAPIVersion, requestShapesFixture, field, route)
		}
		problems := requestShapeProblems(onFile, without, AcceptedByName, next)
		if len(problems) != len(holding) {
			t.Fatalf("the gate reported %d problems, want one naming the field for each recorded contract whose request holds it, %v:\n%s",
				len(problems), holding, strings.Join(problems, "\n"))
		}
		// The gate reads the records in the order of the file, and so does holding.
		for i, contract := range holding {
			t.Logf("the gate reported: %s", problems[i])
			for _, want := range []string{route, "`" + field + "`", fmt.Sprintf("contract %d", contract)} {
				if !strings.Contains(problems[i], want) {
					t.Errorf("the problem reads %q, want it to name %s", problems[i], want)
				}
			}
		}
		own := fmt.Sprintf("contract %d", DaemonAPIVersion)
		if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, own) }) {
			t.Errorf("no problem names %s, this daemon's own contract:\n%s", own, strings.Join(problems, "\n"))
		}
	})

	t.Run("a field leaves the contract and the type reads it by name with a declaration", func(t *testing.T) {
		declared := append(slices.Clone(AcceptedByName), AcceptedField{Route: route, Field: field, LeftAt: next, Ignoring: "a scratch row"})
		if problems := requestShapeProblems(onFile, walked, declared, next); len(problems) != 0 {
			t.Errorf("the gate reported problems for a declared field the type still reads:\n%s", strings.Join(problems, "\n"))
		}
		for _, record := range requestShapesAt(walked, declared, next) {
			if record.Route == route && slices.Contains(record.FieldPaths, field) {
				t.Errorf("the record of contract %d carries `%s`, which the declaration says left at %d", next, field, next)
			}
		}
	})

	t.Run("a declared field the type does not read by name", func(t *testing.T) {
		declared := append(slices.Clone(AcceptedByName), AcceptedField{Route: route, Field: field, LeftAt: next, Ignoring: "a scratch row"})
		problems := requestShapeProblems(onFile, without, declared, next)
		if len(problems) != 1 || !strings.Contains(problems[0], "does not read it by name") {
			t.Fatalf("the gate reported %q, want the one problem saying the type does not read `%s` by name", problems, field)
		}
		t.Logf("the gate reported: %s", problems[0])
	})
}

// What the request gate catches at this daemon's own contract, shown by scratch changes to
// copies of the walked types and the declarations: a field the type reads that the record of
// this contract does not hold is either a field of this contract, which `-update` records, or a
// field read for an older plugin alone, which a row of AcceptedByName declares. With neither,
// the gate names the field, so the row that says why a field is still read cannot vanish
// unseen.
func TestTheRequestGateCatchesAReadFieldItsRecordDoesNotHold(t *testing.T) {
	if *updateGolden {
		t.Skip("the fixture is being rewritten; run the suite again without -update")
	}
	onFile, written := readRequestShapes(t)
	if !written {
		t.Fatalf("%s is not written yet: run `go test ./internal/api/ -update` once to write it", requestShapesFixture)
	}
	walked := walkedRequests(t)
	wantNamed := func(t *testing.T, problems []string, route, field string) {
		t.Helper()
		if len(problems) != 1 {
			t.Fatalf("the gate reported %d problems, want the one naming the field:\n%s", len(problems), strings.Join(problems, "\n"))
		}
		t.Logf("the gate reported: %s", problems[0])
		for _, want := range []string{route, "`" + field + "`", fmt.Sprintf("contract %d", DaemonAPIVersion), "-update", "AcceptedByName"} {
			if !strings.Contains(problems[0], want) {
				t.Errorf("the problem reads %q, want it to name %s", problems[0], want)
			}
		}
	}

	t.Run("a field read for an older plugin loses its declaration", func(t *testing.T) {
		const route, field = "POST /legion/v1/handoff/complete", "ready"
		if _, isDeclared := acceptedRow(AcceptedByName, route, field); !isDeclared {
			t.Fatalf("requests.go's AcceptedByName declares no `%s` on %s; deleting the row proves nothing", field, route)
		}
		declared := slices.DeleteFunc(slices.Clone(AcceptedByName), func(row AcceptedField) bool { return row.Route == route && row.Field == field })
		wantNamed(t, requestShapeProblems(onFile, walked, declared, DaemonAPIVersion), route, field)
	})

	t.Run("a field of this contract the record does not hold yet", func(t *testing.T) {
		const route, field = "POST /legion/v1/roots/close", "scratchField"
		if slices.Contains(walked[route], field) {
			t.Fatalf("%s already reads `%s`; adding it proves nothing", route, field)
		}
		with := maps.Clone(walked)
		with[route] = slices.Sorted(slices.Values(append(slices.Clone(walked[route]), field)))
		wantNamed(t, requestShapeProblems(onFile, with, AcceptedByName, DaemonAPIVersion), route, field)
	})
}

// Every POST route the server registers is a row of pluginRequests or says why no plugin's body
// reaches it, and every route either table names is a registered route. A route added tomorrow
// that reads a plugin's body is red here until its type is recorded.
func TestEveryPostRouteIsARequestRowOrSaysWhyNot(t *testing.T) {
	registered := map[string]bool{}
	for _, pattern := range registeredPatterns {
		registered[pattern] = true
		if !strings.HasPrefix(pattern, "POST ") {
			continue
		}
		_, plugin := pluginRequests[pattern]
		_, other := routesWithNoPluginBody[pattern]
		if plugin == other {
			t.Errorf("%s is in %s of pluginRequests and routesWithNoPluginBody, want exactly one", pattern, map[bool]string{true: "both", false: "neither"}[plugin])
		}
	}
	for _, route := range slices.Concat(slices.Collect(maps.Keys(pluginRequests)), slices.Collect(maps.Keys(routesWithNoPluginBody))) {
		if !registered[route] {
			t.Errorf("%s is no route NewServer registers", route)
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
// pluginRequests does not name, every body decoded on a route whose row of routesWithNoPluginBody
// says it reads no body, and every body decoded on a route neither table names. A decode with no
// pattern is a handler a test called directly, which names no route to hold.
func requestDecodedProblems() []string {
	requestsDecoded.mu.Lock()
	defer requestsDecoded.mu.Unlock()
	problems := []string{}
	for pair := range requestsDecoded.pairs {
		if pair.pattern == "" {
			continue
		}
		want, plugin := pluginRequests[pair.pattern]
		if why, other := routesWithNoPluginBody[pair.pattern]; other {
			if why == readsNoBody {
				problems = append(problems, fmt.Sprintf(
					"%s decoded a body into %s, and routesWithNoPluginBody says %s: correct the reason, or the route", pair.pattern, pair.into, readsNoBody))
			}
			continue
		}
		if !plugin {
			problems = append(problems, fmt.Sprintf(
				"%s decoded a body into %s, and neither pluginRequests nor routesWithNoPluginBody names the route", pair.pattern, pair.into))
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
