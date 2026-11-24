package githubrest

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// A sha, a word or a login a GitHub answer names prints only in the shape GitHub gives it: a
// commit's forty lowercase hex digits, one of the words GitHub documents for the member, a login
// GitHub serves - older logins ending in a hyphen or holding a double one and a managed user's
// `_<shortcode>` among them - that holds no GitHub token form. Anything else - here the probe,
// words that quote a URL carrying made-up credentials, an installation token echoed back, and a
// token form short enough to pass a login's 39 characters - prints as a fixed phrase. An event id
// names a sha that is not one by a digest: two such answers make two ids, and neither names what
// the answer wrote.
func TestAMemberOfGitHubsAnswerPrintsOnlyInItsShape(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	echoed := githubtest.InstallationToken(t.Name())
	for _, c := range []struct {
		name, got, want string
	}{
		{"a commit's sha", ShownSHA(commit), commit},
		{"no sha", ShownSHA(""), ""},
		{"a sha in upper case", ShownSHA(strings.ToUpper(commit)), NotACommit},
		{"a short sha", ShownSHA(commit[:39]), NotACommit},
		{"an echoed token for a sha", ShownSHA(echoed), NotACommit},
		{"the probe for a sha", ShownSHA(oauth2calltest.Words), NotACommit},
		{"a commit's sha as a key", SHAKey(commit), commit},
		{"a documented word", ShownWord("closed", "open", "closed"), "closed"},
		{"a word in another case", ShownWord("Closed", "open", "closed"), NotDocumented},
		{"the probe for a word", ShownWord(oauth2calltest.Words, "open", "closed"), NotDocumented},
		{"a login", ShownLogin("a-writer"), "a-writer"},
		{"a login of 39 characters", ShownLogin(strings.Repeat("a", 39)), strings.Repeat("a", 39)},
		{"a login of 40 characters", ShownLogin(strings.Repeat("a", 40)), NotALogin},
		{"an App's bot account", ShownLogin("legion-reviewer[bot]"), "legion-reviewer[bot]"},
		{"a login with a double hyphen", ShownLogin("a--writer"), "a--writer"},
		{"a login ending in a hyphen", ShownLogin("writer-"), "writer-"},
		{"a managed user's login", ShownLogin("mona-cat_octo"), "mona-cat_octo"},
		{"a login that opens like a token", ShownLogin("ghs_acme"), "ghs_acme"},
		{"a login opening with a hyphen", ShownLogin("-writer"), NotALogin},
		{"a login opening with an underscore", ShownLogin("_writer"), NotALogin},
		{"an echoed token for a login", ShownLogin(echoed), NotALogin},
		{"a token form within a login's 39 characters", ShownLogin(echoed[:30]), NotALogin},
		{"a token form as an App's bot account", ShownLogin(echoed[:30] + "[bot]"), NotALogin},
		{"a fine-grained token's form within 39 characters", ShownLogin("github_pat_0123456789abcdef"), NotALogin},
		{"the probe for a login", ShownLogin(oauth2calltest.Words), NotALogin},
		{"a branch", ShownBranch("release/v7.1"), "release/v7.1"},
		{"a branch holding URL syntax", ShownBranch("release#stable"), "release#stable"},
		{"an echoed token for a branch", ShownBranch(echoed), NotABranch},
		{"a branch name git refuses", ShownBranch("release..stable"), NotABranch},
		{"a branch with a control character", ShownBranch("main\x1b[31m"), NotABranch},
		{"the probe for a branch", ShownBranch(oauth2calltest.Words), NotABranch},
	} {
		if c.got != c.want {
			t.Errorf("%s: printed %q, want %q", c.name, c.got, c.want)
		}
	}
	key := SHAKey(oauth2calltest.Words)
	for _, secret := range []string{"not-a-secret-user", "not-a-secret-password", "not-a-secret-token", "issuer.example"} {
		if strings.Contains(key, secret) {
			t.Errorf("SHAKey of the probe is %q, which names %q", key, secret)
		}
	}
	if !strings.HasPrefix(key, "not-a-commit-") || key == SHAKey(oauth2calltest.Words+" ") {
		t.Errorf("SHAKey of the probe is %q, want a digest that tells it from another text", key)
	}
}
