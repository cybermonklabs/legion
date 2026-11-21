package githubrest

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// A sha, a word or a login a GitHub answer names prints only in the shape GitHub gives it: a
// commit's forty lowercase hex digits, one of the words GitHub documents for the member, a login
// GitHub allows. Anything else - here the probe, words that quote a URL carrying made-up
// credentials, and an installation token echoed back - prints as a fixed phrase. An event id names
// a sha that is not one by a digest: two such answers make two ids, and neither names what the
// answer wrote.
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
		{"a login with a double hyphen", ShownLogin("a--writer"), NotALogin},
		{"a login ending in a hyphen", ShownLogin("writer-"), NotALogin},
		{"an echoed token for a login", ShownLogin(echoed), NotALogin},
		{"the probe for a login", ShownLogin(oauth2calltest.Words), NotALogin},
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
