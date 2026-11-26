package api

// 20261125203834 check-states: a phase completion carries no verdict. `POST /legion/v1/handoff/complete`
// (`api.HandoffCompleteRequest`, contracts' `LegionHandoffCompleteRequest`) keeps `verdict` for a
// production check's report alone: a phase completion that names one from a plugin at this
// contract or a later one is refused 400 `VERDICT_NOT_TAKEN`, "a phase completion carries no
// verdict: Dispatch's check states at the tested head decide the testing phase; send it again
// without verdict; nothing was recorded", and the refusals `TESTER_VERDICT_REQUIRED` and
// `VERDICT_ROLE_FORBIDDEN` go. The states Dispatch publishes of the issue's `before_merge` checks
// as `check.state_changed` decide the testing phase: the tester completes with no verdict, naming
// its `test` record, whose `check_states_serial` the daemon waits to hold every state event
// through; at the head that record names, the move waits while a check of the head's set has no
// state, or no result while its CI run goes on, whatever the other checks read; then the issue
// goes on to the task's next step, `reviewing` in the default steps, when each check passes, and
// otherwise back to the step the tester's completion names in `returnTo`, or, with none named, to
// the first implementation step before the tester, with a task naming each check that does not
// pass with its state and reason, a state Dispatch could not read (`read_failed`) among them
// (workflow's decideTesting). `returnTo`, 219's field, rides a tester's completion with no
// verdict: the route refuses it on any other completion 400 `RETURN_TO_NEEDS_FAIL`, the workflow
// keeps it with the completion until the states decide (`phases.return_to`, store migration
// 20261125000603_phase_return_to), and a pass does not use it. A tester's plugin below this
// contract, in a turn when its daemon was upgraded, still sends `pass` or `fail`: the route takes
// its completion and ignores the verdict, which no longer makes the completion's key, and any
// other role's verdict below it is refused as above (`api.CheckStatesContract`). The reviewer's
// task no longer names `Tester's verdict:`, and a `phase-finished` notice loses `verdict`: from the
// tester's phase its `reason` names each check that does not pass at the tested head, or what the
// move waits for. The instruction a `HANDOFF_STALE_ENTRY` refusal ends with, which the `legion`
// tool's description repeats, names no verdict: it ends "never this completion's record or
// commit". `POST /legion/v1/phase/backward` refuses the tester's move out of `testing`, to any
// target a backward row serves, `BACKWARD_REFUSED`, since the testing step ends by its checks'
// states: the tester records each check's result and completes instead, and the `legion` tool's
// description says so. A tester's phase completion names the head it tested: the route refuses
// one that names no commit 400 `TESTED_HEAD_REQUIRED`, before anything is recorded, from a plugin
// at this contract or below it, since the move reads the states at that head and at no other; the
// `legion` tool refuses a `test` record naming no head before it posts. No shape of
// `GET /legion/v1/state` moves. This contract is served with no code of its own (193's entry): no
// key path or value of any answer moves, and no request field, so a caller at it is served what
// one at 220 is, and its records in `contract-shapes.json` and `request-shapes.json` are 220's
// under its own number. `VERDICT_NOT_TAKEN` and `TESTED_HEAD_REQUIRED` are values of the
// refusal's `code`, which no reader enumerates.
// (This entry took 206 at first, above main's 202: a reading of `const DaemonAPIVersion` at
// refs/pull/<n>/head for each of the 358 open pull requests of cybermonklabs/legion, made on
// 2026-11-21 at 07:29Z, found 203, 204, 205, 207, 208 and 209 declared, and none declaring 206.
// The forward merge of 2026-11-22 that brought in 2feade15 brought main's 205 (the entry of 205
// above). A reading of every open pull request at that merge, which ended at
// 2026-11-22T10:01:29Z, found 206 (#3712), 207 to 220 and 223 and 224 declared, so this entry
// moved to 221, the lowest number above main's 205 that no other open pull request declares. The
// forward merge of 2026-11-23 that brought in 5685883d brought main's 213, below 221, so this
// entry kept 221. The forward merge of 2026-11-24 that brought in 4bd9a2b4 brought main's 217, 219
// and 220 (the entries above), and a reading of every open pull request at that merge, which ended
// at 2026-11-24T11:41:39Z, found 221 declared by #4049, #4392, #4682 and #5164 as well, and 222,
// 223 and 224 declared, so this entry moved to 225, the lowest number above the base's 220 that no
// other open pull request declares. Read the base's own number and every open pull request again
// at every forward merge rather than from this line.)
//
// CheckStatesContract is the daemon contract at which a phase completion carries no verdict (the
// entry of 225 above): a plugin at it or above has its completion naming one refused
// VERDICT_NOT_TAKEN, and a tester's plugin below it has its verdict ignored, so a worker mid-turn
// across the upgrade still completes and Dispatch's check states decide its phase. A later bump
// of DaemonAPIVersion leaves it where it is. It stands above every contract declared before the
// check states' change, which a plugin that still sends a verdict can state
// (TestCheckStatesContractIsAboveEveryContractDeclaredBeforeIt).
const CheckStatesContract = 20261125203834

func init() {
	registerEntry(contractEntry{id: CheckStatesContract, slug: "check-states"})
}
