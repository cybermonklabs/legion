package main

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/ghroute"
)

// ghCheckedOnlyVisibility is the call log's visibility for a call the shared check alone read
// (leakguard.GhReadCheckedOnly): a call that writes nothing, under the check the rule was
// proven against, for which nothing read its repository's visibility. It is neither `github` nor
// `kept`, and not the empty value of a call the guard read no visibility for because it named no
// repository.
const ghCheckedOnlyVisibility = "checked-only"

// ghNoRulesVisibility is the call log's visibility for a call that skipped the private-name step
// because the deployment names no rule file (leakguard.GhReadNoRules): the shared check read it,
// where one runs, and nothing read its repository's visibility, since without a rule there is
// nothing the visibility would decide. It is neither checked-only, which is a read the shared
// check alone clears whatever rules are named, nor the empty value of a call that named no
// repository.
const ghNoRulesVisibility = "no-rules"

// ghProvenCheckSHA256 is the sha256 runGh names to the guard as the shared check its read rule
// (ghroute.Target.WritesNothing) was proven against (leakguard.GhRequest.ProvenCheck): the
// pinned check's, ghroute.ReadsProvenCheckSHA256, which this package's test binary, run as
// `legion`, replaces with its stand-in check's (TestMain, testProvenCheckEnv).
var ghProvenCheckSHA256 = ghroute.ReadsProvenCheckSHA256

// leakguardCurl is the curl the leak guard's curl reads run in this binary, the one every guard
// it builds is handed (leakguard.Options.Curl): `legion gh`'s, that command's answer to the shared
// check's own visibility read (answerVisibilityRead), `legion leak-check`'s, and the credential
// helper's fork read (readForkUpstream). It is empty in the binary, where the guard then runs
// /usr/bin/curl; only tests set it, as they set ghProvenCheckSHA256 (TestMain).
var leakguardCurl string

// ghRateLimitRead is the exact `gh api rate_limit`, a GitHub API call with no repository content,
// which reaches gh before its command is read: it reads no help and no git remote, so a working
// directory whose remotes name two owners does not refuse it. Any option changes the request's
// method or shape and is read as every other call is.
func ghRateLimitRead(argv []string) bool {
	return slices.Equal(argv, []string{"api", "rate_limit"})
}
