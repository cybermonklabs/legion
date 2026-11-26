package api

// 20261126055536 start-wait-token: each start of an issue's `startWaitsForPod`, in
// `GET /legion/v1/state` and in `GET /legion/v1/issues/{key}/record`, is an object of its role and
// the token of the claim it starts, `{role, token}` (api.StartWaitView), where the list held the
// role of each start, each role once. Two claims of one issue can hold one role
// (record.WorkerClaim), so a role named the start of a second agent of the role, such as
// `implementer-2`, whose slot the pod lacks, as a start of the role's own agent, and the two
// starts as one. A start row that names no claim is named by the claim the record gives its
// role's step, the claim the outbox's next try of it acts on. A caller whose contract is below
// this entry's number is served the list as before, the role of each start, each role once (the
// entry's one band, shaping.go's withoutStartWaitTokens): a plugin below it reads the list as
// strings and would refuse the whole state over an object. A caller below the deployment roles'
// entry is served no list, as before; that entry's band leaves the key off by a flag on the issue
// (Issue.startWaitsHidden) rather than clearing the list, so both bands find it and each writes
// its line. The sentence of an issue's `why` at `claim_stopped` names its stranded worker as
// `legion state` names a claim (api.ClaimNameInIssue): `its implementer is stranded at failed`
// for the role's own claim, and the token beside the role, `its implementer (claim <token>) is
// stranded at failed`, for any other claim of the role. No plugin parses that sentence, and its
// key is unchanged, so every contract is served it.
//
// startWaitTokenContract is the entry's id. A plugin below it reads the list as strings and
// refuses the whole body for an entry that is an object, so a caller below it is served the role
// of each start, each role once, as before (withoutStartWaitTokens).
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
