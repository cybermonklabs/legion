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

// GitHubInstallationsShapeVersions are the values of a stored value's shapeVersion this broker
// reads. A minter writes the version of the shape it writes, and a value of any other version, or
// of none, is refused whole (parseGitHubInstallations), so a shape this broker does not know is
// never read as one it does: a sha256 pin of the shapes' fixtures (testdata/minted-shapes/) fails
// only on the side that changed them, and this is the check that runs where both meet.
var GitHubInstallationsShapeVersions = []int{1}

// RepositoryScopeAllExceptProtected is the one value a stored entry's repository_scope takes: the
// seed minter's marker of a token it cut to every repository of the installation but the protected
// ones, stored beside repository_selection "selected" and no repositories list. Only the seed
// minter writes it, into a secret its deployment lets only its minter and its appliers write, so
// Cipher trusts it for the list the token was cut to and serves the token to a profile that names
// no repositories (repositoriesMismatch). Cipher does not prove the cut: the deployment's own
// checks of the admin token do.
const RepositoryScopeAllExceptProtected = "all-except-protected"

// StoredInstallation is one installation token a minter stored: the account's login as the minter
// wrote it, the installation, the token, when GitHub lapses it, the permissions GitHub minted it
// with (each permission and its level), and the repositories it reaches. RepositorySelection is
// "all" for every repository of the installation, or "selected", and then one of three: the seed
// minter's marker (AllExceptProtected); the token's own list (Repositories) with the installation's
// own selection beside it (InstallationRepositories), for a token the minter asked for with no
// repositories and listed with that token; or the token's own list alone, for a token the minter
// cut to a profile's repositories. Each list is "account/name" lowercased, as GitHub compares
// names, and sorted; nil where the minter wrote none.
type StoredInstallation struct {
	Account                  string
	InstallationID           int64
	Token                    string
	ExpiresAt                time.Time
	Permissions              map[string]string
	RepositorySelection      string
	Repositories             []string
	InstallationRepositories []string
	AllExceptProtected       bool
}

// githubInstallationsSecret and githubStoredInstallation are a github-installations secret's
// value as it is stored, which parseGitHubInstallations decodes strictly. ShapeVersion is held raw,
// so a version of any JSON form is named in the refusal of it.
type (
	githubInstallationsSecret struct {
		ShapeVersion  json.RawMessage                     `json:"shapeVersion"`
		Installations map[string]githubStoredInstallation `json:"installations"`
	}
	githubStoredInstallation struct {
		InstallationID           int64             `json:"installation_id"`
		Token                    string            `json:"token"`
		ExpiresAt                time.Time         `json:"expires_at"`
		Permissions              map[string]string `json:"permissions"`
		RepositorySelection      string            `json:"repository_selection"`
		Repositories             []string          `json:"repositories"`
		InstallationRepositories []string          `json:"installation_repositories"`
		RepositoryScope          string            `json:"repository_scope"`
	}
)

// The field names each object of a stored value may hold, exactly as written: encoding/json
// matches a field without regard to case and keeps the last of two keys, so exactKeys holds the
// value to these first.
var (
	installationsSecretKeys = []string{"shapeVersion", "installations"}
	storedInstallationKeys  = []string{"installation_id", "token", "expires_at", "permissions", "repository_selection", "repositories", "installation_repositories", "repository_scope"}
)

// parseGitHubInstallations is ParseValue for a github-installations value: one JSON object and
// nothing after it, holding shapeVersion and installations and no other field, no key twice in any
// object and each field name exactly as written (exactKeys). shapeVersion is one of
// GitHubInstallationsShapeVersions; any other, or none, refuses the value whole, naming the version
// read and the versions known. installations holds at least one entry, each keyed by a GitHub
// login, no login twice whatever its case, and each of exactly installation_id (positive), token
// (printable ASCII with no space, which no header it is sent in can be broken by), expires_at (an
// RFC 3339 time), permissions (at least one, each a GitHub App permission's name at a level GitHub
// gives), repository_selection ("all" or "selected") and, beside "selected" alone, repositories
// with or without installation_repositories (storedRepositories), or repository_scope
// (RepositoryScopeAllExceptProtected, beside no list). No error names a token; an account is named
// only once it is read as a login.
func parseGitHubInstallations(value string) (GitHubInstallationsValue, error) {
	refuse := func(why string) (GitHubInstallationsValue, error) {
		return GitHubInstallationsValue{}, fmt.Errorf("%w: GitHub installation tokens: %s", ErrMintStoredValue, why)
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	var secret githubInstallationsSecret
	if decoder.Decode(&secret) != nil {
		return refuse("not a JSON object of shapeVersion and installations, each installation of installation_id, token, expires_at, permissions and repository_selection, and of no other field")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return refuse("something follows the one JSON object")
	}
	if why := exactKeys(value); why != "" {
		return refuse(why)
	}
	if why := shapeVersion(secret.ShapeVersion); why != "" {
		return refuse(why)
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
		repositories, installationRepositories, why := storedRepositories(account, entry)
		if why == "" {
			why = storedScope(entry)
		}
		if why != "" {
			return refuse(account + ": " + why)
		}
		read.Installations[account] = StoredInstallation{Account: key, InstallationID: entry.InstallationID, Token: entry.Token,
			ExpiresAt: entry.ExpiresAt, Permissions: entry.Permissions, RepositorySelection: entry.RepositorySelection,
			Repositories: repositories, InstallationRepositories: installationRepositories,
			AllExceptProtected: entry.RepositoryScope == RepositoryScopeAllExceptProtected}
	}
	return read, nil
}

// shapeVersion says why a stored value's shapeVersion, as written (raw, nil where the value names
// none), is refused, "" where it is one of GitHubInstallationsShapeVersions. The refusal names the
// version read: "none" for none or null, the number for a whole number, and "that is no number"
// for anything else, whose text is never repeated, since a minter that wrote the wrong field there
// may have written a token.
func shapeVersion(raw json.RawMessage) string {
	known := fmt.Sprint(GitHubInstallationsShapeVersions)
	var version int
	switch {
	case raw == nil || string(raw) == "null":
		return "shapeVersion none, and this broker reads shapeVersion " + known + " alone"
	case json.Unmarshal(raw, &version) != nil:
		return "a shapeVersion that is no number, and this broker reads shapeVersion " + known + " alone"
	case !slices.Contains(GitHubInstallationsShapeVersions, version):
		return fmt.Sprintf("shapeVersion %d, and this broker reads shapeVersion %s alone", version, known)
	}
	return ""
}

// exactKeys says why value, a JSON object encoding/json has decoded, is refused for its keys, ""
// where it is not: an object that holds one key twice, at any depth, or a field name that is not
// one of the shape's exactly as written (installationsSecretKeys at the top,
// storedInstallationKeys in each installation). encoding/json reads "Token" as token and keeps the
// last of two "token" keys, so without this a value could be read as other than it says. No refusal
// names the key, which a malformed value could make a token.
func exactKeys(value string) string {
	return walkKeys(json.NewDecoder(strings.NewReader(value)), installationsSecretRule)
}

// keyRule is what walkKeys holds one JSON value to where it is an object: the keys it may hold
// (nil for any), and the rule of the value under each key (child; nil, or a nil child, for a value
// checked for keys held twice alone).
type keyRule struct {
	keys  []string
	child func(key string) *keyRule
}

// The rules of a stored value's objects: the value itself, its installations, and each
// installation.
var (
	installationsSecretRule = &keyRule{keys: installationsSecretKeys, child: func(key string) *keyRule {
		if key == "installations" {
			return installationsRule
		}
		return nil
	}}
	installationsRule      = &keyRule{child: func(string) *keyRule { return storedInstallationRule }}
	storedInstallationRule = &keyRule{keys: storedInstallationKeys}
)

// walkKeys reads the next JSON value from decoder and says why its keys are refused, "" where they
// are not: an object, at any depth, holding one key twice, or, where rule names its keys, a key it
// does not name.
func walkKeys(decoder *json.Decoder, rule *keyRule) string {
	const notJSON = "not one JSON object"
	token, err := decoder.Token()
	if err != nil {
		return notJSON
	}
	delim, opens := token.(json.Delim)
	if !opens {
		return ""
	}
	if delim == '[' {
		for decoder.More() {
			if why := walkKeys(decoder, nil); why != "" {
				return why
			}
		}
	} else {
		seen := map[string]bool{}
		for decoder.More() {
			token, err := decoder.Token()
			key, isKey := token.(string)
			switch {
			case err != nil || !isKey:
				return notJSON
			case seen[key]:
				return "an object holds one key twice"
			case rule != nil && rule.keys != nil && !slices.Contains(rule.keys, key):
				return "an object holds a field the shape does not, or one of its fields in another case"
			}
			seen[key] = true
			var child *keyRule
			if rule != nil && rule.child != nil {
				child = rule.child(key)
			}
			if why := walkKeys(decoder, child); why != "" {
				return why
			}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return notJSON
	}
	return ""
}

// storedRepositories reads the two repository lists of the stored token of account, a lowercase
// login: the token's own (repositories) and the installation's own selection
// (installation_repositories), each nil where the entry holds none, and otherwise its full names
// lowercased and sorted; or why they are refused. Both stand beside repository_selection "selected"
// alone, since a token of every repository names none, and installation_repositories beside
// repositories alone, since the minter lists the installation's selection with the token it asked
// for with no repositories, which reaches that selection. Each names at least one repository, each
// account/name of account itself, since one installation reaches its own account's repositories
// alone, a name GitHub can hold, and each once, as GitHub compares names, without regard to case.
func storedRepositories(account string, entry githubStoredInstallation) (repositories, installationRepositories []string, why string) {
	switch {
	case entry.Repositories == nil && entry.InstallationRepositories == nil:
		return nil, nil, ""
	case entry.RepositorySelection != "selected":
		return nil, nil, `a repository list stands beside repository_selection "all", a token of every repository`
	case entry.Repositories == nil:
		return nil, nil, "installation_repositories stands beside no repositories list"
	}
	if repositories, why = repositoryList(account, "repositories", entry.Repositories); why != "" {
		return nil, nil, why
	}
	if entry.InstallationRepositories != nil {
		if installationRepositories, why = repositoryList(account, "installation_repositories", entry.InstallationRepositories); why != "" {
			return nil, nil, why
		}
	}
	return repositories, installationRepositories, ""
}

// repositoryList reads list, the stored list of account named field: its full names lowercased and
// sorted, or why it is refused (storedRepositories).
func repositoryList(account, field string, list []string) ([]string, string) {
	if len(list) == 0 {
		return nil, field + " is empty"
	}
	read := make([]string, 0, len(list))
	for _, full := range list {
		owner, name, ok := githubname.SplitEntry(full)
		if !ok || !githubname.ValidRepositoryName(name) {
			return nil, field + " holds an entry that is no account/name GitHub can hold"
		}
		if strings.ToLower(owner) != account {
			return nil, field + " names a repository of another account, which its installation does not reach"
		}
		read = append(read, strings.ToLower(full))
	}
	slices.Sort(read)
	if len(slices.Compact(slices.Clone(read))) != len(read) {
		return nil, field + " names one repository twice: GitHub compares names without regard to case"
	}
	return read, ""
}

// storedScope says why the entry's repository_scope is refused, "" where it has none or the seed
// minter's marker in its place: beside repository_selection "selected" and no repository list,
// since it says which repositories a selected token reaches in place of a list. Any other value is
// refused, so a value Cipher does not read is never served as though it were the marker; and a
// selected entry with neither a list nor the marker is refused, since nothing says what its token
// reaches.
func storedScope(entry githubStoredInstallation) string {
	switch {
	case entry.RepositoryScope == "" && entry.RepositorySelection == "selected" && entry.Repositories == nil:
		return `repository_selection "selected" names neither a repositories list nor a repository_scope, so nothing says what the token reaches`
	case entry.RepositoryScope == "":
		return ""
	case entry.RepositoryScope != RepositoryScopeAllExceptProtected:
		return `repository_scope is no scope Cipher reads; it takes "` + RepositoryScopeAllExceptProtected + `" alone`
	case entry.RepositorySelection != "selected" || entry.Repositories != nil || entry.InstallationRepositories != nil:
		return `repository_scope stands beside repository_selection "selected" and no repository list alone`
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
// be cut to repositories once it is minted: a profile that names repositories is served only a
// token whose own list is exactly those, and a profile that names none only a token of every
// repository, the seed minter's marked admin token, or a token whose own list equals the
// installation's own selection; any other is refused (ErrMintStoredRepositories). requests'
// gitHubScope refuses a target that names repositories of such a profile, so the scope here names
// the profile's own repositories of each account in effect, or none. A request with no scope is a
// programming error in the broker, refused as GitHubApp.Mint refuses it. With req.Check it answers
// the release's verdict and hands over nothing. Every error wraps ErrMintStoredValue,
// ErrMintRefused, ErrMintNarrowingRefused or ErrMintUpstream, names the secret (req.named), and
// holds no token.
//
// What it holds a token to is what the minter declared beside it: the permissions, the
// repositories and the marker. Cipher cannot tell that the token under an account is that
// account's, or that it holds what its entry says; the minter and the deployment's appliers, which
// alone may write the secret, and the deployment's checks of its output stand behind that.
func (m GitHubInstallations) Mint(_ context.Context, req MintRequest) (Minted, error) {
	scope := req.GitHub
	if scope == nil {
		return Minted{}, fmt.Errorf("%w: a github-installations release needs the profile's scope it serves", ErrMintRefused)
	}
	parsed, err := ParseValue(typeGitHubInstallations, req.Value)
	if err != nil {
		return Minted{}, fmt.Errorf("github installations %s: %w", req.named(), err)
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
// is "" when they are the same. A scope that names repositories fits only a token whose own list
// (Repositories) is exactly those. A scope that names none claims every repository the installation
// reaches, so it fits a token of every repository (RepositorySelection "all"), a token the seed
// minter cut to every repository but the protected ones (AllExceptProtected, whose marker Cipher
// trusts for the list), and a token whose own list equals the installation's own selection
// (InstallationRepositories), which the minter writes for a token it asked for with no
// repositories; it does not fit a token the minter cut to a profile's list (no
// InstallationRepositories), or one whose two lists differ. A selected entry that lists nothing
// and carries no marker never gets here: the read refuses it (storedScope).
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
	case named != nil && !slices.Equal(named, stored.Repositories):
		return fmt.Sprintf("the token stored for %s reaches %s, and the profile names %s", account, words(stored.Repositories), words(named))
	case named != nil:
		return ""
	case stored.InstallationRepositories == nil:
		return fmt.Sprintf("the token stored for %s reaches %s alone, which its minter cut it to, and the profile names no repositories, so it would claim every repository of the installation", account, words(stored.Repositories))
	case !slices.Equal(stored.InstallationRepositories, stored.Repositories):
		return fmt.Sprintf("the token stored for %s reaches %s, and its installation selects %s, and the profile names no repositories, so it would claim the installation's selection", account, words(stored.Repositories), words(stored.InstallationRepositories))
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
