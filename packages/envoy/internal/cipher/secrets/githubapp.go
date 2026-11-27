package secrets

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/githubappjwt"
	"github.com/sjawhar/envoy/internal/urljoin"
)

const githubAPIBase = "https://api.github.com"

// GitHubApp mints installation tokens from the App id and private key in a github-app secret.
// Base, HTTP and Now let tests replace GitHub, its transport and the clock. Retry is the policy
// each of a mint's calls is retried under, and the process-wide budget it draws from; nil makes
// one attempt of each. It is a pointer, so every copy of the minter (typeMinters takes it by
// value) draws from the one budget.
type GitHubApp struct {
	Base  string
	HTTP  *http.Client
	Now   func() time.Time
	Retry *Retry
}

type githubInstallation struct {
	ID int64 `json:"id"`
	// Account is the user or organization the App is installed on, named by Login, or the
	// enterprise, which GitHub names by Slug and gives no login.
	Account struct {
		Login string `json:"login"`
		Slug  string `json:"slug"`
	} `json:"account"`
	// SuspendedAt is set while the App is suspended on the installation's account, where GitHub
	// refuses its token, so Mint leaves the installation out rather than failing every owner.
	SuspendedAt *time.Time `json:"suspended_at"`
	// Permissions is what the installation's account approved for the App: each permission it
	// holds, at the access it holds it ("read", "write" or "admin"). The listing carries it, so a
	// profile's permissions are cut to what the installation holds (cut) at no request of its own.
	Permissions map[string]string `json:"permissions"`
}

// githubInstallationToken is GitHub's answer to a token request: the token, its expiry, and what
// GitHub says the token holds, which Mint checks against the request it sent (heldBy).
type githubInstallationToken struct {
	Token               string            `json:"token"`
	ExpiresAt           time.Time         `json:"expires_at"`
	Permissions         map[string]string `json:"permissions"`
	RepositorySelection string            `json:"repository_selection"`
	Repositories        []struct {
		Name string `json:"name"`
	} `json:"repositories"`
}

type githubMintedInstallations struct {
	Installations map[string]githubMintedInstallation `json:"installations"`
}

type githubMintedInstallation struct {
	InstallationID int64     `json:"installation_id"`
	Token          string    `json:"token"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// githubAccessTokensBody is GitHub's POST /app/installations/{id}/access_tokens body for a
// profile's token: the repositories it reaches, left out where the scope names none, and the
// permissions it holds, which every request names (cut).
type githubAccessTokensBody struct {
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

// cut is what a token request asks GitHub for of an installation that holds held
// (githubInstallation.Permissions): each of the profile's permissions at its own level where the
// installation holds it at that level or above, at the installation's level where it holds a
// lower one, and not at all where it holds none. GitHub answers 422 to a request naming a
// permission past what the installation holds and mints nothing, so the cut is what lets an
// installation that holds less than the profile still release what it holds. leftOut is each
// entry the cut left out, sorted: "<permission>:<level>" for a level it asked for in place of the
// higher one, "<permission>" for a permission it left out whole. It reads what the installation
// holds from the listing Mint has already made, so it costs no request.
func cut(profile, held map[string]string) (asked map[string]string, leftOut []string) {
	asked = make(map[string]string, len(profile))
	for _, permission := range slices.Sorted(maps.Keys(profile)) {
		level, holds := profile[permission], held[permission]
		switch {
		case githubname.LevelRank(holds) == 0:
			leftOut = append(leftOut, permission)
		case githubname.LevelRank(holds) < githubname.LevelRank(level):
			asked[permission] = holds
			leftOut = append(leftOut, permission+":"+level)
		default:
			asked[permission] = level
		}
	}
	return asked, leftOut
}

// emptyCut reports whether asked, the cut of profile's permissions to what an installation holds,
// leaves the token nothing the profile is for: no permission at all, or metadata alone of a
// profile that names more. Every installation GitHub lists holds metadata, so a cut that keeps it
// alone is the empty cut GitHub's own listings give, and minting it would release a token that
// does none of what the profile asks, after the check before an approval had let a person be
// asked for it. A profile that names metadata alone asks for nothing more, and its cut is whole.
func emptyCut(profile, asked map[string]string) bool {
	_, kept := asked[githubname.MetadataPermission]
	return len(asked) == 0 || (kept && len(asked) == 1 && len(profile) > 1)
}

// lacked is the profile's permissions an empty cut, asked, left out, for its refusal: each
// permission the profile names that the cut does not keep.
func lacked(profile, asked map[string]string) map[string]string {
	left := make(map[string]string, len(profile))
	for permission, level := range profile {
		if _, kept := asked[permission]; !kept {
			left[permission] = level
		}
	}
	return left
}

// accessTokensBody is GitHub's access_tokens body asking for permissions on repositories, the
// names of the scope's repositories of the one account the token is for (nil for every
// repository the installation reaches). Mint never sends it with no permissions
// (ErrMintEmptyCut). Marshalling a struct of a string slice and a string map cannot fail, so its
// error is dropped.
func accessTokensBody(repositories []string, permissions map[string]string) []byte {
	encoded, _ := json.Marshal(githubAccessTokensBody{Repositories: repositories, Permissions: permissions})
	return encoded
}

// repositoriesOf is the names of the scope's repositories of account, a lowercase login, as
// GitHub's access_tokens body takes them, without the owner: the body is sent to that account's
// installation, which knows its repositories by name alone. It is nil for a scope of every
// repository, and empty for an account the scope names no repository of (refusal refuses that).
func (s *GitHubScope) repositoriesOf(account string) []string {
	if s.Repositories == nil {
		return nil
	}
	names := make([]string, 0, len(s.Repositories))
	for _, entry := range s.Repositories {
		if name, ok := repositoryOf(entry, account); ok {
			names = append(names, name)
		}
	}
	return names
}

// repositoryOf reads one entry of GitHubScope.Repositories, "account/name", as the name of a
// repository of account, and false for an entry of any other account.
func repositoryOf(entry, account string) (string, bool) {
	owner, name, ok := githubname.SplitEntry(entry)
	return name, ok && owner == account
}

// permissionWords is permissions as one phrase of a refusal, "contents: read, issues: write", in
// sorted order. It holds nothing but the profile's own words.
func permissionWords(permissions map[string]string) string {
	words := make([]string, 0, len(permissions))
	for _, permission := range slices.Sorted(maps.Keys(permissions)) {
		words = append(words, permission+": "+permissions[permission])
	}
	return strings.Join(words, ", ")
}

// refusal is the scope's own refusal, before anything is asked of GitHub: a scope that names
// repositories, of which an account in effect holds none, is refused naming that account, since
// that account's token would name no repository and so reach every one its installation does.
// A value-token scope of more than one account is the caller's to refuse, where it can tell the
// session to name one with --target (requests' gitHubScope, the one place that rule is written):
// every scope the broker mints is built there, so the rule is not written a second time here.
// Mint still releases no bare token of more than one installation (ErrMintSeveralInstallations).
func (s *GitHubScope) refusal(appID string) error {
	if s.Repositories == nil {
		return nil
	}
	for _, account := range s.Accounts {
		holds := func(entry string) bool {
			_, ok := repositoryOf(entry, account)
			return ok
		}
		if !slices.ContainsFunc(s.Repositories, holds) {
			return fmt.Errorf("%w: github app %s: the scope names repositories (%s), and none of %s, which is in effect", ErrMintNarrowingRefused, appID, strings.Join(s.Repositories, ", "), account)
		}
	}
	return nil
}

// githubRepositoryAnswerBound is more than any one repository object GitHub's access_tokens answer
// can hold: measured objects are 4.9 to 5.3 KB, and a 100-character name repeated in every URL, a
// 350-character description escaped six bytes a character and a 39-character login repeated in
// the owner's URLs add under 10 KB more.
const githubRepositoryAnswerBound = 64 << 10

// accessTokensResponseLimit caps what Mint reads of a token answer, which names every repository
// a token of named repositories reaches: the common cap plus githubRepositoryAnswerBound for each
// of githubname.MaxNarrowedRepositories. GitHub has minted the token by the time the answer
// arrives, and an answer read in part cannot be released, and its revoke is sent only where the
// part read names the token (giveBackUnusable), so the cap admits whole every answer a scope
// within the maximum can get.
const accessTokensResponseLimit = mintResponseLimit + githubname.MaxNarrowedRepositories*githubRepositoryAnswerBound

// refusal is err, GitHub's answer to the token request for owner, unless it is the 422 GitHub
// gives a token request it cannot serve: that is the caller's scope (ErrMintNarrowingRefused),
// named by what it likely got wrong, the repositories it names or else the permissions it asks
// for (asked), since GitHub's own text is never quoted. A 422 that is a rate limit is no verdict
// on the scope, and stays the upstream's.
func refusal(err error, appID, owner string, repositories []string, asked map[string]string) error {
	var status *mintStatusError
	if !errors.As(err, &status) || status.code != http.StatusUnprocessableEntity || status.rateLimited {
		return err
	}
	cause := fmt.Sprintf("a permission it asks for (%s) that the installation does not hold", permissionWords(asked))
	if len(repositories) > 0 {
		cause = fmt.Sprintf("a repository it names (%s) that the installation does not reach, such as a misspelled name", strings.Join(repositories, ", "))
	}
	return withAttempts(fmt.Errorf("%w: github app %s: GitHub answered HTTP 422 to the profile's token for %s: likely %s", ErrMintNarrowingRefused, appID, owner, cause), err)
}

// heldBy checks that token, as GitHub describes it, holds no more than its request asked: named
// permissions, each one the request named (asked) at no level above the one it named; and, for a
// request naming repositories, a selection of named repositories, each one it named (GitHub names
// them in its own case). It accepts no permission unasked, metadata at read among them: GitHub
// grants that with every installation token, so the profiles file refuses a GitHub profile that
// does not name it (policy.ParseProfiles), and a token holding it for a request that does not is
// past the request like any other. GitHub applies the access_tokens body and refuses what it
// cannot grant, so a token past it means something between the broker and GitHub dropped or
// widened the body; refusing it keeps the scope the profile states true by construction. Its
// errors name the rule the answer failed and never a value the answer holds, which an upstream
// that is not GitHub can fill with what the request carried.
func heldBy(token githubInstallationToken, repositories []string, asked map[string]string) error {
	if len(token.Permissions) == 0 {
		return errors.New("GitHub's token names no permissions, so it may hold more than its request asked")
	}
	for _, permission := range slices.Sorted(maps.Keys(token.Permissions)) {
		level := token.Permissions[permission]
		want, named := asked[permission]
		if !named {
			return errors.New("GitHub's token holds a permission the request did not name")
		}
		if githubname.LevelRank(level) == 0 || githubname.LevelRank(level) > githubname.LevelRank(want) {
			return errors.New("GitHub's token holds a permission at a level past the request")
		}
	}
	if len(repositories) == 0 {
		return nil
	}
	if token.RepositorySelection != "selected" {
		return errors.New(`GitHub's token has a repository selection other than "selected", so it may reach more than the request names`)
	}
	if len(token.Repositories) == 0 {
		return errors.New("GitHub's token names no repositories, so it may reach more than the request names")
	}
	for _, repository := range token.Repositories {
		if !slices.ContainsFunc(repositories, func(name string) bool { return strings.EqualFold(name, repository.Name) }) {
			return errors.New("GitHub's token reaches a repository the request does not name")
		}
	}
	return nil
}

// revokeTimeout bounds the revoke of a token Mint minted and will not release.
const revokeTimeout = 10 * time.Second

// revokedWiderMessage is logged, at WARN, when GitHub does not confirm the revoke of a profile's
// token it minted wider than asked, followed by app=, installation=, expires_at= (when the token
// lapses at GitHub without the revoke) and error=, which never holds the token.
const revokedWiderMessage = "broker: GitHub did not revoke a narrowed token it minted wider than asked; it lapses at its expiry"

// revokedCheckMessage is logged, at WARN, when GitHub does not confirm the revoke of a token a
// check minted — the caller's context ending first is the likeliest cause — followed by app=,
// installation=, expires_at= (when the token lapses at GitHub without the revoke) and error=,
// which never holds the token. It is a line of its own rather than revokedWiderMessage because
// that one names a fault a check usually does not have: a check revokes every token it mints,
// nearly always one GitHub minted exactly as asked, and its reader would go looking at the
// profile for nothing.
const revokedCheckMessage = "broker: GitHub did not revoke a token minted for a check; it lapses at its expiry"

// revokedUnusableMessage is logged, at WARN, when GitHub does not confirm the revoke of a token it
// minted whose answer Mint could not use — an answer cut off part of the way, one past its cap,
// one that does not decode, or one with no expiry — followed by app=, installation= and error=,
// which never holds the token. Nobody got the token, by a check or by a release, and GitHub
// honours it until it lapses.
const revokedUnusableMessage = "broker: GitHub did not revoke a token it minted whose answer the broker could not use; it lapses at its expiry"

// unrevokableMessage is logged, at WARN, when GitHub answered a token request 201, so it minted a
// token, and the answer the broker holds does not name it - a whole answer with no token, or the
// part of a cut-off one that arrived before the token - followed by app=, installation= and
// error=. Nothing can revoke that token, so it stays live at GitHub, held by nobody, until it
// lapses within the hour GitHub gives an installation token.
const unrevokableMessage = "broker: GitHub minted a token the broker cannot revoke, since the answer it holds does not name it; that token stays live until it lapses at its expiry"

// revokedUnreleasedMessage is logged, at WARN, when a release that minted a token hands it to
// nobody - because it failed at a later installation (followed by app=), or, through GiveBack
// (followed by name=), because it failed after that name or holds that name behind the airlock,
// whose session gets placeholders - and GitHub does not confirm the revoke of that token, followed
// by installation=, expires_at= and error=, which never holds the token.
const revokedUnreleasedMessage = "broker: GitHub did not revoke a token a release minted and handed to nobody; it lapses at its expiry"

// revokeInstallationToken asks GitHub to revoke token, as the token itself
// (DELETE /installation/token), for a token Mint minted and will not release, retried as the
// mint's other calls are and from the same wait budget, or made once for a give-back (startOnce).
// It runs to its own bound even once ctx ends, since GitHub honours an unreleased token for an
// hour otherwise. A 401 or a 404 to an attempt after the first answers nil, since an earlier
// attempt may have revoked the token and lost its answer; one to the first attempt stays a
// refusal, since nothing in this revoke can have revoked a token minted moments before.
func (m GitHubApp) revokeInstallationToken(ctx context.Context, mint *mintRetry, token string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	return m.revokeToken(ctx, mint, token)
}

// revokeToken is that same revoke under ctx as it is given, retried from the same wait budget,
// and nothing else. A check's revoke of a token GitHub answered whole calls it with the caller's
// own context (Mint): that caller is waiting on the check's answer inside a budget it bounds, and a
// revoke that outlived the cut context by revokeTimeout for each probe would put the check past
// the 30 s whole-call timeout the agent-secrets client gives every call
// (leasetime.RenewClientTimeout). A release's revoke runs after its caller may be gone, so it
// keeps the detached bound revokeInstallationToken gives it, and so does the give-back of a token
// whose answer a check could not use (giveBackUnusable): the check's bound is the likeliest thing
// to have cut that answer off, and no mint starts after that bound, so at most one such give-back
// runs past it.
func (m GitHubApp) revokeToken(ctx context.Context, mint *mintRetry, token string) error {
	const what = "revoke GitHub App installation token"
	request, err := m.request(ctx, http.MethodDelete, "/installation/token", token, nil)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMintUpstream, what, err)
	}
	_, err = mintCall(mint, m.HTTP, request, what, "GitHub", http.StatusNoContent, mintResponseLimit, true, nil)
	// GitHub answers a request made as a token it has revoked 401, and one made as a token it no
	// longer knows 404. Nothing in this revoke can have revoked a token minted moments before, so
	// either status at the first attempt is GitHub refusing the request, and stays the refusal
	// Mint logs revokedWiderMessage for. At a later attempt either may follow an earlier attempt
	// that revoked the token and lost its answer, past mintAttemptTimeout or on a connection that
	// dropped, so it reads as the revoke done. mintCall's error is its last attempt's alone, so an
	// earlier attempt that was a rate limit, which revoked nothing, cannot be told apart from that
	// and reads the same. A rate limit itself is no verdict on the token at any attempt.
	var status *mintStatusError
	if MintAttempts(err) > 1 && errors.As(err, &status) && !status.rateLimited &&
		(status.code == http.StatusUnauthorized || status.code == http.StatusNotFound) {
		return nil
	}
	return err
}

// giveBackHook is the mintCall hook a token request passes (mintCall's answered): each attempt
// whose answer came with GitHub's 201 but could not be read whole, cut off part of the way or past
// accessTokensResponseLimit, minted a token this call will never hold, and the hook gives it back
// before the request is sent again or the call answers (giveBackUnusable).
func (m GitHubApp) giveBackHook(ctx context.Context, mint *mintRetry, appID string, installationID int64) func([]byte, error) {
	return func(part []byte, err error) {
		m.giveBackUnusable(ctx, mint, appID, installationID, answeredToken(part), err)
	}
}

// answeredToken is the token a 201 answer to a token request names, read from as much of that
// answer as arrived, and "" where that part does not name one whole: the "token" member of the
// top-level object, which GitHub writes first, read token by token so an answer cut off after it,
// or carrying a member that does not decode after it, still gives it up. It quotes nothing.
func answeredToken(payload []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		return ""
	}
	for {
		key, err := decoder.Token()
		name, isKey := key.(string)
		if err != nil || !isKey {
			return ""
		}
		if name == "token" {
			value, err := decoder.Token()
			token, isString := value.(string)
			if err != nil || !isString {
				return ""
			}
			return token
		}
		if skipJSONValue(decoder) != nil {
			return ""
		}
	}
}

// skipJSONValue reads past the one value the decoder is at, however deep it nests.
func skipJSONValue(decoder *json.Decoder) error {
	depth := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}

// giveBackUnusable tries to revoke token, which GitHub minted for a token request whose answer
// Mint could not use, so that it does not stay live at GitHub for nobody. The revoke runs under the
// detached bound a release's revoke has (revokeInstallationToken), for a check as for a release:
// the likeliest reason a check's answer is cut off is the check's own bound, which has then ended
// the caller's context too, and a revoke sent on it would never leave the broker. So a check's
// answer can wait up to revokeTimeout past its bound for this revoke. A revoke GitHub does not
// confirm is logged (revokedUnusableMessage), and the token stays live until it lapses. An answer
// whose part the broker holds does not name its token, "" here, leaves nothing to revoke, and is
// logged as such with why, the error the answer failed with (unrevokableMessage).
func (m GitHubApp) giveBackUnusable(ctx context.Context, mint *mintRetry, appID string, installationID int64, token string, why error) {
	if token == "" {
		slog.Warn(unrevokableMessage, "app", appID, "installation", installationID, "error", why)
		return
	}
	if revokeErr := m.revokeInstallationToken(ctx, mint, token); revokeErr != nil {
		slog.Warn(revokedUnusableMessage, "app", appID, "installation", installationID, "error", revokeErr)
	}
}

// revokeUnreleased tries to revoke, under the detached bound a release's revoke has, every token
// in minted, which a release minted and hands to nobody: one that failed at a later installation
// (Mint), or one whose grant's release failed after this name or holds it behind the airlock
// (GiveBack). A revoke GitHub does not confirm is logged (revokedUnreleasedMessage) after named,
// the key and value that say whose token it was. A check holds no token here, having revoked each
// one at GitHub's answer and blanked it.
func (m GitHubApp) revokeUnreleased(ctx context.Context, mint *mintRetry, minted githubMintedInstallations, named ...any) {
	for _, owner := range slices.Sorted(maps.Keys(minted.Installations)) {
		installation := minted.Installations[owner]
		if installation.Token == "" {
			continue
		}
		if err := m.revokeInstallationToken(ctx, mint, installation.Token); err != nil {
			slog.Warn(revokedUnreleasedMessage, slices.Concat(named, []any{"installation", installation.InstallationID, "expires_at", installation.ExpiresAt, "error", err})...)
		}
	}
}

// GiveBack tries to revoke every installation token in value, a github-app release as Mint
// answered it, which the caller minted for the secret name and hands to nobody
// (requests.Machine.GiveBacks): the broker's release of a grant that failed after this name was
// minted, or that holds this name behind the airlock, whose session gets placeholders while the
// proxy mints a value of its own. form is the github.value of the profile the value was minted
// for, which says what the value is, so nothing is read off the value's text: for
// githubname.ValueToken the one account's bare token, and for every other form a JSON object of
// installations, which a value that does not decode as one is not, and names no token to revoke.
// Each revoke runs under the detached bound a release's revoke has, whatever ctx does, in one
// attempt (startOnce): nobody waits on it, and a retry would draw on the process-wide budget the
// mints a caller waits on retry from, which a GitHub outage leaves short while every airlock
// release of a github-app name gives a token back. A revoke GitHub does not confirm is logged
// with name= (revokedUnreleasedMessage) and leaves its token live until it lapses.
func (m GitHubApp) GiveBack(ctx context.Context, name, value, form string) {
	var minted githubMintedInstallations
	switch {
	case value == "":
		return
	case form == githubname.ValueToken:
		minted.Installations = map[string]githubMintedInstallation{"": {Token: value}}
	case json.Unmarshal([]byte(value), &minted) != nil:
		return
	}
	m.revokeUnreleased(ctx, m.Retry.startOnce(typeGitHubApp), minted, "name", name)
}

// installationMint is one installation a release mints for: the installation, its account as
// GitHub names it and as the scope names it (lowercased), the names of the scope's repositories
// of that account (nil for every repository), and what the cut asks of it and leaves out.
type installationMint struct {
	installation   githubInstallation
	owner, account string
	repositories   []string
	asked          map[string]string
	leftOut        []string
}

// Mint returns a token and expiry for every installation of the App in req.Value that is not
// suspended and whose account the profile's scope (req.GitHub) names, compared without regard to
// case, and, as the whole release's expiry, the earliest of those installations' own: the first
// of its tokens to stop working is when the release stops being wholly usable. It leaves out an
// installation on any other account without asking GitHub for its token: a public App can be
// installed by anyone, and a release must not carry a token for an outsider's account. It also
// leaves out a record with no positive installation id, which names no installation a token can be
// asked for.
//
// Each token request names the profile's permissions cut to what that installation holds (cut),
// so GitHub never grants a permission the request did not name, and the scope's repositories of
// that installation's account alone, by name, as GitHub's body takes them (repositoriesOf); the
// release names each entry the cut left out (Minted.LeftOut). A cut that leaves no permission, or
// metadata alone of a profile that names more (emptyCut), refuses the release (ErrMintEmptyCut)
// before any token request, for every account, since GitHub answers a request naming none with
// every permission the installation holds, and a token of metadata alone does nothing the
// profile is for. Mint releases a token only when GitHub's answer holds no more than the request
// asked (heldBy), for every profile; a token it refuses so it revokes at GitHub first, and it
// sends GitHub the revoke of a token it minted and will not hand over: one GitHub answered in a
// form it cannot use (giveBackUnusable), and on a failed release the tokens of the installations
// it minted for before the failure (revokeUnreleased). A revoke GitHub does not confirm is
// logged, and its token stays live until it lapses. The release is today's JSON of
// installations, or, for a value-token profile, the bare token of its one account (the caller
// holds such a scope to one account, requests' gitHubScope), which is refused
// (ErrMintSeveralInstallations, inside ErrMintNarrowingRefused) before any token request where
// the scope reaches more than one installation, as the App's installations on one account under
// logins that differ only in case do: a token minted for each would leave all but the one
// released live with nobody holding them.
//
// With req.Check it finds out whether that release could be served without serving it, and it
// releases no token: it makes the release's calls in the same order, but for a token request
// that was sent and got no answer, which it does not send again (installationToken); it revokes
// each token GitHub answers with the moment it has it, and answers Minted{} with the error the
// release would have answered, nil where the release would have succeeded.
//
// A request with no scope is a programming error in the broker, refused before GitHub is asked
// anything. The value is the JSON object stored in the secret, decoded by ParseValue before
// anything else, so a value it refuses is refused as the stored value even when the scope is
// refused too; it is never part of an error. Every call it makes is retried under m.Retry, all of
// them waiting from the one wait budget this mint starts, but for a check's token request that
// was sent and got no answer, which is not sent again. Every error wraps one of
// ErrMintStoredValue, ErrMintRefused, ErrMintNarrowingRefused or ErrMintUpstream; the refusal of
// an App with no installation on the scope's accounts also wraps ErrMintNoInstallation, which is
// inside ErrMintRefused, and an empty cut ErrMintEmptyCut, which is inside ErrMintNarrowingRefused;
// the broker's check before an approval reads both.
func (m GitHubApp) Mint(ctx context.Context, req MintRequest) (_ Minted, err error) {
	scope := req.GitHub
	if scope == nil {
		// A programming error in the broker: every github-app name it serves is a profile's, and
		// the release builds the profile's scope (requests' mintRequest). ErrMintRefused is the
		// class for a release the minter itself refuses before any upstream is asked, as it is
		// for the Azure login that is not human tier; ErrMintUpstream would send an operator to
		// GitHub's status and the broker's egress, which this fault has nothing to do with, and
		// ErrMintStoredValue would blame the secret's owner for code the broker holds. It is not
		// ErrMintNoInstallation, so the check reads it as trouble and lets the request through,
		// which is right: the broker's own code is at fault and the caller's scope is not.
		return Minted{}, fmt.Errorf("%w: a github app mint needs the profile's scope it mints", ErrMintRefused)
	}
	parsed, err := ParseValue(typeGitHubApp, req.Value)
	if err != nil {
		return Minted{}, err
	}
	secret := parsed.GitHubApp
	if err := scope.refusal(secret.AppID); err != nil {
		return Minted{}, err
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	// Mint signs its own JWT, at its own clock's now: ParseValue has signed one with this key
	// already, so this signature fails only where that one would have, and as the same class.
	jwt, err := githubappjwt.Sign(secret.AppID, secret.Key, now())
	if err != nil {
		return Minted{}, fmt.Errorf("%w: sign GitHub App JWT: %w", ErrMintStoredValue, err)
	}
	mint := m.Retry.start(req.Type)
	installations, err := m.installations(ctx, mint, jwt)
	if err != nil {
		return Minted{}, err
	}
	// Every installation's cut first, so an empty one refuses the release before any token is
	// asked for, rather than after a token the release would then hand to nobody.
	var each []installationMint
	for _, installation := range installations {
		// JSON decoding leaves a missing or null id 0: no installation GitHub mints for, so the
		// record is left out rather than failing the release before a valid one is reached.
		if installation.SuspendedAt != nil || installation.ID <= 0 {
			continue
		}
		// An account with neither a login nor a slug names nothing a scope can name, so the
		// accounts filter leaves it out like any account the scope does not name.
		owner := cmp.Or(installation.Account.Login, installation.Account.Slug)
		account := strings.ToLower(owner)
		if !slices.Contains(scope.Accounts, account) {
			continue
		}
		asked, leftOut := cut(scope.Permissions, installation.Permissions)
		if emptyCut(scope.Permissions, asked) {
			return Minted{}, fmt.Errorf("%w: github app %s: its installation on %s holds none of %s", ErrMintEmptyCut, secret.AppID, owner, permissionWords(lacked(scope.Permissions, asked)))
		}
		each = append(each, installationMint{installation: installation, owner: owner, account: account, repositories: scope.repositoriesOf(account), asked: asked, leftOut: leftOut})
	}
	if len(each) == 0 {
		// ErrMintNoInstallation, not the ErrMintRefused it wraps: this is one of the two refusals
		// the broker's check refuses a request for, and it is told apart from every other 4xx by
		// this sentinel alone.
		return Minted{}, fmt.Errorf("%w: github app %s has no installation on %s", ErrMintNoInstallation, secret.AppID, strings.Join(scope.Accounts, " or "))
	}
	if scope.Value == githubname.ValueToken && len(each) > 1 {
		// The caller holds a value-token scope to one account (requests' gitHubScope), so these
		// are that account's installations, whose logins differ only in case: the release hands
		// over one bare token, and minting each would leave the others live with nobody holding
		// them. A scope of several accounts that came here anyway is refused the same way.
		owners := make([]string, 0, len(each))
		for _, one := range each {
			owners = append(owners, fmt.Sprintf("%s (installation %d)", one.owner, one.installation.ID))
		}
		return Minted{}, fmt.Errorf("%w: github app %s: its installations on %s: %s", ErrMintSeveralInstallations, secret.AppID, each[0].account, strings.Join(owners, ", "))
	}
	minted := githubMintedInstallations{Installations: make(map[string]githubMintedInstallation, len(each))}
	// A release that fails at an installation after minting for an earlier one hands over none of
	// its tokens, so it gives back every one it holds before it answers (revokeUnreleased).
	defer func() {
		if err != nil {
			m.revokeUnreleased(ctx, mint, minted, "app", secret.AppID)
		}
	}()
	var leftOut map[string][]string
	// expires is the earliest expiry of the installations minted so far, zero until the first.
	var expires time.Time
	for _, one := range each {
		token, err := m.installationToken(ctx, mint, jwt, secret.AppID, one.installation.ID, accessTokensBody(one.repositories, one.asked), req.Check)
		if err != nil {
			// installationToken has given back to GitHub any token it minted in an answer this
			// mint cannot use (giveBackUnusable).
			return Minted{}, refusal(err, secret.AppID, one.owner, one.repositories, one.asked)
		}
		if req.Check {
			// A check releases nothing, so the token goes back to GitHub the moment GitHub
			// answers with it: before heldBy judges it, since a check revokes what heldBy accepts
			// as readily as what it refuses, and under the caller's own context (revokeToken),
			// which is waiting on this answer. Blanking it leaves no token held past this point;
			// heldBy reads what GitHub says the token holds and never the token itself, so the
			// verdict below is still the one the release would get.
			if revokeErr := m.revokeToken(ctx, mint, token.Token); revokeErr != nil {
				slog.Warn(revokedCheckMessage, "app", secret.AppID, "installation", one.installation.ID, "expires_at", token.ExpiresAt, "error", revokeErr)
			}
			token.Token = ""
		}
		if err := heldBy(token, one.repositories, one.asked); err != nil {
			if !req.Check {
				// A check revoked this token already, and holds none to revoke here.
				if revokeErr := m.revokeInstallationToken(ctx, mint, token.Token); revokeErr != nil {
					slog.Warn(revokedWiderMessage, "app", secret.AppID, "installation", one.installation.ID, "expires_at", token.ExpiresAt, "error", revokeErr)
				}
			}
			return Minted{}, fmt.Errorf("%w: github app %s: installation %d: %w", ErrMintUpstream, secret.AppID, one.installation.ID, err)
		}
		minted.Installations[one.owner] = githubMintedInstallation{
			InstallationID: one.installation.ID,
			Token:          token.Token,
			ExpiresAt:      token.ExpiresAt,
		}
		if len(one.leftOut) > 0 {
			if leftOut == nil {
				leftOut = map[string][]string{}
			}
			leftOut[one.account] = one.leftOut
		}
		// An answer that carried no expires_at leaves the token's zero, which says nothing about
		// when it ends and so never becomes the release's expiry.
		if !token.ExpiresAt.IsZero() && (expires.IsZero() || token.ExpiresAt.Before(expires)) {
			expires = token.ExpiresAt
		}
	}
	if req.Check {
		// Every token this check minted is revoked and none is held, so there is nothing to
		// encode and the answer is the verdict alone: the release this check stood for would
		// have succeeded. Marshalling a map of strings and times cannot fail, so the encode
		// below holds no verdict a check could miss by returning here.
		return Minted{}, nil
	}
	if scope.Value == githubname.ValueToken {
		// The check above has refused a value-token scope of more than one installation, so the
		// one installation minted is the release.
		return Minted{Value: minted.Installations[each[0].owner].Token, Expires: expires, LeftOut: leftOut}, nil
	}
	encoded, err := json.Marshal(minted)
	if err != nil {
		return Minted{}, fmt.Errorf("%w: encode GitHub App installations: %w", ErrMintUpstream, err)
	}
	return Minted{Value: string(encoded), Expires: expires, LeftOut: leftOut}, nil
}

// at is ref joined onto the GitHub API's URL path: Base (BROKER_AIRLOCK_TEST_GITHUB_API_BASE,
// which a local stack points at a stand-in GitHub) or githubAPIBase, with ref's path onto its
// path, its query kept ahead of ref's own and its fragment, which no request sends, dropped
// (urljoin.Join). ref appended to that URL's text would sit inside a query or a fragment, and
// GitHub, or the stand-in, would be asked another path. A base that does not parse or names no
// host is named by the setting and never printed: it is a deployment's own value, which can carry
// a credential in its user information or its query, where url.Parse's error, which
// http.NewRequest's carries, quotes the URL whole.
func (m GitHubApp) at(ref string) (string, error) {
	joined, err := urljoin.Join(cmp.Or(m.Base, githubAPIBase), ref)
	if err != nil {
		return "", errors.New("BROKER_AIRLOCK_TEST_GITHUB_API_BASE is not an absolute URL with a host")
	}
	return joined.String(), nil
}

// request is a call to GitHub's API at ref joined onto its URL (at), with bearer as its credential
// (the App's JWT, or an installation token revoking itself), carrying body as JSON when it is not
// nil. A joined URL url.URL.String writes as text url.Parse refuses again (an IPv6 zone escape) is
// named in fixed words, since http.NewRequest's error carries url.Parse's, which quotes the URL
// whole, user information and query included.
func (m GitHubApp) request(ctx context.Context, method, ref, bearer string, body []byte) (*http.Request, error) {
	target, err := m.at(ref)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, errors.New("the URL joined from BROKER_AIRLOCK_TEST_GITHUB_API_BASE does not parse")
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func (m GitHubApp) installations(ctx context.Context, mint *mintRetry, jwt string) ([]githubInstallation, error) {
	const what = "list GitHub App installations"
	var installations []githubInstallation
	for page := 1; ; page++ {
		request, err := m.request(ctx, http.MethodGet, fmt.Sprintf("/app/installations?per_page=100&page=%d", page), jwt, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrMintUpstream, what, err)
		}
		payload, err := mintCall(mint, m.HTTP, request, what, "GitHub", http.StatusOK, mintResponseLimit, true, nil)
		if err != nil {
			return nil, err
		}
		var entries []githubInstallation
		if json.Unmarshal(payload, &entries) != nil {
			return nil, fmt.Errorf("%w: decode GitHub App installations: not a JSON array of installations", ErrMintUpstream)
		}
		installations = append(installations, entries...)
		if len(entries) < 100 {
			return installations, nil
		}
	}
}

// installationToken asks GitHub for installationID's token, sending body, GitHub's access_tokens
// body, when it is not nil. GitHub has minted a token once it answers 201, so each answer of that
// status this cannot use - cut off part of the way, past its cap, not a JSON object of the token's
// fields, or holding no token or no expiry - has the token it names given back to GitHub
// (giveBackUnusable) before the request is sent again or the failure is answered: mintCall calls
// giveBackHook for an answer it could not read whole, and this gives back one that does not
// decode. check is whether this is a check's request (MintRequest.Check), which is not sent again
// once an attempt was sent and got no answer - any failure with no answer but one known to come
// before a connection, a name that did not resolve, a failed dial, a proxy's refused tunnel or a
// certificate the broker did not trust (mintCall's resendLost, mintAttempt): GitHub may have
// received it whole and minted a token nobody will use, which nothing can name, and sending it
// again would mint another for a verdict alone. An attempt GitHub answered with a failure, a 5xx,
// a 408 or a rate limit, names no token, and a check sends it again as any call is sent again,
// revoking the token a later attempt mints as it revokes every token it gets; so does an attempt
// that failed before a connection, which minted nothing. A release sends each of them again,
// since its caller is waiting for the token itself, and each attempt GitHub served whose answer
// it loses leaves its token live until it lapses. appID names the App in the lines a give-back
// logs.
func (m GitHubApp) installationToken(ctx context.Context, mint *mintRetry, jwt, appID string, installationID int64, body []byte, check bool) (githubInstallationToken, error) {
	const what = "mint GitHub App installation token"
	request, err := m.request(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", jwt, body)
	if err != nil {
		return githubInstallationToken{}, fmt.Errorf("%w: %s: %w", ErrMintUpstream, what, err)
	}
	payload, err := mintCall(mint, m.HTTP, request, what, "GitHub", http.StatusCreated, accessTokensResponseLimit, !check,
		m.giveBackHook(ctx, mint, appID, installationID))
	if err != nil {
		return githubInstallationToken{}, err
	}
	var token githubInstallationToken
	// encoding/json's syntax errors quote the offending byte, so this decode does not wrap its
	// error: GitHub's token is a secret.
	if json.Unmarshal(payload, &token) != nil {
		err := fmt.Errorf("%w: decode GitHub App installation token: not a JSON object of its fields", ErrMintUpstream)
		m.giveBackUnusable(ctx, mint, appID, installationID, answeredToken(payload), err)
		return githubInstallationToken{}, err
	}
	if token.Token == "" || token.ExpiresAt.IsZero() {
		err := fmt.Errorf("%w: GitHub returned an incomplete installation token", ErrMintUpstream)
		m.giveBackUnusable(ctx, mint, appID, installationID, token.Token, err)
		return githubInstallationToken{}, err
	}
	return token, nil
}
