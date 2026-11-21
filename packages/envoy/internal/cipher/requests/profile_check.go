// packages/envoy/internal/cipher/requests/profile_check.go
// profile_check.go is the check mint Machine.Create makes of each GitHub profile a request names
// before it writes a row or asks anybody to approve the request, so a profile the broker cannot
// mint costs nobody an approval. The same verdict is made again at release (Machine.Values,
// machine_read.go), which is where it stays for a grant that is released a second time; this file
// is only about reaching it earlier. It is a file of its own because machine_state.go, which holds
// Create, is long already and nothing here is a state transition: the check reads, asks GitHub and
// decides, and writes nothing at all.
package requests

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// NarrowingCheckSkippedMessage is logged, at INFO, each time trouble — never a refusal — ended
// Create's check mint, followed by enrollment=, name=<the profile the check had reached, empty
// where it had reached none>, reason=<why it ended> and, where something failed, error=, which
// never holds a stored value or a token. Such a request takes the path it took before the check
// existed: the person is asked, and the release makes the refusal the check did not. It is INFO
// rather than WARN because nothing is wrong with the request, and it is a line of its own so that
// an operator can read how often GitHub, the secrets store or the check's bounds leave the verdict
// unreached without reading it as a failed mint.
const NarrowingCheckSkippedMessage = "broker: the narrowing check was skipped"

// DefaultNarrowingCheckBudget bounds the whole check of one request — its GitHub profiles and the
// search for the repository GitHub refused together — and DefaultNarrowingCheckMint one mint of
// that check and that mint's revoke, each derived from what is left of the budget.
//
// The two numbers come from the clocks around Create. `agent-secrets` gives one call 30 seconds
// (leasetime.RenewClientTimeout) and the broker bounds every request at 45 seconds
// (requestDeadline, cmd/cipherd/main.go), and Create does its policy work and its whole
// transaction inside that same call, so the check takes at most a third of the client's budget.
// A caller that gave up before the broker wrote its pending request is the cost this check
// removes, so the check must end well inside what the caller is waiting out.
//
// One revoke runs past these bounds: the give-back of a token GitHub minted in an answer the check
// could not use (secrets.GitHubApp's giveBackUnusable) runs under a detached bound of its own,
// secrets' revokeTimeout (10 seconds), since the check's own bound is the likeliest thing to have
// cut that answer off. No mint starts once the budget is spent, so at most one such give-back runs
// past it, and a check takes at most 20 seconds in all: still inside the client's 30, with less
// of that to spare.
const (
	DefaultNarrowingCheckBudget = 10 * time.Second
	DefaultNarrowingCheckMint   = 5 * time.Second
)

// DefaultNarrowingChecks bounds, per enrollment, the checks that reach GitHub: five at once,
// refilled one every twelve seconds. A refused request writes no row, so the guards inside
// Create's transaction (lockPendingNames, pendingRequests) never see it, and a caller in a loop
// could otherwise drive the App into a rate limit at GitHub, which would turn the check off for
// everybody. Past it the GitHub part is skipped and the request goes to its approver, which is
// what it did before this check existed. The budget is per process, as DefaultMissRereads' is,
// and for the same reason: it bounds load rather than authority.
var DefaultNarrowingChecks = ratelimit.Limit{Every: 12 * time.Second, Burst: 5}

// narrowingCheckLimiter is the per-enrollment buckets NarrowingChecks names, built on the first
// check that reaches GitHub.
func (m *Machine) narrowingCheckLimiter() *ratelimit.Keyed {
	m.narrowingChecksOnce.Do(func() {
		m.narrowingChecks = ratelimit.NewKeyed(limitOr(m.NarrowingChecks, DefaultNarrowingChecks))
	})
	return m.narrowingChecks
}

// narrowingCheckBudget bounds the whole check and narrowingCheckMintBound one mint of it: the
// machine's own fields, or the defaults for a machine that sets neither.
func (m *Machine) narrowingCheckBudget() time.Duration {
	return cmp.Or(m.NarrowingCheckBudget, DefaultNarrowingCheckBudget)
}

func (m *Machine) narrowingCheckMintBound() time.Duration {
	return cmp.Or(m.NarrowingCheckMint, DefaultNarrowingCheckMint)
}

// gitHubProfile reports whether served is a profile with a github block, which the check mint
// is made of.
func gitHubProfile(served policy.Secret) bool {
	return served.Profile != nil && served.Profile.GitHub != nil
}

// checkProfiles is Create's whole verdict on the GitHub profiles a request names: the refusal
// Create answers, and nil both for profiles the broker could mint and for any it could not
// decide. names are the names the request asks for, set the snapshot Create evaluates them
// against, facts what Dispatch answered for them (projectFacts, resolved before this), reason
// the request object's own reason and target the target its signed locations name (the zero
// record.Target for none), all as Create holds them. It runs
// after reuseLiveGrant, which asks nobody for a grant that already exists, and before the
// transaction, so a refusal writes no request row, makes no credential-request record and
// interrupts no person; it also holds no row lock while it waits on GitHub, which is the rule
// rereadMissing already states for the policy reread above it.
//
// It runs for every request that names a GitHub profile, whether or not the profile names
// repositories, for the scope in effect, the target's account or the profile's accounts
// (gitHubScope), so an account with no installation and a cut that leaves no permission are
// refused before a person is asked as a repository the installation does not reach is. A request
// that names none returns at once, reading nothing. The whole check runs only where the policy
// would send this request to a person, which is the only case that spends an approval on a
// profile the broker will not mint. To learn that, it reads the requester on the pool and
// evaluates the policy once more for this one decision. That read races the transaction's own
// read of the same rows — a name withheld, or an operator changed, in between — and the race is
// harmless: it decides nothing but whether to check, and the transaction decides the request
// itself as it always did. An error from either read, which includes policy.ErrUnknownSecret and
// ErrMixedApprovers, skips the check and changes no answer for the same reason: the transaction
// answers those in a moment, in the order a caller reads them today. A state that is not pending
// skips it too, so an agent-tier request and the Legion daemon's own token command reach GitHub
// for nothing.
func (m *Machine) checkProfiles(ctx context.Context, enr enrollmentRow, names []string, set *policy.Set, facts projectFacts, reason string, target record.Target) error {
	profiled := false
	for _, name := range names {
		if gitHubProfile(set.Secrets[name]) {
			profiled = true
			break
		}
	}
	if !profiled {
		return nil
	}
	requester, err := m.requester(ctx, m.Store.Pool, enr, facts)
	if err != nil {
		skippedNarrowingCheck(enr.ID, "", "the requester could not be read", err)
		return nil
	}
	e, err := m.evaluate(set, names, requester, reason)
	if err != nil || e.state != "pending" {
		return nil
	}
	mint := m.Minters[policy.TypeGitHubApp]
	if mint == nil {
		skippedNarrowingCheck(enr.ID, "", "this broker has no github-app minter", nil)
		return nil
	}
	if !m.narrowingCheckLimiter().Allow(enr.ID) {
		skippedNarrowingCheck(enr.ID, "", "this enrollment has reached GitHub for more checks than its bound allows", nil)
		return nil
	}
	budget, cancel := context.WithTimeout(ctx, m.narrowingCheckBudget())
	defer cancel()
	// Every GitHub profile of the request in turn, since the release mints for each of them and
	// one refusal refuses the release. The first refusal ends the check: the caller has its
	// answer, and the names after it would be asked about for nothing.
	for _, d := range e.decisions {
		served := set.Secrets[d.Name]
		secretType, source, minted, err := policy.ParseTypeSource(d.Source)
		if err != nil || !minted || secretType != policy.TypeGitHubApp || !gitHubProfile(served) {
			continue
		}
		checked, refusal := m.checkProfile(budget, mint, enr.ID, d.Name, source, served, target)
		if !checked {
			return nil
		}
		if refusal != nil {
			refusedBeforeApproval(enr.ID, d.Name, refusal)
			return refusal
		}
	}
	return nil
}

// checkProfile is the check mint's verdict on one GitHub profile, for the scope in effect: the
// request's target, or the profile's own where it names none (gitHubScope). checked
// is true with no refusal where GitHub would serve its release, and true with the refusal Create
// answers where it would not. checked is false where trouble ended the check instead — a stored
// value that could not be read or that the minter cannot mint from, an answer that is neither a
// token nor a refusal of the scope, the check's own time gone — which this logs
// (NarrowingCheckSkippedMessage, one reason= of its own for each) and which leaves Create on the
// path it took before the check existed. That last rule is a requirement of its own: an outage at
// GitHub must not stop every GitHub App request of the fleet, which is a larger failure than the
// spent approval this check saves.
//
// Only two answers refuse the request: the profile's scope refused (secrets.ErrMintNarrowingRefused:
// a cut that leaves no permission, or a bare token of an account the App has more than one
// installation on, each of which the minter refuses before any token request, or GitHub's 422 to
// the token request) and the App with no installation on an account the scope names
// (secrets.ErrMintNoInstallation). Every other refusal GitHub makes is trouble here, since every
// 4xx that is no rate limit wraps secrets.ErrMintRefused (secrets' mintStatusError) and a 401 to
// the App's JWT, a 403 of some other kind and a 404 on the token route each say nothing about the
// scope. Refusing the request for one of those would tell the caller to fix a profile that is not
// at fault and would hide a broken App key behind it.
func (m *Machine) checkProfile(ctx context.Context, mint Minter, enrollmentID, name, source string, served policy.Secret, target record.Target) (checked bool, refusal error) {
	// Create holds the target to the profile inside its transaction, once the policy has decided
	// the names (checkTarget), which is after this check. This check runs only for a request the
	// policy sends to a person, so no name here is denied, and a target gitHubScope refuses is
	// answered now, as the refusal checkTarget would make, before GitHub is asked anything.
	scope, err := gitHubScope(name, served.Profile, target)
	if err != nil {
		return true, err
	}
	stored, err := m.Secrets.Read(ctx, source)
	if err != nil {
		skippedNarrowingCheck(enrollmentID, name, "the stored value could not be read", err)
		return false, nil
	}
	// The line-ending rule is spent here as the release spends it (Machine.release), and writes no
	// line: the release logs TrailingLineEndingRemovedMessage once for each value the rule
	// changed, and a check that logged it too would write that line twice for one release.
	value, _ := policy.TrimLineEnding(stored)
	switch err := m.checkMint(ctx, mint, name, served, value, scope); {
	case err == nil:
		return true, nil
	case errors.Is(err, secrets.ErrMintNarrowingRefused):
		// An empty cut, a bare token of several installations, or a scope the minter refuses
		// before it asks GitHub anything, is its own answer: every probe below would meet it
		// again and blame the first repository it asks about. GitHub's 422 to a scope of one
		// account's repositories is what the search reads.
		if errors.Is(err, secrets.ErrMintEmptyCut) || errors.Is(err, secrets.ErrMintSeveralInstallations) || len(scope.Accounts) != 1 || len(scope.Repositories) == 0 {
			return true, fmt.Errorf("%s: %w", name, err)
		}
		return true, m.narrowingCause(ctx, mint, name, served, value, scope)
	case errors.Is(err, secrets.ErrMintNoInstallation):
		// The App has no installation on an account the scope names, which no profile of that
		// account can mend and which the release refuses too. The minter's own words hold "has no
		// installation on", which a released client matches in this broker's answer (appauth's
		// brokerNoInstallation, packages/daemon), so they are carried whole and the profile's name
		// is put in front of them.
		return true, fmt.Errorf("%s: %w", name, err)
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		// The check's own budget, a mint's own bound or a caller that went away ended it, either
		// before the mint asked GitHub anything (checkMint answers ctx.Err() first of all) or
		// while a call of it was in flight (it wraps the context error over the minter's own,
		// which writes the context's words into its text without wrapping it). GitHub decided
		// nothing either way, so this is not logged as an answer of GitHub's: an operator reading
		// a run of these raises the bounds or looks at what is slowing Create down.
		skippedNarrowingCheck(enrollmentID, name, "the check's time ran out, or its caller went away, before GitHub answered", err)
		return false, nil
	case errors.Is(err, secrets.ErrMintStoredValue):
		// The stored value was read and the minter cannot mint from it: it is no JSON object of
		// an app_id and a private_key, or the key does not parse. The secret's owner stores a
		// new value, and the release says the same thing again. GitHub said nothing here
		// either, and the error holds no part of the value (secrets' GitHubApp.Mint).
		skippedNarrowingCheck(enrollmentID, name, "the stored value is not an App id and private key the minter can read", err)
		return false, nil
	case errors.Is(err, secrets.ErrMintRefused):
		// GitHub refused the call rather than the scope: the App's JWT was not accepted (a clock
		// far out, a key rotated or wrong), or its answer was some other 4xx that is no rate
		// limit, a 404 on the token route among them. The profile may be perfectly mintable once
		// the App works again, so the request takes the path it took before the check existed
		// and the release says the same thing again.
		skippedNarrowingCheck(enrollmentID, name, "GitHub refused the App rather than the narrowing", err)
		return false, nil
	default:
		skippedNarrowingCheck(enrollmentID, name, "GitHub's answer decided nothing about the narrowing", err)
		return false, nil
	}
}

// checkMint is one mint of the check, of scope: the calls the release makes, in the same order,
// with every token GitHub answers revoked at once and none released, answering the error the
// release would have answered (secrets.MintRequest.Check). The request is the one Machine.release
// builds for this name (mintRequest), with scope, the one in effect or a probe's part of it, and
// Check set, so the check and the release ask GitHub about the same secret, with the same scope;
// the cause search asks with less of it.
//
// Each mint, and the revoke inside it, is bounded by NarrowingCheckMint and by what is left of
// the check's whole budget, whichever ends first: ctx already carries the budget, so deriving the
// mint's own bound from it takes the shorter of the two. The give-back of a token whose answer the
// mint could not use runs to a detached bound of its own instead (DefaultNarrowingCheckBudget's
// comment). A budget already spent is answered without asking GitHub anything, and reads as
// trouble to the caller, which lets the request through.
func (m *Machine) checkMint(ctx context.Context, mint Minter, name string, served policy.Secret, value string, scope *secrets.GitHubScope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, m.narrowingCheckMintBound())
	defer cancel()
	// The check runs for github-app names alone (checkProfiles), so that is the type its minter
	// counts this attempt under, as the release it stands for would.
	request := mintRequest(name, policy.TypeGitHubApp, served, value, scope)
	request.Check = true
	_, err := mint(bounded, request)
	if err != nil && bounded.Err() != nil {
		// The bound, or the budget it was derived from, ended the call while it was in flight.
		// The minter names such a call by its class and writes the context's own words into its
		// text without wrapping the context error (secrets' mintCall and transportFailure), so
		// the context error is wrapped here instead: the caller then reads this as its time
		// running out rather than as an answer of GitHub's that decided nothing.
		return fmt.Errorf("%w: %w", bounded.Err(), err)
	}
	return err
}

// narrowingCause is which repository of scope GitHub refused, as the refusal Create answers.
// GitHub answers 422 and the broker never quotes its text (secrets' refusal), so the only way to
// learn the repository is to ask again with fewer of them, each probe another check mint whose
// token is revoked the moment GitHub answers it. scope names one account and at least one
// repository (checkProfile).
//
// A scope that names exactly one repository needs no probe at all: the verdict mint asked for
// that one repository and GitHub refused it, so that repository is the cause. A scope that names
// more is probed one repository at a time, each naming that repository alone, in the scope's own
// order, stopping at the first GitHub refuses: that repository is the cause. A probe that answers
// neither a mint nor a refusal of the scope — an upstream answer, a rate limit, the budget gone —
// ends the search, as does a search that finds no repository GitHub refuses, and the refusal then
// says the cause was not found and names the whole scope. The request is refused either way:
// GitHub's verdict mint already said so, and only the reason the caller reads was left to find.
func (m *Machine) narrowingCause(ctx context.Context, mint Minter, name string, served policy.Secret, value string, scope *secrets.GitHubScope) error {
	if len(scope.Repositories) == 1 {
		return narrowingRepositoryRefusal(name, scope.Repositories[0], scope.Accounts[0])
	}
	for i, repository := range scope.Repositories {
		if i == narrowingCauseProbes {
			return narrowingCauseNotFound(name, scope)
		}
		probe := *scope
		probe.Repositories = []string{repository}
		switch err := m.checkMint(ctx, mint, name, served, value, &probe); {
		case errors.Is(err, secrets.ErrMintNarrowingRefused):
			return narrowingRepositoryRefusal(name, repository, scope.Accounts[0])
		case err != nil:
			return narrowingCauseNotFound(name, scope)
		}
	}
	return narrowingCauseNotFound(name, scope)
}

// narrowingCauseProbes bounds the per-repository probes one cause search makes. Each probe is a
// whole mint — an installations listing and a token request — and a scope may name up to
// githubname.MaxNarrowedRepositories of them, so an unbounded search would ask GitHub a hundred
// times for a reason the caller is refused for either way. Past this many the search answers
// narrowingCauseNotFound, as it does for the budget, and that refusal still names every
// repository of the scope, which is what a caller of such a profile has to read anyway. The
// budget usually ends the search first; this bound is what holds for a deployment that raises
// the budget, so no one request can drive the App into the rate limit the per-enrollment bound
// exists to prevent.
const narrowingCauseProbes = 5

// narrowingRepositoryRefusal is the refusal of a profile naming a repository the App's
// installation on the account does not reach: the one the verdict mint answered for a scope of a
// single repository, and the one a probe answered for a scope of more.
func narrowingRepositoryRefusal(name, repository, account string) error {
	return fmt.Errorf("%w: %s: the profile names the repository %s, which the App's installation on %s does not reach", secrets.ErrMintNarrowingRefused, name, repository, account)
}

// narrowingCauseNotFound is the refusal of a scope GitHub refused whose cause the search did not
// find: it ran out of budget, met an answer that was no verdict, or found every repository of the
// scope served on its own. It names the whole scope instead of a part, so the caller reads what
// the broker asked GitHub for.
func narrowingCauseNotFound(name string, scope *secrets.GitHubScope) error {
	return fmt.Errorf("%w: %s: GitHub refused the profile's token (%s), and asking again with fewer repositories did not find which one", secrets.ErrMintNarrowingRefused, name, scopeWords(scope))
}

// scopeWords is a scope as one phrase of a refusal: the accounts and the repositories it names.
// It holds nothing but the profile's own words, so no stored value and no token can reach a
// refusal through it.
func scopeWords(scope *secrets.GitHubScope) string {
	words := "accounts " + strings.Join(scope.Accounts, ", ")
	if len(scope.Repositories) > 0 {
		words += ", repositories " + strings.Join(scope.Repositories, ", ")
	}
	return words
}

// refusedBeforeApproval logs one refusal this check made, at WARN and under the message a
// release's refused scope is logged with (NarrowingRefusedMessage), with no grant= key: no grant
// exists, and the refusal is why none will.
func refusedBeforeApproval(enrollmentID, name string, err error) {
	slog.Warn(NarrowingRefusedMessage, "enrollment", enrollmentID, "name", name, "error", err)
}

// skippedNarrowingCheck logs the trouble that ended a check and let its request through
// (NarrowingCheckSkippedMessage): the enrollment, the profile the check had reached, empty where
// it had reached none, why it ended, and the failure itself where there was one. A reason with no
// failure carries no error= key at all, rather than an empty one a reader would go looking for.
func skippedNarrowingCheck(enrollmentID, name, reason string, err error) {
	attributes := []any{"enrollment", enrollmentID, "name", name, "reason", reason}
	if err != nil {
		attributes = append(attributes, "error", err)
	}
	slog.Info(NarrowingCheckSkippedMessage, attributes...)
}
