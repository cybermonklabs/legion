// packages/envoy/internal/cipher/requests/target.go
// A request's target (record.Target) against the GitHub profile it narrows: the scope a release
// of the profile mints for it (gitHubScope), the refusal Create and an approval make of a target
// the profile cannot serve (checkTarget), and the words such a refusal names the profile in
// (profileWords). The release (machine_read.go), Create and an approval (machine_state.go) and
// Create's check mint (profile_check.go) all read a target through these.
package requests

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// gitHubScope is the scope a release of name, served as served, mints for target (the zero
// record.Target for none), and nil for a name that is no GitHub profile. Its repositories are
// account/name entries (githubname.Entry), as the profiles file writes them
// (secrets.GitHubScope), so the minter sends each account its own.
//
// With no target it is the profile's own accounts, repositories, permissions and value form.
// policy.ParseProfiles refuses a profile that lists an account none of its repositories names,
// so each account in effect has a repository of its own; were one built some other way, the
// minter refuses its scope rather than reach the account whole (secrets.GitHubScope's refusal).
// A value-token profile, whose release is one account's bare token, is refused with no target
// while it names more than one account (ErrTargetRefused, telling the caller to name one with
// --target): this is the one place that rule is written, for Create (checkTarget) and every
// release alike, so the minter is never asked for such a token.
//
// A target narrows the profile and never widens it: its one account, which the profile must
// name, with the target's repositories, each one the profile names where it names any, or, for an
// account target, the profile's repositories of that account. A target's repository matches the
// profile's without regard to case, as GitHub matches repository names (secrets' heldBy), and the
// scope names it as the profiles file writes it, so the minter is asked for the profile's own
// words; for a profile that names no repository it names it as the target does. A target outside
// the profile and a target on a name with no github block are refused (ErrTargetRefused), naming
// the target (record.Target.String) and the profile's accounts and repositories: Create answers
// that refusal before anybody is asked (checkTarget), and a release mints nothing for it. A
// profile served from a github-installations secret (policy.TypeGitHubInstallations) hands over
// tokens a minter made ahead of the request, which cannot be cut once they are minted, so a
// target of it names its account alone, and one naming repositories is refused
// (ErrTargetRepositoriesRefused).
func gitHubScope(name string, served policy.Secret, target record.Target) (*secrets.GitHubScope, error) {
	profile := served.Profile
	if profile == nil || profile.GitHub == nil {
		if target.Account != "" {
			return nil, fmt.Errorf("%w: %s has no github block, so it names no GitHub account or repository a target could narrow", ErrTargetRefused, name)
		}
		return nil, nil
	}
	block := profile.GitHub
	minted := served.Type() == policy.TypeGitHubInstallations
	scope := &secrets.GitHubScope{Accounts: block.Accounts, Permissions: block.Permissions, Value: block.Value}
	if target.Account == "" {
		if block.Value == githubname.ValueToken && len(block.Accounts) > 1 {
			form := "ACCOUNT[/REPOSITORY]"
			if minted {
				form = "ACCOUNT"
			}
			return nil, fmt.Errorf("%w: %s releases one account's bare token, and the profile names %s: name one with --target %s", ErrTargetRefused, name, profileWords(block), form)
		}
		scope.Repositories = block.Repositories
		return scope, nil
	}
	// The profile's repositories of the target's account, or nil for a profile that names none
	// and so reaches every repository its installations do.
	var reached []string
	for _, repository := range block.Repositories {
		if owner, _, _ := githubname.SplitEntry(repository); owner == target.Account {
			reached = append(reached, repository)
		}
	}
	outside := func() error {
		return fmt.Errorf("%w: %s: the target %s is outside the profile, which names %s", ErrTargetRefused, name, target.String(), profileWords(block))
	}
	// An account the profile names no repository of is refused too. ParseProfiles refuses such a
	// profile, so this guards only one built some other way, and it must: an empty reached is the
	// nil Repositories of a scope of every repository, so without it such a target would widen.
	if !slices.Contains(block.Accounts, target.Account) || block.Repositories != nil && len(reached) == 0 {
		return nil, outside()
	}
	scope.Accounts = []string{target.Account}
	scope.Repositories = reached
	if len(target.Repositories) == 0 {
		return scope, nil
	}
	if minted {
		return nil, fmt.Errorf("%w: %s: name the account alone, --target %s", ErrTargetRepositoriesRefused, name, target.Account)
	}
	// The target's repositories as account/name entries, each the profile's own entry for it
	// where the profile names repositories. Two names of the target that differ only in case give
	// the profile's one entry, so the entries are sorted again and each kept once.
	scope.Repositories = make([]string, 0, len(target.Repositories))
	for _, entry := range target.Entries() {
		if block.Repositories != nil {
			at := slices.IndexFunc(reached, func(r string) bool { return strings.EqualFold(r, entry) })
			if at < 0 {
				return nil, outside()
			}
			entry = reached[at]
		}
		scope.Repositories = append(scope.Repositories, entry)
	}
	slices.Sort(scope.Repositories)
	scope.Repositories = slices.Compact(scope.Repositories)
	return scope, nil
}

// checkTarget is the refusal of a request's target (ErrTargetRefused) that Create makes, and an
// approval that would grant makes again (applyDecisionAttempt), nil where every name can be
// served with it. target is the target the request's signed locations name, the zero
// record.Target for none; set is the snapshot Create or the approval evaluates names against. A
// name set does not serve is passed over, since the evaluation answers it with the policy's own
// error. Every refusal is gitHubScope's, the rule the release mints by: a target outside a name's
// profile, a target on a name with no github block, and no target on a value-token profile of
// several accounts. Here nobody is asked to approve a release that rule refuses.
func checkTarget(set *policy.Set, names []string, target record.Target) error {
	for _, name := range names {
		served, ok := set.Secrets[name]
		if !ok {
			continue
		}
		if _, err := gitHubScope(name, served, target); err != nil {
			return err
		}
	}
	return nil
}

// profileWords is a GitHub profile's scope as a refusal of a target names it, so the caller reads
// what a target may name: its accounts, and its repositories or every repository its
// installations reach. It holds nothing but the profiles file's own words.
func profileWords(block *policy.GitHubBlock) string {
	words := "the accounts " + strings.Join(block.Accounts, ", ")
	if len(block.Accounts) == 1 {
		words = "the account " + block.Accounts[0]
	}
	if block.Repositories == nil {
		return words + " and every repository the App's installations on them reach"
	}
	return words + " and the repositories " + strings.Join(block.Repositories, ", ")
}
