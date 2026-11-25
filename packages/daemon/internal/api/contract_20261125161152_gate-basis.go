package api

import "github.com/sjawhar/legion/daemon/internal/record"

// 20261125161152 gate-basis: dispatch://INFRA-3320 -- a root's design-gate step can stand open on an ancestor's
// approval, or wait, closed, on an ancestor's open approval request: a basis. Three surfaces
// move.
// `POST /legion/v1/gates/register` (`api.GateRegisterRequest`) takes three optional fields that
// come together or not at all: `basisArtifactId`, the same step's document of an ancestor on the
// issue's Dispatch `child_of` chain; `basisVersion`, the version of it a person approved or an
// open approval request asks a person to approve; and `reason`, 1 to 1,000 characters with no
// decision block, which the daemon posts on the root's issue. A call with a basis is refused
// with a code that names what Dispatch shows (`BASIS_INCOMPLETE`, `BASIS_NOT_ON_CHAIN`,
// `BASIS_NOT_NEAREST`, `BASIS_NOT_IN_LEGION`, `BASIS_ACCEPTED`, `BASIS_NOT_APPROVED`,
// `BASIS_VERSION_MISMATCH`, `BASIS_NOT_BY_PERSON`, `BASIS_REQUEST_NOT_OPEN`,
// `BASIS_NO_REQUIREMENTS`, `ROOT_VERSION_NOT_LATEST`, `ROOT_CHANGES_REQUESTED`, the six
// `VIEW_` codes, `REASON_INVALID` and `REASON_HOLDS_DECISION_BLOCK`) and records nothing. The
// answer is unchanged, and a call that names none of the three fields is the call it was.
// Every root issue record of `GET /legion/v1/state` — the record `read_record` reads — gains
// two things. Its `gatedSteps` entries take two status words beside the four of 55:
// `open_on_basis`, a step its architect registered on a person's approval of the ancestor's
// document, whose `approvedVersion` is the root's version registered, which no person approved;
// and `pending_on_basis`, a step that waits, closed, on the ancestor's open approval request.
// Each such entry carries `basis`: the ancestor `issue`, its document's `artifactId`, the
// `version` approved or asked for, and `approver` and `approvedAt` for an approval or `askId`
// for a request. `designGate.approvedVersion` of a step open on a basis is that same version
// registered, so a reader that names a person's approval reads `gatedSteps`. And every root of
// the daemon's project carries `treeRuns`, whether its tree runs under the workflow by the
// daemon's own rule (`record.TreeRuns`): its lifecycle stands open under the workflow, the root
// does not linger, and it does not wait in the line for its tree to be opened. The record held
// no field that decided it in every case: a root a re-admission left at `todo` runs nothing
// whatever its architect's claim reads.
// The architect's `catch-up` notice states a step on a basis too: each entry of its `gates` may
// carry `pending` and `basis` (`issue`, `artifact`, `version`, `approver`, `approved_at`, `ask`),
// and a step open on a basis reads `open` with no `approved`, since no person approved the
// version its root registered. No plugin decodes the notice: it reaches the architect's session
// as the daemon writes it, so it needs no narrowing.
// `POST /legion/v1/signoff` and `POST /legion/v1/roots/close` answer two more refusals, after
// `SIGNOFF_CHILD_IN_WORKFLOW` and in this order: `SIGNOFF_RESTING_ROOT_IN_WORKFLOW`, while a root
// whose step rests on the issue's approval is still in the workflow, and `SIGNOFF_CHILD_OPEN`,
// while a Dispatch child of the issue is open (`record.ChildOpen`). Each names the issues that
// hold it; the refusal's shape is the one every refusal has.
// `GET /legion/v1/state` and `GET /legion/v1/issues/{key}/record` serve a caller below this entry's contract — one
// that states a contract below it in `Legion-Plugin-Contract`, or else one from a pod whose
// claims record one — the shape its own contract describes, by 193's serving rule, and this
// entry adds one band and two value strips to that serving, and a word to a third strip, 219's
// `withoutUnadmittedHeldWords`. The band is the row of `gateBasisContract` in shaping.go's
// `stateNarrowings` (`withoutGateBasis`): it follows the contract the caller states, else its
// pod's highest, and serves every issue without `treeRuns`, and without `gatedSteps` on an
// issue a step of which carries `basis`. The bands compose, as
// 193's entry says of them: a caller below 178 is served the state without `paused` and
// `lastPause` as well, the entry of 178 above (`withoutPause`), a caller below 146 the issues
// without both entry times and `architectStalls`, the entry of 146 (`withoutEntryTimes`), and so
// on down `stateNarrowings`. 178's band and this one touch no key of each other's, and the bands
// of 193, 202, 220, 228, 246, 250, 256 and 275 stand between them (232 adds none), so a caller at 202
// through 275 is served the pause, the keys 193 added and the repositories block 202 writes, and
// none of this entry's keys and words (a caller that states 203 or 204 is served 202's records,
// the nearest recorded contract at or below it, one that states 205 to 212 is served 205's, which
// are 202's under its own number, as 205's entry says, one that states 213 to 216 is served 213's,
// which are 205's with `caches` among the words of a capability row's `source`, as 213's entry
// says, one that states 217 or 218 is served 217's, which are 213's under its own number, as
// 217's entry says, one that states 219 is served 219's, which are 217's with the owner's plan's
// routes, fields and words, as 219's entry says, one that states 220 to 227 is served 220's,
// which are 219's with each assignment's `plan`, as 220's entry says, one that states 228 to 231
// is served 228's, which are 220's with `intakeGiveUps` and the two give-up answers, as 228's
// entry says, one that states 232 to 245 is served 232's, whose answer records are 228's under
// its own number, since 232 adds two fields to a request and moves no answer's shape, one that
// states 246 to 249 is served 246's, which are 232's with each issue's `podHold`, as 246's entry
// says, one that states 250 to 255 is served 250's, which are 246's with each issue's `priority`
// and its waitingLine's `priority`, as 250's entry says, one that states 256 to 274 is served
// 256's, which are 250's with each claim view's `lastDeathAt` and `lastStartFailureAt`, as 256's
// entry says, and one that states 275 is served 275's, which are 256's with the state's
// `dispatchCredential`, as 275's entry says, so a caller below 275 is served no
// `dispatchCredential`, one below 256 is served no `lastDeathAt` or `lastStartFailureAt`, one below
// 250 no `priority` either, one below 246 no `podHold` either, one below 228 no `intakeGiveUps`
// either, one below 220 no assignment's `plan` either, and one below 219 none of 219's additions
// either), a
// caller at 193 through 201 the pause, 193's keys and the block in 152's shape (one that states
// 194 to 198 is served 193's shape and one that states 199 to 201 199's, and 199's is 193's, since
// 199 moves no shape of an answer a plugin parses), a caller at 178 through 192 the pause and the
// block in 152's shape and neither 193's keys nor this entry's, and a caller at 146 through 177
// the entry times and none of the pause, 193's keys and this entry's.
// The strip of `gatedSteps` is not for the key's sake alone: every plugin from 55 on reads
// `status` with a strict enum of the four words and refuses the whole body for a fifth, and no
// word of the four is true of such a step — `waiting` would send its architect to ask a person
// for an approval of its own, and `approved` names a person's approval no person gave — so such a
// caller is served no entry of that issue at all rather than a wrong one. The two words are values
// of an enumerated field, which this entry adds to answers.go's `EnumeratedFields`
// (`gatedStepStatuses`), so a value strip holds them back too, following the contract the caller
// states, else its pod's lowest: `withoutUnadmittedStepStatuses`, a row of shaping.go's
// `valueStrips`, serves an issue any step of which reads a word the caller does not admit with no
// `gatedSteps`. Every reader from this entry's contract on takes `treeRuns` and `basis` as optional and reads the
// six words (`packages/contracts/src/legion-api.ts`).
// The change ships store migrations `20261114064234_design_gate_basis` and
// `20261122154643_outbox_kinds_with_the_basis_seeds`, which keep the basis on the gate row and
// admit the two outbox kinds that read it; they move the daemon's own record and are no part of
// this number. Nor are the new notice kinds `basis-ended`, `basis-changes-requested` and
// `resting-root`, which reach an architect's session as text, as every notice kind does.
// The same contract carries a parent's wait on its open children, a new phase value
// `awaiting_children` (dispatch://INFRA-3320 R11 to R13). `GET /legion/v1/state` serves an issue in
// it with `phase: awaiting_children`, which `LEGION_WORKFLOW_PHASES` names, and an
// `admission.held` entry for such a parent with `reason: awaiting_children`, which
// `LEGION_HELD_REASONS` names. A caller below this entry's contract knows neither word — its phase enum and its held
// words are the ones before this change — so the daemon serves it that issue's `phase` as
// `admitted`, the status that names no worker's phase, and that held entry's `reason` as
// `children_first`, the word a started parent's wait took over. Both are value strips, rows of
// shaping.go's `valueStrips` that read each word's arrival from answers.go's `EnumeratedFields`
// (`phases`, `heldReasons`) and follow the contract the caller states, else its pod's lowest:
// `withoutUnadmittedPhases` and `withoutUnadmittedHeldWords`. `heldFrom` and
// `pendingTask.phase` read `LEGION_WORKFLOW_PHASES` too, so the table declares the word for them
// as well (`workflowPhases`), and `withoutUnadmittedPhases` leaves either key off where it names
// the word for a caller below this entry's contract; the daemon writes the word in neither, since `heldFrom` is null
// in the phase and a hold never comes from it, and no task is queued for a phase no role works.
// The change ships store migration `20261114232555_awaiting_children`, which adds the phase value
// and the parent's held start to the issues table; it moves the daemon's own record and is no part
// of this number.
// `POST /legion/v1/phase/backward`, `POST /legion/v1/signoff` and 219's `POST /legion/v1/tasks/stop`
// on a parent in that phase answer one more refusal, `WAITING_ON_CHILDREN`, naming its open
// children and the start it holds: each acts on a phase or on the task standing in one, and the
// parent stands in none (dispatch://INFRA-3320 R11). The refusal's shape
// is the one every refusal has. The new notice kind `awaiting-children`, which tells the parent's
// architect of the wait in place of the phase-finished notice of the start it holds, is no part
// of this number either: it reaches an architect's session as text, as every notice kind does.
// The forward merge of main at 0497e861b0d9 brought in 205's removal of the operation that took a
// child out of the workflow through the daemon, and this entry's refusals and notices lost their
// sentences that named it: `SIGNOFF_CHILD_IN_WORKFLOW` and `SIGNOFF_CHILD_OPEN`, as
// `POST /legion/v1/signoff` and `POST /legion/v1/roots/close` answer them, carry the sentence on a
// status the daemon has not taken yet and the rules for ending a child (`record.StatusNotTakenYet`,
// `record.EndChildRule`), followed by the sentence on asking the person (workflow's
// endChildrenFirst), and the `awaiting-children` notice and the `children-first` notice end a
// child by the rules alone.
// `WAITING_ON_CHILDREN` on a parent with an open child, as `POST /legion/v1/signoff`, the
// architect's `POST /legion/v1/phase/backward` and `POST /legion/v1/tasks/stop` answer it, ends
// with the same sentence on a status not taken yet and the same rules; as a phase worker's
// `POST /legion/v1/phase/backward` answers it, it ends by telling the worker that its architect
// acts on the children, and gives none of the architect's rules, since a worker ends no child.
// Each refusal keeps its code and the shape every refusal has, so no reader's parse moves.
// The forward merge of main at c4196c824b7d brought in 213's `caches`, a third word of a
// capability row's `source` (dispatch://INFRA-4036, 213's entry above), whose value strip runs
// beside this entry's two in shaping.go's `valueStrips` and serves it to a caller at 213 and
// above, so this entry's own records name `caches` in the domain of `capabilities.*.source` as
// 213's do.
//
// gateBasisContract is the daemon contract that let a root's gated step stand open on an
// ancestor's approval or wait on an ancestor's approval request (dispatch://INFRA-3320, R1 and R2), served each root whether its tree runs, and added the phase
// awaiting_children and the held word of that name (R11 to R13). It added two keys to an issue:
// the gated step's `basis`, which rides only on a step of the two status words it added, and each
// root's `treeRuns`. A plugin before it is served the state without `treeRuns`, which one built
// before sjawhar/legion#2788 would refuse the whole body for and one built after strips, and
// without the `gatedSteps` of every issue a step of which carries `basis` (withoutGateBasis, its
// row of stateNarrowings). The issue keeps every other key, its `designGate` among them.
//
// It added four values to enumerated fields as well, each newer than its field, so each rides a
// value strip and not this band (fieldArrivals.heldBack says why). `open_on_basis` and
// `pending_on_basis` joined `gatedSteps[].status` (answers.go's gatedStepStatuses), which a reader
// from gatedStepsContract on parses with an enum of the four words before them, refusing the whole
// body for a fifth, and no word of the four is true of such a step: `waiting` would send its
// architect to ask a person for an approval of its own, and `approved` names a person's approval
// no person gave. So an issue with such a step is served no `gatedSteps`
// (withoutUnadmittedStepStatuses). `awaiting_children` joined an issue's `phase` and the two
// fields that read the workflow's phases, `heldFrom` and `pendingTask.phase` (phases,
// workflowPhases): the phase is served as `admitted`, the value the wait replaced and the status
// that names no worker's phase, and the other two are left off where they name it
// (withoutUnadmittedPhases). And `awaiting_children` joined `admission.held[].reason`
// (heldReasons), served as `children_first`, the word a started parent's wait took over
// (withoutUnadmittedHeldWords, heldReasonStandIn).
//
// Of the bands of stateNarrowings, dispatchCredentialContract's stands right below this one,
// causeTimesContract's below that, priorityContract's below that, podHoldContract's below that, intakeGiveUpsContract's below that,
// assignmentPlanContract's below that, repositoriesLiveContract's below that and
// shapedAnswersContract's below that, and this band touches no key of any of theirs:
// shapedAnswersContract's takes `lowestPluginContract` and the claim views' `updateState` and
// `wake`, repositoriesLiveContract's writes the `repositories` block in contract 152's shape,
// assignmentPlanContract's takes each assignment's `plan`, intakeGiveUpsContract's takes the
// state's `intakeGiveUps`, podHoldContract's takes the issues' `podHold`, priorityContract's takes
// the issues' `priority` and their waitingLine's `priority`, causeTimesContract's takes the claim
// views' `lastDeathAt` and `lastStartFailureAt`, dispatchCredentialContract's takes the state's
// `dispatchCredential`, and this one takes `treeRuns` and some
// `gatedSteps`. So a caller from 202 up to the number below this contract is served the keys 193
// added and the block 202 writes and none of this contract's keys and words. One that states a
// number between 202 and this contract is served the nearest recorded contract at or below it
// (recordedContractAtOrBelow): one at 203 or 204 is served 202's, one at 205 to 212 is served
// 205's, whose records are 202's under its own number, as version.go's 205 entry says, one at 213
// to 216 is served 213's, which are 205's with `caches` among the words of a capability row's
// `source` (cachesSourceContract), one at 217 or 218 is served 217's, which are 213's under its own
// number, as version.go's 217 entry says, one at 219 is served OwnerPlanContract's, which are
// 217's with the owner's plan's routes, fields and words, one at 220 to 227 is served
// assignmentPlanContract's, which are OwnerPlanContract's with each assignment's `plan`, one at
// 228 to 231 is served intakeGiveUpsContract's, which are assignmentPlanContract's with
// `intakeGiveUps` and the two give-up answers, one at 232 to 245 is served 232's, whose answer
// records are intakeGiveUpsContract's under its own number, as version.go's 232 entry adds two
// fields to a request and moves no answer's shape, one at 246 to 249 is served podHoldContract's,
// which are 232's with each issue's `podHold`, one at 250 to 255 is served priorityContract's,
// which are podHoldContract's with each issue's `priority` and its waitingLine's `priority`, one at
// 256 to 274 is served causeTimesContract's, which are priorityContract's with each claim view's
// `lastDeathAt` and `lastStartFailureAt`, and one at 275 is served dispatchCredentialContract's,
// which are causeTimesContract's with the state's `dispatchCredential`. So a caller below
// dispatchCredentialContract is served no `dispatchCredential`, one below causeTimesContract no
// cause times either, one below priorityContract no `priority`
// either, one below podHoldContract no `podHold` either, one below intakeGiveUpsContract no
// `intakeGiveUps` either, one below assignmentPlanContract no assignment's `plan` either, and one
// below OwnerPlanContract neither the owner's plan's additions nor this contract's. A caller from
// 193 to 201 is served the keys 193 added, the block in contract 152's shape and none of this
// contract's: one that states a number from 194 to 198 is served 193's shape, and one that states
// 199, 200 or 201 is served 199's, which is 193's, since 199 moves no shape of an answer a plugin
// parses.
// Every such caller states its contract: podContracts leaves out a claim at shapedAnswersContract
// or above, so a pod reading places no caller there.
const gateBasisContract = 20261125161152

func init() {
	registerEntry(contractEntry{
		id: gateBasisContract, slug: "gate-basis",
		bands: []stateNarrowing{
			{contract: gateBasisContract, issue: withoutGateBasis,
				dropped: "the issues' treeRuns and the gatedSteps of every issue a step of which stands on an ancestor's basis"},
		},
		valueStrips: []valueStrip{
			{issue: withoutUnadmittedStepStatuses, dropped: droppedStepStatus},
			{issue: withoutUnadmittedPhases, dropped: droppedPhase},
		},
		enumeratedValues: []enumeratedValues{
			{answer: AnswerState, keyPath: issueKeyPath + "phase", values: awaitingChildren},
			{answer: AnswerState, keyPath: issueKeyPath + "heldFrom", values: awaitingChildren},
			{answer: AnswerState, keyPath: issueKeyPath + "architect.pendingTask.phase", values: awaitingChildren},
			{answer: AnswerState, keyPath: issueKeyPath + "workers.*.claim.pendingTask.phase", values: awaitingChildren},
			{answer: AnswerState, keyPath: "admission.held.*.reason", values: []EnumeratedValue{
				{Value: string(record.HeldAwaitingChildren), Since: gateBasisContract},
			}},
			{answer: AnswerState, keyPath: issueKeyPath + "gatedSteps.*.status", values: []EnumeratedValue{
				{Value: string(GatedStepOpenOnBasis), Since: gateBasisContract},
				{Value: string(GatedStepPendingOnBasis), Since: gateBasisContract},
			}},
		},
	})
}

// awaitingChildren is the phase word this entry adds to an issue's `phase` and to the two fields
// that read the workflow's phases, `heldFrom` and `pendingTask.phase`, one value at each row.
var awaitingChildren = []EnumeratedValue{{Value: "awaiting_children", Since: gateBasisContract}}
