package api

// 20261126055536 start-wait-token: each start of an issue's `startWaitsForPod`, in `GET /legion/v1/state` and in
// `GET /legion/v1/issues/{key}/record`, is an object of its role and the token of the claim it
// starts, `{role, token}` (api.StartWaitView), where the list held the role of each start, each
// role once. Two claims of one issue can hold one role (record.WorkerClaim), so a role named the
// start of a second agent of the role, such as `implementer-2`, whose slot the pod lacks, as a
// start of the role's own agent, and the two starts as one. A start row that names no claim is
// named by the claim the record gives its role's step, the claim the outbox's next try of it acts
// on. A caller whose contract is below 300 is served the list as before, the role of each start,
// each role once (one band of shaping.go at `startWaitTokenContract`, `withoutStartWaitTokens`):
// a plugin below it reads the list as strings and would refuse the whole state over an object. A
// caller below 297 is served no list, as before; the band of 297 now leaves the key off by a flag
// on the issue (Issue.startWaitsHidden) rather than clearing the list, so both bands find it and
// each writes its line. The sentence of an issue's `why` at `claim_stopped` names its stranded
// worker as `legion state` names a claim (api.ClaimNameInIssue): `its implementer is stranded at
// failed` for the role's own claim, and the token beside the role, `its implementer (claim
// <token>) is stranded at failed`, for any other claim of the role. No plugin parses that
// sentence, and its key is unchanged, so every contract is served it. From this entry the last
// number of answers.go's SupportedContracts is `api.startWaitTokenContract`, after 299, and
// `strandedTokenContract` stays 298.
// (This entry took 285, which the contract allocator confirmed free at the forward merge that
// brought the base's 284 into this change, and moved to 300, the number the repository's
// allocator gave it, when main reached 275 and the base moved to 299. It took 245 before, above
// the base's 222 then, at a reading of `const DaemonAPIVersion` at refs/pull/<n>/head for each of
// the 514 open pull requests, made between 2026-11-24T07:08:28Z and 07:09:16Z, which read all 514
// and found 242 the highest number declared; a later reading, of 492 open pull requests between
// 2026-11-24T11:07:24Z and 11:08:45Z, all read, found #4960 at 245 too, so 245 was not this
// entry's alone. Read main's own number and every open pull request again before a push rather
// than from this history.)
//
// startWaitTokenContract is the daemon contract that names each start of an issue's
// `startWaitsForPod` by its role and the token of the claim it starts (version.go's 300), where
// the list held the roles of the starts, each role once: two claims of one issue can hold one
// role, so a role named the start of a second agent of the role, such as `implementer-2`, as a
// start of the role's own agent. A plugin below it reads the list as strings and refuses the
// whole body for an entry that is an object, so a caller below it is served the role of each
// start, each role once, as before (withoutStartWaitTokens).
//
// It is a literal of its own and never DaemonAPIVersion, for the reason deploymentRolesContract
// gives, and it is the last number of answers.go's SupportedContracts, which names the constant
// rather than its number for that reason.
const startWaitTokenContract = 20261126055536

func init() {
	registerEntry(contractEntry{
		id: startWaitTokenContract, slug: "start-wait-token",
		bands: []stateNarrowing{
			{contract: startWaitTokenContract, issue: withoutStartWaitTokens,
				dropped: "the claim tokens of the issues' startWaitsForPod, " +
					"served as the roles of its starts, each once"},
		},
	})
}
