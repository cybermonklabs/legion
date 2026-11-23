// Package githubname is the shape of GitHub's own words, written once for the broker: a login, a
// repository's name, a permission's name and level, the two forms a GitHub profile's value takes,
// and the most repositories one token names. It imports nothing but the standard library, so every
// holder of these rules can import it without what it holds: the profiles file's parser (policy),
// the request object's locations (record), and the minter (secrets), and record does not link the
// minters through it. Each caller keeps its own refusal's words; only the rule is here.
package githubname

import (
	"regexp"
	"slices"
	"strings"
)

// The two forms a GitHub profile's value takes (secrets.GitHubScope.Value, and policy's
// GitHubBlock.Value): the JSON of one entry per account, with its installation_id, token and
// expires_at, which is the default, or the bare token of the one account in effect.
const (
	ValueInstallations = "installations"
	ValueToken         = "token"
)

// MaxNarrowedRepositories is the most repositories one token names: a GitHub profile's
// repositories, and so a target's locations. GitHub's access_tokens body takes 500, but its answer
// carries a full repository object for each one, about 5 KB, and the minter reads that answer to a
// cap sized for this many (secrets' accessTokensResponseLimit): a token GitHub minted whose answer
// the minter cannot read whole cannot be released, and its revoke is sent only where the part it
// read names the token.
const MaxNarrowedRepositories = 100

// Entry is one repository as the profiles file, a minter's scope and a target's locations write
// it: its account and its own name, account/name. Every join of the two is this one.
func Entry(account, name string) string {
	return account + "/" + name
}

// SplitEntry reads an Entry back: the account before the first "/" and the name after it, and
// false for text that holds no "/". A name that holds another "/" is left whole for its caller's
// rule (ValidRepositoryName) to refuse. Every split of an entry is this one.
func SplitEntry(entry string) (account, name string, ok bool) {
	return strings.Cut(entry, "/")
}

// MetadataPermission is the permission GitHub grants at read with every installation token,
// whatever its request named, and which every installation it lists holds. The minter refuses a
// token holding a permission its request did not name (secrets' heldBy), so every GitHub profile
// names it, at read and no other level (policy.ParseProfiles): every endpoint GitHub lists under it
// ("Permissions required for GitHub Apps") needs read.
const MetadataPermission = "metadata"

// LevelRank orders the levels GitHub gives a permission: read 1, write 2, admin 3. Every other
// word, "" among them, ranks 0, below read, so it holds nothing and is no level GitHub gives.
func LevelRank(level string) int {
	switch level {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	default:
		return 0
	}
}

// permissionName is what a GitHub App permission's name is: lowercase letters and underscores
// (contents, pull_requests, organization_administration).
var permissionName = regexp.MustCompile(`^[a-z_]+$`)

// ValidPermissionName reports whether name can be a GitHub App permission's name.
func ValidPermissionName(name string) bool {
	return permissionName.MatchString(name)
}

// writeOnlyPermissions are the permissions GitHub grants at write alone, the one level its REST
// description (components.schemas.app-permissions) gives them.
var writeOnlyPermissions = []string{"profile", "workflows"}

// WriteOnlyPermission reports whether GitHub grants permission at write alone, so a request for
// it at read is one GitHub refuses at every mint.
func WriteOnlyPermission(permission string) bool {
	return slices.Contains(writeOnlyPermissions, permission)
}

// gitHubLogin is what a GitHub login can be: an ASCII letter or digit, then ASCII letters, digits,
// hyphens and underscores. It is matched against the text as written, before any lowercasing,
// since Unicode lowercasing maps some non-ASCII letters onto ASCII ones (U+212A KELVIN SIGN to `k`,
// U+0130 to `i` with a combining dot). GitHub's sign-up rule today also refuses an underscore and
// a trailing or doubled hyphen, but accounts made under its older rules keep their hyphens (`x-`
// is an organization from 2011), and a managed user's login ends in `_<shortcode>`
// (`mona-cat_octo`), so this refuses only what no login can hold. A login is at most
// maxGitHubLogin characters, which the pattern does not check.
var gitHubLogin = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

const maxGitHubLogin = 39

// ValidLogin reports whether login, as written, can be a GitHub login: up to 39 ASCII letters,
// digits, hyphens and underscores, starting with a letter or digit. Either case passes, since
// GitHub compares logins without regard to it; a caller that holds a login to lowercase, or
// refuses a word of its own, says so beside the call.
func ValidLogin(login string) bool {
	return len(login) <= maxGitHubLogin && gitHubLogin.MatchString(login)
}

// gitHubRepositoryName is what a repository name GitHub creates can be: ASCII letters, digits,
// '.', '-' and '_', at most 100 of them.
var gitHubRepositoryName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// ValidRepositoryName reports whether name can be a GitHub repository's own name, without its
// owner: gitHubRepositoryName, and neither . nor .., which name no repository in a path.
func ValidRepositoryName(name string) bool {
	return gitHubRepositoryName.MatchString(name) && name != "." && name != ".."
}
