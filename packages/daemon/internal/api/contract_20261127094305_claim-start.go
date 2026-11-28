package api

// 20261127094305 claim-start: dispatch://INFRA-5649 -- the operator's and the controller's claim routes answer what the
// start they ask for did. First, a fresh session (`POST
// /legion/v1/claims/fresh-session`, and `POST /legion/v1/operator/claims/{token}/retry` with
// `freshSession`) or a plain retry of a failed or retired phase worker whose phase's start the
// workflow queues behind its issue's admission slot — the issue holds no slot while its phase
// keeps one — names that wait in `phaseNotRestarted`: the claim stays failed or retired, with its
// session left behind where the request asked, and its start waits for the issue's slot, with the
// issues the issue waits on, where there are any. For a suspended claim, which runs again at once,
// the sentence says the claim is back in a new session and its phase's task waits for the slot.
// Where the issue holds its slot, no sentence is written and the start is claimable at once. The
// controller's own plain retry (`POST /legion/v1/agents/retry`, the agent door) names the same
// wait in a key its answer gains, `phaseNotRestarted` on `api.AgentActionResponse`, a string
// absent when empty, with the claim's `state` still failed or retired; it is the one key this
// entry adds, and `packages/contracts/src/legion-api.ts` mirrors it as optional.
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
// writes no launch, such as one whose launch the wake pacing delays, is answered as before. A
// caller whose contract (the one it states, else its pod's lowest) is below this entry's number
// (entryNumber(claimStartContract)) is answered the first and the second as before: no sentence
// for a start that waits for a slot, and the refusal itself (answersAsBefore, which follows
// restore as processReadContract's decision does), and the agent actions' answer without
// `phaseNotRestarted` (shapeAnswer's agent actions' row, which follows strip, and placedByPod
// places the caller of an answer that carries the key by its pod). The third is served to every
// caller, since its shape does not move and holding it back would keep the caller waiting past
// its own timeout. The sentences are values of `phaseNotRestarted`, a string no reader enumerates.
// Since #5399 no branch picks a number: this entry's number is its rank in main's merge order.
//
// claimStartContract is the daemon contract at which the operator's and the controller's claim
// routes name a start that waits for its issue's admission slot, answer a start refused after a
// fresh session's forget with 200, and answer a retry or a resume once its start is under way
// (dispatch://INFRA-5649). It added one key path, the agent actions' `phaseNotRestarted`, which a
// caller below it is served without (shapeAnswer). It changed what two answers mean, and a caller
// below it is served each as before (answersAsBefore): no `phaseNotRestarted` for a start that
// waits for a slot (waitNamed), and the refusal itself, as the error it was, for a start refused
// after the forget (refusedAfterTheForget), because its `legion` tool description reads an empty
// answer as a start that ran. The earlier answer of a start under way is served to every caller:
// its shape is the one it was, and holding it back would keep the caller waiting past its own
// timeout.
const claimStartContract = 20261127094305

func init() {
	registerEntry(contractEntry{id: claimStartContract, slug: "claim-start"})
}
