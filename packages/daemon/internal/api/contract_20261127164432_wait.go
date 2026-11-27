package api

import (
	"github.com/sjawhar/legion/daemon/internal/record"
)

// 20261127164432 wait: dispatch://INFRA-2040 -- a phase worker declares that it waits on something outside its
// own work, until a time it names, and the record shows each wait that stands. Two claim routes
// are added, each authenticated by the registration's secret as `POST /legion/v1/claims/activity`
// is and held to the session the claim records. `POST /legion/v1/claims/wait` (`api.WaitRequest`:
// `claimToken`, `sessionId`, `secret`, `waitingOn`, and `wakeAt`, an RFC 3339 instant from one
// minute to 24 hours after the daemon records the declaration) answers `{wait: {id, waitingOn,
// wakeAt, declaredAt}}` (`api.WaitResponse`), or refuses with `WAIT_TEXT_REQUIRED`,
// `WAIT_TOO_SOON` or `WAIT_TOO_FAR` (400), or `WAIT_NOT_OPEN_PHASE` or `WAIT_HELD_BY_DESIGN_GATE`
// (409), and records nothing. `POST /legion/v1/claims/wait-news` (`api.WaitNewsRequest`: the
// claim, its session, its secret, the `waitId` the declaration's answer named and, optional, the
// reported delivery's own `deliveryKey`) answers `{ended}` (`api.WaitNewsResponse`), and
// `{ended: false, updateAsk: true}` for a key of the claim's own update ask, which ends nothing
// and records nothing (record.IsUpdateAskKey). Each refuses a stale plugin's claim with the
// stale-contract 409 as every claim route does (refuseIfStaleClaim), and neither refuses a caller
// by a contract of its own: a plugin below this entry knows neither route and sends neither.
// Every claim view of `GET /legion/v1/state` gains `wait` (`api.WaitView`, the four keys the
// declaration's answer named) while its worker's wait stands (workflow.WaitStands): declared and
// not ended, in the session, phase, generation and phase entry it was declared at, of a claim
// whose role works its issue's open phase, and not a merger the design gate holds. A wait row
// with no end that no longer stands — a fresh session, a phase that moved — shows on no claim.
// So `read_record`, which is the state's issue record, carries it, and the operator's plain
// `legion state` prints one `waits:` line naming each.
// The claim view of a merger also gains `hold` (`api.HoldView`: `step`, `artifactId` and
// `version`) while the closed design gate holds that merger (workflow.HeldByDesignGate), so
// `read_record` carries it, and the plain `legion state` prints one `holds:` line naming each.
// A caller below this entry — at the contract its `Legion-Plugin-Contract` header states, else at
// the contract its pod's claims record — is served every claim view without the two keys, on the
// state and on `GET /legion/v1/issues/{key}/record` alike (withoutWaitAndHold, the band this
// entry's `bands` row below adds to stateNarrowings), as a caller below 193 is served them
// without `updateState` and `wake`; the bands compose, and every reader from this entry on takes
// each key as optional (`packages/contracts/src/legion-api.ts`). `hold.step` is a design step,
// which a reader enumerates, and its three words arrived with the key at this entry (holdSteps),
// so no value strip is owed. The answer of `POST /legion/v1/claims/owed` (`api.OwedResponse`, the
// repository correction's claim route) gains the same two keys, read by the same two rules:
// `wait` while the claim's wait stands and `hold` while the closed design gate holds the claim,
// each named whether or not the claim owes, so a plugin can stop reminding a worker that waits or
// a merger the gate holds. A caller below this entry is served that answer without either key
// (shapeAnswer's row of AnswerOwed, withoutOwedWaitAndHold), which is why the owed answer is no
// identity answer from this entry on; every plugin that knows the route states its contract in
// the header, so the header alone places it. `hold.step` of that answer is a row of this entry's
// `enumeratedFields` too, beside the state's two.
// The two routes' answers arrived with their routes at this entry, so no contract below it
// records them (AnswerWait and AnswerWaitNews, each one of this entry's `identityAnswers`, which
// shapeAnswer serves as written), and their requests are rows this entry's test file adds to the
// request gate's table (pluginRequests). This entry's fixture holds what it changes: the key
// paths of `wait` and `hold` under every claim view of the state and of the record and on the
// owed answer, the two new answers, the two new requests and the three rows of `hold.step`.
// The change carries three store migrations beside it: `20261127165000_claim_waits`, the table
// that keeps each wait apart from the claim's per-launch columns so a relaunch keeps it,
// `20261127165100_outbox_kinds_with_the_wait_due`, the outbox kind of a wait's time, and
// `20261127165200_pending_delivery_gate_message`, the mark that a merger's pending task is the
// design gate's own message. None is part of this entry: a migration moves the daemon's own
// record, and the shapes a caller reads are the two routes and the two keys above.
//
// This entry was numbered 212 before the cutover (d41b74eb), and
// scripts/daemon-contract-convert.sh converted it at 7fa22de66d, before the merge of main at
// 67bcd7c78393.
//
// waitContract is the entry's id and never a contract number: each build numbers the entry by
// its place in main's merge order (entryNumber), and every row below names the entry by this
// constant, so no line moves when another entry merges before it. It is the entry that added two
// keys to each claim view: `wait`, the wait the claim's worker declared, while it stands
// (workflow.WaitStands), and `hold`, the closed design gate that holds a merger, while the hold
// stands (workflow.HeldByDesignGate). A plugin built before sjawhar/legion#2788 reads the state
// with a strict schema and would refuse the whole body for either key, and one built after
// strips a key it does not name, so the band is what keeps the two keys from a caller that did
// not ask for them rather than what keeps such a caller reading at all: a caller below this
// entry is served every claim view without them, on the state and on the issue record alike
// (withoutWaitAndHold). The same two keys arrived on the owed answer at this entry, and a caller
// below it is served that answer without them (withoutOwedWaitAndHold). The words of `hold.step`
// arrived with the key at this entry (holdSteps), so no value strip is needed.
const waitContract = 20261127164432

func init() {
	registerEntry(contractEntry{
		id: waitContract, slug: "wait",
		bands: []stateNarrowing{
			{contract: waitContract, issue: withoutWaitAndHold, dropped: "the claim views' wait and hold"},
		},
		answers: []Answer{
			// This change's entry: the wait a phase worker declared, as the daemon recorded it
			// (`api.WaitResponse`). The route arrives with the answer at this entry, so no contract
			// below it records either.
			{ID: AnswerWait, Routes: []string{"POST /legion/v1/claims/wait"}, Since: waitContract},
			// This change's entry: whether a report of news ended the wait it names
			// (`api.WaitNewsResponse`), arrived with its route as the wait's answer did.
			{ID: AnswerWaitNews, Routes: []string{"POST /legion/v1/claims/wait-news"}, Since: waitContract},
		},
		identityAnswers: []AnswerID{AnswerWait, AnswerWaitNews},
		enumeratedFields: []EnumeratedField{
			// `step` of a merger's hold, the gated step whose row of the closed design gate holds it
			// (api.HoldView.Step, record.DesignStep). It arrived with `hold` at this entry and is
			// served down below it with the whole key (shaping.go's withoutWaitAndHold), so no body
			// below this entry carries a value of it. The architect's view and each worker's carry
			// the one key, as every claim view does, though the gate holds a merger alone.
			{Answer: AnswerState, KeyPath: "issues.*.architect.hold.step", Values: holdSteps},
			{Answer: AnswerState, KeyPath: "issues.*.workers.*.claim.hold.step", Values: holdSteps},
			// `step` of the owed answer's hold, the gated step whose row of the closed design gate
			// holds the asking merger (api.OwedResponse.Hold, record.DesignStep). It arrived with
			// `hold` at this entry and is served down below it with the whole key (shaping.go's
			// withoutOwedWaitAndHold), so no body below this entry carries a value of it, as on the
			// state.
			{Answer: AnswerOwed, KeyPath: "hold.step", Values: holdSteps},
		},
	})
}

// holdSteps is every step of a deployment's design gate (record.DesignSteps) at the entry `hold`
// arrived at, which a merger's hold names its step from: no claim view and no owed answer carries
// a hold below that entry at all (shaping.go's withoutWaitAndHold and withoutOwedWaitAndHold).
var holdSteps = valuesFrom(waitContract, record.DesignSteps...)

const AnswerWait AnswerID = "wait"

const AnswerWaitNews AnswerID = "wait-news"
