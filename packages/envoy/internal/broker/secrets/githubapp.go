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

	"github.com/sjawhar/envoy/internal/githubappjwt"
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
}

// githubInstallationToken is GitHub's answer to a token request: the token, its expiry, and what
// GitHub says the token holds, which Mint checks against a narrowing (GitHubNarrowing.heldBy).
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

// githubAccessTokensBody is GitHub's POST /app/installations/{id}/access_tokens body for a narrowed
// token: the repositories it reaches and the permissions it holds, each left out when the
// narrowing does not name it.
type githubAccessTokensBody struct {
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

// readOnlyPermissions are the permissions a read-only narrowing asks GitHub for, and the most a
// token minted for one may hold. They are the Legion daemon's own read set (its appauth
// readPermissions), so a caller reading a repository over a broker-released token reads what one
// minted from a configured key reads: its contents, its pull requests and the Actions runs, jobs
// and job logs its merges produced.
var readOnlyPermissions = map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read", "actions": "read"}

// optionalReadOnlyPermission is the one permission of readOnlyPermissions an installation may not
// hold: an App is granted Actions only once each installation's account approves that grant,
// while the other three are a GitHub App's ordinary repository reads. GitHub answers a token
// request naming a permission its installation does not hold HTTP 422 and mints nothing rather
// than leaving that one out, so a read-only narrowing GitHub refuses is asked for again without
// it (Mint): an App that is not granted Actions releases the other three rather than nothing.
const optionalReadOnlyPermission = "actions"

// readOnlyPermissionsWithoutActions is the body of that second request. It is built once and
// never written to, as readOnlyPermissions is.
var readOnlyPermissionsWithoutActions = func() map[string]string {
	reduced := maps.Clone(readOnlyPermissions)
	delete(reduced, optionalReadOnlyPermission)
	return reduced
}()

// narrowsToken reports whether n narrows the token GitHub mints, not only which installation it is
// minted for: whether it sends GitHub an access_tokens body.
func (n *GitHubNarrowing) narrowsToken() bool {
	return n != nil && (len(n.Repositories) > 0 || n.ReadOnly)
}

// accessTokensBody is narrowing as GitHub's access_tokens body, nil for a narrowing that names
// neither repositories nor read-only (it narrows to its owner alone) and for none, whose tokens
// are asked for with no body, as before narrowing existed. permissions is what a read-only
// narrowing asks GitHub for: readOnlyPermissions, or readOnlyPermissionsWithoutActions for the
// second request Mint makes of an installation that holds no actions. Marshalling a struct of a
// string slice and a string map cannot fail, so its error is dropped.
func (n *GitHubNarrowing) accessTokensBody(permissions map[string]string) []byte {
	if !n.narrowsToken() {
		return nil
	}
	body := githubAccessTokensBody{Repositories: n.Repositories}
	if n.ReadOnly {
		body.Permissions = permissions
	}
	encoded, _ := json.Marshal(body)
	return encoded
}

// githubRepositoryAnswerBound is more than any one repository object GitHub's access_tokens answer
// can hold: measured objects are 4.9 to 5.3 KB, and a 100-character name repeated in every URL, a
// 350-character description escaped six bytes a character and a 39-character login repeated in
// the owner's URLs add under 10 KB more.
const githubRepositoryAnswerBound = 64 << 10

// accessTokensResponseLimit caps what Mint reads of a token answer, which names every repository
// a narrowed token reaches: the common cap plus githubRepositoryAnswerBound for each of
// MaxNarrowedRepositories. GitHub has minted the token by the time the answer arrives, and an
// answer read in part cannot be released, and its revoke is sent only where the part read names
// the token (giveBackUnusable), so the cap admits whole every answer a narrowing within the
// maximum can get.
const accessTokensResponseLimit = mintResponseLimit + MaxNarrowedRepositories*githubRepositoryAnswerBound

// refusal is err, GitHub's answer to the token request for owner, unless it is the 422 GitHub
// gives a narrowed token it cannot mint: that is the caller's narrowing (ErrMintNarrowingRefused),
// named by what it likely got wrong, since GitHub's own text is never quoted. A 422 to a request
// that sent no body narrowed nothing, and stays the App's; a 422 that is a rate limit is no verdict
// on the narrowing, and stays the upstream's. A read-only narrowing reaches it only once the
// token was asked for again without actions (Mint, optionalReadOnlyPermission), so the one grant
// an App is not always given is not among the causes it names.
func (n *GitHubNarrowing) refusal(err error, appID, owner string) error {
	var status *mintStatusError
	if !n.narrowsToken() || !errors.As(err, &status) || status.code != http.StatusUnprocessableEntity || status.rateLimited {
		return err
	}
	var causes []string
	if len(n.Repositories) > 0 {
		causes = append(causes, fmt.Sprintf("a repository it names (%s) that the installation does not reach, such as a misspelled name", strings.Join(n.Repositories, ", ")))
	}
	if n.ReadOnly {
		causes = append(causes, "contents: read, metadata: read or pull_requests: read, which the App does not hold")
	}
	return withAttempts(fmt.Errorf("%w: github app %s: GitHub answered HTTP 422 to the narrowed token for %s: likely %s; the caller fixes the narrowing", ErrMintNarrowingRefused, appID, owner, strings.Join(causes, ", or ")), err)
}

// refusedPermissions reports GitHub refusing the permissions a read-only narrowing's token
// request named (a 422): the installation holds fewer than the body asked for, and GitHub minted
// nothing. A 422 that is a rate limit is the upstream's trouble and no verdict on the
// permissions, and a narrowing that is not read-only named none to be refused.
func (n *GitHubNarrowing) refusedPermissions(err error) bool {
	var status *mintStatusError
	return n != nil && n.ReadOnly && errors.As(err, &status) &&
		status.code == http.StatusUnprocessableEntity && !status.rateLimited
}

// heldBy checks that token, as GitHub describes it, holds no more than n asked: for repositories, a
// selection of named repositories, each one n names (GitHub names them in its own case); for
// read-only, named permissions, each at most readOnlyPermissions'. GitHub applies the
// access_tokens body and refuses what it cannot grant, so a token past it means something between
// the broker and GitHub dropped or widened the body; refusing it keeps the narrowing the release
// answers with true by construction. Its errors name the rule the answer failed and never a value
// the answer holds, which an upstream that is not GitHub can fill with what the request carried. A
// token not narrowed is not checked.
func (n *GitHubNarrowing) heldBy(token githubInstallationToken) error {
	if !n.narrowsToken() {
		return nil
	}
	if len(n.Repositories) > 0 {
		if token.RepositorySelection != "selected" {
			return errors.New(`GitHub's token has a repository selection other than "selected", so it may reach more than the narrowing names`)
		}
		if len(token.Repositories) == 0 {
			return errors.New("GitHub's token names no repositories, so it may reach more than the narrowing names")
		}
		for _, repository := range token.Repositories {
			if !slices.ContainsFunc(n.Repositories, func(name string) bool { return strings.EqualFold(name, repository.Name) }) {
				return errors.New("GitHub's token reaches a repository the narrowing does not name")
			}
		}
	}
	if n.ReadOnly {
		if len(token.Permissions) == 0 {
			return errors.New("GitHub's token names no permissions, so it may hold more than read access")
		}
		for _, permission := range slices.Sorted(maps.Keys(token.Permissions)) {
			if _, asked := readOnlyPermissions[permission]; !asked {
				return errors.New("GitHub's token holds a permission past contents, metadata, pull requests and actions")
			}
			if token.Permissions[permission] != readOnlyPermissions[permission] {
				return errors.New("GitHub's token holds more than read access to contents, metadata, pull requests or actions")
			}
		}
	}
	return nil
}

// revokeTimeout bounds the revoke of a token Mint minted and will not release.
const revokeTimeout = 10 * time.Second

// revokedWiderMessage is logged, at WARN, when GitHub does not confirm the revoke of a narrowed
// token it minted wider than asked, followed by app=, installation=, expires_at= (when the token
// lapses at GitHub without the revoke) and error=, which never holds the token.
const revokedWiderMessage = "broker: GitHub did not revoke a narrowed token it minted wider than asked; it lapses at its expiry"

// revokedCheckMessage is logged, at WARN, when GitHub does not confirm the revoke of a token a
// check minted — the caller's context ending first is the likeliest cause — followed by app=,
// installation=, expires_at= (when the token lapses at GitHub without the revoke) and error=,
// which never holds the token. It is a line of its own rather than revokedWiderMessage because
// that one names a fault a check usually does not have: a check revokes every token it mints,
// nearly always one GitHub minted exactly as asked, and its reader would go looking at the
// narrowing for nothing.
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

// missingActionsMessage is logged, at WARN, when GitHub refuses the permissions a read-only
// narrowing asked for and the token is minted without actions, followed by app= and
// installation=. It never holds the token. A check (MintRequest.Check) does not log it: it
// releases no token, so its words about the token released for the installation would be
// untrue, and one check of a narrowing mints as many times as it has repositories to probe,
// each of which would write the line again for the one release behind it.
const missingActionsMessage = "broker: a GitHub App installation does not hold actions, so the read-only token released for it reads no Actions runs, jobs or job logs; grant the App Actions: Read and approve it on that account"

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
	request, err := githubRequest(ctx, http.MethodDelete, m.base()+"/installation/token", token, nil)
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

// GiveBack tries to revoke every installation token in value, a github-app release's JSON object
// of installations as Mint answered it, which the caller minted for the secret name and hands to
// nobody (requests.Machine.GiveBacks): the broker's release of a grant that failed after this name
// was minted, or that holds this name behind the airlock, whose session gets placeholders while the
// proxy mints a value of its own. Each revoke runs under the detached bound a release's revoke has,
// whatever ctx does, in one attempt (startOnce): nobody waits on it, and a retry would draw on the
// process-wide budget the mints a caller waits on retry from, which a GitHub outage leaves short
// while every airlock release of a github-app name gives a token back. A revoke GitHub does not
// confirm is logged with name= (revokedUnreleasedMessage) and leaves its token live until it
// lapses. A value that is no such object names no token, and is left alone.
func (m GitHubApp) GiveBack(ctx context.Context, name, value string) {
	var minted githubMintedInstallations
	if json.Unmarshal([]byte(value), &minted) != nil {
		return
	}
	m.revokeUnreleased(ctx, m.Retry.startOnce(typeGitHubApp), minted, "name", name)
}

// Mint returns a token and expiry for every installation of the App in req.Value that is not
// suspended and whose account req.GitHubOwners lists, compared without regard to case, and, as
// the whole release's expiry, the earliest of those installations' own: the first of its tokens
// to stop working is when the release stops being wholly usable. It leaves
// out an installation on any other account without asking GitHub for its token: a public App can
// be installed by anyone, and a release must not carry a token for an outsider's account. It also
// leaves out a record with no positive installation id, which names no installation a token can be
// asked for. With req.GitHubNarrowing it mints for the narrowing's owner alone, which
// req.GitHubOwners must list (it refuses, asking GitHub nothing, otherwise), sending the narrowing
// as GitHub's access_tokens body, and releases the token only when GitHub's answer holds no more
// than the narrowing asked (heldBy); a token it refuses so it revokes at GitHub first, and it
// sends GitHub the revoke of a token it minted and will not hand over: one GitHub answered in a
// form it cannot use (giveBackUnusable), and on a failed release the tokens of the installations
// it minted for before the failure (revokeUnreleased). A revoke GitHub does not confirm is logged,
// and its token stays live until it lapses.
// With req.Check it finds out whether that release could be served without serving it, and it
// releases no token. For a narrowing that names repositories or read-only it makes the release's
// calls in the same order, but for a token request that may have reached GitHub and failed, which
// it does not send again (installationToken); it revokes each token GitHub answers with the moment
// it has it, and answers Minted{} with the error the release would have answered, nil where the
// release would have succeeded. For a narrowing to its owner alone it lists the App's
// installations and asks GitHub for no token. Such a narrowing sends GitHub no access_tokens body,
// so GitHub's answer to the token request can refuse the App but never the narrowing, and heldBy
// checks nothing of it: the check answers the release's refusal of an account with no
// installation, and nil for an account with one, even where the release's token request would
// still fail. A token minted there would hold every permission of the installation before anybody
// approved the request.
// A check needs req.GitHubNarrowing, since a release without one is the owner-wide one the
// broker already makes; a check without one is refused before GitHub is asked anything.
// The value is the JSON object stored in the secret, decoded by ParseValue before anything else,
// so a value it refuses is refused as the stored value even when the narrowing is refused too;
// it is never part of an error. Every call it makes is retried under m.Retry, all of them
// waiting from the one wait budget this mint starts, but for a check's token request that may have
// reached GitHub and failed, which is not sent again. Every error wraps one of ErrMintStoredValue,
// ErrMintRefused, ErrMintNarrowingRefused or ErrMintUpstream; the refusal of an App with no
// installation on the account also wraps ErrMintNoInstallation, which is inside ErrMintRefused and
// which the broker's narrowing check reads.
func (m GitHubApp) Mint(ctx context.Context, req MintRequest) (_ Minted, err error) {
	if req.Check && req.GitHubNarrowing == nil {
		// A programming error in the broker: the caller asked this minter to find out about a
		// narrowing it did not name. ErrMintRefused is the class for a release the minter itself
		// refuses before any upstream is asked, as it is for the Azure login that is not human
		// tier; ErrMintUpstream would log MintErrorClass's "upstream" and send an operator to
		// GitHub's status and the broker's egress, which this fault has nothing to do with, and
		// ErrMintStoredValue would blame the secret's owner for code the broker holds. It is not
		// ErrMintNoInstallation, so the check reads it as trouble and lets the request through,
		// which is right: the broker's own code is at fault and the caller's narrowing is not.
		return Minted{}, fmt.Errorf("%w: a github app mint check needs the narrowing it checks", ErrMintRefused)
	}
	parsed, err := ParseValue(typeGitHubApp, req.Value)
	if err != nil {
		return Minted{}, err
	}
	secret := parsed.GitHubApp
	owners := req.GitHubOwners
	narrowing := req.GitHubNarrowing
	if narrowing != nil {
		// requests.Machine.Values refuses an unlisted owner before any minter runs; this is the
		// second barrier.
		owner := strings.ToLower(narrowing.Owner)
		if !slices.Contains(owners, owner) {
			return Minted{}, fmt.Errorf("%w: github app %s: %s is not an account its github-owners tag lists", ErrMintNarrowingRefused, secret.AppID, narrowing.Owner)
		}
		owners = []string{owner}
	}
	body := narrowing.accessTokensBody(readOnlyPermissions)
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
	minted := githubMintedInstallations{Installations: make(map[string]githubMintedInstallation, len(owners))}
	// A release that fails at an installation after minting for an earlier one hands over none of
	// its tokens, so it gives back every one it holds before it answers (revokeUnreleased).
	defer func() {
		if err != nil {
			m.revokeUnreleased(ctx, mint, minted, "app", secret.AppID)
		}
	}()
	// expires is the earliest expiry of the installations minted so far, zero until the first.
	var expires time.Time
	for _, installation := range installations {
		// JSON decoding leaves a missing or null id 0: no installation GitHub mints for, so the
		// record is left out rather than failing the release before a valid one is reached.
		if installation.SuspendedAt != nil || installation.ID <= 0 {
			continue
		}
		// An account with neither a login nor a slug names nothing github-owners can list, so the
		// owners filter leaves it out like any unlisted account.
		owner := cmp.Or(installation.Account.Login, installation.Account.Slug)
		if !slices.Contains(owners, strings.ToLower(owner)) {
			continue
		}
		if req.Check && !narrowing.narrowsToken() {
			// A narrowing to its owner alone: the listing has given this check its whole verdict,
			// an installation on the account. Its token request would carry no body, so GitHub's
			// answer could refuse the App but not the narrowing. A token minted for it would hold
			// every permission of the installation, and it would stay live at GitHub for up to an
			// hour wherever the revoke did not finish.
			return Minted{}, nil
		}
		token, err := m.installationToken(ctx, mint, jwt, secret.AppID, installation.ID, body, req.Check)
		if narrowing.refusedPermissions(err) {
			// The installation holds fewer permissions than the body named, and actions is the
			// one of them an App is not always granted: ask once more without it, so an App that
			// reads contents, metadata and pull requests releases those rather than nothing
			// (optionalReadOnlyPermission). heldBy still bounds what the answer may hold.
			token, err = m.installationToken(ctx, mint, jwt, secret.AppID, installation.ID, narrowing.accessTokensBody(readOnlyPermissionsWithoutActions), req.Check)
			if err == nil && !req.Check {
				// A check releases nothing, so the line would say a token was released that
				// never was, and it would be written once for each mint the check makes
				// (missingActionsMessage).
				slog.Warn(missingActionsMessage, "app", secret.AppID, "installation", installation.ID)
			}
		}
		if err != nil {
			// installationToken has given back to GitHub any token it minted in an answer this
			// mint cannot use (giveBackUnusable).
			return Minted{}, narrowing.refusal(err, secret.AppID, owner)
		}
		if req.Check {
			// A check releases nothing, so the token goes back to GitHub the moment GitHub
			// answers with it: before heldBy judges it, since a check revokes what heldBy accepts
			// as readily as what it refuses, and under the caller's own context (revokeToken),
			// which is waiting on this answer. Blanking it leaves no token held past this point;
			// heldBy reads what GitHub says the token holds and never the token itself, so the
			// verdict below is still the one the release would get.
			if revokeErr := m.revokeToken(ctx, mint, token.Token); revokeErr != nil {
				slog.Warn(revokedCheckMessage, "app", secret.AppID, "installation", installation.ID, "expires_at", token.ExpiresAt, "error", revokeErr)
			}
			token.Token = ""
		}
		if err := narrowing.heldBy(token); err != nil {
			if !req.Check {
				// A check revoked this token already, and holds none to revoke here.
				if revokeErr := m.revokeInstallationToken(ctx, mint, token.Token); revokeErr != nil {
					slog.Warn(revokedWiderMessage, "app", secret.AppID, "installation", installation.ID, "expires_at", token.ExpiresAt, "error", revokeErr)
				}
			}
			return Minted{}, fmt.Errorf("%w: github app %s: installation %d: %w", ErrMintUpstream, secret.AppID, installation.ID, err)
		}
		minted.Installations[owner] = githubMintedInstallation{
			InstallationID: installation.ID,
			Token:          token.Token,
			ExpiresAt:      token.ExpiresAt,
		}
		// An answer that carried no expires_at leaves the token's zero, which says nothing about
		// when it ends and so never becomes the release's expiry.
		if !token.ExpiresAt.IsZero() && (expires.IsZero() || token.ExpiresAt.Before(expires)) {
			expires = token.ExpiresAt
		}
	}
	if len(minted.Installations) == 0 {
		// ErrMintNoInstallation, not the ErrMintRefused it wraps: this is the one refusal of
		// GitHub's the broker's narrowing check refuses a request for, and it is told apart
		// from every other 4xx by this sentinel alone.
		if narrowing != nil {
			return Minted{}, fmt.Errorf("%w: github app %s has no installation on %s", ErrMintNoInstallation, secret.AppID, narrowing.Owner)
		}
		return Minted{}, fmt.Errorf("%w: github app %s has no installation on an account its github-owners tag lists", ErrMintNoInstallation, secret.AppID)
	}
	if req.Check {
		// Every token this check minted is revoked and none is held, so there is nothing to
		// encode and the answer is the verdict alone: the release this check stood for would
		// have succeeded. Marshalling a map of strings and times cannot fail, so the encode
		// below holds no verdict a check could miss by returning here.
		return Minted{}, nil
	}
	encoded, err := json.Marshal(minted)
	if err != nil {
		return Minted{}, fmt.Errorf("%w: encode GitHub App installations: %w", ErrMintUpstream, err)
	}
	return Minted{Value: string(encoded), Expires: expires}, nil
}

func (m GitHubApp) base() string {
	return cmp.Or(strings.TrimSuffix(m.Base, "/"), githubAPIBase)
}

// githubRequest is a call to GitHub's API with bearer as its credential (the App's JWT, or an
// installation token revoking itself), carrying body as JSON when it is not nil.
func githubRequest(ctx context.Context, method, target, bearer string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
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
		request, err := githubRequest(ctx, http.MethodGet, fmt.Sprintf("%s/app/installations?per_page=100&page=%d", m.base(), page), jwt, nil)
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
// once an attempt may have reached GitHub and failed - any failure but one known to come before a
// connection, a name that did not resolve, a failed dial, a proxy's refused tunnel or a
// certificate the broker did not trust (mintCall's resendLost, mintAttempt): GitHub may have
// received it whole and minted a token nobody will use, which nothing can name, and sending it
// again would mint another for a verdict alone. An attempt that failed before a connection minted
// nothing, and a check sends it again as any call is sent again. A release sends either again,
// since its caller is waiting for the token itself, and each attempt GitHub served whose answer
// it loses leaves its token live until it lapses. appID names the App in the lines a give-back
// logs.
func (m GitHubApp) installationToken(ctx context.Context, mint *mintRetry, jwt, appID string, installationID int64, body []byte, check bool) (githubInstallationToken, error) {
	const what = "mint GitHub App installation token"
	request, err := githubRequest(ctx, http.MethodPost, m.base()+"/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", jwt, body)
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
