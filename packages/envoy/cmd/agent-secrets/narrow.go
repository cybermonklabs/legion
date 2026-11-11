// packages/envoy/cmd/agent-secrets/narrow.go
//
// The exec form's --github-owner, --github-repo and --github-read-only: a narrowing of the
// release's GitHub App tokens (POST /v1/grants/{id}/values's github_app), which the broker applies
// to every github-app secret the grant holds, and which the exec form checks the broker applied
// before it runs anything. The exec form's --github-token is here too: it is no narrowing, but it
// reads the same release, and it hands the child that one installation's token alone in place of
// the JSON object of installations the release carries.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/policy"
)

// narrowingFlags names each field of a narrowing (policy.NarrowingError's Field) by the flag that
// gives it.
var narrowingFlags = map[string]string{"owner": "--github-owner", "repositories": "--github-repo"}

// githubNarrowing is the narrowing the flags ask for, nil when they ask for none, checked and in
// its canonical form by policy.ParseGitHubNarrowing, the broker's own parser, so a narrowing the
// broker would refuse 400 INVALID_NARROWING exits 2 here before any request or approval wait, and
// narrowedAsAsked compares like with like. ownerSet says whether --github-owner was given at all:
// given with no account (an unset variable a caller passed) it is refused rather than read as no
// narrowing, which would release the owner-wide tokens; and --github-repo or --github-read-only
// without --github-owner is refused, since a narrowing names one account's token. tokenAlone says
// whether --github-token was given: it hands over one account's token, so it is refused the same
// way, and it is refused first, since a caller who asked for a token and named no account is told
// about the flag they gave.
func githubNarrowing(owner string, ownerSet bool, repositories []string, readOnly, tokenAlone bool) (*GitHubNarrowing, error) {
	switch {
	case !ownerSet && tokenAlone:
		return nil, errors.New("--github-token hands over one account's GitHub App token: name the account with --github-owner")
	case !ownerSet && (len(repositories) > 0 || readOnly):
		return nil, errors.New("--github-repo and --github-read-only narrow one account's GitHub App token: name the account with --github-owner")
	case !ownerSet:
		return nil, nil
	}
	parsed, refusal := policy.ParseGitHubNarrowing(owner, repositories, readOnly)
	if refusal != nil {
		return nil, fmt.Errorf("%s %s", narrowingFlags[refusal.Field], refusal.Reason)
	}
	return &GitHubNarrowing{Owner: parsed.Owner, Repositories: parsed.Repositories, ReadOnly: parsed.ReadOnly}, nil
}

// narrowedAsAsked checks that the broker applied asked to the release it answered with applied. A
// broker from before narrowing ignores the release's body and answers no github_app, having
// released the owner-wide tokens, which the command must not get in place of the narrowed one.
// This check stays: a client can meet an older broker on every future independent rollout.
func narrowedAsAsked(asked, applied *GitHubNarrowing) error {
	if asked == nil {
		return nil
	}
	if applied != nil && applied.Owner == asked.Owner && slices.Equal(applied.Repositories, asked.Repositories) && applied.ReadOnly == asked.ReadOnly {
		return nil
	}
	answered := "no narrowing, as a broker from before narrowing does when it releases the owner-wide token"
	if applied != nil {
		encoded, _ := json.Marshal(applied)
		answered = string(encoded)
	}
	encoded, _ := json.Marshal(asked)
	return fmt.Errorf("the broker did not narrow the GitHub App token to %s: it answered %s", encoded, answered)
}

// githubTokensOf reads value as a github-app secret's released value: the JSON object the broker
// mints, {"installations":{"<account>":{"installation_id":…,"token":…,"expires_at":…}}}, one entry
// per installation the release covers (internal/broker/secrets, githubMintedInstallations). It
// answers each account's own token, keyed by that account, and ok only for that object: the
// document holds the installations field and nothing else, every key is a GitHub login by the
// broker's own parser (policy.ParseGitHubLogin), and every entry carries all three fields of a
// minted installation, each of its own type. The broker keys each installation by the account
// GitHub answered with, so a document whose key is no login is no release of the broker's. The
// daemon's token_command reads a release the same way
// (packages/daemon/internal/appauth/tokencommand.go, loginPattern). Every other value — a
// password, a JSON document of another shape — answers ok false and keeps its own value, so
// --github-token reads a release and never guesses at one.
//
// It reads no expiry and no installation id beyond their shape: the broker refuses an expired
// mint, and the flag chooses what the child is handed, not how long it works for.
func githubTokensOf(value string) (tokens map[string]string, ok bool) {
	var document map[string]json.RawMessage
	if json.Unmarshal([]byte(value), &document) != nil || len(document) != 1 || document["installations"] == nil {
		return nil, false
	}
	// Each field is a pointer, so that a missing one is told from an empty one, and so that a
	// JSON null leaves it nil: an entry short of a field is no minted installation. The broker
	// writes the installation id as a number, so a JSON string that holds one, such as "8",
	// fails this decode as any other wrong type does.
	var installations map[string]struct {
		InstallationID *int64  `json:"installation_id"`
		Token          *string `json:"token"`
		ExpiresAt      *string `json:"expires_at"`
	}
	if json.Unmarshal(document["installations"], &installations) != nil || installations == nil {
		return nil, false
	}
	tokens = make(map[string]string, len(installations))
	for account, installation := range installations {
		if _, err := policy.ParseGitHubLogin(account); err != nil || installation.InstallationID == nil {
			return nil, false
		}
		if installation.Token == nil || *installation.Token == "" || installation.ExpiresAt == nil || *installation.ExpiresAt == "" {
			return nil, false
		}
		tokens[account] = *installation.Token
	}
	return tokens, true
}

// githubTokensAlone is what --github-token does to the released values: every value that is a
// github-app release becomes that installation's own token, the string a command puts in an
// Authorization header. Without the flag the value stays the JSON object of installations, which
// a command sends to GitHub as it stands and GitHub answers 401. A value that is no such release
// keeps its own value, since the flag says what a GitHub App token is handed over as and says
// nothing about a password.
//
// Two refusals keep the flag from handing over something a caller cannot use. A release of other
// than one installation has no one token to hand over, and a narrowing cannot always make it one:
// GitHubApp.Mint compares the owner without regard to case, so --github-owner ada mints for ada
// and for Ada alike. The refusal therefore asks for a narrowing that leaves one installation, or
// for the same run without the flag, whose child reads the token out of the JSON object. A grant
// with no release at all is a flag that does nothing, and the caller would learn of it only at
// GitHub's 401. Each refusal names the secret or the names it read, the accounts and the flag,
// and no part of any token.
func githubTokensAlone(values map[string]string) error {
	names := slices.Sorted(maps.Keys(values))
	// The releases are rewritten only once the walk found no reason to refuse, so a refusal
	// leaves the caller's values as they were rather than half rewritten.
	alone := make(map[string]string, len(names))
	for _, name := range names {
		tokens, isRelease := githubTokensOf(values[name])
		if !isRelease {
			continue
		}
		accounts := slices.Sorted(maps.Keys(tokens))
		if len(accounts) != 1 {
			held := "no installation"
			if len(accounts) > 0 {
				held = fmt.Sprintf("%d installations (%s)", len(accounts), strings.Join(accounts, ", "))
			}
			return fmt.Errorf("--github-token hands over one installation's token, and the release of %s holds %s: ask again with a narrowing that leaves one installation, or without --github-token, and read the token out of the JSON object the release hands over", name, held)
		}
		alone[name] = tokens[accounts[0]]
	}
	if len(alone) == 0 {
		read := "no value at all"
		if len(names) > 0 {
			read = strings.Join(names, ", ")
		}
		return fmt.Errorf("--github-token hands over a GitHub App installation token, and the grant released none: it read %s", read)
	}
	maps.Copy(values, alone)
	return nil
}
