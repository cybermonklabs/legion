package api

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// 20261125162422 project-release: dispatch://INFRA-3583 -- the project release.
// The shim answers a new frame from the daemon, `read-unpushed` (shimwire.ReadUnpushed: `id`,
// `issue`, `merged`, `sessions`, `timeoutMs`), with `read-unpushed-result`
// (shimwire.ReadUnpushedResult: `id`, `ok`, `commits`, `workingCopyChanged`, `error`): how many
// commits of the issue's work the issue's workspace holds that no remote branch or tag holds, and
// whether the reading role's working copy holds a change, read in the workspace with no git run and
// no commit sent. Of the commits no remote branch or tag holds, less the root and each with no
// change and no description, the issue's work is each under a working copy or a local bookmark,
// each with a change and no description, and, while `merged` (whether the record holds the issue's
// pull request merged) is false, each whose description has a line `Omp-Session: <one of sessions>`
// or `Dispatch: <issue>`; `sessions` is the `session` and `file-id` names the issue's claims hold
// in claim_sessions that meet the trailer's rule (claim.SessionIDRule), at most
// shimwire.MaxReadUnpushedSessions of them, and the daemon reads no workspace whose issue's claims
// hold more. A shim older than this entry forwards the frame to Oh My Pi, which refuses it, and the
// daemon reads that refusal as a workspace unread. The operator's routes gain `POST
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
// project's newest release, absent while none was recorded. The daemon omits it for a caller whose
// contract is below this entry's number, including a caller placed by a pod's claims below
// shapedAnswersContract (podContracts). The band (withoutProjectRelease) composes with the older
// bands: a caller at or above repositoriesLiveContract but below this entry is served the `live`
// repositories block and no `projectRelease`. The release's six enumerated fields on the state are
// `projectRelease.endedBy`, `projectRelease.result.sandboxes.*.kind`, `….outcome`,
// `….asked.*.role`, `projectRelease.result.claims.*.role` and `….ended`. Each value arrives
// with its key in this entry, so its band holds back all six fields without a separate value
// strip. A claim's `suspendedBy` on the operator routes takes `project_release`, a claim the
// release stopped, which the daemon's next boot starts again. The release's ask to a role rides
// the role's own topic as an update round's ask does, under the round `release:<id>`, and `POST
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
// process that has run one answers it, 200, whatever the pause, as above. While a release runs (its
// record neither finished nor ended by a boot, store.ProjectRelease.Runs), both doors' pause routes
// answer 409 `PROJECT_RELEASING`, the Failure body alone, and pause nothing. This entry's fixture
// records the key paths of `projectRelease` on the state and `heldPods` on the refusal, and the
// domains and registrations of the six enumerated fields. The numbered records are unchanged.
const projectReleaseContract = 20261125162422

func init() {
	registerEntry(contractEntry{
		id: projectReleaseContract, slug: "project-release",
		bands: []stateNarrowing{
			{contract: projectReleaseContract, state: withoutProjectRelease, dropped: "projectRelease"},
		},
		enumeratedFields: []EnumeratedField{
			// The project's newest project release (api.ProjectReleaseResponse): who ended it, and
			// on each line of its result a Sandbox's `kind` and `outcome` and each asked role's
			// `role`, and a stopped claim's `role` and `ended`. All six arrived with
			// `projectRelease`, which is served below this entry's number without that entire
			// object (withoutProjectRelease), so no such body carries any of those values. A claim
			// line's role takes `controller` beside the claim roles, since the release stops the
			// controller's claim too; an asked role never does, since the release asks the roles of
			// an issue pod alone.
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
var (
	releaseEnders = valuesFrom(projectReleaseContract, store.ProjectReleaseEndedByBoot)

	releaseOutcomes = valuesFrom(projectReleaseContract,
		ReleaseWaiting, ReleaseDeletedClean, ReleaseDeletedUnread, ReleaseDeletedLost, ReleaseDeletedReadRefused,
		ReleaseDeleted, ReleaseLeft)

	releasedSandboxKinds = valuesFrom(projectReleaseContract,
		runtime.ProjectSandboxIssue, runtime.ProjectSandboxController, runtime.ProjectSandboxCache,
		runtime.ProjectSandboxProbe, runtime.ProjectSandboxUnknown)

	releaseClaimEnds = valuesFrom(projectReleaseContract,
		ReleasedClaimEnd(supervise.StateQueued), ReleasedClaimEnd(supervise.StateLaunchUncertain),
		ReleasedClaimEnd(supervise.StateLaunching), ReleasedClaimEnd(supervise.StateShimConnected),
		ReleasedClaimEnd(supervise.StateRegistered), ReleasedClaimEnd(supervise.StateReady),
		ReleasedClaimEnd(supervise.StateWorking), ReleasedClaimEnd(supervise.StateIdle),
		ReleasedClaimEnd(supervise.StateFailed), ReleasedClaimEnd(supervise.StateRetired),
		ReleasedClaimEnd(supervise.SuspendedByUnknown), ReleasedClaimEnd(supervise.SuspendedByOperator),
		ReleasedClaimEnd(supervise.SuspendedByDaemon), ReleasedClaimEnd(supervise.SuspendedByExit),
		ReleasedClaimEnd(supervise.SuspendedByReleased), ReleasedClaimEnd(supervise.SuspendedByProjectRelease),
		ReleasedClaimUnrecorded)
)

// releaseAskRoles is floorRoles at the contract `projectRelease` arrived at, which the role of
// each asked role of a release's Sandbox line takes (api.ReleasedAsk.Role): no `projectRelease`
// is served below that contract at all (shaping.go's withoutProjectRelease).
var releaseAskRoles = valuesFrom(projectReleaseContract, floorRoles...)

// releasedClaimRoles is the role of each claim a release stopped (api.ReleasedClaim.Role):
// floorRoles with `controller` beside them, since the release stops the project's controller
// too, recorded at the contract `projectRelease` arrived at, as releaseAskRoles is.
var releasedClaimRoles = valuesFrom(projectReleaseContract, append(slices.Clone(floorRoles), claim.RoleController)...)
