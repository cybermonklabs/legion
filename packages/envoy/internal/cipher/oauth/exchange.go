package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc8693"
	"authelia.com/provider/oauth2/token/jose"
	"authelia.com/provider/oauth2/token/jwt"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/hold"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/oidc"
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
	// oauthwire.ProjectScopePerson, oauthwire.ProjectScopeNone or oauthwire.ProjectScopePublic:
	// the none scope for a read entry's token, which is granted oauthwire.ScopeRead beside it, or
	// for a trade that asked for oauthwire.ScopeRead the narrowed word of one of the first three
	// (oauthwire.ProjectScopeReadNamed, oauthwire.ProjectScopeReadPerson,
	// oauthwire.ProjectScopeReadNone), whose projects mean what its base word's do
	// (writeProjectScope); and the public scope for a job entry's alone. A job's token is named or
	// public as its entry's list is, and asks Dispatch nothing (projectAccess), because the
	// operator's entry chose its list and it names no person whose sight of a project there is to
	// check; no trade narrows it. It is written always, so a reader never has to take an absent
	// list as "every project", "no project at all" or a claim nobody decided.
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
	// ClaimAccountIssuer and ClaimAccountSubject are the Google account the credential behind a
	// token naming a person records, written onto the token's own session when it is minted and
	// read nowhere else (Provider.Introspect): for a person's sign-in on a machine, the `iss` (as
	// oidc.CanonicalIssuer spells it) and `sub` of the person token that made the trade
	// (personSignIn); for a token exchanged from a machine login's own, the account that approved
	// that login (launcher_credentials' approver columns, read once in the exchange's transaction,
	// approverAccount). Both are absent for a token recording none: one naming no person, one of a
	// login no person's account approved, and one minted before the broker recorded them on the
	// token, which an introspection answers as recording no account and never reads a row for.
	ClaimAccountIssuer  = "account_issuer"
	ClaimAccountSubject = "account_subject"
	// ClaimCredentialCreatedAt is when the credential behind a token naming a person was made, as
	// Unix seconds from the broker database's own clock: the person's sign-in on a machine
	// (person_signins.created_at, personSignIn) or the machine login a token was exchanged from
	// (launcher_credentials.created_at, approverAccount), not the token's own iat, which every
	// refresh renews. It is written onto the token's own session when it is minted, read nowhere
	// else (Provider.Introspect), and absent for a token naming no person and for one minted
	// before the broker recorded it. A resource server holds a token recording no account to it.
	ClaimCredentialCreatedAt = "credential_created_at"
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

// ExchangeConfig is what the RFC 8693 token exchange decides and verifies with. None of it is the
// provider's to decide: the broker's wiring reads it from the deployment's configuration and
// hands it over whole.
type ExchangeConfig struct {
	// Registry is the broker's identity providers (internal/cipher/identity/boot): the token
	// types each adds to the exchange, configured or not, the decision each makes for its own
	// subject tokens, and the listed identities of all of them. A nil Registry is a deployment
	// that has listed none and trusts no issuer, which New reads as the empty registry: the
	// exchange is composed whether or not any identity or issuer is, because the token-exchange
	// grant is the exchange client's only grant and a caller it refuses must be refused by name.
	Registry *identity.Registry
	// MachineCredentials creates or re-uses the launcher credential a machine holds, in the
	// transaction the exchange carries (oauthstore.TxFrom), and answers its id. Every secrets
	// route is an enrollment's, and an enrollment is made under a launcher credential and a
	// proof signed by its key, so without this a machine holding a Dispatch access token could
	// still read no secret. It is required of a broker that registers a configured token type
	// minting a launcher credential and unused by one that does not;
	// enroll.Service.IssueTokenLoginTx is what the broker's wiring hands over.
	MachineCredentials MachineCredentials
	// PersonSignIns records the sign-in of the person a trade names on the box key its DPoP
	// proof presented, as the account the person's token names, in the transaction the exchange
	// carries, and refuses with ErrSignInRevoked a trade whose key that person has revoked and
	// with ErrSignInSubjectReplaced one whose account no longer holds the person's address. It is
	// what makes a revoked sign-in stop working at the broker rather than at the person's own
	// machine, and it is required of a broker that registers a configured sign-in token type and
	// unused by one that does not.
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
	// Hold is the check of whether Dispatch holds a person's credentials until they sign in
	// again (hold.Gate.Decide), built on the Dispatch client Projects is, so the two share one
	// cache of person answers. Nil is a broker with no Dispatch to ask, which holds nobody.
	Hold *hold.Gate
}

// validate refuses a configuration the exchange could not serve: a configured type that mints a
// launcher credential with no MachineCredentials to mint it, and a configured sign-in type with no
// PersonSignIns to record the sign-in.
func (c ExchangeConfig) validate() error {
	for _, tt := range c.Registry.TokenTypes() {
		if tt.Launcher != identity.LauncherNone && c.MachineCredentials == nil {
			return errors.New("oauth: a broker that accepts a machine's Google identity token needs MachineCredentials: the exchange mints the machine's launcher credential, and without it no machine it answers could enroll or read a secret")
		}
		if tt.Launcher == identity.LauncherSignIn && c.PersonSignIns == nil {
			return errors.New("oauth: a broker that accepts a person's Google identity token needs PersonSignIns: the first trade records the sign-in the box key belongs to, and every later one is held to it")
		}
	}
	return nil
}

// MachineCredential is the launcher credential one machine's exchange asks for. The broker's
// wiring turns it into a row; nothing here is the provider's to decide.
type MachineCredential struct {
	// Subject keys the credential, so a machine's next exchange moves the one credential it
	// already holds rather than adding another (the identity provider's LauncherSubject).
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
	// Issuer and Subject are the account the person's own Google identity token was minted for:
	// its `iss`, as oidc.CanonicalIssuer spells it, and its `sub`. An address is held by one
	// account at a time, and Dispatch names the account an address no longer belongs to by
	// these two, which is what the broker ends that account's sign-ins by.
	Issuer  string
	Subject string
}

// PersonSignIns records the sign-in in, in the transaction ctx carries, and answers
// ErrSignInRevoked when that person has revoked that box key, ErrSignInMachineChanged and
// ErrSignInSubjectChanged when the sign-in that key belongs to was made on another machine or by
// another account, and ErrSignInSubjectReplaced when the account no longer holds the person's
// address.
type PersonSignIns func(ctx context.Context, in PersonSignIn) error

// subjectTokenTypes is every type in types a caller may present as a subject token, sorted: all
// of them but the actor-only types, which the exchange registers beside a type whose trade
// carries one, and which are no subject of their own. It is read off the registered types rather
// than built from the configuration a second time, so what the metadata document names is
// exactly what the library will accept.
func subjectTokenTypes(types map[string]oauth2.RFC8693TokenType, registry *identity.Registry) []string {
	names := make([]string, 0, len(types))
	for name := range types {
		// The proxy placeholder is no subject token of its own, whatever the registry holds: it is
		// the type a release hands the airlock, and it is registered by this provider, not by an
		// identity provider.
		if name == oauthwire.TokenTypeProxyPlaceholder {
			continue
		}
		if tt, _, ok := registry.TokenType(name); !ok || !tt.ActorOnly {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// tokenTypes are the token types the library registers for the exchange, keyed by the exact
// string a request names them with.
//
// The access token is always registered: it is the only kind this provider issues, so it is both
// the default requested type and the type a dev box session presents its machine access token
// under. Each identity provider's type is registered only where its issuer is configured
// (identity.TokenType.Configured), so a caller presenting an issuer the broker was never told
// about is refused by the library rather than by a half-built handler. Each is held to its
// provider's issuer, its lifetime tolerance, and single use where its tokens are minted per
// request; its key is resolved against its issuer's published keys (keyFunc).
func (c ExchangeConfig) tokenTypes() map[string]oauth2.RFC8693TokenType {
	types := map[string]oauth2.RFC8693TokenType{
		tokenTypeAccessToken: &rfc8693.DefaultTokenType{Name: tokenTypeAccessToken},
		// The proxy placeholder, the one other type a caller may ask for: a person's sign-in
		// traded for the approve capability (exchangeGrant, grantApprove). It is no subject type.
		oauthwire.TokenTypeProxyPlaceholder: &rfc8693.DefaultTokenType{Name: oauthwire.TokenTypeProxyPlaceholder},
	}
	for _, tt := range c.Registry.TokenTypes() {
		types[tt.Name] = &rfc8693.JWTType{
			Name:   tt.Name,
			Issuer: tt.Issuer,
			JWTValidationConfig: rfc8693.JWTValidationConfig{
				ValidateJTI:                tt.SingleUse,
				JWTLifetimeToleranceWindow: tt.Tolerance,
				ValidateFunc:               keyFunc(tt.Keys),
			},
		}
	}
	return types
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
func keyFunc(keys identity.KeySource) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		if !slices.Contains(subjectTokenAlgorithms, token.SignatureAlgorithm) {
			return nil, fmt.Errorf("oauth: a subject token signed with %q is refused: its issuer signs with a published key", token.SignatureAlgorithm)
		}
		return keys.Key(context.Background(), token.KeyID, string(token.SignatureAlgorithm))
	}
}

// exchangeGrant is the token endpoint's RFC 8693 branch: a caller's platform identity for a
// Dispatch access token, bound to the key its DPoP proof names, or, for a read trade that sends a
// holder proof, to the key that proof names (holderKey).
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
// set, and the claim must not end before the token it is made for.
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
// The hold of the token's person (heldRefusal) is asked in the first half, before projectAccess,
// so the gate rather than projectAccess's member kind normally fetches Dispatch's answer for the
// person and keeps it, and before the transaction, so a slow Dispatch holds no row lock. Its
// answer is applied in the second half, once the credential's own checks have passed
// (liveCredential, personSignIn): a revoked or ended credential gets its own refusal first, and a
// held person's trade then rolls back, minting nothing.
//
// A trade that asks for the approve capability's proxy placeholder (placeholderTrade) is answered
// no token. It may not ask for oauthwire.ScopeRead, and one that does is refused before the
// transaction, since a trade narrowed to reading approves nothing. The library builds nothing for
// it: the approve grant is written in the transaction instead (grantApprove), and sets the expiry
// the session name is claimed until. Both answers end the same way, with the claim, the commit and
// the warning for a name the claim took (logSessionTaken), and then the answer is written.
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
	placeholderTrade := request.GetRequestForm().Get(formRequestedTokenType) == oauthwire.TokenTypeProxyPlaceholder
	if read && placeholderTrade {
		// A trade narrowed to reading approves nothing, and the approve grant holds the scope
		// approve alone, so a placeholder trade that asks for read - with a cut or a holder proof,
		// which only such a trade may carry - is refused before its holder proof is read, before
		// its claims are held to Dispatch and before the transaction: nothing of a grant or a token
		// is written.
		p.writeError(ctx, w, request, oauth2.ErrInvalidRequest.WithHintf(placeholderReadRefused, oauthwire.ScopeRead))
		return
	}
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
	held := p.heldRefusal(ctx, personOf(session))
	holder, err := p.holderKey(ctx, r, subjectTokenType, session, read)
	if err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	if err := p.projectAccess(ctx, subjectTokenType, session); err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	// A lifetime the decision held the token to counts from here, the last answer of Dispatch that
	// admitted it, and not from the mint, which waits on the transaction below.
	session.StartAccessLifetime(time.Now())
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
		if err := p.approverAccount(txCtx, session); err != nil {
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
	launcher := p.launcher(subjectTokenType)
	if asked && launcher == identity.LauncherMachine {
		credentialID, credential, err = p.machineCredential(txCtx, r, subjectTokenType, session)
		if err != nil {
			p.writeError(ctx, w, request, err)
			return
		}
	}
	if launcher == identity.LauncherSignIn {
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
			credentialID, credential, err = p.personCredential(txCtx, r, subjectTokenType, session)
		}
		if err != nil {
			p.writeError(ctx, w, request, err)
			return
		}
	}
	if held != nil {
		p.writeError(ctx, w, request, held)
		return
	}
	if err := p.registeredPodSession(txCtx, session); err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	var placeholderAnswer map[string]any
	var resp oauth2.AccessResponder
	if placeholderTrade {
		// A person's sign-in alone, on a broker that serves the capability: the grant is written
		// in this transaction, and the library mints nothing.
		placeholderAnswer, err = p.grantApprove(txCtx, session, subjectTokenType, credentialID)
	} else {
		if holder != "" {
			// The token is the holder's: the library stores the session as it stands, answers its
			// token type from this binding, and an introspection answers it as the token's jkt.
			session.SetDPoPJWKThumbprint(holder)
		}
		resp, err = p.oauth.NewAccessResponse(txCtx, request)
	}
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
	logSessionTaken(session, taken, fixed)
	if placeholderTrade {
		writeOAuthJSON(w, http.StatusOK, placeholderAnswer)
		return
	}
	if credentialID != "" {
		// Named in the answer as the device login's own token answer names it, so one client
		// reads a credential the same way whichever grant gave it one.
		resp.SetExtra(oauthwire.ExtraCredentialID, credentialID)
		resp.SetExtra(oauthwire.ExtraCredentialExpiresAt, credential.Expires.UTC().Format(time.RFC3339))
	}
	p.oauth.WriteAccessResponse(ctx, w, request, resp)
}

// logSessionTaken writes the warning for a session name claimSession took from another principal
// (taken, "" for none), once the claim has committed: the two ways a principal loses a name it used
// within the retention. exchangeGrant writes it for both of its answers, the access token and the
// approve capability's placeholder, since both claim the name the same way.
func logSessionTaken(session *oauthstore.ExchangeSession, taken string, fixed bool) {
	switch {
	case taken != "" && fixed:
		// The configuration now fixes the name for an entry.
		slog.Warn("broker: the token exchange took a session name the configuration fixes from its holder",
			"session", session.Extra[ClaimSession], "from", taken, "to", session.Principal)
	case taken != "":
		// A claim the configuration does not fix takes a live name only from an account paired
		// with its own (oauthstore.Store.SetSessionPairs), as an operator moving a deployment's
		// pods to another account does.
		slog.Warn("broker: the token exchange took a session name from the account paired with its own",
			"session", session.Extra[ClaimSession], "from", taken, "to", session.Principal)
	}
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

// placeholderReadRefused is the refusal of a trade that asks for the approve capability's proxy
// placeholder and for the scope read (its verb is the scope): such a trade is narrowed to reading,
// and a session narrowed to reading approves nothing.
const placeholderReadRefused = "A trade for the approve capability's proxy placeholder asks for the scope %s, and a trade narrowed to reading approves nothing: trade for the placeholder without the scope, a cut or a DPoP-Holder proof."

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
// for all five. The one thing it asks outside the request is the machine access-token type's:
// what kind of member Dispatch calls the login's approver (approverKind), which decides that
// token's scope, and which it asks before any transaction opens, as exchangeGrant calls it for
// that type alone. What Dispatch says about the projects it decided is projectAccess's question,
// asked after it.
//
// In order: the projects the request names, which must be a list (oauthwire.ParseProjects) before
// anything else is read, and the cut, which only a trade asking for oauthwire.ScopeRead may name
// (readCut); the subject token's audience, which the library never checks and which a token
// minted for another service would fail; the policy's decision, which names the session, the
// person or the service, the project scope, the projects and the principal; the refusal of a job
// entry's trade that asks for read (exchange.Claims.Job); and then the claims themselves, onto
// the session, where the row the token is stored in carries them and an introspection answers
// from them, with the principal beside them for claimSession, the lifetime the token is held to
// (oauthstore.ExchangeSession.AccessLifetime), and the OAuth scopes the decision carries granted
// on the request (writeProjectScope).
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
func (p *Provider) decide(ctx context.Context, request oauth2.Requester, subjectToken map[string]any) (string, error) {
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
	if subjectTokenType == tokenTypeAccessToken {
		// A machine access token is the broker's own, so there is no issuer to check it against:
		// the login that was issued it recorded the approver it acts for and the launcher
		// credential it holds, and those are the identity. What Dispatch calls that approver
		// decides the token's scope, so it is asked first, and a refusal to answer is the trade's.
		credential, _ := subjectToken[oauthwire.ExtraCredentialID].(string)
		approver, _ := subjectToken["sub"].(string)
		kind, refused := p.approverKind(ctx, approver)
		if refused != nil {
			return "", refused
		}
		claims, err = p.exchange.Registry.Policy().Machine(credential, approver, name, projects, kind)
	} else {
		tt, provider, ok := p.exchange.Registry.TokenType(subjectTokenType)
		if !ok || tt.ActorOnly || !tt.Configured() {
			return "", oauth2.ErrInvalidRequest.WithHintf("The '%s' token type is not one this broker exchanges.", subjectTokenType)
		}
		err = p.audience(tt, subjectToken)
		// A type whose trade carries an actor holds both tokens, each to the audience its own
		// type is configured with, so neither is spendable in the other's place: a person's to
		// the device-flow client's, the machine's to the one a machine trades for itself with.
		// The actor token's claims are on the session because the library validated it before it
		// asked for the subject (rfc8693.CustomJWTTypeHandler), and a request that carried none
		// leaves it nil, which the provider refuses by name. A type that names no actor never
		// reads one, whatever the request carried.
		var actor map[string]any
		if tt.Actor != "" {
			actor = session.GetActorToken()
			if actorType, _, known := p.exchange.Registry.TokenType(tt.Actor); err == nil && actor != nil && known {
				err = p.audience(actorType, actor)
			}
		}
		if err == nil {
			claims, err = provider.Decide(identity.Trade{TokenType: subjectTokenType, Subject: subjectToken, Actor: actor, Session: name, Projects: projects})
		}
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
	session.AccessLifetime = claims.Lifetime
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
	// No account carries over from a session read back out of a subject token's row: a token
	// records the account its own trade names (personSignIn, approverAccount), or none, and the
	// creation time of its own credential.
	delete(session.Extra, ClaimAccountIssuer)
	delete(session.Extra, ClaimAccountSubject)
	delete(session.Extra, ClaimCredentialCreatedAt)
	if err := writeProjectScope(request, session, claims, read); err != nil {
		return "", err
	}
	if cut.IsZero() {
		delete(session.Extra, ClaimAsOf)
	} else {
		session.Extra[ClaimAsOf] = cut.UTC().Format(cutLayout)
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
// No path through decide hands it one today, since every identity provider's decision and the
// set's own (exchange.Set.Machine) set one of the four values below, so the refusal is what keeps
// a value the exchange package adds later from being minted as one of them before this switch
// learns its word.
func writeProjectScope(request oauth2.Requester, session *oauthstore.ExchangeSession, claims exchange.Claims, read bool) error {
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

// personOf is the person the token decide wrote onto session names, "" for one naming none: a
// machine login's token names the person its stored session names as the login's operator, a
// sign-in trade its person, and an exchange entry the person it lists.
func personOf(session *oauthstore.ExchangeSession) string {
	person, _ := session.Extra[ClaimPerson].(string)
	return person
}

// heldRefusal is the token endpoint's answer for a credential of person, whose hold the gate
// (ExchangeConfig.Hold) is asked here and the caller applies once the credential's own checks have
// passed, by the gate's Verdict: nil for a person the gate serves, for no person and on a broker
// with no gate; membership_held, naming the person in the gate's words, for hold.Held; and for
// every kind that decides nothing, a held answer the gate could not keep among them, server_error
// saying why in fixed words (hold.Verdict.Words), or refusedByDispatch when Dispatch refused the
// broker's own account. The gate's own error goes to the log alone.
func (p *Provider) heldRefusal(ctx context.Context, person string) error {
	if person == "" {
		return nil
	}
	switch v := p.exchange.Hold.Decide(ctx, person); v.Kind {
	case hold.Served:
		return nil
	case hold.Held:
		return (&oauth2.RFC6749Error{
			ErrorField:       oauthwire.MembershipHeld,
			DescriptionField: v.Words(),
			CodeField:        http.StatusBadRequest,
		}).WithWrap(v.Err).WithDebugError(v.Err)
	case hold.BrokerRefused:
		return refusedByDispatch(v.Err)
	default:
		return oauth2.ErrServerError.WithHint(v.Words()).WithWrap(v.Err).WithDebugError(v.Err)
	}
}

// approverUnreachable is a machine login's trade's refusal when Dispatch could not be asked what
// kind of member the login's approver is, which a client asks again on.
func approverUnreachable(err error) error {
	return oauth2.ErrTemporarilyUnavailable.WithHint("Dispatch could not be asked what kind of member the machine login's approver is, so this broker cannot tell whether the token is held to one project; try again.").WithWrap(err).WithDebugError(err)
}

// approverKind is what Dispatch calls the approver of a machine login whose token is being traded
// (projectaccess's MemberKind), which decides that token's scope (exchange.Set.Machine): a
// restricted member's machine is held to the one project its trade names, and everyone else's acts
// as its person, a full member's and that of a person Dispatch holds no row for alike. A broker
// that checks no project against Dispatch asks nothing and answers ApproverUnrestricted, as it
// mints every trade naming no project; a login naming no approver asks nothing either, and
// answers ApproverUnknown, which Machine refuses after its own refusal of the missing approver.
//
// While Dispatch cannot be asked, an approver whose last kept member kind is full, and no older
// than projectaccess.KeptAnswerTTL (one hour), answers ApproverUnrestricted, so the trade goes on,
// a list naming no project included. Every other
// unanswered read is refused temporarily_unavailable: this broker cannot tell a restricted
// member's machine from anyone else's then, and a guess would mint a token acting as its person
// for one. The broker's own account token not working (refusedByDispatch) and a member kind it
// does not know (unknownMemberKind) are refused server_error, as every trade that meets them is.
func (p *Provider) approverKind(ctx context.Context, approver string) (exchange.ApproverKind, error) {
	if strings.TrimSpace(approver) == "" {
		return exchange.ApproverUnknown, nil
	}
	if p.exchange.Projects == nil {
		return exchange.ApproverUnrestricted, nil
	}
	kind, err := p.exchange.Projects.MemberKind(ctx, approver)
	switch {
	case errors.Is(err, dispatchread.ErrBrokerRefused):
		return exchange.ApproverUnknown, refusedByDispatch(err)
	case errors.Is(err, projectaccess.ErrUnknownMemberKind):
		return exchange.ApproverUnknown, unknownMemberKind(err)
	case err != nil:
		return exchange.ApproverUnknown, approverUnreachable(err)
	}
	switch kind {
	case projectaccess.MemberRestricted:
		return exchange.ApproverRestricted, nil
	case projectaccess.MemberFull, projectaccess.MemberNone:
		return exchange.ApproverUnrestricted, nil
	}
	err = fmt.Errorf("%w: %q for %s", projectaccess.ErrUnknownMemberKind, kind, approver)
	return exchange.ApproverUnknown, unknownMemberKind(err)
}

// projectAccess holds the claims decide wrote onto session to what Dispatch says, before the
// exchange opens its transaction (exchangeGrant). It reads the claims back off the session, so
// what it checks is exactly what the token will carry.
//
// A token a person's own credential made: every project it names must be one Dispatch says that
// person sees (projectaccess.Visible). That is a token acting as its person, a sign-in's or a
// machine login's, and a restricted member's machine login's, which names exactly one project
// (exchange.Set.Machine). Whether the person is a restricted member was settled before it was
// decided: a sign-in is taken as a full member's and asks no member kind at all, since the broker
// verified its account against BROKER_SIGNIN_DOMAIN itself, and a machine login's trade asked
// Dispatch already (approverKind), so this asks no member kind of either. A list that names no
// project mints as it always has; one that names a project is never minted unchecked: with no
// check configured it is refused naming the setting, and while Dispatch cannot be asked it is
// refused temporarily_unavailable, which a client asks again on.
//
// A token an operator's entry maps to its person: its one project must be one Dispatch says the
// entry's person sees, and a pairing Dispatch refuses is refused invalid_request naming both. An
// account entry that names no project (the shared public-session account's) is refused when
// Dispatch calls its person a restricted member, who sees only the projects that name them
// (projectaccess.ListAllowed), and mints while Dispatch cannot be asked
// (projectaccess.ErrUnchecked), logging one line naming the principal, the session and the
// person. With no check configured an entry's token mints as it always has, since the operator's
// own setting is what chose the project, or chose none.
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
	if scope == oauthwire.ProjectScopeNamed && subjectTokenType != tokenTypeAccessToken {
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
	if len(projects) == 0 {
		return nil
	}
	if access == nil {
		return oauth2.ErrInvalidRequest.WithHintf("The token names projects, and this broker checks no project against Dispatch: its operator sets BROKER_PROJECT_ACCESS_URL and BROKER_PROJECT_ACCESS_TOKEN_FILE to take a list of projects.")
	}
	err = projectaccess.Visible(ctx, access, person, projects)
	var unseen *projectaccess.NotVisibleError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &unseen):
		return oauth2.ErrInvalidRequest.WithHintf("The token names a project its person cannot see: %s.", unseen.Project).WithWrap(err).WithDebugError(err)
	case errors.Is(err, dispatchread.ErrBrokerRefused):
		return refusedByDispatch(err)
	default:
		return dispatchUnreachable(err)
	}
}

// claimSession claims the session name decide wrote onto session for the principal it decided,
// until oauthstore.SessionRetention past the expiry the library has just set on the issued token,
// or grantApprove on the approve grant it wrote, in the transaction that stores that token or
// grant: the claim can never end before what it was made for, and a token or grant whose claim is
// refused is never stored. It answers the principal it took the name from ("" for none) and
// whether the configuration fixed the claim, which is what tells exchangeGrant's log a fixed
// claim's take from a pair's.
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

// approverAccount writes onto the session of a token exchanged from a machine login's own the
// Google account that approved that login (oauthstore.Store.CredentialApprover: the credential's
// approver columns, written once when the login was approved) and when the login was made, read in
// the exchange's transaction after liveCredential holds the credential. The token records both
// from then on, so an introspection answers them from the token's own row (ClaimAccountIssuer,
// ClaimCredentialCreatedAt). A login no account approved records no account, and its token none,
// but its creation time all the same: that is what a resource server holds a token recording no
// account to. A token naming no person records neither.
func (p *Provider) approverAccount(ctx context.Context, session *oauthstore.ExchangeSession) error {
	if person, _ := session.Extra[ClaimPerson].(string); person == "" {
		return nil
	}
	credential, _ := session.GetSubjectToken()[oauthwire.ExtraCredentialID].(string)
	read, err := p.store.CredentialApprover(ctx, credential)
	if err != nil {
		return oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	if read.Issuer != "" && read.Subject != "" {
		session.Extra[ClaimAccountIssuer] = read.Issuer
		session.Extra[ClaimAccountSubject] = read.Subject
	}
	recordCredentialCreatedAt(session, read.CreatedAt)
	return nil
}

// recordCredentialCreatedAt writes onto session when the credential behind the token it mints was
// made, as Unix seconds (ClaimCredentialCreatedAt), and nothing for the zero time.
func recordCredentialCreatedAt(session *oauthstore.ExchangeSession, createdAt time.Time) {
	if !createdAt.IsZero() {
		session.Extra[ClaimCredentialCreatedAt] = createdAt.Unix()
	}
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
func (p *Provider) machineCredential(ctx context.Context, r *http.Request, subjectTokenType string, session *oauthstore.ExchangeSession) (string, MachineCredential, error) {
	subjectToken := session.GetSubjectToken()
	thumbprint := session.GetDPoPJWKThumbprint()
	key, err := proofKey(r, thumbprint)
	if err != nil {
		return "", MachineCredential{}, oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	expires, ok := expiryOf(subjectToken)
	if !ok {
		return "", MachineCredential{}, oauth2.ErrInvalidRequest.WithHint("The subject token names no expiry, so there is nothing to expire the machine's credential with.")
	}
	subject, host := p.launcherSubject(subjectTokenType, session, thumbprint)
	cred := MachineCredential{
		Subject:    subject,
		Service:    session.Fleet,
		Host:       host,
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
// account no longer holds the person's address, whose box key that person has revoked, or whose
// box key belongs to a sign-in made on another machine or by another account.
//
// The sign-in is the person, the box key, the machine and the account: the person the decision
// named, the RFC 7638 thumbprint of the key the request's DPoP proof presented — which is the key
// `agent-secrets login` made at the sign-in and keeps with Google's refresh token — the Google
// account the actor token named, and the `iss` and `sub` of the person's own token, which the
// library has verified. The first trade of a person and a box key writes the row; every later
// trade of that pair, from that same machine and by that same account, is the same sign-in used
// again.
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
// Every refusal is invalid_grant rather than a server error: the subject token is sound and it is
// the broker's own record that refuses. Three of them the person fixes by signing in again, with a
// key of its own, and they name the sign-in command. The machine's is a refusal and not an update
// because the credential this sign-in holds is keyed by the person, the machine and the key
// (exchange.PersonSubject): a row that followed its key to another machine would ask for a subject
// no live credential has, and every trade of it from then on would collide on
// launcher_credentials_live_key and be refused invalid_grant with nothing naming the cure. The
// account's is a refusal because the account is what a revoke of a replaced account ends the
// sign-in by. The fourth, an account that no longer holds the address, nothing on this box fixes:
// Google gave the address to another account, and it says so.
func (p *Provider) personSignIn(ctx context.Context, session *oauthstore.ExchangeSession) error {
	person, _ := session.Extra[ClaimPerson].(string)
	machine, _ := session.Extra[ClaimMachine].(string)
	thumbprint := session.GetDPoPJWKThumbprint()
	issuer, _ := session.GetSubjectToken()["iss"].(string)
	subject, _ := session.GetSubjectToken()["sub"].(string)
	if person == "" || machine == "" || thumbprint == "" || issuer == "" || subject == "" {
		return oauth2.ErrServerError.WithDebugf("the exchange reached a person's sign-in with person (%t), machine (%t), bound key (%t), issuer (%t) and subject (%t)",
			person != "", machine != "", thumbprint != "", issuer != "", subject != "")
	}
	err := p.exchange.PersonSignIns(ctx, PersonSignIn{
		Person: person, Thumbprint: thumbprint, Machine: machine,
		Issuer: oidc.CanonicalIssuer(issuer), Subject: subject,
	})
	switch {
	case errors.Is(err, ErrSignInSubjectReplaced):
		return oauth2.ErrInvalidGrant.WithHintf("The Google account this token was minted for no longer holds %s: another account holds that address now, and the broker takes no sign-in of the account it replaced.", person).WithWrap(err).WithDebugError(err)
	case errors.Is(err, ErrSignInRevoked):
		return oauth2.ErrInvalidGrant.WithHintf("%s revoked this machine's sign-in, so the key this proof was signed with trades nothing. Sign in again with `agent-secrets login`.", person).WithWrap(err).WithDebugError(err)
	case errors.Is(err, ErrSignInMachineChanged):
		return oauth2.ErrInvalidGrant.WithHintf("%s's sign-in on this box key was made on another machine, and a sign-in belongs to the machine it was made on. Sign in again with `agent-secrets login`.", person).WithWrap(err).WithDebugError(err)
	case errors.Is(err, ErrSignInSubjectChanged):
		return oauth2.ErrInvalidGrant.WithHintf("%s's sign-in on this box key was made by another Google account, and a sign-in belongs to the account that made it. Sign in again with `agent-secrets login`.", person).WithWrap(err).WithDebugError(err)
	}
	if err != nil {
		return oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	// Write the account this trade's identity token names, and when the sign-in row was made, onto
	// the session the token is stored with, so an introspection answers both from the token's own
	// row and never from the sign-in's. A pre-0047 sign-in can record no account when EndUnrecorded
	// is false; its newly minted token still records this trade's identity account.
	session.Extra[ClaimAccountIssuer] = oidc.CanonicalIssuer(issuer)
	session.Extra[ClaimAccountSubject] = subject
	createdAt, err := p.store.SignInCreatedAt(ctx, person, thumbprint)
	if err != nil {
		return oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	recordCredentialCreatedAt(session, createdAt)
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
func (p *Provider) personCredential(ctx context.Context, r *http.Request, subjectTokenType string, session *oauthstore.ExchangeSession) (string, MachineCredential, error) {
	person, _ := session.Extra[ClaimPerson].(string)
	thumbprint := session.GetDPoPJWKThumbprint()
	key, err := proofKey(r, thumbprint)
	if err != nil {
		return "", MachineCredential{}, oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	expires, ok := expiryOf(session.GetSubjectToken())
	if !ok {
		return "", MachineCredential{}, oauth2.ErrInvalidRequest.WithHint("The subject token names no expiry, so there is nothing to expire the sign-in's credential with.")
	}
	subject, host := p.launcherSubject(subjectTokenType, session, thumbprint)
	cred := MachineCredential{
		Subject:    subject,
		Service:    session.Fleet,
		Operator:   person,
		Host:       host,
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

// launcher is the launcher credential a trade of subjectTokenType mints: none for the broker's own
// access token and for any type no provider registers.
func (p *Provider) launcher(subjectTokenType string) identity.Launcher {
	tt, _, _ := p.exchange.Registry.TokenType(subjectTokenType)
	return tt.Launcher
}

// launcherSubject is the subject and host of the credential the trade on session mints, as the
// provider of its subject token type spells them, thumbprint being the key its DPoP proof
// presented. The person and the machine a sign-in names are the ones the decision wrote onto the
// session.
func (p *Provider) launcherSubject(subjectTokenType string, session *oauthstore.ExchangeSession, thumbprint string) (subject, host string) {
	_, provider, ok := p.exchange.Registry.TokenType(subjectTokenType)
	if !ok {
		return "", ""
	}
	person, _ := session.Extra[ClaimPerson].(string)
	machine, _ := session.Extra[ClaimMachine].(string)
	trade := identity.Trade{TokenType: subjectTokenType, Subject: session.GetSubjectToken(), Actor: session.GetActorToken()}
	return provider.LauncherSubject(trade, exchange.Claims{Person: person, Actor: machine}, thumbprint)
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
func (p *Provider) audience(tt identity.TokenType, subjectToken map[string]any) error {
	if hasAudience(subjectToken["aud"], tt.Audience) {
		return nil
	}
	return fmt.Errorf("the subject token was not minted for %s, which is the audience this broker accepts from %s", tt.Audience, tt.Issuer)
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
