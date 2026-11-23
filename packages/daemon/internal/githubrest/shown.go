package githubrest

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// A member of a GitHub answer that the daemon decodes as a plain string and decides on as it came -
// a commit's sha, a state, an account's login - is GitHub's words, or the words of whatever answers
// in GitHub's place, which can echo the request back, its installation token and all. A log line or
// an error names such a member only in the one shape GitHub gives it, and a fixed phrase in place of
// anything else: nothing else the answer wrote there prints, and every decision is still made on the
// member as it came.

// CommitSHA says whether s is a commit as GitHub names one: exactly 40 lowercase hexadecimal
// characters. GitHub spells every commit it reports this way.
func CommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// NotACommit is what ShownSHA prints in place of a sha that is not a commit's.
const NotACommit = "(a sha that is not a commit's)"

// ShownSHA is sha, a commit a GitHub answer names, as a message may print it: the sha where it is a
// commit's (CommitSHA), "" where it is empty, and NotACommit otherwise.
func ShownSHA(sha string) string {
	if sha == "" || CommitSHA(sha) {
		return sha
	}
	return NotACommit
}

// SHAKey is sha, a commit a GitHub answer names, as an event id names it: the sha where it is a
// commit's (CommitSHA) or empty, and otherwise "not-a-commit-" and the hex SHA-256 of what the
// answer wrote there. Two answers that differ there make two ids, as the sha as it came would, so a
// fact applied once per id is still decided on the answer as it came; and the id, which a log line
// or an error can print (intake.ApplyFact names it), names nothing the answer wrote.
func SHAKey(sha string) string {
	if sha == "" || CommitSHA(sha) {
		return sha
	}
	digest := sha256.Sum256([]byte(sha))
	return "not-a-commit-" + hex.EncodeToString(digest[:])
}

// NotDocumented is what ShownWord prints in place of a word GitHub does not document for the member.
const NotDocumented = "(a word GitHub does not document there)"

// ShownWord is word, a member of a GitHub answer that GitHub documents as one of a few words (a pull
// request's state, an activity's type, a review's state, a permission), as a message may print it:
// word where it is one of documented, and NotDocumented otherwise.
func ShownWord(word string, documented ...string) string {
	if slices.Contains(documented, word) {
		return word
	}
	return NotDocumented
}

// NotALogin is what ShownLogin prints in place of a login GitHub does not allow.
const NotALogin = "(a login GitHub does not allow)"

// ShownLogin is login, an account a GitHub answer names, as a message may print it: login where it
// is one GitHub serves (ghrepo.IsLogin: a letter or digit, then up to 38 letters, digits, hyphens
// and underscores), or an App's bot account, its slug and "[bot]", and holds no GitHub token form
// (HoldsToken); NotALogin otherwise. The grammar is the one GitHub serves, not its sign-up form's:
// older logins GitHub still serves end in a hyphen or hold a double one, and a managed user's ends
// in `_<shortcode>`, and a message that named such an account as no login would say what is
// false. A token GitHub mints is 40 characters or more and fails the grammar's 39, but a shorter
// text spelled as one passes the underscore the grammar admits, so the token test refuses it.
func ShownLogin(login string) string {
	if ghrepo.IsLogin(strings.TrimSuffix(login, "[bot]")) && !HoldsToken(login) {
		return login
	}
	return NotALogin
}

// tokenForm matches the GitHub token forms: installation, OAuth, user-to-server, refresh and
// personal (gh[opsur]_), and fine-grained personal (github_pat_).
var tokenForm = regexp.MustCompile(`(gh[opsur]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,})`)

// HoldsToken says whether text holds a GitHub token form anywhere in it. A text that merely opens
// like one (`ghs_acme`) holds none.
func HoldsToken(text string) bool { return tokenForm.MatchString(text) }

// RedactTokens is text with every GitHub token form in it (HoldsToken) replaced by "[redacted]".
func RedactTokens(text []byte) []byte { return tokenForm.ReplaceAll(text, []byte("[redacted]")) }
