package api

// 20261127213144 branch-refused: an issue's branch row that GitHub, or the
// deployment's private-name rules, refused in a way a retry meets again ends rather than retries
// for ever, and the owning architect is told once for the issue's generation with a
// `branch-refused` notice, which reaches its session as text, as every notice kind does. `GET
// /legion/v1/state` and `GET /legion/v1/issues/{key}/record` serve each issue whose branch row was
// so refused at the generation it stands in with `branchRefused` (`api.BranchRefusedView`): `row`,
// the outbox row's id; `write`, `create` or `move`; `kind`, the refusal's kind (ghbranch's
// RefusalKind: `name-refused`, `forbidden`, `not-found`, `conflict`, `unprocessable`, `no-base`,
// `no-commit`, `no-node-id`, `compare-status`, `graphql-forbidden` or `graphql-not-found`);
// `reason`, the row's error as the row kept it; and `at`, when it was refused. A caller below this
// entry's contract is served every issue without it: this entry's row of `bands`
// (withoutBranchRefused), which follows the contract the caller states, else its pod's highest.
// `kind` is no enumerated field: no plugin code reads it (`LEGION_ANSWER_READS`), so a reader takes
// each member as optional and each word as a string, and a kind a later daemon adds is read rather
// than refused. The record of a refusal is the table `branch_refusals` of migration
// `20261127215416_branch_refusals`, which moves the daemon's own record and is no part of this
// number.
const branchRefusedContract = 20261127213144

func init() {
	registerEntry(contractEntry{
		id: branchRefusedContract, slug: "branch-refused",
		bands: []stateNarrowing{
			{contract: branchRefusedContract, issue: withoutBranchRefused, dropped: "the issues' branchRefused"},
		},
	})
}

// withoutBranchRefused is the issue with no `branchRefused`, the key this entry added to an issue.
// It is an `omitempty` pointer, set only on an issue whose branch row was refused for good, so the
// band reports a move only for an issue that carries it. The issue is a value of its own, so the
// state the source answered is left as it was.
func withoutBranchRefused(issue Issue) (Issue, bool) {
	moved := issue.BranchRefused != nil
	issue.BranchRefused = nil
	return issue, moved
}
