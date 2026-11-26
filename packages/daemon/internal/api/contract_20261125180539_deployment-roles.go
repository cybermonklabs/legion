package api

// 20261125180539 deployment-roles: the roles a deployment defines run steps of an issue's task. An issue whose running step is
// of such a role stands at a new phase, `step`, of `GET /legion/v1/state`'s `phase`, `heldFrom` and
// a claim view's `pendingTask.phase`, and names that role in a new key, `issues.<key>.stepRole`; a
// stranded claim and a waiting wake may name such a role. An issue names, in a new key
// `issues.<key>.startWaitsForPod`, the roles whose start waits because its pod was built before the
// deployment defined them, absent where none waits. A caller whose contract is below 258 is served
// an issue at step as at work, at `implementing`, its hold from step as one from `implementing`, a
// pending task at step as one at `implementing`, no `stepRole`, no `startWaitsForPod` (a plugin
// before memoryContract reads an issue as a strict object and would refuse the whole state over
// it), and no stranded claim or waiting wake of a role outside Legion's six (one band of
// shaping.go at `deploymentRolesContract`), and the daemon logs one line for each claim it leaves
// out (`leftAClaimOut`); the issue stays in `issues` and in `admission.active`. Such a role's agent
// completes its step with `POST /legion/v1/handoff/complete` and the verdict `pass` or `fail` and
// a commit, and no handoff record (`400 STEP_VERDICT_REQUIRED`); `VERDICT_ROLE_FORBIDDEN` now
// reads `only a tester or a role of the deployment reports a verdict`. `POST
// /legion/v1/phase/backward` takes such a role's grant, and one optional field, `returnTo`, the
// place of the step the move returns to (below 1 `400 INVALID_RETURN_TO`; a place the workflow may
// not name, or a `to` that is not the phase that step runs as, `409 BACKWARD_REFUSED`). `readBody`
// refuses an unknown field, so a plugin sending `returnTo` to a daemon below 258 would have the
// move refused; the bump is why the two never meet (`deploymentRolesContract`). `fresh_session`
// and `compact` name the roles `architect, planner, implementer, tester, reviewer, merger, or a role
// of this deployment`. From this entry the last number of answers.go's SupportedContracts is
// `api.deploymentRolesContract`, after `api.causeTimesContract` (256).
// (This entry took 255, the lowest number above main's 232 and above every number an open pull
// request declared, and outside 246 to 250, which another change holds: a reading of `const
// DaemonAPIVersion` at the head of each of the 480 open pull requests, made between
// 2026-11-24T13:09:22Z and 13:11:08Z, read all 480 and found 254 the highest declared, and 229 to
// 240, 242 to 246, 250 and 251 among the numbers below it. The entry first took 221, the lowest
// free number at a reading of 2026-11-21, then 243 at the forward merge of main on 2026-11-24 that
// brought main's 220; by the forward merge that brought main's 228 another open pull request
// declared 243 and others declared numbers up to 251, so this entry moved to 252; by the forward
// merge that brought main's 232 another open pull request declared 254, so this entry moved to 255.
// The forward merge that brought main's 246 (the pod hold's entry) kept 255: a reading of the 477
// open pull requests, made between 2026-11-24T15:59:22Z and 16:01:21Z, read all 477 and found 255
// declared by this change alone. The forward merge that brought main's 250 and 256 (the entries
// above this one) moved this entry to 258, the lowest number above main's 256 that no open pull
// request declared: a reading of `const DaemonAPIVersion` at the head of each of the 486 open pull
// requests, made between 2026-11-24T19:07:20Z and 19:10:45Z, read 485 of them and found 255 (this
// change at its earlier number), 256 (#5428), 257 (#4580), 261 (#5332), 262 (#5204) and 265
// (#5285) at 255 or above. Read main's own number and every open pull request again at the next
// merge rather than from this line.)
//
// deploymentRolesContract is the daemon contract that added the roles a deployment defines to the
// state (version.go's 258): the phase step, each issue's `stepRole` and `startWaitsForPod`, and
// the stranded claims, waiting wakes and pause stops of a role outside Legion's six. A plugin
// below it reads an issue's `phase`, its `heldFrom` and a pending task's `phase` with an
// enumeration that names no step, and the `role` of a stranded claim, of a waiting wake and of a
// pause stop with an enumeration of the six, and refuses the whole body for another value. So a
// caller below it is served the state without them (withoutDeploymentRoles,
// withoutDeploymentRoleClaims), and the daemon logs each claim it left out (leftDeploymentRoles).
//
// It is one band, which follows strip, though it takes values as well as keys, where a value
// strip follows restore (fieldArrivals.heldBack). The two numbers are one below this contract:
// a caller that states its contract is served at that number on both sides, and a pod's reckoning
// counts only claims below shapedAnswersContract (podContracts), so every caller whose strip is
// below this contract has its restore below it too, and no reader that refuses step or a role of
// the deployment is served one.
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes moves
// that constant, and the contract these keys and values arrived at stays where it is. It moves
// only with this change's own number, and answers.go's SupportedContracts names the constant
// rather than its number for that reason.
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
	})
}
