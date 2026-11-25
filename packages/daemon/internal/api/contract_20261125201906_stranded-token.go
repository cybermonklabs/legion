package api

// 20261125201906 stranded-token: a claim of `GET /legion/v1/state`'s `strandedClaims` names its claim token in a new key,
// `strandedClaims.*.token`. Two claims of one issue can hold one role, a keyed agent's on another
// role's step among them, so the issue and the role no longer name one claim; the boot's log line
// and `legion state` read who suspended each stranded claim by that token (api.Suspenders), so
// each takes the move its own suspender calls for. A caller whose contract is below 298 is served
// each stranded claim without the key (one band of shaping.go at `strandedTokenContract`): an
// earlier plugin reads a stranded claim as a strict object and would refuse the whole state over
// it. A release of `POST /legion/v1/claims/release-records` names the tokens of the claims it
// suspended in a new key, `releases.*.claims` (api.ReleaseRecord), absent on a release that
// suspended none and on one recorded before this release: a release's mark matches the claims
// it suspended by their tokens, so two claims of one role are two claims, and `roles` names each
// role once. A resume of the same answer names the token of the claim its wake brought back in a
// new key, `resumes.*.claim` (api.ResumeRecord), absent on a wake recorded before the claim-resume
// migration: each claim of a role can be woken, and the stranded report reads a wake by that
// claim. A caller below 298 is served each release and each resume without these keys
// (withoutRecordClaims). A claim view of the state and of the issue record names its claim's
// token, `token` (api.ClaimView), and an issue lists each claim neither its `architect` nor its
// `workers` shows, with its role, in `otherClaims` (api.Issue.OtherClaims): `workers` holds one
// entry per role, so the other claim of a role a keyed agent works the step of was absent from
// the state. A caller below 298 is served the issues without either (withoutClaimTokens). A row
// a pause holds (api.PauseHeldView) that names no claim, a merge-queue message or a row the release
// before wrote, names in `claim` the worker on its role's step, the claim its executor reaches,
// where it named the role's own token; the key and its shape are unchanged, so every contract is
// served it. The controller's `POST /legion/v1/claims/fresh-session` and
// `POST /legion/v1/claims/compact` take a claim's token in a new member, `claim`
// (api.ClaimRequest), a claim of the issue that holds the role the body names: the issue and the
// role reach the claim that works the role's step, and the other claim of that role a notice can
// name was out of the controller's reach. A plugin sends it only when it names one, since a daemon
// below 298 refuses an unknown member. `POST /legion/v1/handoff/complete` refuses, at every
// contract, a completion from a claim of the role that does not work the role's step the record
// names, `HANDOFF_NOT_THE_STEPS_WORKER`, and the same report from two claims of one role is two
// completions, so neither is answered `HANDOFF_ALREADY_RECORDED` for the other's. From this entry
// the last number of answers.go's SupportedContracts is `api.strandedTokenContract`, and
// `deploymentRolesContract` stays 297. This entry's 298 stands above the base's 297: it was 259,
// above the base's 258, and moved to 298, the number the repository's allocator gave it, when main
// reached 275 and the base moved to 297. Read main's own number and every open pull request again
// before a push rather than from this history.
//
// strandedTokenContract is the daemon contract that added each stranded claim's token to the
// state (version.go's 298, `strandedClaims.*.token`), and to the release records the tokens of the
// claims a release suspended (`releases.*.claims`) and of the claim a wake brought back
// (`resumes.*.claim`): two claims of one issue can hold one role, so the issue and the role no
// longer name one claim.
//
// It is a literal of its own and never DaemonAPIVersion, for the reason deploymentRolesContract
// gives, and it is the last number of answers.go's SupportedContracts, which names the constant
// rather than its number for that reason.
const strandedTokenContract = 20261125201906

func init() {
	registerEntry(contractEntry{
		id: strandedTokenContract, slug: "stranded-token",
		bands: []stateNarrowing{
			{contract: strandedTokenContract, state: withoutStrandedTokens, issue: withoutClaimTokens,
				dropped: "the stranded claims' token, the claim views' token and the issues' otherClaims"},
		},
		enumeratedFields: []EnumeratedField{
			// An issue's other claims (api.Issue.OtherClaims, contract 298) carry the same claim view, so
			// each of its enumerated fields is declared again under `otherClaims`. The key arrived at 298
			// and a caller below it is served no `otherClaims` at all (shaping.go's withoutClaimTokens), so
			// each list is recorded from that contract (arrivedWith).
			{Answer: AnswerState, KeyPath: "issues.*.otherClaims.*.claim.state", Values: arrivedWith(strandedTokenContract, claimViewStates)},
			{Answer: AnswerState, KeyPath: "issues.*.otherClaims.*.claim.updateState", Values: arrivedWith(strandedTokenContract, updateStates)},
			{Answer: AnswerState, KeyPath: "issues.*.otherClaims.*.claim.failure.cause", Values: arrivedWith(strandedTokenContract, failureCauses)},
			{Answer: AnswerState, KeyPath: "issues.*.otherClaims.*.claim.pendingCause", Values: arrivedWith(strandedTokenContract, failureCauses)},
			{Answer: AnswerState, KeyPath: "issues.*.otherClaims.*.claim.pendingTask.phase", Values: arrivedWith(strandedTokenContract, workflowPhases)},
		},
	})
}
