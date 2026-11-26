package api

// 20261126014218 retro-after-close: dispatch://INFRA-5602 -- when an issue Legion started closes
// `done` and the newest run that started had no retro, Legion runs that retro after the close, and
// `POST /legion/v1/handoff/complete` on a `done` issue takes its report. A completion from the claim
// that serves the retro after the close (the task of no phase that the retro's start row carries,
// supervise.Claim.ServingRetro, or the retro's own turn that was running at the close) is that
// retro's report: answered 200 with the empty body (`HandoffCompleteResponse`) and recorded as
// `intake.RetroReport`, with no status write and no read of GitHub. It carries `summary`,
// `planRating` and `planReason` and nothing else: no rating is refused `400 PLAN_RATING_REQUIRED`
// whatever contract the plugin states, a rating of the wrong form `400 PLAN_RATING_INPUT`, a verdict
// `400 RETRO_VERDICT_UNEXPECTED`, a commit `400 RETRO_COMMIT_UNEXPECTED`, a wake `400
// CHECK_WAKE_UNEXPECTED`, a handoff record `409 HANDOFF_RECORD_MISMATCH` and a `returnTo` `400
// RETURN_TO_NEEDS_FAIL` (or `INVALID_RETURN_TO`). A report of a retro that is not open is refused
// `409 RETRO_NOT_OPEN`, naming how it ended (its report, its 24-hour bound with no report, or a
// `todo`), one from a claim other than the retro's agent `409 RETRO_AGENT_MISMATCH`, and the same
// report sent again `409 HANDOFF_ALREADY_RECORDED`. Every other completion on a `done` issue stays
// `409 CHECK_LOOK_NOT_SERVED`, and its text now names whose report the issue takes: the production
// check's role, the retro's agent, or that the retro ended at its bound. Every refusal records
// nothing, so the report sent again, corrected, is taken.
//
// No shape moves, so the entry adds no row to a table the daemon serves from: the request and the
// answer keep their schemas, and the codes are values of a refusal's `code`, which no reader
// enumerates. The number moves for the skew. A plugin at this contract or later posts the retro's
// report on a `done` issue (`verdict: ""` with `planRating` and `planReason`), which a daemon below
// it refuses `CHECK_LOOK_NOT_SERVED`, so the boot gate holds the daemon and the plugin to one
// number. A plugin below it never posts such a report: it reads the route as before, and a retro
// after a close nobody reports ends at its bound.
const retroAfterCloseContract = 20261126014218

func init() {
	registerEntry(contractEntry{id: retroAfterCloseContract, slug: "retro-after-close"})
}
