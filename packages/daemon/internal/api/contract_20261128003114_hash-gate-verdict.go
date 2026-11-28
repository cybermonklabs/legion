package api

// 20261128003114 hash-gate-verdict: dispatch://INFRA-6065 -- the daemon's design gate follows
// Dispatch's own verdict on whether a person's approval of a gated document stands (the hash
// gate), where Dispatch sends one, in place of comparing version numbers, and two fields a reader
// of the state reads change what they mean.
// First, `designGate` and each entry of `gatedSteps` of `GET /legion/v1/state`, and of the record
// `read_record` reads: a gate can stand open, and a gated step read `approved`, with
// `approvedVersion` below `currentVersion`, where Dispatch reports the person's approval of that
// older version current for the current one (dispatch.Approval.Stands). Against a Dispatch that
// sends no digest verdict the gate opens, as before, only where the two are equal, so a reader
// that compares them to say whether the gate is open reads the step's `status` instead. A step
// reads `approved`, or `open_on_basis`, by the standing the daemon last told its tree
// (record.ToldStanding): while a newer version owes Dispatch's digest the work the gate holds
// waits, and the step keeps the word it was last told until the digest settles it.
// Second, `gatedSteps[].basis.version` of a step open on an ancestor's approval, the
// `basisVersion` that `POST /legion/v1/gates/register` takes for it, and the `version` of a
// basis in the architect's `catch-up` notice: each is the ancestor's latest version whose
// approval by a person Dispatch reports standing, which can be above the version that person
// approved, so none of them names a version a person approved. A registration that names the
// version the person approved below that latest version is refused with
// `BASIS_VERSION_MISMATCH`, and each refusal of a basis that names an approval names the version
// the person approved and, below the latest version, that Dispatch reports it current there.
// No key, route, refusal code or enumerated value is added or moved, so every reader parses what
// it parsed and the entry adds no row to any table: it is an entry because it changes what fields
// both sides read mean (`packages/contracts/src/legion-api.ts` states both).
const hashGateVerdictContract = 20261128003114

func init() {
	registerEntry(contractEntry{id: hashGateVerdictContract, slug: "hash-gate-verdict"})
}
