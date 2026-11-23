package secrets

import (
	"errors"
	"fmt"
	"time"
)

// MintRequest is one release's input to a minter: the secret's stored value, its name, the name
// of the stored secret that value was read from, its type and whether the policy snapshot checked
// for this release holds it as human tier.
// Name is what the session asked for: a profile's name, or a stored secret's own.
// Source is the policy name of the stored secret the value was read from: a profile's source, and
// Name itself for a secret served by its own name. The deployment's settings that bind a minted
// type name stored secrets (BROKER_SERVICE_ACCOUNT_SECRETS, BROKER_AZURE_LOGIN_SECRET), so the
// google-service-account minter looks its binding up by Source, and the Azure minter requires
// Source to be its configured secret, with HumanTier true: a profile of that secret mints under
// its source's binding, whatever its own name.
// Type is the secret's type tag, which the HTTP minters count their attempts under
// (broker_mint_attempts_total).
// GitHub is a github-app profile's scope in effect for this release, which only the github-app
// minter reads and every other type leaves nil; that minter refuses a request without one, since
// every GitHub App token the broker mints is a profile's (policy.ReasonGitHubAppNoProfile).
// GoogleScopes is a google-service-account secret's scopes as the whole OAuth scope URLs
// policy.Secret.ScopeURLs answers (a profile's google.scopes, or else its source's scopes tag),
// and nil both for such a secret that names no scopes and for every other type; the
// service-account minter then asks Google for the cloud-platform scope alone, which is what it
// asked for before the tag existed.
type MintRequest struct {
	Name         string
	Source       string
	Type         string
	HumanTier    bool
	Value        string
	GoogleScopes []string
	GitHub       *GitHubScope

	// Check asks the minter whether it could serve this request, without serving it; only the
	// github-app minter reads it. A check mints exactly what a release would mint, by the same
	// calls in the same order but for a token request that may have reached GitHub and failed,
	// which it does not send again where a release does, so such a request that GitHub served
	// leaves one token live at GitHub, which nothing can name, until it lapses; a token request
	// known to have failed before a connection (a name that did not resolve, a failed dial, a
	// proxy's refused tunnel, a certificate the broker did not trust) is sent again. It releases no
	// token: it sends GitHub the revoke of every token GitHub answers it with as soon as GitHub
	// answers, among them a token whose answer it cannot use (cut off, past its cap, undecodable,
	// with no expiry) wherever the part that arrived names it, and answers Minted{} with the error
	// the release would have answered, nil where the release would have succeeded. The revoke of a
	// whole answer's token runs under the caller's context; the revoke of a token whose answer it
	// cannot use runs to a bound of its own, as a release's does, since the check's own bound is the
	// likeliest thing to have cut that answer off. A revoke GitHub does not confirm is logged and
	// leaves its token live until it lapses. So a caller learns the verdict before it spends
	// anything on the release itself.
	Check bool
}

// named is how a minter's refusal names the secret a release is for: its name, and, for a profile
// of another name, the stored secret it is a profile of, whose name the deployment binds.
func (r MintRequest) named() string {
	if r.Source == r.Name {
		return r.Name
	}
	return r.Name + ", a profile of " + r.Source
}

// GitHubScope is a profile's scope in effect for one release: the target's when the request
// carries one, else the profile's.
type GitHubScope struct {
	// Accounts are the lowercase logins of the accounts whose installations the release mints
	// for, each once.
	Accounts []string
	// Repositories are "account/name" entries, the account lowercased, sorted, at most
	// githubname.MaxNarrowedRepositories of them; nil is every repository the installation
	// reaches. Each account's token names that account's repositories alone, by name
	// (GitHubApp.Mint), so a scope that names repositories names some of each account in effect.
	Repositories []string
	// Permissions are the profile's, before the cut: each GitHub App permission and its level.
	Permissions map[string]string
	// Value is githubname.ValueInstallations or githubname.ValueToken: today's JSON of one entry
	// per account, or the bare token of the one account in effect.
	Value string
}

// Minted is one minted token: the value a release hands over, and when it stops working, zero
// when the type's upstream does not say. The airlock's proxy mints again five minutes before
// Expires, so no call starts on a token about to end. LeftOut is, for each account a github-app
// profile's token was minted for, every entry the cut to what its installation holds left out of
// the profile's permissions, sorted: "<permission>" for a permission the installation holds none
// of, and "<permission>:<level>" for one it holds at a lower level, which was asked for in place
// of the higher one. It is nil when the cut left nothing out, and for every other type.
type Minted struct {
	Value   string
	Expires time.Time
	LeftOut map[string][]string
}

// The classes of a minter's failure. Every error a minter returns wraps exactly one, so the
// broker's log names the class an operator acts on without reading the message, which never
// holds the stored value or a token.
var (
	// ErrMintStoredValue: the secret's stored value cannot be minted from (it does not decode, its
	// key does not parse, it names no App id). Its owner stores a new value.
	ErrMintStoredValue = errors.New("the stored value cannot be minted from")
	// ErrMintRefused: the minter refuses the release (an Azure login that is not the configured
	// human-tier secret, a github-app release that carries no profile's scope), or GitHub or
	// Google refused (a 4xx other than GitHub's 422 to a profile's token, a Google login that needs
	// a new sign-in, an App with no installation on an account the scope names, a 401 to the
	// App's own JWT). A refusal of the profile's scope is ErrMintNarrowingRefused, and the one
	// refusal that is about the account alone is ErrMintNoInstallation.
	ErrMintRefused = errors.New("the upstream refused the mint")
	// ErrMintNarrowingRefused: the profile's scope in effect for the release cannot be served: its
	// permissions, cut to what an installation holds, leave none but metadata (ErrMintEmptyCut); it
	// names repositories, and an account in effect holds none of them; it is a bare token of more
	// than one installation (ErrMintSeveralInstallations); or GitHub answered its token request
	// 422, which it does for a repository the installation does not reach (a misspelled name among
	// them) or a permission it does not hold. The caller names another profile or a target, or
	// the profiles file changes; neither the App nor the secret is at fault. Its name and its
	// words are the ones it had for the narrowing a caller once asked for, which a released client
	// still matches (packages/daemon's appauth).
	ErrMintNarrowingRefused = errors.New("the narrowing cannot be served")
	// ErrMintUpstream: GitHub or Google could not be asked or answered no usable reply (the network,
	// a 5xx, a rate limit, a body past its cap or one that does not decode, a token GitHub describes
	// as holding more than its request asked, which GitHubApp.Mint revokes at GitHub before it
	// refuses), or the broker's Azure projected token is unreadable, malformed or expired.
	// The broker's operator looks at the projected volume, egress and the upstream's status.
	ErrMintUpstream = errors.New("the upstream could not be asked")
	// ErrMintNotBound: the google-service-account secret this release names is not bound to the
	// account its stored value holds — the deployment's BROKER_SERVICE_ACCOUNT_SECRETS names
	// another account for this secret, or names the secret not at all. Unlike ErrMintStoredValue,
	// which the secret's owner fixes by storing a new value, this one is the deployment
	// operator's to fix in that setting, which is why it is a class of its own; the mint is
	// refused before any credential is resolved and before any call reaches Google.
	ErrMintNotBound = errors.New("the deployment binds no such account to this secret")
)

// ErrMintEmptyCut is the refusal of a profile whose permissions, cut to what an installation holds,
// leave none but metadata where it names more (GitHubApp.Mint's emptyCut): GitHub answers a token
// request naming no permissions with every permission the installation holds, and grants metadata
// with every token, which every installation it lists holds, so a token of metadata alone does
// nothing the profile is for; the minter asks GitHub for no token at all. It is no class of its
// own: it wraps ErrMintNarrowingRefused, which MintErrorClass reads, and it exists because the
// broker's check before an approval must tell it from GitHub's 422: a search for the one
// repository GitHub refused would read an empty cut, which every probe meets again, as the first
// repository it asks about (requests' narrowingCause).
var ErrMintEmptyCut = fmt.Errorf("%w: the profile's permissions, cut to what the installation holds, leave none but metadata", ErrMintNarrowingRefused)

// ErrMintSeveralInstallations is the refusal of a value-token profile whose one account the App
// has more than one installation on, logins that differ only in case, which GitHub compares as
// one: the release hands over one installation's bare token, and a token minted for each would
// leave all but that one live with nobody holding it, so the minter asks GitHub for no token at
// all (GitHubApp.Mint). It is no class of its own: it wraps ErrMintNarrowingRefused, as
// ErrMintEmptyCut does, and for the same reason: the broker's check before an approval must tell
// it from GitHub's 422, since a search for the repository GitHub refused would meet it at every
// probe and blame the first repository it asks about (requests' narrowingCause).
var ErrMintSeveralInstallations = fmt.Errorf("%w: the profile releases one account's bare token, and the App has more than one installation on that account", ErrMintNarrowingRefused)

// ErrMintNoInstallation is the one refusal of GitHub's that is about the account a release is
// for: the App has no installation on any account the profile's scope names (GitHubApp.Mint). It
// is no class of its own: it wraps ErrMintRefused, so MintErrorClass still reads "refused" and a
// release still answers it as the mint failure it is.
//
// It exists because the broker's check before an approval must tell this refusal from every
// other 4xx GitHub answers (mintStatusError.Unwrap wraps them all in ErrMintRefused): a 401 to
// the App's JWT, a 403 that is no rate limit and a 404 on the token route say nothing about the
// scope the caller asked for, and a check that read them as a refusal would tell the caller to
// fix a scope that is not at fault. Its own text is ErrMintRefused's, which it wraps, so a
// refusal naming it reads word for word as it did when it named the class alone: a released
// client matches "has no installation on" in those words (packages/daemon's appauth).
var ErrMintNoInstallation = fmt.Errorf("%w", ErrMintRefused)

// MintErrorClass names the class err wraps, for the broker's log line: "stored-value",
// "refused", "narrowing", "binding", "upstream", or "unknown" for an error that wraps none.
func MintErrorClass(err error) string {
	switch {
	case errors.Is(err, ErrMintStoredValue):
		return "stored-value"
	case errors.Is(err, ErrMintNarrowingRefused):
		return "narrowing"
	case errors.Is(err, ErrMintRefused):
		return "refused"
	case errors.Is(err, ErrMintUpstream):
		return "upstream"
	case errors.Is(err, ErrMintNotBound):
		return "binding"
	default:
		return "unknown"
	}
}
