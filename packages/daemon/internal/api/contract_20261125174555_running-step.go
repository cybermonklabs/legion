package api

// 20261125174555 running-step: a later version of an owner's plan can change the text of a running task alone, and
// register_plan's answer names where each running task stands once the version is taken. Each
// task of `api.PlanRegisterResponse` that runs gains `running`, a list of one entry, the act it
// stands at (`api.RunningStep`: `kind`, one of `step`, `retro` and `merge`; `place`, from 1 for a
// step and 0 for the retro and the merge; and `role`, the role whose agent works that act), left
// out for a task that waits, is stopped or is done. The route reads it from the record once the
// version is taken, and reads it the same way for a retry of a registration taken. A read that
// fails answers `500 RECORD_UNAVAILABLE` with the version taken, a code register_plan did not
// answer before, and a value of the refusal's `code`, which no reader enumerates; the same
// registration given again reads it again. A caller below 296 is served each task without
// `running` (shaping.go's AnswerPlanRegister row, `withoutRunningSteps`), the answer its reader
// read before. The plugin reads `kind` and `role` as strings, so no enumerated field arrives. A
// version that removes a running task or changes more of it than its text is still
// `409 PLAN_REFUSED`, whose reason now says that the task runs and that a later version can change
// only its text. An issue whose experiment's group runs the project's default steps takes such a
// version of its task default too, which keeps the plan's source default, and every other plan of
// it is still `409 PLAN_NOT_TAKEN`. No route, request field or other key moves.
// (This entry took 233 at first, the lowest number above this branch's 219 that no open pull
// request declared at a reading of `const DaemonAPIVersion` at refs/pull/<n>/head made on
// 2026-11-23 near 04:45Z. The reading before the push of 2026-11-23, of all 518 open pull requests
// between 06:51:08Z and 06:54:29Z, found 233 declared by #4596 and 234 by #4065, and 235 by none,
// so this entry moved to 235: the lowest number above main's 217 and this branch's 219 that no
// open pull request declares. The forward merge of 2026-11-24 that brought in b83f99e7 brought
// main's 220 (the experiment group's entry above), and a reading of all 510 open pull requests
// between 2026-11-24T00:04:50Z and 00:08:08Z found 235 declared by this change's pull request
// alone, so this entry kept 235, and runningStepContract comes after assignmentPlanContract in
// SupportedContracts. The forward merge of 2026-11-24 that brought in 15338e75 brought main's 228
// (the intake give-ups' entry above), and a reading of all 490 open pull requests between
// 2026-11-24T10:12:37Z and 10:15:47Z found 235 declared by this change's pull request alone, so
// this entry kept 235, and runningStepContract comes after intakeGiveUpsContract in
// SupportedContracts. The forward merge of 2026-11-24 that brought in d2740571 brought main's 232
// (the turn outcome's entry above), and a reading of all 479 open pull requests between
// 2026-11-24T12:35:49Z and 12:38:57Z found 235 declared by this change's pull request alone, so
// this entry kept 235, and runningStepContract comes after 232 in SupportedContracts. The forward
// merge of 2026-11-24 that brought in 7e75126f brought main's 246 (#4960, the pod hold's entry
// above), and a reading of all 473 open pull requests between 2026-11-24T15:30:06Z and 15:33:18Z
// found 247 declared by #4580 and #5384, 250 by #5365, 251 by #5307 and 252 by #4540. 248 and
// 249 lie in 246 to 250, and each of 253 to 261 was declared by an open pull request or named for
// another change, so this entry moved to 262, and runningStepContract comes after podHoldContract
// in SupportedContracts. The forward merge of 2026-11-24 that brought in b113b8d3 brought main's
// 250 (#5365, the waiting line's priority, the entry above), and a reading of all 488 open pull
// requests between 2026-11-24T18:18:10Z and 18:22:06Z found 262 declared by this change's pull
// request alone (one head, #5399, could not be read), so this entry kept 262, and
// runningStepContract comes after priorityContract in SupportedContracts. The forward merge of
// 2026-11-24 that brought in bcc5d7e6 brought main's 256 (#5349, the cause times' entry above),
// and a reading of all 487 open pull requests between 2026-11-24T19:17:55Z and 19:21:53Z found
// 262 declared by this change's pull request alone (one head, #5399, could not be read), so this
// entry kept 262, and runningStepContract comes after causeTimesContract in SupportedContracts.
// The forward merge of 2026-11-25 that brought in ac1c9a332e brought main's 275 (#4392, the
// Dispatch credential's entry above), above 262, so this entry moved to 296, the number the
// architect who allocates contract numbers gave it at 10:07Z: a reading of all 502 open pull
// requests between 2026-11-25T10:01:14Z and 10:06:34Z found 276 to 278, 280 to 285, 288 to
// 290 and 292 declared above 275, and 296 by none (#5399, a registry, could not be read), and
// runningStepContract comes after dispatchCredentialContract in SupportedContracts. Read the
// base's own number and every open pull request again at the next merge rather than from this
// line.)
//
// runningStepContract is the daemon contract that added `running` to each task of
// register_plan's answer (version.go's 296, RegisteredTask.Running): the act a running task stands
// at once a later version that changes its text alone is taken. A caller below it is served each
// task without it (shaping.go's AnswerPlanRegister row, withoutRunningSteps). It is a literal of
// its own and the last number of SupportedContracts, for the reason OwnerPlanContract is one: a
// later contract moves DaemonAPIVersion, and the contract this key arrived at stays where it is.
const runningStepContract = 20261125174555

func init() {
	registerEntry(contractEntry{id: runningStepContract, slug: "running-step"})
}
