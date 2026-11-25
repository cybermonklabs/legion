package api

// 20261125180539 deployment-roles: the roles a deployment defines run steps of an issue's task. An
// issue whose running step is of such a role stands at a new phase, `step`, of `GET
// /legion/v1/state`'s `phase`, `heldFrom` and a claim view's `pendingTask.phase`, and names that
// role in a new key, `issues.<key>.stepRole`; a stranded claim, a waiting wake and a pause stop may
// name such a role. An issue names, in a new key `issues.<key>.startWaitsForPod`, the roles whose
// start waits because its pod was built before the deployment defined them, absent where none
// waits. A caller whose contract is below this entry's number is served an issue at step as at
// work, at `implementing`, its hold from step as one from `implementing`, a pending task at step as
// one at `implementing`, no `stepRole`, no `startWaitsForPod` (a plugin before memoryContract reads
// an issue as a strict object and would refuse the whole state over it), and no stranded claim,
// waiting wake or pause stop of a role outside Legion's six (the entry's one band), and the daemon
// logs one line for each claim it leaves out (`leftAClaimOut`); the issue stays in `issues` and in
// `admission.active`. Such a role's agent completes its step with `POST
// /legion/v1/handoff/complete` and the verdict `pass` or `fail` and a commit, and no handoff record
// (`400 STEP_VERDICT_REQUIRED`); `VERDICT_ROLE_FORBIDDEN` now reads `only a tester or a role of the
// deployment reports a verdict`. `POST /legion/v1/phase/backward` takes such a role's grant, the
// target `step`, and one optional field, `returnTo`, the place of the step the move returns to
// (below 1 `400 INVALID_RETURN_TO`; a place the workflow may not name, or a `to` that is not the
// phase that step runs as, `409 BACKWARD_REFUSED`). `fresh_session` and `compact` name the roles
// `architect, planner, implementer, tester, reviewer, merger, or a role of this deployment`.
//
// The entry adds `step` to the four enumerated fields that take an issue's phase (its
// enumeratedValues), and the record answer takes it at its own key paths (withValuesAdded). The
// `role` of a stranded claim, a waiting wake, a pause stop and a registration keeps Legion's six as
// its domain (answers.go's claimRoles says why).
//
// deploymentRolesContract is this entry's id. A plugin below the entry's number reads an issue's
// `phase`, its `heldFrom` and a pending task's `phase` with an enumeration that names no step, and
// the `role` of a stranded claim, of a waiting wake and of a pause stop with an enumeration of the
// six, and refuses the whole body for another value. So a caller below it is served the state
// without them (withoutDeploymentRoles, withoutDeploymentRoleClaims), and the daemon logs each
// claim it left out (leftDeploymentRoles).
//
// It is one band, which follows strip, though it takes values as well as keys, where a value strip
// follows restore (fieldArrivals.heldBack). The two numbers are one below the entry's number: a
// caller that states its contract is served at that number on both sides, and a pod's reckoning
// counts only claims below shapedAnswersContract (podContracts), so every caller whose strip is
// below the entry has its restore below it too, and no reader that refuses step or a role of the
// deployment is served one.
const deploymentRolesContract = 20261125180539

func init() {
	registerEntry(contractEntry{
		id: deploymentRolesContract, slug: "deployment-roles",
		bands: []stateNarrowing{
			{contract: deploymentRolesContract, state: withoutDeploymentRoleClaims, issue: withoutDeploymentRoles,
				left: leftDeploymentRoles,
				dropped: "the phase step (served as implementing), the issues' stepRole and startWaitsForPod, and the " +
					"stranded claims, waiting wakes and pause stops of a deployment's roles"},
		},
		enumeratedValues: []enumeratedValues{
			{answer: AnswerState, keyPath: "issues.*.phase", values: valuesFrom(deploymentRolesContract, "step")},
			{answer: AnswerState, keyPath: "issues.*.heldFrom", values: valuesFrom(deploymentRolesContract, "step")},
			{answer: AnswerState, keyPath: "issues.*.architect.pendingTask.phase", values: valuesFrom(deploymentRolesContract, "step")},
			{answer: AnswerState, keyPath: "issues.*.workers.*.claim.pendingTask.phase", values: valuesFrom(deploymentRolesContract, "step")},
		},
	})
}
