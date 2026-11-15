package api

import "github.com/sjawhar/envoy/internal/dispatch/model"

// results_currency.go answers `currency` on each result of a results read: a compatibility
// field for `dispatch` clients older than pi-envoy 17.1.0. Those clients print a result's
// `currency` beside it, or "unknown" where the read answers none, and read nothing of the
// comparison that replaced it (#3530): `commit_stands`, `check_words_moved` and `retracted`.
// The field is derived from those three alone, after the comparison has answered them, so it
// costs no read of the database or of GitHub and changes none of them.
//
// Remove this file, its call in listCheckResults, model.CheckResult.Currency and the two
// model.Currency* words once no client older than pi-envoy 17.1.0 is in use: pi-envoy 17.1.0 is
// the first release that reads `commit_stands`.

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
		case result.Retracted == nil:
			return model.CurrencyCurrent
		}
	}
	return ""
}
