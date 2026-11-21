package api

// This file is the serving-down, the half of this package that answers one question: what does
// the caller's own plugin read? It holds the contract header a caller states, the reading of the
// pod a caller that states none is in, the shape of every answer at every contract this daemon
// serves, and the one writer each answer goes through. The table it reads is answers.go's, beside
// it; server.go routes the requests and calls writeAnswer and writeAnswerFrom for every body it
// answers, and reads lowestPluginContract for the number the state carries.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strconv"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// pluginContractHeader is the request header a caller states the contract its own plugin speaks
// in (version.go's 193). A caller that states one is placed by what it states and by nothing
// else, whatever address the request came from, which is how a caller the daemon can place in no
// pod — a pane, an operator's command, a process of a pod whose claims record nothing — reaches
// the body its own reader parses.
const pluginContractHeader = "Legion-Plugin-Contract"

// statedContract is the contract the caller states in pluginContractHeader, and false where it
// states none or states what is no positive number. The header refuses nothing: a caller whose
// header this cannot read is served exactly as one that states nothing is.
func statedContract(r *http.Request) (int, bool) {
	stated := r.Header.Get(pluginContractHeader)
	if stated == "" {
		return 0, false
	}
	contract, err := strconv.Atoi(stated)
	if err != nil || contract <= 0 {
		return 0, false
	}
	return contract, true
}

// placement is where contractsFor placed a caller: the pair its answer is shaped at, and what a
// narrowing line names of the caller (recordShaping).
type placement struct {
	// restore is the contract whose removed keys are written back into the body, and strip the
	// contract whose later keys are left out of it. shapeAnswer applies both.
	restore, strip int
	// pod is the pod the caller was placed by, and empty for a caller its header placed or one
	// placed in no pod.
	pod string
	// contract is the caller's own contract as a band's line names it: the one its header
	// states, or the highest its pod records. It is zero for a caller served this daemon's own
	// body, which no band narrows and no line names.
	contract int
}

// ownBody is the placement of a caller served the body this contract writes, neither restored
// nor stripped.
var ownBody = placement{restore: DaemonAPIVersion, strip: DaemonAPIVersion}

// contractsFor is the placement of the caller of r: restore, the contract whose removed keys are
// written back into its answer, and strip, the contract whose later keys are left out of it.
//
// A caller that states a contract (pluginContractHeader) decides both numbers itself. A caller
// that states none keeps today's reading of its pod: restore is the lowest contract the claims
// whose locator names that pod record, and strip the highest of the same set. A claim recording
// no contract moves neither; a claim with no locator, in another pod, or recording a contract
// that states itself in the header is in no pod's reckoning at all, which is why a retired,
// failed or suspended claim counts for nothing and why a process of this change's own build
// counts for nothing either (podContracts). A caller the daemon cannot place in a pod — every
// caller under the tmux runtime, where Options.CallerPod is nil — and one whose pod holds no
// such claim are served the body this contract writes, both numbers DaemonAPIVersion.
//
// The two halves differ because the two kinds of change differ. A key the daemon stopped serving
// is restored at the lowest contract the pod records, which is safe for every process there: a
// reader at a later contract drops a key it does not know (dispatch://INFRA-1048), and a reader
// below the contract that removed the key needs it. A key a later contract added is left out at
// the highest contract the pod records, because the newest process of the pod is the one that
// may require such a key: `check.role` is required by the reader at checkRoleContract and above,
// and `packages/contracts/src/legion-api.ts` reads every other key added between the floor and
// this contract as optional. Strip by the lowest instead and a process that works today stops:
// `check.role` would be taken from the reader that requires it.
//
// Four edges:
//
//   - A contract below ServingFloor takes the floor's shape on the restore side, the lowest
//     shape this daemon describes, and keeps every strip its own number gives it, so such a
//     reader loses no key it loses today.
//   - A contract above DaemonAPIVersion takes the body this contract writes, on both sides.
//   - A contract between the floor and this daemon's own that SupportedContracts does not hold —
//     a number version.go carries no numbered entry for — takes the nearest recorded contract
//     below it, on both sides.
//   - The first and the last write one log line each (servedContracts).
//
// What the shaping takes off the body is written to the log after it is applied, by
// writeAnswerFrom, from the placement this returns and what the shaping reports (recordShaping).
func (s *server) contractsFor(r *http.Request, claims []supervise.Claim) placement {
	if stated, ok := statedContract(r); ok {
		restore, strip := s.servedContracts(r, stated, stated, "")
		return placement{restore: restore, strip: strip, contract: stated}
	}
	// With no claims to read a contract from, no pod reports one: the pair is this daemon's own
	// whatever pod the caller is in, and reading that pod decides nothing. Under the Sandbox
	// runtime the reading lists every cached pod and scans it, which every answer written with
	// no claims would pay for — each plain refusal, each grant, the catch-all's 404 and `GET
	// /healthz` (writeAnswer). The two routes that answer from the state's own snapshot hand their
	// claims in and are placed by their pod as before, and so is every answer placedByPod names,
	// whose claims writeAnswer reads for it (placingClaims).
	if len(claims) == 0 {
		return ownBody
	}
	// A daemon that does not know its callers' pods serves every caller the body this contract
	// writes, and scans nothing for it: under runtime tmux that is every request of every pane
	// and of `legion state`.
	if s.callerPod == nil {
		return ownBody
	}
	caller, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return ownBody
	}
	pod, inPod := s.callerPod(caller.Addr().Unmap())
	if !inPod {
		return ownBody
	}
	lowest, highest, recorded := podContracts(claims, pod)
	if lowest == 0 {
		return ownBody
	}
	restore, strip := s.servedContracts(r, lowest, highest, recorded)
	return placement{restore: restore, strip: strip, pod: pod, contract: highest}
}

// servedContracts is the pair lowest and highest are served at, the four edges of contractsFor
// applied, and the one log line each of the outer two writes. recorded is the claim the lowest
// was read from, and empty for a caller that stated its contract itself, which no claim answers
// for.
func (s *server) servedContracts(r *http.Request, lowest, highest int, recorded claim.Token) (restore, strip int) {
	if lowest < ServingFloor {
		args := []any{"pluginContract", lowest, "servingFloor", ServingFloor}
		if recorded == "" {
			args = append(args, "caller", r.RemoteAddr)
		} else {
			args = append(args, "claim", string(recorded))
		}
		s.log.Warn("api: serving a caller below the lowest contract this daemon shapes an answer for", args...)
	}
	if highest > DaemonAPIVersion {
		s.log.Warn("api: serving a caller above this daemon's own contract",
			"pluginContract", highest, "daemonApiVersion", DaemonAPIVersion, "caller", r.RemoteAddr)
	}
	return restoreContract(lowest), stripContract(highest)
}

// restoreContract is the contract a caller at n has removed keys restored at: the floor for a
// caller below it, which is the lowest shape this daemon describes, and otherwise the recorded
// contract n itself takes.
func restoreContract(n int) int {
	if n < ServingFloor {
		return ServingFloor
	}
	return recordedContractAtOrBelow(n)
}

// stripContract is the contract a caller at n has later keys left out at: its own number for a
// caller below the floor, which keeps every strip that number gives it today, and otherwise the
// recorded contract n itself takes.
func stripContract(n int) int {
	if n < ServingFloor {
		return n
	}
	return recordedContractAtOrBelow(n)
}

// recordedContractAtOrBelow is n itself where SupportedContracts records it, the nearest recorded
// contract below n where it does not (a number between two entries takes the lower), and
// DaemonAPIVersion for an n above this daemon's own. Its callers hand it nothing below
// ServingFloor.
func recordedContractAtOrBelow(n int) int {
	if n >= DaemonAPIVersion {
		return DaemonAPIVersion
	}
	nearest := ServingFloor
	for _, recorded := range SupportedContracts {
		if recorded <= n && recorded > nearest {
			nearest = recorded
		}
	}
	return nearest
}

// shapeAnswer is body, the answer id names, as the plugin that asked for it reads it: the keys
// this daemon stopped writing put back at restore, and the keys it began writing after strip
// left out again (contractsFor decides the two).
//
// Every answer of the table has a row here, and the rows of the answers whose shape has not
// moved since ServingFloor are written out one by one rather than left to a default. A shape
// change that lands with no row of its own is then an answer this function does not name, which
// TestShapeAnswerShapesEveryAnswer catches, where a default would quietly serve the new key to a
// reader that refuses the whole body for it. A body of a type its row does not name is the same
// mistake and ends the same way, in a panic: a route writing a State under AnswerIssueRecord
// would otherwise have that body served unshaped and without a sound.
//
// The state's row is stateFor, and it carries the reasoning of every serving-down this daemon
// makes. A plugin older than dispatch://INFRA-1048 — which is who this is kept for — reads the
// state with a strict schema and refuses the whole body for one key it does not know, where
// today's plugin strips one; and an update round serves a claim of the contract before it until
// its pod is replaced (version.go's 46). Which keys leave the body, and below which contract
// each of them leaves it, is one row per band of stateNarrowings: stateFor takes off the
// body's own keys, shapeIssue each issue's, and recordShaping writes what left. That table is
// the list, so no prose here counts the bands. The bands compose: a caller below all of them is
// served a body carrying no key of any row, each key absent rather than empty, since a strict
// reader before the contract that added a key refuses it whether or not it names anything.
// Every later reader takes those keys as optional — `packages/contracts/src/legion-api.ts`
// marks them optional for exactly these bodies.
//
// The rows of valueStrips take a value rather than a key, and they compose with the bands. A
// strict reader refuses the whole body for a value its enumeration does not name as surely as
// for a key it does not know, so each row takes off the values of one field that restore's
// reader does not admit, read from the declared table (answers.go's EnumeratedFields;
// fieldArrivals.heldBack says which values and why). valueStrips is the list of the strips, as
// stateNarrowings is of the bands. A caller at this daemon's own contract admits every value the
// table declares and loses none. A value the table declares no arrival for is read as arriving
// at this daemon's own contract, so every caller below it is served the body without that value.
//
// The record's row is shapeIssue, the per-issue half of the state's own row: the record route
// answers the very value the whole-project body holds under `issues.<key>` (issueRecordRoute),
// so one function shapes the issue a caller reads on its own and the issue it reads inside the
// whole body, and the two cannot part company.
//
// Two more rows take keys off, below pauseContract, which added keys to two answers beside the
// state and the record: the release records' row serves every resume without its `pause`
// (withoutResumePauses), and the refusal's row serves a PauseFailure as its Failure alone, with
// no `setting`, `pause` or `lastPause`. Both follow strip, as every band of stateNarrowings
// does, and placedByPod places the caller of both by its pod, so a band here reaches the caller
// of a pod as the state's do. A plain refusal, a claim.Refusal or a Failure, carries no key of
// that contract and is the identity at every contract.
//
// On the other side, a caller below fixAttemptsContract is served the two keys that contract
// removed, each as a stand-in (withFixAttempts, withMaxFixAttempts). A settings key no reader
// types is owed neither a stand-in nor a band, whatever a contract did to it (answers.go's
// typedSettings). Any caller at this daemon's own contract — a pod at it, the operator's `legion
// state`, a tmux pane — is served the state as this contract writes it, neither restored nor
// stripped.
//
// It reports what it took off the body beside the body itself. The state's row fills it on the
// one application it makes, and every other row reports nothing, the record's, the release
// records' and the refusal's included: the log holds a line for the state alone
// (recordShaping), so those routes record nothing.
func shapeAnswer(id AnswerID, body any, restore, strip int) (any, tookOff) {
	var took tookOff
	switch id {
	case AnswerState:
		state, isState := body.(State)
		if !isState {
			panic(fmt.Sprintf("api: the %s answer was written a %T, which its row cannot shape", id, body))
		}
		return stateFor(state, restore, strip, &took), took
	case AnswerIssueRecord:
		issue, isIssue := body.(Issue)
		if !isIssue {
			panic(fmt.Sprintf("api: the %s answer was written a %T, which its row cannot shape", id, body))
		}
		return shapeIssue(issue, restore, strip, nil), took
	case AnswerObservedState:
		observed, isObserved := body.(ObservedState)
		if !isObserved {
			panic(fmt.Sprintf("api: the %s answer was written a %T, which its row cannot shape", id, body))
		}
		return observedFor(observed, restore, strip), took
	case AnswerReleaseRecords:
		records, isRecords := body.(ReleaseRecordsResponse)
		if !isRecords {
			panic(fmt.Sprintf("api: the %s answer was written a %T, which its row cannot shape", id, body))
		}
		if strip < pauseContract {
			records = withoutResumePauses(records)
		}
		return records, took
	case AnswerRefusal:
		switch refusal := body.(type) {
		case claim.Refusal, Failure:
			return body, took
		case PauseFailure:
			if strip < pauseContract {
				return refusal.Failure, took
			}
			return refusal, took
		default:
			panic(fmt.Sprintf("api: the %s answer was written a %T, which its row cannot shape", id, body))
		}
	case AnswerHealth, AnswerRegister, AnswerRegisterController, AnswerNoContent,
		AnswerGrant, AnswerHandoffComplete, AnswerEmptyObject, AnswerFreshSession,
		AnswerCompact, AnswerTurnOutcome, AnswerWaveRelease, AnswerProjectPause, AnswerProjectResume:
		// The identity, one row each: every contract this daemon serves one of these answers
		// reads the body it writes. No key has arrived on any of them since that answer arrived.
		// One key has gone: AnswerHandoffComplete lost its `note` at stateMachineContract, and
		// no reader from ServingFloor to 85 requires it, so no stand-in is written for it
		// (version.go's 86). AnswerTurnOutcome arrived whole at stateMachineContract, and
		// AnswerProjectPause and AnswerProjectResume arrived whole at pauseContract, so no older
		// contract records any of the three at all.
		return body, took
	case answerUnparsed:
		// The identity too, and for a reason of its own: no plugin parses these bodies, so no
		// contract describes a shape for them (writeUnparsedAnswer).
		return body, took
	default:
		panic("api: shapeAnswer has no row for the answer " + string(id))
	}
}

// tookOff is what one shaping of the state took off that body: the rows of stateNarrowings and
// of valueStrips that moved it, each by its place in its own table. stateFor fills it, with
// shapeIssue for each issue of the state, on the one application it makes, and recordShaping
// writes a line for each row that is set, so the log names what the body served lost and
// nothing it did not carry.
type tookOff struct {
	// bands is one entry per row of stateNarrowings and values one per row of valueStrips, each
	// nil until a row of its own table moves the body.
	bands  []bool
	values []bool
}

// band is whether the row of stateNarrowings at place moved the body.
func (t *tookOff) band(place int) bool {
	return t.bands != nil && t.bands[place]
}

// value is whether the row of valueStrips at place moved the body.
func (t *tookOff) value(place int) bool {
	return t.values != nil && t.values[place]
}

// applyBand is narrow applied to body, with the row at place recorded in t where narrow reports
// that it took something off. The report is the band's own, read from the fields it clears
// (stateNarrowing), so no two bodies are compared to find it. t is nil on the record route,
// which records nothing.
func applyBand[T any](t *tookOff, place int, narrow func(T) (T, bool), body T) T {
	narrowed, moved := narrow(body)
	if moved && t != nil {
		t.bands = movedAt(t.bands, len(stateNarrowings), place)
	}
	return narrowed
}

// applyValueStrip is take applied to body at restore, with the row of valueStrips at place
// recorded in t where take reports that it took a value off, as applyBand records a band. The
// report is the strip's own (valueStrip). t is nil on the record route, which records nothing.
func applyValueStrip[T any](t *tookOff, place int, take func(T, int) (T, bool), body T, restore int) T {
	taken, moved := take(body, restore)
	if moved && t != nil {
		t.values = movedAt(t.values, len(valueStrips), place)
	}
	return taken
}

// movedAt is moved with the entry at place set, one entry per row of a table rows long, made
// where it is still nil because no row of that table has moved the body yet.
func movedAt(moved []bool, rows, place int) []bool {
	if moved == nil {
		moved = make([]bool, rows)
	}
	moved[place] = true
	return moved
}

// stateFor is the state's row of shapeAnswer: the keys restore's reader requires written back,
// and the keys strip's reader refuses left out, with what left recorded in took.
//
// The body's own keys are shaped here and every key of an issue in shapeIssue, which the record
// route serves one value of on its own (AnswerIssueRecord). One pass over the issue map applies
// every per-issue serving-down, so the map is copied once however many of them a caller takes,
// and a caller at this daemon's own contract, whom no row of either function moves, is served
// the map the source answered.
//
// Each band is a row of stateNarrowings and each value strip a row of valueStrips, applied from
// its table, so a band or a strip this function serves is one the log records and its table
// names (recordShaping). The lines beside the loops are the keys a contract removed, written
// back at restore, which no table holds a row for.
//
// The claims a caller's contract was read from are the state's own snapshot's, because the body
// is: a claim and the state it was projected into cannot come from two moments (store.ClaimsTx).
// A state whose claims the source could not read is no state at all — its claim views are built
// from them — so the route refuses it before this runs, and no readable state is left whose
// caller's contract is unknown.
func stateFor(state State, restore, strip int, took *tookOff) State {
	// The mark says which shape this body took: the contract this binary declares and serves,
	// and the contract the shaping served the caller. The second is written for every caller,
	// one at this daemon's own contract too. strip is never above DaemonAPIVersion
	// (recordedContractAtOrBelow), so the number is always the contract whose shape the caller
	// gets, and never a number the caller only stated. The mark has the same two keys for a
	// caller at this daemon's own contract as for a caller below it, so the record of a contract
	// does not change when a later contract lands above it and serves that caller a body below
	// the daemon's own.
	// The mark is written before anything is taken out, so the band that leaves it off is the
	// pair of the key (withoutContractMark) rather than a key written only where it is allowed.
	state.Contract = &ContractView{Daemon: DaemonAPIVersion, Caller: strip}
	if restore < fixAttemptsContract {
		state = withMaxFixAttempts(state)
	}
	for place, narrowing := range stateNarrowings {
		if narrowing.state != nil && strip < narrowing.contract {
			state = applyBand(took, place, narrowing.state, state)
		}
	}
	// The value strips follow restore where every band above follows strip (contractsFor says
	// what each number is, and fieldArrivals.heldBack why a value follows restore). This loop
	// runs the rows' halves of the body's own values, and shapeIssue the halves of an issue's.
	// Each strip reports whether it took anything, and takes nothing from a caller at this
	// daemon's own contract, whose reader admits every value the table declares.
	for place, row := range valueStrips {
		if row.state != nil {
			state = applyValueStrip(took, place, row.state, state, restore)
		}
	}
	// Every row of shapeIssue is below this daemon's own contract, so a caller at it takes none
	// of them and is served the issue map the source answered, with no copy made of it.
	if restore < DaemonAPIVersion || strip < DaemonAPIVersion {
		issues := make(map[string]Issue, len(state.Issues))
		for key, issue := range state.Issues {
			issues[key] = shapeIssue(issue, restore, strip, took)
		}
		state.Issues = issues
	}
	return state
}

// shapeIssue is one issue of the state as the caller reads it, with what left recorded in took:
// the per-issue half of stateFor, and the record's row of shapeAnswer, since the record route
// answers the very value the whole-project body holds for that issue (issueRecordRoute). The two
// readings of one issue are shaped here and nowhere else, so a caller served the state and a
// caller served the record read the same issue field for field at every contract. took is nil
// on the record route, which records nothing (shapeAnswer).
//
// Each band is a row of stateNarrowings and each value strip a row of valueStrips, as for the
// body's own keys, and the line beside the loops is of the same kind: the keys a contract
// removed, written back at restore.
//
// The issue is a value of its own, and each pointer and map it carries is copied before it is
// written, so the state the source answered is left as it was.
func shapeIssue(issue Issue, restore, strip int, took *tookOff) Issue {
	if restore < fixAttemptsContract {
		issue = withFixAttempts(issue)
	}
	for place, narrowing := range stateNarrowings {
		if narrowing.issue != nil && strip < narrowing.contract {
			issue = applyBand(took, place, narrowing.issue, issue)
		}
	}
	// The rows' halves of an issue's values, keyed to restore for the reason
	// fieldArrivals.heldBack gives, and no band of their own: the table says which values this
	// caller's reader admits, and a caller at this daemon's own contract admits every value the
	// table declares. Each strip reports whether it took something. They run after the bands, so
	// a caller whose strip is below whyAndLineFieldsContract has lost `kind` with the rest of the
	// line's fields already, and withoutWaitingOnIssue finds no word to take from it.
	for place, row := range valueStrips {
		if row.issue != nil {
			issue = applyValueStrip(took, place, row.issue, issue, restore)
		}
	}
	return issue
}

// observedFor is the observed view's row of shapeAnswer (AnswerObservedState): observed as a
// caller at restore and strip reads it, narrowed by the very bands and value strips stateFor
// applies to the whole state. The view is read as the part of a whole state it is (stateOfObserved),
// shaped by stateFor, and read back (observedOf), so at every contract each member of the view is
// the whole state's member at that contract, and a band a later contract adds to a member the
// view carries reaches the view with no line here. The whole state's mark (ContractView) is no
// member of the view and is left out with the rest of the whole state's keys.
//
// A caller at this daemon's own contract is served the view the source answered, with no copy
// made of it: stateFor applies no band to it, and no value strip takes anything from it. That is
// every read Dispatch makes, since it states no contract and asks from no pod. The shaping
// records nothing in the log, where recordShaping writes the whole state's lines alone.
func observedFor(observed ObservedState, restore, strip int) ObservedState {
	if restore >= DaemonAPIVersion && strip >= DaemonAPIVersion {
		return observed
	}
	return observedOf(stateFor(stateOfObserved(observed), restore, strip, nil))
}

// stateOfObserved is observed as the part of a whole state it is: its daemon block, its settings,
// its admission block, and each issue with the members the view carries, its slot holding its
// index alone and each claim view its state alone. observedOf reads it back to the same view.
func stateOfObserved(observed ObservedState) State {
	issues := make(map[string]Issue, len(observed.Issues))
	for key, view := range observed.Issues {
		issue := Issue{Key: view.Key, Phase: view.Phase, Tree: view.Tree, WaitingOn: view.WaitingOn}
		if view.Slot != nil {
			issue.Slot = &SlotView{Index: view.Slot.Index}
		}
		if view.Architect != nil {
			issue.Architect = &ClaimView{State: view.Architect.State}
		}
		if len(view.Workers) > 0 {
			issue.Workers = make(map[claim.Role]PhaseView, len(view.Workers))
			for role, worker := range view.Workers {
				issue.Workers[role] = PhaseView{Claim: ClaimView{State: worker.Claim.State}}
			}
		}
		issues[key] = issue
	}
	return State{Daemon: observed.Daemon, Settings: observed.Settings, Admission: observed.Admission, Issues: issues}
}

// narrowedTheState is the message of the line the daemon writes for each band it serves the
// state down by, named here because the test that reads the daemon's own log reads the same
// string (TestANarrowedBodySaysSo).
const narrowedTheState = "api: narrowed the state to the caller's plugin contract"

// stateNarrowing is one band of the serving-down: the contract that added its keys to this
// route, what that band takes off the state's own keys, what it takes off each issue of the
// state, and what the log line names as dropped. `state` is nil for a band whose keys are all
// an issue's, and `issue` is nil for a band whose keys are all the body's own. A band that added
// keys to both carries both halves. Each band carries its own `dropped` rather than the contract
// number alone, because a reader of the log who is told only a number has to find out elsewhere
// what left the body.
//
// Each half returns the body it narrowed and whether it took anything off that body. That second
// value decides the band's log line (applyBand, recordShaping). A half reads it from the fields
// it clears, before it clears them: a field that holds a value the wire carries, or a claim view
// that the strip changes. A slice or a map is asked its length and never whether it is nil,
// because `omitempty` keeps an empty one off the wire as it keeps a nil one, and a band that
// reported it would log a key the body never carried. A half that sets a flag to leave a key off
// reports a move on every body it sets the flag on, and each such key is one the body carries
// whatever it holds (MarshalJSON writes it on every body but the one the flag is set on). A band
// that finds nothing to take reports nothing, and no two whole bodies are compared.
type stateNarrowing struct {
	contract int
	state    func(State) (State, bool)
	issue    func(Issue) (Issue, bool)
	dropped  string
}

// stateNarrowings is every band of the state's shape, lowest contract first, which is the order
// stateFor and shapeIssue apply them in and the order recordShaping writes them in: a caller
// below all of them is served a body with every one of these keys gone. The two functions and
// the record read these rows and hold no band of their own, so one row serves a band, records it
// and describes it. TestANarrowedBodySaysSo reads the log of the very request it reads the body
// of.
//
// The order is the history of the route and nothing a band depends on. One pair does touch one
// key: waitingLineContract's band drops the issues' `waitingLine` whole, and
// whyAndLineFieldsContract's takes the fields the record added from inside that same view. They
// still do not depend on their order, because a caller below both is served no `waitingLine` at
// all whichever runs first, and a caller below only the higher one keeps the field with its
// `place` and its `length`, which is what the lower band's own contract describes.
//
// A second pair touches one block, and one band of it writes that block in an older shape
// rather than taking a key out: repositoriesLiveContract's band writes `repositories` as
// contract 152 gave it, keys this daemon no longer writes among them, from the entries of that
// block alone, and repositoryAppsContract's takes `apps` off the entries of that older block.
// Neither depends on the other's order either, because each sets a flag on the block and reads
// nothing another band left, and the block's marshaller reads both flags (RepositoriesView): a
// caller below both is served the older block with no `apps` whichever runs first. A caller below
// repositoryAppsContract is below repositoriesLiveContract too, so the first is never served
// alone. A band that read what another left, or put back a key another takes out, would not be
// sound on these terms, and this table would be the wrong shape for it.
//
// The value strips are no rows of this table: they follow restore rather than strip, and take a
// value rather than a key a contract added, which are questions this table's one number cannot
// ask (valueStrip). They are the rows of valueStrips, applied, recorded and described from there
// as a band is from here (tookOff, recordShaping), so a body served without a value says so in
// the log as a body served without a key does. A value that arrived with its field is this
// table's (fieldArrivals.heldBack says which values and why).
var stateNarrowings = []stateNarrowing{
	{contract: supervise.PodUpdateContract, issue: withoutUpdateRounds,
		dropped: "the claim views' updateRound and updateHead"},
	{contract: memoryContract, issue: withoutMemory,
		dropped: "the issues' memoryGroup, memoryDrawMode and memoryDrawnAt"},
	{contract: treeContract, state: withoutTrees, issue: withoutTree,
		dropped: "admission.trees and the issues' tree"},
	{contract: wakesWaitingContract, state: withoutWakesWaiting, dropped: "wakesWaiting"},
	{contract: checkRoleContract, issue: withoutCheckRole, dropped: "the issues' check role"},
	{contract: gatedStepsContract, issue: withoutGatedSteps, dropped: "the issues' gatedSteps"},
	{contract: stallRecordContract, issue: withoutStallRecord, dropped: "the claim views' step"},
	{contract: waitingLineContract, issue: withoutWaitingLine, dropped: "the issues' waitingLine"},
	{contract: backwardMovesContract, issue: withoutBackwardMoves, dropped: "the workers' backwardMoves"},
	{contract: claimsToRefreshContract, state: withoutClaimsToRefresh, dropped: "claimsToRefresh"},
	{contract: recordFieldsContract, state: withoutContractMark, issue: withoutRecordFields,
		dropped: "the claim views' servingGeneration, " +
			"updateReasons, providerErrorRun, pendingTask, failure and pendingCause, the issues' heldFrom, " +
			"lingerUntil and ancestry, and the contract mark"},
	{contract: whyAndLineFieldsContract, issue: withoutWhyAndLineFields,
		dropped: "the issues' why and " +
			"every field of the waiting line beside place and length"},
	{contract: stateMachineContract, issue: withoutStateMachineFields,
		dropped: "the issues' pullRequest.mergeState, the workers' pass, and the claim " +
			"views' lastTurnStart, lastTurnEnd, lastDeathCause, lastStartFailure and lastReady"},
	{contract: leakCheckCauseContract, issue: withoutLeakCheckCause, dropped: "the claim views' leakCheckCause"},
	{contract: phaseEntriesContract, issue: withoutPhaseEntries, dropped: "the issues' phaseEntry and givenEntry"},
	{contract: heldContract, state: withoutHeld, dropped: "admission.held"},
	{contract: quietTimeContract, issue: withoutQuietTime, dropped: "the issues' treeMovedAt and quietHours"},
	{contract: mergeWaitContract, issue: withoutMergeWait, dropped: "the issues' mergeWait"},
	{contract: assignmentsContract, issue: withoutAssignments, dropped: "the issues' assignments"},
	{contract: entryTimesContract, issue: withoutEntryTimes,
		dropped: "the issues' entryQueuedAt, entryConfirmedAt and architectStalls"},
	{contract: repositoryAppsContract, state: withoutRepositoryApps,
		dropped: "the apps of repositories.mapped and repositories.refused"},
	{contract: pauseContract, state: withoutPause, dropped: "paused and lastPause"},
	{contract: shapedAnswersContract, state: withoutLowestPluginContract, issue: withoutClaimRoundReport,
		dropped: "lowestPluginContract and the claim views' updateState and wake"},
	{contract: repositoriesLiveContract, state: withoutRepositoriesLive,
		dropped: "repositories.live, served as contract 152's owners, mapped, refused and withoutWebhookApp"},
	{contract: stateSeqContract, state: withoutStateSeq,
		dropped: "daemon.stateSeq, daemon.stateId and daemon.stateRun"},
}

// valueStrip is one strip of a value from the state: what it takes off the state's own values,
// what it takes off each issue of the state, and what the log line names as dropped. `state` is
// nil for a strip of an issue's value, and `issue` is nil for a strip of the body's own. Each
// half is handed restore and takes each value of its field that the declared table holds back
// from restore (answers.go's EnumeratedFields; fieldArrivals.heldBack says which values and
// why). It returns the body with whether it took anything, read from the values it takes before
// it takes them. That second value decides the strip's log line as a band's decides the band's
// (applyValueStrip, recordShaping). A half that finds no such value returns the body as it was,
// with no copy made.
type valueStrip struct {
	state   func(State, int) (State, bool)
	issue   func(Issue, int) (Issue, bool)
	dropped string
}

// valueStrips is every strip of a value from the state, in the order recordShaping writes their
// lines, after every band's. stateFor applies the rows' state halves after its bands and
// shapeIssue their issue halves after its own, and neither holds a strip of its own, so one row
// serves a strip, records it and describes it, as a row of stateNarrowings does a band. Every row
// follows restore (fieldArrivals.heldBack says why).
// TestEveryValueStripFollowsThePodsLowestContract reads every row on a mixed pod and fails on a
// row it holds no case for.
var valueStrips = []valueStrip{
	{state: withoutUnadmittedStrandedStates, dropped: droppedStrandedState},
	{issue: withoutUnadmittedWaitAnswer, dropped: droppedWaitAnswer},
	{issue: withoutWaitingOnIssue, dropped: droppedLineKind},
}

// recordShaping writes the daemon's own record of what it left out of the state it answered: one
// line for each band of stateNarrowings that took something off this body, and one for each
// value strip that took something off it, naming the caller, the pod it asked from, the contract
// that band or strip followed, the contract this daemon declares and what left the body. The pod
// is empty for a caller the header placed, which no pod answers for. A band or a strip that found
// nothing to take writes no line: it left the body as it was, and a line for it would say
// something went that the body never carried. The caller chooses the volume of these lines on a
// route that authenticates nobody, which is why a band that moved nothing stays quiet.
//
// The body itself cannot carry the record, which is why the daemon's log holds it: a key added
// to a body served down is exactly what the serving-down exists to prevent, so an absent fact
// and a withheld fact would otherwise read alike (dispatch://INFRA-3083, R12). What the body
// does carry, for a caller at or above recordFieldsContract and for no caller below it, is the
// two numbers these lines name (`contract`, ContractView).
//
// What left is what the shaping itself reported on the one application it made (tookOff), so
// the log and the serving-down cannot part company over what a band or a strip takes. It writes
// for the state alone, the answer stateNarrowings describes. The record route's own body is one
// issue of that state, shaped by the per-issue half of the same bands, and writes no line.
//
// A band's line names the caller's own contract (placement.contract), and a value strip's names
// restore, with a clause under `follows` saying which number it is. For a caller its pod placed,
// those are the highest and the lowest contract the pod records; for a caller that states its
// contract, both are read from the contract it states (servedContracts). An operator reading a
// band line at 85 and a value line at 64 in the one request is reading two rules rather than a
// fault (fieldArrivals.heldBack says why the two differ).
func (s *server) recordShaping(r *http.Request, id AnswerID, at placement, took tookOff) {
	if id != AnswerState {
		return
	}
	for place, narrowing := range stateNarrowings {
		if !took.band(place) {
			continue
		}
		s.log.Info(narrowedTheState, "caller", r.RemoteAddr, "pod", at.pod,
			"pluginContract", at.contract, "daemonApiVersion", DaemonAPIVersion,
			"dropped", narrowing.dropped)
	}
	for place, row := range valueStrips {
		if !took.value(place) {
			continue
		}
		s.log.Info(narrowedTheState, "caller", r.RemoteAddr, "pod", at.pod,
			"pluginContract", at.restore, "daemonApiVersion", DaemonAPIVersion,
			"follows", followsRestore, "dropped", row.dropped)
	}
}

// followsRestore is the clause each value strip's line carries and no band's line does: it
// says that the number beside it is restore, the contract the caller states or else the lowest
// its pod records, and not strip, the contract it states or else its pod's highest, which a key
// follows (contractsFor). The line of a fresh session served the failure contract 103 gave
// carries it too, since that decision follows restore as a value strip does (refusedAfterTheStop).
const followsRestore = "the contract the caller states, else its pod's lowest, which a value follows where a key follows the stated contract, else the pod's highest"

// droppedStrandedState, droppedWaitAnswer and droppedLineKind are what the rows of valueStrips
// name as dropped. They are named rather than written in the rows because the tests that read
// the daemon's log and the mixed pod's body name a row by them (narrowing_test.go,
// shaped_answers_test.go). Each names what its row takes by the table's rule rather than by
// today's value, so the line stays true of a value the table declares later.
const (
	droppedStrandedState = "the entries of strandedClaims in a state this caller does not admit"
	droppedWaitAnswer    = "the answeredAt and answeredBy of each wait answered by a value this caller does not admit"
	droppedLineKind      = "the waiting line's kind of every member of a kind this caller does not admit"
)

// memoryContract is the daemon contract that added the issues' memory group to the state
// (version.go's 49): a plugin before it reads the state with a strict schema that knows none of
// `memoryGroup`, `memoryDrawMode` and `memoryDrawnAt` and refuses the whole body for one of them.
const memoryContract = 49

// treeContract is the daemon contract that made the admission cap count the issues at work rather
// than the trees (version.go's 51), which added `admission.trees`, the root of every live tree,
// and each issue's `tree`, its own tree's root: a plugin before it reads the state with a strict
// schema that knows neither key and refuses the whole body for one of them.
const treeContract = 51

// wakesWaitingContract is the daemon contract that added `wakesWaiting` to the state (version.go's
// 53): a plugin before it reads the state with a strict schema that knows no such key and refuses
// the whole body for it.
const wakesWaitingContract = 53

// gatedStepsContract is the daemon contract that added the issues' gated steps to the state
// (version.go's 55): a plugin before it reads the state with a strict schema that knows no
// `gatedSteps` and refuses the whole body for it.
const gatedStepsContract = 55

// retiredStrandedContract is the daemon contract whose entry in version.go added `retired` to the
// states a stranded claim may be in (version.go's 56): a plugin before it reads
// `strandedClaims[].state` with a strict enum of two values, `suspended` and `failed`, and refuses
// the whole body for a third.
const retiredStrandedContract = 56

// stallRecordContract is the daemon contract that added each claim view's `step`, what the daemon
// last saw that claim's agent do (dispatch://INFRA-1274). A plugin before it is served the state
// without the key: one built before sjawhar/legion#2788 reads it with a strict schema and would
// refuse the whole body for a key it does not name, and one built after strips the key instead.
//
// It is 57 because that is the number DaemonAPIVersion took at the bump this record forced: main
// held 56, INFRA-1480's retired stranded state, which carries no step record, so that branch took
// the first number above it, and the release that carries the record declares that number. This
// daemon's own contract is never below it — DaemonAPIVersion (version.go) declares what this
// binary serves, and that number only rises — so a pod at this contract or at a later one is
// served the record and only a pod still running an older plugin is not.
const stallRecordContract = 57

// waitingLineContract is the daemon contract that added each waiting issue's place in the line
// to the state (version.go's 58). A plugin before it is served the state without the field:
// one built before sjawhar/legion#2788 reads it with a strict schema and would refuse the
// whole body for a key it does not name, and one built after strips the key instead, so the
// strip is what keeps the field from a caller that did not ask for it rather than what keeps
// such a caller reading at all.
const waitingLineContract = 58

// checkRoleContract is the daemon contract that added each production check view's `role`, the
// role the check records as the one that works it (version.go's 54): a plugin before it reads
// the check view with a strict schema that knows no `role` and refuses the whole body for it.
// The boot gate alone covered that reader until this daemon shaped its answer (version.go's 193).
const checkRoleContract = 54

// fixAttemptsContract is the daemon contract that took the fix-attempt count out of the state
// (version.go's 59): each pull request's `fixAttempts`, which counted the heads pushed onto a red
// verdict, and the settings' `max_fix_attempts`, which capped it. A plugin before it parses the
// pull request with a strict schema that requires `fixAttempts` and refuses the whole body
// without it, and every worker's `read_record` parses the state through that reader, so a caller
// below this contract is served both keys as stand-ins (withFixAttempts, withMaxFixAttempts).
// The boot gate alone covered that reader until this daemon shaped its answer (version.go's 193).
const fixAttemptsContract = 59

// backwardMovesContract is the daemon contract that added each worker's counts of the moves back
// its issue has made to the state (version.go's 62). A plugin before it is served the state
// without the field: one built before sjawhar/legion#2788 reads it with a strict schema and
// would refuse the whole body for a key it does not name, and one built after strips the key
// instead, so the strip is what keeps the counts from a caller that did not ask for them rather
// than what keeps such a caller reading at all.
const backwardMovesContract = 62

// claimsToRefreshContract is the daemon contract that added `claimsToRefresh`, how many claims
// the stale-plugin lane still has to relaunch onto this daemon's plugin (version.go's 73). A
// plugin before it is served the state without the key: one built before sjawhar/legion#2788
// reads it with a strict schema and would refuse the whole body for a key it does not name, and
// one built after strips the key instead, so the strip is what keeps the count from a caller
// that did not ask for it rather than what keeps such a caller reading at all. It is the band
// below recordFieldsContract's: a pod at 73 or 74 is served the count and no older pod is.
const claimsToRefreshContract = 73

// recordFieldsContract is the daemon contract that decided every field of the claim record and
// of the issue record, served or withheld (dispatch://INFRA-3083, ClaimRecordFields and
// IssueRecordFields). It added to this route a claim view's `servingGeneration`,
// `updateReasons`, `providerErrorRun`, `pendingTask`, `failure` and `pendingCause`, an issue's
// `heldFrom`, `lingerUntil` and `ancestry`, and the `contract` mark beside the daemon's block,
// which is what its row of stateNarrowings names as dropped. A plugin before it is served the
// state without any of them: one built before sjawhar/legion#2788 reads it with a strict schema
// and would refuse the whole body for a key it does not name, and one built after strips the key
// instead.
//
// The nine arrive at this one number and leave together, so the issue's half takes one pass
// (withoutRecordFields) rather than one for each, and the mark, which is the state's own key,
// leaves under the same number (withoutContractMark).
const recordFieldsContract = 75

// whyAndLineFieldsContract is the daemon contract that added the one rendered answer every issue
// that is not working now carries, `why` (WhyView), and the waiting line's own fields beside it:
// `part`, `kind`, `partPlace`, `partLength`, `holdsSlot`, `since`, `rootRank`, `ownRank`,
// `rootKey` and `reason` on WaitingLineView (version.go's 85). The two halves ride one contract
// and so one serving-down (withoutWhyAndLineFields): they are one answer read two ways, the
// sentence a person reads and the fields a program acts on, one pass of the projection renders
// both from the same facts, and both arrive in this one change, so no caller stands between them
// and no second number could tell them apart. A plugin below 85 is served every issue without
// `why` and every member of the line with its `place` and its `length` alone, the two
// waitingLineContract added — no `partPlace` and no other field of this contract reaches such a
// caller: one built before sjawhar/legion#2788 reads the state with a strict schema and would
// refuse the whole body for a key it does not name, and one built after strips the key instead,
// so the strip is what keeps these fields from a caller that did not ask for them rather than
// what keeps such a caller reading at all.
const whyAndLineFieldsContract = 85

// stateMachineContract is the daemon contract that made the daemon an event-driven state machine
// (version.go's 86, dispatch://INFRA-714). It added seven keys to an issue: each pull request's
// `mergeState`, GitHub's own merge state for the head; each phase worker's `pass`, the passes
// that phase has made through its stretch of the workflow; and every claim view's
// `lastTurnStart`, `lastTurnEnd`, `lastDeathCause`, `lastStartFailure` and `lastReady`. A plugin
// before it reads the state with a schema that names none of the seven and refuses the whole
// body for one of them, so a caller below this contract is served the state without them
// (withoutStateMachineFields).
//
// It removed one key, each pull request's `checksVerdict`. Every reader from ServingFloor to 85
// declared it `nonEmptyString.optional()`, so none of them requires it, and the records of
// those contracts leave it out: no caller is served the key, and no stand-in is written back
// for it, as none is for the `note` 86 took off AnswerHandoffComplete (version.go's 193).
//
// It took one value out of an enumeration too — `awaiting_merge`, from an issue's `phase` and
// from the two fields of the decided record that take the workflow's phases (answers.go's
// `phases` and `workflowPhases`) — and a removed value needs no serving-down. The comment on
// answers.go's `phases` says why.
const stateMachineContract = 86

// leakCheckCauseContract is the daemon contract that added each claim view's `leakCheckCause`,
// the sentence the claim's latest launch try was refused with because the shared public leak
// check could not be installed in the role's pod (version.go's 87, ClaimView.LeakCheckCause). A
// plugin below it is served every claim view without the key: one built before
// sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body for a
// key it does not name, and one built after strips the key instead, so the strip is what keeps
// the sentence from a caller that did not ask for it rather than what keeps such a caller
// reading at all. The contract removed no key and added no value to an enumeration: the
// sentence is free text. It put the same key on the operator claim too, and the operator routes
// are not served down, because their reader is the `legion` built with this daemon.
const leakCheckCauseContract = 87

// phaseEntriesContract is the daemon contract that added each issue's `phaseEntry` and
// `givenEntry`, the entry the role of its current phase stands on and the entry that role was
// given (version.go's 94). A plugin below it is served every issue without the two keys
// (withoutPhaseEntries): one built before sjawhar/legion#2788 reads the state with a strict
// schema and would refuse the whole body for a key it does not name, and one built after strips
// the keys instead, so the strip is what keeps the numbers from a caller that did not ask for
// them rather than what keeps such a caller reading at all. The contract removed no key and
// added no value to an enumeration: the two are numbers. Its other change, the completion's
// refusal code renamed to `HANDOFF_STALE_ENTRY`, is a value of the refusal body's `code`, which
// no reader enumerates.
const phaseEntriesContract = 94

// heldContract is the daemon contract that added `admission.held` to the state (version.go's
// 98): every issue the daemon's record holds in the workflow that stands in neither `active` nor
// `waiting`, each with the one word for why (record.HeldReason) and the issues that hold it. A
// plugin below it is served the admission block without the key (withoutHeld): one built before
// sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body for a
// key it does not name, and refuses `held` emptied as surely as filled. The contract removed no
// key and added no value to an existing enumeration. The row's `reason` is an enumeration of its
// own, and every value of it arrived with the key at this contract (answers.go's heldReasons), so
// the band that takes the key off takes every value of it off too, and no value strip is needed.
// It changed no request a plugin sends.
const heldContract = 98

// processReadContract is the daemon contract at which the two claim recoveries read the claim's
// process (version.go's 104). It moved no key path of any answer. It changed what one answer
// means: a fresh session that stopped the claim and could not start it again answers 200 with
// the refusal in `phaseNotRestarted`, where contract 103 answered the failure of the refused
// step. A caller below it gets that failure (readsTheStopAsAFailure), because its `legion` tool
// description reads a 200 as a claim that runs again. The refusal codes that moved at this
// contract are values of the refusal body's `code`, which no reader enumerates.
const processReadContract = 104

// quietTimeContract is the daemon contract that added each issue's measure of its tree's quiet
// time to the state (contract 111 in version.go): `treeMovedAt`, the newest movement the daemon
// has seen anywhere in that tree, and `quietHours`, how long it has been still. A plugin below
// it is served every issue without the two keys (withoutQuietTime): one built before
// sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body for a
// key it does not name, and one built after strips the keys instead, so the strip is what keeps
// the pair from a caller that did not ask for it rather than what keeps such a caller reading at
// all. The two are one band because they are one measure: a caller served the movement without
// the quiet time beside it, or the other way about, would hold half of what one read of the tree
// said. The contract removed no key and added no value to an enumeration: the two are a time and
// a number. It widened which issues `admission.held` names under `at_work` and `unclassified`,
// and both words arrived with the key at heldContract (answers.go's heldReasons), so no value
// strip is needed. It changed no request a plugin sends.
const quietTimeContract = 111

// shapedAnswersContract is the daemon contract this change lands under (version.go's 193), and
// the contract that added three keys of its own: `lowestPluginContract` on the state, the
// lowest contract among the claims whose locator names a pod and that record a contract
// (recordsContract), and each claim view's `updateState` and `wake`. A plugin before it reads
// the state with a schema that names none of the three and refuses the whole body for one of
// them, so a caller below this contract is served the state without them
// (withoutLowestPluginContract, withoutClaimRoundReport).
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes
// moves that constant, and the contract these keys arrived at stays where it is. It moves only
// with this change's own number, as the arrival contracts of this change's two new answers do
// (answers.go's AnswerIssueRecord and AnswerHealth).
const shapedAnswersContract = 193

// waitingOnIssueContract is the daemon contract that added `waiting_on_issue` to the waiting
// line's `kind` (version.go's 132): a started issue that gave its admission slot back to wait on
// another issue (record.LineWaitingOnIssue). A plugin from 85 up to it reads `kind` with a
// `z.enum` of the four words before it, which refuses a word it does not name and with it the
// whole state body. So a caller whose restore is below it is served such a member without its
// `kind` (withoutWaitingOnIssue), which that schema takes as optional. The number is written
// once, in the declared table (answers.go's lineKinds), and the strip reads it from there.
//
// It is a literal of its own and never shapedAnswersContract: a renumber of this change moves
// that constant, and the contract the word arrived at stays where it is.
const waitingOnIssueContract = 132

// mergeWaitContract is the daemon contract that added `mergeWait`, what a reviewing issue's
// round waits on from GitHub (version.go's 133). A plugin below it is served every issue without
// the object (withoutMergeWait): one built before sjawhar/legion#2788 reads the state with a
// strict schema and would refuse the whole body for a key it does not name, and one built after
// strips the key instead, so the strip is what keeps the object from a caller that did not ask
// for it rather than what keeps such a caller reading at all. The object's `mergeState` and
// `toldState` are enumerated (answers.go's mergeWaitStates), and their words arrived with the
// key at this contract, so no value strip is needed.
//
// It is a literal of its own and never shapedAnswersContract: a renumber of this change moves
// that constant, and the contract the object arrived at stays where it is.
const mergeWaitContract = 133

// assignmentsContract is the daemon contract that added each issue's experiment assignments to
// the state (version.go's 141). A plugin before it is served every issue without the field
// (withoutAssignments): one built before sjawhar/legion#2788 reads it with a strict schema and
// would refuse the whole body for a key it does not name, and one built after strips the key
// instead. The three memory fields keep their own band at memoryContract beside this one, so a
// caller from memoryContract to below this one is served the one experiment's answer in the three
// keys it has read since 49 and no assignments, and a caller below 49 neither. The field's
// `variant` and `mode` ride whatever they hold and are no enumeration, so no value strip is owed.
// `settings.projects.<KEY>.experiments` arrived at this contract too, and no reader types it, so
// no record holds it and no band is owed (answers.go's typedSettings).
//
// It is a literal of its own and never shapedAnswersContract: a renumber of this change moves
// that constant, and the contract the field arrived at stays where it is.
const assignmentsContract = 141

// entryTimesContract is the daemon contract that added each issue's `entryQueuedAt` and
// `entryConfirmedAt`, the time the task of its phase entry was first queued to its role's claim
// and the time that claim's worker confirmed it, and each root's `architectStalls`, the arms of
// the architect-stalled reading that hold for its architect (version.go's 146). A plugin before
// it is served every issue without the three keys (withoutEntryTimes): one built before
// sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body for a
// key it does not name, and one built after strips the keys instead, so the strip is what keeps
// them from a caller that did not ask for them rather than what keeps such a caller reading at
// all. assignmentsContract stands below it with no row of stateNarrowings between the two, so a
// caller in a pod at or above assignmentsContract and below this contract is served
// `assignments` and not these three keys.
//
// It is a literal of its own and never shapedAnswersContract: a renumber of this change moves
// that constant, and the contract the keys arrived at stays where it is.
const entryTimesContract = 146

// repositoryAppsContract is the daemon contract that added `apps`, each role App's installation
// on the repository, to every entry of the state's `repositories.mapped` and, where GitHub
// answered both Apps' reads, to an entry of `repositories.refused` (version.go's 152). A plugin
// before it is served the block with no `apps` on either list (withoutRepositoryApps). The
// readers of 46 (7b4ffe3c) and 48 (45548900) are strict: `legionRepositories` and both lists'
// entries are `z.strictObject` (legion-api.ts:447-457 and 453-463), so the key would refuse the
// whole state. The readers from 49 (e68c26f9, legion-api.ts:464-475) to 151 (d9299f7a,
// legion-api.ts:1055-1066) are `z.object` and strip the key, and none of them types it, so there
// the band keeps the body their own records describe rather than what keeps them reading.
// entryTimesContract stands below it with no row of stateNarrowings between the two, so a caller
// in a pod at or above entryTimesContract and below this contract is served the entry times and
// not `apps`. The contract's other changes are no key of an answer a plugin parses: the
// controller's github-credential answer is read by `legion controller start` alone, and what
// `mapped` and `refused` hold now is read by no plugin code (version.go's 193).
//
// It is a literal of its own and never shapedAnswersContract: a renumber of this change moves
// that constant, and the contract the key arrived at stays where it is.
const repositoryAppsContract = 152

// pauseContract is the daemon contract that added the project pause (version.go's 178,
// dispatch://INFRA-3759). It added keys to three answers a plugin parses: `paused` and
// `lastPause` to the state, `pause` to each resume of the release records (api.ResumeRecord),
// and `setting`, `pause` and `lastPause` to a refusal (api.PauseFailure). It added two answers
// whole with their routes, the controller's pause and resume (answers.go's AnswerProjectPause
// and AnswerProjectResume), which no contract below it records. A plugin before it is served the
// state without its two keys (withoutPause, its row of stateNarrowings), every resume without
// its `pause` (withoutResumePauses) and a PauseFailure as the Failure it carries, those two in
// their answers' rows of shapeAnswer: one built before sjawhar/legion#2788 reads the state with
// a strict schema and would refuse the whole body for a key it does not name, and one built
// after strips the key instead, so each band keeps the body its own contract's record describes.
// The pause's codes are values of the refusal's `code`, which no reader enumerates. The one
// enumerated field it added is `paused.waitsOn.*.role` (answers.go's pauseStopRoles), whose values
// fieldArrivals.heldBack decides as every other field's.
//
// It is a literal of its own and never shapedAnswersContract: a renumber of this change moves
// that constant, and the contract the keys arrived at stays where it is.
const pauseContract = 178

// repositoriesLiveContract is the daemon contract that made the state's `repositories` block the
// repositories the project's live records name (version.go's 202, dispatch://INFRA-4401): `live`,
// one entry per repository with what GitHub and Dispatch answered of it, beside `caches` and
// `withoutWebhookApp`, which it keeps in contract 152's shape for every caller, in place of
// `owners`, `mapped` and `refused`. A plugin before it reads the block with the schema of contract
// 152, which requires those three keys, so the block this contract writes refuses that plugin's
// whole state, as surely as a key a strict reader does not name. That caller is served the block in
// contract 152's shape, written from the same entries (withoutRepositoriesLive), and every other
// caller the block this contract writes. shapedAnswersContract stands below it with no row of
// stateNarrowings between the two, so a caller at or above shapedAnswersContract and below this
// contract, 193 to 201, is served the block in contract 152's shape: one from 193 to 198 is served
// 193's keys, and one from 199 to 201 is served those of 199, the recorded contract at or below
// it, which are the same keys on this route. The block's one enumerated field, the cache's
// `state` at `repositories.live.*.cache.state`, arrived with it (answers.go's liveCacheStates), so
// the band takes every value of it off with the key and no value strip is owed.
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes
// moves that constant, and the contract this block arrived at stays where it is. It moves only
// with this change's own number, and answers.go's SupportedContracts names the constant rather
// than its number for that reason.
const repositoriesLiveContract = 202

// stateSeqContract is the daemon contract that added `daemon.stateSeq`, the sequence of the newest
// state event the project's daemon committed, read in the body's own snapshot, and beside it
// `daemon.stateId`, the id of the store that count is kept in, and `daemon.stateRun`, the id of the
// daemon run that serves the body (version.go's 207, DaemonInfo.StateSeq, StateID and
// StateRun). A plugin before it is served the daemon block without the three keys
// (withoutStateSeq, its row of stateNarrowings): one built before sjawhar/legion#2788 reads the
// state with a strict schema and would refuse the whole body for a key it does not name, and one
// built after strips the keys instead, so the strip is what keeps them from a caller that did not
// ask for them rather than what keeps such a caller reading at all. The same contract added the
// observed view of the state route (AnswerObservedState), which carries the same daemon block and
// is narrowed by the same band (observedFor). repositoriesLiveContract stands below it with no row
// of stateNarrowings between the two, so a caller at or above repositoriesLiveContract and below
// this contract is served `repositories.live` and none of the three keys.
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes moves
// that constant, and the contract these keys arrived at stays where it is.
const stateSeqContract = 207

// withoutGatedSteps is the issue with no `gatedSteps`, the field gatedStepsContract added to the
// state; nothing else of that contract is on the route. The field is `omitempty`, and a root of a
// deployment that gates no step carries an empty list of it (GatedStepViewsOf), which the wire
// leaves off as it leaves off a nil one, so the band reports a move only for a list that holds a
// step.
func withoutGatedSteps(issue Issue) (Issue, bool) {
	moved := len(issue.GatedSteps) > 0
	issue.GatedSteps = nil
	return issue, moved
}

// withoutPhaseEntries is the issue with no `phaseEntry` and no `givenEntry`, the two fields
// phaseEntriesContract added to the state; nothing else of that contract is on the route.
func withoutPhaseEntries(issue Issue) (Issue, bool) {
	moved := issue.PhaseEntry != 0 || issue.GivenEntry != 0
	issue.PhaseEntry, issue.GivenEntry = 0, 0
	return issue, moved
}

// withoutQuietTime is the issue with no `treeMovedAt` and no `quietHours`, the two fields
// quietTimeContract added to the state; nothing else of that contract is on the route.
func withoutQuietTime(issue Issue) (Issue, bool) {
	moved := issue.TreeMovedAt != nil || issue.QuietHours != nil
	issue.TreeMovedAt, issue.QuietHours = nil, nil
	return issue, moved
}

// withoutMergeWait is the issue with no `mergeWait`, the object mergeWaitContract added to the
// state; nothing else of that contract is on the route.
func withoutMergeWait(issue Issue) (Issue, bool) {
	moved := issue.MergeWait != nil
	issue.MergeWait = nil
	return issue, moved
}

// withoutAssignments is the issue with no `assignments`, the one field assignmentsContract added
// to an issue; nothing else of that contract is on the route but a `settings` key, which every
// plugin reads loosely. The three memory fields are left where they are: they are the band of
// memoryContract, and a caller above it reads them whatever it reads here. The field is
// `omitempty` and absent from an issue no experiment answers for, so the band reports a move only
// for a list that holds an answer.
func withoutAssignments(issue Issue) (Issue, bool) {
	moved := len(issue.Assignments) > 0
	issue.Assignments = nil
	return issue, moved
}

// withoutEntryTimes is the issue with no `entryQueuedAt`, `entryConfirmedAt` or
// `architectStalls`, the three fields entryTimesContract added to the state; nothing else of that
// contract is on the route. Like the two entry numbers they are written only where the daemon has
// one to write — an issue whose role holds no task of its entry carries neither time, and a root
// no arm holds for carries no arm — so the band reports a move only for an issue that carries one.
func withoutEntryTimes(issue Issue) (Issue, bool) {
	moved := issue.EntryQueuedAt != nil || issue.EntryConfirmedAt != nil || len(issue.ArchitectStalls) > 0
	issue.EntryQueuedAt, issue.EntryConfirmedAt, issue.ArchitectStalls = nil, nil, nil
	return issue, moved
}

// withoutRepositoryApps is state whose `repositories` block carries no `apps` on any entry of
// `mapped` or `refused`, the key repositoryAppsContract added to both lists; nothing else of that
// contract is a key of this route. Those two lists are the block in the shape contract 152 gave
// it, which the daemon writes from `live` for every caller below repositoriesLiveContract
// (withoutRepositoriesLive), and every caller below this contract is below that one too. So the
// key is left off by a flag on the block (RepositoriesView.appsHidden), which the older block's
// writer reads (RepositoriesView.at152), as Admission.MarshalJSON leaves off `held` for
// withoutHeld. That block carries the key on every entry both role Apps are installed on and on
// every other entry GitHub answered both reads of, which are the live entries that carry `apps`,
// so the band reports a move only for a block one of whose entries does. The flag is set on a
// value of the state's own and the entries are read and never written, so the state the source
// answered is left as it was.
func withoutRepositoryApps(state State) (State, bool) {
	moved := slices.ContainsFunc(state.Repositories.Live, func(entry LiveRepositoryView) bool { return entry.Apps != nil })
	state.Repositories.appsHidden = true
	return state, moved
}

// withoutRepositoriesLive is state with its `repositories` block in the shape contract 152 gave
// it, written from the block's own entries (RepositoriesView.at152): the one change
// repositoriesLiveContract made to this route. The flag is set on a value of the state's own and
// the entries are read and never written, so the state the source answered is left as it was. The
// state writes the block on every body and the older shape replaces it whole, so the band reports
// a move on every body it serves, as withoutTrees does.
func withoutRepositoriesLive(state State) (State, bool) {
	state.Repositories.narrowed = true
	return state, true
}

// withoutWaitingLine is the issue with no `waitingLine`, the field waitingLineContract added to
// the state; nothing else of that contract is on the route.
func withoutWaitingLine(issue Issue) (Issue, bool) {
	moved := issue.WaitingLine != nil
	issue.WaitingLine = nil
	return issue, moved
}

// withoutBackwardMoves is the issue with no worker carrying `backwardMoves`, the one key
// backwardMovesContract added to this route; nothing else of that contract is on it, the
// settings' `hooks` riding under a block every plugin reads loosely. The worker map is copied
// only for an issue a worker of which carries the counts, since the field rides on the
// implementer of an issue that has moved back and on nobody else, so the issues without one are
// left as the source answered them rather than rebuilt.
func withoutBackwardMoves(issue Issue) (Issue, bool) {
	if !issueCarriesBackwardMoves(issue) {
		return issue, false
	}
	workers := make(map[claim.Role]PhaseView, len(issue.Workers))
	for role, worker := range issue.Workers {
		worker.BackwardMoves = nil
		workers[role] = worker
	}
	issue.Workers = workers
	return issue, true
}

// issueCarriesBackwardMoves is whether a worker of this one issue carries the counts. The field
// is written only where there is one — an issue that has had no move back carries no such key —
// so the strip asks this rather than copying every worker map it meets.
func issueCarriesBackwardMoves(issue Issue) bool {
	for _, worker := range issue.Workers {
		if len(worker.BackwardMoves) > 0 {
			return true
		}
	}
	return false
}

// recordsContract is whether c holds a process in a pod and records the contract that process's
// plugin speaks. It is the one test both readings of the claims make, so a caller's pod and the
// fleet count the same claims (podContracts, lowestPluginContract).
//
// A claim with no locator, and one whose locator names no pod, holds no such process:
// suspending, failing or retiring a claim ends what the machine had with its process, and the
// claim gives up its locator with it (supervise's letGo), so such a claim calls nothing and no
// shape is owed to it. A claim recording no contract reports nothing either.
func recordsContract(c supervise.Claim) bool {
	return c.Locator != nil && c.Locator.Sandbox != nil && c.PluginContract != 0
}

// podContracts is the lowest and the highest plugin contract recorded by the claims that run in
// pod and read the body this reading shapes, and the claim the lowest was read from. All three
// are zero where the pod holds no such claim: a claim of another pod, a claim recordsContract
// answers false for, and a claim recording shapedAnswersContract or more are in no pod's
// reckoning.
//
// The last of the three is left out because its process never reads a body this reading shaped.
// Every build at shapedAnswersContract or later states its own contract in
// pluginContractHeader, and contractsFor answers on the header before it reads the pod at all.
// Counting such a claim would shape the pod-decided body for a reader that does not read it and
// break the one that does: in the `{46, 193}` pod a roll makes, the highest would be 193, and the
// 46 process — the one process this body is written for — would be served `admission.trees` and
// the memory fields, which its reader refuses the whole body for. So the pod reading answers for
// the processes that read it and for no others. The fleet's reading counts every claim that
// holds a process, this change's own builds included, because it answers another question
// (lowestPluginContract).
//
// Every role of a pod is served by the plugin that pod runs, so the lowest is the contract whose
// removed keys every counted process there can read, and the highest the contract whose later
// keys the pod's newest counted process requires (contractsFor says why the two halves differ).
func podContracts(claims []supervise.Claim, pod string) (lowest, highest int, recorded claim.Token) {
	for _, c := range claims {
		if !recordsContract(c) || c.Locator.Sandbox.PodUID != pod || c.PluginContract >= shapedAnswersContract {
			continue
		}
		if lowest == 0 || c.PluginContract < lowest {
			lowest, recorded = c.PluginContract, c.Token
		}
		if c.PluginContract > highest {
			highest = c.PluginContract
		}
	}
	return lowest, highest, recorded
}

// lowestPluginContract is the lowest contract the claims that hold a process record, over every
// pod of the project rather than one caller's: the claims recordsContract answers true for, with
// the pod left out of the question podContracts asks. Nil where no such claim exists.
func lowestPluginContract(claims []supervise.Claim) *int {
	lowest := 0
	for _, c := range claims {
		if !recordsContract(c) {
			continue
		}
		if lowest == 0 || c.PluginContract < lowest {
			lowest = c.PluginContract
		}
	}
	if lowest == 0 {
		return nil
	}
	return &lowest
}

// withEachClaimView is the issue with change applied to every claim view it carries: the
// architect's and each worker's, and whether change altered any of them. A view is compared with
// what change made of it by the compiler's own `!=`: every field of ClaimView is a string, a
// number, a bool or a pointer, so the type is comparable, and a build that puts a slice or a map
// on it fails here.
//
// Nothing is copied for a view that change leaves as it was. The architect's view is copied only
// when change alters it, and the worker map only at the first worker whose view change alters,
// so an issue that carries none of the keys a band takes off is returned as the source answered
// it, and the state the source answered is left as it was either way. The map returned is the
// source's own where no worker's view moved, so a caller must not write into it.
func withEachClaimView(issue Issue, change func(ClaimView) ClaimView) (Issue, bool) {
	moved := false
	if issue.Architect != nil {
		if architect := change(*issue.Architect); architect != *issue.Architect {
			issue.Architect, moved = &architect, true
		}
	}
	var workers map[claim.Role]PhaseView
	for role, worker := range issue.Workers {
		changed := change(worker.Claim)
		if changed == worker.Claim {
			continue
		}
		if workers == nil {
			workers = maps.Clone(issue.Workers)
		}
		worker.Claim = changed
		workers[role] = worker
	}
	if workers != nil {
		issue.Workers, moved = workers, true
	}
	return issue, moved
}

// withoutUpdateRounds is the issue with no claim view carrying `updateRound` or `updateHead`, the
// two fields supervise.PodUpdateContract added to the state; nothing else of that contract is on
// the route but `settings` keys, which every plugin reads loosely.
func withoutUpdateRounds(issue Issue) (Issue, bool) {
	return withEachClaimView(issue, func(view ClaimView) ClaimView {
		view.UpdateRound, view.UpdateHead = "", ""
		return view
	})
}

// withoutMemory is the issue with no `memoryGroup`, `memoryDrawMode` or `memoryDrawnAt`, the
// three fields memoryContract added to the state; nothing else of that contract is on the route
// but `settings` keys, which every plugin reads loosely.
func withoutMemory(issue Issue) (Issue, bool) {
	moved := issue.MemoryGroup != "" || issue.MemoryDrawMode != "" || issue.MemoryDrawnAt != nil
	issue.MemoryGroup, issue.MemoryDrawMode, issue.MemoryDrawnAt = "", "", nil
	return issue, moved
}

// withoutTrees is state with no `admission.trees`, the first of the two keys treeContract added;
// each issue's `tree`, the second, is withoutTree's. Nothing else of that contract is a key of
// its own: `active` and `waiting` keep their names and count issues rather than trees, which no
// downgrade can carry, and which the boot gate and `legion probe-image` keep a plugin of the
// contract before this one from reading (version.go's 51). The admission block is a value of the
// state's own, so the state the source answered is left as it was. The flag leaves the key off
// every body it is set on, so the band reports a move on every body it serves.
func withoutTrees(state State) (State, bool) {
	state.Admission.Trees, state.Admission.treesHidden = nil, true
	return state, true
}

// withoutHeld is state with no `admission.held`, the one key heldContract added; nothing else of
// that contract is on the route. The key is left off rather than emptied, because the strict
// reader before the contract refuses it as surely when it is empty as when it names an issue,
// which is why Admission.MarshalJSON writes the key on every body but this one
// (Admission.heldHidden), as it leaves off `trees` for withoutTrees. The admission block is a
// value of the state's own, so the state the source answered is left as it was. The band
// reports a move on every body it serves, as withoutTrees does.
func withoutHeld(state State) (State, bool) {
	state.Admission.Held, state.Admission.heldHidden = nil, true
	return state, true
}

// withoutTree is the issue with no `tree`, the second of the two keys treeContract added
// (withoutTrees carries the first).
func withoutTree(issue Issue) (Issue, bool) {
	moved := issue.Tree != ""
	issue.Tree = ""
	return issue, moved
}

// withoutWakesWaiting is state with no `wakesWaiting`, the top-level array wakesWaitingContract
// added; nothing else of that contract is on the route but `settings` keys, which every plugin
// reads loosely. The key is left off rather than emptied, because the strict reader before the
// contract refuses it as surely when it is empty as when it names a wake, which is why
// State.MarshalJSON writes the key on every body but this one (State.wakesWaitingHidden), as
// Admission.MarshalJSON leaves off `trees` for withoutTrees. The flag is set on a value of the
// state's own, so the state the source answered is left as it was. The band reports a move on
// every body it serves, as withoutTrees does.
func withoutWakesWaiting(state State) (State, bool) {
	state.WakesWaiting, state.wakesWaitingHidden = nil, true
	return state, true
}

// withoutClaimsToRefresh is state with no `claimsToRefresh`, the one key claimsToRefreshContract
// added to this route; nothing else of that contract is on it, the new wake kind riding under
// `wakesWaiting[].kind`, which the schema leaves an unconstrained string. The key is left off
// rather than zeroed, because the strict reader before the contract refuses it when it is 0 as
// surely as when it names a number, which is why State.MarshalJSON writes the count on every
// body but this one (State.claimsToRefreshHidden), as withoutWakesWaiting leaves off
// `wakesWaiting`. The count is zeroed with the flag so the hidden body holds no number a later
// reader of the value could take for one it was served; the flag is set on a value of the
// state's own, so the state the source answered is left as it was. A caller below
// wakesWaitingContract is below this contract too, so both strips run on one body and it
// carries neither key. The band reports a move on every body it serves, as withoutTrees does.
func withoutClaimsToRefresh(state State) (State, bool) {
	state.ClaimsToRefresh, state.claimsToRefreshHidden = 0, true
	return state, true
}

// withoutUnadmittedStrandedStates is state whose report of claims nothing will wake names no
// claim in a state restore's reader does not admit. It follows the declared table (answers.go's
// strandedStates) and drops every entry whose state the table holds back from restore
// (fieldArrivals.heldBack says which states and why): today the `retired`
// retiredStrandedContract added, and tomorrow any state the table gives a later arrival. The
// field is older than the floor, so every reader this daemon serves reads it with an
// enumeration, and the states recorded at the floor are the ones that came with it. Nothing else
// of that contract is on this route, the operator claim view's three delivery fields being
// served where no plugin reads. The entry is dropped rather than rewritten, since its state is
// the claim's own and no older value stands for it, and a reader before that state's arrival
// refuses the whole body for one of them.
//
// It reports whether it took an entry, which is what the log line of this strip is written from
// (tookOff), as withoutUnadmittedWaitAnswer reports the pair it takes. A report that names no
// such claim is returned as it is, with no copy made. Otherwise the slice is copied, so the
// state the source answered is left as it was, and it stays non-nil when every entry goes:
// `strandedClaims` is never null on the wire (state.go's normalisation).
func withoutUnadmittedStrandedStates(state State, restore int) (State, bool) {
	unadmitted := func(c StrandedClaim) bool { return strandedStateArrivals.heldBack(c.State, restore) }
	if !slices.ContainsFunc(state.StrandedClaims, unadmitted) {
		return state, false
	}
	stranded := make([]StrandedClaim, 0, len(state.StrandedClaims))
	for _, c := range state.StrandedClaims {
		if !unadmitted(c) {
			stranded = append(stranded, c)
		}
	}
	state.StrandedClaims = stranded
	return state, true
}

// withoutUnadmittedWaitAnswer is the issue whose wait carries no `answeredAt` and no
// `answeredBy` where that `answeredBy` is a value restore's reader does not admit: one the
// declared table holds back from restore (answers.go's EnumeratedFields; fieldArrivals.heldBack
// says which values and why). Today that is the `declined` of version.go's 68, and tomorrow any
// value the table gives a later arrival. The rule is the table's and not one value's, so a value
// added later is served down by this same strip.
//
// The pair goes and the `waitingOn` block stays, which is where this parts company with
// withoutUnadmittedStrandedStates: a stranded entry has nothing true left in it once its state
// is gone, and most of a wait is true at every contract. A block dropped whole would tell a
// reader below the contract that the root waits on nobody, which is false and which that reader
// cannot tell apart from a root that truly waits on no person; dropping the pair leaves `kind`,
// `ask` or `gateVersion` and `artifactId`, and `since`, every one of them true at that
// contract, and loses only the answer, which is the part no older value spells.
//
// The pair is readable at every contract, measured on `packages/contracts/src/legion-api.ts`:
// `legionWaitTimes` declares `since`, `answeredAt` and `answeredBy` each `.optional()`, and
// `legionWaitingOn`'s `refine` asks only that `answeredAt` and `answeredBy` appear together,
// which dropping both satisfies. A wait nobody has answered carries no pair at all, and that is
// the ordinary body of every `awaiting_person` root at every contract, so a `waitingOn` with no
// answered pair is a shape every reader already parses. The two go together or not at all:
// either one alone refuses the body on that `refine`.
//
// The wait is a pointer of the issue's own, and it is copied before it is written, so the state
// the source answered is left as it was. It reports whether it took the pair, which is what the
// log line of this strip is written from (tookOff).
//
// A caller below the floor is read as a caller at it, and needs no reading here: restoreContract
// hands this the floor for such a caller and no number below it. A value the table records at
// ServingFloor is older than the floor rather than arrived there (answers.go's
// EnumeratedFields), so no reader at all is below such a value's arrival. `answeredBy` is that
// old itself, so those are the values that came with the field.
func withoutUnadmittedWaitAnswer(issue Issue, restore int) (Issue, bool) {
	if issue.WaitingOn == nil || issue.WaitingOn.AnsweredBy == "" {
		return issue, false
	}
	if !waitAnswerArrivals.heldBack(string(issue.WaitingOn.AnsweredBy), restore) {
		return issue, false
	}
	wait := *issue.WaitingOn
	wait.AnsweredAt, wait.AnsweredBy = nil, ""
	issue.WaitingOn = &wait
	return issue, true
}

// withoutWaitingOnIssue is the issue whose waiting line member carries no `kind` where that
// word is one restore's reader does not admit. It follows the declared table (answers.go's
// lineKinds) and takes any word the table holds back from restore (fieldArrivals.heldBack says
// which words and why): today the `waiting_on_issue` of waitingOnIssueContract, and tomorrow any
// word the table gives a later arrival. It is named for the one word it takes today. Every other
// field of the member stays, since each is a field of whyAndLineFieldsContract, and so does every
// member whose word restore's reader admits. `kind` is `omitempty`, so the emptied field writes no
// key, and every reader from 85 up takes `kind` as optional.
//
// The four words of whyAndLineFieldsContract arrived with the field, so they are no strip's:
// they ride the band of that contract, which takes `kind` with the rest of the line's fields
// from a caller whose strip is below 85 (withoutWhyAndLineFields). So a pod recording 46 and 133
// is served each member's `kind` where it is one of those four, since its 133 process reads them
// and its 46 process knows no `kind` key to refuse them by, and no `kind` on a member that waits
// on another issue: that word is newer than the field, and a process of the pod from 85 to 131,
// which the pod's two numbers do not rule out, refuses the whole body for it. A caller whose
// strip is below whyAndLineFieldsContract has lost `kind` with the rest of the line's fields
// before this runs (shapeIssue), so this finds nothing to take there and writes no line.
//
// The member is a pointer of the issue's own, and it is copied before it is written, so the
// state the source answered is left as it was. It reports whether it took the word, which is
// what the log line of this strip is written from (tookOff).
func withoutWaitingOnIssue(issue Issue, restore int) (Issue, bool) {
	line := issue.WaitingLine
	if line == nil || line.Kind == "" {
		return issue, false
	}
	if !lineKindArrivals.heldBack(line.Kind, restore) {
		return issue, false
	}
	member := *line
	member.Kind = ""
	issue.WaitingLine = &member
	return issue, true
}

// fieldArrivals is one enumerated field of the declared table as a value strip reads it: the
// contract the field itself arrived at, and the contract each of its values did (arrivalsOf).
type fieldArrivals struct {
	// field is the earliest arrival of the field's list, which is the field's own: the table
	// records each value at the later of the value's own arrival and its field's (answers.go's
	// EnumeratedFields). A field older than the floor reads as ServingFloor. That holds while the
	// list still declares a value the field arrived with, which
	// TestEveryEnumeratedFieldsEarliestValueArrivedWithItsKey holds each list to, reading the
	// key's arrival from the recorded shapes; that test says what a list that lost every such
	// value would let through.
	field int
	// values is each declared value's arrival.
	values map[string]int
}

// heldBack is whether the body is served without value to a caller whose restore is restore:
// the contract the caller states, else the lowest its pod records (contractsFor).
//
// A value that arrived after its field rides a strip, which holds it back wherever restore is
// below the value's arrival. A reader from the field's arrival up to the contract before the
// value's knows the field and reads it with an enumeration that does not name the value, so it
// refuses the whole body for it. No reader requires a value, so the strip follows restore where
// a band follows strip (contractsFor). The pod's reading keeps its lowest and its highest contract
// alone (podContracts), so a pod whose lowest contract is below the field's arrival may hold
// such a reader too, and the value is held back from that pod as well.
//
// A value that arrived with its field is never held back here, whatever restore is: it leaves
// with the field's key. A reader below that arrival knows no such key and has no enumeration of
// it, so the value by itself refuses nothing. The key is the band's: the row of stateNarrowings
// for the contract that added it takes the key, and the value with it, from a caller whose strip
// is below that contract, and a pod whose newest process reads the key is served the value with
// it. Taking such a value at restore as well would cost that process a fact and make no reader
// safer. withoutWaitingOnIssue works both halves through on one pod.
//
// A value no row of the list names reads as arriving at this daemon's own contract: no older
// reader is known to admit it, so a strip holds it back from every caller below this daemon's
// own contract. The gate therefore sees it only in a body served at DaemonAPIVersion, where a
// fixture carries it, and reds there until the table declares it (contract_shapes_test.go).
func (f fieldArrivals) heldBack(value string, restore int) bool {
	arrival, declared := f.values[value]
	if !declared {
		arrival = DaemonAPIVersion
	}
	return arrival > f.field && restore < arrival
}

// strandedStateArrivals, waitAnswerArrivals and lineKindArrivals are the arrivals of
// `strandedClaims[].state`, of `answeredBy` and of the waiting line's `kind`, read once from the
// list the table itself is written from (answers.go's strandedStates, waitAnswers and
// lineKinds). The state's row answers for the record's too: an issue's field is one field at
// two key paths (answers.go's enumeratedFields), both rows take the one list, and shapeIssue
// shapes the issue behind both answers.
var (
	strandedStateArrivals = arrivalsOf(strandedStates)
	waitAnswerArrivals    = arrivalsOf(waitAnswers)
	lineKindArrivals      = arrivalsOf(lineKinds)
)

// arrivalsOf is one list of the declared table as the arrivals of its field: each value under
// its own arrival, and the earliest of them as the field's. Every list holds at least one value,
// which TestEveryEnumeratedFieldsEarliestValueArrivedWithItsKey holds it to.
func arrivalsOf(values []EnumeratedValue) fieldArrivals {
	arrivals := fieldArrivals{values: make(map[string]int, len(values))}
	for i, value := range values {
		arrivals.values[value.Value] = value.Since
		if i == 0 || value.Since < arrivals.field {
			arrivals.field = value.Since
		}
	}
	return arrivals
}

// withoutStallRecord is the issue with no claim view carrying `step`, the one key
// stallRecordContract added to this route; nothing else of that contract is on it.
func withoutStallRecord(issue Issue) (Issue, bool) {
	return withEachClaimView(issue, func(view ClaimView) ClaimView {
		view.Step = nil
		return view
	})
}

// withoutContractMark is state with no `contract`, the one key recordFieldsContract added to
// the body itself. The mark goes with the nine keys of that contract, and must: it names the
// two contracts, and a key this caller's plugin does not know refuses the whole body at it,
// which is the one thing the serving-down is for. What such a caller is told instead is told to
// the operator of the daemon rather than to the caller (recordShaping). The field is the
// state's own value, so the state the source answered is left as it was, as
// withoutLowestPluginContract leaves it.
func withoutContractMark(state State) (State, bool) {
	moved := state.Contract != nil
	state.Contract = nil
	return state, moved
}

// stripRecordFields is view with none of the keys recordFieldsContract added to a claim view:
// `servingGeneration`, `updateReasons`, `providerErrorRun`, `pendingTask`, `failure` and
// `pendingCause`. It is the one list of those keys, and withoutRecordFields applies it through
// withEachClaimView, which also reads from it whether a view carried any of them: a view this
// strip changes is a view that carried one, since each key it clears is `omitempty`.
func stripRecordFields(view ClaimView) ClaimView {
	view.ServingGeneration = 0
	view.UpdateReasons = ""
	view.ProviderErrorRun = 0
	view.PendingTask = nil
	view.Failure = nil
	view.PendingCause = ""
	return view
}

// withoutRecordFields is the issue with no claim view carrying any of the keys
// stripRecordFields takes off and no `heldFrom`, `lingerUntil` or `ancestry`, which is every key
// recordFieldsContract added to an issue; the `contract` mark, the one key it added to the body
// itself, is withoutContractMark's. The issue's own three are cleared on the value this
// function was handed. Its architect and its worker map are copied only where a claim view of
// that issue carries one of the strip's keys (withEachClaimView), as withoutBackwardMoves copies
// only the worker maps that carry the counts, because this runs on every state read of every pod
// below the contract. `ancestry` is asked its length: it is `omitempty`, so an empty list is no
// key on the wire.
func withoutRecordFields(issue Issue) (Issue, bool) {
	moved := issue.HeldFrom != "" || issue.LingerUntil != nil || len(issue.Ancestry) > 0
	issue.HeldFrom = ""
	issue.LingerUntil = nil
	issue.Ancestry = nil
	issue, viewsMoved := withEachClaimView(issue, stripRecordFields)
	return issue, moved || viewsMoved
}

// lineFieldsCarried is whether one member's view carries a field of whyAndLineFieldsContract's
// half of the line. It is read as whatever the view is beyond the two fields waitingLineContract
// added, rather than as a list of the ten names, so a field added to WaitingLineView later is
// seen by this and left off by withoutWhyAndLineFields without either being told of it. The view
// is comparable and its two pointers compare as pointers, which is what is wanted: a `holdsSlot`
// pointing at `false` is a field the member carries, as much as one pointing at `true`.
func lineFieldsCarried(view WaitingLineView) bool {
	return view != WaitingLineView{Place: view.Place, Length: view.Length}
}

// withoutWhyAndLineFields is the issue with no `why` and no waiting line member carrying a field
// beside its `place` and its `length`, the two halves whyAndLineFieldsContract added; nothing
// else of that contract is on the route. It is unlike every serving-down before it in one way:
// withoutWaitingLine drops `waitingLine` whole for a caller below waitingLineContract, where
// this serves a caller between the two contracts the field that one describes — the place and
// the length it asked for — and takes away only what this contract added. That is why the member
// is rebuilt from those two rather than emptied field by field. Every field left out is
// omitempty and `holdsSlot` is a pointer for exactly this (WaitingLineView), so the rebuilt
// member puts no key of this contract on the wire rather than writing one as its zero. The
// member is a new view rather than a write through the pointer the snapshot holds, so the state
// the source answered is left as it was for the next caller.
func withoutWhyAndLineFields(issue Issue) (Issue, bool) {
	moved := issue.Why != nil
	issue.Why = nil
	if line := issue.WaitingLine; line != nil && lineFieldsCarried(*line) {
		issue.WaitingLine = &WaitingLineView{Place: line.Place, Length: line.Length}
		moved = true
	}
	return issue, moved
}

// withoutLowestPluginContract is state with no `lowestPluginContract`, the one key
// shapedAnswersContract added to the body itself. The field is the state's own value, so the
// state the source answered is left as it was, as withoutWakesWaiting leaves it.
func withoutLowestPluginContract(state State) (State, bool) {
	moved := state.LowestPluginContract != nil
	state.LowestPluginContract = nil
	return state, moved
}

// withoutPause is state with no `paused` and no `lastPause`, the two keys pauseContract added to
// the state; nothing else of that contract is a key of this route. Both are pointers with
// `omitempty`, absent wherever the state holds none, so the band reports a move only for a state
// that carries one. The fields are the state's own values, so the state the source answered is
// left as it was, as withoutLowestPluginContract leaves it.
func withoutPause(state State) (State, bool) {
	moved := state.Paused != nil || state.LastPause != nil
	state.Paused, state.LastPause = nil, nil
	return state, moved
}

// withoutStateSeq is state with no `daemon.stateSeq`, no `daemon.stateId` and no
// `daemon.stateRun`, the three keys stateSeqContract added to this route; nothing else of that
// contract is a key of the state. The sequence is left off rather than zeroed, because the strict
// reader before the contract refuses it when it is 0 as surely as when it names a sequence, and a
// 0 would tell a reader of this contract that the project has no state event. The daemon's own
// source sets the sequence on every body it answers, 0 included, so the band reports a move for
// every such body; a state built without any of the three carries nothing to strip. The daemon
// block is a value of the state's own, so the state the source answered is left as it was.
func withoutStateSeq(state State) (State, bool) {
	moved := state.Daemon.StateSeq != nil || state.Daemon.StateID != "" || state.Daemon.StateRun != ""
	state.Daemon.StateSeq, state.Daemon.StateID, state.Daemon.StateRun = nil, "", ""
	return state, moved
}

// withoutResumePauses is records with no resume carrying `pause`, the one key pauseContract added
// to the release records. The key is `omitempty` and absent from a wake no pause held, so records
// whose resumes carry none are returned as they are, with no copy made. Otherwise the resumes are
// copied before one is written, so the body the route built is left as it was, and the list stays
// non-nil, as every list of the body is (ReleaseRecordsResponse).
func withoutResumePauses(records ReleaseRecordsResponse) ReleaseRecordsResponse {
	if !slices.ContainsFunc(records.Resumes, func(resume ResumeRecord) bool { return resume.Pause != 0 }) {
		return records
	}
	resumes := make([]ResumeRecord, len(records.Resumes))
	for i, resume := range records.Resumes {
		resume.Pause = 0
		resumes[i] = resume
	}
	records.Resumes = resumes
	return records
}

// withoutClaimRoundReport is the issue with no claim view carrying `updateState` or `wake`, the
// two keys shapedAnswersContract added to the claim views.
func withoutClaimRoundReport(issue Issue) (Issue, bool) {
	return withEachClaimView(issue, func(view ClaimView) ClaimView {
		view.UpdateState, view.Wake = "", nil
		return view
	})
}

// withoutLeakCheckCause is the issue with no claim view carrying `leakCheckCause`, the one key
// leakCheckCauseContract added to this route; nothing else of that contract is on it.
func withoutLeakCheckCause(issue Issue) (Issue, bool) {
	return withEachClaimView(issue, func(view ClaimView) ClaimView {
		view.LeakCheckCause = ""
		return view
	})
}

// withoutCheckRole is the issue whose check carries no `role`, the one key checkRoleContract
// added to this route. The key is left off rather than emptied, because the strict reader before
// the contract refuses it as surely when it is empty as when it names a role, which is why
// CheckView.MarshalJSON writes the key on every body but this one (CheckView.roleHidden), as
// Admission.MarshalJSON leaves off `trees`. The flag is set on a check of the issue's own, so the
// state the source answered is left as it was. The flag leaves the key off every check it is set
// on, so the band reports a move on every issue that carries a check.
func withoutCheckRole(issue Issue) (Issue, bool) {
	if issue.Check == nil {
		return issue, false
	}
	check := *issue.Check
	check.roleHidden = true
	issue.Check = &check
	return issue, true
}

// fixAttemptsStandIn is the `fixAttempts` a caller below fixAttemptsContract is served on every
// pull request: zero, what the stopped `fix_attempts` column takes on an insert and never leaves
// (record's PutPullRequest). Nothing has counted a fix attempt since dispatch://INFRA-956, and no
// plugin reads the value — the count drove a block this daemon no longer applies — so the
// stand-in gives that contract's reader the key its schema requires and reports nothing. One
// value stands for every pull request of every body, so it is a constant and the view carries a
// flag rather than a number (PullRequestView.fixAttemptsShown, which MarshalJSON writes it on).
const fixAttemptsStandIn = 0

// maxFixAttemptsStandIn is the `max_fix_attempts` a caller below fixAttemptsContract is served in
// the settings block: three, the setting's default before that contract removed it from
// legion.yaml. No plugin reads it either, for the same reason fixAttemptsStandIn is read by none,
// and it is a constant for the same reason too (Settings.maxFixAttemptsShown).
const maxFixAttemptsStandIn = 3

// withFixAttempts is the issue whose pull request carries `fixAttempts`, the key
// fixAttemptsContract took off this route, as the stand-in a reader below that contract requires.
// The pull request view is copied, so the state the source answered is left as it was.
func withFixAttempts(issue Issue) Issue {
	if issue.PullRequest != nil {
		pullRequest := *issue.PullRequest
		pullRequest.fixAttemptsShown = true
		issue.PullRequest = &pullRequest
	}
	return issue
}

// withMaxFixAttempts is state whose settings block carries `max_fix_attempts`, the other key
// fixAttemptsContract took off this route, as the stand-in a reader below that contract is
// served beside `fixAttempts`. The block is a value of the state's own, so the state the source
// answered is left as it was.
func withMaxFixAttempts(state State) State {
	state.Settings.maxFixAttemptsShown = true
	return state
}

// withoutStateMachineFields is the issue with no `pullRequest.mergeState`, no `pass` on any
// worker's phase view, and no claim view carrying `lastTurnStart`, `lastTurnEnd`,
// `lastDeathCause`, `lastStartFailure` or `lastReady`: every key stateMachineContract added to an
// issue. The architect's view is a claim view and carries no `pass`. The seven arrive at one
// number and leave together, so the issue takes one pass rather than one for each, as
// withoutRecordFields takes one.
//
// `pass` is the one of the seven that is no `omitempty` field, so it is left off by a flag on the
// view the marshaller reads (PhaseView.passHidden) rather than by zeroing it, which would write
// `"pass": 0` to a reader that knows no such key. The flag leaves the key off every worker it is
// set on, so the band reports a move on every issue that has a worker, and every worker of such
// an issue is written again. This function therefore builds its own worker map and strips each
// worker's view in the same pass, rather than through withEachClaimView, whose map is the
// source's own where no view moved and so must not be written into. The architect's view and the
// pull request view are copied where they are written, so the state the source answered is left
// as it was.
func withoutStateMachineFields(issue Issue) (Issue, bool) {
	moved := len(issue.Workers) > 0
	if issue.PullRequest != nil {
		moved = moved || issue.PullRequest.MergeState != ""
		pullRequest := *issue.PullRequest
		pullRequest.MergeState = ""
		issue.PullRequest = &pullRequest
	}
	if issue.Architect != nil {
		if architect := stripTurnRecord(*issue.Architect); architect != *issue.Architect {
			issue.Architect, moved = &architect, true
		}
	}
	if len(issue.Workers) > 0 {
		workers := make(map[claim.Role]PhaseView, len(issue.Workers))
		for role, worker := range issue.Workers {
			worker.Claim = stripTurnRecord(worker.Claim)
			worker.passHidden = true
			workers[role] = worker
		}
		issue.Workers = workers
	}
	return issue, moved
}

// stripTurnRecord is view with none of the keys stateMachineContract added to a claim view:
// `lastTurnStart`, `lastTurnEnd`, `lastDeathCause`, `lastStartFailure` and `lastReady`. It
// stands on its own because the architect's view and every worker's take the same five, as
// stripRecordFields stands on its own for the keys of the decided record.
func stripTurnRecord(view ClaimView) ClaimView {
	view.LastTurnStart, view.LastTurnEnd, view.LastReady = nil, nil, nil
	view.LastDeathCause, view.LastStartFailure = "", ""
	return view
}

// answerUnparsed names a body no plugin parses: the operator's own answers, the controller's
// secret and credential, which `legion` the command and the daemon's own controller read, and
// the readiness probe's answer, which the kubelet reads (readyz). It is no row of Answers, which
// is the plugin's table alone, and it shapes to nothing, since no contract describes a shape for
// a body no plugin reads (writeUnparsedAnswer).
const answerUnparsed AnswerID = "unparsed"

// AnswerObservedState names the observed view of the state route, the body of
// `GET /legion/v1/state?view=observed` (ObservedState), which Dispatch reads. It is no row of
// Answers, which is the plugin's table alone: no plugin parses the view, and pi-shared's census of
// that table holds every row to a parse site of the plugin's client. It is shaped all the same,
// unlike answerUnparsed: a caller below a contract reads no member of the view that the whole
// state does not serve it at that contract (observedFor).
const AnswerObservedState AnswerID = "observed-state"

// answerWritten is told the pattern of the route and the id of the answer of every body this
// package writes (writeAnswerFrom). It is nil in a daemon and set by one test of this package,
// which holds each pair to the table: an answer's Routes say which doors write it, and a door
// that writes an id those Routes do not name shapes its body to a contract that door never
// declared. The hook lives here rather than in the registration because a door writes more than
// one answer — its own, and a refusal for every caller it turns away — so no one id can be
// bound to a route.
var answerWritten func(pattern string, id AnswerID)

// writeAnswer writes body as the answer id names it, shaped down to the contracts the caller
// reads at (contractsFor, shapeAnswer). Every body this package writes goes through it or through
// writeAnswerFrom, which it stands on.
//
// It places a caller that states no contract by its pod on an answer placedByPod names, with the
// claims placingClaims reads, and hands contractsFor no claims for any other, which costs a
// caller nothing: a caller that states its own contract is placed by its header alone, and
// contractsFor reads no pod for an answer written with none. The state and the record are
// written by writeAnswerFrom with the claims of the very snapshot each answers.
func (s *server) writeAnswer(w http.ResponseWriter, r *http.Request, id AnswerID, status int, body any) {
	var claims []supervise.Claim
	if placedByPod(id, body) {
		claims = s.placingClaims(r)
	}
	s.writeAnswerFrom(w, r, id, status, body, claims)
}

// placedByPod is whether the answer id, written as body, places a caller that states no contract
// by the claims of its pod: true for the release records and for a refusal whose body is a
// PauseFailure, and false for every other answer. Those two answers' shapes moved at
// pauseContract, and their writers answer from no snapshot of their own, so writeAnswer reads the
// claims for them (placingClaims). Every other answer writeAnswer writes is the identity at every
// contract, and so is a plain refusal (a claim.Refusal or a Failure), so a read of the claims for
// one of them would decide nothing. A serving-down added to one of them reaches the caller of a
// pod only once this answers true for it. The state and the record are no question for it: their
// routes hand in their own snapshot's claims (writeAnswerFrom).
func placedByPod(id AnswerID, body any) bool {
	switch id {
	case AnswerReleaseRecords:
		return true
	case AnswerRefusal:
		_, pause := body.(PauseFailure)
		return pause
	default:
		return false
	}
}

// placingClaims is the project's claims, read for a writer that answers from no snapshot of its
// own: writeAnswer's, for an answer placedByPod names, and readsTheStopAsAFailure's. A caller that
// states no contract is placed by its pod there as on the state.
//
// It reads the store only where the reading can place the caller. It answers nil, and reads
// nothing, for a daemon that knows no caller's pod (Options.CallerPod is nil, as under the tmux
// runtime), for a caller that states its contract (pluginContractHeader), whom its header alone
// places, and for a server with no supervisor to read from. A read that fails is logged and
// answers nil too, which serves this daemon's own body, as a caller with no claims is served
// (contractsFor).
func (s *server) placingClaims(r *http.Request) []supervise.Claim {
	if s.callerPod == nil || s.supervisor == nil {
		return nil
	}
	if _, stated := statedContract(r); stated {
		return nil
	}
	claims, err := s.supervisor.Claims(r.Context())
	if err != nil {
		s.logRequestFailure(r, err, "api: read the claims that place the caller of an answer")
		return nil
	}
	return claims
}

// writeAnswerFrom is writeAnswer with the claims that place the caller handed in: the state and
// the record hand in the project's claims as the body being answered holds them, which each route
// reads through the transaction it projected that body in, and writeAnswer hands in what
// placingClaims reads for an answer placedByPod names, and none for any other. It places the
// caller (contractsFor), shapes the body once (shapeAnswer), and writes to the log what that
// shaping reported it took off (recordShaping) before it writes the body.
//
// It is the one writer of a response body in this package: the encoding and the write are its
// own, and no other function of the package writes a body, so a body written around it is one
// a route writes by hand, which is a body no contract shaped.
//
// It is also where the pair a write makes — the route's own pattern and the answer id it writes
// under — is told to answerWritten, so that the package's own census can hold each pair to the
// table (daemon_contract_header_test.go). Nothing is told anything in a daemon: answerWritten is
// nil there, and only a test of this package ever sets it.
func (s *server) writeAnswerFrom(w http.ResponseWriter, r *http.Request, id AnswerID, status int, body any, claims []supervise.Claim) {
	if answerWritten != nil {
		answerWritten(r.Pattern, id)
	}
	at := s.contractsFor(r, claims)
	shaped, took := shapeAnswer(id, body, at.restore, at.strip)
	s.recordShaping(r, id, at, took)
	encoded, err := json.Marshal(shaped)
	if err != nil {
		slog.Error("api: encode response", "error", err)
		http.Error(w, `{"error":"encode failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(append(encoded, '\n')); err != nil {
		slog.Debug("api: write response", "error", err)
	}
}

// writeUnparsedAnswer writes a body no plugin parses: the operator's claim answers, the
// controller's secret and credential, and the readiness probe's 200 and 503 (answerUnparsed). It
// is written through the one writer like every other body, so a route cannot write it around
// the shaping; what it says is that there is no contract to shape this body to. A refusal on the
// operator's and the controller's routes is a refusal like any other, and is written as
// AnswerRefusal.
func (s *server) writeUnparsedAnswer(w http.ResponseWriter, r *http.Request, status int, body any) {
	s.writeAnswer(w, r, answerUnparsed, status, body)
}
