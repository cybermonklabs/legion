package api

// 20261125174555 running-step: a later version of an owner's plan can change the text of a running
// task alone, and register_plan's answer names where each running task stands once the version is
// taken. Each task of `api.PlanRegisterResponse` that runs gains `running`, a list of one entry, the
// act it stands at (`api.RunningStep`: `kind`, one of `step`, `retro` and `merge`; `place`, from 1
// for a step and 0 for the retro and the merge; and `role`, the role whose agent works that act),
// left out for a task that waits, is stopped or is done. The route reads it from the record once
// the version is taken, and reads it the same way for a retry of a registration taken. A read that
// fails answers `500 RECORD_UNAVAILABLE` with the version taken, a code register_plan did not
// answer before, and a value of the refusal's `code`, which no reader enumerates; the same
// registration given again reads it again. A caller below this entry is served each task without
// `running` (shaping.go's AnswerPlanRegister case, `withoutRunningSteps`), the answer its reader
// read before. The plugin reads `kind` and `role` as strings, so no enumerated field arrives. A
// version that removes a running task or changes more of it than its text is still
// `409 PLAN_REFUSED`, whose reason now says that the task runs and that a later version can change
// only its text. An issue whose experiment's group runs the project's default steps takes such a
// version of its task default too, which keeps the plan's source default, and every other plan of
// it is still `409 PLAN_NOT_TAKEN`. No route, request field or other key moves. (The entry was
// numbered by hand until main stopped numbering contracts, last as 296, and is converted here.)
//
// runningStepContract is the entry's id: a caller whose contract is below the entry's number
// (entryNumber) is served each task of register_plan's answer without `running`.
const runningStepContract = 20261125174555

func init() {
	registerEntry(contractEntry{id: runningStepContract, slug: "running-step"})
}
