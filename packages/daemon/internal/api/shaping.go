package api

// This file is the serving-down, the half of this package that answers one question: what does
// the caller's own plugin read? It holds the contract header a caller states, the reading of the
// pod a caller that states none is in, the shape of every answer at every contract this daemon
// serves, and the one writer each answer goes through. The table it reads is answers.go's, beside
// it; server.go routes the requests and calls writeAnswer and writeAnswerFrom, which is the whole
// of what ties the two together.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strconv"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// pluginContractHeader is the request header a caller states the contract its own plugin speaks
// in (version.go's 89). A caller that states one is placed by what it states and by nothing
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

// contractsFor is the pair of contracts an answer to r is shaped at: restore, the contract whose
// removed keys are written back into the body, and strip, the contract whose later keys are left
// out of it. shapeAnswer applies both.
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
// the highest contract the pod records, because no one body serves both a strict reader that
// refuses `check.role` and a reader that requires it, and this rule keeps the newer process
// working. Strip by the lowest instead and a process that works today stops: `check.role` would
// be taken from the reader that requires it.
//
// Four edges:
//
//   - A contract below ServingFloor takes the floor's shape on the restore side, the lowest
//     shape this daemon describes, and keeps every strip its own number gives it, so such a
//     reader loses no key it loses today.
//   - A contract above DaemonAPIVersion takes the body this contract writes, on both sides.
//   - A contract between the floor and this daemon's own that SupportedContracts does not hold —
//     47 and 50, the two numbers no change ever shipped under — takes the nearest recorded
//     contract below it, on both sides.
//   - The first and the last write one log line each (servedContracts).
//
// Every band the pair leaves out of a state is written to the daemon's own log beside those
// lines, which is why the answer's id is asked for here (recordShaping).
func (s *server) contractsFor(r *http.Request, id AnswerID, claims []supervise.Claim) (restore, strip int) {
	if stated, ok := statedContract(r); ok {
		restore, strip = s.servedContracts(r, stated, stated, "")
		s.recordShaping(r, id, "", stated, strip)
		return restore, strip
	}
	// With no claims to read a contract from, no pod reports one: the pair is this daemon's own
	// whatever pod the caller is in, and reading that pod decides nothing. Under the Sandbox
	// runtime the reading lists every cached pod and scans it, which every answer written with
	// no claims would pay for — each refusal, each grant, the catch-all's 404 and `GET /healthz`
	// (writeAnswer). The two routes that answer from the state's own snapshot hand their claims
	// in and are placed by their pod as before (writeAnswerFrom).
	if len(claims) == 0 {
		return DaemonAPIVersion, DaemonAPIVersion
	}
	// A daemon that does not know its callers' pods serves every caller the body this contract
	// writes, and scans nothing for it: under runtime tmux that is every request of every pane
	// and of `legion state`.
	if s.callerPod == nil {
		return DaemonAPIVersion, DaemonAPIVersion
	}
	caller, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return DaemonAPIVersion, DaemonAPIVersion
	}
	pod, inPod := s.callerPod(caller.Addr().Unmap())
	if !inPod {
		return DaemonAPIVersion, DaemonAPIVersion
	}
	lowest, highest, recorded := podContracts(claims, pod)
	if lowest == 0 {
		return DaemonAPIVersion, DaemonAPIVersion
	}
	restore, strip = s.servedContracts(r, lowest, highest, recorded)
	s.recordShaping(r, id, pod, highest, strip)
	return restore, strip
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
// contract below n where it does not (47 takes 46 and 50 takes 49), and DaemonAPIVersion for an n
// above this daemon's own. Its callers hand it nothing below ServingFloor.
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
// The state's row is shapeState, and it carries the reasoning of every serving-down this daemon
// makes. A plugin older than dispatch://INFRA-1048 — which is who this is kept for — reads the
// state with a strict schema and refuses the whole body for one key it does not know, where
// today's plugin strips one; and an update round serves a claim of the contract before it until
// its pod is replaced (version.go's 46). So a caller below supervise.PodUpdateContract, the
// contract that added `updateRound` and `updateHead` to the claim views, is served the state
// without them (withoutUpdateRounds), which is the body that contract's schema describes; a
// caller below memoryContract is served it without the issues' three memory fields
// (withoutMemory); a caller below treeContract without `admission.trees` and the issues' `tree`
// (withoutTrees, withoutTree); a caller below wakesWaitingContract without `wakesWaiting`
// (withoutWakesWaiting); a caller below gatedStepsContract without the issues' `gatedSteps`
// (withoutGatedSteps); a caller below retiredStrandedContract is served the report of claims
// nothing will wake without its retired entries (withoutRetiredStranded); a caller below
// stallRecordContract without the claim views' `step` (withoutStallRecord); a caller below
// waitingLineContract without each waiting issue's `waitingLine` (withoutWaitingLine); a caller
// below backwardMovesContract without the workers' `backwardMoves` (withoutBackwardMoves); a
// caller below claimsToRefreshContract without `claimsToRefresh`, the count of claims the
// stale-plugin lane still has to relaunch onto this daemon's plugin (withoutClaimsToRefresh); a
// caller below checkRoleContract is served each issue's check without `role`
// (withoutCheckRole); a caller below recordFieldsContract is served the body without the
// `contract` mark (withoutContractMark) and every issue without the nine keys that contract
// decided — a claim view's `servingGeneration`, `updateReasons`, `providerErrorRun`,
// `pendingTask`, `failure` and `pendingCause`, and the issue's own `heldFrom`, `lingerUntil`
// and `ancestry` (withoutRecordFields); a caller below whyAndLineFieldsContract is served every
// issue without `why` and every member of the waiting line with its `place` and its `length`
// alone (withoutWhyAndLineFields), which is the body waitingLineContract's own schema
// describes; and a caller below shapedAnswersContract is served the body without
// `lowestPluginContract` (withoutLowestPluginContract) and its claim views without
// `updateState`, `updateReaskedForPushCheck`, `unpromptedTurnTakesAnswerBack` and `wake`
// (withoutClaimRoundReport). The sixteen compose: a caller below all of them is served a body
// carrying none of the thirty keys, the waiting line's own fields being fields of a view such a
// caller is served none of at all, each of them absent rather than empty, since a strict reader
// before the contract that added a key refuses it whether or not it names anything, and with no
// retired entry under `strandedClaims`. Every later reader takes those keys as optional and all
// three stranded states — `packages/contracts/src/legion-api.ts` marks them optional for
// exactly these bodies.
//
// The seventeenth row takes a value rather than a key, and composes with the sixteen: each
// issue's wait is served without its answered pair where `answeredBy` is a value the caller's
// contract does not admit (withoutUnadmittedWaitAnswer), because a strict reader refuses the
// whole body for a value its enumeration does not name as surely as for a key it does not know
// (answers.go's EnumeratedFields). The row is asked of every caller, as the rows above it are.
// A caller at this daemon's own contract admits every value the table declares and loses
// nothing; a value the table declares no arrival for is read as arriving at this daemon's own
// contract, so every caller below it is served the wait without the pair (waitAnswerArrival).
//
// The record's row is shapeIssue, the per-issue half of the state's own row: the record route
// answers the very value the whole-project body holds under `issues.<key>` (issueRecordRoute),
// so one function shapes the issue a caller reads on its own and the issue it reads inside the
// whole body, and the two cannot part company.
//
// On the other side, a caller below fixAttemptsContract is served the two keys that contract
// removed, as stand-ins its own reader requires (withFixAttempts, withMaxFixAttempts). Any
// caller at this daemon's own contract — a pod at it, the operator's `legion state`, a tmux
// pane — is served the state as this contract writes it, neither restored nor stripped.
func shapeAnswer(id AnswerID, body any, restore, strip int) any {
	switch id {
	case AnswerState:
		state, isState := body.(State)
		if !isState {
			panic(fmt.Sprintf("api: the %s answer was written a %T, which its row cannot shape", id, body))
		}
		return shapeState(state, restore, strip)
	case AnswerIssueRecord:
		issue, isIssue := body.(Issue)
		if !isIssue {
			panic(fmt.Sprintf("api: the %s answer was written a %T, which its row cannot shape", id, body))
		}
		return shapeIssue(issue, restore, strip)
	case AnswerHealth, AnswerRegister, AnswerRegisterController, AnswerNoContent,
		AnswerReleaseRecords, AnswerGrant, AnswerHandoffComplete, AnswerEmptyObject, AnswerFreshSession,
		AnswerCompact, AnswerWaveRelease, AnswerRefusal:
		// The identity, one row each: no key of these answers has arrived or gone since
		// ServingFloor, so every contract this daemon serves reads the body it writes.
		return body
	case answerUnparsed:
		// The identity too, and for a reason of its own: no plugin parses these bodies, so no
		// contract describes a shape for them (writeUnparsedAnswer).
		return body
	default:
		panic("api: shapeAnswer has no row for the answer " + string(id))
	}
}

// shapeState is the state's row of shapeAnswer: the keys restore's reader requires written back,
// and the keys strip's reader refuses left out.
//
// The body's own keys are shaped here and every key of an issue in shapeIssue, which the record
// route serves one value of on its own (AnswerIssueRecord). One pass over the issue map applies
// every per-issue serving-down, so the map is copied once however many of them a caller takes,
// and a caller at this daemon's own contract, whom no row of either function moves, is served
// the map the source answered.
//
// The claims a caller's contract was read from are the state's own snapshot's, because the body
// is: a claim and the state it was projected into cannot come from two moments (store.ClaimsTx).
// A state whose claims the source could not read is no state at all — its claim views are built
// from them — so the route refuses it before this runs, and no readable state is left whose
// caller's contract is unknown.
func shapeState(state State, restore, strip int) State {
	// The mark says which shape this body took: the contract this binary declares and serves,
	// and, where the shaping served the caller a body below it, the contract it was shaped to.
	// It is written before anything is taken out, so the band that leaves it off is the pair of
	// the key (withoutContractMark) rather than a key written only where it is allowed.
	state.Contract = &ContractView{Daemon: DaemonAPIVersion}
	if strip < DaemonAPIVersion {
		state.Contract.Caller = strip
	}
	if restore < fixAttemptsContract {
		state = withMaxFixAttempts(state)
	}
	if strip < treeContract && carriesTrees(state) {
		state = withoutTrees(state)
	}
	if strip < wakesWaitingContract && carriesWakesWaiting(state) {
		state = withoutWakesWaiting(state)
	}
	if strip < retiredStrandedContract && carriesRetiredStranded(state) {
		state = withoutRetiredStranded(state)
	}
	if strip < claimsToRefreshContract && carriesClaimsToRefresh(state) {
		state = withoutClaimsToRefresh(state)
	}
	if strip < recordFieldsContract {
		state = withoutContractMark(state)
	}
	if strip < shapedAnswersContract {
		state = withoutLowestPluginContract(state)
	}
	// Every row of shapeIssue is below this daemon's own contract, so a caller at it takes none
	// of them and is served the issue map the source answered, with no copy made of it.
	if restore < DaemonAPIVersion || strip < DaemonAPIVersion {
		issues := make(map[string]Issue, len(state.Issues))
		for key, issue := range state.Issues {
			issues[key] = shapeIssue(issue, restore, strip)
		}
		state.Issues = issues
	}
	return state
}

// shapeIssue is one issue of the state as the caller reads it: the per-issue half of shapeState,
// and the record's row of shapeAnswer, since the record route answers the very value the
// whole-project body holds for that issue (issueRecordRoute). The two readings of one issue are
// shaped here and nowhere else, so a caller served the state and a caller served the record read
// the same issue field for field at every contract.
//
// The issue is a value of its own, and each pointer and map it carries is copied before it is
// written, so the state the source answered is left as it was.
func shapeIssue(issue Issue, restore, strip int) Issue {
	if restore < fixAttemptsContract {
		issue = withFixAttempts(issue)
	}
	if strip < supervise.PodUpdateContract {
		issue = withoutUpdateRounds(issue)
	}
	if strip < memoryContract {
		issue = withoutMemory(issue)
	}
	if strip < treeContract {
		issue = withoutTree(issue)
	}
	if strip < checkRoleContract {
		issue = withoutCheckRole(issue)
	}
	if strip < gatedStepsContract {
		issue = withoutGatedSteps(issue)
	}
	if strip < stallRecordContract {
		issue = withoutStallRecord(issue)
	}
	if strip < waitingLineContract {
		issue = withoutWaitingLine(issue)
	}
	if strip < backwardMovesContract {
		issue = withoutBackwardMoves(issue)
	}
	if strip < recordFieldsContract {
		issue = withoutRecordFields(issue)
	}
	if strip < whyAndLineFieldsContract {
		issue = withoutWhyAndLineFields(issue)
	}
	// No band of its own: the table says which values this caller's reader admits, and a caller
	// at this daemon's own contract admits every value the table declares.
	issue = withoutUnadmittedWaitAnswer(issue, strip)
	if strip < shapedAnswersContract {
		issue = withoutClaimRoundReport(issue)
	}
	return issue
}

// narrowedTheState is the message of the line the daemon writes for each band it serves the
// state down by, named here because the test that reads the daemon's own log reads the same
// string (TestANarrowedBodySaysSo).
const narrowedTheState = "api: narrowed the state to the caller's plugin contract"

// stateNarrowing is one band of shapeState and shapeIssue that takes keys off the state: the
// contract that added those keys to this route, and what the log line names as dropped. Each
// band carries its own `dropped` rather than the contract number alone, because a reader of the
// log who is told only a number has to find out elsewhere what left the body.
type stateNarrowing struct {
	contract int
	dropped  string
}

// stateNarrowings is every band of the state's shape, lowest contract first, which is the order
// recordShaping writes them in: a caller below all of them is served a body with every one of
// these keys gone. It describes the two functions above and applies nothing itself, so a band
// added to shapeState or to shapeIssue takes a row here the same day; a row and the `if` that
// serves it are held together by the contract constant both of them name, and by
// TestANarrowedBodySaysSo, which reads the log of the very request it reads the body of.
//
// The order is the history of the route and nothing a band depends on. One pair does touch one
// key: waitingLineContract's band drops the issues' `waitingLine` whole, and
// whyAndLineFieldsContract's takes the fields the record added from inside that same view. They
// still do not depend on their order, because a caller below both is served no `waitingLine` at
// all whichever runs first, and a caller below only the higher one keeps the field with its
// `place` and its `length`, which is what the lower band's own contract describes.
var stateNarrowings = []stateNarrowing{
	{supervise.PodUpdateContract, "the claim views' updateRound and updateHead"},
	{memoryContract, "the issues' memoryGroup, memoryDrawMode and memoryDrawnAt"},
	{treeContract, "admission.trees and the issues' tree"},
	{wakesWaitingContract, "wakesWaiting"},
	{checkRoleContract, "the issues' check role"},
	{gatedStepsContract, "the issues' gatedSteps"},
	{retiredStrandedContract, "the retired entries of strandedClaims"},
	{stallRecordContract, "the claim views' step"},
	{waitingLineContract, "the issues' waitingLine"},
	{backwardMovesContract, "the workers' backwardMoves"},
	{claimsToRefreshContract, "claimsToRefresh"},
	{recordFieldsContract, "the claim views' servingGeneration, " +
		"updateReasons, providerErrorRun, pendingTask, failure and pendingCause, the issues' heldFrom, " +
		"lingerUntil and ancestry, and the contract mark"},
	{whyAndLineFieldsContract, "the issues' why and " +
		"every field of the waiting line beside place and length"},
	{shapedAnswersContract, "lowestPluginContract and the claim views' updateState, " +
		"updateReaskedForPushCheck, unpromptedTurnTakesAnswerBack and wake"},
}

// recordShaping writes the daemon's own record of what it left out of the state it answered: one
// line per band of stateNarrowings the caller stands below, naming the caller, the pod it asked
// from, the contract it reads at, the contract this daemon declares and what that band leaves
// off. pod is empty for a caller the header placed, which no pod answers for.
//
// The body itself cannot carry the record, which is why the daemon's log holds it: a key added
// to a body served down is exactly what the serving-down exists to prevent, so an absent fact
// and a withheld fact would otherwise read alike (dispatch://INFRA-3083, R12). What the body
// does carry, for a caller at or above recordFieldsContract and for no caller below it, is the
// two numbers these lines name (`contract`, ContractView).
//
// It writes for the state alone, the answer stateNarrowings describes. The record route's own
// body is one issue of that state, shaped by the per-issue half of the same bands, and the keys
// each row names are the whole state's.
func (s *server) recordShaping(r *http.Request, id AnswerID, pod string, contract, strip int) {
	if id != AnswerState {
		return
	}
	for _, narrowing := range stateNarrowings {
		if strip >= narrowing.contract {
			continue
		}
		s.log.Info(narrowedTheState, "caller", r.RemoteAddr, "pod", pod,
			"pluginContract", contract, "daemonApiVersion", DaemonAPIVersion,
			"dropped", narrowing.dropped)
	}
}

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
// The boot gate alone covered that reader until this daemon shaped its answer (version.go's 89).
const checkRoleContract = 54

// fixAttemptsContract is the daemon contract that took the fix-attempt count out of the state
// (version.go's 59): each pull request's `fixAttempts`, which counted the heads pushed onto a red
// verdict, and the settings' `max_fix_attempts`, which capped it. A plugin before it parses the
// pull request with a strict schema that requires `fixAttempts` and refuses the whole body
// without it, and every worker's `read_record` parses the state through that reader, so a caller
// below this contract is served both keys as stand-ins (withFixAttempts, withMaxFixAttempts).
// The boot gate alone covered that reader until this daemon shaped its answer (version.go's 89).
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

// shapedAnswersContract is the daemon contract this change lands under (version.go's 89), and
// the contract that added five keys of its own: `lowestPluginContract` on the state, the
// lowest contract any claim of the project records, and each claim view's `updateState`,
// `updateReaskedForPushCheck`, `unpromptedTurnTakesAnswerBack` and `wake`. A plugin before it
// reads the state with a schema that names none of the five and refuses the whole body for one
// of them, so a caller below this contract is served the state without them
// (withoutLowestPluginContract, withoutClaimRoundReport).
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes
// moves that constant, and the contract these keys arrived at stays where it is. It moves only
// with this change's own number, as the arrival contracts of this change's two new answers do
// (answers.go's AnswerIssueRecord and AnswerHealth).
const shapedAnswersContract = 89

// carriesTrees is whether state carries `admission.trees`, the key of treeContract's serving-down
// that is the body's own; each issue's `tree`, the other key, is shapeIssue's (withoutTree). It
// carries `admission.trees` whatever the daemon holds, since the admission block writes that key
// on every body, empty rather than null (Admission.MarshalJSON), and the strict reader before the
// contract refuses the empty key as surely as one naming a tree. So this is true of every state
// the route writes; it is asked beside carriesWakesWaiting, carriesClaimsToRefresh and
// carriesRetiredStranded all the same, because shapeState puts the same question to the four and
// because a later contract that stops writing `trees` on every body answers it here.
func carriesTrees(State) bool { return true }

// carriesWakesWaiting is whether state carries the key wakesWaitingContract added: `wakesWaiting`,
// the wakes the pacing holds. It carries that key whatever the daemon holds, since the state
// writes it on every body, empty rather than null (State.MarshalJSON), and the strict reader
// before the contract refuses the empty key as surely as one naming a wake. So this is true of
// every state the route writes; it is asked beside carriesTrees, carriesClaimsToRefresh and
// carriesRetiredStranded all the same, because shapeState puts the same question to the four and
// because a later contract that stops writing `wakesWaiting` on every body answers it here.
func carriesWakesWaiting(State) bool { return true }

// carriesClaimsToRefresh is whether state carries the key claimsToRefreshContract added:
// `claimsToRefresh`, how many claims the stale-plugin lane still has left. It carries that key
// whatever the daemon holds, since the state writes the number on every body, 0 rather than
// absent (State.MarshalJSON), and the strict reader before the contract refuses the key when it
// is 0 as surely as when it names a number. So this is true of every state the route writes, as
// carriesWakesWaiting is; it is asked beside carriesTrees, carriesWakesWaiting and
// carriesRetiredStranded all the same, because shapeState puts the same question to the four and
// because a later contract that stops writing `claimsToRefresh` on every body answers it here.
func carriesClaimsToRefresh(State) bool { return true }

// withoutGatedSteps is the issue with no `gatedSteps`, the field gatedStepsContract added to the
// state; nothing else of that contract is on the route.
func withoutGatedSteps(issue Issue) Issue {
	issue.GatedSteps = nil
	return issue
}

// carriesRetiredStranded is whether any claim of state's report of claims nothing will wake is
// retired, the state retiredStrandedContract added.
func carriesRetiredStranded(state State) bool {
	return slices.ContainsFunc(state.StrandedClaims, func(c StrandedClaim) bool {
		return c.State == string(supervise.StateRetired)
	})
}

// withoutWaitingLine is the issue with no `waitingLine`, the field waitingLineContract added to
// the state; nothing else of that contract is on the route.
func withoutWaitingLine(issue Issue) Issue {
	issue.WaitingLine = nil
	return issue
}

// withoutBackwardMoves is the issue with no worker carrying `backwardMoves`, the one key
// backwardMovesContract added to this route; nothing else of that contract is on it, the
// settings' `hooks` riding under a block every plugin reads loosely. The worker map is copied
// only for an issue a worker of which carries the counts, since the field rides on the
// implementer of an issue that has moved back and on nobody else, so the issues without one are
// left as the source answered them rather than rebuilt.
func withoutBackwardMoves(issue Issue) Issue {
	if !issueCarriesBackwardMoves(issue) {
		return issue
	}
	workers := make(map[claim.Role]PhaseView, len(issue.Workers))
	for role, worker := range issue.Workers {
		worker.BackwardMoves = nil
		workers[role] = worker
	}
	issue.Workers = workers
	return issue
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
// break the one that does: in the `{46, 89}` pod a roll makes, the highest would be 89, and the
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

// withoutUpdateRounds is the issue with no claim view carrying `updateRound` or `updateHead`, the
// two fields supervise.PodUpdateContract added to the state; nothing else of that contract is on
// the route but `settings` keys, which every plugin reads loosely. The architect's view and the
// worker map are copied, so the state the source answered is left as it was.
func withoutUpdateRounds(issue Issue) Issue {
	if issue.Architect != nil {
		architect := *issue.Architect
		architect.UpdateRound, architect.UpdateHead = "", ""
		issue.Architect = &architect
	}
	if issue.Workers != nil {
		workers := make(map[claim.Role]PhaseView, len(issue.Workers))
		for role, worker := range issue.Workers {
			worker.Claim.UpdateRound, worker.Claim.UpdateHead = "", ""
			workers[role] = worker
		}
		issue.Workers = workers
	}
	return issue
}

// withoutMemory is the issue with no `memoryGroup`, `memoryDrawMode` or `memoryDrawnAt`, the
// three fields memoryContract added to the state; nothing else of that contract is on the route
// but `settings` keys, which every plugin reads loosely.
func withoutMemory(issue Issue) Issue {
	issue.MemoryGroup, issue.MemoryDrawMode, issue.MemoryDrawnAt = "", "", nil
	return issue
}

// withoutTrees is state with no `admission.trees`, the first of the two keys treeContract added;
// each issue's `tree`, the second, is withoutTree's. Nothing else of that contract is a key of
// its own: `active` and `waiting` keep their names and count issues rather than trees, which no
// downgrade can carry, and which the boot gate and `legion probe-image` keep a plugin of the
// contract before this one from reading (version.go's 51). The admission block is a value of the
// state's own, so the state the source answered is left as it was.
func withoutTrees(state State) State {
	state.Admission.Trees, state.Admission.treesHidden = nil, true
	return state
}

// withoutTree is the issue with no `tree`, the second of the two keys treeContract added
// (withoutTrees carries the first).
func withoutTree(issue Issue) Issue {
	issue.Tree = ""
	return issue
}

// withoutWakesWaiting is state with no `wakesWaiting`, the top-level array wakesWaitingContract
// added; nothing else of that contract is on the route but `settings` keys, which every plugin
// reads loosely. The key is left off rather than emptied, because the strict reader before the
// contract refuses it as surely when it is empty as when it names a wake, which is why
// State.MarshalJSON writes the key on every body but this one (State.wakesWaitingHidden), as
// Admission.MarshalJSON leaves off `trees` for withoutTrees. The flag is set on a value of the
// state's own, so the state the source answered is left as it was.
func withoutWakesWaiting(state State) State {
	state.WakesWaiting, state.wakesWaitingHidden = nil, true
	return state
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
// carries neither key.
func withoutClaimsToRefresh(state State) State {
	state.ClaimsToRefresh, state.claimsToRefreshHidden = 0, true
	return state
}

// withoutRetiredStranded is state whose report of claims nothing will wake names no retired claim,
// the one state retiredStrandedContract added; nothing else of that contract is on this route, the
// operator claim view's three delivery fields being served where no plugin reads. The entry is
// dropped rather than rewritten, since its state is the claim's own and no older value stands for
// it, and a reader before the contract refuses the whole body for one of them. The slice is
// copied, so the state the source answered is left as it was, and it stays non-nil when every
// entry goes: `strandedClaims` is never null on the wire (state.go's normalisation).
func withoutRetiredStranded(state State) State {
	stranded := make([]StrandedClaim, 0, len(state.StrandedClaims))
	for _, c := range state.StrandedClaims {
		if c.State == string(supervise.StateRetired) {
			continue
		}
		stranded = append(stranded, c)
	}
	state.StrandedClaims = stranded
	return state
}

// withoutUnadmittedWaitAnswer is the issue whose wait carries no `answeredAt` and no
// `answeredBy` where that `answeredBy` is a value strip's reader does not admit: today the
// `declined` of version.go's 68, and tomorrow any value the table gives a later arrival
// (answers.go's EnumeratedFields). The rule is the table's and not one value's, so a value
// added later is served down by this same row.
//
// The pair goes and the `waitingOn` block stays, which is where this parts company with
// withoutRetiredStranded: a retired stranded entry has nothing true left in it once its state
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
// the source answered is left as it was.
//
// A caller below the floor is read as a caller at it: a value the table records at ServingFloor
// is older than the floor rather than arrived there (answers.go's EnumeratedFields), and the
// floor is the lowest shape this daemon describes, so no reader at all is below such a value's
// arrival. stripContract hands this a caller's own number below the floor, which the max is
// here for.
func withoutUnadmittedWaitAnswer(issue Issue, strip int) Issue {
	if issue.WaitingOn == nil || issue.WaitingOn.AnsweredBy == "" {
		return issue
	}
	if waitAnswerArrival(issue.WaitingOn.AnsweredBy) <= max(strip, ServingFloor) {
		return issue
	}
	wait := *issue.WaitingOn
	wait.AnsweredAt, wait.AnsweredBy = nil, ""
	issue.WaitingOn = &wait
	return issue
}

// waitAnswerArrival is the contract value arrived at, read from the declared table. A value no
// row of the table names reads as this daemon's own contract: no older reader is known to admit
// it, so none is served it, and the gate reds on it at every contract until the table declares
// it (contract_shapes_test.go).
func waitAnswerArrival(value record.Answer) int {
	if since, declared := waitAnswerArrivals[string(value)]; declared {
		return since
	}
	return DaemonAPIVersion
}

// waitAnswerArrivals is the contract each value of `answeredBy` arrived at, read once from the
// table. The state's row answers for the record's too: the two declare one field of the daemon's
// own (answers.go's EnumeratedFields), and shapeIssue shapes the issue behind both answers.
var waitAnswerArrivals = arrivalsOf(AnswerState, "issues.*.waitingOn.answeredBy")

// arrivalsOf is the contract each value of the declared enumerated field arrived at. It panics
// where the table declares no such field, at the package's first use: a serving-down of a value
// reads the row that explains it, and a row renamed away leaves the serving-down with no rule.
func arrivalsOf(answer AnswerID, keyPath string) map[string]int {
	for _, field := range EnumeratedFields {
		if field.Answer != answer || field.KeyPath != keyPath {
			continue
		}
		arrivals := make(map[string]int, len(field.Values))
		for _, value := range field.Values {
			arrivals[value.Value] = value.Since
		}
		return arrivals
	}
	panic("api: answers.go declares no enumerated field `" + keyPath + "` of the " + string(answer) + " answer")
}

// withoutStallRecord is the issue with no claim view carrying `step`, the one key
// stallRecordContract added to this route; nothing else of that contract is on it. The
// architect's view and the worker map are copied, so the state the source answered is left as it
// was, as withoutUpdateRounds leaves it.
func withoutStallRecord(issue Issue) Issue {
	if issue.Architect != nil {
		architect := *issue.Architect
		architect.Step = nil
		issue.Architect = &architect
	}
	if issue.Workers != nil {
		workers := make(map[claim.Role]PhaseView, len(issue.Workers))
		for role, worker := range issue.Workers {
			worker.Claim.Step = nil
			workers[role] = worker
		}
		issue.Workers = workers
	}
	return issue
}

// withoutContractMark is state with no `contract`, the one key recordFieldsContract added to
// the body itself. The mark goes with the nine keys of that contract, and must: it names the
// two contracts, and a key this caller's plugin does not know refuses the whole body at it,
// which is the one thing the serving-down is for. What such a caller is told instead is told to
// the operator of the daemon rather than to the caller (recordShaping). The field is the
// state's own value, so the state the source answered is left as it was, as
// withoutLowestPluginContract leaves it.
func withoutContractMark(state State) State {
	state.Contract = nil
	return state
}

// stripRecordFields is view with none of the keys recordFieldsContract added to a claim view:
// `servingGeneration`, `updateReasons`, `providerErrorRun`, `pendingTask`, `failure` and
// `pendingCause`. It stands on its own because two callers must agree on that list to the key —
// withoutRecordFields, which takes them off the views of a caller under the contract, and
// issueCarriesRecordFields, which asks whether a view carries any of them and asks it by this
// function — so a key added here is answered by both and cannot be added to one alone.
func stripRecordFields(view ClaimView) ClaimView {
	view.ServingGeneration = 0
	view.UpdateReasons = ""
	view.ProviderErrorRun = 0
	view.PendingTask = nil
	view.Failure = nil
	view.PendingCause = ""
	return view
}

// issueCarriesRecordFields is whether a claim view of this one issue carries any key the record
// added to a claim view. It is what the serving-down below reads to decide whether to build an
// issue's views again, and it asks stripRecordFields itself rather than list its keys a second
// time: a view that strip would change is a view carrying one of them, so the question and the
// strip cannot part company over what carrying means — a key added to stripRecordFields is
// answered here by the same line — as issueCarriesBackwardMoves holds that for the counts. The
// comparison is the compiler's: every field of ClaimView is a string, a number, a bool or a
// pointer, so the type is comparable and a build that puts a slice or a map on it fails here.
func issueCarriesRecordFields(issue Issue) bool {
	carries := func(v ClaimView) bool { return stripRecordFields(v) != v }
	if issue.Architect != nil && carries(*issue.Architect) {
		return true
	}
	for _, worker := range issue.Workers {
		if carries(worker.Claim) {
			return true
		}
	}
	return false
}

// withoutRecordFields is the issue with no claim view carrying any of the keys
// stripRecordFields takes off and no `heldFrom`, `lingerUntil` or `ancestry`, which is every key
// recordFieldsContract added to an issue; the `contract` mark, the one key it added to the body
// itself, is withoutContractMark's. The issue's own three are cleared on the value this
// function was handed; its architect and its worker map are built again only where a claim view
// of that issue carries one of the strip's keys, as withoutBackwardMoves rebuilds only the
// worker maps that carry the counts, because this runs on every state read of every pod below
// the contract.
func withoutRecordFields(issue Issue) Issue {
	issue.HeldFrom = ""
	issue.LingerUntil = nil
	issue.Ancestry = nil
	if !issueCarriesRecordFields(issue) {
		return issue
	}
	if issue.Architect != nil {
		architect := stripRecordFields(*issue.Architect)
		issue.Architect = &architect
	}
	if issue.Workers != nil {
		workers := make(map[claim.Role]PhaseView, len(issue.Workers))
		for role, worker := range issue.Workers {
			worker.Claim = stripRecordFields(worker.Claim)
			workers[role] = worker
		}
		issue.Workers = workers
	}
	return issue
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
func withoutWhyAndLineFields(issue Issue) Issue {
	issue.Why = nil
	if line := issue.WaitingLine; line != nil && lineFieldsCarried(*line) {
		issue.WaitingLine = &WaitingLineView{Place: line.Place, Length: line.Length}
	}
	return issue
}

// withoutLowestPluginContract is state with no `lowestPluginContract`, the one key
// shapedAnswersContract added to the body itself. The field is the state's own value, so the
// state the source answered is left as it was, as withoutWakesWaiting leaves it.
func withoutLowestPluginContract(state State) State {
	state.LowestPluginContract = nil
	return state
}

// withoutClaimRoundReport is the issue with no claim view carrying `updateState`,
// `updateReaskedForPushCheck`, `unpromptedTurnTakesAnswerBack` or `wake`, the four keys
// shapedAnswersContract added to the claim views. The architect's view and every worker's are
// each stripped, as withoutStallRecord strips them. The architect's view and the worker map are
// copied, so the state the source answered is left as it was.
func withoutClaimRoundReport(issue Issue) Issue {
	strip := func(v *ClaimView) {
		v.UpdateState, v.UpdateReaskedForPushCheck, v.UnpromptedTurnTakesAnswerBack, v.Wake = "", false, false, nil
	}
	if issue.Architect != nil {
		architect := *issue.Architect
		strip(&architect)
		issue.Architect = &architect
	}
	if issue.Workers != nil {
		workers := make(map[claim.Role]PhaseView, len(issue.Workers))
		for role, worker := range issue.Workers {
			strip(&worker.Claim)
			workers[role] = worker
		}
		issue.Workers = workers
	}
	return issue
}

// withoutCheckRole is the issue whose check carries no `role`, the one key checkRoleContract
// added to this route. The key is left off rather than emptied, because the strict reader before
// the contract refuses it as surely when it is empty as when it names a role, which is why
// CheckView.MarshalJSON writes the key on every body but this one (CheckView.roleHidden), as
// Admission.MarshalJSON leaves off `trees`. The flag is set on a check of the issue's own, so the
// state the source answered is left as it was.
func withoutCheckRole(issue Issue) Issue {
	if issue.Check != nil {
		check := *issue.Check
		check.roleHidden = true
		issue.Check = &check
	}
	return issue
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

// answerUnparsed names a body no plugin parses: the operator's own answers and the controller's
// secret and credential, which `legion` the command and the daemon's own controller read. It is
// no row of Answers, which is the plugin's table alone, and it shapes to nothing, since no
// contract describes a shape for a body no plugin reads (writeUnparsedAnswer).
const answerUnparsed AnswerID = "unparsed"

// answerWritten is told the pattern of the route and the id of the answer of every body this
// package writes (writeAnswerFrom). It is nil in a daemon and set by one test of this package,
// which holds each pair to the table: an answer's Routes say which doors write it, and a door
// that writes an id those Routes do not name shapes its body to a contract that door never
// declared. The hook lives here rather than in the registration because a door writes more than
// one answer — its own, and a refusal for every caller it turns away — so no one id can be
// bound to a route.
var answerWritten func(pattern string, id AnswerID)

// writeAnswer writes body as the answer id names it, shaped down to the contracts the caller
// reads at (contractsFor, shapeAnswer). Every body this package writes goes through it, or
// through writeAnswerFrom which it stands on, and nothing else calls writeJSON, so a shape a
// later contract moves cannot be written past the shaping.
//
// It hands contractsFor no claims, which costs a caller nothing: a caller that states its own
// contract is placed by its header alone, and the state and the record are the two answers whose
// shape has moved since ServingFloor. Both are written by writeAnswerFrom, which hands the
// claims of the very snapshot each answers. Every other answer is the identity at every contract
// this daemon serves, so a read of the claims for one of them would decide nothing: such an
// answer is written without one, and contractsFor reads no pod for it either.
func (s *server) writeAnswer(w http.ResponseWriter, r *http.Request, id AnswerID, status int, body any) {
	s.writeAnswerFrom(w, r, id, status, body, nil)
}

// writeAnswerFrom is writeAnswer for an answer whose own claims place the caller: claims are the
// project's claims as the body being answered holds them, which the state route reads through
// the transaction it projected that state in.
//
// It is also where the pair a write makes — the route's own pattern and the answer id it writes
// under — is told to answerWritten, so that the package's own census can hold each pair to the
// table (daemon_contract_header_test.go). Nothing is told anything in a daemon: answerWritten is
// nil there, and only a test of this package ever sets it.
func (s *server) writeAnswerFrom(w http.ResponseWriter, r *http.Request, id AnswerID, status int, body any, claims []supervise.Claim) {
	if answerWritten != nil {
		answerWritten(r.Pattern, id)
	}
	restore, strip := s.contractsFor(r, id, claims)
	writeJSON(w, status, shapeAnswer(id, body, restore, strip))
}

// writeUnparsedAnswer writes a success body no plugin parses: the operator's claim answers and
// the controller's secret and credential (answerUnparsed). It is written through the one writer
// like every other body, so a route cannot reach writeJSON around the shaping; what it says is
// that there is no contract to shape this body to. A refusal on those same routes is a refusal
// like any other, and is written as AnswerRefusal.
func (s *server) writeUnparsedAnswer(w http.ResponseWriter, r *http.Request, status int, body any) {
	s.writeAnswer(w, r, answerUnparsed, status, body)
}

// writeJSON is the one writer of a response body in this package. Only writeAnswerFrom calls it,
// and it stays unexported for that reason: a body written around it is a body no contract shaped.
func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
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
