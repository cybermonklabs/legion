package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
)

// MintedTokenMargin: the 20-minute margin is the stored token's own remaining life, so a caller never gets a token about to expire; it does not bound the grant; GitHub fixes a token's life at one hour, so a token can outlive its grant by up to an hour; no revocation is added.
//
// A release refuses a token whose expires_at is not later than now plus the margin
// (ErrMintTokenStale). It answers the earliest of its tokens' expires_at less the margin as when its
// value stops being one to hand over (Minted.Expires), which the airlock proxy reads the grant
// again five minutes before, so the proxy too carries no call on a token about to expire.
//
// Twenty minutes, for two reasons. A caller must be able to rely on the token it is handed for
// that long: the Legion daemon serves a cached installation token until twenty minutes before its
// expiry (packages/daemon's appauth refreshWindow), so a token handed over with less is one it asks
// again for at once. And a minter that runs every ten minutes, on tokens GitHub gives an hour,
// leaves a stored token at least fifty minutes of life, so the margin lets three of its runs in a
// row fail before any release is refused: the token written last is served until forty minutes
// after it was minted.
const MintedTokenMargin = 20 * time.Minute

// ErrMintStoredWider is the refusal of a stored installation token whose permissions are past
// the profile's: one the profile does not name, or one at a level above the one it names. A token
// can be cut no further once it is minted, so the broker hands over no token wider than the
// profile it serves, whatever the minter minted. It wraps ErrMintNarrowingRefused, as
// ErrMintEmptyCut does: the profile cannot be served from this secret's tokens, and the profiles
// file or the minter's permission set fixes it; neither is the secret's own fault, so the release
// does not refuse the secret's value (policy.Current.RefuseValue), and every other profile of the
// same secret is still served.
var ErrMintStoredWider = fmt.Errorf("%w: a stored installation token holds more than the profile's permissions", ErrMintNarrowingRefused)

// ErrMintStoredRepositories is the refusal of a stored installation token whose repositories are
// not the profile's: a token its minter narrowed to repositories (repository_selection "selected")
// serves only a profile that names exactly those, since a profile naming fewer would be handed a
// token that reaches more and a profile naming none would claim every repository of the
// installation; and a token of every repository serves only a profile that names none. A token
// cannot be cut once it is minted, so the broker refuses rather than widen what a profile says.
// It wraps ErrMintNarrowingRefused, as ErrMintStoredWider does, and for the same reasons.
var ErrMintStoredRepositories = fmt.Errorf("%w: the repositories a stored installation token reaches are not the profile's", ErrMintNarrowingRefused)

// ErrMintTokenStale is the refusal of a stored installation token whose expires_at is not later
// than now plus MintedTokenMargin: its minter has not written a newer one in time. It wraps
// ErrMintUpstream, the class of an upstream that gave no usable answer, here the minter: the
// broker's operator looks at the minter, a release answers MINT_FAILED, the airlock proxy answers
// a call it cannot carry for it 503 (make the call again), and the minter's next write of the
// secret serves again.
var ErrMintTokenStale = fmt.Errorf("%w: a stored installation token is within the margin of its expiry", ErrMintUpstream)

// GitHubInstallationsValue is a github-installations secret's value as ParseValue decodes it:
// each installation token its minter stored, keyed by the account's login lowercased, as GitHub
// compares logins and as a profile names its accounts.
type GitHubInstallationsValue struct {
	Installations map[string]StoredInstallation
}

// RepositoryScopeAllExceptProtected is the one value a stored entry's repository_scope takes: the
// seed minter's marker of a token it cut to every repository of the installation but the protected
// ones, stored beside repository_selection "selected" and no repositories list. Only the seed
// minter writes it, into a secret only the seed's own route may write, so Cipher trusts it for the
// list the token was cut to and serves the token to a profile that names no repositories
// (repositoriesMismatch). Cipher does not prove the cut: the deployment's own checks of the admin
// token do.
const RepositoryScopeAllExceptProtected = "all-except-protected"

// StoredInstallation is one installation token a minter stored: the account's login as the minter
// wrote it, the installation, the token, when GitHub lapses it, the permissions GitHub minted it
// with (each permission and its level), and the repositories it reaches: RepositorySelection "all"
// for every repository of the installation, or "selected", and then Repositories, where the minter
// listed them, each "account/name" lowercased, as GitHub compares names, sorted; nil where it
// listed none. AllExceptProtected is whether the entry carries the seed minter's marker
// (RepositoryScopeAllExceptProtected), which stands beside "selected" and no list alone.
type StoredInstallation struct {
	Account             string
	InstallationID      int64
	Token               string
	ExpiresAt           time.Time
	Permissions         map[string]string
	RepositorySelection string
	Repositories        []string
	AllExceptProtected  bool
}

// githubInstallationsSecret and githubStoredInstallation are a github-installations secret's
// value as it is stored, which parseGitHubInstallations decodes strictly.
type (
	githubInstallationsSecret struct {
		Installations map[string]githubStoredInstallation `json:"installations"`
	}
	githubStoredInstallation struct {
		InstallationID      int64             `json:"installation_id"`
		Token               string            `json:"token"`
		ExpiresAt           time.Time         `json:"expires_at"`
		Permissions         map[string]string `json:"permissions"`
		RepositorySelection string            `json:"repository_selection"`
		Repositories        []string          `json:"repositories"`
		RepositoryScope     string            `json:"repository_scope"`
	}
)

// parseGitHubInstallations is ParseValue for a github-installations value: one JSON object and
// nothing after it, holding installations and no other field, at least one; each keyed by a GitHub
// login, no login twice whatever its case, and each of exactly installation_id (positive), token
// (printable ASCII with no space, which no header it is sent in can be broken by), expires_at (an
// RFC 3339 time), permissions (at least one, each a GitHub App permission's name at a level GitHub
// gives), repository_selection ("all" or "selected") and, beside "selected" alone, repositories
// (storedRepositories) or repository_scope (RepositoryScopeAllExceptProtected, beside no list). No
// error names a token; an account is named only once it is read as a login.
func parseGitHubInstallations(value string) (GitHubInstallationsValue, error) {
	refuse := func(why string) (GitHubInstallationsValue, error) {
		return GitHubInstallationsValue{}, fmt.Errorf("%w: GitHub installation tokens: %s", ErrMintStoredValue, why)
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	var secret githubInstallationsSecret
	if decoder.Decode(&secret) != nil {
		return refuse("not a JSON object of installations, each of installation_id, token, expires_at, permissions and repository_selection, and of no other field")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return refuse("something follows the one JSON object")
	}
	if len(secret.Installations) == 0 {
		return refuse("it names no installation")
	}
	read := GitHubInstallationsValue{Installations: make(map[string]StoredInstallation, len(secret.Installations))}
	for _, key := range slices.Sorted(maps.Keys(secret.Installations)) {
		if !githubname.ValidLogin(key) {
			return refuse("an installation's key is no GitHub login")
		}
		account := strings.ToLower(key)
		if _, twice := read.Installations[account]; twice {
			return refuse(fmt.Sprintf("it names the account %s twice: GitHub compares logins without regard to case", account))
		}
		entry := secret.Installations[key]
		switch {
		case entry.InstallationID <= 0:
			return refuse(account + ": installation_id is no installation's id")
		case !installationToken(entry.Token):
			return refuse(account + ": token is empty, or holds a space or a character outside printable ASCII")
		case entry.ExpiresAt.IsZero():
			return refuse(account + ": expires_at names no time")
		case len(entry.Permissions) == 0:
			return refuse(account + ": permissions names none")
		case entry.RepositorySelection != "all" && entry.RepositorySelection != "selected":
			return refuse(account + `: repository_selection is neither "all" nor "selected"`)
		}
		for _, permission := range slices.Sorted(maps.Keys(entry.Permissions)) {
			if !githubname.ValidPermissionName(permission) || githubname.LevelRank(entry.Permissions[permission]) == 0 {
				return refuse(account + ": permissions holds an entry that is no GitHub App permission at a level GitHub gives")
			}
		}
		repositories, why := storedRepositories(account, entry)
		if why == "" {
			why = storedScope(entry)
		}
		if why != "" {
			return refuse(account + ": " + why)
		}
		read.Installations[account] = StoredInstallation{Account: key, InstallationID: entry.InstallationID, Token: entry.Token,
			ExpiresAt: entry.ExpiresAt, Permissions: entry.Permissions, RepositorySelection: entry.RepositorySelection, Repositories: repositories,
			AllExceptProtected: entry.RepositoryScope == RepositoryScopeAllExceptProtected}
	}
	return read, nil
}

// storedRepositories reads the repositories of the stored token of account, a lowercase login: nil
// where the entry lists none, and otherwise its full names lowercased and sorted, or why the
// list is refused. A list stands beside repository_selection "selected" alone, since a token of
// every repository names none; it names at least one repository and at most
// githubname.MaxNarrowedRepositories, each account/name of account itself, since one installation
// reaches its own account's repositories alone, a name GitHub can hold, and each once, as GitHub
// compares names, without regard to case.
func storedRepositories(account string, entry githubStoredInstallation) ([]string, string) {
	switch {
	case entry.Repositories == nil:
		return nil, ""
	case entry.RepositorySelection != "selected":
		return nil, `repositories stands beside repository_selection "all", a token of every repository`
	case len(entry.Repositories) == 0:
		return nil, "repositories is empty"
	case len(entry.Repositories) > githubname.MaxNarrowedRepositories:
		return nil, fmt.Sprintf("repositories names %d repositories, past the %d a token takes", len(entry.Repositories), githubname.MaxNarrowedRepositories)
	}
	repositories := make([]string, 0, len(entry.Repositories))
	for _, full := range entry.Repositories {
		owner, name, ok := githubname.SplitEntry(full)
		if !ok || !githubname.ValidRepositoryName(name) {
			return nil, "repositories holds an entry that is no account/name GitHub can hold"
		}
		if strings.ToLower(owner) != account {
			return nil, "repositories names a repository of another account, which its installation does not reach"
		}
		repositories = append(repositories, strings.ToLower(full))
	}
	slices.Sort(repositories)
	if len(slices.Compact(slices.Clone(repositories))) != len(repositories) {
		return nil, "repositories names one repository twice: GitHub compares names without regard to case"
	}
	return repositories, ""
}

// storedScope says why the entry's repository_scope is refused, "" where it has none or the seed
// minter's marker in its place: beside repository_selection "selected" and no repositories list,
// since it says which repositories a selected token reaches in place of a list. Any other value is
// refused, so a value Cipher does not read is never served as though it were the marker.
func storedScope(entry githubStoredInstallation) string {
	switch {
	case entry.RepositoryScope == "":
		return ""
	case entry.RepositoryScope != RepositoryScopeAllExceptProtected:
		return `repository_scope is no scope Cipher reads; it takes "` + RepositoryScopeAllExceptProtected + `" alone`
	case entry.RepositorySelection != "selected" || entry.Repositories != nil:
		return `repository_scope stands beside repository_selection "selected" and no repositories list alone`
	}
	return ""
}

// installationToken reports whether token can be a token: not empty, and printable ASCII with no
// space.
func installationToken(token string) bool {
	return token != "" && !strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r > '~' })
}

// GitHubInstallations hands over the installation tokens a github-installations secret stores,
// which a minter outside the broker minted ahead of any request: it asks GitHub nothing and holds
// no App key. Now lets a test replace the clock.
//
// It has no give-back (requests.Machine.GiveBacks): the token it hands over is the one stored, which
// every other release of the secret hands over too, and revoking it would end every one of them.
type GitHubInstallations struct {
	Now func() time.Time
}

// Mint answers the release of the stored tokens in req.Value that the profile's scope (req.GitHub)
// reaches: one for each account in effect that the value holds a token of, an account the value
// holds none of being left out, as GitHubApp.Mint leaves out an account the App is not installed
// on, and none at all being refused (ErrMintNoInstallation). The release is GitHubApp.Mint's: for a
// value-token profile, the one account's bare token; for every other, the JSON object of
// installations, each the installation's id, token and expiry under the login as stored, with no
// permissions and no repository selection, which no client of that release reads.
//
// Each token is held to the profile's permissions before anything is handed over. One holding a
// permission past the profile's, by name or by level, is refused (ErrMintStoredWider): no token
// wider than its profile leaves the broker. One holding less than the profile, as a minter's cut
// to what the installation holds leaves it, is served, and the release names each of the profile's
// permissions it lacks as GitHubApp.Mint's cut names them (cut, Minted.LeftOut), so the release and
// its log say what the token cannot do. One holding none of the profile's permissions but metadata,
// of a profile that names more, is refused as GitHubApp.Mint's empty cut is (ErrMintEmptyCut).
//
// A token is handed over only while more than MintedTokenMargin of its own life is left; one whose
// expires_at is not later than now plus the margin is refused (ErrMintTokenStale). The margin does
// not bound the grant (MintedTokenMargin). Minted.Expires is the earliest of the tokens'
// expires_at less the margin, so the airlock proxy, which reads the grant again five minutes before
// it, reads the newer token the minter has stored by then rather than carry a call on one about to
// expire.
//
// Each token is held to the profile's repositories too (repositoriesMismatch), since a token cannot
// be cut to repositories once it is minted: one its minter narrowed to repositories serves only a
// profile that names exactly those, and one of every repository only a profile that names none;
// any other is refused (ErrMintStoredRepositories). requests' gitHubScope refuses a target that
// names repositories of such a profile, so the scope here names the profile's own repositories of
// each account in effect, or none. A request with no scope is a programming error in the broker,
// refused as GitHubApp.Mint refuses it. With req.Check it answers the release's verdict and hands
// over nothing. Every error wraps ErrMintStoredValue, ErrMintRefused, ErrMintNarrowingRefused or
// ErrMintUpstream, and none holds a token.
func (m GitHubInstallations) Mint(_ context.Context, req MintRequest) (Minted, error) {
	scope := req.GitHub
	if scope == nil {
		return Minted{}, fmt.Errorf("%w: a github-installations release needs the profile's scope it serves", ErrMintRefused)
	}
	parsed, err := ParseValue(typeGitHubInstallations, req.Value)
	if err != nil {
		return Minted{}, err
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	served := make([]StoredInstallation, 0, len(scope.Accounts))
	var leftOut map[string][]string
	var expires time.Time
	for _, account := range scope.Accounts {
		stored, held := parsed.GitHubInstallations.Installations[account]
		if !held {
			continue
		}
		if wider := widerThan(scope.Permissions, stored.Permissions); len(wider) > 0 {
			return Minted{}, fmt.Errorf("%w: github installations %s: the token stored for %s holds %s, past the profile's permissions (%s)", ErrMintStoredWider, req.named(), account, strings.Join(wider, ", "), permissionWords(scope.Permissions))
		}
		if why := repositoriesMismatch(account, scope, stored); why != "" {
			return Minted{}, fmt.Errorf("%w: github installations %s: %s", ErrMintStoredRepositories, req.named(), why)
		}
		asked, cutOut := cut(scope.Permissions, stored.Permissions)
		if emptyCut(scope.Permissions, asked) {
			return Minted{}, fmt.Errorf("%w: github installations %s: the token stored for %s holds none of %s", ErrMintEmptyCut, req.named(), account, permissionWords(lacked(scope.Permissions, asked)))
		}
		end := stored.ExpiresAt.Add(-MintedTokenMargin)
		if !end.After(now()) {
			return Minted{}, fmt.Errorf("%w: github installations %s: the token stored for %s expires at %s, within %s of now, and its minter has stored no newer one", ErrMintTokenStale, req.named(), account, stored.ExpiresAt.UTC().Format(time.RFC3339), MintedTokenMargin)
		}
		if expires.IsZero() || end.Before(expires) {
			expires = end
		}
		if len(cutOut) > 0 {
			if leftOut == nil {
				leftOut = map[string][]string{}
			}
			leftOut[account] = cutOut
		}
		served = append(served, stored)
	}
	if len(served) == 0 {
		// ErrMintNoInstallation's words, "has no installation on", are the ones a released client
		// matches in the broker's answer (packages/daemon's appauth), as for a github-app secret.
		return Minted{}, fmt.Errorf("%w: github installations %s has no installation on %s: its minter stored no token of it", ErrMintNoInstallation, req.named(), strings.Join(scope.Accounts, " or "))
	}
	if scope.Value == githubname.ValueToken && len(served) > 1 {
		// requests' gitHubScope holds a value-token scope to one account, and the decoder holds
		// each account to one entry, so no release reaches this; one built some other way is
		// refused rather than handed one of several tokens.
		return Minted{}, fmt.Errorf("%w: github installations %s: a bare token of %d accounts", ErrMintSeveralInstallations, req.named(), len(served))
	}
	if req.Check {
		return Minted{}, nil
	}
	if scope.Value == githubname.ValueToken {
		return Minted{Value: served[0].Token, Expires: expires, LeftOut: leftOut}, nil
	}
	release := githubMintedInstallations{Installations: make(map[string]githubMintedInstallation, len(served))}
	for _, stored := range served {
		release.Installations[stored.Account] = githubMintedInstallation{InstallationID: stored.InstallationID, Token: stored.Token, ExpiresAt: stored.ExpiresAt}
	}
	// Marshalling a map of strings, numbers and times cannot fail.
	encoded, _ := json.Marshal(release)
	return Minted{Value: string(encoded), Expires: expires, LeftOut: leftOut}, nil
}

// repositoriesMismatch says how the repositories the token stored for account reaches differ from
// the ones scope names of account, compared as GitHub compares names, without regard to case, and
// is "" when they are the same: a token of every repository (RepositorySelection "all") of a scope
// that names no repository; a token the seed minter cut to every repository but the protected ones
// (AllExceptProtected, whose marker Cipher trusts for the list) of a scope that names no
// repository; or a token narrowed to repositories its minter listed, of a scope that names those
// exactly. A narrowed token whose minter listed none and marked nothing is refused whatever the
// scope names, since nothing says what it reaches.
func repositoriesMismatch(account string, scope *GitHubScope, stored StoredInstallation) string {
	var named []string // the scope's account/name entries of account, lowercased; nil for none
	if scope.Repositories != nil {
		named = []string{}
		for _, entry := range scope.Repositories {
			if _, ok := repositoryOf(entry, account); ok {
				named = append(named, strings.ToLower(entry))
			}
		}
		slices.Sort(named)
		named = slices.Compact(named)
	}
	words := func(repositories []string) string {
		if len(repositories) == 0 {
			return "no repository of " + account
		}
		return strings.Join(repositories, ", ")
	}
	switch {
	case stored.RepositorySelection == "all" && named == nil:
		return ""
	case stored.RepositorySelection == "all":
		return fmt.Sprintf("the token stored for %s reaches every repository of its installation, and the profile names %s", account, words(named))
	case stored.AllExceptProtected && named == nil:
		return ""
	case stored.AllExceptProtected:
		return fmt.Sprintf("the token stored for %s reaches every repository of its installation but the protected ones, and the profile names %s", account, words(named))
	case stored.Repositories == nil:
		return fmt.Sprintf("the token stored for %s reaches repositories its minter selected and did not list", account)
	case named == nil:
		return fmt.Sprintf("the token stored for %s reaches %s alone, and the profile names no repositories, so it would claim every repository of the installation", account, words(stored.Repositories))
	case !slices.Equal(named, stored.Repositories):
		return fmt.Sprintf("the token stored for %s reaches %s, and the profile names %s", account, words(stored.Repositories), words(named))
	}
	return ""
}

// widerThan is each of held's permissions past profile's, sorted, as "<permission>:<level>": one
// profile does not name, or one held at a level above the one profile names.
func widerThan(profile, held map[string]string) []string {
	var wider []string
	for _, permission := range slices.Sorted(maps.Keys(held)) {
		level, named := profile[permission]
		if !named || githubname.LevelRank(held[permission]) > githubname.LevelRank(level) {
			wider = append(wider, permission+":"+held[permission])
		}
	}
	return wider
}
