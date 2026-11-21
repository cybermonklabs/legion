package api

// broker_token.go is the credential that is not a bearer: an access token the secrets broker
// issued, presented under RFC 9449's DPoP scheme. internal/ciphertoken decides it; what is here
// is where that decision is made on a request, how it is carried to every layer that resolves
// the caller, what each refusal says, and what an open connection's heartbeat asks instead.
//
// A DPoP proof is single use by its 'jti' and 'htu' (store.DPoPProofs), which is what the two
// pieces below are for: decideBrokerToken decides a request's presentation once however many
// times its handler resolves the caller, and callerStillResolves asks a connection's broker only
// whether the token is still live, never checking the proof the upgrade already spent.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// decideBrokerToken decides a request's DPoP presentation before its handler runs and carries
// the decision as the caller every layer behind it resolves (resolvedCallerKey), because a DPoP
// proof is single use: a route that resolves its caller more than once (an issue create is
// requireAuthenticated and then requireActor, and a keyed write resolves before its handler)
// would present one proof twice and refuse the second as the replay it would be. The refusal is
// carried too, so a caller is told why it was refused rather than told its sound proof was
// spent.
//
// It answers nothing itself: each route refuses a caller that does not resolve in its own way —
// a JSON error, a closed socket — and reaches this decision through optionalActor as it always
// has. A request presenting no such token passes straight through and is resolved where it
// always was. The one layer that reads the decision without resolving anything is the read-only
// gate inside it (refuseReadOnlyToken), which answers a read-only token on every route the read
// list does not name.
func (s *server) decideBrokerToken(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, presented := ciphertoken.Presented(r); !presented {
			handler(w, r)
			return
		}
		actor, human, err := s.brokerTokenActor(r)
		decided := resolvedCaller{actor: actor, human: human, err: err}
		handler(w, r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, decided)))
	}
}

// callerStillResolves answers whether the caller a live socket was admitted as still resolves,
// which is what whileCallerResolves asks on that socket's every heartbeat, and the caller it
// resolved. For every credential but one that is the request's own resolution, unchanged. The
// document socket asks something narrower, the sign-in cookie alone, which is all it is admitted
// by (signInStillResolves).
//
// The exception is a broker-issued token. The heartbeat's copy of the request still carries the
// proof that opened the connection, and that proof is spent: checking it again would be the
// replay it looks like and would close a sound socket on its first heartbeat. It authenticated
// the upgrade, so what is left to ask is whether the broker still holds the token live, which is
// ciphertoken.Verifier.Live, and, for a token naming a person, whether that person may still act
// and the token's account is still theirs (brokerPersonAdmitted), as every request under the
// token asks: so a socket a replaced account's broker token opened closes at its next heartbeat
// after the replacing sign-in commits. It answers the person the token names as the caller's owner,
// and, as the caller's viewer, only whether that person is a restricted member, which its admission
// read in its one statement (broker true: a broker-issued token names its one project, or none,
// for its whole life, so no grant is compared). The heartbeat holds the connection to that
// (memberKindPerson, personRestricted), with no read of its own, so a socket a demoted member's
// broker token opened closes too. A token naming no person, a read-only one among them, answers
// no caller. Nothing is admitted through this, only kept.
func (s *server) callerStillResolves(r *http.Request) (caller model.Actor, broker bool, err error) {
	if _, presented := ciphertoken.Presented(r); presented {
		if s.deps.BrokerTokens == nil {
			return model.Actor{}, true, errNoBrokerConfigured()
		}
		live, err := s.deps.BrokerTokens.Live(r.Context(), r)
		if err != nil || live.Person == "" {
			return model.Actor{}, true, err
		}
		admission, err := s.brokerPersonAdmitted(r.Context(), live)
		if err != nil {
			return model.Actor{}, true, err
		}
		person := live.Person
		return model.Actor{Owner: &person, Viewer: &model.Viewer{Person: person, Restricted: admission.Restricted}}, true, nil
	}
	caller, _, err = s.optionalActor(r)
	return caller, false, err
}

// brokerPersonAdmitted holds a broker-issued token naming a person (caller.Person) to the rule
// that person's own personal token is held to, since it too carries their sign-in to an agent, and
// to the Google account it is. One statement reads, by the person's key, their member kind, the
// account (`sub`) people records for them, and when people recorded it (sub_recorded_at).
//
// A token is held to the account its credential records (brokerTokenAccount), read in the same
// snapshot, on a deployment whose sign-in records accounts (Deps.SignInIssuer set; elsewhere
// people.sub may be a Google account left from before a switch to a pool, and names nothing):
//
//   - a token recording the account people records for the person now, the holder's own, acts:
//     the broker answers the account the token's own trade recorded on it when it was minted, and
//     reads it from no other row, and no recorded account ever changes, so no token the replaced
//     account traded records the holder's;
//   - a token recording another account is refused from the commit that changed people's: 401
//     BROKER_TOKEN_NOT_ACTIVE when a revocation names that account as the replaced one or as an
//     earlier holder (it lost the address), 401 BROKER_ACCOUNT_NOT_SIGNED_IN when none does
//     (Google gave the address to an account that has not signed in to Dispatch since);
//   - a token recording an account of another issuer (brokerForeignAccount), which Dispatch cannot
//     compare with the one it records, is refused 401 BROKER_TOKEN_RECORDS_NO_ACCOUNT once people
//     records an account for the person, whenever it was made: no credential of the broker's
//     records such an account where Dispatch's sign-in records Google's, and holding it to a time
//     would admit one the replaced account made under that issuer after the margin;
//   - a token recording no account (its credential made before the broker recorded accounts)
//     might be any account's, so it is held to when its credential was made
//     (ciphertoken.Caller.CredentialCreatedAt): made before people recorded the person's account,
//     or less than brokerCredentialMargin after it, or at a time the broker did not record, it is
//     refused 401 BROKER_TOKEN_RECORDS_NO_ACCOUNT; made at least the margin after, or with no
//     recording to compare with (sub_recorded_at null), it acts.
//
// A token an operator's exchange entry mapped (ciphertoken.ProjectScopeNamed: a pod's account, a
// workflow, a Google machine's account, each listed for the person) is held to no account and no
// time: no person's credential made it, the operator's configuration names its person, and a pod
// cannot sign in again, so a time would refuse it for as long as it lives once the person's account
// is recorded, and every trade it makes in the margin after. Such a token records neither an
// account nor its credential's creation, and that is how it is told apart: a restricted member's
// machine login trades for a named token too (exchange.Set.Machine, held to one project), but a
// person's credential made it and its trade recorded that credential, so it is held to its account
// as the person scope's is (personCredential). A named token holds no scope, so it never mints or
// decides.
//
// That leaves no window to wait out while the broker pays a revocation: every credential the
// replaced account holds records it or was made before the recording that replaced it, since
// every credential the broker has made since it recorded accounts records the account that made
// it, and the replacing sign-in sets sub_recorded_at in the transaction that records the
// revocation (store.PgPeopleStore.SignInSubject). The broker revocation still ends those
// credentials at the broker, for the secret reads Dispatch does not see (pass, broker_revocations).
//
// A restricted member is then held to restrictedOwnerAdmitted, the one check both credentials
// share. A read that fails is 503 IDENTITY_UNAVAILABLE, never an accept. So the token of a full
// member, or of a person no people row holds, costs that one read and answers as it always did
// while its account is theirs; only a restricted member's reads the lists. A token naming a
// service and no person is not asked. The admission answers whether the person is restricted,
// which a live socket's heartbeat holds it to. A broker token never mints or decides
// (brokerPersonScopes), so nothing past the admission reads its account.
func (s *server) brokerPersonAdmitted(ctx context.Context, caller ciphertoken.Caller) (brokerAdmission, error) {
	var kind string
	var replaced, named bool
	subject, issuer := s.brokerTokenAccount(caller)
	foreign := s.brokerForeignAccount(caller)
	held := s.deps.SignInIssuer != "" && (caller.ProjectScope != ciphertoken.ProjectScopeNamed || personCredential(caller))
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce((select member_kind from people where email = $1), ''),
			`+brokerAccountReplaced("$1", "$2::text", "$6::text", "$5::boolean", "$3::timestamptz", "$4::boolean")+`,
			`+brokerAccountNamed("$1", "$2::text")+`
	`, model.CanonicalLogin(caller.Person), subject, timeOrNull(caller.CredentialCreatedAt), held, foreign, issuer).
		Scan(&kind, &replaced, &named)
	if err != nil {
		slog.Warn("dispatch: a broker token naming a person was refused: their member kind and recorded account could not be read", "error", err)
		return brokerAdmission{}, errorf(http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE",
			"whether this token's person may still act could not be read; try again in a minute")
	}
	if replaced {
		return brokerAdmission{}, brokerReplacedRefusal(subject == "", named)
	}
	admission := brokerAdmission{Restricted: model.MemberKind(kind) == model.MemberRestricted}
	if !admission.Restricted {
		return admission, nil
	}
	return admission, s.restrictedOwnerAdmitted(ctx, caller.Person)
}

// personCredential reports whether a broker token's trade recorded the person's credential that
// made it: the account it records, or when that credential was made. A sign-in's and a machine
// login's trade record both, or the creation alone for a credential no account approved; an
// operator entry's records neither, since no person's credential made it.
func personCredential(caller ciphertoken.Caller) bool {
	return caller.AccountSubject != "" || !caller.CredentialCreatedAt.IsZero()
}

// brokerCredentialMargin is how long after people recorded an address's Google account a broker
// credential recording no account must have been made to count as made after it
// (brokerAccountReplaced). Two things can put a credential the replaced account made after the
// recording by the clocks, and the margin has to cover both, failing closed:
//
//   - The relay. An approve by the replaced account's credential that passed its last check
//     (approverStanding) just before the replacing sign-in committed is relayed, and the broker
//     makes the machine login in its own transaction later. Dispatch sends the decision within
//     its own time after that check, and its client (cipher.Client's defaultTimeout, 5 s) bounds
//     how long the request takes to reach the broker; the broker's handler then has its request
//     deadline, 45 s (cmd/cipherd requestDeadline), to begin the transaction whose start is the
//     login's created_at, so a late commit does not move it. So 50 s and Dispatch's own time.
//   - The two clocks. The credential's time is the broker database's now() and the recording's
//     is Dispatch's database's clock_timestamp(), so a broker clock ahead of Dispatch's by d moves
//     every credential d later. Both databases run outside the cluster, managed, on Google's
//     infrastructure, whose hosts take their time from Google's NTP (metadata.google.internal):
//     on 2026-11-17 a GCP host of the same project read 2 µs slow of it (chronyc tracking, ±187 µs
//     a sample, 0.65 ms root dispersion), and the production broker's and Dispatch's own hosts read
//     within [-31, +131] ms and [-21, +90] ms of that host by their HTTP Date headers (a bound the
//     round trip limits). The databases' own clocks could not be read from here. So healthy
//     clocks are well under a second apart; what the margin really covers is a clock whose
//     synchronization has failed and drifts.
//
// Five minutes less that 50 s relay leaves under 250 s of skew, less Dispatch's own time between
// its check and its request, before a credential could be admitted wrongly: at a quartz drift of
// 50 ppm, 4.3 s a day (a typical crystal's tolerance, not a measured one), a database clock would
// have to run unsynchronized for about 58 days to use it up, which monitoring of either database
// meets long before. A skew refuses, never admits. The
// cost is one sign-in at the broker again for a credential recording no account made within five
// minutes after its address's account was recorded, which after the recording only an older broker
// makes: every approve then names the account (credentialSubjectSQL).
const brokerCredentialMargin = 5 * time.Minute

// brokerCredentialMarginSQL is brokerCredentialMargin as an SQL interval.
var brokerCredentialMarginSQL = fmt.Sprintf("interval '%d seconds'", int(brokerCredentialMargin/time.Second))

// timeOrNull is t as a statement parameter, nil (SQL null) for the zero time.
func timeOrNull(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// brokerReplacedRefusal is the refusal of a broker token whose account no longer holds its
// person's address (brokerAccountReplaced): a token recording no account Dispatch can name, none
// or another issuer's (BROKER_TOKEN_RECORDS_NO_ACCOUNT), the replaced account's
// (BROKER_TOKEN_NOT_ACTIVE, a revocation names it), or an account's that has not signed in since
// Google gave it the address (BROKER_ACCOUNT_NOT_SIGNED_IN).
func brokerReplacedRefusal(noAccount, named bool) error {
	switch {
	case noAccount:
		return errBrokerTokenRecordsNoAccount()
	case named:
		return errBrokerCredentialRevoked()
	}
	return errBrokerAccountNotSignedIn()
}

// errBrokerTokenRecordsNoAccount is the refusal of a broker token whose credential records no
// Google account Dispatch can name: none, made before people recorded its person's account, less
// than brokerCredentialMargin after or at a time the broker did not record, or an account of
// another issuer, whenever made (brokerAccountReplaced): an account that held the address before
// may have made it. A credential the new holder made the same way needs one sign-in at the broker
// again, whose credential records its account. The words name no person and no account.
func errBrokerTokenRecordsNoAccount() error {
	return errorf(http.StatusUnauthorized, "BROKER_TOKEN_RECORDS_NO_ACCOUNT",
		"this access token's credential records no Google account Dispatch can name, and Dispatch cannot tell it was made after it recorded the account that holds this address, so it acts for nobody; sign in again at the secrets broker, whose new credential records your account")
}

// brokerTokenAccount is the account a broker token's credential records, as an account Dispatch can
// name: the broker's answer's `sub` (subject) when its issuer is one whose accounts people records
// (recordedIssuer: Dispatch's sign-in issuer, or the outside origin for a passkey's subject), with
// that issuer as people.sub_issuer spells it (issuer: "" for the sign-in issuer). Both are "" for a
// token recording none (a sign-in or machine login made before the broker recorded accounts), for
// one whose account is of another issuer (brokerForeignAccount), and for a Dispatch that signs
// nobody in.
func (s *server) brokerTokenAccount(caller ciphertoken.Caller) (subject, issuer string) {
	if caller.AccountSubject == "" || s.deps.SignInIssuer == "" {
		return "", ""
	}
	issuer, known := s.recordedIssuer(caller.AccountIssuer)
	if !known {
		return "", ""
	}
	return caller.AccountSubject, issuer
}

// recordedIssuer is issuer as people.sub_issuer spells it, and whether people records accounts of
// it at all: "" for Dispatch's sign-in issuer, the outside origin itself where passkey sign-in is
// on (Deps.Passkeys), and false for any other, each compared as oidc.CanonicalIssuer spells it.
func (s *server) recordedIssuer(issuer string) (string, bool) {
	switch canonical := oidc.CanonicalIssuer(issuer); {
	case s.deps.SignInIssuer != "" && canonical == s.deps.SignInIssuer:
		return "", true
	case s.deps.Passkeys != nil && canonical == s.deps.Passkeys.Issuer():
		return canonical, true
	}
	return "", false
}

// brokerForeignAccount reports whether a broker token's credential records an account of an issuer
// whose accounts people records none of (recordedIssuer), on a deployment whose sign-in records
// accounts: an account Dispatch cannot compare with the one it records for the person, which
// brokerAccountReplaced refuses once it records one. The broker's own trades record its person
// token's issuer, Google's, and its approvals the issuer Dispatch relays (the sign-in issuer's, or
// the outside origin's for a passkey's account), so none is expected; the arm is what keeps one
// from being held to a time instead.
func (s *server) brokerForeignAccount(caller ciphertoken.Caller) bool {
	if caller.AccountSubject == "" || s.deps.SignInIssuer == "" {
		return false
	}
	_, known := s.recordedIssuer(caller.AccountIssuer)
	return !known
}

// brokerAdmission is a broker token's person admitted (brokerPersonAdmitted): whether the person
// is a restricted member, read in the same statement as the token's account, which a live socket's
// heartbeat compares (personRestricted).
type brokerAdmission struct {
	Restricted bool
}

// brokerAccountReplaced is the predicate that a broker token's account no longer holds the person
// address, over six SQL expressions the statement names: the address, the token's own account
// (brokerTokenAccount: its `sub`, "" for none, and its issuer as people.sub_issuer spells it),
// whether that account is of another issuer (brokerForeignAccount), when its credential was made
// (null for a time the broker did not record) and whether the token is held to an account at all:
// the deployment's sign-in records accounts, and, at an admission, the token is no operator entry's.
// With accounts held and people recording one for the address, it holds for a token recording
// another account (the replaced account's, an account's that has not signed in since Google gave
// it the address, or the same `sub` of another issuer), for one recording an account of another
// issuer, and for one recording none whose credential was made before people recorded the
// address's account or less than brokerCredentialMargin after it, or at an unrecorded time. It is
// false for an address recording no account, and, for a token recording none, with no recording
// time, so nothing is refused that predates the column. An admission reads it
// (brokerPersonAdmitted), the one place it is read: a broker token neither decides nor mints
// (brokerPersonScopes), so no read made again once a request's body has arrived asks it.
func brokerAccountReplaced(address, subject, issuer, foreign, createdAt, held string) string {
	return "(" + held + " and exists (select 1 from people where email = " + address + " and sub is not null and (" +
		foreign + " or (nullif(" + subject + ", '') is not null and (sub <> " + subject + " or coalesce(sub_issuer, '') <> " + issuer + "))" +
		" or (nullif(" + subject + ", '') is null and sub_recorded_at is not null and (" + createdAt + " is null or " +
		createdAt + " < sub_recorded_at + " + brokerCredentialMarginSQL + ")))))"
}

// brokerAccountNamed is the predicate that a broker revocation of the person address names
// subject, a broker token's own account, as the account replaced or as an earlier holder, over
// two SQL expressions the statement names: such an account held the address and lost it, so a
// token of it that people no longer records is the replaced account's (BROKER_TOKEN_NOT_ACTIVE),
// not an account that has not signed in yet. It is false for a token recording none.
func brokerAccountNamed(address, subject string) string {
	return "exists (select 1 from broker_revocations where email = " + address + " and nullif(" + subject + ", '') in (subject, holder_subject))"
}

// errBrokerCredentialRevoked is the refusal of a broker token naming a person whose account a
// revocation names as the replaced one or an earlier holder while people records another
// (brokerReplacedRefusal): the code a token the broker itself no longer holds live is refused with
// (brokerTokenActor), since that is what the token is once the revocation is paid. The words name
// no person.
func errBrokerCredentialRevoked() error {
	return errorf(http.StatusUnauthorized, "BROKER_TOKEN_NOT_ACTIVE",
		"another Google account signed in to this access token's address, so every credential the secrets broker issued for the address is revoked")
}

// errBrokerAccountNotSignedIn is the refusal of a broker token whose credential records a Google
// account other than the one people records for its person when no revocation names that account
// (brokerPersonAdmitted): Google gave the address to an account that has not signed in to Dispatch
// since, so its own tokens act once it does. Its own code, not BROKER_TOKEN_NOT_ACTIVE, which a
// client takes for a stale token and trades again for, which would fail the same way on every
// request. A token whose account a revocation names is the replaced account's, and is
// errBrokerCredentialRevoked. The words name no person and no account.
func errBrokerAccountNotSignedIn() error {
	return errorf(http.StatusUnauthorized, "BROKER_ACCOUNT_NOT_SIGNED_IN",
		"the Google account this access token was issued for has not signed in to Dispatch since it was given this address; sign in to Dispatch with it, then use the token again")
}

// errNoBrokerConfigured is what a DPoP presentation is to a Dispatch with no broker: there is no
// such credential kind, and this is what an Authorization header that is not a bearer has always
// been here (auth.BearerToken reads no token from it).
func errNoBrokerConfigured() *apiError {
	return errorf(http.StatusUnauthorized, "UNAUTHORIZED", "invalid bearer token")
}

var brokerPersonScopes = []string{model.ScopePerson}

// brokerTokenActor authenticates a caller presenting an access token the secrets broker issued,
// under the DPoP scheme: the broker is asked whether the token is live and the caller is held to
// the key that token is bound to (internal/ciphertoken). A verified caller is the session the
// token names, acting for the person it names, or, where the broker names none, for the service
// the machine holding it belongs to. It is never retried as another credential.
//
// A caller that names a service and no person is a machine nobody has signed in on
// (`agent-secrets login`). It reads as any session does, and writeNeedsASignedInPerson refuses
// its every write at the one chokepoint (project_scope.go), so no write is recorded under an
// empty writer. It holds no OAuth scope either, so the rows a person's own credential reaches
// turn it away earlier still (gateRouteScopes): there is no person for it to act as.
//
// A read-only token is one of those too, a service and no person, and the broker held it to less
// still: the scope read alone, the project scope none, no project (ciphertoken.Caller.ReadOnly,
// which only a verifier built with Options.ServesReadOnly answers). Its credential is marked
// read-only (model.Credential.ReadOnly) and holds no OAuth scope, and refuseReadOnlyToken answers
// it 403 READ_ONLY_TOKEN on every route the read list does not name (readScopeRoutes), ahead of
// the scope gate, the sign-in check and everything else the route does. On a listed route it is
// the session it names, reading as that session does.
//
// Each refusal is its own code, and none of them repeats the token the caller presented:
//
//   - 401 BROKER_TOKEN_NOT_ACTIVE — the broker answered that the token is not live (unknown,
//     revoked or expired).
//   - 401 BROKER_TOKEN_PROOF_INVALID — the proof did not meet RFC 9449 Section 4.3: a bad
//     signature, a method or target URI that is not this request's, an 'iat' outside the window,
//     a missing or wrong 'ath', or a proof already spent; or its 'jti' holds a control character,
//     which the replay store records nothing of (store.ErrUnstorableProof) and the library reports
//     as its own failure, so it is answered here as the caller's.
//   - 401 BROKER_TOKEN_KEY_MISMATCH — a sound proof signed by a key that is not the token's
//     'jkt', which is what makes a stolen token useless.
//   - 401 BROKER_TOKEN_NOT_A_SESSION — the broker holds the token live, but it is not a
//     session's credential: it names no session, it names neither a person nor a service, it
//     was issued to a client other than the exchange a session's token comes from, or its
//     project scope and its scope are a shape no broker mints (the scope read beside a project
//     scope that writes, or the project scope none in any shape but a read-only token's). A
//     machine login's own access token is one of these, and Dispatch records every write under
//     the session its credential proved, so it acts on none of them rather than letting a
//     request body name a session for a credential that names none.
//   - 429 BROKER_TOKEN_RATE_LIMITED — this source address has no introspection left in its
//     bucket; nothing was asked of the broker. It carries the wait in Retry-After as well as in
//     its prose, as the broker's own 429s do.
//   - 503 BROKER_UNAVAILABLE — the broker produced no answer, so the token was never judged. It
//     is a 503 for the reason OIDC_ISSUER_UNAVAILABLE is: a caller told its token was refused
//     would discard a token that may be perfectly live, where a 503 asks it to come back.
//
// A verified token naming a person is then held to the rule that person's personal token is
// (brokerPersonAdmitted): a restricted member's acts only while the sign-in rule would sign them
// in now (restrictedOwnerAdmitted), and is otherwise 401 TOKEN_OWNER_NOT_ADMITTED, or 503
// IDENTITY_UNAVAILABLE when that cannot be read; and it is held to the Google account its
// credential records: a token of an account a revocation names while people records another is 401
// BROKER_TOKEN_NOT_ACTIVE, as a token the broker no longer holds live is; a token of another
// account no revocation names is 401 BROKER_ACCOUNT_NOT_SIGNED_IN; and a token recording no
// account Dispatch can name, whose credential was made before people recorded its person's account
// or within brokerCredentialMargin after, is 401 BROKER_TOKEN_RECORDS_NO_ACCOUNT.
func (s *server) brokerTokenActor(r *http.Request) (model.Actor, bool, error) {
	if s.deps.BrokerTokens == nil {
		return model.Actor{}, false, errNoBrokerConfigured()
	}
	caller, err := s.deps.BrokerTokens.Verify(r.Context(), r)
	switch {
	case err == nil:
	case errors.Is(err, ciphertoken.ErrNotActive):
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "BROKER_TOKEN_NOT_ACTIVE",
			"the secrets broker does not hold this access token as live")
	case errors.Is(err, ciphertoken.ErrKeyMismatch):
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "BROKER_TOKEN_KEY_MISMATCH",
			"the DPoP proof's key is not the key this access token is bound to")
	case errors.Is(err, ciphertoken.ErrProofInvalid), errors.Is(err, ciphertoken.ErrNotPresented), errors.Is(err, store.ErrUnstorableProof):
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "BROKER_TOKEN_PROOF_INVALID",
			"the DPoP proof did not verify")
	case errors.Is(err, ciphertoken.ErrNotASession):
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "BROKER_TOKEN_NOT_A_SESSION",
			"this access token is not a session's credential: the secrets broker names no session for it")
	case errors.Is(err, ciphertoken.ErrRateLimited):
		wait := s.deps.BrokerTokens.RetryAfter()
		refusal := errorf(http.StatusTooManyRequests, "BROKER_TOKEN_RATE_LIMITED",
			"too many access tokens checked from this address; try again in %s", wait)
		// The one rounding every bucket of internal/ratelimit's refusals carries, the broker's
		// own 429s included, so a caller reads one wait the same way whichever refused it.
		refusal.retryAfter = ratelimit.RetryAfterSeconds(wait)
		return model.Actor{}, false, refusal
	default:
		// The class only, as serviceTokenActor logs one: this runs before any authorization, so
		// anything the caller chose would be theirs to write into the operator's log.
		slog.Warn("dispatch: the secrets broker could not be reached to check an access token", "error", err)
		return model.Actor{}, false, errorf(http.StatusServiceUnavailable, "BROKER_UNAVAILABLE",
			"the secrets broker cannot be reached; try again in a minute")
	}
	var admission brokerAdmission
	if caller.Person != "" {
		if admission, err = s.brokerPersonAdmitted(r.Context(), caller); err != nil {
			return model.Actor{}, false, err
		}
	}
	// A token the broker scoped to its person holds the person scope: that token is the person's
	// own agent acting for them, so it reads, writes and answers their questions as they do. No
	// broker-issued token holds approve, whatever its project scope (brokerPersonScopes): every
	// one is made from a file on the machine that holds it, a machine login or a person's sign-in,
	// so whoever copies that file holds it, and approve settles what only the person settles - an
	// approval, a credential decision or revoke, a personal token. Those need the person's browser
	// session or a personal token the person gave approve. A token scoped to a project gets no
	// scope, because the deployment mapped that token to a project and not to the person: it
	// proves a session, not that any person stands behind the request. A restricted member's
	// machine login trades for one too: a person's own credential made that one, and it is held to
	// that person's account (brokerPersonAdmitted, personCredential), but it is the outside
	// person's agent held to the one project they work on, which acts in that project and settles
	// nothing of the person's own, so it holds no scope either. A restricted member's token the
	// broker scoped to their person gets none for the same reason: a broker whose project check is
	// off, an older broker during a rollout, or a token minted before the broker held restricted
	// members to one project can still answer one, and brokerPersonAdmitted has read the member's
	// kind above, so the scope does not rest on the broker's check alone. A token that names a
	// service and no person gets none either, whatever scope the broker answered: there is no
	// person for it to act as, and a scope that said otherwise would let it act as the session's
	// own id on the routes that read what belongs to a person. A read-only token is one of those,
	// and its one scope, read, is carried as the credential's ReadOnly mark and never as a scope a
	// route could require.
	var scopes []string
	if caller.Person != "" && caller.ProjectScope == ciphertoken.ProjectScopePerson && !admission.Restricted {
		scopes = brokerPersonScopes
	}
	actor := model.Actor{
		Kind: "session",
		ID:   caller.Session,
		// The broker proved this session, so the credential already names the agent behind the
		// request and no request body has to name it, and the actor is Proven. A read-only token
		// is marked as one, which is all refuseReadOnlyToken reads.
		Credential: &model.Credential{Scopes: scopes, Session: caller.Session, ReadOnly: caller.ReadOnly},
		Proven:     true,
	}
	// A caller names a person or a service, never both and never neither (ciphertoken.Caller),
	// and the two fields are set from the token alone, never from a body. whoami reports both.
	if caller.Person != "" {
		actor.Owner = &caller.Person
	}
	if caller.Service != "" {
		actor.Service = &caller.Service
	}
	// A token's list is its credential's, which the one rule of what a caller sees reads
	// (access.CredentialCovers): for a full member's token it decides only which private projects
	// the token reaches, so while the deployment holds no private project it changes nothing the
	// token did before projects could be private (R9); for a restricted member's token it narrows
	// every project (R3). A token held to one project names that project: one an operator's entry
	// mapped, today's named-scope rule, and a restricted member's machine login's (the broker's
	// exchange.Set.Machine); ciphertoken refuses a named token naming two. A full member's such
	// token has its writes held to that project as they were before, whatever the deployment holds
	// (NamedProject; namedProjectOf), and a restricted member's is held by its list, which covers
	// that project alone. A token acting as its person carries the list its trade asked for
	// (ciphertoken.Caller.Projects), which holds no write by name, since a dev box session's person
	// writes wherever they may. Every other token carries no list: a named token naming none (the
	// shared public-session account's), which reads and writes as a token naming no project does;
	// the token of a machine naming a service and no person, which names no project at all; and a
	// read-only token, whose project scope none names no project and writes none.
	switch {
	case caller.ProjectScope == ciphertoken.ProjectScopeNamed && len(caller.Projects) == 1:
		actor.Credential.Projects = caller.Projects
		actor.Credential.NamedProject = caller.Projects[0]
	case caller.ProjectScope == ciphertoken.ProjectScopePerson && len(caller.Projects) > 0:
		actor.Credential.Projects = caller.Projects
	}
	// The writer the line names: the person, or the service for a token that names none, under
	// the `service:` prefix requestKeyLogin spells a service with, so one field never reads a
	// fleet's name as a person's login.
	writer := caller.Person
	if writer == "" {
		writer = "service:" + caller.Service
	}
	accesslog.Note(r.Context(), accesslog.KindBrokerToken, writer)
	return actor, false, nil
}
