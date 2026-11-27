package api

// 20261127094305 claim-start: dispatch://INFRA-5649 -- the operator's and the controller's claim routes answer what the
// start they ask for did, and no answer's shape moves. First, a fresh session (`POST
// /legion/v1/claims/fresh-session`, and `POST /legion/v1/operator/claims/{token}/retry` with
// `freshSession`) or a plain retry of a failed or retired phase worker whose phase's start the
// workflow queues behind its issue's admission slot — the issue holds no slot while its phase
// keeps one — names that wait in `phaseNotRestarted`: the claim stays failed or retired, with its
// session left behind where the request asked, and its start waits for the issue's slot, with the
// issues the issue waits on, where there are any. For a suspended claim, which runs again at once,
// the sentence says the claim is back in a new session and its phase's task waits for the slot.
// Where the issue holds its slot, no sentence is written and the start is claimable at once.
// Second, once a fresh session has left the claim's session behind, a restart the workflow
// refuses, the machine's own refusal of the claim's retry, and its refusal of the resume of a
// claim that was suspended before the request, answer 200 at both routes with the refusal in
// `phaseNotRestarted` and the claim failed, retired or suspended with no session, where each
// answered the refusal as an error (409 or 500): the forget changed the claim. Third, every resume
// (`POST /legion/v1/operator/claims/{token}/resume`), an operator's deliver that revives a
// suspended claim (`POST /legion/v1/operator/claims/{token}/deliver`), a retry the machine runs
// itself (an architect, or a role that does not work its issue's current phase) and a fresh
// session wherever it takes any of these paths answer once the machine has written the claim
// `launching`, while its start goes on under a context the answer does not cancel: the
// operator's routes, the deliver included, answer 202 with the claim launching, the controller's
// route 200, and `read_state` and `legion claims list` show how the start ended. A request that
// writes no launch, such as one whose launch the wake pacing parks, is answered as before. A
// caller whose contract (the one it states, else its pod's lowest) is below 266 is answered the
// first and the second as before: no sentence for a start that waits for a slot, and the refusal
// itself (answersAsBefore, which follows restore as processReadContract's decision does). The
// third is served to every caller, since its shape does not move and holding it back would keep
// the caller waiting past its own timeout. The sentences are values of `phaseNotRestarted`, a
// string no reader enumerates, so the records of 266 are those of 256 at a new number.
// (This entry takes 266, the number INFRA-3118's architect, which allocates its tree's contract
// numbers, gave it at 2026-11-24T17:09:45Z. It had taken 248, which a reading of every open head
// between 2026-11-24T14:05:55Z and 14:09:16Z found free, and #3993, #4049, #4065 and #4392 declared
// 248 since. A reading of `const DaemonAPIVersion` at the head commit of each open pull request
// whose file list names this file or was cut short, made between 2026-11-24T17:29:02Z and
// 17:32:37Z, listed 483 open pull requests, read 142 heads, and found the highest number declared
// to be 261 (#5332), with none at 266; #5399 declares no number. The forward merge of 2026-11-24
// that brought in 5fc8b422 brought main's 246 (#4960, the pod hold's entry above), and the one
// that brought in 5df00e2f brought main's 250 (#5365, the waiting line's priority's entry above),
// and the one that brought in b530b5a220 brought main's 256 (#5349, the claim views' cause
// times' entry above), all below 266, so this entry kept 266. From this entry the last number of
// answers.go's SupportedContracts is `api.claimStartContract`, after `causeTimesContract`. Read main's own
// number and every open head again at the next merge rather than from this line.)
//
// claimStartContract is the daemon contract at which the operator's and the controller's claim
// routes name a start that waits for its issue's admission slot, answer a start refused after a
// fresh session's forget with 200, and answer a retry or a resume once its start is under way
// (version.go's 266, dispatch://INFRA-5649). It moved no key path of any answer. It changed what
// two answers mean, and a caller below it is served each as before (answersAsBefore): no
// `phaseNotRestarted` for a start that waits for a slot (waitNamed), and the refusal itself, as
// the error it was, for a start refused after the forget (refusedAfterTheForget), because its
// `legion` tool description reads an empty answer as a start that ran. The earlier answer of a
// start under way is served to every caller: its shape is the one it was, and holding it back
// would keep the caller waiting past its own timeout.
const claimStartContract = 20261127094305

func init() {
	registerEntry(contractEntry{id: claimStartContract, slug: "claim-start"})
}
