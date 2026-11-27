package api

// 20261125201906 stranded-token: a claim of GET /legion/v1/state's strandedClaims names its claim token in the new key
// strandedClaims.*.token. Two claims of one issue can hold one role, so the issue and role no
// longer name one claim. The boot log and legion state read who suspended each stranded claim
// by that token (api.Suspenders). A caller below 259 is served each claim without its token
// (shaping.go's strandedTokenContract), because an earlier plugin reads a strict object.
// This unreleased entry first took 222; the coordinated allocation places it after the
// deployment roles at 258. This branch carries that allocation without changing its owner's head.
// SupportedContracts names api.strandedTokenContract after api.deploymentRolesContract.
//
// strandedTokenContract is the daemon contract that added each stranded claim's token to the
// state (version.go's 259, `strandedClaims.*.token`): two claims of one issue can hold one role,
// so the issue and the role no longer name one claim.
//
// It is a literal of its own and never DaemonAPIVersion, for the reason deploymentRolesContract
// gives, and answers.go's SupportedContracts names the constant rather than its number for that
// reason.
const strandedTokenContract = 20261125201906

func init() {
	registerEntry(contractEntry{
		id: strandedTokenContract, slug: "stranded-token",
		bands: []stateNarrowing{
			{contract: strandedTokenContract, state: withoutStrandedTokens, dropped: "the stranded claims' token"},
		},
	})
}
