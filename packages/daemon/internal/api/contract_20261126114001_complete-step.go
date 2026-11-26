package api

// 20261126114001 complete-step: the owner of an issue, its own architect, reports a step of its
// own: a step whose key is owner, or the retro of a task whose retro is the owner's. A new route,
// `POST /legion/v1/steps/complete` (`grantId`, `issue`, `step`, `summary`, and, as a completion of
// that place carries them, `verdict`, `commit`, `handoffId`, `handoffVersion`, `returnTo`,
// `planRating` and `planReason`), the architect's `complete_step`, takes it; `step` names the step
// by its place in the task, 1 for its first, and 0 for the retro. It answers the empty object every
// fact route answers, so the entry adds the route to that answer's row (answerRoutes). It refuses
// a grant of any claim but the issue's own architect `403 NOT_THE_OWNER`, a step that does not run
// now or whose agent is not the owner `409 OWNER_STEP_NOT_RUNNING`, a step below 0 `400
// INVALID_STEP`, and otherwise as the completion route refuses a report of that place. The
// owner's failed acceptance that names no step to return to, with no step of the type
// implementation before it, stops the task as any such failed step does.
// No plugin below this entry calls the route. A reviewer of an issue with no pull request record
// reports its step on `POST /legion/v1/handoff/complete` with the verdict `pass` or `fail` and a
// commit (`400 STEP_VERDICT_REQUIRED`, `400 MISSING_FIELD`), as the owner's review step does on the
// new route, since no review on GitHub decides either; `VERDICT_ROLE_FORBIDDEN` now reads `only a
// tester, a role of the deployment or of the plan, the role acceptance, a reviewer of an issue with
// no pull request, and the owner in a review step report a verdict`. `POST /legion/v1/signoff`
// refuses a sign-off of an acceptance that is not its owner's own step, a step `acceptance` with no
// key, whose fresh agent ends it with its own verdict, `409 SIGNOFF_NOT_THE_OWNERS`. Each issue of
// the state, and the record route's issue, gains `placeRole`, the role of the claim that works its
// task's place (record.Issue.PlaceRole): the architect at a step or a retro the owner does, and
// otherwise the step's own role (`acceptance` for the fresh agent of a step `acceptance` with no
// key) or, at the retro, the role its agent runs as; it is absent on an issue with no plan rows and
// at the merge. So a reader such as `legion state` names who works an issue at accepting, which the
// phase alone names as the architect. A caller below this entry is served each issue without it
// (the entry's one band, shaping.go's withoutPlaceRole): a plugin of an older release reads an
// issue as a strict object. The entry's request records hold the new route's request, and no
// recorded request field leaves. A claim of the role `acceptance`, the fresh agent of a step
// `acceptance` with no key, is a role outside Legion's six, which the deployment roles' entry's
// band already leaves out of the state served a caller below that entry. The two notice kinds this
// entry adds
// (`owner-step`, and `merge-forward-lost` for an ask or a review of a merge forward that did not
// reach its agent) ride inside a notice row's payload, as the earlier notice kinds do, and are no
// key or value of an answer a plugin parses.
//
// completeStepContract is the entry's id. A plugin below it never calls the route, and is served
// each issue without `placeRole` (withoutPlaceRole).
const completeStepContract = 20261126114001

func init() {
	registerEntry(contractEntry{
		id: completeStepContract, slug: "complete-step",
		bands: []stateNarrowing{
			{contract: completeStepContract, issue: withoutPlaceRole, dropped: "the issues' placeRole"},
		},
		answerRoutes: []answerRoutes{{answer: AnswerEmptyObject, routes: []string{"POST /legion/v1/steps/complete"}}},
	})
}
