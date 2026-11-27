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
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/access"
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
// For a presentation it admits as an approve-only token (ciphertoken.Caller.ApproveOnly) that
// holds the approve scope, it carries one thing more (brokerTokenActorBinding): the person that
// token names and the account its credential records (approveOnlyBinding), which a credential
// decision binds itself to where the account binding holds (approverStanding). The decision then
// reads the account the broker answered for the very proof this request spent, and asks no broker
// again.
//
// It answers nothing itself: each route refuses a caller that does not resolve in its own way —
// a JSON error, a closed socket — and reaches this decision through optionalActor as it always
// has. A request presenting no such token passes straight through and is resolved where it
// always was. The one layer that reads the decision before the route resolves its caller is the
// read-only gate inside it (refuseReadOnlyToken), which verifies no broker-issued token itself:
// it answers a read-only token on every route outside the read class, and one that carries a cut
// on every read but whoami. A personal token is the gate's own to resolve, and the gate carries
// that result, its refusal included, as this carries its decision.
func (s *server) decideBrokerToken(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, presented := ciphertoken.Presented(r); !presented {
			handler(w, r)
			return
		}
		var bound *approveOnlyBinding
		actor, human, err := s.brokerTokenActorBinding(r, &bound)
		decided := resolvedCaller{actor: actor, human: human, err: err}
		ctx := r.Context()
		if bound != nil {
			ctx = context.WithValue(ctx, approveOnlyBindingKey{}, *bound)
		}
		handler(w, r.WithContext(context.WithValue(ctx, resolvedCallerKey{}, decided)))
	}
}

// approveOnlyBindingKey carries a request's approveOnlyBinding from the one resolution of its DPoP
// presentation (decideBrokerToken) to a credential decision's re-read of its caller
// (approverStanding). It is a key of its own because the layers between them carry the resolved
// caller again from its actor alone (gateRouteScopes, refuseChangedLogin, keyedWrites), and none
// of them writes this key, while every one of them derives its context from the request's.
type approveOnlyBindingKey struct{}

// approveOnlyBinding is what a verified approve-only token binds its decision to: the canonical
// login of the person it names and the account its credential records, as people records accounts
// (brokerTokenAccount: a subject "" for none Dispatch can name, which no decision accepts where the
// account binding holds, and its issuer as people.sub_issuer spells it).
type approveOnlyBinding struct {
	person  string
	account credentialAccount
}

// approveOnlyBindingOf is the binding decideBrokerToken carried for r when actor, as actingPerson
// resolved it, is the person that binding names and r's resolved credential is marked approve-only
// (approveOnlyRequest), and nil otherwise: for every other credential, and for a broker token
// decideBrokerToken carried no binding for, which decides nothing (refuseUnboundCredential).
func approveOnlyBindingOf(r *http.Request, actor model.Actor) *approveOnlyBinding {
	bound, carried := r.Context().Value(approveOnlyBindingKey{}).(approveOnlyBinding)
	if !carried || !approveOnlyRequest(r) || bound.person != actor.ID {
		return nil
	}
	return &bound
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
// after the replacing sign-in commits. Then, as brokerTokenActor, a held introspection answer
// (ciphertoken.Caller.Held) is 401 MEMBERSHIP_HELD (brokerTokenHeld), so a socket closes at its
// next heartbeat after the broker answers its person held, and a not-held answer past its deadline
// (ciphertoken.Verifier.Lapsed) is 503 BROKER_UNAVAILABLE, so a socket closes at its next heartbeat
// after that deadline. It answers the person the token names as the caller's owner,
// and, as the caller's viewer, only whether that person is a restricted member, which its admission
// read in its one statement (broker true: a broker-issued token names its one project, or none,
// for its whole life, so no grant is compared). The heartbeat holds the connection to that
// (memberKindPerson, personRestricted), with no read of its own, so a socket a demoted member's
// broker token opened closes too. A token naming no person, the read entry's read-only one and a
// job's among them, answers no caller. Nothing is admitted through this, only kept.
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
		switch {
		case live.Held:
			return model.Actor{}, true, s.brokerTokenHeld(live.Person)
		case s.deps.BrokerTokens.Lapsed(live):
			return model.Actor{}, true, errBrokerTokenLapsed()
		}
		if err := restrictedBrokerTokenList(admission, live); err != nil {
			return model.Actor{}, true, err
		}
		person := live.Person
		return model.Actor{Owner: &person, Viewer: &model.Viewer{Person: person, Restricted: admission.Restricted}}, true, nil
	}
	caller, _, err = s.optionalActor(r)
	return caller, false, err
}

// restrictedBrokerTokenList is the refusal of a broker token naming a restricted member that lists
// other than one project, admission being what brokerPersonAdmitted read of its person, or nil
// (R16: an outside person's agent's token lists exactly one project). The broker mints a restricted
// member's machine login the project scope named with the one project its trade names, and refuses
// a trade naming none or several, but a broker whose project check is off, an older broker during a
// rollout, or a token minted before that rule can still answer one, so Dispatch holds it here, as it
// holds their personal token (personalTokenActor), with the same 403 RESTRICTED_MEMBER
// (errRestrictedTokenList). Its every request and its socket's every heartbeat ask it
// (brokerTokenActor, callerStillResolves).
func restrictedBrokerTokenList(admission brokerAdmission, caller ciphertoken.Caller) error {
	if !admission.Restricted || len(caller.Projects) == 1 {
		return nil
	}
	return errRestrictedTokenList(len(caller.Projects),
		"its machine trades its login for a token naming the one project its agent works in")
}

// brokerTokenHeld is the refusal of a broker token whose introspection answered its person held
// (ciphertoken.Caller.Held), 401 MEMBERSHIP_HELD in words that say what ends the hold. Under IAP
// identity (Standing.ChecksDirectory) a sign-in releases nothing and the Workspace Directory's next
// active answer does, so the token gets the words a personal token gets there, from the one
// constant both read (membershipHeldDirectoryText). Under every other rule it gets the broker's own
// words, naming the person, since the answer carries no rule of its own.
func (s *server) brokerTokenHeld(person string) error {
	if s.deps.Standing != nil && s.deps.Standing.ChecksDirectory() {
		return errMembershipHeld(membershipHeldDirectoryText)
	}
	return errMembershipHeld(fmt.Sprintf("%[1]s's credentials are held by Dispatch; they work again when %[1]s signs in to Dispatch, or when Dispatch can confirm %[1]s with Google again", person))
}

// errBrokerTokenLapsed is the refusal of a broker token whose introspection answered its person
// not held only until a deadline that has passed (ciphertoken.Verifier.Lapsed): an IAP deployment's
// Dispatch has had no active Workspace Directory answer for that person in 24 hours. It is 503
// BROKER_UNAVAILABLE with Retry-After 60, as its words ask, never a refusal of the token, which
// works again at the next active answer.
func errBrokerTokenLapsed() error {
	refusal := errorf(http.StatusServiceUnavailable, "BROKER_UNAVAILABLE",
		"whether this token's person may still act could not be read; try again in a minute")
	refusal.retryAfter = 60
	return refusal
}

// brokerPersonAdmitted holds a broker-issued token naming a person (caller.Person) to the rule
// that person's own personal token is held to, since it too carries their sign-in to an agent, and
// to the Google account it is. One statement reads, by the person's key, their member kind, the
// account (`sub`) people records for them, and when people recorded it (sub_recorded_at).
//
// A token is held to the account its credential records (brokerTokenAccount), read in the same
// snapshot, on a deployment whose sign-in records accounts (Deps.SignInIssuer set; elsewhere
// people.sub may be a Google account left from before a switch to a pool, and names nothing) and
// while no build without the account binding is in the fleet (accountsHeldSQL), which admits such a
// token whatever account it records:
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
// as the person scope's is (personCredential). A named token holds no scope but legion-status, a
// Legion daemon's own (legionStatusScopes), so it never mints or decides.
//
// That leaves no window to wait out while the broker pays a revocation: every credential the
// replaced account holds records it, or records no account and was made before the recording that
// replaced it or within brokerCredentialMargin after it, which refuses it. A credential the broker
// made after it recorded accounts records none only where the approve that made it named none: the
// approving credential is bound to no account, or Dispatch relayed it to a broker whose settings
// answered without approver_fields (cipher.Client.TakesApproverFields: an older broker, or a pod of
// one during a roll). An approve of the replaced account's passed its last check
// (approverStanding) before the recording, and reaches the broker within the margin, so its login
// is refused that way. The replacing sign-in sets sub_recorded_at in the transaction that records
// the revocation (store.PgPeopleStore.SignInSubject). The broker revocation still ends those
// credentials at the broker, for the secret reads Dispatch does not see (pass, broker_revocations).
//
// A restricted member is then held to restrictedOwnerAdmitted, the one check both credentials
// share. A read that fails is 503 IDENTITY_UNAVAILABLE, never an accept. So the token of a full
// member, or of a person no people row holds, costs that one read and answers as it always did
// while its account is theirs; only a restricted member's reads the lists. A token naming a
// service and no person is not asked. The admission answers whether the person is restricted,
// which a live socket's heartbeat holds it to. A broker token never mints, and decides a
// credential request only as an approve-only token, whose account its admission answers
// (decideBrokerToken) and the decision's re-read compares again, under the address's lock, with the
// one people records (approverStanding) where the account binding holds; nothing else past the
// admission reads its account.
//
// Last, where a Google rule signs people in (Deps.Standing set), the same statement reads whether
// Dispatch records a hold of the person (recordedHoldSQL), which is 401 MEMBERSHIP_HELD in the
// words for what ends it (recordedHoldRefusal), whatever the token's project scope: an operator
// entry's token names the person too. It asks Standing nothing: the broker's hold gate already
// asked the person route, and its introspection's held answer, which a caller reads after this
// admission (ciphertoken.Caller.Held), carries every hold this read cannot see. With Standing nil
// a recorded hold holds nobody.
func (s *server) brokerPersonAdmitted(ctx context.Context, caller ciphertoken.Caller) (brokerAdmission, error) {
	var kind string
	var replaced, named, recorded bool
	subject, issuer := s.brokerTokenAccount(caller)
	foreign := s.brokerForeignAccount(caller)
	held := s.deps.SignInIssuer != "" && (caller.ProjectScope != ciphertoken.ProjectScopeNamed || personCredential(caller))
	legacyWindow, markWindow := store.AccountBindingWindows()
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce((select member_kind from people where email = $1), ''),
			`+brokerAccountReplaced("$1", "$2::text", "$8::text", "$5::boolean", "$3::timestamptz", accountsHeldSQL("$4::boolean", "$6", "$7"))+`,
			`+brokerAccountNamed("$1", "$2::text")+`,
			`+recordedHoldSQL("$1")+`
	`, model.CanonicalLogin(caller.Person), subject, timeOrNull(caller.CredentialCreatedAt), held, foreign,
		legacyWindow, markWindow, issuer).
		Scan(&kind, &replaced, &named, &recorded)
	if err != nil {
		slog.Warn("dispatch: a broker token naming a person was refused: their member kind and recorded account could not be read", "error", err)
		return brokerAdmission{}, errorf(http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE",
			"whether this token's person may still act could not be read; try again in a minute")
	}
	if replaced {
		return brokerAdmission{}, brokerReplacedRefusal(subject == "", named)
	}
	admission := brokerAdmission{Restricted: model.MemberKind(kind) == model.MemberRestricted}
	if admission.Restricted {
		if err := s.restrictedOwnerAdmitted(ctx, caller.Person); err != nil {
			return brokerAdmission{}, err
		}
	}
	if recorded && s.deps.Standing != nil {
		return brokerAdmission{}, s.recordedHoldRefusal()
	}
	return admission, nil
}

// recordedHoldRefusal is the refusal of a credential whose person people records a hold of, read
// in the credential's own statement (recordedHoldSQL), where a Google rule signs people in
// (Deps.Standing set): 401 MEMBERSHIP_HELD in the words for what ends the hold. Under IAP identity
// (Standing.ChecksDirectory) the Workspace Directory recorded it and only its next active answer
// ends it, so the words are the Directory's (membershipHeldDirectoryText), as brokerTokenHeld's and
// a personal token's are there; under a Google sign-in rule a sign-in ends it (membershipHeldText).
func (s *server) recordedHoldRefusal() error {
	if s.deps.Standing.ChecksDirectory() {
		return errMembershipHeld(membershipHeldDirectoryText)
	}
	return errMembershipHeld(membershipHeldText)
}

// recordedHoldSQL is the SQL expression that people records a hold of the person address names
// (held_since later than signed_in_at, which a sign-in by any build releases), false for an
// address no row holds.
func recordedHoldSQL(address string) string {
	return "coalesce((select held_since > signed_in_at from people where email = " + address + "), false)"
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
// minutes after its address's account was recorded, which after the recording a broker makes only
// for an approve that named no account: one relayed to a broker whose settings answered without
// approver_fields (an older broker, or a pod of one during a roll; cipher.Client.TakesApproverFields);
// every other approve names the account (credentialSubjectSQL).
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
	case s.deps.Passkeys != nil && canonical == s.deps.Passkeys.Store().Issuer():
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
// the account binding holds (the deployment's sign-in records accounts and no build without the
// binding is in the fleet, accountsHeldSQL), and, at an admission, the token is no operator
// entry's. With accounts held and people recording one for the address, it holds for a token
// recording another account (the replaced account's, an account's that has not signed in since
// Google gave it the address, or the same `sub` of another issuer), for one recording an account
// of another issuer, and for one recording none whose credential was made before people recorded
// the address's account or less than brokerCredentialMargin after it, or at an unrecorded time.
// It is false for an address recording no account, and, for a token recording none, with no
// recording time, so nothing is refused that predates the column. An admission reads it
// (brokerPersonAdmitted), the one place it is read: the one broker token that decides a credential
// request, the approve-only token, is held at that decision to its recorded account alone where the
// account binding holds, which must then be the one people records for the address
// (approverStanding), and no broker token mints.
func brokerAccountReplaced(address, subject, issuer, foreign, createdAt, held string) string {
	return "(exists (select 1 from people where email = " + address + " and sub is not null and (" +
		foreign + " or (nullif(" + subject + ", '') is not null and (sub <> " + subject + " or coalesce(sub_issuer, '') <> " + issuer + "))" +
		" or (nullif(" + subject + ", '') is null and sub_recorded_at is not null and (" + createdAt + " is null or " +
		createdAt + " < sub_recorded_at + " + brokerCredentialMarginSQL + ")))) and " + held + ")"
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

// brokerPersonScopes is what a broker token that acts for its person holds, a read-only one
// included, but for an approve-only token (brokerApproveOnlyScopes): the person scope alone, never
// approve (brokerTokenActor). Every such credential shares this one slice, as model.BrowserActor's
// credential shares model.Scopes, so a request allocates none; nothing writes it.
var brokerPersonScopes = []string{model.ScopePerson}

// brokerApproveOnlyScopes is what an approve-only token holds (brokerTokenActor): the person and
// approve scopes, never p0, which no broker-issued token holds. It is a slice of its own, as
// brokerPersonScopes is, so no request allocates one; nothing writes it.
var brokerApproveOnlyScopes = []string{model.ScopePerson, model.ScopeApprove}

// legionStatusScopes is the scopes of a Legion daemon's own broker token: model.ScopeLegionStatus
// alone. It is a slice of its own, as brokerPersonScopes is, so no request allocates one and
// nothing appends to another credential's scopes. Read it, never write it: every daemon's
// credential shares this one slice.
var legionStatusScopes = []string{model.ScopeLegionStatus}

// maxTiedSessions bounds tiedSessions: once it holds this many pairs it starts again empty, which
// costs a later request of each pair one more statement, access.TieSession's, that finds its row
// and writes nothing.
const maxTiedSessions = 1 << 16

// tiedSessions is each (session, person) pair this process has recorded in session_owners
// (tieBrokerSession). A row there is never removed, so a pair held here is one the table holds. It
// holds no token's list: tieBrokerSession records the list on each request.
type tiedSessions struct {
	mu   sync.Mutex
	held map[string]struct{}
}

// tieBrokerSession records that session acts for person, whom a broker-issued token proved
// (access.TieSession), and records each private project the token's list covers as one the
// session reached (access.RecordListedReach): the session reads under that list from this request
// on, and the listener's record of the list is only sampled, so a token traded to a wider list
// and back between two samples would otherwise leave nothing saying what the session could read.
// A pair this process tied already is not written again; the list's record is made on each
// request that lists projects, held pair or not, because it records only the listed projects
// private at that moment and a listed project can turn private between two requests, which the
// switch records only for a session whose stored listener list names it. A server with no database
// records nothing: it relays no agent view either.
func (s *server) tieBrokerSession(ctx context.Context, session, person string, projects []string) error {
	login := model.CanonicalLogin(person)
	if session == "" || login == "" || s.deps.Store == nil || s.deps.Store.Pool == nil {
		return nil
	}
	pair := session + "\x00" + login
	ties := &s.sessionTies
	ties.mu.Lock()
	_, held := ties.held[pair]
	ties.mu.Unlock()
	if !held {
		if err := access.TieSession(ctx, s.deps.Store.Pool, session, login); err != nil {
			return err
		}
		ties.mu.Lock()
		if ties.held == nil || len(ties.held) >= maxTiedSessions {
			ties.held = make(map[string]struct{})
		}
		ties.held[pair] = struct{}{}
		ties.mu.Unlock()
	}
	return access.RecordListedReach(ctx, s.deps.Store.Pool, session, projects)
}

// brokerTokenActor authenticates a caller presenting an access token the secrets broker issued,
// under the DPoP scheme: the broker is asked whether the token is live and the caller is held to
// the key that token is bound to (internal/ciphertoken). A verified caller is the session the
// token names, acting for the person it names, or, where the broker names none, for the service
// it names: the fleet of the machine holding it, the account an operator's entry lists, or a
// job's identity. It is never retried as another credential. The credential carries the token's
// project scope and its list as the broker answered them (model.Credential.ProjectScope,
// Projects), which is what tells the callers that name a service and no person apart.
//
// A job's token is one of those: the exchange mints it from a job entry, which names an identity
// (a pod's Kubernetes service account, a workflow, or a Google account), that identity's projects
// and no person. Its service is that identity, and its project scope is `named` with the one
// project its entry names or `public` with its entry's private projects, beside which it reaches
// every public project. It writes as its session, for its service and under no person:
// writeNeedsASignedInPerson lets a broker credential naming no person write under those two
// scopes alone (project_scope.go). A `named` one sets Credential.NamedProject (below), so the
// project check judges it as it judges a person's named token for that project; a `public` one
// names no one project, so the project check holds it to none and its writes that belong to the
// deployment follow the rule of a token naming no project. It holds no OAuth scope, so the rows
// that need `person` or `approve` turn it away (gateRouteScopes).
//
// A caller that names a service and no person under the project scope named and names no project
// is an account entry's that names no person and no project (cipher/exchange's Set.Pod): its
// service is the account, system:serviceaccount:<namespace>:<name>, and its writes are recorded
// under that account, as the session the token names acting for that service and for no person
// (model.Actor.Service). writeNeedsASignedInPerson lets it write, since there is somebody to
// record the write under: the account the operator listed. It holds no OAuth scope, so a route
// that needs `person` or `approve` refuses it (gateRouteScopes), and it names no project, so it
// writes wherever Dispatch's own rules let a credential naming none write.
//
// A caller that names a service and no person under the project scope person is a machine nobody
// has signed in on (`agent-secrets login`). It reads as any session does, and
// writeNeedsASignedInPerson refuses its every write at the one chokepoint, so no write is
// recorded for a machine nobody stands behind. It holds no OAuth scope either, so the rows a
// person's own credential reaches turn it away earlier still (gateRouteScopes): there is no
// person for it to act as.
//
// A read-only token writes nothing (ciphertoken.Caller.ReadOnly, which only a verifier built with
// Options.ServesReadOnly answers). It is the read entry's token, which names a service and no
// person and holds the scope read alone, or a token narrowed to reading because its trade asked
// for read, which names the person or the service, and the projects, that the same trade would
// have named without read: an account entry's naming no person among them, narrowed, names its
// account and writes nothing, as every read-only token does. The broker narrows no job's token.
// Its credential is marked read-only (model.Credential.ReadOnly), with the token's cut
// (model.Credential.Cut), and refuseReadOnlyToken answers it 403 READ_ONLY_TOKEN on every route
// outside the read class (routeDecl.Read) and on GET /ws/live, ahead of the scope gate, the
// sign-in check, an account entry's write as its account and everything else the route does.
// While no build answers a read at a past time, it answers one that carries a cut 403
// AS_OF_TOKEN_NOT_SERVED on every read but GET /api/v1/whoami. On a read it serves, it is the
// session it names, reading as that session does.
//
// Each refusal is its own code, and none of them repeats the token the caller presented:
//
//   - 401 BROKER_TOKEN_NOT_ACTIVE — the broker answered that the token is not live (unknown,
//     revoked or expired).
//   - 401 BROKER_TOKEN_PROOF_INVALID — the proof did not meet RFC 9449 Section 4.3: a bad
//     signature, a method or target URI that is not this request's, an 'iat' outside the window,
//     a missing or wrong 'ath', or a proof already spent; or its 'jti' holds a control character,
//     or its 'jti' and 'htu' together run over the 2,048 bytes the replay store records
//     (store.ErrUnstorableProof: the store records nothing of either), which the library reports
//     as its own failure, so it is answered here as the caller's.
//   - 401 BROKER_TOKEN_KEY_MISMATCH — a sound proof signed by a key that is not the token's
//     'jkt', which is what makes a stolen token useless.
//   - 401 BROKER_TOKEN_NOT_A_SESSION — the broker holds the token live, but it is not a
//     session's credential this verifier serves: every case ciphertoken.ErrNotASession lists,
//     the one list of them. Among them: it names no session, it names neither a person nor a
//     service, it was issued to a client other than the exchange a session's token comes from,
//     or its project scope and its scope are a shape no broker mints - the scope read or a cut
//     beside a project scope that writes, the project scope none or a narrowed one in any shape
//     but a read-only token's, a job's shapes broken (public beside a person or a scope, named
//     beside no person and a scope), or the scope approve in any shape but an approve-only
//     token's. A machine login's own access token is one of these, and Dispatch
//     records every write under the session its credential proved, so it acts on none of them
//     rather than letting a request body name a session for a credential that names none.
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
// or within brokerCredentialMargin after, is 401 BROKER_TOKEN_RECORDS_NO_ACCOUNT. Last, a person
// Dispatch holds is 401 MEMBERSHIP_HELD: a hold Dispatch records (brokerPersonAdmitted), and then
// one the broker's introspection answered (ciphertoken.Caller.Held), in the words brokerTokenHeld
// gives under the rule that signs people in. So a revocation and the admission rule answer before
// a hold. A not-held answer whose deadline has passed (ciphertoken.Verifier.Lapsed, an IAP
// deployment's person with no active Workspace Directory answer in 24 hours) is 503
// BROKER_UNAVAILABLE, saying whether the token's person may still act could not be read. After
// those, a token naming a restricted member that lists other than one project is 403
// RESTRICTED_MEMBER (restrictedBrokerTokenList, R16).
func (s *server) brokerTokenActor(r *http.Request) (model.Actor, bool, error) {
	return s.brokerTokenActorBinding(r, nil)
}

// brokerTokenActorBinding is brokerTokenActor that also answers, in *bound when bound is not nil,
// the binding of a verified approve-only token that holds the approve scope (approveOnlyBinding),
// which decideBrokerToken carries; it leaves *bound nil for every other caller and every refusal.
func (s *server) brokerTokenActorBinding(r *http.Request, bound **approveOnlyBinding) (model.Actor, bool, error) {
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
	switch {
	case caller.Held:
		return model.Actor{}, false, s.brokerTokenHeld(caller.Person)
	case s.deps.BrokerTokens.Lapsed(caller):
		return model.Actor{}, false, errBrokerTokenLapsed()
	}
	if err := restrictedBrokerTokenList(admission, caller); err != nil {
		return model.Actor{}, false, err
	}
	// A token the broker scoped to its person holds the person scope: that token is the person's
	// own agent acting for them, so it reads, writes and answers their questions as they do. No
	// broker-issued token a machine holds gets approve, whatever its project scope
	// (brokerPersonScopes): every one is made from a file on the machine that holds it, a machine
	// login or a person's sign-in, so whoever copies that file holds it, and approve settles what
	// only the person settles - an approval, a credential decision or revoke, a personal token.
	// Those need the person's browser session, a personal token the person gave approve, or the
	// one broker-issued token no machine holds: an approve-only token (ciphertoken.Caller.
	// ApproveOnly), which the secrets broker mints for a person's approve grant, binds to its own
	// pod's key and spends through its proxy without ever releasing it. That token holds person
	// and approve (brokerApproveOnlyScopes), never p0, and is marked approve-only, so
	// refuseApproveOnlyToken holds it to the routes that state ApproveOnly (routeDecl.ApproveOnly)
	// before any route runs. Those let it reach the scope gate of a credential request's approve
	// and deny too, and there it is the one broker-issued token that decides, as its person: where
	// the account binding holds, it is bound to the account its credential records, which this
	// resolution answers (bound, below) and decideBrokerToken carries, and which the decision's
	// re-read compares with the one people records for the address under the address's lock
	// (approverStanding). Their handler refuses every other broker-issued token
	// (refuseUnboundCredential). A token scoped to a project gets no person scope, because the
	// deployment mapped that token to a project and not to the person: it proves a session, not
	// that any person stands behind the request. A token that names a service and no person gets
	// none either, whatever scope the broker answered: there is no person for it to act as, and a
	// scope that said otherwise would let it act as the session's own id on the routes that read
	// what belongs to a person. A job's token, named or public, is one of those.
	//
	// A read-only token is read by its base word (ciphertoken.Caller.ProjectScope) as the same
	// token without read would be: one narrowed from a token that acts as its person still acts
	// for that person, so it holds the person scope, and every other holds none. The scope read
	// itself is carried as the credential's ReadOnly mark and never as a scope a route could
	// require, so no recorded act names it.
	//
	// A token scoped to one project whose answer holds legion-status gets that scope alone
	// (legionStatusScopes). The broker grants it to a Legion daemon's own token only. A named
	// token that names no project names a person, as the shared public-session account's token
	// does. legion-status on such a token would pass R26 on the Legion issues of every project,
	// so it gets no scope. Dispatch does not check the token's session: the broker decides who
	// holds the scope. No credential holds it beside person, and no act records it: the act of a
	// proven session (bearerSessionActor) and of a person (actingPerson) record the credential's
	// scopes through actScopes, which leaves legion-status out. oneProject is also the
	// test for the one project the token may write (Credential.NamedProject, below), so the two
	// cannot drift apart.
	//
	// A restricted member's token gets no scope at all, whatever the cases above would give it,
	// so that case is tested first. A restricted member's machine login trades for a token scoped
	// to a project: a person's own credential made that one, and it is held to that person's
	// account (brokerPersonAdmitted, personCredential), but it is the outside person's agent held
	// to the one project they work on, which acts in that project and settles nothing of the
	// person's own. A restricted member's token the broker scoped to their person, a read-only
	// one narrowed from it included, gets none for the same reason: a broker whose project check
	// is off, an older broker during a rollout, or a token minted before the broker held
	// restricted members to one project can still answer one, and brokerPersonAdmitted has read
	// the member's kind above, so the scope does not rest on the broker's check alone. Their
	// approve-only token gets none either, approve included: such a member decides a pending
	// machine login of their own at their browser alone (ownMachineLogin), never through a token.
	// Such a token that lists other than one project acts for nobody (restrictedBrokerTokenList,
	// above).
	oneProject := caller.ProjectScope == ciphertoken.ProjectScopeNamed && len(caller.Projects) == 1
	var scopes []string
	switch {
	case admission.Restricted:
		// No scope (above).
	case caller.ApproveOnly:
		scopes = brokerApproveOnlyScopes
	case caller.Person != "" && caller.ProjectScope == ciphertoken.ProjectScopePerson:
		scopes = brokerPersonScopes
	case caller.LegionStatus && oneProject:
		scopes = legionStatusScopes
	}
	actor := model.Actor{
		Kind: "session",
		ID:   caller.Session,
		// The broker proved this session, so the credential already names the agent behind the
		// request and no request body has to name it, and the actor is Proven. A read-only token
		// is marked as one, with the cut it reads at, which are what refuseReadOnlyToken reads, an
		// approve-only token as one, which is all refuseApproveOnlyToken reads, and the project scope
		// and the list are carried as the broker answered them, the scope as its base word
		// (ciphertoken.Caller.ProjectScope), which is what writeNeedsASignedInPerson reads of a
		// caller naming a service and no person: a job's token and an account entry's token naming
		// no person write, and a machine's nobody signed in on does not. The principal is the one the
		// listener records a registration on this token as, which a successor's reply is held to.
		Credential: &model.Credential{
			Scopes:       scopes,
			Session:      caller.Session,
			ReadOnly:     caller.ReadOnly,
			ApproveOnly:  caller.ApproveOnly,
			Cut:          caller.Cut,
			ProjectScope: caller.ProjectScope,
			Projects:     caller.Projects,
			Principal:    caller.Principal(),
		},
		Proven: true,
	}
	// A caller names a person or a service, never both and never neither (ciphertoken.Caller),
	// and the two fields are set from the token alone, never from a body. whoami reports both.
	if caller.Person != "" {
		actor.Owner = &caller.Person
		// The token proved this session acts for its person, on a read as on a write, so the tie
		// the live agent view reads (session_owners, access.SessionContentClause) is recorded
		// here, before the request reads anything: a session that only reads is tied too, and
		// each private project its list covers is recorded as one it reached.
		if err := s.tieBrokerSession(r.Context(), caller.Session, caller.Person, caller.Projects); err != nil {
			if refusal := s.tieFailureRefusal(r.Context(), err); refusal != nil {
				return model.Actor{}, false, refusal
			}
		}
	}
	if caller.Service != "" {
		actor.Service = &caller.Service
	}
	// A token's list is its credential's, carried as the broker answered it (Projects, above),
	// which the one rule of what a caller sees reads (access.CredentialCovers): for a full
	// member's token it decides only which private projects the token reaches, so while the
	// deployment holds no private project it changes nothing the token did before projects could
	// be private (R9); for a restricted member's token it narrows every project (R3).
	//
	// A token held to one project names that project (NamedProject, which namedProjectOf reads):
	// one an operator's entry mapped, today's named-scope rule; a job's named token, whose entry
	// names one project, named the same way, so the project check judges the two alike; and a
	// restricted member's machine login's (the broker's exchange.Set.Machine). A token narrowed
	// from a named one names its project too, as the project it reads as, and writes none. A full
	// member's such token writes every project's objects that a token naming no project may
	// write. The one exception is a private project it does not name (access.CredentialCovers). A
	// target that resolves to no project or to nothing is still refused it (withinProjectScope).
	// A restricted member's such token is held by its list, which covers that project alone.
	// ciphertoken refuses a named token naming two, and a named token naming no person, no
	// project and a service that is no account. A named token naming none (the shared
	// public-session account's, and an account entry's naming no person) writes wherever
	// Dispatch's own rules let it.
	//
	// A token acting as its person carries the list its trade asked for
	// (ciphertoken.Caller.Projects), and that list holds no write by name: a dev box session's
	// person writes wherever they may, and the token of a machine naming a service and no person
	// names no project at all. A job's public token names no one project either, so it writes
	// wherever a token naming no project may: the private projects of its list limit no write
	// here. Nor does a token of the project scope none, the read entry's or one narrowed from it,
	// which names no project and writes none.
	if oneProject {
		actor.Credential.NamedProject = caller.Projects[0]
	}
	// The writer the line names: the person, or the service for a token that names none, under
	// the `service:` prefix requestKeyLogin spells a service with, so one field never reads a
	// fleet's name or an account as a person's login.
	writer := caller.Person
	if writer == "" {
		writer = "service:" + caller.Service
	}
	accesslog.Note(r.Context(), accesslog.KindBrokerToken, writer)
	// The shape is the verifier's (ciphertoken.Caller.ApproveOnly), never a scope, a body, a query
	// or the mere presence of a bearer. A token that holds no approve scope is bound to nothing and
	// so decides nothing: a restricted member's holds none (above), though
	// restrictedBrokerTokenList has refused every approve-only token of theirs already, since such
	// a token lists no project.
	if bound != nil && caller.ApproveOnly && actor.Credential.Holds(model.ScopeApprove) {
		subject, issuer := s.brokerTokenAccount(caller)
		*bound = &approveOnlyBinding{
			person:  model.CanonicalLogin(caller.Person),
			account: credentialAccount{subject: subject, issuer: issuer},
		}
	}
	return actor, false, nil
}

// tieFailureRefusal is what a broker-issued token's request is answered when its session's tie to
// its person (tieBrokerSession) could not be written, err the failed write. While any project is
// private the tie decides what the live agent view relays and which conversations a restricted
// member reads (access.SessionContentClause), so the request is refused 503 DATABASE_UNAVAILABLE
// and the caller comes back, as before. While no project is private every reader that may watch a
// session watches it without a tie (R9), so the failure is logged and the request served: the
// pair is not held as written (tieBrokerSession keeps only a pair it wrote), so the session's next
// request writes it again. A read of whether any project is private that fails too refuses, since
// nothing then says the tie decides nothing.
func (s *server) tieFailureRefusal(ctx context.Context, err error) *apiError {
	private, readErr := access.AnyProjectPrivate(ctx, s.deps.Store.Pool)
	if readErr != nil || private {
		slog.Warn("dispatch: a broker-issued token's session could not be tied to its person", "error", err, "private_read_error", readErr)
		return errorf(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE",
			"this session's person could not be recorded; try again in a minute")
	}
	slog.Warn("dispatch: a broker-issued token's session could not be tied to its person; no project is private, so the request is served and its session's next request writes the tie again", "error", err)
	return nil
}
