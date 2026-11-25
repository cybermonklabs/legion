package githubrest

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
)

// A member of a GitHub answer that the daemon decodes as a plain string and decides on as it came -
// a commit's sha, a state, an account's login - is GitHub's words, or the words of whatever answers
// in GitHub's place, which can echo the request back, its installation token and all. A log line or
// an error names such a member only in the one shape GitHub gives it, and a fixed phrase in place of
// anything else: nothing else the answer wrote there prints, and every decision is still made on the
// member as it came.

// NotACommit is what ShownSHA prints in place of a sha that is not a commit's.
const NotACommit = "(a sha that is not a commit's)"

// ShownSHA is sha, a commit a GitHub answer names, as a message may print it: the sha where it is
// spelled as GitHub spells every commit it reports, forty lowercase hexadecimal characters
// (reviewround.IsHead, the one test of that spelling), "" where it is empty, and NotACommit
// otherwise.
func ShownSHA(sha string) string {
	if sha == "" || reviewround.IsHead(sha) {
		return sha
	}
	return NotACommit
}

// SHAKey is sha, a commit a GitHub answer names, as an event id names it: the sha where it is
// spelled as a commit (reviewround.IsHead) or empty, and otherwise "not-a-commit-" and the hex
// SHA-256 of what the answer wrote there. Two answers that differ there make two ids, as the sha
// as it came would, so a fact applied once per id is still decided on the answer as it came; and
// the id, which a log line or an error can print (intake.ApplyFact names it), names nothing the
// answer wrote.
func SHAKey(sha string) string {
	if sha == "" || reviewround.IsHead(sha) {
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

// ShownLogins is each of logins, the accounts a GitHub answer names, as a message may print it
// (ShownLogin), in the order the answer named them, so a reader still sees every entry is there.
// The answer is decided on as it came; only its printing goes through this. The answer is never
// nil, so a route that serves it serves an empty list rather than null.
func ShownLogins(logins []string) []string {
	shown := make([]string, 0, len(logins))
	for _, login := range logins {
		shown = append(shown, ShownLogin(login))
	}
	return shown
}

// NotABranch is what ShownBranch prints in place of a branch name it does not print.
const NotABranch = "(a branch name the daemon does not print)"

// ShownBranch is branch, a branch a GitHub answer names (a repository's default_branch, which the
// daemon records and later names), as a message may print it: branch where git takes it as a
// branch's name (branchName) and it holds no GitHub token form (HoldsToken); NotABranch
// otherwise. A token GitHub mints is a name git takes, so the token test refuses it.
func ShownBranch(branch string) string {
	if branchName(branch) && !HoldsToken(branch) {
		return branch
	}
	return NotABranch
}

// branchName says whether name is one `git check-ref-format --branch` takes, held to what a
// message may print: valid UTF-8 of graphic characters with no space, none of `~^:?*[\`, no `..`
// or `@{`, not `@` and not opening with `-`, no empty component and none that opens with `.` or
// ends in `.lock`, ending in neither `/` nor `.`, and at most 255 bytes, GitHub's bound on a ref.
func branchName(name string) bool {
	if name == "" || name == "@" || len(name) > 255 || !utf8.ValidString(name) ||
		strings.HasPrefix(name, "-") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.ContainsAny(name, `~^:?*[\`) {
		return false
	}
	for _, r := range name {
		if !unicode.IsGraphic(r) || unicode.IsSpace(r) {
			return false
		}
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// tokenForm matches the GitHub token forms: installation, OAuth, user-to-server, refresh and
// personal (gh[opsur]_), and fine-grained personal (github_pat_).
var tokenForm = regexp.MustCompile(`(gh[opsur]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,})`)

// HoldsToken says whether text holds a GitHub token form anywhere in it. A text that merely opens
// like one (`ghs_acme`) holds none.
func HoldsToken(text string) bool { return tokenForm.MatchString(text) }

// RedactTokens is text with every GitHub token form in it (HoldsToken) replaced by "[redacted]".
func RedactTokens(text []byte) []byte { return tokenForm.ReplaceAll(text, []byte("[redacted]")) }

// plainMessage is a message of GitHub's that is plain words: letters, spaces and a sentence's
// punctuation, at most 200 characters, the shape of each message GitHub's REST API writes for a
// read it refuses ("Not Found", "Bad credentials", "Moved Permanently"). A digit, a slash, a colon
// or an underscore is none of it, so no URL and no token GitHub mints has the shape.
var plainMessage = regexp.MustCompile(`^[A-Za-z][A-Za-z ,.'()-]{0,199}$`)

// PlainMessage says whether message, a message of GitHub's answer, is plain words (plainMessage).
func PlainMessage(message string) bool { return plainMessage.MatchString(message) }

// repositoryRedirect is where GitHub's redirect for a repository that moved points: the
// repository by its id on api.github.com, with no user information, query or fragment.
var repositoryRedirect = regexp.MustCompile(`^https://api\.github\.com/repositories/[0-9]+$`)

// RepositoryRedirect says whether location, a Location of GitHub's answer, is repositoryRedirect.
func RepositoryRedirect(location string) bool { return repositoryRedirect.MatchString(location) }
