package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/apifixture"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The numbers of the numbered entries are written where every reader with no Go toolchain reads
// them: `released-contracts.json`, a JSON list of SupportedContracts' frozen list
// (frozenContracts), the numbered entries from ServingFloor up, in strictly ascending order. Its
// greatest number is L, to which `scripts/daemon-contract.sh`, `daemonContract()` of
// packages/contracts and `legion-pins` add the count of entry fixtures to read a commit's
// contract, and each of them refuses a file of another form. The list never moves, so neither
// does the file; a new entry changes neither.
func TestReleasedContractsGolden(t *testing.T) {
	if greatest := frozenContracts[len(frozenContracts)-1]; greatest != LastNumberedContract {
		t.Errorf("the frozen list ends at %d, want L (%d)", greatest, LastNumberedContract)
	}
	if !*updateGolden {
		path := apifixture.Path("released-contracts.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (run `go test ./internal/api/ -update`): %v", path, err)
		}
		var onFile []int
		if err := json.Unmarshal(raw, &onFile); err != nil || onFile == nil {
			t.Fatalf("%s is no JSON list of integers (%v), the one form its readers take", path, err)
		}
		for at := 1; at < len(onFile); at++ {
			if onFile[at] <= onFile[at-1] {
				t.Errorf("%s lists %d after %d: its readers take a list in strictly ascending order", path, onFile[at], onFile[at-1])
			}
		}
	}
	golden(t, "released-contracts.json", frozenContracts)
}

// The closed set of causes a claim can fail under is two hand-kept lists — the constants of
// `supervise.FailureCause` and the `LEGION_FAILURE_CAUSES` enum of
// `packages/contracts/src/legion-api.ts`, which `failure.cause` and `pendingCause` are read
// through — and a word added to the Go list alone would make every strict plugin refuse the
// whole state body. So the Go roster is written where the contracts suite reads it, as the
// numbered contracts above are: this golden, which `legion-api.test.ts` holds equal to that enum,
// word for word and in order. A cause added to supervise leaves this fixture stale, and the
// fixture written again (`go test ./internal/api/ -update`) then fails the contracts case until
// the enum names the word too.
//
// The eight words are written out here beside it, so a constant renamed on the Go side — which
// would otherwise rewrite the fixture with nobody reading the diff — fails here by its own name.
func TestFailureCausesGolden(t *testing.T) {
	want := []supervise.FailureCause{"launch_refused", "died", "never_registered", "stale_plugin",
		"prompt_retires_exhausted", "provider_error", "cyber_policy", "unrecorded"}
	if !slices.Equal(supervise.FailureCauses, want) {
		t.Fatalf("supervise.FailureCauses = %q, want the wire words %q", supervise.FailureCauses, want)
	}
	golden(t, "failure-causes.json", map[string][]supervise.FailureCause{"causes": supervise.FailureCauses})
}

// Below deploymentRolesContract a reader reads the `role` of a stranded claim, of a waiting wake
// and of a pause stop with an enumeration of Legion's six roles, and the band of that contract
// serves it the claims of claim.Roles alone (shaping.go's withoutDeploymentRoleClaims), whatever
// roles the deployment defines. From that contract the three fields, and a registration's `role`,
// take any role of the role grammar (claim.IsRole), which no list of values records
// (answers.go's enumeratedFields), so no row of EnumeratedFields holds what this test holds. So
// claim.Roles must be those six, named here one by one: a role added to claim.Roles would pass the
// band and reach a reader whose enumeration refuses the whole state for it. And the band is held
// to them: a claim of each of the six is kept in each of the three lists, and a claim of a role
// of the deployment is dropped from each.
func TestEveryRoleAnOlderReaderIsServedIsOneItsEnumerationNames(t *testing.T) {
	olderReaders := []claim.Role{claim.RoleArchitect, claim.RolePlanner, claim.RoleImplementer, claim.RoleTester,
		claim.RoleReviewer, claim.RoleMerger}
	if !slices.Equal(claim.Roles, olderReaders) {
		t.Fatalf("claim.Roles = %v, want the six roles a reader below deploymentRolesContract enumerates, %v: "+
			"withoutDeploymentRoleClaims keeps every role of claim.Roles, so a role added there is served to a reader "+
			"that refuses the whole state for it; serve it down below the contract it arrives at", claim.Roles, olderReaders)
	}
	state := State{Paused: &PausedView{}}
	for _, role := range append(slices.Clone(claim.Roles), "checker") {
		state.StrandedClaims = append(state.StrandedClaims, StrandedClaim{Issue: "LEGION-208", Role: role, State: "failed"})
		state.WakesWaiting = append(state.WakesWaiting, WakeWaiting{Issue: "LEGION-208", Role: role, Kind: "launch"})
		state.Paused.WaitsOn = append(state.Paused.WaitsOn, PauseStopView{Issue: "LEGION-208", Role: role})
	}
	served, moved := withoutDeploymentRoleClaims(state)
	if !moved {
		t.Fatal("withoutDeploymentRoleClaims reports no move on a state carrying a checker's claim in each list")
	}
	kept := map[string][]claim.Role{}
	for _, c := range served.StrandedClaims {
		kept["strandedClaims"] = append(kept["strandedClaims"], c.Role)
	}
	for _, wake := range served.WakesWaiting {
		kept["wakesWaiting"] = append(kept["wakesWaiting"], wake.Role)
	}
	for _, stop := range served.Paused.WaitsOn {
		kept["paused.waitsOn"] = append(kept["paused.waitsOn"], stop.Role)
	}
	for _, list := range []string{"strandedClaims", "wakesWaiting", "paused.waitsOn"} {
		if !slices.Equal(kept[list], claim.Roles) {
			t.Errorf("%s served below deploymentRolesContract names %v, want Legion's six %v and no role of the deployment",
				list, kept[list], claim.Roles)
		}
	}
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
	if supervise.PodUpdateContract > DaemonAPIVersion() {
		t.Fatalf("supervise.PodUpdateContract = %d, above DaemonAPIVersion %d: no claim could answer an update round",
			supervise.PodUpdateContract, DaemonAPIVersion())
	}
}

// OwnerPlanContract is the contract whose entry in version.go added the owner's plan: the routes
// register_plan and stop_task, a completion's returnTo, and the issues' holdReason task-stopped
// and children. The number is pinned, as PodUpdateContract's is, because the declared table records
// the two new hold reasons and the two new held words at it, and the value strips hold them back
// from a caller below it (answers.go's holdReasons and heldReasons): a constant left behind at a
// renumber would serve a plugin that cannot parse them the new values. Above the daemon's own
// contract, no caller could be served them.
func TestTheOwnerPlanContractIsTheContractThatAddedThePlan(t *testing.T) {
	const addedOwnerPlan = 219
	if OwnerPlanContract != addedOwnerPlan {
		t.Fatalf("OwnerPlanContract = %d, want %d, the contract whose entry in version.go added register_plan, stop_task, returnTo and the two new hold reasons",
			OwnerPlanContract, addedOwnerPlan)
	}
	if OwnerPlanContract > DaemonAPIVersion() {
		t.Fatalf("OwnerPlanContract = %d, above DaemonAPIVersion %d: no caller could be served the two new hold reasons",
			OwnerPlanContract, DaemonAPIVersion())
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

// The frozen list of SupportedContracts (answers.go's frozenContracts) is the list of the
// numbered contracts this daemon describes a shape for, and version.go's numbered entries are
// what say which numbered contracts exist. The two are held together in both directions, from the
// floor to L: an entry in that range that the list omits is a contract no answer is shaped for,
// and a number in the list that no entry names is a contract nothing ever shipped under. A number
// with no entry would be the second case, which is why none is in the list. SupportedContracts
// begins with the frozen list, and holds above it the numbers of the new entries alone
// (contracts_test.go's wantNumbered).
func TestSupportedContractsAreTheNumberedEntriesFromTheFloorUp(t *testing.T) {
	entries := numberedEntries(t)
	listed := map[int]bool{}
	for _, contract := range frozenContracts {
		listed[contract] = true
	}
	for contract := ServingFloor; contract <= LastNumberedContract; contract++ {
		if entries[contract] && !listed[contract] {
			t.Errorf("version.go has an entry for %d and SupportedContracts' frozen list leaves it out", contract)
		}
	}
	for _, contract := range frozenContracts {
		if contract < ServingFloor || contract > LastNumberedContract {
			t.Errorf("SupportedContracts' frozen list names %d, outside the floor %d and L (%d)",
				contract, ServingFloor, LastNumberedContract)
		}
		if !entries[contract] {
			t.Errorf("SupportedContracts' frozen list names %d and version.go has no entry for it", contract)
		}
	}
	if supported := SupportedContracts(); !slices.Equal(supported[:len(frozenContracts)], frozenContracts) {
		t.Errorf("SupportedContracts() = %v, want it to begin with the frozen list %v", supported, frozenContracts)
	}
}

// constantsSentence is the sentence of frozenContracts' comment that lists the entries the list
// writes as constants, read with the comment's line breaks folded to spaces: the numbers, then
// "are constants rather than their numbers".
var constantsSentence = regexp.MustCompile(`((?:\d+, )*\d+ and \d+) are constants rather than their numbers`)

// The comment on frozenContracts (answers.go) lists the number of every entry the list writes as
// a constant rather than as its number, and names each of those constants. A number that list
// leaves out reads as an entry written as itself, so a reader who renumbers an entry, or checks
// from the comment which elements a renumber moves, misses it. The list's elements are read out
// of answers.go in their order, so the i-th is frozenContracts[i], and a constant is an element
// that is an identifier.
func TestTheFrozenListsCommentNamesEachConstantItHolds(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "answers.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse answers.go: %v", err)
	}
	var doc *ast.CommentGroup
	var elements []ast.Expr
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || value.Names[0].Name != "frozenContracts" || len(value.Values) != 1 {
				continue
			}
			literal, ok := value.Values[0].(*ast.CompositeLit)
			if !ok {
				t.Fatal("answers.go's frozenContracts is not a composite literal, which this test reads")
			}
			doc, elements = gen.Doc, literal.Elts
		}
	}
	if len(elements) != len(frozenContracts) {
		t.Fatalf("answers.go's frozenContracts literal has %d elements and the list holds %d: this test read the wrong declaration, or none",
			len(elements), len(frozenContracts))
	}
	text := strings.Join(strings.Fields(doc.Text()), " ")
	var constants []int
	for i, element := range elements {
		name, ok := element.(*ast.Ident)
		if !ok {
			continue
		}
		constants = append(constants, frozenContracts[i])
		if !strings.Contains(text, name.Name) {
			t.Errorf("the comment on frozenContracts does not name %s (%d), which the list holds", name.Name, frozenContracts[i])
		}
	}
	if len(constants) == 0 {
		t.Fatal("frozenContracts holds no constant this test read, so the comment is held to nothing")
	}
	match := constantsSentence.FindStringSubmatch(text)
	if match == nil {
		t.Fatalf("the comment on frozenContracts holds no sentence %q, which this test reads", constantsSentence.String())
	}
	var listed []int
	for _, number := range regexp.MustCompile(`\d+`).FindAllString(match[1], -1) {
		contract, err := strconv.Atoi(number)
		if err != nil {
			t.Fatalf("the comment's number %q: %v", number, err)
		}
		listed = append(listed, contract)
	}
	if !slices.Equal(listed, constants) {
		t.Errorf("the comment on frozenContracts lists %v as the entries written as constants, and the list's constants are %v",
			listed, constants)
	}
}
