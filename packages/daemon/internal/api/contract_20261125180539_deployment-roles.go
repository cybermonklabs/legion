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
// `admission.active`. A claim of such a role that registers from a plugin below this entry's
// number is answered `409` with a refusal that names both numbers, in place of its registration.
// Such a role's agent completes its step with `POST
// /legion/v1/handoff/complete` and the verdict `pass` or `fail` and a commit, and no handoff record
// (`400 STEP_VERDICT_REQUIRED`); `VERDICT_ROLE_FORBIDDEN` now reads `only a tester or a role of the
// deployment reports a verdict`. `POST /legion/v1/phase/backward` takes such a role's grant, the
// target `step`, and one optional field, `returnTo`, the place of the step the move returns to
// (below 1 `400 INVALID_RETURN_TO`; a place the workflow may not name, or a `to` that is not the
// phase that step runs as, `409 BACKWARD_REFUSED`). `fresh_session` and `compact` name the roles
// `architect, planner, implementer, tester, reviewer, merger, or a role of this deployment`.
//
// The entry adds `step` to the four enumerated fields that take an issue's phase (its
// enumeratedValues), and the record answer takes it at its own key paths (withValuesAdded). It
// opens the four enumerated fields that take a claim's role (its openedFields): the `role` of a
// stranded claim, of a waiting wake, of a pause stop and of a registration takes any role of the
// role grammar from this entry's number, and Legion's six stay its domain below it (answers.go's
// claimRoles). So this entry's fixture records each of the four as open from the entry's number,
// the record of that number holds no domain of any of them, and the shape gate checks none of
// their values there.
//
// deploymentRolesContract is this entry's id. A plugin below the entry's number reads an issue's
// `phase`, its `heldFrom` and a pending task's `phase` with an enumeration that names no step, and
// the `role` of a stranded claim, of a waiting wake, of a pause stop and of a registration with an
// enumeration of the six, and refuses the whole body for another value. So a caller below it is
// served the state without them (withoutDeploymentRoles, withoutDeploymentRoleClaims), and the
// daemon logs each claim it left out (leftDeploymentRoles). A registration's `role` is the claim
// itself and cannot be left out of its answer, so the registration of such a role's claim from a
// plugin below the entry's number is refused instead (claims.go's register).
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
		openedFields: []openedField{
			{answer: AnswerState, keyPath: "strandedClaims.*.role"},
			{answer: AnswerState, keyPath: "wakesWaiting.*.role"},
			{answer: AnswerState, keyPath: "paused.waitsOn.*.role"},
			{answer: AnswerRegister, keyPath: "role"},
		},
	})
}
