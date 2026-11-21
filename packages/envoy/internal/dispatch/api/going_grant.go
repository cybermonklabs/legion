package api

// going_grant.go is the credential a command-bar run acts with (R12). The request that starts a
// run, or approves its plan, records the run's grant (recordGoingGrant): whom that request's
// credential names and by what, with no secret. Each call of the run is made as the caller the
// grant resolves to at that call (goingGrantCaller), checked against the store as it is then, as
// the person's own request would be checked, so a sign-out, an expired sign-in, a revoked token or
// a lost membership refuses the run's next call.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The kinds of grant, as going_run_grants.kind holds them.
const (
	// goingGrantSignIn is a sign-in cookie (identity.CookieIdentity).
	goingGrantSignIn = "sign_in"
	// goingGrantIAP is a sign-in through Identity-Aware Proxy (identity.IAPIdentity).
	goingGrantIAP = "iap"
	// goingGrantToken is a personal agent token.
	goingGrantToken = "token"
	// goingGrantHeader is the header identity of a test server or a local deployment
	// (identity.HeaderIdentity).
	goingGrantHeader = "header"
)

// codeRunNeedsPersonCredential refuses the start or the approval of a run under a credential that
// is no person's own: a broker-issued token, whose DPoP proof serves one request alone, the
// deployment's shared token and a pod's service-account token, which name no person. It is a 403,
// as a read-only token's refusal and a missing scope are: the credential resolved, and it cannot
// do this.
const codeRunNeedsPersonCredential = "RUN_NEEDS_PERSON_CREDENTIAL"

// Why a run's grant no longer names its person (goingGrantEnded.Reason): the reason its run stops
// with.
const (
	// goingGrantSessionEnded is a sign-in whose person signed out, which moved the generation of
	// their sign-ins, or one the deployment can no longer check.
	goingGrantSessionEnded = "SESSION_ENDED"
	// goingGrantSignInExpired is a sign-in past its cookie's expiry, or a sign-in through IAP
	// past the end of its run's working budget.
	goingGrantSignInExpired = "SIGN_IN_EXPIRED"
	// goingGrantTokenRevoked is a personal token revoked, or no longer its person's.
	goingGrantTokenRevoked = "TOKEN_REVOKED"
	// goingGrantMembershipEnded is a person the deployment's membership check no longer admits.
	goingGrantMembershipEnded = "MEMBERSHIP_ENDED"
)

// goingGrantEnded refuses a call of a run whose grant no longer names its person, and Reason says
// why: goingGrantSessionEnded, goingGrantSignInExpired, goingGrantTokenRevoked or
// goingGrantMembershipEnded.
type goingGrantEnded struct {
	Reason string
}

func (e goingGrantEnded) Error() string {
	switch e.Reason {
	case goingGrantSessionEnded:
		return "SESSION_ENDED: the person this run acts for signed out"
	case goingGrantSignInExpired:
		return "SIGN_IN_EXPIRED: the sign-in this run acts with has expired"
	case goingGrantTokenRevoked:
		return "TOKEN_REVOKED: the agent token this run acts with was revoked"
	case goingGrantMembershipEnded:
		return "MEMBERSHIP_ENDED: the person this run acts for may no longer use Dispatch"
	}
	return e.Reason + ": this run's grant no longer names its person"
}

// goingGrant is one run's grant as its row of going_run_grants holds it: its kind, the person's
// canonical login, for a sign-in the generation of the person's sign-ins and the cookie's expiry,
// and for a personal token the token's agent_tokens row. It holds no cookie, assertion, token or
// hash of one.
type goingGrant struct {
	kind       string
	login      string
	generation *int64
	expires    *time.Time
	token      *string
}

// recordGoingGrant writes grant as run's grant in tx, the transaction of the request that starts
// the run or approves its plan, in place of any grant the run held, so an approval moves the run
// to the credential that approved it. The request resolves grant from its own credential
// (goingGrantOf) before it opens tx, since resolving a caller reads the store, which a request
// holding a transaction may not (store.ErrNestedAcquire).
func recordGoingGrant(ctx context.Context, tx pgx.Tx, run string, grant goingGrant) error {
	if _, err := tx.Exec(ctx, `
		insert into going_run_grants (run_id, kind, login, generation, expires_at, token_id)
		values ($1::uuid, $2, $3, $4, $5, $6::uuid)
		on conflict (run_id) do update set
		  kind = excluded.kind, login = excluded.login, generation = excluded.generation,
		  expires_at = excluded.expires_at, token_id = excluded.token_id, granted_at = now()
	`, run, grant.kind, grant.login, grant.generation, grant.expires, grant.token); err != nil {
		return fmt.Errorf("record the run's grant: %w", err)
	}
	return nil
}

// goingGrantOf is the grant of r's credential, for the request that starts a run or approves its
// plan: a person's sign-in, by the identity the deployment names its people with - its session
// cookie, IAP's assertion, or the header of a test server or a local deployment - or a personal
// token, by its row. Every other credential is refused 403 RUN_NEEDS_PERSON_CREDENTIAL, and so is
// a request the command bar sent in-process, since a run's own calls start and approve nothing. A
// caller that does not resolve is refused as optionalActor refuses it.
func (s *server) goingGrantOf(r *http.Request) (goingGrant, error) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		return goingGrant{}, err
	}
	if goingServedInProcess(r.Context()) {
		return goingGrant{}, errRunNeedsPersonCredential()
	}
	if !human {
		if actor.Credential == nil || actor.Credential.TokenID == "" || actor.Owner == nil {
			return goingGrant{}, errRunNeedsPersonCredential()
		}
		token := actor.Credential.TokenID
		return goingGrant{kind: goingGrantToken, login: model.CanonicalLogin(*actor.Owner), token: &token}, nil
	}
	login := model.CanonicalLogin(actor.ID)
	switch people := s.deps.Identity.(type) {
	case identity.CookieIdentity:
		session, signed := auth.SessionFromRequest(r, people.SigningKey)
		if !signed || model.CanonicalLogin(session.Login) != login {
			return goingGrant{}, errRunNeedsPersonCredential()
		}
		return goingGrant{kind: goingGrantSignIn, login: login, generation: &session.Generation, expires: &session.Expires}, nil
	case identity.IAPIdentity:
		return goingGrant{kind: goingGrantIAP, login: login}, nil
	case identity.HeaderIdentity:
		return goingGrant{kind: goingGrantHeader, login: login}, nil
	}
	return goingGrant{}, errRunNeedsPersonCredential()
}

// errRunNeedsPersonCredential is the refusal of a run's start or approval under a credential that
// is no person's own.
func errRunNeedsPersonCredential() *apiError {
	return errorf(http.StatusForbidden, codeRunNeedsPersonCredential,
		"a run acts with a person's own sign-in or personal agent token, and this request's credential is neither: "+
			"a broker-issued token, the deployment's shared token and a service-account token start and approve no run")
}

// goingGrantCaller is the caller a call of run is made as, resolved from the run's grant and from
// the store as both are at this call, with the checks the person's own request would get: for a
// sign-in, the generation of the person's sign-ins is still the grant's (a sign-out moves it), the
// cookie's expiry is ahead, and the deployment's membership check passes; for a sign-in through
// IAP, whose assertion is signed for each request and kept by nobody, the run's working budget,
// which ends at budgetEnds, has not ended, and the membership check passes; for a personal token,
// its row is unrevoked and still the person's, and the caller holds the scopes the row holds now;
// for the header identity, the login.
//
// A grant that no longer names its person is refused with a goingGrantEnded naming why. A
// membership check the sign-in issuer could not answer is refused with an error matching
// identity.ErrSignInUnavailable, which ends nothing: the person is still signed in, and the call
// can wait and be made again. A scope the credential lacks is no refusal here: the route that
// needs it refuses it, as it would the person's own request.
//
// The caller holds the credential the route's scope gate reads, and an act naming that
// credential's scopes beside the command bar and run (goingBarCaller).
func (s *server) goingGrantCaller(ctx context.Context, run string, budgetEnds time.Time) (resolvedCaller, error) {
	var grant goingGrant
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select kind, login, generation, expires_at, token_id::text
		from going_run_grants
		where run_id = $1::uuid
	`, run).Scan(&grant.kind, &grant.login, &grant.generation, &grant.expires, &grant.token); err != nil {
		return resolvedCaller{}, fmt.Errorf("read the run's grant: %w", err)
	}
	switch grant.kind {
	case goingGrantSignIn:
		if err := s.goingSignInStands(ctx, grant); err != nil {
			return resolvedCaller{}, err
		}
	case goingGrantIAP:
		if !time.Now().Before(budgetEnds) {
			return resolvedCaller{}, goingGrantEnded{Reason: goingGrantSignInExpired}
		}
		if err := s.goingMembershipStands(ctx, grant.login); err != nil {
			return resolvedCaller{}, err
		}
	case goingGrantToken:
		return s.goingTokenCaller(ctx, grant, run)
	case goingGrantHeader:
	default:
		return resolvedCaller{}, fmt.Errorf("the run's grant is of kind %q, which this server does not read", grant.kind)
	}
	// A person's own sign-in holds every scope, as optionalActor resolves a browser's.
	person := model.Actor{Kind: "user", ID: grant.login, Credential: personCredential, Proven: true}
	return goingBarCaller(person, true, run), nil
}

// goingSignInStands checks a sign-in grant as CookieIdentity checks the cookie it was made from:
// the generation of the person's sign-ins is still the grant's, the cookie's expiry is ahead, and
// the membership check passes (goingMembershipStands). A deployment that no longer names its
// people by a session cookie checks no cookie, so the sign-in has ended there.
func (s *server) goingSignInStands(ctx context.Context, grant goingGrant) error {
	people, cookies := s.deps.Identity.(identity.CookieIdentity)
	if !cookies || people.Sessions == nil || grant.generation == nil || grant.expires == nil {
		return goingGrantEnded{Reason: goingGrantSessionEnded}
	}
	generation, found, err := people.Sessions.CurrentSessionGeneration(ctx, grant.login)
	if err != nil {
		return fmt.Errorf("read the generation of the person's sign-ins: %w", err)
	}
	if !found || generation != *grant.generation {
		return goingGrantEnded{Reason: goingGrantSessionEnded}
	}
	if !time.Now().Before(*grant.expires) {
		return goingGrantEnded{Reason: goingGrantSignInExpired}
	}
	return s.goingMembershipStands(ctx, grant.login)
}

// goingMembershipStands checks that login may still use Dispatch, with the check the deployment's
// cookie identity makes of each request (identity.Membership.Confirm): a membership that ended is
// goingGrantMembershipEnded, and one the sign-in issuer could not confirm now is Confirm's own
// error, matching identity.ErrSignInUnavailable. A deployment that keeps no membership - the local
// dev sign-in, the header identity, and IAP, which decides each request from its own assertion -
// has nothing to check.
func (s *server) goingMembershipStands(ctx context.Context, login string) error {
	people, cookies := s.deps.Identity.(identity.CookieIdentity)
	if !cookies || people.Membership == nil {
		return nil
	}
	err := people.Membership.Confirm(ctx, login)
	if errors.Is(err, identity.ErrNoIdentity) {
		return goingGrantEnded{Reason: goingGrantMembershipEnded}
	}
	return err
}

// goingTokenCaller is the caller of a token grant, while its token's row is unrevoked and still
// the grant's person's: the person, as a sign-in's call is, with the token's credential - the
// scopes its row holds now, which the route's scope gate reads - and an act naming the token. So
// each change of the run is the person's own, as every route records a person, and its history
// names the token and the command bar; a route the token's scopes do not open refuses the call as
// it refuses the token's own request.
func (s *server) goingTokenCaller(ctx context.Context, grant goingGrant, run string) (resolvedCaller, error) {
	if grant.token == nil {
		return resolvedCaller{}, goingGrantEnded{Reason: goingGrantTokenRevoked}
	}
	var owner, name string
	var scopes []string
	var revokedAt *time.Time
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select owner, name, scopes, revoked_at
		from agent_tokens
		where id = $1::uuid
	`, *grant.token).Scan(&owner, &name, &scopes, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// The grant goes with its token's row, so this is a row deleted since the grant was read.
		return resolvedCaller{}, goingGrantEnded{Reason: goingGrantTokenRevoked}
	}
	if err != nil {
		return resolvedCaller{}, fmt.Errorf("read the run's agent token: %w", err)
	}
	if revokedAt != nil || model.CanonicalLogin(owner) != grant.login {
		return resolvedCaller{}, goingGrantEnded{Reason: goingGrantTokenRevoked}
	}
	person := model.Actor{
		Kind:       "user",
		ID:         grant.login,
		Credential: &model.Credential{Scopes: scopes, TokenID: *grant.token, TokenName: name},
		Proven:     true,
	}
	return goingBarCaller(person, true, run), nil
}

// goingBarCaller is actor, a caller resolved as optionalActor resolves one (human for a person's
// own sign-in, and for a run's call under a personal token, which acts as the person), as a call
// of the command bar's run is made as it: the same credential, with the bar's act beside it
// (goingBarAct).
func goingBarCaller(actor model.Actor, human bool, run string) resolvedCaller {
	actor.Act = goingBarAct(actor.Credential, run)
	return resolvedCaller{actor: actor, human: human}
}

// goingBarAct is the act a call of run makes under credential: the command bar and the run, beside
// the credential's scopes - every scope for a person's own sign-in, a personal token's own - and
// the token it is. So a change shows in its item's history as the person's, through the command
// bar, and an approval answered through the bar carries to the next version as the person's own
// does (model.ActorAct.Holds).
func goingBarAct(credential *model.Credential, run string) *model.ActorAct {
	act := &model.ActorAct{Agent: model.ActAgentCommandBar, Run: run}
	if credential != nil {
		act.TokenID, act.TokenName, act.Scopes = credential.TokenID, credential.TokenName, credential.Scopes
	}
	return act
}
