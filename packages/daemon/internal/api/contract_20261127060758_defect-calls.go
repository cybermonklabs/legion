package api

// 20261127060758 defect-calls: dispatch://INFRA-5580 (Task 9 of its plan) -- a merged issue waits
// in its own phase, `verifying`, for the checks of its goal, a role reports a defect of the change
// once the task's last step passed, and the state names what the close waits for. Four surfaces
// move.
// `POST /legion/v1/defects/report` (`api.DefectReportRequest`: `grantId`, `issue`, `defect`,
// `head`, 40 lowercase hex, and an optional `check`) records a defect of the issue's change for
// its next change and answers `api.DefectReportResponse`: `defect`, the defect's id as a decimal
// string, `state`, `recorded`, `same` (an open defect of the issue whose text, trimmed with its
// white space folded, is the report's) or `joined` (a reason of the issue the report's `check`
// names), and `nextChange`, the generation of the change that will carry it. It moves no phase
// and completes nothing: the role goes on with its step, and no plugin's phase hook closes the
// phase on it. The implementer reports in retro, the merger in merging and in verifying until its
// completion of the merged generation lands, and the owning architect, by the rule of sign_off,
// from retro on, a hold from retro or merging and a parent held before its retro for its children
// included. It refuses `409 DEFECT_BEFORE_LAST_STEP` before the task's last step passed, in
// accepting too, `403 DEFECT_CALLER_NOT_ALLOWED`, `400 DEFECT_TEXT_REQUIRED`,
// `400 DEFECT_TEXT_TOO_LONG` and `400 DEFECT_TEXT_DECISION_BLOCK` (the defect held to Dispatch's
// bounds of a message, as a backward move's reason is), `400 DEFECT_HEAD_REQUIRED`,
// `400 DEFECT_CHECK_INVALID` (a `check` that is no block id: letters, digits, `_` and `-`, at most
// 128), `409 DEFECT_ISSUE_CLOSED` and `409 TREE_LINGERING`, and records nothing.
// `POST /legion/v1/defects/withdraw` (`api.DefectWithdrawRequest`: `grantId`, `issue`, `defect`,
// the id, and `reason`) ends a reported defect for the owning architect, by the rule of sign_off,
// and answers `api.DefectWithdrawResponse`: `defect` and `state`, `withdrawn`. It refuses
// `403 DEFECT_WITHDRAW_NOT_OWNER`, `404 DEFECT_NOT_FOUND`, `409 DEFECT_IS_A_CHECK` (a failed
// check's or a production check's reason, which only a new approved version of the Verification
// document changes), `409 DEFECT_IN_MERGED_CHANGE`, `400 DEFECT_REASON_REQUIRED`,
// `400 DEFECT_REASON_TOO_LONG` and `400 DEFECT_REASON_DECISION_BLOCK` (the reason held to the
// bounds the defect's text is), and changes nothing. A withdrawal is said once in close-wait.md:
// the stored wait keeps it until a version holding it is uploaded. Each refusal's shape is the one
// every refusal has. Both answers arrive whole with their
// routes, so no contract below this entry records either, and no answer older than this entry
// moves. A next change that carries a defect is not part of this entry: the report records the
// defect and starts nothing.
// `GET /legion/v1/state` and `GET /legion/v1/issues/{key}/record` serve an issue whose pull request
// merged and whose close waits with `phase: verifying`, which `LEGION_WORKFLOW_PHASES` names from
// this entry on. Every plugin before it parses `phase` with a strict enumeration that lacks the
// word and would refuse the whole body, so a caller below this entry's contract — one that states
// a contract below it in `Legion-Plugin-Contract`, or else one from a pod whose claims record one —
// is served such an issue at `merging`, the phase a merged issue waited in before this entry gave
// the wait its own: a value strip, shaping.go's withoutUnadmittedPhases, which reads the word's
// arrival from answers.go's EnumeratedFields, where this entry adds it (`verifying`), and serves it
// as phaseStandIn names. The strip is gateBasisContract's row of `valueStrips`, which holds back
// every phase word the table gives a later arrival, so this entry adds no second row of it.
// `heldFrom` and `pendingTask.phase` read the same enumeration, so the entry adds the word at
// their rows too; the daemon writes it in neither, since verifying is never held from and no role
// works it. The base of this entry, dispatch://INFRA-5580's branch `INFRA-5580-done-by-checks`,
// serves `verifying` with no strip: this entry closes that gap, so that branch merges with it and
// never alone.
// Each issue in verifying, and each in retro or merging, or held from either, that holds a reported
// defect, carries `closeWait` (`api.CloseWaitView`): `since`, each of `beneath` and `openChildren`
// (`key`, `status`, `since`), `checks` (`id`, `when`, `answer`, `reason`, `since`), `production`
// (`state`, `generation`, `merge`, `outcome`, `nextLookAt`, `since`), `defects` (`id`, `kind`,
// `role`, `text`, `head`, `check`, `since`), `nextChanges`, `unapproved` (`checks`, `since`) and
// `unread` (`cause`, `since`), the issue's stored wait, which close-wait.md renders. A caller below
// this entry's contract is served every issue without it: this entry's row of `bands`
// (withoutCloseWait), which follows the contract the caller states, else its pod's highest. No
// member of it is an enumerated field: no plugin code reads one (`LEGION_ANSWER_READS`), so each
// reader takes every member as optional and each word as a string. An issue in verifying reads
// `why.state` `waiting`, whose sentence names the first thing its close waits for; the word is
// older than the floor, so no value moves.
// The change ships no store migration: the `defects` table and the issues' wait columns are
// `20261126223725_done_by_checks`'s, which moves the daemon's own record and is no part of this
// number. The notice kind `defect-reported`, which tells the owning architect of a defect another
// role reported, reaches an architect's session as text, as every notice kind does.
const defectCallsContract = 20261127060758

func init() {
	registerEntry(contractEntry{
		id: defectCallsContract, slug: "defect-calls",
		bands: []stateNarrowing{
			{contract: defectCallsContract, issue: withoutCloseWait, dropped: "the issues' closeWait"},
		},
		enumeratedValues: []enumeratedValues{
			{answer: AnswerState, keyPath: issueKeyPath + "phase", values: verifyingPhase},
			{answer: AnswerState, keyPath: issueKeyPath + "heldFrom", values: verifyingPhase},
			{answer: AnswerState, keyPath: issueKeyPath + "architect.pendingTask.phase", values: verifyingPhase},
			{answer: AnswerState, keyPath: issueKeyPath + "workers.*.claim.pendingTask.phase", values: verifyingPhase},
		},
		answers: []Answer{
			// What report_defect did with its defect (`api.DefectReportResponse`). The route arrives
			// with the answer, so no contract below this entry records either.
			{ID: AnswerDefectReport, Routes: []string{"POST /legion/v1/defects/report"}, Since: defectCallsContract},
			// What withdraw_defect did (`api.DefectWithdrawResponse`), arrived with its route as
			// report_defect's answer did.
			{ID: AnswerDefectWithdraw, Routes: []string{"POST /legion/v1/defects/withdraw"}, Since: defectCallsContract},
		},
		identityAnswers: []AnswerID{AnswerDefectReport, AnswerDefectWithdraw},
	})
}

// AnswerDefectReport and AnswerDefectWithdraw are the answers of the two calls this entry adds.
const (
	AnswerDefectReport   AnswerID = "defect-report"
	AnswerDefectWithdraw AnswerID = "defect-withdraw"
)

// verifyingPhase is the phase word this entry adds to an issue's `phase` and to the two fields
// that read the workflow's phases, `heldFrom` and `pendingTask.phase`, one value at each row.
var verifyingPhase = []EnumeratedValue{{Value: "verifying", Since: defectCallsContract}}

// withoutCloseWait is the issue with no `closeWait`, the key this entry added to an issue. It is
// an `omitempty` pointer, set only on an issue whose close waits or that holds a reported defect,
// so the band reports a move only for an issue that carries it. The issue is a value of its own,
// so the state the source answered is left as it was.
func withoutCloseWait(issue Issue) (Issue, bool) {
	moved := issue.CloseWait != nil
	issue.CloseWait = nil
	return issue, moved
}
