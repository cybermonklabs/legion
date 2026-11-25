package api

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// 20261125162422 project-release: dispatch://INFRA-3583 -- the project release.
// `read-unpushed` (shimwire.ReadUnpushed: `id`, `issue`, `merged`, `sessions`, `timeoutMs`), with
// `read-unpushed-result` (shimwire.ReadUnpushedResult: `id`, `ok`, `commits`, `workingCopyChanged`,
// `error`): how many commits of the issue's work the issue's workspace holds that no remote branch
// or tag holds, and whether the reading role's working copy holds a change, read in the workspace
// with no git run and no commit sent. Of the commits no remote branch or tag holds, less the
// root and each with no change and no description, the issue's work is each under a working copy or
// a local bookmark, each with a change and no description, and, while `merged` (whether the record
// holds the issue's pull request merged) is false, each whose description has a line `Omp-Session:
// <one of sessions>` or `Dispatch: <issue>`; `sessions` is the `session` and `file-id` names the
// issue's claims hold in claim_sessions that meet the trailer's rule (claim.SessionIDRule), at most
// shimwire.MaxReadUnpushedSessions of them, and the daemon reads no workspace whose issue's claims
// hold more. A shim older than this entry forwards the frame to Oh My Pi, which refuses it, and
// the daemon reads that refusal as a workspace unread. The operator's routes gain `POST
// /legion/v1/operator/project/release` (body `bound`, a Go duration, and `dryRun`; 202 with the
// release's record, 200 with a dry run's report, and 200 with this process's release once one ran,
// which starts nothing; 409 `PROJECT_RELEASE_NO_POD` on the tmux runtime) and `GET` on the same
// path (200 with the newest release, 404 when none ran), whose body is api.ProjectReleaseResponse:
// `id`, `dryRun`, `startedAt`, `boundAt`, `bound`, `finishedAt`, `endedAt`, `endedBy` and `result`,
// the result one line per Sandbox of the project (`name`, `kind`, `issue`, `outcome`, `reason`,
// `commits`, `workingCopyChanged`, `refusal`, `otherImage`, `asked` with each asked role's `role`,
// `answeredAt` and `bound`) and one per claim the release stopped (`token`, `issue`, `role`,
// `session`, `ended`, `bootStarts`, `detail`), and `failure`. Those two routes answer the `legion`
// command, which is built from the daemon's own commit, so their bodies are no row of answers.go's
// Answers, as the operator's pause and resume are none, and the request body is a row of the routes
// no plugin sends a body to. `outcome`, `endedBy` and `ended` are closed sets of words
// (api.ReleaseOutcomes, store.ProjectReleaseEndedByBoot and api.ReleasedClaimEnds), each a `z.enum`
// in `packages/contracts/src/legion-api.ts`, which refuses a word it does not name, so a new word
// needs its own entry. `GET /legion/v1/state` gains `projectRelease`, that same body for the
// project's newest release, absent while none was recorded. The daemon omits it for a caller
// whose contract is below this entry's number, including a caller placed by a pod's claims
// below shapedAnswersContract (podContracts). The band (withoutProjectRelease) composes with
// the older bands: a caller at or above repositoriesLiveContract but below this entry is served
// the `live` repositories block and no `projectRelease`. The release's six enumerated fields
// on the state are `projectRelease.endedBy`, `projectRelease.result.sandboxes.*.kind`,
// `….outcome`, `….asked.*.role`, `projectRelease.result.claims.*.role` and `….ended`. Each value
// arrives with its key in this entry, so its band holds back all six fields without a separate
// value strip. A claim's `suspendedBy` on the operator routes takes `project_release`, a claim
// the release stopped, which the daemon's next boot starts again. The release's ask to a role
// rides the role's own topic as an
// update round's ask does, under the round `release:<id>`, and `POST
// /legion/v1/claims/ready-for-update` hands an answer naming the running release's round to the
// release, which records it on its own record and none of the claim's round columns. While a
// release holds every start, the operator's spawn, retry, resume and deliver, and either door's
// fresh session and compaction, answer 409 with the code `PROJECT_RELEASED` and the sentence
// api.ProjectReleaseHeldSentence, the Failure body alone. The release and 178's project pause
// refuse each other: while the project is paused, or a pod a pause stopped is still held after the
// resume, `POST /legion/v1/operator/project/release` to a daemon process that has run no release, a
// dry run's included, answers 409 `PROJECT_PAUSED` with api.PauseFailure, which gains `heldPods`,
// each such pod's `issue`, `pause`, `heldAt` and `claims` (api.PauseHeldPodView), beside the
// `pause` that stands where one does, and records, stops and deletes nothing; the refusal's row of
// shapeAnswer serves a caller below this entry's number a PauseFailure without `heldPods`. A
// process that has run one answers it, 200, whatever the pause, as above. While a release
// runs (its record neither finished nor ended by a boot, store.ProjectRelease.Runs), both doors'
// pause answers 409
// `PROJECT_RELEASING`, the Failure body alone, and pause nothing. This entry's fixture records
// the key paths of `projectRelease` on the state and `heldPods` on the refusal, and the domains
// and registrations of the six enumerated fields. The numbered records are unchanged.
//
// Before #5399, this change took 209 at the forward merge that brought in 3c59c0cc, the lowest
// number above main's 202 that no other open pull request declared then: main reached 202 with
// #4493 (dispatch://INFRA-4401, its entry above), and a reading of `const DaemonAPIVersion` at the
// head of every open pull request that changes this file, made between 2026-11-21T05:39:55Z and
// 05:42:59Z, found 199 declared by nineteen open pull requests, 200 by this one alone, 202 by
// fifteen, 203 by #2230, #4065, #4175, #4553, #4596 and #4661, 204 by #3831, 205 by #3712 and
// #4573, 206 by #4540, 207 by #3993, #4030, #4392, #4432, #4590 and #4592, and 208 by #4611, and
// none declaring 209. Main reached 205 with #4573 (dispatch://INFRA-3178, its entry above). This
// change took 150, 157, 159, 161, 163, 171, 180, 191 and 200 before, from the forward merge of
// 2026-11-15 on, each the lowest number above main's that no other open pull request declared at
// its own reading, and each left at a forward merge whose base had reached or passed it, or at
// whose reading another open pull request declared it too. It kept 209 at the merges that brought
// in main's 205: the reading of every open head made between 2026-11-22T00:20:54Z and 00:32:41Z
// found no other open pull request declaring 209. The forward merge of 2026-11-22 that brought in
// 83b91235 brought main's 213 (INFRA-4036's entry above), and a reading of every open pull
// request's head, made between 2026-11-22T11:21:23Z and 11:26:08Z, found 214 declared by #4592, 215
// by #4065 and #4596, 216 by #4173, #4180, #4182, #4193, #4259, #4932 and #4984, 217 by #4273,
// #4590 and #4771, 218 by #3831, 219 by #4611, 220 by #4930, 223 by #4805 and 224 by #4960, and no
// other declaring 221, so this entry moved to 221, the lowest number above main's 213 that no other
// open pull request declared. The forward merge of 2026-11-22 that brought in 5c418da8 left main at
// 213, and a reading of every open head made between 2026-11-22T15:54:21Z and 15:58:32Z found
// #3993, #4065, #4392, #4432 and #4771 declaring 221 as well, so this entry kept 221: whichever of
// these merges after another takes the next free number at its next forward merge. The forward
// merge of 2026-11-22 that brought in e7381b7d left main at 213, and a reading of every open head
// made between 2026-11-22T21:10:50Z and 21:17:52Z found #3993, #4392, #4432 and #4771 declaring 221
// as well, so this entry kept 221 again. The forward merges of 2026-11-23 that brought in 958ff939
// and then 8f332682 left main at 213 and then at 217 (#4273's entry), and a reading of every open
// head made between 2026-11-23T01:01:55Z and 01:06:00Z found #3993, #4392, #4432, #4771 and #4940
// declaring 221 as well, so this entry kept 221 again. The forward merges that brought in e96f1420,
// cdf532a8, 55d89c8b and then 7c880c15 left main at 217, and a reading of every open head made
// between 2026-11-23T10:44:20Z and 10:46:02Z found #4392, #4432, #4940 and #5164 declaring 221 as
// well, so this entry kept 221 again. The forward merge of 2026-11-23 that brought in 8c63a4c5 left
// main at 219 (#4611's, the entry right above), and a reading of every open head made between
// 2026-11-23T17:28:40Z and 17:32:24Z found #4392, #4432, #4940 and #5164 declaring 221 as well, so
// this entry kept 221 again. The forward merges that brought in 7cb1f229 and 4a3eb021 left main at
// 219, and the one that brought in 7df64124 left main at 220 (#4930's, the entry right above); a
// reading of every open head made between 2026-11-23T23:27:56Z and 23:31:09Z found 220 declared by
// #5014 and #5184, and #4392, #4432, #4940 and #5164 declaring 221 as well, and none declaring 221
// that had not before, so this entry kept 221 again. The forward merges that brought in 8b51e3f3,
// 016962aa and 94b086c6 left main at 220, and the forward merge of 2026-11-24 that brought in
// 843a9f45 brought main's 228 (#4590, INFRA-1575's entry above); a reading of every open pull
// request's head, made between 2026-11-24T10:32:04Z and 10:35:59Z, found 229 (#2230), 230 (#4580),
// 231 (#4540), 232 (#4432 and #5271), 233 (#4173, #4180, #4182, #4193 and #4932), 234 (#4065), 235
// (#5204), 236 (#4984), 237 (#5168 and #5169), 238 (#5285), 239 (#4592), 241 (#4771 and #5307), 242
// (#4596), 243 (#5349) and 244 (#3831) declared, and none declaring 240, so this entry moved to
// 240, the lowest number above main's 228 that no other open pull request declares. The forward
// merge of 2026-11-24 that brought in a29bdd07 brought main's 232 (#5271, dispatch://INFRA-4508's
// entry above), and a reading of every open pull request's head, made between 2026-11-24T12:22:29Z
// and 12:27:07Z, found each of 233 to 240 declared by another pull request (240 by #3712, #3993 and
// #4771) and 241 by none, so this entry moved to 241, the lowest number above main's 232 that no
// other open pull request declares. The forward merge of 2026-11-24 that brought in 7e75126f
// brought main's 246 (#4960, the pod hold's entry above), and a reading of every open pull
// request's head, made between 2026-11-24T15:41:05Z and 15:46:25Z, found 247 declared by #4580 and
// #5384 and 248 by none, so this entry moved to 248, the lowest number above main's 246 that no
// other open pull request declares. The forward merge of 2026-11-24 that brought in 5df00e2f
// brought main's 250 (#5365, the waiting line's priority, the entry above), and a reading of every
// open pull request's head, made between 2026-11-24T18:01:44Z and 18:05:16Z, found 251 declared by
// #5307, 252 by #4540 and 253 by none, so this entry moved to 253, the lowest number above main's
// 250 that no other open pull request declares. The forward merge of 2026-11-24 that brought in
// 6c61165e brought main's 256 (#5349, the claim views' cause times, the entry above), and a reading
// of every open pull request's head, made between 2026-11-24T19:43:55Z and 19:46:33Z, found 253
// declared by #4065 and #4596 as well, 254 by #4432, 255 by #5164, 256 by #5180 and #5230, 257 by
// #4580 and 258 by none. This entry took 258 provisionally and was not pushed at that number.
// #4432 and #5164 then declared 258. Before #5399 merged, dispatch://INFRA-3118 allocated 272
// to this release. The forward merge of 2026-11-25 that brought in ac1c9a33 brought main's 275
// (#4392, dispatch://INFRA-4425), above 272, and 295 was the provisional allocation before this
// change was converted to an entry.
//
// projectReleaseContract is this entry's id. The registry computes its number from its rank
// in the build's order (entryNumber).
const projectReleaseContract = 20261125162422

func init() {
	registerEntry(contractEntry{
		id: projectReleaseContract, slug: "project-release",
		bands: []stateNarrowing{
			{contract: projectReleaseContract, state: withoutProjectRelease, dropped: "projectRelease"},
		},
		enumeratedFields: []EnumeratedField{
			// The project's newest project release (api.ProjectReleaseResponse): who ended it, and on
			// each line of its result a Sandbox's `kind` and `outcome` and each asked role's `role`, and a
			// stopped claim's `role` and `ended`. All six arrived with `projectRelease`, which is served
			// below this entry's number without that entire object (withoutProjectRelease), so no
			// such body carries any of those values. A claim line's role takes `controller` beside the claim
			// roles, since the release stops the controller's claim too; an asked role never does, since
			// the release asks the roles of an issue pod alone.
			{Answer: AnswerState, KeyPath: "projectRelease.endedBy", Values: releaseEnders},
			{Answer: AnswerState, KeyPath: "projectRelease.result.sandboxes.*.kind", Values: releasedSandboxKinds},
			{Answer: AnswerState, KeyPath: "projectRelease.result.sandboxes.*.outcome", Values: releaseOutcomes},
			{Answer: AnswerState, KeyPath: "projectRelease.result.sandboxes.*.asked.*.role", Values: releaseAskRoles},
			{Answer: AnswerState, KeyPath: "projectRelease.result.claims.*.role", Values: releasedClaimRoles},
			{Answer: AnswerState, KeyPath: "projectRelease.result.claims.*.ended", Values: releaseClaimEnds},
		},
	})
}

// releaseEnders is every word of `projectRelease.endedBy` (store.ProjectReleaseEnder): the
// daemon's next boot, and nothing else. releaseOutcomes is every outcome of a Sandbox's line
// (api.ReleaseOutcome), and releasedSandboxKinds every kind of Sandbox a line names
// (runtime.ProjectSandboxKind). releaseClaimEnds is every word for how a claim of the stop set
// ended (api.ReleasedClaimEnd): each claim state but `suspended`, each suspension source, and
// `unrecorded`. All four record the entry id, which the registry resolves to the contract
// `projectRelease` arrived at. Each names its constants rather than reading api.ReleaseOutcomes or
// api.ReleasedClaimEnds, as failureCauses names its causes, because a word added to one of those
// later arrives at its own contract and not at this one; TestProjectReleaseWordsGolden holds the
// rosters themselves to the plugin's enums.
var releaseEnders = valuesFrom(projectReleaseContract, store.ProjectReleaseEndedByBoot)

var releasedSandboxKinds = valuesFrom(projectReleaseContract,
	runtime.ProjectSandboxIssue, runtime.ProjectSandboxController, runtime.ProjectSandboxCache,
	runtime.ProjectSandboxProbe, runtime.ProjectSandboxUnknown)

var releaseOutcomes = valuesFrom(projectReleaseContract,
	ReleaseWaiting, ReleaseDeletedClean, ReleaseDeletedUnread, ReleaseDeletedLost, ReleaseDeletedReadRefused,
	ReleaseDeleted, ReleaseLeft)

// releaseAskRoles is the same list of roles at the contract `projectRelease` arrived at, which
// the role of each asked role of a release's Sandbox line takes (api.ReleasedAsk.Role): no
// `projectRelease` is served below that contract at all (shaping.go's withoutProjectRelease).
var releaseAskRoles = valuesFrom(projectReleaseContract, floorRoles...)

// releasedClaimRoles is the role of each claim a release stopped (api.ReleasedClaim.Role): the
// same list of roles with `controller` beside them, since the release stops the project's
// controller too, recorded at the contract `projectRelease` arrived at, as releaseAskRoles is.
var releasedClaimRoles = valuesFrom(projectReleaseContract, append(slices.Clone(floorRoles), claim.RoleController)...)

var releaseClaimEnds = valuesFrom(projectReleaseContract,
	ReleasedClaimEnd(supervise.StateQueued), ReleasedClaimEnd(supervise.StateLaunchUncertain),
	ReleasedClaimEnd(supervise.StateLaunching), ReleasedClaimEnd(supervise.StateShimConnected),
	ReleasedClaimEnd(supervise.StateRegistered), ReleasedClaimEnd(supervise.StateReady),
	ReleasedClaimEnd(supervise.StateWorking), ReleasedClaimEnd(supervise.StateIdle),
	ReleasedClaimEnd(supervise.StateFailed), ReleasedClaimEnd(supervise.StateRetired),
	ReleasedClaimEnd(supervise.SuspendedByUnknown), ReleasedClaimEnd(supervise.SuspendedByOperator),
	ReleasedClaimEnd(supervise.SuspendedByDaemon), ReleasedClaimEnd(supervise.SuspendedByExit),
	ReleasedClaimEnd(supervise.SuspendedByReleased), ReleasedClaimEnd(supervise.SuspendedByProjectRelease),
	ReleasedClaimUnrecorded)
