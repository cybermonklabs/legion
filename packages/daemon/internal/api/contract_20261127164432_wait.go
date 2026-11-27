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
// by a contract of its own: a plugin below 212 knows neither route and sends neither.
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
// A caller below 212 — at the contract its `Legion-Plugin-Contract` header states, else at the
// contract its pod's claims record — is served every claim view without the two keys, on the
// state and on `GET /legion/v1/issues/{key}/record` alike (shaping.go's band of `waitContract`,
// withoutWaitAndHold), as a caller below 193 is served them without `updateState` and `wake`;
// the bands compose, and every reader from 212 on takes each key as optional
// (`packages/contracts/src/legion-api.ts`). `hold.step` is a design step, which a reader
// enumerates, and its three words arrived with the key at 212 (answers.go's holdSteps), so no
// value strip is owed. The answer of `POST /legion/v1/claims/owed` (`api.OwedResponse`, 206's
// claim route) gains the same two keys, read by the same two rules: `wait` while the claim's
// wait stands and `hold` while the closed design gate holds the claim, each named whether or
// not the claim owes, so a plugin can stop reminding a worker that waits or a merger the gate
// holds. A caller below 212 is served that answer without either key (shapeAnswer's row of
// AnswerOwed, withoutOwedWaitAndHold); every plugin that knows the route states its contract
// in the header, so the header alone places it. `hold.step` of that answer is a row of
// answers.go's EnumeratedFields too, so the owed answer's record at 206 carries its empty domain.
// The two routes' answers arrived with their routes at 212, so no contract
// below records them (answers.go's AnswerWait and AnswerWaitNews), and their requests are
// recorded from 212 on (`request-shapes.json`). 212's records are 206's under its own number,
// with the key paths of `wait` and `hold` under every claim view of the state and of the record
// and on the owed answer, and the two new answers.
// The change carries three store migrations beside it: `20261121160000_claim_waits`, the table
// that keeps each wait apart from the claim's per-launch columns so a relaunch keeps it,
// `20261121160100_outbox_kinds_with_the_wait_due`, the outbox kind of a wait's time, and
// `20261121160200_pending_delivery_gate_message`, the mark that a merger's pending task is the
// design gate's own message. None is part of this number: a migration moves the daemon's own
// record, and the shapes a caller reads are the two routes and the two keys above.
// (This change declared 125, the lowest number above the base's 111 that no open pull request
// declared, read on 2026-11-14 against main at b994309153eba441588f393cabf50d0200684c49. It took
// 167, 170 and 176 at the forward merges of 2026-11-19, 183 at the first of 2026-11-20, and 194
// at the forward merge of 2026-11-20 that brought main in at 1c7c8253678f, which carried #4543's
// 188. It kept 194 at the forward merge of that day that brought main in at a4cd4949fce7, which
// carried INFRA-4108's 189, and kept it at the forward merge of that day that brought main in at
// 8a9540682d8a, which carried #3659's 193. It took 204 at the forward merge of 2026-11-21 that
// brought main in at 0d3786da5367, which carried #4495's 199, and 207 at the forward merge of that
// day that brought main in at a86c31c9fd62, which carried #4493's 202. It kept 207 at the merge
// of that day that stacked this change on cybermonklabs/legion#3712 (legion/INFRA-3136 at
// b40a08ba6f52), which carries INFRA-3136's 206, the entry right above, and took 213 at the push
// of Stage B that day. It takes 212 at the merge of that day that brought in #3712's a3cce239,
// which carries main's 205 (#4573) under #3712's 206. A renumber moves `waitContract` in
// shaping.go with it, and that one constant alone: it is the number the two keys of this entry
// are served down below, the arrival answers.go gives the two answers and the three words of
// `hold.step`, and the last number of answers.go's SupportedContracts.
// A reading of `const DaemonAPIVersion` at the head of each of the 420 open pull requests, listed
// at 2026-11-21T22:07:50Z and read by 22:11:24Z, gave 205, main's own number, to the heads that
// carry main's, and above it: 206 to #3712 and #4539; 207 to #3993, #4392, #4432, #4590, #4682,
// #4723 and #4805; 208 to #4175; 209 to #4049 and #4580; 210 to #4661; 211 to #4611 and #4902;
// 213 to #4553 as well as this change; 214 to #4592 and #4596; 215 to #4065; 216 to #3831,
// #4173, #4259 and #4540; and 217 to #2230, #4273 and #4771. So this entry takes 212, the lowest
// number above #3712's 206 that no other open pull request declares. Each of those is the number
// that head declared at that reading and no head's for good. Read the base's own number and every
// open head again at the last push rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// waitContract is the daemon contract this change lands under (version.go's 212,
// dispatch://INFRA-2040), and the contract that added two keys to each claim view: `wait`, the
// wait the claim's worker declared, while it stands (workflow.WaitStands), and `hold`, the closed
// design gate that holds a merger, while the hold stands (workflow.HeldByDesignGate). A plugin
// built before sjawhar/legion#2788 reads the state with a strict schema and would refuse the
// whole body for either key, and one built after strips a key it does not name, so the band is
// what keeps the two keys from a caller that did not ask for them rather than what keeps such a
// caller reading at all: a caller below this contract is served every claim view without them, on
// the state and on the issue record alike (withoutWaitAndHold). The same two keys arrived on the
// owed answer at this contract, and a caller below it is served that answer without them
// (withoutOwedWaitAndHold). The words of `hold.step` arrived with the key at this contract
// (answers.go's holdSteps), so no value strip is needed.
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes moves
// that constant, and the contract these keys arrived at stays where it is. It moves only with
// this change's own number, as the arrival contracts of this change's two new answers do
// (answers.go's AnswerWait and AnswerWaitNews).
const waitContract = 20261127164432

func init() {
	registerEntry(contractEntry{
		id: waitContract, slug: "wait",
		bands: []stateNarrowing{
			{contract: waitContract, issue: withoutWaitAndHold, dropped: "the claim views' wait and hold"},
		},
		answers: []Answer{
			// version.go's 212: the wait a phase worker declared, as the daemon recorded it
			// (`api.WaitResponse`). The route arrives with the answer, so no contract below 212 records
			// either.
			{ID: AnswerWait, Routes: []string{"POST /legion/v1/claims/wait"}, Since: waitContract},
			// version.go's 212: whether a report of news ended the wait it names (`api.WaitNewsResponse`),
			// arrived with its route as the wait's answer did.
			{ID: AnswerWaitNews, Routes: []string{"POST /legion/v1/claims/wait-news"}, Since: waitContract},
		},
		enumeratedFields: []EnumeratedField{
			// `step` of a merger's hold, the gated step whose row of the closed design gate holds it
			// (api.HoldView.Step, record.DesignStep). It arrived with `hold` at waitContract and is served
			// down below that contract with the whole key (shaping.go's withoutWaitAndHold), so no body
			// below 212 carries a value of it. The architect's view and each worker's carry the one key,
			// as every claim view does, though the gate holds a merger alone.
			{Answer: AnswerState, KeyPath: "issues.*.architect.hold.step", Values: holdSteps},
			{Answer: AnswerState, KeyPath: "issues.*.workers.*.claim.hold.step", Values: holdSteps},
		},
	})
}

// holdSteps is every step of a deployment's design gate (record.DesignSteps) at the contract
// `hold` arrived at, which a merger's hold names its step from: no claim view and no owed answer
// carries a hold below that contract at all (shaping.go's withoutWaitAndHold and
// withoutOwedWaitAndHold).
var holdSteps = valuesFrom(waitContract, record.DesignSteps...)

const AnswerWait AnswerID = "wait"

const AnswerWaitNews AnswerID = "wait-news"
