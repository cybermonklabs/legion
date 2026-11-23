package refs

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// externalRepoPattern is a repository of GitHub, `owner/name`: each half free of `/` and of
	// the whitespace RE2's `\s` matches (a space, a tab, a newline, a carriage return and a form
	// feed), and the name free of `#`.
	externalRepoPattern = regexp.MustCompile(`^([^/\s]+)/([^/\s#]+)$`)
	// externalRefPattern is a reference to an issue of such a repository, `<repository>#number`:
	// the repository free of that whitespace, which ParseExternalRepo reads, and the number after
	// the last `#`, since the repository's name holds none.
	externalRefPattern = regexp.MustCompile(`^(\S*)#([1-9][0-9]*)$`)
)

// ParseExternalRepo reads raw, an `owner/name` with none of the whitespace RE2's `\s` matches in
// or around it, into the repository it names in the form Dispatch stores it (ExternalRepo), and
// false where raw has not that shape. It trims none of that whitespace: its pattern refuses a
// space, a tab, a newline, a carriage return and a form feed anywhere in raw, so a caller that
// takes a value a person wrote trims it first, as each did when it read the value as the reference
// `<value>#1`. ExternalRepo then trims each half of every other space strings.TrimSpace removes,
// which the pattern takes: the ASCII vertical tab, and Unicode's spaces such as U+00A0, so
// `acme/x\u00a0` reads as `acme/x`. It is the one reading of a repository a person writes: the
// API reads an issue's repository, a project's default repository, a source's and the App
// installation read's path through it (ParseGitHubRepo).
func ParseExternalRepo(raw string) (repo string, ok bool) {
	match := externalRepoPattern.FindStringSubmatch(raw)
	if match == nil {
		return "", false
	}
	return ExternalRepo(match[1], match[2]), true
}

// ParseGitHubRepo reads raw as ParseExternalRepo does and holds the form it reads, the form
// Dispatch stores, to the names GitHub allows (SplitGitHubRepo), answering that form's two halves.
// The rule is put to the stored form because every later read of it trims one trailing `.git`
// more: `acme/.git.git` reads as `acme/.git`, which the rule refuses, and
// `acme/<100 characters>.git` as the 100 characters, which it takes. It is the rule of a
// repository a person writes for Dispatch to store and ask GitHub about: the API reads an issue's
// own repository and a project's default repository (api's parseIssueRepo), and a source's, the
// App installation read's path and the delivery settings' deploy repository (api's
// parseSourceRepo) through it, so a change to the rule is made here alone. It trims what
// ParseExternalRepo trims and nothing more, so each caller keeps its own trim, and each words its
// own refusal. A name the rule refuses is SplitGitHubRepo's ErrNotGitHubRepo; a raw
// ParseExternalRepo cannot read is an error that is not ErrNotGitHubRepo, so a caller can tell the
// two apart.
func ParseGitHubRepo(raw string) (owner, name string, err error) {
	repo, ok := ParseExternalRepo(raw)
	if !ok {
		return "", "", fmt.Errorf("repository %q is not an owner/name: two halves joined by one /, with no space, tab or line break, and no # in the name", raw)
	}
	return SplitGitHubRepo(repo)
}

// ParseExternalRef reads ref, an `owner/name#number` reference with any space around it, into the
// repository it names (ParseExternalRepo) and its number, and false where ref has not that shape.
// It is the one reading of such a reference: the API reads an issue's reference through it (api's
// externalRef).
func ParseExternalRef(ref string) (repo, number string, ok bool) {
	match := externalRefPattern.FindStringSubmatch(strings.TrimSpace(ref))
	if match == nil {
		return "", "", false
	}
	if repo, ok = ParseExternalRepo(match[1]); !ok {
		return "", "", false
	}
	return repo, match[2], true
}

// ExternalRepo is the repository owner/repo in the form Dispatch stores one it reads from a
// reference or a path: each half trimmed of surrounding space and lower-cased, and one trailing
// `.git` trimmed off the name.
func ExternalRepo(owner, repo string) string {
	owner = strings.ToLower(strings.TrimSpace(owner))
	repo = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(repo)), ".git")
	return owner + "/" + repo
}
