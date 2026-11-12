// packages/envoy/internal/broker/requests/narrowing_check.go
// narrowing_check.go is the verdict Machine.Create reaches on a GitHub App narrowing before it
// writes a row or asks anybody to approve the request, so a narrowing the broker cannot serve
// costs nobody an approval. The same verdict is made again at release (Machine.Values and
// Machine.narrowingRefusal, machine_read.go), which is where it stays for a grant that is
// released a second time; this file is only about reaching it earlier. It is a file of its own
// because machine_state.go, which holds Create, is long already and nothing here is a state
// transition: the check reads, asks GitHub and decides, and writes nothing at all.
package requests

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// NarrowingCheckSkippedMessage is logged, at INFO, each time trouble — never a refusal — ended
// Create's check of a narrowing, followed by enrollment=, name=<the secret the check had reached,
// empty where it had reached none>, reason=<why it ended> and, where something failed, error=,
// which never holds a stored value or a token. Such a request takes the path it took before the
// check existed: the person is asked, and the release makes the refusal the check did not. It is
// INFO rather than WARN because nothing is wrong with the request, and it is a line of its own so
// that an operator can read how often GitHub, the secrets store or the check's bounds leave the
// verdict unreached without reading it as a failed mint.
const NarrowingCheckSkippedMessage = "broker: the narrowing check was skipped"

// DefaultNarrowingCheckBudget bounds the whole check of one request — its github-app secrets and
// the search for the part GitHub refused together — and DefaultNarrowingCheckMint one mint of
// that check and that mint's revoke, each derived from what is left of the budget.
//
// The two numbers come from the clocks around Create. `agent-secrets` gives one call 30 seconds
// (leasetime.RenewClientTimeout) and the broker bounds every request at 45 seconds
// (requestDeadline, cmd/broker/main.go), and Create does its policy work and its whole
// transaction inside that same call, so the check takes at most a third of the client's budget.
// A caller that gave up before the broker wrote its pending request is the cost this check
// removes, so the check must end well inside what the caller is waiting out.
const (
	DefaultNarrowingCheckBudget = 10 * time.Second
	DefaultNarrowingCheckMint   = 5 * time.Second
)

// DefaultNarrowingChecks bounds, per enrollment, the checks that reach GitHub: five at once,
// refilled one every twelve seconds. A refused request writes no row, so the guards inside
// Create's transaction (lockPendingNames, pendingRequest) never see it, and a caller in a loop
// could otherwise drive the App into a rate limit at GitHub, which would turn the check off for
// everybody. Five lets a caller correct its flags a few times in a minute; past it the GitHub
// part is skipped and the request goes to its approver, which is what it did before this check
// existed. The budget is per process, as DefaultMissRereads' is, and for the same reason: it
// bounds load rather than authority.
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

// checkNarrowing is Create's whole verdict on the narrowing the caller will release this
// request's grant with: the refusal Create answers, and nil both for a narrowing the broker could
// serve and for one it could not decide. names are the names the request asks for, set the
// snapshot Create evaluates them against and reason the request object's own reason, all three as
// Create holds them. It runs after reuseLiveGrant, which asks nobody for a grant that already
// exists, and before the transaction, so a refusal writes no request row, makes no
// credential-request record and interrupts no person; it also holds no row lock while it waits on
// GitHub, which is the rule rereadMissing already states for the policy reread above it.
//
// The whole check runs only where the policy would send this request to a person, which is the
// only case that spends an approval on a narrowing the broker will not serve. To learn that, it
// reads the requester on the pool and evaluates the policy once more for this one decision. That
// read races the transaction's own read of the same rows — a name withheld, or an operator
// changed, in between — and the race is harmless: it decides nothing but whether to check, and
// the transaction decides the request itself as it always did. An error from either read, which
// includes policy.ErrUnknownSecret and ErrMixedApprovers, skips the check and changes no answer
// for the same reason: the transaction answers those in a moment, in the order a caller reads
// them today. A state that is not pending skips it too, so an agent-tier narrowed request and the
// Legion daemon's own token command keep today's behaviour exactly and reach GitHub for nothing.
func (m *Machine) checkNarrowing(ctx context.Context, enr enrollmentRow, names []string, set *policy.Set, reason string, narrowing *secrets.GitHubNarrowing) error {
	requester, err := m.requester(ctx, m.Store.Pool, enr)
	if err != nil {
		skippedNarrowingCheck(enr.ID, "", "the requester could not be read", err)
		return nil
	}
	e, err := evaluate(set, names, requester, reason)
	if err != nil || e.state != "pending" {
		return nil
	}
	// A decision carries exactly what a requestedSecret holds, so the rule that refuses a
	// release's narrowing refuses this request's: one function for both, never a second copy of
	// the rule that could answer one of them differently.
	requested := make([]requestedSecret, 0, len(e.decisions))
	for _, d := range e.decisions {
		requested = append(requested, requestedSecret{name: d.Name, source: d.Source, decision: d.Decision, airlockDigest: d.AirlockDigest})
	}
	if name, err := m.narrowingRefusal(set, requested, narrowing); err != nil {
		refusedBeforeApproval(enr.ID, name, err)
		return err
	}
	mint := m.Minters[policy.KindGitHubApp]
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
	// Every github-app name of the request in turn, since the release mints for each of them and
	// one refusal refuses the release. The first refusal ends the check: the caller has its
	// answer, and the names after it would be asked about for nothing.
	for _, g := range requested {
		kind, source, minted, err := policy.ParseKindSource(g.source)
		if err != nil || !minted || kind != policy.KindGitHubApp {
			continue
		}
		checked, refusal := m.checkNarrowingFor(budget, mint, enr.ID, g.name, source, set.Secrets[g.name], narrowing)
		if !checked {
			return nil
		}
		if refusal != nil {
			refusedBeforeApproval(enr.ID, g.name, refusal)
			return refusal
		}
	}
	return nil
}

// checkNarrowingFor is GitHub's own verdict on narrowing for one github-app name: checked is true
// with no refusal where GitHub would serve the release, and true with the refusal Create answers
// where it would not. checked is false where trouble ended the check instead — a stored value
// that could not be read or that the minter cannot mint from, an answer that is neither a token
// nor a refusal of the narrowing, the check's own time gone — which this logs
// (NarrowingCheckSkippedMessage, one reason= of its own for each) and which leaves Create on the
// path it took before the check existed. That last rule is a requirement of its own: an outage at
// GitHub must not stop every GitHub App request of the fleet, which is a larger failure than the
// spent approval this check saves.
func (m *Machine) checkNarrowingFor(ctx context.Context, mint Minter, enrollmentID, name, source string, served policy.Secret, narrowing *secrets.GitHubNarrowing) (checked bool, refusal error) {
	stored, err := m.Secrets.Read(ctx, source)
	if err != nil {
		skippedNarrowingCheck(enrollmentID, name, "the stored value could not be read", err)
		return false, nil
	}
	// The shape rule is spent here as the release spends it (Machine.release), and writes no
	// line: the release logs TrailingLineEndingRemovedMessage once for each value the rule
	// changed, and a check that logged it too would write that line twice for one release.
	value, _ := policy.TrimForShape(stored, served.Shape)
	switch err := m.checkNarrowingMint(ctx, mint, name, served, value, narrowing); {
	case err == nil:
		return true, nil
	case errors.Is(err, secrets.ErrMintNarrowingRefused):
		return true, m.narrowingCause(ctx, mint, name, served, value, narrowing)
	case errors.Is(err, secrets.ErrMintRefused):
		// The App has no installation on the account the narrowing names, which no narrowing of
		// that account can mend and which the release refuses too. The minter's own words hold
		// "has no installation on", which a released client matches in this broker's answer
		// (appauth's brokerNoInstallation, packages/daemon), so they are carried whole and the
		// secret's name is put in front of them.
		return true, fmt.Errorf("%s: %w", name, err)
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		// The check's own budget, a mint's own bound or a caller that went away ended it before
		// the mint asked GitHub anything (checkNarrowingMint answers ctx.Err() first of all).
		// GitHub said nothing here, so this is not logged as an answer of GitHub's: an operator
		// reading a run of these raises the bounds or looks at what is slowing Create down.
		skippedNarrowingCheck(enrollmentID, name, "the check's time ran out, or its caller went away, before GitHub was asked", err)
		return false, nil
	case errors.Is(err, secrets.ErrMintStoredValue):
		// The stored value was read and the minter cannot mint from it: it is no JSON object of
		// an app_id and a private_key, or the key does not parse. The secret's owner stores a
		// new value, and the release says the same thing again. GitHub said nothing here
		// either, and the error holds no part of the value (secrets' GitHubApp.Mint).
		skippedNarrowingCheck(enrollmentID, name, "the stored value is not an App id and private key the minter can read", err)
		return false, nil
	default:
		skippedNarrowingCheck(enrollmentID, name, "GitHub's answer decided nothing about the narrowing", err)
		return false, nil
	}
}

// checkNarrowingMint is one mint of the check: the calls the release makes, in the same
// order, with every token GitHub answers revoked at once and none released, answering the error
// the release would have answered (secrets.MintRequest.Check). The request is built from the
// snapshot this request is being evaluated against, as Machine.release builds one, so the check
// and the release ask GitHub the same thing about the same secret.
//
// Each mint, and the revoke inside it, is bounded by NarrowingCheckMint and by what is left of
// the check's whole budget, whichever ends first: ctx already carries the budget, so deriving the
// mint's own bound from it takes the shorter of the two. A budget already spent is answered
// without asking GitHub anything, and reads as trouble to the caller, which lets the request
// through.
func (m *Machine) checkNarrowingMint(ctx context.Context, mint Minter, name string, served policy.Secret, value string, narrowing *secrets.GitHubNarrowing) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, m.narrowingCheckMintBound())
	defer cancel()
	_, err := mint(ctx, secrets.MintRequest{
		Name: name, HumanTier: served.Tier == policy.TierHuman, Value: value,
		GitHubOwners: served.GitHubOwnerLogins(), GitHubNarrowing: narrowing, Check: true,
	})
	return err
}

// narrowingCause is which part of the narrowing GitHub refused, as the refusal Create answers.
// GitHub answers 422 and the broker never quotes its text (secrets' GitHubNarrowing.refusal), so
// the only way to learn the part is to ask again with less of the narrowing, each probe another
// check mint whose token is revoked the moment GitHub answers it.
//
// A read-only narrowing that names repositories is asked for first without them: refused, the
// read permissions are the cause and no repository is probed; minted, the permissions are held
// and a repository is the cause. A read-only narrowing that names no repository needs no such
// probe, the verdict mint having been exactly it. A narrowing that is not read-only names no
// permissions to be refused and so makes no such probe at all: it could only pass, and it would
// mint a token for the whole installation to learn nothing.
//
// Then the repositories, where the verdict mint has not already answered for them. A narrowing
// that names exactly one repository needs no probe at all: the verdict mint asked for that one
// repository and GitHub refused it, and for a read-only narrowing the permissions probe above
// has just held the other half, so that repository is the cause. A narrowing that names more is
// probed one repository at a time, each naming that repository alone, in the narrowing's own
// order, stopping at the first GitHub refuses: that repository is the cause. A probe that
// answers neither a mint nor a refusal of the narrowing — an upstream answer, a rate limit, the
// budget gone — ends the search, as does a search that finds no part GitHub refuses, and the
// refusal then says the cause was not found and names the whole narrowing the caller asked for.
// The request is refused either way: GitHub's verdict mint already said so, and only the reason
// the caller reads was left to find.
func (m *Machine) narrowingCause(ctx context.Context, mint Minter, name string, served policy.Secret, value string, narrowing *secrets.GitHubNarrowing) error {
	if narrowing.ReadOnly {
		if len(narrowing.Repositories) == 0 {
			return narrowingPermissionsRefusal(name, narrowing)
		}
		probe := &secrets.GitHubNarrowing{Owner: narrowing.Owner, ReadOnly: true}
		switch err := m.checkNarrowingMint(ctx, mint, name, served, value, probe); {
		case errors.Is(err, secrets.ErrMintNarrowingRefused):
			return narrowingPermissionsRefusal(name, narrowing)
		case err != nil:
			return narrowingCauseNotFound(name, narrowing)
		}
	}
	if len(narrowing.Repositories) == 1 {
		return narrowingRepositoryRefusal(name, narrowing.Repositories[0], narrowing)
	}
	for i, repository := range narrowing.Repositories {
		if i == narrowingCauseProbes {
			return narrowingCauseNotFound(name, narrowing)
		}
		probe := &secrets.GitHubNarrowing{Owner: narrowing.Owner, Repositories: []string{repository}, ReadOnly: narrowing.ReadOnly}
		switch err := m.checkNarrowingMint(ctx, mint, name, served, value, probe); {
		case errors.Is(err, secrets.ErrMintNarrowingRefused):
			return narrowingRepositoryRefusal(name, repository, narrowing)
		case err != nil:
			return narrowingCauseNotFound(name, narrowing)
		}
	}
	return narrowingCauseNotFound(name, narrowing)
}

// narrowingCauseProbes bounds the per-repository probes one cause search makes. Each probe is a
// whole mint — an installations listing, a token request, and for a read-only narrowing a second
// token request without actions — and a narrowing may name up to secrets.MaxNarrowedRepositories
// of them, so an unbounded search would ask GitHub a hundred times for a reason the caller is
// refused for either way. Past this many the search answers narrowingCauseNotFound, as it does
// for the budget, and that refusal still names every repository the caller asked for, which is
// what a caller of such a narrowing has to read anyway. The budget usually ends the search first;
// this bound is what holds for a deployment that raises the budget, so no one request can drive
// the App into the rate limit the per-enrollment bound exists to prevent.
const narrowingCauseProbes = 5

// narrowingRepositoryRefusal is the refusal of a narrowing naming a repository the App's
// installation on the account does not reach: the one the verdict mint answered for a narrowing
// of a single repository, and the one a probe answered for a narrowing of more.
func narrowingRepositoryRefusal(name, repository string, narrowing *secrets.GitHubNarrowing) error {
	return fmt.Errorf("%w: %s: the narrowing names the repository %s, which the App's installation on %s does not reach", secrets.ErrMintNarrowingRefused, name, repository, narrowing.Owner)
}

// narrowingPermissionsRefusal is the refusal of a read-only narrowing whose read permissions
// GitHub will not grant: the App's installation holds fewer than contents, metadata and pull
// requests, the three a read-only token request always asks for, the fourth (actions) having been
// asked for again without already (secrets' optionalReadOnlyPermission).
func narrowingPermissionsRefusal(name string, narrowing *secrets.GitHubNarrowing) error {
	return fmt.Errorf("%w: %s: the App's installation on %s does not hold the read access the narrowing asks for: contents, metadata and pull requests", secrets.ErrMintNarrowingRefused, name, narrowing.Owner)
}

// narrowingCauseNotFound is the refusal of a narrowing GitHub refused whose cause the search did
// not find: it ran out of budget, met an answer that was no verdict, or found every part of the
// narrowing served on its own. It names the whole narrowing instead of a part, so the caller
// reads what the broker asked GitHub for and can ask for less of it.
func narrowingCauseNotFound(name string, narrowing *secrets.GitHubNarrowing) error {
	return fmt.Errorf("%w: %s: GitHub refused the narrowing (%s), and asking again with less of it did not find which part; the caller fixes the narrowing", secrets.ErrMintNarrowingRefused, name, narrowingWords(narrowing))
}

// narrowingWords is a narrowing as one phrase of a refusal: the account, the repositories it
// names and whether it asked for read access alone. It holds nothing but what the caller itself
// sent, so no stored value and no token can reach a refusal through it.
func narrowingWords(narrowing *secrets.GitHubNarrowing) string {
	words := "owner " + narrowing.Owner
	if len(narrowing.Repositories) > 0 {
		words += ", repositories " + strings.Join(narrowing.Repositories, ", ")
	}
	if narrowing.ReadOnly {
		words += ", read-only"
	}
	return words
}

// refusedBeforeApproval logs one refusal this check made, at WARN and under the message a
// release's refused narrowing is logged with (NarrowingRefusedMessage), with no grant= key: no
// grant exists, and the refusal is why none will. name is empty for the one refusal that is about
// no single secret (ErrNarrowingNotApplicable, which is about the request as a whole).
func refusedBeforeApproval(enrollmentID, name string, err error) {
	slog.Warn(NarrowingRefusedMessage, "enrollment", enrollmentID, "name", name, "error", err)
}

// skippedNarrowingCheck logs the trouble that ended a check and let its request through
// (NarrowingCheckSkippedMessage): the enrollment, the secret the check had reached, empty where
// it had reached none, why it ended, and the failure itself where there was one. A reason with no
// failure carries no error= key at all, rather than an empty one a reader would go looking for.
func skippedNarrowingCheck(enrollmentID, name, reason string, err error) {
	attributes := []any{"enrollment", enrollmentID, "name", name, "reason", reason}
	if err != nil {
		attributes = append(attributes, "error", err)
	}
	slog.Info(NarrowingCheckSkippedMessage, attributes...)
}
