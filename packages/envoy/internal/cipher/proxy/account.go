// packages/envoy/internal/cipher/proxy/account.go
// Which account's token a GitHub call gets: a github-app name holds one token per installation,
// and the token swapped in is the one of the account the request's path names, where the name
// holds that account (serveBound reads accountSlot after choosePair).
package proxy

import (
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/requests"
)

// gitHubGitHost is the host GitHub serves a repository's git transport on, at /{owner}/{repo}.
const gitHubGitHost = "github.com"

// accountSlot is the slot of the account a GitHub request's path names, and whether slots - the
// slots of the github-app name the request is handled for, one per installation - hold it:
// /repos/{owner}/..., /orgs/{owner} and /users/{owner} on an API host, and git's /{owner}/{repo}
// on github.com. GitHub reads a login without regard to case, and the account's slot is
// requests.GitHubSlot's, the one function the release writes each installation's slot with, so
// the two fold an account alike. A path that names no account - GraphQL, search, /user - or names
// one the name holds no installation on answers false, and the request is handled for the account
// whose placeholder it carries, which is the account its gh configuration names.
func accountSlot(host, path string, slots map[string]string) (string, bool) {
	first, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	second, _, _ := strings.Cut(rest, "/")
	if first == "" || second == "" {
		return "", false
	}
	account := ""
	switch {
	case host == gitHubGitHost:
		account = first
	case first == "repos" || first == "orgs" || first == "users":
		account = second
	default:
		return "", false
	}
	slot := requests.GitHubSlot(account)
	_, held := slots[slot]
	return slot, held
}
