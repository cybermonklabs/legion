package api

import "github.com/sjawhar/legion/daemon/internal/record"

// 20261125170900 chain: dispatch://INFRA-4730 -- the daemon stores no tree: it reads every parent and child question
// from its copy of Dispatch's links (dispatch://INFRA-3320, R15 to R26), and three surfaces move.
// The status calls decide over that copy and queue their writes. `POST /legion/v1/issues/status`
// (`api.IssueStatusRequest`) takes two optional fields, the arguments Dispatch's parent rule asks
// for (R24): `children`, `include` or `unlink`, with `backlog` or `icebox`, and `parent`, `include`,
// with `todo`; `POST /legion/v1/children/rerun` (`api.RerunChildRequest`) takes `parent` the same
// way, and `POST /legion/v1/waves/release` takes none. Each refuses with the new codes
// `CHILDREN_ARGUMENT_REQUIRED`, `PARENT_ARGUMENT_REQUIRED` and `PARENT_NOT_RELEASED` where the rule
// asks; set_status refuses an issue of another Dispatch project, whose own daemon alone writes its
// status, with the new code `ISSUE_OF_ANOTHER_PROJECT` (422, naming the issue's project); and every
// architect call refuses `CHAIN_UNKNOWN` where the copy cannot read the issue's chain to its top;
// `ISSUE_OUTSIDE_TREE`, `CHILD_IN_OTHER_TREE`, `CHILD_NOT_ADOPTABLE`, `TREE_LINGERING`
// and `CHILD_REQUIRED` are answered no more, and `ISSUE_NOT_OWNED`, `ROOT_REQUIRED` and
// `GATE_ROOT_ONLY` read the chain and name the architect claim that may act. register_plan and
// stop_task (219's routes) refuse a caller that does not act on the issue over the chain
// `403 ISSUE_NOT_OWNED`, as every other architect call does, and answer `500 RECORD_UNAVAILABLE`
// where the daemon cannot read the issue's record and chain; the workflow's second judgement of an
// architect request under the fact lock answers `403 ISSUE_NOT_OWNED` where it answered
// `403 ISSUE_OUTSIDE_TREE`. Each refusal keeps the shape every refusal has. The three calls answer
// once their transaction commits, naming each status write they queued under a new key `queued`
// (`issue`, `status`, `row`): set_status and rerun_child answer `api.StatusCallResponse` (this
// entry's `status-call` answer, the empty object below this entry's contract), and
// release_children's answer gains the key; a reader below this entry's contract is served each
// answer without it (shaping.go's shapeAnswer, which compares with entryNumber(chainContract)).
// `GET /legion/v1/state` gains `daemon.convertedAt`, the time the project's first boot on this
// release finished converting the store, and `admission.held[].reason` takes two words,
// `chain_unknown`, an issue not at work on a chain the copy cannot read to its top, and
// `awaiting_todo`, an issue whose run starts as a root's does and that waits for a person's todo,
// as a root a person moved to `in_progress` before its first start does, and a root whose merge
// closed it and that a person reopened straight to `in_progress`. A caller below this entry's
// contract is served the state without `daemon.convertedAt` (`withoutConvertedAt`, this entry's
// `bands` row, which the registry appends to the derived `stateNarrowings()`) and each
// `chain_unknown` or `awaiting_todo` row as `unclassified` (219's value strip
// `withoutUnadmittedHeldWords`, which reads each word's arrival through `arrivalsAt` from this
// entry's `enumeratedValues` row of `admission.held.*.reason`), each following the contract the
// caller states, else its pod's. The band composes with every other band of `stateNarrowings()`:
// a caller below this entry's contract is served without the keys of each band its contract stands
// below as well, and this band touches no key of another.
// Each issue's `tree` is its gate root on the chain and `admission.trees` the roots in Legion's
// work, with their keys as they were; `lingerUntil` and `ancestry`, optional to every reader, are
// written no more, and `settings.linger_hours` reads 0.
// The waiting line inherits priority from the same top issue on the Dispatch chain that supplies
// its board rank, not from the gate holder. The keys contract 250 added do not change.
// The registration (`POST /legion/v1/claims/register`) answers as `tree` the issue's gate root at
// the claim's last launch, and `POST /legion/v1/grants` takes any `tree`: a grant acts only as the
// chain allows. Neither answer's shape moves.
// (This change took 215, above main's 205 and above 214, which legion/INFRA-4728 then declared and
// this branch carries: a REST raw read of each open head's version.go at 2026-11-22 found 206, 210,
// 212, 213, 214 (#4592, and #4596, this change's own head), 216, 217, 219 and 220 declared by open
// heads and 215 by none. At the forward merge of legion/INFRA-4728 that brought main in at
// c4196c824b7d, main carried 213 (dispatch://INFRA-4036, version.go's 213), below 214 and 215,
// and this change kept 215. At the forward merge of 2026-11-23 that brought legion/INFRA-4728 in
// at 6b3ecf1411 and main in at e96f1420b4, main carried 217 (version.go's 217) and
// legion/INFRA-4728 declared 232. INFRA-4728's census of 01:12:49Z to 01:16:31Z, with a REST raw
// read of each open head's version.go it had not read, made between 2026-11-23T02:11:04Z and
// 02:11:52Z, found 218 to 231 declared by other heads, 232 by #4173 and
// #4592, and nothing at 233 or above, so this change moved 215 → 233, the lowest number above 232
// that no open head declares (dispatch://INFRA-4730/artifact/contract-census-2026-11-23-infra4730-md).
// At the forward merge of 2026-11-23 that brought legion/INFRA-4728 in at 18edf7d778cf, which
// carries main at 4a8d82bfd090, main carried 219 (the owner's plan, version.go's 219) and
// legion/INFRA-4728 declared 239 (the gate-basis entry's number then). A count of each open head's
// version.go, a REST raw read of each of the 510 open heads' file between 2026-11-23T23:03Z and
// 23:05Z, found 237 declared by #5168 and #5169, 238 by #4771 and #5285, 239 by #4592, and nothing
// above 239; this change took 240, the lowest number above 239 that no open head declares. At the
// forward merge of 2026-11-24 that brought legion/INFRA-4728 in at 9751f6267041, which carries
// main at
// 8517faba31ac, main carried 220 (the experiment's plan, version.go's 220) and legion/INFRA-4728
// declared 239. A read of `packages/daemon/internal/api/version.go` at each of the 521 open heads
// between 2026-11-24T02:52:01Z and 02:54:10Z found 239 declared by #4592, 240 by #3831, #5271 and
// this change, 241 by #4771 and #5307, and nothing above 241; this change took 242, the lowest
// number above every number another open head declares. At the forward merge of 2026-11-24 that
// brought legion/INFRA-4728 in at 98a5271d1bd6, which carries main at 25cbe2b315fb, main carried
// 246 (#4960's pod hold, version.go's 246) and legion/INFRA-4728 declared 249 (the gate-basis
// entry's number then). A read of `packages/daemon/internal/api/version.go` at each of the 492
// open heads at 2026-11-24T15:54Z found 243 to 248, 250 to 252, 254, 255 and 261 declared by other
// heads, and INFRA-4728's census of the 478 open heads, ending at 16:01Z, found 249 and 253
// declared by none; this change moved 242 → 253, the lowest number above 249 that no other open
// head declares.
// At the forward merge of legion/INFRA-4728 at 6d85200c, which carries main's #5399
// (dispatch://INFRA-1569), the change became this entry, which picks no number: each build gives
// it one by main's merge order (entryNumber).)
// dispatch://INFRA-3118 allocated #4596 the hand-picked number this entry replaces.
//
// chainContract is the daemon contract at which the daemon stores no tree and reads every parent
// and child question from its copy of Dispatch's links (this entry, dispatch://INFRA-4730).
// It added one key a plugin parses to the state, `daemon.convertedAt`, the time the project's first
// boot on this release finished its conversion, which a plugin below it is served without
// (withoutConvertedAt, this entry's `bands` row); two held words, `chain_unknown` and
// `awaiting_todo`, which a value strip serves to a reader below it as `unclassified` (this entry's
// `enumeratedValues` row, which withoutUnadmittedHeldWords reads); and `queued`, the status
// writes a status call queued, to the answers of set_status, rerun_child and release_children,
// which a reader below it is served without (shapeAnswer). The state's `tree` and
// `admission.trees` keep their keys and read the chain (R22 of dispatch://INFRA-3320), so no band
// is owed for them. The rest of the contract is requests and refusal codes, which no reader
// enumerates.
//
// It is the entry's id, never a contract number: entryNumber(chainContract) is the number a build
// gives the entry, by main's merge order, and every comparison with this entry's contract reads it.
const chainContract = 20261125170900

func init() {
	registerEntry(contractEntry{
		id: chainContract, slug: "chain",
		bands: []stateNarrowing{
			{contract: chainContract, state: withoutConvertedAt, dropped: "daemon.convertedAt"},
		},
		answers: []Answer{
			// The status-call answer (chainContract) lists the writes set_status or rerun_child queued.
			// Earlier callers still get the empty object.
			{ID: AnswerStatusCall, Routes: []string{
				"POST /legion/v1/issues/status",
				"POST /legion/v1/children/rerun",
			}, Since: chainContract},
		},
		enumeratedValues: []enumeratedValues{
			{answer: AnswerState, keyPath: "admission.held.*.reason", values: []EnumeratedValue{
				{Value: string(record.HeldAwaitingTodo), Since: chainContract},
				{Value: string(record.HeldChainUnknown), Since: chainContract},
			}},
		},
	})
}

const AnswerStatusCall AnswerID = "status-call"
