package oauth

import (
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc8693"
	"authelia.com/provider/oauth2/token/jose"
	"authelia.com/provider/oauth2/token/jwt"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
)

// tokenTypeAccessToken is RFC 8693's own name for an access token: the only kind this provider's
// exchange mints, the kind it asks for by default, and the kind a dev box session presents its
// machine login's token as. It is the name the client registration lists, so both read it from
// the one place the broker and its callers share.
const tokenTypeAccessToken = oauthwire.SubjectTokenTypeAccessToken

// The form parameter of an exchange request the broker reads itself; the library reads the rest
// (RFC 8693 Section 2.1). The session name a caller asks to write under is oauthwire.FormSession,
// whether the trade wants a launcher credential is oauthwire.FormLauncherCredential, the
// projects the token names are oauthwire.FormProjects, and the cut a narrowed token reads at is
// oauthwire.FormAsOf, the broker's own four parameters, which the clients that send them read
// from that same declaration.
const (
	// FormSubjectTokenType is the request's subject_token_type, which says which kind of
	// identity the caller is presenting and so which decision the exchange makes.
	FormSubjectTokenType = "subject_token_type"
)

// The extra claims an exchanged access token carries, which are what an introspection answers
// from: this provider's access tokens are opaque, so the claims live on the stored session rather
// than in the token's own bytes.
const (
	// ClaimSession is the session the token's writes are recorded as.
	ClaimSession = "session"
	// ClaimPerson is the person the session acts for.
	ClaimPerson = "person"
	// ClaimService is the service a token that names no person is, a machine's fleet, the service
	// a read entry names, the account of an account entry naming no person, or a job entry's
	// listed identity (exchange.Claims.Service), and is empty for every token that names one. It
	// is written always, for the reason ClaimProjectScope is: a reader of the row never has to
	// take an absent service as either "no service" or "a claim this build of the broker did not
	// write".
	ClaimService = "service"
	// ClaimMachine is the machine a person signed in on acted through: the Google account the
	// trade's actor token named (exchange.Claims.Actor). It is set only beside ClaimPerson, and
	// only for a person's sign-in; it is absent from every other token, which no machine acted
	// for. The RFC 8693 'act' claim the library writes beside it records the same delegation as
	// the actor token's issuer spells it, a Google subject rather than an account, so this is
	// the name an introspection can answer and a reader can act on.
	ClaimMachine = "machine"
	// ClaimProjectScope is what ClaimProjects means, one of oauthwire.ProjectScopeNamed,
	// oauthwire.ProjectScopePerson, oauthwire.ProjectScopeNone, oauthwire.ProjectScopeResults or
	// oauthwire.ProjectScopePublic: none for a read entry's token, which is granted
	// oauthwire.ScopeRead beside it; results for a result entry's token alone, which is granted
	// oauthwire.ScopeResults beside it; or for a trade that asked for oauthwire.ScopeRead the
	// narrowed word of named, person or none (oauthwire.ProjectScopeReadNamed,
	// oauthwire.ProjectScopeReadPerson, oauthwire.ProjectScopeReadNone), whose projects mean what
	// its base word's do (writeProjectScope); and public for a job entry's alone. A job's token is
	// named or public as its entry's list is, and asks Dispatch nothing (projectAccess), because
	// the operator's entry chose its list and it names no person whose sight of a project there
	// is to check; no trade narrows it. It is written always, so a reader never has to take an
	// absent list as "every project", "no project at all" or a claim nobody decided.
	ClaimProjectScope = "project_scope"
	// ClaimProjects is the token's list of projects in its stored spelling: the keys separated by
	// one space (oauthwire.JoinProjects), and the empty string for none, which is all a token of
	// the none scope ever carries. A job's token carries its entry's whole list, whatever the trade
	// named, with no question to Dispatch, since the operator's entry chose it: one project under
	// the named scope, and the private projects under the public scope, beside which it reaches
	// every public project, which no list names. It is written always, for the reason
	// ClaimProjectScope is. It is a string and not a list because the session's extra claims are
	// stored as JSON and read back as map[string]any, so a []string written here would come back as
	// []any and read as no projects at all; one grammar serves the form parameter, this claim and
	// AGENT_SECRETS_PROJECTS. An introspection answers it as a list.
	ClaimProjects = "projects"
	// ClaimAsOf is a narrowed token's cut, the past time it reads at (oauthwire.FormAsOf), in UTC
	// to the microsecond (cutLayout). It is absent from every token whose trade named no cut, and
	// only a trade that asks for oauthwire.ScopeRead may name one (readCut).
	ClaimAsOf = "as_of"
)

// subjectTokenAlgorithms are the signature algorithms a JWT subject token may be signed with: the
// asymmetric ones, which are the only ones an issuer's published key set can hold. The library
// parses a subject token allowing every algorithm including 'none' and leaves the choice to the
// type's ValidateFunc, so refusing the rest here is what keeps an unsigned token, or one signed
// with a secret the issuer shares with somebody, from ever reaching a key.
var subjectTokenAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

// The lifetime tolerance each JWT subject token type allows, which the library applies to both
// ends: a token whose iat is older than the window, and one whose exp is further ahead than it,
// are refused.
const (
	// kubernetesTolerance admits a projected service-account token's whole life. Kubernetes
	// bounds one at 24 hours, so a shorter window would refuse a freshly minted token outright
	// for expiring too far in the future. It is also what a pod-session registration is kept for
	// past its last write (oauthstore.PodSessionRetention), since a registration must outlive
	// every token the pod it was written for can still present.
	kubernetesTolerance = 24 * time.Hour
	// githubTolerance admits a workflow run's token, which lives minutes, while bounding how
	// stale an iat the exchange accepts.
	githubTolerance = time.Hour
	// googleTolerance admits a machine's Google identity token, which Google mints for an hour
	// and which its metadata server hands out again until it is nearly spent, so the window has
	// to cover a whole lifetime as the GitHub one does rather than a fresh token alone.
	googleTolerance = time.Hour
)

// KeySource answers the public key a token's header names, which is what the library's
// jwt.Keyfunc has to return: *oidc.Keys is one, reading the issuer's published set.
type KeySource interface {
	// Key is the public key a token naming kid and signed with alg is checked against.
	Key(ctx context.Context, kid, alg string) (crypto.PublicKey, error)
}

// IssuerConfig is one issuer whose tokens the exchange accepts as a subject token: a Kubernetes
// cluster, or GitHub. Every field is required of a configured issuer and the zero value is an
// issuer the broker was not told about, whose token type is then not registered at all — so a
// caller presenting it is refused by the library before any handler of the broker's runs.
type IssuerConfig struct {
	// Issuer is the `iss` the subject token must carry, which the library compares exactly.
	Issuer string
	// Audience is what the subject token's `aud` must carry. The library checks a subject
	// token's issuer and its expiry and never its audience, so the exchange checks this itself:
	// a token minted for another service is not spendable here.
	Audience string
	// Keys is the issuer's published signing keys, which the token's `kid` and `alg` are
	// resolved against.
	Keys KeySource
}

// configured reports whether the broker was told about this issuer at all.
func (c IssuerConfig) configured() bool {
	return c.Issuer != "" || c.Audience != "" || c.Keys != nil
}

// validate refuses an issuer that is named but not complete: half a configuration would register
// a token type whose tokens could not be checked, which is worse than not accepting that kind of
// caller at all. where names the issuer in the refusal.
func (c IssuerConfig) validate(where string) error {
	if !c.configured() {
		return nil
	}
	if c.Issuer == "" || c.Audience == "" || c.Keys == nil {
		return fmt.Errorf("oauth: the %s subject token issuer is configured in part (issuer %q, audience %q, keys %t); it needs all three or none", where, c.Issuer, c.Audience, c.Keys != nil)
	}
	return nil
}

// ExchangeConfig is what the RFC 8693 token exchange decides and verifies with. None of it is the
// provider's to decide: the broker's wiring reads it from the deployment's configuration and
// hands it over whole.
type ExchangeConfig struct {
	// Policy is the listed identities and what each may write. A nil Policy is a deployment
	// that has listed none, which New reads as the empty set: the exchange is composed whether
	// or not any identity or issuer is, because the token-exchange grant is the exchange
	// client's only grant and a caller it refuses must be refused by name.
	Policy *exchange.Set
	// Kubernetes is the cluster issuer a pod's projected service-account token comes from.
	Kubernetes IssuerConfig
	// GitHub is the issuer a continuous-integration workflow run's token comes from.
	GitHub IssuerConfig
	// Google is the issuer a machine's identity token comes from, Google's own in a deployment
	// and a stand-in for it on a dev stack.
	Google IssuerConfig
	// MachineCredentials creates or re-uses the launcher credential a machine holds, in the
	// transaction the exchange carries (oauthstore.TxFrom), and answers its id. Every secrets
	// route is an enrollment's, and an enrollment is made under a launcher credential and a
	// proof signed by its key, so without this a machine holding a Dispatch access token could
	// still read no secret. It is required of a broker that accepts a Google subject token and
	// unused by one that does not; enroll.Service.IssueTokenLoginTx is what the broker's wiring
	// hands over.
	MachineCredentials MachineCredentials
	// PersonAudience is what a person's own Google identity token's `aud` must carry
	// (BROKER_SIGNIN_DEVICE_AUDIENCE): the device-flow client `agent-secrets login` runs
	// Google's device grant as. It is an audience of its own beside Google.Audience, which is a
	// machine's, so neither token is spendable where the other is: a machine's identity token
	// cannot be presented as the person it acts for, and a person's sign-in token cannot be
	// presented as a machine. Empty is a broker that accepts no person's sign-in at all, whose
	// person subject token type and machine actor token type are then not registered, so a
	// caller presenting either is refused by the library before any handler of the broker's
	// runs. Its issuer and keys are Google's own, since both tokens come from the one issuer.
	PersonAudience string
	// PersonSignIns records the sign-in of the person a trade names on the box key its DPoP
	// proof presented, in the transaction the exchange carries, and refuses with ErrSignInRevoked
	// a trade whose key that person has revoked. It is what makes a revoked sign-in stop working
	// at the broker rather than at the person's own machine, and it is required of a broker that
	// accepts a person's Google identity token and unused by one that does not.
	PersonSignIns PersonSignIns
	// Projects asks Dispatch whether a project is private, whether a person may see it, and what
	// kind of member a person is (BROKER_PROJECT_ACCESS_URL). The exchange asks it before it opens
	// its transaction, about the claims it decided: a token acting as its person names only
	// projects that person sees, and a restricted member's names at least one; a token an
	// operator's entry maps names a project its entry's person sees. Nil is a broker with no such
	// check, which mints every token naming no project as it always has and refuses a person's
	// trade that names one, since nothing could say the person may see it; an entry's token it
	// mints as before, the operator's own setting having chosen its project.
	Projects projectaccess.Access
}

// validate refuses a configuration the exchange could not serve.
func (c ExchangeConfig) validate() error {
	if err := c.Kubernetes.validate("Kubernetes"); err != nil {
		return err
	}
	if err := c.GitHub.validate("GitHub"); err != nil {
		return err
	}
	if err := c.Google.validate("Google"); err != nil {
		return err
	}
	if c.Google.configured() && c.MachineCredentials == nil {
		return errors.New("oauth: a broker that accepts a machine's Google identity token needs MachineCredentials: the exchange mints the machine's launcher credential, and without it no machine it answers could enroll or read a secret")
	}
	if c.PersonAudience != "" && !c.Google.configured() {
		return errors.New("oauth: a broker that accepts a person's Google identity token needs the Google issuer too: the machine the person signs in on presents its own identity token as the actor, and the entries that list that machine are the Google ones")
	}
	if c.PersonAudience != "" && c.PersonSignIns == nil {
		return errors.New("oauth: a broker that accepts a person's Google identity token needs PersonSignIns: the first trade records the sign-in the box key belongs to, and every later one is held to it")
	}
	return nil
}

// MachineCredential is the launcher credential one machine's exchange asks for. The broker's
// wiring turns it into a row; nothing here is the provider's to decide.
type MachineCredential struct {
	// Subject keys the credential, so a machine's next exchange moves the one credential it
	// already holds rather than adding another (exchange.MachineSubject).
	Subject string
	// Service is the fleet the machine's account is listed for (exchange.Claims.Fleet). A
	// credential keyed by a subject carries one, which is what says the login is a service's
	// and no person approved it.
	Service string
	// Operator is the person a sign-in's credential acts for, which makes its enrollments that
	// person's sessions exactly as a dev box's machine login does. A machine's own credential
	// leaves it empty, whatever its fleet's entry names: secret authority follows the access
	// token, and a machine's token names a service and no person.
	Operator string
	// Host is the machine as an operator reads it on their machine-login page:
	// <project>/<zone>/<instance name> for a machine minted on a Compute Engine instance, and
	// the account for one minted anywhere else.
	Host string
	// Thumbprint and JWK are the key the request's DPoP proof presented, which is the key the
	// machine's later launcher proofs are signed with.
	Thumbprint string
	JWK        json.RawMessage
	// Expires is when the credential ends: the subject token's own expiry, so a machine that
	// stops being able to mint an identity token stops enrolling within the hour Google bounds
	// one at.
	Expires time.Time
}

// MachineCredentials creates or re-uses the credential cred describes, in the transaction ctx
// carries, and answers its id.
type MachineCredentials func(ctx context.Context, cred MachineCredential) (string, error)

// PersonSignIn is one person's sign-in on one machine as the exchange decided it. The broker's
// wiring turns it into a row; nothing here is the provider's to decide.
type PersonSignIn struct {
	// Person is the login the person's own Google identity token names, the token's person.
	Person string
	// Thumbprint is the RFC 7638 thumbprint of the box key the request's DPoP proof presented:
	// the key `agent-secrets login` made at the sign-in and keeps with Google's refresh token,
	// which is what tells this sign-in from the person's others.
	Thumbprint string
	// Machine is the Google account of the machine the person signed in on, as the actor token
	// named it and BROKER_VM_SERVICES lists it.
	Machine string
}

// PersonSignIns records the sign-in in, in the transaction ctx carries, and answers
// ErrSignInRevoked when that person has revoked that box key.
type PersonSignIns func(ctx context.Context, in PersonSignIn) error

// subjectTokenTypes is every type in types a caller may present as a subject token, sorted: all
// of them but the machine's actor token type, which the exchange registers beside the person's
// own so that a person's trade can carry it, and which is no subject of its own, and the airlock
// placeholder, which a result entry's run is answered and nobody presents. It is read off the
// registered types rather than built from the configuration a second time, so what the metadata
// document names is exactly what the library will accept.
func subjectTokenTypes(types map[string]oauth2.RFC8693TokenType) []string {
	names := make([]string, 0, len(types))
	for name := range types {
		if name != oauthwire.ActorTokenTypeMachine && name != oauthwire.TokenTypeAirlockPlaceholder {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// tokenTypes are the token types the library registers for the exchange, keyed by the exact
// string a request names them with.
//
// The access token is always registered: it is the kind this provider issues every caller but a
// result entry's run, so it is both the default requested type and the type a dev box session
// presents its machine access token under. So is the airlock placeholder, the one type a result
// entry's run may ask for (decide): the library's own NewAccessRequest validates such a request,
// spending the run token's jti, and exchangeGrant answers it with the grant it writes instead of
// any token the library would mint. Each JWT type is registered only where its issuer is
// configured, so a caller presenting an issuer the broker was never told about is refused by the
// library rather than by a half-built handler.
func (c ExchangeConfig) tokenTypes() map[string]oauth2.RFC8693TokenType {
	types := map[string]oauth2.RFC8693TokenType{
		tokenTypeAccessToken:                  &rfc8693.DefaultTokenType{Name: tokenTypeAccessToken},
		oauthwire.TokenTypeAirlockPlaceholder: &rfc8693.DefaultTokenType{Name: oauthwire.TokenTypeAirlockPlaceholder},
	}
	if c.Kubernetes.configured() {
		types[oauthwire.SubjectTokenTypeKubernetes] = &rfc8693.JWTType{
			Name:   oauthwire.SubjectTokenTypeKubernetes,
			Issuer: c.Kubernetes.Issuer,
			JWTValidationConfig: rfc8693.JWTValidationConfig{
				// A pod's projected token is a standing credential its kubelet refreshes, and a
				// long-running role exchanges many times with the one it holds, so single use
				// would refuse every renewal. What keeps a token from being spendable outside a
				// pod is the pod binding the decision requires.
				ValidateJTI:                false,
				JWTLifetimeToleranceWindow: kubernetesTolerance,
				ValidateFunc:               keyFunc(c.Kubernetes.Keys),
			},
		}
	}
	if c.GitHub.configured() {
		types[oauthwire.SubjectTokenTypeGitHub] = &rfc8693.JWTType{
			Name:   oauthwire.SubjectTokenTypeGitHub,
			Issuer: c.GitHub.Issuer,
			JWTValidationConfig: rfc8693.JWTValidationConfig{
				// A workflow run mints a fresh token per request, so one presentation is all a
				// token needs and a leaked one cannot be replayed.
				ValidateJTI:                true,
				JWTLifetimeToleranceWindow: githubTolerance,
				ValidateFunc:               keyFunc(c.GitHub.Keys),
			},
		}
	}
	if c.Google.configured() {
		types[oauthwire.SubjectTokenTypeGoogle] = &rfc8693.JWTType{
			Name:   oauthwire.SubjectTokenTypeGoogle,
			Issuer: c.Google.Issuer,
			JWTValidationConfig: rfc8693.JWTValidationConfig{
				// A machine reads one identity token from its metadata server and is handed the
				// same token until it is nearly spent, so a machine renewing its access token
				// presents the one it holds again and single use would refuse every renewal, as
				// it would a pod's. What keeps such a token from being spendable by anyone who
				// copied it is the DPoP key the minted token is bound to.
				ValidateJTI:                false,
				JWTLifetimeToleranceWindow: googleTolerance,
				ValidateFunc:               keyFunc(c.Google.Keys),
			},
		}
	}
	if c.PersonAudience != "" {
		// A person's own token and the machine's actor token come from the one issuer and are
		// checked against the one key set; what tells them apart is the audience each must
		// carry, which the broker checks itself (audience). Both are registered together: a
		// person's trade carries both, and neither is presentable alone — the person type as a
		// subject with no actor is refused by the decision, and the machine type is an actor
		// type the exchange client may present and no subject type at all.
		for _, name := range []string{oauthwire.SubjectTokenTypePerson, oauthwire.ActorTokenTypeMachine} {
			types[name] = &rfc8693.JWTType{
				Name:   name,
				Issuer: c.Google.Issuer,
				JWTValidationConfig: rfc8693.JWTValidationConfig{
					// A person's sign-in holds Google's refresh token and mints an identity
					// token from it whenever one is wanted, and the machine presents the same
					// metadata token its own trades do, so neither is single use, for the
					// reason a machine's subject token is not. What keeps a copied pair from
					// being spendable is the box key the sign-in is recorded against.
					ValidateJTI:                false,
					JWTLifetimeToleranceWindow: googleTolerance,
					ValidateFunc:               keyFunc(c.Google.Keys),
				},
			}
		}
	}
	return types
}

// issuerOf is the configured issuer a token type's tokens come from, and false for a type that
// is not one of the JWT kinds. The machine's actor token is Google's own, the same token its
// subject type names; a person's carries the device-flow client's audience over Google's issuer
// and keys, so it is answered as an issuer of its own.
func (c ExchangeConfig) issuerOf(subjectTokenType string) (IssuerConfig, bool) {
	switch subjectTokenType {
	case oauthwire.SubjectTokenTypeKubernetes:
		return c.Kubernetes, true
	case oauthwire.SubjectTokenTypeGitHub:
		return c.GitHub, true
	case oauthwire.SubjectTokenTypeGoogle, oauthwire.ActorTokenTypeMachine:
		return c.Google, true
	case oauthwire.SubjectTokenTypePerson:
		return IssuerConfig{Issuer: c.Google.Issuer, Audience: c.PersonAudience, Keys: c.Google.Keys}, true
	}
	return IssuerConfig{}, false
}

// keyFunc is a JWT subject token type's ValidateFunc: the library hands it the parsed but
// unverified token and checks the signature with whatever key it returns.
//
// The algorithm comes from a header nobody has verified, and the library's parse allows every
// algorithm including 'none', so the header is refused before any key is returned: an issuer
// publishes public keys, so a token signed with a shared secret or not signed at all is not one
// of its tokens whatever key id it names.
//
// The read runs on its own context rather than the request's, because the library's signature
// gives it none; the key source bounds its own fetch, and a set already cached costs no read.
func keyFunc(keys KeySource) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		if !slices.Contains(subjectTokenAlgorithms, token.SignatureAlgorithm) {
			return nil, fmt.Errorf("oauth: a subject token signed with %q is refused: its issuer signs with a published key", token.SignatureAlgorithm)
		}
		return keys.Key(context.Background(), token.KeyID, string(token.SignatureAlgorithm))
	}
}

// exchangeGrant is the token endpoint's RFC 8693 branch: a caller's platform identity for a
// Dispatch access token, bound to the key its DPoP proof names, or, for a read trade that sends a
// holder proof, to the key that proof names (holderKey); or, for a result entry's run, for an
// airlock placeholder and a result grant (grantPlaceholder) in place of any token.
//
// It runs in two halves. The first reads the request outside any transaction: the library
// validates it, which writes nothing the token depends on — the DPoP proof's and a GitHub token's
// single-use markers, each one statement that commits on its own, so a refused trade has spent
// its proof and its run token as every client expects, since each mints both afresh — and the
// decision is made (decide: the three JWT types through Store.ExchangeSubject while the library
// validates, the machine access-token type once it has). A read trade's holder proof is read and
// spent then too, its single-use marker committing on its own as the DPoP proof's does. Then the
// claims decided are held to what Dispatch says (projectAccess). That question is an HTTP call,
// and it is asked here, before the transaction begins, so a Dispatch that is slow to answer holds
// no row lock and no pooled connection of the broker's: the rule internal/cipher/projectaccess
// states for every caller.
//
// The second half runs in one transaction (oauthstore.TxFrom carries it), because the broker's
// own step writes: the session name it claims and the token it is claimed for must land together
// or not at all. The machine access-token subject type holds its launcher credential's row
// (liveCredential) until the token commits. The Google subject type mints or moves the machine's
// own launcher credential here (machineCredential), for the same reason and in the same
// transaction: the answer names it, and a machine whose token committed without it could enroll
// nothing. The person subject type records the sign-in its box key belongs to here
// (personSignIn), for the same reason again: a revoke that commits first refuses the trade, and
// one that commits after it ends the token the trade minted, and then takes the credential that
// sign-in holds (personCredential). The machine's credential is minted only for a trade that
// asked for one (launcherCredentialAsked): a caller that wants a Dispatch access token and
// nothing else leaves no launcher login behind for an operator to read through. A person's
// trade may not decline at all (personDeclinesNothing): a sign-in holds its credential. Beside
// those, and for every type, the registration a pod's session name needs is read before the
// library builds anything (registeredPodSession). The claim on the session name is made for every
// type once the library has built the answer, because that is when the issued token's expiry is
// set, and the claim must not end before the token it is made for. A request for the airlock
// placeholder, which decide grants a result entry's run alone, leaves the library after
// NewAccessRequest, which has spent the run token's jti: the library mints nothing, and
// grantPlaceholder writes the grant and claims the session name until the grant's end, in the
// same transaction.
//
// A read trade, one that asks for oauthwire.ScopeRead, makes, moves and extends no launcher
// credential: it asks for none (launcherCredentialAsked), and a person's read trade proves its
// sign-in as any trade does and then names the credential the sign-in already holds
// (signInCredential) rather than taking one (personCredential), so the sign-in's revoke ends the
// token. Its token is bound to the holder's key, set on the session once the sign-in, its
// credential and the pod's registration have been read with the DPoP proof's key and before the
// library stores the token, so the stored row, the introspection's jkt and the answer's token type
// all follow the holder's key. Its expiry is bounded by its subject's (subjectBound), and its
// answer names no credential.
//
// trade is TokenHooks.Trade: on the trade of one of the broker's own access tokens it runs once
// the library has validated the request, before the exchange decides anything, and a *Refusal it
// answers is written as that refusal. Nil runs nothing.
func (p *Provider) exchangeGrant(w http.ResponseWriter, r *http.Request, trade func(context.Context, map[string]any) error) {
	ctx := r.Context()
	if err := p.reserveSessions(ctx); err != nil {
		p.writeError(ctx, w, nil, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	request, err := p.oauth.NewAccessRequest(ctx, r, oauthstore.NewExchangeSession())
	noteClient(ctx, request)
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	session, ok := request.GetSession().(*oauthstore.ExchangeSession)
	if !ok {
		p.writeError(ctx, w, request, oauth2.ErrServerError.WithDebugf("the exchange ran on a %T session", request.GetSession()))
		return
	}
	subjectTokenType := request.GetRequestForm().Get(FormSubjectTokenType)
	if subjectTokenType == tokenTypeAccessToken && trade != nil {
		if err := trade(ctx, session.GetSubjectToken()); err != nil {
			var refused *Refusal
			if errors.As(err, &refused) {
				WriteRefusal(w, refused)
				return
			}
			p.writeError(ctx, w, request, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
			return
		}
	}
	read := oauthstore.NarrowedRequest(request)
	if read {
		// Before anything below can move the session's access-token expiry, which for a machine
		// login's token as the subject is that token's own, read back from its row.
		p.subjectBound(ctx, request, session, subjectTokenType)
	}
	if subjectTokenType == tokenTypeAccessToken {
		if _, err := p.decide(ctx, request, session.GetSubjectToken()); err != nil {
			p.writeError(ctx, w, request, err)
			return
		}
	}
	holder, err := p.holderKey(ctx, r, subjectTokenType, session, read)
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	if err := p.projectAccess(ctx, subjectTokenType, session); err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	txCtx, err := p.store.BeginTX(ctx)
	if err != nil {
		p.writeError(ctx, w, request, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = p.store.Rollback(txCtx)
		}
	}()
	if subjectTokenType == tokenTypeAccessToken {
		if err := p.liveCredential(txCtx, session.GetSubjectToken()); err != nil {
			p.writeError(ctx, w, request, err)
			return
		}
	}
	asked, err := launcherCredentialAsked(request.GetRequestForm(), read)
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	var credential MachineCredential
	var credentialID string
	if asked && subjectTokenType == oauthwire.SubjectTokenTypeGoogle {
		credentialID, credential, err = p.machineCredential(txCtx, r, session)
		if err != nil {
			p.writeError(ctx, w, request, err)
			return
		}
	}
	if subjectTokenType == oauthwire.SubjectTokenTypePerson {
		// A sign-in holds its credential: the person's trade is the one trade that may not
		// decline (personDeclinesNothing), so every token it mints names the credential its
		// revoke ends. A read trade asks for none and names the one the sign-in holds.
		if !asked && !read {
			p.writeError(ctx, w, request, personDeclinesNothing())
			return
		}
		// The sign-in first, so a box key the person has revoked is refused before anything is
		// minted for it, and then the credential that sign-in holds, in this same transaction.
		if err := p.personSignIn(txCtx, session); err != nil {
			p.writeError(ctx, w, request, err)
			return
		}
		if read {
			err = p.signInCredential(txCtx, session)
		} else {
			credentialID, credential, err = p.personCredential(txCtx, r, session)
		}
		if err != nil {
			p.writeError(ctx, w, request, err)
			return
		}
	}
	if err := p.registeredPodSession(txCtx, session); err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	if asksForPlaceholder(request.GetRequestForm()) {
		// A result entry's run, the one decision decide grants the placeholder: NewAccessRequest
		// has spent its run token's jti, and the library mints nothing for it. The grant is
		// written, and its session name claimed, in this transaction, and the answer is C8's.
		p.grantPlaceholder(ctx, txCtx, w, request, session, &committed)
		return
	}
	if holder != "" {
		// The token is the holder's: the library stores the session as it stands, answers its
		// token type from this binding, and an introspection answers it as the token's jkt.
		session.SetDPoPJWKThumbprint(holder)
	}
	resp, err := p.oauth.NewAccessResponse(txCtx, request)
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	taken, fixed, err := p.claimSession(txCtx, session)
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	if err := p.store.Commit(txCtx); err != nil {
		p.writeError(ctx, w, request, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	committed = true
	if credentialID != "" {
		// Named in the answer as the device login's own token answer names it, so one client
		// reads a credential the same way whichever grant gave it one.
		resp.SetExtra(oauthwire.ExtraCredentialID, credentialID)
		resp.SetExtra(oauthwire.ExtraCredentialExpiresAt, credential.Expires.UTC().Format(time.RFC3339))
	}
	switch {
	case taken != "" && fixed:
		// Logged once the claim has committed, because it is one of the two ways a principal
		// loses a name it used within the retention: the configuration now fixes it for an entry.
		slog.Warn("broker: the token exchange took a session name the configuration fixes from its holder",
			"session", session.Extra[ClaimSession], "from", taken, "to", session.Principal)
	case taken != "":
		// The other way: a claim the configuration does not fix takes a live name only from an
		// account paired with its own (oauthstore.Store.SetSessionPairs), as an operator moving a
		// deployment's pods to another account does. Logged once it has committed, as the other.
		slog.Warn("broker: the token exchange took a session name from the account paired with its own",
			"session", session.Extra[ClaimSession], "from", taken, "to", session.Principal)
	}
	p.oauth.WriteAccessResponse(ctx, w, request, resp)
}

// subjectBound writes onto a read trade's session what CreateAccessTokenSession bounds the
// narrowed token by, so it never outlives the token it was traded from: the subject's expiry, and
// for a machine login's access token as the subject, that token's signature, which the narrowed
// row names so the token ends when that row is revoked, retired or expired.
//
// A JWT subject's expiry is its `exp` claim (expiryOf). A machine login's is the expiry stored
// with its row, which the library read into this session when it validated the subject
// (rfc8693.AccessTokenTypeHandler reads the subject token's row into the request's own session)
// and which the library replaces with the new token's when it issues it, so it is read here,
// before anything is issued.
func (p *Provider) subjectBound(ctx context.Context, request oauth2.Requester, session *oauthstore.ExchangeSession, subjectTokenType string) {
	if subjectTokenType == tokenTypeAccessToken {
		session.SubjectExpiresAt = session.GetExpiresAt(oauth2.AccessToken)
		session.SubjectSignature = p.strategy.AccessTokenSignature(ctx, request.GetRequestForm().Get("subject_token"))
		return
	}
	if expires, ok := expiryOf(session.GetSubjectToken()); ok {
		session.SubjectExpiresAt = expires
	}
}

// holderProofRefused is the refusal of a DPoP-Holder header that holds more than one proof, or a
// proof that does not verify.
const holderProofRefused = "The DPoP-Holder proof was refused: a trade sends one, a DPoP proof of this request signed with the key it embeds, spent nowhere before."

// holderKey reads a trade's holder proof (oauthwire.HeaderDPoPHolder) and answers the thumbprint
// of the key it proves, or "" for a trade that sends none. The proof is verified with the
// library's own DPoP strategy (Provider.holderProofs) against the request's own method and URL,
// which the broker's route has already set to api.Deps.PublicURL and the path, as the DPoP proof's
// check reads them, with the same skew and replay table, so a holder proof is spent once.
// api.Deps.PublicURL is BROKER_PUBLIC_URL, or BROKER_EDGE_URL at the edge (api.RegisterEdge).
//
// Each refusal is invalid_request naming the header: a holder proof on a trade that does not ask
// for oauthwire.ScopeRead; a holder proof that does not verify; a holder key that is the DPoP
// proof's own key; and a person's read trade that sends no holder proof at all. A person's trade
// proves the sign-in with the box key, which signs no read token, so the token must be bound to a
// key of its own whatever the box key's state; every other read trade may send a holder proof,
// and one whose proof key holds a launcher credential is refused without one when the token is
// stored (oauthstore.Store.CreateAccessTokenSession).
func (p *Provider) holderKey(ctx context.Context, r *http.Request, subjectTokenType string, session *oauthstore.ExchangeSession, read bool) (string, error) {
	proofs := r.Header.Values(oauthwire.HeaderDPoPHolder)
	switch {
	case len(proofs) == 0 && read && subjectTokenType == oauthwire.SubjectTokenTypePerson:
		return "", oauth2.ErrInvalidRequest.WithHint("A person's read trade sends a DPoP-Holder proof: the box key proves the sign-in and is bound to no read token, so the token is bound to a key made for its holder.")
	case len(proofs) == 0:
		return "", nil
	case !read:
		return "", oauth2.ErrInvalidRequest.WithHint("The DPoP-Holder header binds a read token to its holder's key, and this trade does not ask for the scope read. Ask for it, or send no DPoP-Holder header.")
	case len(proofs) > 1:
		return "", oauth2.ErrInvalidRequest.WithHint(holderProofRefused)
	}
	proof, err := p.holderProofs.ValidateDPoPProof(ctx, r.Method, oauth2.RequestURL(r), proofs[0], p.holderNonce)
	if errors.Is(err, oauth2.ErrServerError) {
		return "", err
	}
	if err != nil {
		return "", oauth2.ErrInvalidRequest.WithHint(holderProofRefused).WithWrap(err).WithDebugError(err)
	}
	if proof.Thumbprint == session.GetDPoPJWKThumbprint() {
		return "", oauth2.ErrInvalidRequest.WithHint("The DPoP-Holder proof is signed with the key the DPoP proof is signed with, and the holder's key is a key of its own.")
	}
	return proof.Thumbprint, nil
}

// reserveSessions holds every session name an entry fixes for that entry's principal
// (oauthstore.Store.ReserveSessions), once for the life of the provider and before its first
// exchange, so the first caller of another principal to name one is refused by the same claim
// that refuses any held name. Each replica of the broker reserves for the configuration it was
// started with. A failure answers that exchange server_error, and the next one tries again.
//
// It takes no name another principal holds: replicas whose configurations disagree, as during a
// rollout that re-points a fixed name, would otherwise take it back and forth in the order they
// first exchanged. claimSession gives the name to its entry when that entry first asks for it,
// and a name left with its holder until then is logged, naming both.
func (p *Provider) reserveSessions(ctx context.Context) error {
	if p.reserved.Load() {
		return nil
	}
	p.reserving.Lock()
	defer p.reserving.Unlock()
	if p.reserved.Load() {
		return nil
	}
	left, err := p.store.ReserveSessions(ctx, p.reservations)
	if err != nil {
		return err
	}
	for session, holder := range left {
		slog.Warn("broker: the token exchange left a session name the configuration fixes with its holder until the entry claims it",
			"session", session, "holder", holder, "entry", p.reservations[session])
	}
	p.reserved.Store(true)
	return nil
}

// decide is the exchange's own decision, and the only one: what the minted token says, and who
// the session name it says it is writing under belongs to. Both paths into the exchange reach it
// — the four JWT subject types through Store.ExchangeSubject while the library validates the
// request, the machine access-token type from exchangeGrant once it has — so one function decides
// for all five. It asks nothing of anything outside the request: what Dispatch says about the
// projects it decided is projectAccess's question, asked after it.
//
// In order: the projects the request names, which must be a list (oauthwire.ParseProjects) before
// anything else is read, and the cut, which only a trade asking for oauthwire.ScopeRead may name
// (readCut); the subject token's audience, which the library never checks and which a token
// minted for another service would fail; the policy's decision, which names the session, the
// person or the service, the project scope, the projects and the principal; and then the claims
// themselves, onto the session, where the row the token is stored in carries them and an
// introspection answers from them, with the principal beside them for claimSession, and the OAuth
// scopes the decision carries granted on the request (writeProjectScope); and last, what the
// request asks for held to the decision (placeholderDecision): a result entry's run asks for the
// airlock placeholder, and nothing else does.
//
// A trade that asks for read is narrowed on the identity its subject would have had, whatever its
// kind: the same decision, the same session, person, service, machine and projects, under the
// narrowed word of its project scope, holding read and, for a token that acts for its person,
// person beside it. A trade that asks for no scope gets the token it always got. A result entry's
// run is the one decision no trade narrows, and its trade that asks for read is refused
// (writeProjectScope).
// person or the service, the project scope, the projects and the principal; the refusal of a job
// entry's trade that asks for read (exchange.Claims.Job); and then the claims themselves, onto
// the session, where the row the token is stored in carries them and an introspection answers
// from them, with the principal beside them for claimSession, and the OAuth scopes the decision
// carries granted on the request (writeProjectScope).
//
// A trade that asks for read is narrowed on the identity its subject would have had, whatever its
// kind but a job entry's, which is refused invalid_request since a job's token is minted whole,
// with no OAuth scope: the same decision, the same session, person, service, machine and
// projects, under the narrowed word of its project scope, holding read and, for a token that acts
// for its person, person beside it. A trade that asks for no scope gets the token it always got.
//
// The subject the library asks for is the principal, which every decision names: a token that
// names a service and no person has no person to be the subject of, and a subject that is
// sometimes empty would leave the library's own 'sub' saying nothing. The principal says which
// listed identity asked, which is what a reader of the stored row wants of a subject.
//
// Every refusal is an OAuth error the caller can act on and none repeats the subject token:
// RFC 8693 Section 2.2.2 makes a subject token that is unacceptable on policy invalid_request.
func (p *Provider) decide(_ context.Context, request oauth2.Requester, subjectToken map[string]any) (string, error) {
	session, ok := request.GetSession().(*oauthstore.ExchangeSession)
	if !ok {
		return "", oauth2.ErrServerError.WithDebugf("the exchange ran on a %T session", request.GetSession())
	}
	form := request.GetRequestForm()
	projects, err := oauthwire.ParseProjects(form.Get(oauthwire.FormProjects))
	if err != nil {
		return "", oauth2.ErrInvalidRequest.WithHintf("The '%s' parameter was refused: %s.", oauthwire.FormProjects, err).WithWrap(err).WithDebugError(err)
	}
	read := oauthstore.NarrowedRequest(request)
	cut, err := readCut(form, read, request.GetRequestedAt())
	if err != nil {
		return "", err
	}
	subjectTokenType := form.Get(FormSubjectTokenType)
	name := form.Get(oauthwire.FormSession)
	var claims exchange.Claims
	switch subjectTokenType {
	case oauthwire.SubjectTokenTypeKubernetes:
		if err = p.audience(subjectTokenType, subjectToken); err == nil {
			claims, err = p.exchange.Policy.Pod(subjectToken, name, projects)
		}
	case oauthwire.SubjectTokenTypeGitHub:
		if err = p.audience(subjectTokenType, subjectToken); err == nil {
			claims, err = p.exchange.Policy.Workflow(subjectToken, name, projects)
		}
	case oauthwire.SubjectTokenTypeGoogle:
		if err = p.audience(subjectTokenType, subjectToken); err == nil {
			claims, err = p.exchange.Policy.Google(subjectToken, name, projects)
		}
	case oauthwire.SubjectTokenTypePerson:
		// Two tokens, each held to the audience its own type is configured with: the person's
		// to the device-flow client's, the machine's to the one a machine trades for itself
		// with, so neither is spendable in the other's place. The actor token's claims are on
		// the session because the library validated it before it asked for the subject
		// (rfc8693.CustomJWTTypeHandler), and a request that carried none leaves it nil, which
		// the policy refuses by name.
		actor := session.GetActorToken()
		if err = p.audience(subjectTokenType, subjectToken); err == nil && actor != nil {
			err = p.audience(oauthwire.ActorTokenTypeMachine, actor)
		}
		if err == nil {
			claims, err = p.exchange.Policy.Person(subjectToken, actor, name, projects)
		}
	case tokenTypeAccessToken:
		// A machine access token is the broker's own, so there is no issuer to check it against:
		// the login that was issued it recorded the approver it acts for and the launcher
		// credential it holds, and those are the identity.
		credential, _ := subjectToken[oauthwire.ExtraCredentialID].(string)
		approver, _ := subjectToken["sub"].(string)
		claims, err = p.exchange.Policy.Machine(credential, approver, name, projects)
	default:
		return "", oauth2.ErrInvalidRequest.WithHintf("The '%s' token type is not one this broker exchanges.", subjectTokenType)
	}
	if err != nil {
		// The refusal's own text is the hint: every sentence the exchange policy refuses with
		// names what to fix, and repeats nothing of the token but the public identity it claimed.
		return "", oauth2.ErrInvalidRequest.WithHintf("The subject token was refused: %s.", err).WithWrap(err).WithDebugError(err)
	}
	if read && claims.Job {
		// A job's token holds no OAuth scope and the broker has no narrowed word for it, so a
		// trade that asks for read would mint a token no verifier takes.
		return "", oauth2.ErrInvalidRequest.WithHintf("The scope '%s' narrows a token to reading, and a job entry's token is minted whole, with no scope: ask for it without that scope.", oauthwire.ScopeRead)
	}
	session.Subject, session.Username = claims.Principal, claims.Principal
	session.Principal, session.Instance, session.Fleet = claims.Principal, claims.Instance, claims.Fleet
	if session.Extra == nil {
		// A session read back out of a row that carried no extra claims has none to write on.
		session.Extra = map[string]any{}
	}
	session.Extra[ClaimSession] = claims.Session
	session.Extra[ClaimPerson] = claims.Person
	session.Extra[ClaimService] = claims.Service
	if claims.Actor == "" {
		// Absent rather than empty, unlike the service: every token carries a person or a
		// service, so an absent one of those would be ambiguous, where a machine is the mark of
		// the one kind of trade that has an actor at all.
		delete(session.Extra, ClaimMachine)
	} else {
		session.Extra[ClaimMachine] = claims.Actor
	}
	if err := writeProjectScope(request, session, claims, read); err != nil {
		return "", err
	}
	if cut.IsZero() {
		delete(session.Extra, ClaimAsOf)
	} else {
		session.Extra[ClaimAsOf] = cut.UTC().Format(cutLayout)
	}
	if err := placeholderDecision(request.GetRequestForm(), claims); err != nil {
		return "", err
	}
	return claims.Principal, nil
}

// cutLayout is how a cut is stored and answered: in UTC, to the microsecond, every digit written
// (oauthwire.Introspection.AsOf).
const cutLayout = "2006-01-02T15:04:05.000000Z"

// cutForm is the one form a cut takes on the wire: an RFC 3339 time, a T between its date and its
// time, at most six fractional digits, and Z or a numeric offset. time.Parse takes more than
// this, nine fractional digits among it, and a cut the broker could not store whole is refused
// rather than cut short.
var cutForm = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?(Z|[+-][0-9]{2}:[0-9]{2})$`)

// cutFormRefused is the refusal of a cut in any form but cutForm, or one naming no real time; its
// verb is the parameter's name.
const cutFormRefused = "The '%s' parameter is not an RFC 3339 time with at most six fractional digits."

// readCut reads the trade's cut (oauthwire.FormAsOf): the zero time when the form names none.
// Each refusal is invalid_request naming the parameter: a cut on a trade that does not ask for
// oauthwire.ScopeRead, a cut in any form but cutForm or naming no real time, the zero time, which
// decide and every reader of the claim take for no cut (ciphertoken's cutOf refuses it too), and
// a cut later than the trade's own time, now (the library's time for the request, or the clock's
// when it set none). A narrowed token reads as of a time that has passed.
func readCut(form url.Values, read bool, now time.Time) (time.Time, error) {
	raw, named := form[oauthwire.FormAsOf]
	if !named {
		return time.Time{}, nil
	}
	if !read {
		return time.Time{}, oauth2.ErrInvalidRequest.WithHintf("The '%s' parameter names a cut, and only a trade that asks for the scope %s may name one.", oauthwire.FormAsOf, oauthwire.ScopeRead)
	}
	if len(raw) != 1 || !cutForm.MatchString(raw[0]) {
		return time.Time{}, oauth2.ErrInvalidRequest.WithHintf(cutFormRefused, oauthwire.FormAsOf)
	}
	cut, err := time.Parse(time.RFC3339Nano, raw[0])
	if err != nil {
		return time.Time{}, oauth2.ErrInvalidRequest.WithHintf(cutFormRefused, oauthwire.FormAsOf).WithWrap(err).WithDebugError(err)
	}
	if cut.IsZero() {
		return time.Time{}, oauth2.ErrInvalidRequest.WithHintf("The '%s' parameter names the zero time, which is no cut: a trade that reads the present names no cut.", oauthwire.FormAsOf)
	}
	if now.IsZero() {
		now = time.Now()
	}
	if cut.After(now) {
		return time.Time{}, oauth2.ErrInvalidRequest.WithHintf("The '%s' parameter names a time later than this trade, and a cut is a time that has passed.", oauthwire.FormAsOf)
	}
	return cut, nil
}

// writeProjectScope writes what claims.Scope decides onto the session the token is stored with,
// and grants the OAuth scopes that decision carries: the project scope's wire word under
// ClaimProjectScope, its narrowed word for a trade that asked for oauthwire.ScopeRead (read);
// the list of projects under ClaimProjects, which is empty for every token of the none scope and
// the one project whose results a results token writes; a named token's one project under
// claimLegacyProject too, unless it is narrowed, and none for any other; the run under
// oauthwire.ClaimCI for a results token, and no run for any other; oauthwire.ScopeRead for a
// narrowed trade and for exchange.ProjectScopeNone, the read entry's decision;
// oauthwire.ScopePerson beside read for a narrowed person-scoped token that names its person, the
// one kind of token that acts for that person; and oauthwire.ScopeResults for
// exchange.ProjectScopeResults, the result entry's decision. Every other token is granted no
// scope.
//
// The request's own scope parameter decides only whether the trade is narrowed: the exchange
// client is registered for oauthwire.Scope and oauthwire.ScopeRead (oauthstore.exchangeClient),
// the library refuses any other scope invalid_scope before decide runs, oauthwire.ScopeResults
// among them, and it validates a requested scope without granting it. So a request naming
// oauthwire.Scope is granted nothing for it: under a read entry it is granted read and nothing
// else, under a result entry results and nothing else, and under any other entry nothing at all.
// The result entry decides its scope, and no results token is narrowed, so a trade under a result
// entry that asks for read is refused invalid_scope.
// the list of projects under ClaimProjects, which is empty for every token of the none scope; a
// named token's one project under claimLegacyProject too, unless it is narrowed, and none for any
// other; oauthwire.ScopeRead for a narrowed trade and for exchange.ProjectScopeNone, the read
// entry's decision; oauthwire.ScopePerson beside read for a narrowed person-scoped token that
// names its person, the one kind of token that acts for that person; and
// oauthwire.ScopeLegionStatus for a named token that is not narrowed and whose
// claims.LegionStatus is set, the decision of a Legion daemon's own entry (exchange.Set.Pod).
// Every other token is granted no scope. exchange.ProjectScopePublic, a job entry's decision, is
// written as oauthwire.ProjectScopePublic with the entry's whole list, no legacy claim, which
// holds a named token's one project alone, and no scope, since oauthwire.ScopeRead is a read
// entry's and a narrowed trade's, and a job's token writes. It has no narrowed word, so a
// narrowed trade for it is the broker's own failure here: decide refuses that trade before it
// comes this far.
//
// The request's own scope parameter decides only whether the trade is narrowed: the exchange
// client is registered for oauthwire.Scope and oauthwire.ScopeRead (oauthstore.exchangeClient),
// the library refuses any other scope, oauthwire.ScopeLegionStatus included, invalid_scope before
// decide runs, and it validates a requested scope without granting it. So a request naming
// oauthwire.Scope is granted nothing for it: under a read entry it is granted read and nothing
// else, under a Legion daemon's own entry legion-status and nothing else, and under any other
// entry nothing at all. A Legion daemon's trade that asks for read is narrowed and is granted no
// legion-status: a token narrowed to reading writes nothing, and a verifier takes a narrowed
// token that holds any word beside read and person for no session's (ciphertoken's shapeOf).
//
// claims.LegionStatus beside any project scope but exchange.ProjectScopeNamed is a server error
// that names it, and nothing is written or granted. Set.Pod sets it for an account entry that names
// a project (isLegionDaemon), whose decision is always named, so it beside another scope is a
// decision this broker never makes, and the refusal keeps such a Claims from minting a token that
// holds the scope.
//
// Any other value, exchange.ProjectScopeUnset included, is a server error that names it, and
// nothing is written or granted: every exchange constructor sets the scope, so an unset one is a
// decision nobody made and the broker's own failure rather than a token whose empty list reads as
// any scope. Read as the person scope, as a build before exchange.ProjectScopeNone read every
// value that was not named, a Claims nobody filled in would mint a token that acts as its person.
// So is a run beside any scope but results, and a results decision with no run, either of which
// would mint a token whose run says something other than its scope. No path through decide hands
// it one today, since ExchangeConfig.Policy is a concrete *exchange.Set and each of its decisions
// sets a known scope and a run exactly beside results, so the refusal keeps a later new value from
// being minted before this switch learns its word.
func writeProjectScope(request oauth2.Requester, session *oauthstore.ExchangeSession, claims exchange.Claims, read bool) error {
	if (claims.CI != nil) != (claims.Scope == exchange.ProjectScopeResults) {
		return oauth2.ErrServerError.WithDebugf("the exchange decided project scope %d (%s) with a run (%t), and a run goes beside the results scope alone", int(claims.Scope), claims.Scope, claims.CI != nil)
	}
	delete(session.Extra, oauthwire.ClaimCI)
	if claims.LegionStatus && claims.Scope != exchange.ProjectScopeNamed {
		return oauth2.ErrServerError.WithDebugf("the exchange decided %s beside project scope %d (%s) for %s, and this broker grants %s to a named token alone",
			oauthwire.ScopeLegionStatus, int(claims.Scope), claims.Scope, claims.Principal, oauthwire.ScopeLegionStatus)
	}
	var word, narrowed string
	switch claims.Scope {
	case exchange.ProjectScopeNamed:
		word, narrowed = oauthwire.ProjectScopeNamed, oauthwire.ProjectScopeReadNamed
	case exchange.ProjectScopePerson:
		word, narrowed = oauthwire.ProjectScopePerson, oauthwire.ProjectScopeReadPerson
	case exchange.ProjectScopeNone:
		word, narrowed = oauthwire.ProjectScopeNone, oauthwire.ProjectScopeReadNone
	case exchange.ProjectScopeResults:
		if read {
			return oauth2.ErrInvalidScope.WithHintf("A result entry's run is granted the scope %s alone, and its trade may not ask for the scope %s.", oauthwire.ScopeResults, oauthwire.ScopeRead)
		}
		word = oauthwire.ProjectScopeResults
		// The run itself, which the store keeps as the JSON object its tags spell and an
		// introspection reads back through the same declaration (Introspect).
		session.Extra[oauthwire.ClaimCI] = *claims.CI
		request.GrantScope(oauthwire.ScopeResults)
	case exchange.ProjectScopePublic:
		word = oauthwire.ProjectScopePublic
	default:
		return oauth2.ErrServerError.WithDebugf("the exchange decided project scope %d (%s) for %s, which this broker writes no word for", int(claims.Scope), claims.Scope, claims.Principal)
	}
	if read {
		if narrowed == "" {
			return oauth2.ErrServerError.WithDebugf("the exchange narrowed a token of project scope %s for %s, which has no narrowed word", claims.Scope, claims.Principal)
		}
		word = narrowed
	}
	session.Extra[ClaimProjectScope] = word
	if read || claims.Scope == exchange.ProjectScopeNone {
		request.GrantScope(oauthwire.ScopeRead)
	}
	if read && claims.Scope == exchange.ProjectScopePerson && claims.Person != "" {
		request.GrantScope(oauthwire.ScopePerson)
	}
	if claims.LegionStatus && !read {
		request.GrantScope(oauthwire.ScopeLegionStatus)
	}
	session.Extra[ClaimProjects] = oauthwire.JoinProjects(claims.Projects)
	// A named token naming one project also stores it under the claim a broker before the list
	// read it from. While such a broker still answers introspection beside this one (a rolling
	// update, a canary, a rollback), it then answers `"project":"<key>"`, which this release's
	// readers refuse, rather than a named token naming none, which they would read as the
	// public-session shape and hold to no project. No reader of this release reads the claim. A
	// narrowed token stores none: it is kept where no broker before the narrowed words looks.
	if !read && claims.Scope == exchange.ProjectScopeNamed && len(claims.Projects) == 1 {
		session.Extra[claimLegacyProject] = claims.Projects[0]
	} else {
		delete(session.Extra, claimLegacyProject)
	}
	return nil
}

// claimLegacyProject is the claim a broker before the list stored a named token's one project
// under, and read it from to answer introspection's `project`.
const claimLegacyProject = "project"

// refusedByDispatch is the trade's refusal when the broker's own account token does not work
// (dispatchread.ErrBrokerRefused): Dispatch answered 401 or 403 to it, because Dispatch does not
// list the broker's account or does not accept its token (one minted for another audience, say),
// or the broker could not read it from its file at all. That is a misconfiguration and not an
// outage, so the trade is refused naming it rather than minted.
func refusedByDispatch(err error) error {
	return oauth2.ErrServerError.WithHint("Dispatch refused the broker's own account token (BROKER_PROJECT_ACCESS_TOKEN_FILE), or the broker could not read that file, so this broker cannot check the token's projects: its operator mounts the file and lists the broker's account as one Dispatch answers.").WithWrap(err).WithDebugError(err)
}

// unknownMemberKind is the trade's refusal when Dispatch answers a member kind the broker does not
// know (projectaccess.ErrUnknownMemberKind): the broker cannot tell whether the person is held to
// the projects that name them, so it mints nothing on a guess.
func unknownMemberKind(err error) error {
	return oauth2.ErrServerError.WithHint("Dispatch answered a member kind this broker does not know for the token's person, so this broker cannot tell whether the token must name its projects: its operator runs a broker that knows that kind.").WithWrap(err).WithDebugError(err)
}

// dispatchUnreachable is the trade's refusal when Dispatch could not be asked about a token that
// names a project, which a client asks again on.
func dispatchUnreachable(err error) error {
	return oauth2.ErrTemporarilyUnavailable.WithHint("Dispatch could not be asked whether the token's person sees the projects it names; try again.").WithWrap(err).WithDebugError(err)
}

// projectAccess holds the claims decide wrote onto session to what Dispatch says, before the
// exchange opens its transaction (exchangeGrant). It reads the claims back off the session, so
// what it checks is exactly what the token will carry.
//
// A token acting as its person: every project it names must be one Dispatch says that person
// sees (projectaccess.Visible), and a restricted member's must name at least one
// (projectaccess.ListAllowed). A person's sign-in is taken as a full member's and asks no member
// kind at all, since the broker verified its account against BROKER_SIGNIN_DOMAIN itself; every
// other trade acting as its person asks.
//
// A token an operator's entry maps, a named token or a result entry's results token: its one
// project must be one Dispatch says the entry's person sees, and a pairing Dispatch refuses is
// refused invalid_request naming both. An account entry that names no project (the shared
// public-session account's) is refused when Dispatch calls its person a restricted member, who
// sees only the projects that name them (projectaccess.ListAllowed).
// With no check configured an entry's token mints as it always has, since the operator's own
// setting is what chose the project, or chose none.
// A token an operator's entry maps to its person: its one project must be one Dispatch says the
// entry's person sees, and a pairing Dispatch refuses is refused invalid_request naming both. An
// account entry that names no project (the shared public-session account's) is refused when
// Dispatch calls its person a restricted member, who sees only the projects that name them
// (projectaccess.ListAllowed). With no check configured an entry's token mints as it always has,
// since the operator's own setting is what chose the project, or chose none.
//
// Either way, a list that names no project mints as it always has when Dispatch answers anything
// but restricted, and when the broker has no check configured; while Dispatch cannot be asked
// (projectaccess.ErrUnchecked) it mints too, and logs one line naming the principal, the session
// and the person. A list that names a project is never minted unchecked: with no check configured
// a person's is refused naming the setting, and while Dispatch cannot be asked either kind is
// refused temporarily_unavailable, which a client asks again on.
//
// The broker's own account token not working (refusedByDispatch) and a member kind the broker does
// not know (unknownMemberKind) refuse every trade that meets them server_error: each is a
// misconfiguration, never an outage to mint through.
//
// A token that names no person asks Dispatch nothing. A machine's whose entry names no person and
// a read entry's name no project, and a trade naming one is refused
// (exchange.ErrProjectsNoPerson). An account entry's naming no person names no project either,
// since its entry names none, so a trade naming one is refused as under any entry naming none
// (exchange.ErrProjectsFixed). A job entry's names its entry's whole list, under the named scope
// or the public one, and that is not asked either: the operator's entry chose the list, and there
// is no person whose sight of a project to check, so the token mints the same whether Dispatch is
// configured, reachable or neither. With no person there is nobody whose member kind Dispatch
// could be asked about, so the restricted-member check does not run for any of them.
//
// A narrowed token is checked as its base word (oauthwire.BaseProjectScope): a trade that asked
// for read names what the same trade without it would, so it is held to exactly the same rules
// and answered exactly the same way. A job entry's token is the exception that decide refuses
// before it comes to any of this: the broker narrows no job's token.
func (p *Provider) projectAccess(ctx context.Context, subjectTokenType string, session *oauthstore.ExchangeSession) error {
	person, _ := session.Extra[ClaimPerson].(string)
	word, _ := session.Extra[ClaimProjectScope].(string)
	scope := oauthwire.BaseProjectScope(word)
	name, _ := session.Extra[ClaimSession].(string)
	if person == "" {
		return nil
	}
	stored, ok := session.Extra[ClaimProjects].(string)
	if !ok {
		return oauth2.ErrServerError.WithDebugf("the decided projects are a %T, not their string", session.Extra[ClaimProjects])
	}
	projects, err := oauthwire.ParseProjects(stored)
	if err != nil {
		return oauth2.ErrServerError.WithWrap(err).WithDebugf("the decided projects %q do not parse: %v", stored, err)
	}
	access := p.exchange.Projects
	if scope == oauthwire.ProjectScopeNamed || scope == oauthwire.ProjectScopeResults {
		if access == nil {
			return nil
		}
		err := projectaccess.ListAllowed(ctx, access, person, projects)
		var unseen *projectaccess.NotVisibleError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &unseen):
			return oauth2.ErrInvalidRequest.WithHintf("The subject token was refused: its entry names the project %s, which its person %s cannot see.", unseen.Project, person).WithWrap(err).WithDebugError(err)
		case errors.Is(err, projectaccess.ErrListRequired):
			return oauth2.ErrInvalidRequest.WithHintf("The subject token was refused: its entry names no project, and its person %s is a restricted member, who sees only the projects that name them.", person).WithWrap(err).WithDebugError(err)
		case errors.Is(err, dispatchread.ErrBrokerRefused):
			return refusedByDispatch(err)
		case errors.Is(err, projectaccess.ErrUnknownMemberKind):
			return unknownMemberKind(err)
		case errors.Is(err, projectaccess.ErrUnchecked):
			slog.WarnContext(ctx, "broker: the token exchange goes on to mint an entry's token naming no project unchecked, since Dispatch could not be asked what kind of member its person is",
				"principal", session.Principal, "session", name, "person", person, "error", err)
			return nil
		default:
			return dispatchUnreachable(err)
		}
	}
	if access == nil {
		if len(projects) == 0 {
			return nil
		}
		return oauth2.ErrInvalidRequest.WithHintf("The token names projects, and this broker checks no project against Dispatch: its operator sets BROKER_PROJECT_ACCESS_URL and BROKER_PROJECT_ACCESS_TOKEN_FILE to take a list of projects.")
	}
	if subjectTokenType == oauthwire.SubjectTokenTypePerson {
		err = projectaccess.Visible(ctx, access, person, projects)
	} else {
		err = projectaccess.ListAllowed(ctx, access, person, projects)
	}
	var unseen *projectaccess.NotVisibleError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &unseen):
		return oauth2.ErrInvalidRequest.WithHintf("The token names a project its person cannot see: %s.", unseen.Project).WithWrap(err).WithDebugError(err)
	case errors.Is(err, projectaccess.ErrListRequired):
		return oauth2.ErrInvalidRequest.WithHintf("%s is a restricted member, whose token names its projects: set AGENT_SECRETS_PROJECTS to the projects this session works on.", person).WithWrap(err).WithDebugError(err)
	case errors.Is(err, dispatchread.ErrBrokerRefused):
		return refusedByDispatch(err)
	case errors.Is(err, projectaccess.ErrUnknownMemberKind):
		return unknownMemberKind(err)
	case errors.Is(err, projectaccess.ErrUnchecked):
		slog.WarnContext(ctx, "broker: the token exchange goes on to mint a person's token naming no project unchecked, since Dispatch could not be asked what kind of member the person is",
			"principal", session.Principal, "session", name, "person", person, "error", err)
		return nil
	default:
		return dispatchUnreachable(err)
	}
}

// claimSession claims the session name decide wrote onto session for the principal it decided,
// until oauthstore.SessionRetention past the expiry the library has just set on the issued token,
// in the transaction that stores that token: the claim can never end before the token it was made
// for, and a token whose claim is refused is never stored. It answers the principal it took the
// name from ("" for none) and whether the configuration fixed the claim, which is what tells
// exchangeGrant's log a fixed claim's take from a pair's.
//
// It is where the configuration decides who a fixed name belongs to: when this provider's
// configuration fixes the name for the principal asking (the entry's own caller), the claim holds
// it for that principal with no expiry and takes it from any other holder, answering whom it took
// it from, so an entry is never refused its own name, whatever a replica started with another
// configuration reserved. Every other claim is the ordinary one, which a name another principal
// holds refuses unless that principal is an account paired with the claimant's
// (oauthstore.Store.SetSessionPairs): such a claim takes the name, answering whom it took it
// from, though never a name the configuration fixes, and a pair does not chain.
// oauthstore.Store.BindSession makes both claims, and its refusal is the only refusal of a claim.
//
// A name another principal holds and the claimant may not take is invalid_grant, because the
// subject token is sound and it is the broker's state that refuses.
func (p *Provider) claimSession(ctx context.Context, session *oauthstore.ExchangeSession) (taken string, fixed bool, err error) {
	name, _ := session.Extra[ClaimSession].(string)
	expires := session.GetExpiresAt(oauth2.AccessToken)
	if name == "" || session.Principal == "" || expires.IsZero() {
		return "", false, oauth2.ErrServerError.WithDebugf("the exchange issued a token without deciding its session name (%t), principal (%t) and expiry (%t)",
			name != "", session.Principal != "", !expires.IsZero())
	}
	fixed = p.reservations[name] == session.Principal
	taken, err = p.store.BindSession(ctx, name, session.Principal, fixed, expires)
	if err != nil {
		if errors.Is(err, oauthstore.ErrSessionHeld) {
			return "", false, oauth2.ErrInvalidGrant.WithHintf("The session name '%s' belongs to another identity: one the broker's configuration fixes it for, or one that has written under it within the last %d days. Ask under a name of your own.",
				name, int(oauthstore.SessionRetention/(24*time.Hour))).WithWrap(err).WithDebugError(err)
		}
		return "", false, oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	return taken, fixed, nil
}

// liveCredential holds the launcher credential a machine access token names
// (oauthstore.LockCredential) in the exchange's transaction, until the exchanged token commits,
// and refuses an exchange whose credential is revoked or past its expiry invalid_grant, as the
// device login's own grants refuse a login that has ended.
//
// The library validated the subject token with a plain read of its row, which a revoke can
// deactivate a moment later; the exchanged token names the same credential (its session is read
// out of the subject token's row), so without the lock the token could commit after the revoke
// had ended the credential's tokens and stay live for its whole lifetime. With it, the revoke
// either commits first and the exchange is refused, or waits and then ends the exchanged token
// with the rest.
func (p *Provider) liveCredential(ctx context.Context, subjectToken map[string]any) error {
	credential, _ := subjectToken[oauthwire.ExtraCredentialID].(string)
	err := oauthstore.LockCredential(ctx, credential)
	if errors.Is(err, oauthstore.ErrCredentialEnded) {
		return oauth2.ErrInvalidGrant.WithHint("The machine login the subject token belongs to has ended.").WithWrap(err).WithDebugError(err)
	}
	if err != nil {
		return oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	return nil
}

// launcherCredentialAsked reports whether this trade wants the launcher credential its identity
// holds minted or moved onto the key it proved (oauthwire.FormLauncherCredential).
//
// An absent parameter is yes, because that is what every client written before the parameter
// sends and those clients — `agent-secrets`, the helper, a person's sign-in — exist to hold that
// credential: a trade of theirs answered without one could enroll nothing and read no secret.
// Only the one spelling oauthwire.FormLauncherCredentialDeclined declines; every other value is
// invalid_request rather than a guess, since a caller that misspelled "false" and was read as
// asking would go on filling an operator's machine-logins list, and one misspelled the other way
// would lose its secrets with nothing naming the cause.
//
// A read trade (read) makes, moves and extends no launcher credential, so for it an absent
// parameter declines, and "true" is invalid_request: a token that only reads must not leave a
// credential that reads secrets on the key it proved.
func launcherCredentialAsked(form url.Values, read bool) (bool, error) {
	switch asked := form.Get(oauthwire.FormLauncherCredential); asked {
	case "":
		return !read, nil
	case "true":
		if read {
			return false, oauth2.ErrInvalidRequest.WithHintf("A read trade makes or moves no launcher credential, so it cannot send %s=true. Trade for read without the parameter.",
				oauthwire.FormLauncherCredential)
		}
		return true, nil
	case oauthwire.FormLauncherCredentialDeclined:
		return false, nil
	default:
		return false, oauth2.ErrInvalidRequest.WithHintf("The %s parameter takes 'true' or 'false', and this request sent '%s'. A request that sends none is asking for the launcher credential its identity holds.",
			oauthwire.FormLauncherCredential, asked)
	}
}

// personDeclinesNothing refuses a person's trade that sent launcher_credential=false.
//
// A person's sign-in exists to hold a launcher credential: `agent-secrets login` reads the
// credential id out of the answer and refuses one without it, and the sessions that sign-in
// enrols read the person's own secrets under it. A trade that declined would leave a sign-in
// holding none, which is a sign-in that does nothing and an answer `agent-secrets login` refuses,
// so the parameter is refused here rather than ignored: a caller that sends it has misread what a
// person's trade is for, and reading it as "true" instead would be the kind of guess
// launcherCredentialAsked refuses to make for a misspelling.
//
// It is invalid_request and not invalid_grant because the subject token is sound and it is the
// request's own parameter that cannot be honoured. A machine's trade declines freely: that one
// wants a Dispatch access token and nothing else, and leaves no launcher login behind.
func personDeclinesNothing() error {
	return oauth2.ErrInvalidRequest.WithHintf("A sign-in holds its credential, so a person's trade cannot send %s=%s. Trade without the parameter, as `agent-secrets login` does.",
		oauthwire.FormLauncherCredential, oauthwire.FormLauncherCredentialDeclined)
}

// machineCredential creates or re-uses the launcher credential the machine presenting a Google
// identity token holds (ExchangeConfig.MachineCredentials), in the exchange's own transaction,
// and writes its id onto the session so the stored token names it. It answers the id and what it
// asked for, which is what the token answer's extras repeat.
//
// It is the bridge between the two halves of what a machine does: the exchange answers a
// Dispatch access token, which writes Dispatch and reads no secret, and every secrets route is
// an enrollment's, made under a launcher credential and a proof signed by its key. One trade
// gives the machine both, and both end together, because the credential is written in the
// transaction the token is written in and expires when the subject token does.
//
// The credential is keyed by the machine (exchange.MachineSubject) and bound to the key the
// request's DPoP proof presented, so a machine's next exchange moves the one credential it holds
// and no machine can move another's. It names its fleet's service and no person, whatever the
// fleet's entry says: secret authority follows the access token, so a machine's sessions reach
// its service's secrets and the shared agent-tier ones and no person's. The person a fleet entry
// names reaches the Dispatch token's claims and no secret (the request machine also lists that
// fleet's shared secrets' requests in their Inbox); a person reaches their own secrets on that
// machine only through their own sign-in.
//
// A key another live credential holds is invalid_grant rather than a server error: the broker's
// state refuses it and the machine fixes it by making a key of its own, which is what every
// machine does. A key a live read token is bound to is invalid_grant too, with a hint of its own
// (keyBindsReadTokenHint).
func (p *Provider) machineCredential(ctx context.Context, r *http.Request, session *oauthstore.ExchangeSession) (string, MachineCredential, error) {
	subjectToken := session.GetSubjectToken()
	identity := exchange.ReadGoogleIdentity(subjectToken)
	thumbprint := session.GetDPoPJWKThumbprint()
	key, err := proofKey(r, thumbprint)
	if err != nil {
		return "", MachineCredential{}, oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	expires, ok := expiryOf(subjectToken)
	if !ok {
		return "", MachineCredential{}, oauth2.ErrInvalidRequest.WithHint("The subject token names no expiry, so there is nothing to expire the machine's credential with.")
	}
	cred := MachineCredential{
		Subject:    exchange.MachineSubject(identity, thumbprint),
		Service:    session.Fleet,
		Host:       machineHost(identity),
		Thumbprint: thumbprint,
		JWK:        key,
		Expires:    expires,
	}
	id, err := p.exchange.MachineCredentials(ctx, cred)
	if err != nil {
		if errors.Is(err, ErrMachineKeyTaken) {
			return "", MachineCredential{}, oauth2.ErrInvalidGrant.WithHint("Another live machine login holds the key this proof was signed with. A machine makes a key of its own.").WithWrap(err).WithDebugError(err)
		}
		if errors.Is(err, ErrMachineKeyBindsReadToken) {
			return "", MachineCredential{}, oauth2.ErrInvalidGrant.WithHint(keyBindsReadTokenHint).WithWrap(err).WithDebugError(err)
		}
		return "", MachineCredential{}, oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	if session.Extra == nil {
		session.Extra = map[string]any{}
	}
	// On the session, not only in the answer, because the row the token is stored in is what a
	// second exchange of that token reads its credential back out of (decide, the access-token
	// subject type).
	session.Extra[oauthwire.ExtraCredentialID] = id
	return id, cred, nil
}

// keyBindsReadTokenHint is the hint of a machine's trade and a person's refused because a live
// read token is bound to the key the trade's proof was signed with (ErrMachineKeyBindsReadToken):
// a read token's key signs no launcher proof, so the cure is a new key.
const keyBindsReadTokenHint = "A live read token is bound to the key this proof was signed with. Make a new key."

// personSignIn records the sign-in the person's trade belongs to
// (ExchangeConfig.PersonSignIns), in the exchange's own transaction, and refuses a trade whose
// box key that person has revoked, or whose box key belongs to a sign-in made on another
// machine.
//
// The sign-in is the person, the box key and the machine: the person the decision named, the
// RFC 7638 thumbprint of the key the request's DPoP proof presented — which is the key
// `agent-secrets login` made at the sign-in and keeps with Google's refresh token — and the
// Google account the actor token named. The first trade of a person and a box key writes the
// row; every later trade of that pair, from that same machine, is the same sign-in used again.
//
// It runs in the transaction the token is written in, for the reason the machine's credential
// does: a revoke that commits first refuses this trade, and one that commits after it ends the
// token this trade minted, so no token outlives the sign-in it was minted under. The revoke
// needs both of its halves for that. store.RevokePersonSignInTx ends every live access token
// whose stored session names this person and this box key, but its one statement reads the
// tokens as they stood when it began, so a token from a trade that commits while the revoke waits
// on the sign-in row's lock is not among them; enroll.Service.RevokePersonSignIn's next
// statement ends that one, through the credential the sign-in holds (endCredential, by
// credential_id). That is why a person's trade may not decline the credential
// (personDeclinesNothing): a token with none would escape the second half.
//
// Both refusals are invalid_grant rather than a server error, and name the sign-in command: the
// subject token is sound and it is the broker's own record that refuses, which the person fixes
// by signing in again, with a key of its own. The machine's is a refusal and not an update
// because the credential this sign-in holds is keyed by all three (exchange.PersonSubject): a
// row that followed its key to another machine would ask for a subject no live credential has,
// and every trade of it from then on would collide on launcher_credentials_live_key and be
// refused invalid_grant with nothing naming the cure.
func (p *Provider) personSignIn(ctx context.Context, session *oauthstore.ExchangeSession) error {
	person, _ := session.Extra[ClaimPerson].(string)
	machine, _ := session.Extra[ClaimMachine].(string)
	thumbprint := session.GetDPoPJWKThumbprint()
	if person == "" || machine == "" || thumbprint == "" {
		return oauth2.ErrServerError.WithDebugf("the exchange reached a person's sign-in with person (%t), machine (%t) and bound key (%t)",
			person != "", machine != "", thumbprint != "")
	}
	err := p.exchange.PersonSignIns(ctx, PersonSignIn{Person: person, Thumbprint: thumbprint, Machine: machine})
	switch {
	case errors.Is(err, ErrSignInRevoked):
		return oauth2.ErrInvalidGrant.WithHintf("%s revoked this machine's sign-in, so the key this proof was signed with trades nothing. Sign in again with `agent-secrets login`.", person).WithWrap(err).WithDebugError(err)
	case errors.Is(err, ErrSignInMachineChanged):
		return oauth2.ErrInvalidGrant.WithHintf("%s's sign-in on this box key was made on another machine, and a sign-in belongs to the machine it was made on. Sign in again with `agent-secrets login`.", person).WithWrap(err).WithDebugError(err)
	}
	if err != nil {
		return oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	return nil
}

// signInCredential names, on a person's read trade's session, the launcher credential that
// person's sign-in on this machine already holds (exchange.PersonSubject: the person, the
// machine's account and the box key), read and held with oauthstore.LockHeldCredential in the
// exchange's own transaction. It mints, moves and extends nothing, which personCredential would.
//
// The token names the credential so the sign-in's revoke ends it: the revoke's second step ends
// that credential (enroll.Service.RevokePersonSignIn), and a narrowed token is live only while
// the credential it names is (oauthstore.Store.GetAccessTokenSession), a trade that commits
// while the revoke waits included. Its answer names no credential.
//
// A sign-in that holds no credential yet, and one whose credential has lapsed with the subject
// token it was last extended to, are refused invalid_grant naming the cure: `agent-secrets login`
// for the first, and for the second one ordinary trade of the sign-in, which `agent-secrets`
// makes and which renews the credential. The read tokens a lapsed credential named have ended
// with it.
func (p *Provider) signInCredential(ctx context.Context, session *oauthstore.ExchangeSession) error {
	person, _ := session.Extra[ClaimPerson].(string)
	machine, _ := session.Extra[ClaimMachine].(string)
	id, err := oauthstore.LockHeldCredential(ctx, exchange.PersonSubject(person, machine, session.GetDPoPJWKThumbprint()))
	switch {
	case errors.Is(err, oauthstore.ErrNoCredentialHeld):
		return oauth2.ErrInvalidGrant.WithHintf("%s's sign-in on this machine holds no launcher credential yet, and a read trade makes none. Sign in with `agent-secrets login` first.", person).WithWrap(err).WithDebugError(err)
	case errors.Is(err, oauthstore.ErrCredentialEnded):
		return oauth2.ErrInvalidGrant.WithHintf("%s's sign-in on this machine holds a launcher credential that has lapsed, and a read trade renews none. One ordinary trade of the sign-in renews it, as `agent-secrets` makes; then trade for read again.", person).WithWrap(err).WithDebugError(err)
	case err != nil:
		return oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	session.Extra[oauthwire.ExtraCredentialID] = id
	return nil
}

// personCredential creates or re-uses the launcher credential a person's sign-in on a machine
// holds (ExchangeConfig.MachineCredentials, the same seam a machine's own credential is minted
// through), in the exchange's own transaction, and writes its id onto the session so the stored
// token names it. It answers the id and what it asked for, which is what the token answer's
// extras repeat.
//
// It is why a person reaches their own secrets on a box at all. The exchange answers a Dispatch
// access token, which writes Dispatch and reads no secret; every secrets route is an
// enrollment's, made under a launcher credential and a proof signed by its key. The machine's
// own credential acts for no person, so a session enrolled under it reaches its fleet's secrets
// and no person's; this one carries the person as its operator, so a session enrolled under it
// is that person's, exactly as a dev box's machine login's sessions are (enroll.authorized).
// One sign-in gives the person both halves, and both end together, because the credential is
// written in the transaction the token is written in and the revoke ends them together too
// (enroll.Service.RevokePersonSignIn).
//
// It carries the fleet's service as well as the person, which the table requires of any
// credential keyed by a subject (launcher_credentials_subject_names_service): that service is
// what holds the credential to a listed fleet on every call (enroll.tokenLoginListed), and it
// grants nothing on its own, since a session's service authority is read off the enrollment's
// own recorded subject, which a credential carrying an operator never writes
// (enroll.Service.Create).
//
// The credential is keyed by the sign-in (exchange.PersonSubject: the person, the machine's
// account and the box key) and bound to that box key, so two people signed in on one box hold a
// credential each, one person signed in on two boxes holds one per box, and no sign-in's trade
// moves another's.
func (p *Provider) personCredential(ctx context.Context, r *http.Request, session *oauthstore.ExchangeSession) (string, MachineCredential, error) {
	person, _ := session.Extra[ClaimPerson].(string)
	machine, _ := session.Extra[ClaimMachine].(string)
	thumbprint := session.GetDPoPJWKThumbprint()
	key, err := proofKey(r, thumbprint)
	if err != nil {
		return "", MachineCredential{}, oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	expires, ok := expiryOf(session.GetSubjectToken())
	if !ok {
		return "", MachineCredential{}, oauth2.ErrInvalidRequest.WithHint("The subject token names no expiry, so there is nothing to expire the sign-in's credential with.")
	}
	cred := MachineCredential{
		Subject:    exchange.PersonSubject(person, machine, thumbprint),
		Service:    session.Fleet,
		Operator:   person,
		Host:       machineHost(exchange.ReadGoogleIdentity(session.GetActorToken())),
		Thumbprint: thumbprint,
		JWK:        key,
		Expires:    expires,
	}
	id, err := p.exchange.MachineCredentials(ctx, cred)
	if err != nil {
		if errors.Is(err, ErrMachineKeyTaken) {
			return "", MachineCredential{}, oauth2.ErrInvalidGrant.WithHint("Another live machine login holds the key this proof was signed with. A sign-in makes a box key of its own.").WithWrap(err).WithDebugError(err)
		}
		if errors.Is(err, ErrMachineKeyBindsReadToken) {
			return "", MachineCredential{}, oauth2.ErrInvalidGrant.WithHint(keyBindsReadTokenHint).WithWrap(err).WithDebugError(err)
		}
		return "", MachineCredential{}, oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	if session.Extra == nil {
		session.Extra = map[string]any{}
	}
	session.Extra[oauthwire.ExtraCredentialID] = id
	return id, cred, nil
}

// machineHost is the machine as its credential's row records it, which is what an operator reads
// on their machine-login page: the instance a Compute Engine token was minted on, and the
// account alone for a token minted anywhere else, which names no instance to show.
func machineHost(identity exchange.GoogleIdentity) string {
	if identity.Compute.Whole() {
		return identity.Compute.Project + "/" + identity.Compute.Zone + "/" + identity.Compute.Instance
	}
	return identity.Account
}

// proofKey is the public key of the DPoP proof on r, as JSON, which the machine's launcher
// credential records beside its thumbprint. The library has already verified this proof and
// bound the session to its key, and it publishes the parsed key only inside its own request
// handling, so the header is read again here and held to the thumbprint the library bound: a
// key whose thumbprint is not that one is not the key this request proved.
func proofKey(r *http.Request, thumbprint string) (json.RawMessage, error) {
	header := r.Header.Get("DPoP")
	if header == "" || thumbprint == "" {
		return nil, fmt.Errorf("oauth: the exchange reached the machine's credential with no DPoP proof (%t) or no bound key (%t)", header != "", thumbprint != "")
	}
	proof, err := jose.ParseSigned(header, subjectTokenAlgorithms)
	if err != nil {
		return nil, fmt.Errorf("oauth: re-read the request's DPoP proof: %w", err)
	}
	if len(proof.Signatures) != 1 || proof.Signatures[0].Header.JSONWebKey == nil {
		return nil, fmt.Errorf("oauth: the request's DPoP proof carries %d signatures and no embedded key", len(proof.Signatures))
	}
	key := proof.Signatures[0].Header.JSONWebKey.Public()
	jkt, err := jwt.ThumbprintJWK(&key)
	if err != nil {
		return nil, fmt.Errorf("oauth: the thumbprint of the DPoP proof's key: %w", err)
	}
	if jkt != thumbprint {
		return nil, fmt.Errorf("oauth: the DPoP proof's key is %s and the request is bound to %s", jkt, thumbprint)
	}
	raw, err := json.Marshal(&key)
	if err != nil {
		return nil, fmt.Errorf("oauth: the DPoP proof's key as JSON: %w", err)
	}
	return raw, nil
}

// expiryOf is a verified token's `exp` as a time. A claim map decoded from JSON holds a number
// as float64, and the library's own parse holds one as json.Number, so both are read; ok is
// false for a token that names no expiry at all.
func expiryOf(token map[string]any) (time.Time, bool) {
	switch value := token["exp"].(type) {
	case float64:
		return time.Unix(int64(value), 0), true
	case int64:
		return time.Unix(value, 0), true
	case json.Number:
		seconds, err := value.Int64()
		return time.Unix(seconds, 0), err == nil
	}
	return time.Time{}, false
}

// registeredPodSession refuses an exchange whose claims name an instance (exchange.Claims.Instance,
// a pod uid) that no live registration names for the session the token would write under,
// reading oauthstore.Store.PodSessionRegistered in the exchange's own transaction. A registration
// counts only while it names a registrar, that registrar has not released it, and the session
// name is bound to that registrar: a name is bound for good to the daemon that first registered
// it (oauthstore.Store.RegisterPodSession refuses it to every other), so a pod trades under a name
// only when the daemon holding that name registered it. A row no registrar is named on, which a
// broker of the build before registrars wrote, is answered as no row is. An exchange whose claims
// carry no instance is not gated: a fixed-session entry's pod, a workflow run and a dev box each
// carry a name their configuration or their person decides, and nothing of theirs is registered.
//
// It is the second, independent rule beside the claim on the session name: the claim refuses
// another principal the name, an account paired with the holder's excepted
// (oauthstore.Store.SetSessionPairs), and this refuses another pod of the same principal, which
// the claim cannot tell apart because the name belongs to the account; a pod of a paired account
// is held to it too. It runs here rather than inside decide because a refusal raised through the
// library's store hook arrives as invalid_request, and this one is invalid_grant: the subject
// token is sound and it is the broker's state that refuses it, exactly as the claim refuses a
// session name another principal holds and the caller may not take.
//
// Its error_description begins oauthwire.PodSessionNotRegistered and a colon, in place of the
// library's stock invalid_grant text, because unlike every other invalid_grant a later trade can
// cure it: a pod's trade can reach the broker before its registration does (its daemon registers
// at the claim, a session's watch at the pod's creation, and either registrar can be down for
// minutes), so both clients read these words as a refusal to trade again on rather than a final
// one.
//
// A registration that names a key (the thumbprint the registrar sent, which the Legion daemon
// sends for each role's own agent-secrets key) admits only an exchange whose DPoP proof is of that
// key, and refuses one proved with another invalid_grant, naming the session. The roles of one
// issue pod read one exchange token and are registered for one pod uid, so this is what keeps a
// role holding the pod's token and a key of its own from trading for a sibling role's session. A
// registration that names none is the pod's, as a session watch registers a session pod's one
// session.
//
// That refusal is the one trade here that is the attack the key exists to stop, so it logs a WARN
// line of its own naming the principal, the pod, the session, the key the registration is bound to
// and the key the trade proved: writeError logs only a server error, and without the line the
// refusal reads in the log as an expired grant or a registration not yet written does.
//
// A registration its registrar released is refused invalid_grant with the library's own
// description, never oauthwire.PodSessionNotRegistered: no later trade cures it, since only the
// registrar's own next write of that pod and name makes the registration live again. Its hint
// names the session and nothing of who released it, because no exchange refusal names another
// daemon: the pod asking is not that daemon's, and a private project's daemon account carries
// the project's name. It logs a WARN line naming the principal, the pod, the session and the
// registrar, which the operator reads and the pod does not.
//
// A live registration written by a daemon other than the one the session name is bound to, or of
// a name bound to none, is refused invalid_grant with a description beginning
// oauthwire.SessionNameNotThisRegistrars and a colon, never oauthwire.PodSessionNotRegistered: no
// later trade cures it, so both clients read it as final. A name another daemon holds is that
// daemon's for good, so only that daemon's registration of the pod would admit it. No route of
// this build writes a live row of a name bound to none, since the route binds the name in the
// transaction that writes the row; only a hand edit of the database, a row written or a binding
// deleted by hand, makes one, and only a registration that binds the name to the pod's registrar
// admits the pod. Each case logs a WARN line of its own and has a hint of its own. Each hint names
// the session and neither names a daemon, for the reason the released refusal names no daemon.
// Each WARN line names the principal, the pod, the session and the registrar that wrote the
// registration, and the line of a name another daemon holds names the holder too; the operator
// reads them and the pod does not.
func (p *Provider) registeredPodSession(ctx context.Context, session *oauthstore.ExchangeSession) error {
	if session.Instance == "" {
		return nil
	}
	name, _ := session.Extra[ClaimSession].(string)
	presented := session.GetDPoPJWKThumbprint()
	registered, err := p.store.PodSessionRegistered(ctx, session.Instance, name, presented)
	var bound *oauthstore.PodSessionBoundError
	var released *oauthstore.PodSessionReleasedError
	var notThisRegistrars *oauthstore.PodSessionNotThisRegistrarsError
	switch {
	case errors.As(err, &released):
		slog.Warn("broker: the token exchange refused a pod's trade under a registration its registrar released",
			"principal", session.Principal, "pod", session.Instance, "session", name, "registrar", released.Registrar)
		return oauth2.ErrInvalidGrant.WithHintf("Refused: this pod's registration for session '%s' was released; a pod trades only under a registration its registrar has not released.", name)
	case errors.As(err, &notThisRegistrars):
		refused := oauth2.ErrInvalidGrant.WithDescription(oauthwire.SessionNameNotThisRegistrars + ":")
		if notThisRegistrars.Holder == "" {
			slog.Warn("broker: the token exchange refused a pod's trade under a session name bound to no daemon",
				"principal", session.Principal, "pod", session.Instance, "session", name,
				"registrar", notThisRegistrars.Registrar)
			return refused.WithHintf("The session name '%s' is bound to no daemon: a pod trades only under a session name the daemon that registered it holds.", name)
		}
		slog.Warn("broker: the token exchange refused a pod's trade under a session name another daemon holds",
			"principal", session.Principal, "pod", session.Instance, "session", name,
			"registrar", notThisRegistrars.Registrar, "holder", notThisRegistrars.Holder)
		return refused.WithHintf("The session name '%s' belongs to another daemon: a pod trades only under a session name the daemon that registered it holds.", name)
	case errors.As(err, &bound):
		slog.Warn("broker: the token exchange refused a pod's trade proved with a key other than the one its registration for the session is bound to",
			"principal", session.Principal, "pod", session.Instance, "session", name,
			"bound_thumbprint", bound.Bound, "presented_thumbprint", presented)
		return oauth2.ErrInvalidGrant.WithHintf("Refused: this pod's registration for session '%s' is bound to another key; a role trades only for its own session.", name)
	case err != nil:
		return oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	if !registered {
		return oauth2.ErrInvalidGrant.WithDescription(oauthwire.PodSessionNotRegistered+":").
			WithHintf("No registration says this pod may write under the session name '%s'. A pod writes under a session name its deployment registered for it.", name)
	}
	return nil
}

// audience refuses a subject token whose `aud` does not carry the audience its issuer is
// configured with. The library checks a subject token's issuer and its expiry and never its
// audience, so without this a token a pod was given for another service would be spendable here.
func (p *Provider) audience(subjectTokenType string, subjectToken map[string]any) error {
	issuer, ok := p.exchange.issuerOf(subjectTokenType)
	if !ok {
		return nil
	}
	if hasAudience(subjectToken["aud"], issuer.Audience) {
		return nil
	}
	return fmt.Errorf("the subject token was not minted for %s, which is the audience this broker accepts from %s", issuer.Audience, issuer.Issuer)
}

// hasAudience reports whether an `aud` claim carries want. RFC 7519 Section 4.1.3 allows either
// one string or an array of them, and a claim map decoded from JSON holds the array as []any.
func hasAudience(claim any, want string) bool {
	switch audience := claim.(type) {
	case string:
		return audience == want
	case []string:
		return slices.Contains(audience, want)
	case []any:
		for _, entry := range audience {
			if text, ok := entry.(string); ok && text == want {
				return true
			}
		}
	}
	return false
}
