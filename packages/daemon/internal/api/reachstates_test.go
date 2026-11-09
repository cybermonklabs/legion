package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// What the two recoveries reach, checked against the predicates that decide it rather than
// against a copy of the sentences. The other two ways this package pins a refusal are each
// blind to half of that: a want constant copied off a route by hand sees a sentence change and
// not a sentence that was never true (wantFreshSessionReaches), and an expectation composed
// from the very constants the route splices sees neither, which is how 377 cases asserted a
// broken sentence against itself (heldSuspensionRefusal, round 25's controllerKeeperName).
// Both faults the suite shipped were of this kind: a reach clause short by `registered`, and a
// clause spliced mid-sentence.
//
// So nothing here is copied from a route. The states are read from the supervise package's
// source, the words for them are built from agentRunning, startsAgain and the machine's own
// onCompact rows (supervise.TakesCompaction), and the refusals are held to those words. One
// test is the exception and says why: it asserts three rendered sentences whole, as literals,
// which is what catches a constant that stopped being a noun phrase.

// supervisePackage is where the states are declared, from this package's directory, which is
// where `go test` runs.
const supervisePackage = "../supervise"

// claimStates is every state a claim can be in, read from the supervise package's source so a
// state added there is a state these tests cover with nothing added here. It is the same set
// by the same means as the table's own cover test (supervise's sealed), and it fails on a
// constant it cannot read rather than passing over it: a state this scan skipped would be a
// state no refusal below is held to.
func claimStates(t *testing.T) []supervise.ClaimState {
	t.Helper()
	entries, err := os.ReadDir(supervisePackage)
	if err != nil {
		t.Fatal(err)
	}
	var states []supervise.ClaimState
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(supervisePackage, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			declared, ok := decl.(*ast.GenDecl)
			if !ok || declared.Tok != token.CONST {
				continue
			}
			states = append(states, claimStatesOf(t, name, declared)...)
		}
	}
	if len(states) == 0 {
		t.Fatalf("read no ClaimState from %s, where they are declared", supervisePackage)
	}
	for _, state := range slices.Concat(supervise.LiveStates(), supervise.RunsNothingStates()) {
		if !slices.Contains(states, state) {
			t.Fatalf("the scan of %s missed %s, which the package itself publishes", supervisePackage, state)
		}
	}
	return states
}

// claimStatesOf is the states one const block declares. A spec that carries neither a type nor
// a value repeats the one above it, so a ClaimState written that way is one this scan would
// not read, and it fails here instead.
func claimStatesOf(t *testing.T, file string, declared *ast.GenDecl) []supervise.ClaimState {
	t.Helper()
	var states []supervise.ClaimState
	claimState := false
	for _, spec := range declared.Specs {
		value, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		named, _ := value.Type.(*ast.Ident)
		switch {
		case named != nil && named.Name == "ClaimState":
			claimState = true
		case claimState && value.Type == nil && len(value.Values) == 0:
			t.Fatalf("%s: %v repeats the ClaimState above it, which this scan does not read", file, value.Names)
		default:
			claimState = false
			continue
		}
		for _, expr := range value.Values {
			literal, ok := expr.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatalf("%s: %v is a ClaimState this scan cannot read", file, value.Names)
			}
			text, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			states = append(states, supervise.ClaimState(text))
		}
	}
	return states
}

// statesTaken is every state a predicate admits, in the order the states are declared, which
// is the order a refusal spells them in.
func statesTaken(all []supervise.ClaimState, takes func(supervise.ClaimState) bool) []supervise.ClaimState {
	var taken []supervise.ClaimState
	for _, state := range all {
		if takes(state) {
			taken = append(taken, state)
		}
	}
	return taken
}

// stateWords is a set of states in the words a refusal gives them: the words `legion claims
// list` prints, joined as "a, b or c".
func stateWords(states []supervise.ClaimState) string {
	words := make([]string, 0, len(states))
	for _, state := range states {
		words = append(words, string(state))
	}
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

// statesNamed is every state the text spells as a word of its own, in the order the states are
// declared. The match is on word boundaries, so "already" names no state.
func statesNamed(text string, all []supervise.ClaimState) []supervise.ClaimState {
	var named []supervise.ClaimState
	for _, state := range all {
		if regexp.MustCompile(`(^|\W)` + regexp.QuoteMeta(string(state)) + `(\W|$)`).MatchString(text) {
			named = append(named, state)
		}
	}
	return named
}

// sentenceNaming is the one sentence of a refusal that names the needle, which is how the
// clause under test is found without reading the rest of the wording.
func sentenceNaming(t *testing.T, refusal, needle string) string {
	t.Helper()
	var found []string
	for _, sentence := range strings.Split(refusal, ". ") {
		if strings.Contains(sentence, needle) {
			found = append(found, sentence)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d sentences of %q name %q, want exactly one", len(found), refusal, needle)
	}
	return found[0]
}

// reachDoor is one of the two recoveries as the sentences are checked against it: the words it
// writes, and the predicates that decide what it reaches. live is the agent it acts on where
// one runs — three states for the fresh session, which suspends a registered agent too, and
// the machine's own two for the compaction, which has no session to ask until the agent is
// ready (agentRunning, supervise.TakesCompaction). stopped is the claim it starts where no
// process runs, which only the fresh session reaches (startsAgain).
type reachDoor struct {
	op      controllerOp
	live    func(supervise.ClaimState) bool
	stopped func(supervise.ClaimState) bool
}

func reachDoors() []reachDoor {
	return []reachDoor{
		{op: freshSessionOp, live: agentRunning, stopped: startsAgain},
		{op: compactOp, live: supervise.TakesCompaction},
	}
}

// reaches is every state a door reaches, live or stopped.
func (d reachDoor) reaches(state supervise.ClaimState) bool {
	return d.live(state) || (d.stopped != nil && d.stopped(state))
}

// Each door's words for the states it reaches are the states its own predicates admit, over
// every state a claim can be in: the sentence every refusal that ends in askAgainLater carries
// (controllerOp.reaches), and the live half the held suspension's refusal writes its own
// sentence around (controllerOp.liveStates). A door that named the other door's set, or a set
// that had gone stale against the predicate, fails here whatever the surrounding wording is.
func TestEachDoorNamesTheStatesItsOwnPredicatesReach(t *testing.T) {
	all := claimStates(t)
	for _, door := range reachDoors() {
		t.Run(door.op.name, func(t *testing.T) {
			live := statesTaken(all, door.live)
			if door.op.liveStates != stateWords(live) {
				t.Errorf("liveStates = %q, want %q, the states %s reaches while an agent runs",
					door.op.liveStates, stateWords(live), door.op.name)
			}
			if clause := "whose agent is " + stateWords(live); !strings.Contains(door.op.reaches, clause) {
				t.Errorf("reaches = %q, want it to carry %q", door.op.reaches, clause)
			}
			if door.stopped != nil {
				if clause := "one that is " + stateWords(statesTaken(all, door.stopped)); !strings.Contains(door.op.reaches, clause) {
					t.Errorf("reaches = %q, want it to carry %q", door.op.reaches, clause)
				}
			}
			want := statesTaken(all, door.reaches)
			if named := statesNamed(door.op.reaches, all); !slices.Equal(named, want) {
				t.Errorf("reaches names %v, want %v: %q", named, want, door.op.reaches)
			}
		})
	}
}

// The refusal of a claim a suspension is held over names the asking door's own live states,
// over every state the hold can find the claim in. The fresh session's are three and the
// compaction's two, so one sentence shared by both doors is short by `registered` at one of
// them, which is the fault this test exists to catch (refuseHeldSuspension). The states are
// read off the rendered sentence by clause, so the check holds whatever the wording around
// them is, and the code each state takes is asserted with them, since the sentence changed and
// the code did not.
func TestTheHeldSuspensionRefusalNamesEachDoorsOwnLiveStates(t *testing.T) {
	all := claimStates(t)
	stopped := statesTaken(all, startsAgain)
	for _, door := range reachDoors() {
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleController} {
			// The fresh session refuses the project's own controller at its door, so no
			// controller claim ever reads its held-suspension refusal (freshSession's
			// NOT_THE_CONTROLLER).
			if role == claim.RoleController && door.op.name == freshSessionOp.name {
				continue
			}
			live := statesTaken(all, door.live)
			token := claim.Token("LEGION-209-" + string(role))
			for _, state := range all {
				t.Run(door.op.name+" over a "+string(role)+" that is "+string(state), func(t *testing.T) {
					recorder := httptest.NewRecorder()
					if refuseHeldSuspension(recorder, token, state, role, door.op) {
						t.Fatal("the refusal reported true, want the false every caller returns as its own answer")
					}
					var failure Failure
					decodeInto(t, recorder, &failure)
					wantCode := claimUnsettled
					if state == supervise.StateWorking {
						wantCode = claimInATurn
					}
					if recorder.Code != http.StatusConflict || failure.Code != wantCode {
						t.Fatalf("refusal = %d %s, want %d %s", recorder.Code, failure.Code, http.StatusConflict, wantCode)
					}

					clauses := strings.SplitN(sentenceNaming(t, failure.Error, startsAgainName(role)), ", and ", 2)
					if len(clauses) != 2 {
						t.Fatalf("the sentence of what reaches the claim is %q, want a clause for each of the two", clauses[0])
					}
					if named := statesNamed(clauses[0], all); !slices.Equal(named, stopped) {
						t.Errorf("%s reaches %v in the refusal, want %v: %q", startsAgainName(role), named, stopped, failure.Error)
					}
					if !strings.Contains(clauses[1], door.op.name) {
						t.Errorf("the second clause is %q, want it to name %s", clauses[1], door.op.name)
					}
					if named := statesNamed(clauses[1], all); !slices.Equal(named, live) {
						t.Errorf("%s reaches %v in the refusal, want %v: %q", door.op.name, named, live, failure.Error)
					}
					want := slices.Concat(stopped, live)
					slices.SortFunc(want, func(a, b supervise.ClaimState) int {
						return slices.Index(all, a) - slices.Index(all, b)
					})
					if named := statesNamed(failure.Error, all); !slices.Equal(named, want) {
						t.Errorf("the refusal names %v, want %v: %q", named, want, failure.Error)
					}
				})
			}
		}
	}
}

// Three whole refusals, written out here rather than composed from the constants the route
// splices into them. A composed expectation cannot see a constant that stopped being a noun
// phrase: round 25 grew controllerKeeperName a trailing clause, every composed case still
// matched, and the rendered sentence read "... suspension holds starts it again instead". The
// state is ready for the two worker rows, which is the hold the clauses of this refusal have
// to state without denying each other: the claim is ready now, held now, and reached by
// neither door while the hold stands.
func TestTheHeldSuspensionRefusalReadsWholeAtEachDoor(t *testing.T) {
	for _, row := range []struct {
		name  string
		op    controllerOp
		token claim.Token
		state supervise.ClaimState
		role  claim.Role
		want  string
	}{
		{
			name:  "a fresh session over a ready worker",
			op:    freshSessionOp,
			token: "LEGION-209-implementer",
			state: supervise.StateReady,
			role:  claim.RoleImplementer,
			want: "LEGION-209-implementer has a suspension held over it, and a fresh session reaches no claim a " +
				"suspension waits over, whatever state it is in. Of the claims with no suspension held over them, " +
				"fresh_session (`legion claims retry --fresh-session`) reaches those that are suspended, failed or " +
				"retired, and a fresh session reaches those whose agent is registered, ready or idle. Ask again " +
				"later; `read_state` (`legion claims list`) shows its state",
		},
		{
			name:  "a compaction over a ready worker",
			op:    compactOp,
			token: "LEGION-209-implementer",
			state: supervise.StateReady,
			role:  claim.RoleImplementer,
			want: "LEGION-209-implementer has a suspension held over it, and a compaction reaches no claim a " +
				"suspension waits over, whatever state it is in. Of the claims with no suspension held over them, " +
				"fresh_session (`legion claims retry --fresh-session`) reaches those that are suspended, failed or " +
				"retired, and a compaction reaches those whose agent is ready or idle. Ask again later; " +
				"`read_state` (`legion claims list`) shows its state",
		},
		{
			name:  "a compaction over the project's own controller, still launching",
			op:    compactOp,
			token: "INFRA-controller",
			state: supervise.StateLaunching,
			role:  claim.RoleController,
			want: "INFRA-controller has a suspension held over it, and a compaction reaches no claim a suspension " +
				"waits over, whatever state it is in. Of the claims with no suspension held over them, the daemon's " +
				"controller keeper (whose resume an operator's own suspension holds) reaches those that are " +
				"suspended, failed or retired, and a compaction reaches those whose agent is ready or idle. Ask " +
				"again later; `read_state` (`legion claims list`) shows its state",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			refuseHeldSuspension(recorder, row.token, row.state, row.role, row.op)
			var failure Failure
			decodeInto(t, recorder, &failure)
			if failure.Error != row.want {
				t.Errorf("refusal = %q, want %q", failure.Error, row.want)
			}
		})
	}
}
