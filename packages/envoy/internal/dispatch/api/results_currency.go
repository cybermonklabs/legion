package api

import "github.com/sjawhar/envoy/internal/dispatch/model"

// results_currency.go answers `currency` on each result of a results read: a compatibility
// field for `dispatch` clients older than pi-envoy 17.1.0. Those clients print a result's
// `currency` beside it, or "unknown" where the read answers none, and read nothing of the
// comparison that replaced it (#3530): `commit_stands`, `check_words_moved` and `retracted`.
// The field is derived from those three alone, after the comparison has answered them, so it
// costs no read of the database or of GitHub and changes none of them.
//
// Once no client older than pi-envoy 17.1.0 is in use, remove this file and
// results_currency_test.go, the answerCompatibilityCurrency call in listCheckResults,
// model.CheckResult.Currency and model.Currency*, and the currency descriptions in
// packages/envoy/AGENTS.md, packages/envoy/cmd/dispatch/README.md and routes_table.go.
// From the repository root, grep for `pi-envoy 17[.]1[.]0|Currency` in packages/envoy and
// packages/contracts to find the compatibility code, tests and documentation together.
// Regenerate packages/contracts/schemas/dispatch-openapi.json with
// `go run ./cmd/dispatch openapi --out ../contracts/schemas/dispatch-openapi.json` from
// packages/envoy, then packages/contracts/src/dispatch-api.generated.ts with
// `bun run gen:api` from packages/contracts. Keep standingCommit: the gate uses it.

// answerCompatibilityCurrency adds only the field older clients print. A read that names a
// commit answers none: no such client makes one, and current there would mean the commit the
// reader named rather than the work as it stands.
func answerCompatibilityCurrency(checks []model.CheckBlock, at string) {
	if at != noCommitNamed {
		return
	}
	for index := range checks {
		for result := range checks[index].Results {
			checks[index].Results[result].Currency = compatibilityCurrency(checks[index].Results[result])
		}
	}
}

// compatibilityCurrency is the word a client older than pi-envoy 17.1.0 prints for one result,
// in that client's own vocabulary, where `current` meant the result speaks for the work as it
// stands and `older` that it was recorded at an earlier commit or against words that have
// changed since. It is "" where neither word is true, which leaves the key out, and that
// client then prints "unknown":
//
//   - at_basis under the check's words now, with no retraction against it, is `current`;
//   - at_basis under words that moved since is `older`;
//   - other_commit is `older`, whether or not the words moved or a retraction stands;
//   - a retracted result at its basis under the words now is neither: it is not current, and
//     it is at no earlier commit, so the old words have nothing true to say of it;
//   - unknown is neither, whether or not the words moved, as the server before #3530 answered
//     `unknown` wherever it could not read where the commit stands.
func compatibilityCurrency(result model.CheckResult) string {
	if result.CommitStands == nil || result.CheckWordsMoved == nil {
		return ""
	}
	switch result.CommitStands.Answer {
	case model.CommitOtherCommit:
		return model.CurrencyOlder
	case model.CommitAtBasis:
		switch {
		case *result.CheckWordsMoved:
			return model.CurrencyOlder
		case standingCommit(result) == model.CommitAtBasis:
			return model.CurrencyCurrent
		}
	}
	return ""
}
