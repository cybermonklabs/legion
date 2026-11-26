package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/hold"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// Introspect is what the broker knows about token, for the introspection route to answer with.
//
// It is built on the library's IntrospectToken rather than NewIntrospectionRequest: that one
// authenticates its caller as an OAuth client, and this route's caller is Dispatch, authenticated
// by its own Kubernetes service-account token before the provider is reached.
//
// A token the broker does not know, one it has revoked and one that has expired are all answered
// as simply not live, with no error and nothing else disclosed, which is what RFC 7662 Section
// 2.2 asks for. So is any token that is not an access token, a machine login's refresh token
// among them: the answer carries no token type, so a refresh token answered live — it outlives
// the access token issued beside it — would pass for a credential no request can present. The
// library takes oauth2.AccessToken only as a hint and falls through to the refresh-token store
// when the access lookup misses, so the kind it reports finding is checked here rather than
// trusted to the hint. An error is returned only where the broker itself failed — a storage
// outage, or a row it could not decode — because answering that as "not live" would both deny a
// sound token and hide the outage; notLive is how the two are told apart, and brokerFailure makes
// that error say which of the two it was.
//
// A token whose HMAC is not the broker's is answered not live before anything is read: the
// provider's own HMAC strategy (codeStrategy, built in New) checks it, as ValidateAccessToken ends
// by doing (authelia strategy_hmac.go:125-141), with no prefix to trim since the broker's tokens
// carry none (compose.NewOAuth2HMACStrategy). The auth service introspects every session token a
// NATS connection presents, so a forged one must cost the broker no database query.
//
// A live token naming a person also answers the Google account the credential behind it records,
// which the token's trade wrote onto the token's own session when it minted it
// (ClaimAccountIssuer, ClaimAccountSubject), so the whole answer is the one row the library read.
// No other row is read for it: a sign-in's or a machine login's row could change, or be ended and
// replaced, while the token lives, and a second read could pair this token with another account.
// A token that records none, one minted before the broker recorded the account on the token among
// them, answers none, which a resource server reads as "the broker cannot say which account this
// is".
//
// A live token naming a person, as its person or, for a machine login's own token, as the login's
// operator, is answered held when the hold gate (ExchangeConfig.Hold) says Dispatch holds that
// person: still active, since a hold revokes nothing, and refused by the resource server after its
// own checks. A person the gate serves until a deadline is answered with that deadline
// (not_held_until, Unix seconds rounded down), past which the resource server keeps no such answer,
// and one whose deadline has passed is answered with a not_held_until every clock is past. Any other
// case the gate cannot tell is *HoldUncheckableError, never an answer.
//
// A token narrowed to reading is answered with its narrowed project scope, its scope (read, or
// read person) and its cut (AsOf), and its end is decided again at every introspection: the store
// reads it live only while its own row, the launcher credential it names and the machine login's
// token it was traded from are (oauthstore.Store.GetAccessTokenSession), so a revoke or a retire
// any broker build made ends it, and the library then reports it inactive.
func (p *Provider) Introspect(ctx context.Context, token string) (oauthwire.Introspection, error) {
	if p.strategy.Enigma.Validate(ctx, token) != nil {
		return oauthwire.Introspection{}, nil
	}
	kind, request, err := p.oauth.IntrospectToken(ctx, token, oauth2.AccessToken, oauthstore.NewExchangeSession())
	if err != nil {
		if notLive(err) {
			return oauthwire.Introspection{}, nil
		}
		return oauthwire.Introspection{}, brokerFailure(err)
	}
	if kind != oauth2.AccessToken {
		return oauthwire.Introspection{}, nil
	}
	answer := oauthwire.Introspection{Active: true, Scope: strings.Join(request.GetGrantedScopes(), " ")}
	if client := request.GetClient(); client != nil {
		answer.ClientID = client.GetID()
	}
	session, ok := request.GetSession().(*oauthstore.ExchangeSession)
	if !ok || session.DefaultSession == nil {
		return answer, nil
	}
	answer.Thumbprint = session.GetDPoPJWKThumbprint()
	if expires := session.GetExpiresAt(oauth2.AccessToken); !expires.IsZero() {
		answer.ExpiresAt = expires.Unix()
	}
	answer.Session, _ = session.Extra[ClaimSession].(string)
	answer.Person, _ = session.Extra[ClaimPerson].(string)
	answer.Service, _ = session.Extra[ClaimService].(string)
	answer.Machine, _ = session.Extra[ClaimMachine].(string)
	answer.ProjectScope, _ = session.Extra[ClaimProjectScope].(string)
	if answer.Person != "" {
		issuer, _ := session.Extra[ClaimAccountIssuer].(string)
		subject, _ := session.Extra[ClaimAccountSubject].(string)
		if issuer != "" && subject != "" {
			answer.AccountIssuer, answer.AccountSubject = issuer, subject
		}
		answer.CredentialCreatedAt = unixSeconds(session.Extra[ClaimCredentialCreatedAt])
	}
	answer.AsOf, _ = session.Extra[ClaimAsOf].(string)
	stored, present := session.Extra[ClaimProjects]
	if !present {
		if oauthwire.BaseProjectScope(answer.ProjectScope) == oauthwire.ProjectScopeNamed {
			// A named token an exchange decided before the list stored its one project under
			// another claim and none under this one. Read as no projects, it would pass as a named
			// token naming none, the public-session account's shape, which holds it to no project;
			// it is answered not live instead, so its holder trades again for a token of this
			// release. A narrowed named token is read as its base word, so one stored with no list
			// is not live either. A person token of that release stored no project claim at all,
			// which meant then what an empty list means now, so it reads as naming none; a machine
			// login's own token, which no exchange decided, carries neither claim.
			return oauthwire.Introspection{}, nil
		}
		return p.withHold(ctx, answer, session)
	}
	// The stored claim is the list's space-joined spelling (ClaimProjects), which the store
	// hands back as the string it wrote; one that is not a string or does not parse is a row this
	// broker did not write: ErrStoredClaimMalformed, naming the row (malformedClaimOwner) so an
	// operator reads which row to look at rather than an outage, and never a token naming no
	// project.
	text, ok := stored.(string)
	if !ok {
		return oauthwire.Introspection{}, fmt.Errorf("%w: %s stores its projects as a %T, not their string", ErrStoredClaimMalformed, malformedClaimOwner(session, answer.Session), stored)
	}
	projects, err := oauthwire.ParseProjects(text)
	if err != nil {
		return oauthwire.Introspection{}, fmt.Errorf("%w: %s stores projects that do not parse: %w", ErrStoredClaimMalformed, malformedClaimOwner(session, answer.Session), err)
	}
	answer.Projects = projects
	return p.withHold(ctx, answer, session)
}

// withHold is answer with Held set when Dispatch holds the person it names, its person or, for a
// machine login's own token, which names none, the login's operator (session's extraOperator), and
// with NotHeldUntil set to the deadline the gate serves a person it does not hold until, in Unix
// seconds rounded down so no reader keeps the answer later than the gate serves it. A person whose
// not-held answer has passed its deadline is answered active and not held, with NotHeldUntil
// oauthwire.NotHeldUntilPassed, so every reader refuses the token as a deadline passed, as it
// refuses one whose kept answer lapsed: the Envoy listener 503 BROKER_UNAVAILABLE with
// Retry-After: 60, and the NATS auth service under deadline_passed (Dispatch reads neither field
// yet). Every other case the gate
// cannot tell (hold.ErrUncheckable, tested after a passed deadline and before a hold, so a held
// answer it could not keep is no answer either) is *HoldUncheckableError.
func (p *Provider) withHold(ctx context.Context, answer oauthwire.Introspection, session *oauthstore.ExchangeSession) (oauthwire.Introspection, error) {
	person := answer.Person
	if person == "" {
		person, _ = session.Extra[extraOperator].(string)
	}
	if person == "" {
		return answer, nil
	}
	until, err := p.exchange.Hold.CheckUntil(ctx, person)
	switch {
	case err == nil:
		if !until.IsZero() {
			answer.NotHeldUntil = until.Unix()
		}
	case errors.Is(err, hold.ErrDeadlinePassed) && !errors.Is(err, hold.ErrHeld):
		answer.NotHeldUntil = oauthwire.NotHeldUntilPassed
	case errors.Is(err, hold.ErrUncheckable), !errors.Is(err, hold.ErrHeld):
		return oauthwire.Introspection{}, &HoldUncheckableError{Person: person, Err: err}
	default:
		answer.Held = true
	}
	return answer, nil
}

// HoldUncheckableError is Introspect's failure when the hold gate cannot tell whether Dispatch
// holds the person a live token names. Its text is the one the introspection route answers 503
// HOLD_UNCHECKABLE with, naming the person and why in hold.Reason's fixed words; Err is the gate's
// own error, for the log alone, which it wraps.
type HoldUncheckableError struct {
	Person string
	Err    error
}

func (e *HoldUncheckableError) Error() string {
	return "whether Dispatch holds " + e.Person + " cannot be read: " + hold.Reason(e.Err)
}

func (e *HoldUncheckableError) Unwrap() error { return e.Err }

// unixSeconds reads a Unix time a session's extras hold, as the exchange wrote it (an int64) or as
// it reads back out of the stored request's JSON (a float64), and 0 for anything else.
func unixSeconds(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// malformedClaimOwner names the row of a malformed stored claim: the token's session and the
// launcher credential it follows, if any.
func malformedClaimOwner(session *oauthstore.ExchangeSession, name string) string {
	owner := fmt.Sprintf("the token of the session %q", name)
	if credential, _ := session.Extra[oauthwire.ExtraCredentialID].(string); credential != "" {
		owner += fmt.Sprintf(" under the launcher credential %s", credential)
	}
	return owner
}

// ErrStoredClaimMalformed is a token row whose stored project claim this broker could not have
// written: not its string, or a string that is no list. It is no outage, so the introspection route
// answers it as what it is rather than as a database it could not reach.
var ErrStoredClaimMalformed = errors.New("oauth: the token's stored project claim is malformed")

// notLiveCauses are the library's own sentinels for a token it found wanting: a string with no
// signature to look up (oauth2.ErrNotFound, which the library wraps itself), a row the store
// answers revoked (oauth2.ErrInactiveToken), a token past its expiry, and one whose bytes do not
// check against the secret. errors.Is matches an *oauth2.RFC6749Error on its error code and
// status, and each of these has its own. A string with a half that is not base64url has no
// sentinel: notLive tells it apart by its decode error.
var notLiveCauses = []error{
	oauth2.ErrNotFound, oauth2.ErrInactiveToken, oauth2.ErrTokenExpired,
	oauth2.ErrInvalidTokenFormat, oauth2.ErrTokenSignatureMismatch,
}

// notLive reports whether err, an IntrospectToken failure, is the library deciding the token is
// not live, rather than the broker failing to read it.
//
// The library's status cannot tell them apart: it wraps every failed read of a token's row, a lost
// connection or a row that does not decode alike, in oauth2.ErrRequestUnauthorized, a 401, the
// same error it answers a revoked token with. So the failure is told apart by its cause. It is not
// live when the chain holds one of notLiveCauses, or when it is the library's own
// ErrRequestUnauthorized wrapping nothing at all: the store answers a row it does not have
// oauth2.ErrNotFound and nothing else does (oauthstore's hydrate keeps the sentinel out of a row
// it cannot decode), the library takes that as "not mine" from every introspector, and
// IntrospectToken then answers that bare error.
//
// Nor is it live when the chain holds a base64.CorruptInputError and no ErrRequestUnauthorized:
// that is a presented string with a half that is not base64url, a live token with a character
// added in front among them, which is a token the broker never minted. Its signature half can be
// a live token's own, so the store finds that token's row, and the library's HMAC check of the
// string then fails with no sentinel. In authelia.com/provider/oauth2@v0.3.3, validate decodes
// each half (token/hmac/hmacsha.go:165-173) and Validate returns the decode error as it is
// (:135); ValidateAccessToken (handler/oauth2/strategy_hmac.go:140, ValidateRefreshToken at
// :243-250) and the introspector (handler/oauth2/introspector.go:168-169) pass it up unwrapped,
// and IntrospectToken (introspect.go:65) wraps it as the library's unknown error
// (errors.go:389-395), a 500 that is no ErrRequestUnauthorized. That is the token being
// malformed, not the broker failing. A row fails with the same decode error: oauthstore's
// hydrate (store.go:350) reads the session's public_key_jwk, a []byte, with json.Unmarshal,
// which decodes it as base64, so a row holding one that does not decode fails with it. The
// library wraps that failed read, as it does every failed read of a row, in
// ErrRequestUnauthorized (handler/oauth2/introspector.go:165, 204), and that is what keeps an
// undecodable row the broker's failure.
//
// Anything else wraps an error of the store's own — a read that failed, or a row it could not
// decode — and is the broker's failure, never a confident "not live": a cause this does not
// recognise is answered as an outage rather than as a negative.
func notLive(err error) bool {
	for _, cause := range notLiveCauses {
		if errors.Is(err, cause) {
			return true
		}
	}
	if errors.Is(err, oauth2.ErrRequestUnauthorized) {
		return errors.Unwrap(oauth2.ErrorToRFC6749Error(err)) == nil
	}
	var corrupt base64.CorruptInputError
	return errors.As(err, &corrupt)
}

// brokerFailure is err, an IntrospectToken failure notLive does not recognise, as an error that
// says what failed, for the route to log beside its 503.
//
// The library's wrapper cannot say it: its text is only its error code, "request_unauthorized"
// whatever it wraps, so returned as it is, an outage and a row the store could not decode would
// both reach the log as an authorization failure naming neither. What failed is the store's own
// error beneath it — pgx's, or the row's decode error naming the client it found — and that is
// returned in its place. It names the broker's own state, never the token: the store is handed
// only the token's signature and puts that in no error it returns. A failure that wraps nothing
// is named by the library's code and description, which carry no token either.
func brokerFailure(err error) error {
	rfc := oauth2.ErrorToRFC6749Error(err)
	if cause := errors.Unwrap(rfc); cause != nil {
		return cause
	}
	return fmt.Errorf("the provider failed the introspection with %s: %s", rfc.ErrorField, rfc.GetDescription())
}
