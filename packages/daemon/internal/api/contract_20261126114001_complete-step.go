package api

// 20261126114001 complete-step: the owner of an issue, its own architect, reports a step of its own: a step whose key is
// owner, or the retro of a task whose retro is the owner's. A new route, `POST
// /legion/v1/steps/complete` (`grantId`, `issue`, `step`, `summary`, and, as a completion of that
// place carries them, `verdict`, `commit`, `handoffId`, `handoffVersion`, `returnTo`, `planRating`
// and `planReason`), the architect's `complete_step`, takes it; `step` names the step by its place
// in the task, 1 for its first, and 0 for the retro. It answers the empty object every fact route
// answers. It refuses a grant of any claim but the issue's own architect `403 NOT_THE_OWNER`, a
// step that does not run now or whose agent is not the owner `409 OWNER_STEP_NOT_RUNNING`, a step
// below 0 `400 INVALID_STEP`, and otherwise as the completion route refuses a report of that place.
// The owner's failed acceptance that names no step to return to, with no step of the type
// implementation before it, stops the task as any such failed step does. No plugin below 302
// calls it. A reviewer of an issue with no pull request record reports its step on `POST
// /legion/v1/handoff/complete` with the verdict `pass` or `fail` and a commit (`400
// STEP_VERDICT_REQUIRED`, `400 MISSING_FIELD`), as the owner's review step does on the new route,
// since no review on GitHub decides either; `VERDICT_ROLE_FORBIDDEN` now reads `only a tester, a
// role of the deployment or of the plan, the role acceptance, a reviewer of an issue with no pull
// request, and the owner in a review step report a verdict`. `POST /legion/v1/signoff` refuses a
// sign-off of an acceptance that is not its owner's own step, a step `acceptance` with no key,
// whose fresh agent ends it with its own verdict, `409 SIGNOFF_NOT_THE_OWNERS`. Each issue of the
// state, and the record route's issue, gains `placeRole`, the role of the claim that works its
// task's place (record.Issue.PlaceRole): the architect at a step or a retro the owner does, and
// otherwise the step's own role (`acceptance` for the fresh agent of a step `acceptance` with no
// key) or, at the retro, the role its agent runs as; it is absent on an issue with no plan rows
// and at the merge. So a reader such as `legion state` names who works an issue at accepting,
// which the phase alone names as the architect. A caller below 302 is served each issue without
// it (one band of shaping.go at `completeStepContract`, withoutPlaceRole): a plugin before
// INFRA-1048 reads an issue as a strict object. The request records of 302 hold the new route's
// request, and no recorded request field leaves. A claim of the role `acceptance`, the fresh agent
// of a step `acceptance` with no key, is a role outside Legion's six, which 258's band already
// leaves out of the state served a caller below 258. The two notice kinds this entry adds
// (`owner-step`, and `merge-forward-lost` for an ask or a review of a merge forward that did not
// reach its agent) ride inside a notice row's payload, as 189's does, and are no key or value of
// an answer a plugin parses. From this entry the last number of answers.go's SupportedContracts
// is `api.completeStepContract`, after 301; `deploymentRolesContract` stays 258,
// `strandedTokenContract` 259 and `startWaitTokenContract` 285.
// (This entry took 227 at first, the lowest number above 222 (the stranded claims' token, the
// entry above at its number then) that no other open pull request declared: a reading of `const
// DaemonAPIVersion` at the head of each of the 509 open pull requests, made between
// 2026-11-23T22:55:43Z and 22:56:54Z, read all 509 and found 227 declared by this change alone;
// 236 and 238, which it took at earlier readings, were declared by other open pull requests by
// then. The forward merge that brought the base's 301 (the entry above) into this change moved it
// from 227 to 302, above 301. Read main's own number and every open pull request again before a
// push rather than from this history.)
//
// completeStepContract is the daemon contract that added the owner's report of a step of its own,
// `POST /legion/v1/steps/complete` (version.go's 302), the reviewer's verdict where no review on
// GitHub decides its step, and each issue's `placeRole` in the state, the role of the claim that
// works its task's place. A plugin below it never calls the route, and is served each issue
// without `placeRole` (shaping.go's withoutPlaceRole).
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes moves
// that constant, and the contract this route arrived at stays where it is. It moves only with this
// change's own number, and it is the last number of answers.go's SupportedContracts, which names
// the constant rather than its number for that reason.
const completeStepContract = 20261126114001

func init() {
	registerEntry(contractEntry{
		id: completeStepContract, slug: "complete-step",
		bands: []stateNarrowing{
			{contract: completeStepContract, issue: withoutPlaceRole, dropped: "the issues' placeRole"},
		},
	})
}
