package oauthstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc8693"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/exchange"
)

// What the exchange's handlers ask of the session they run on: the token exchange's own
// interface, the provider's, and the DPoP binding the broker enforces, which the embedded
// *oauth2.DefaultSession answers so a session that carries a binding keeps it through an
// exchange.
var (
	_ oauth2.Session          = (*ExchangeSession)(nil)
	_ rfc8693.Session         = (*ExchangeSession)(nil)
	_ oauth2.DPoPBoundSession = (*ExchangeSession)(nil)
)

// ErrSessionHeld is a session name another principal holds and the claimant may not take: its
// holder is no account paired with the claimant (SetSessionPairs; a claim reads the claimant's own
// pairs, so a pair does not chain), or the configuration fixes the name, which no pair takes. It
// is the one refusal BindSession has, and the text names the session so an operator reading it
// knows which name is taken without having to find the request again.
var ErrSessionHeld = errors.New("oauthstore: another identity holds this session name")

// SessionRetention is how long a session name stays its principal's after the last token minted
// under it expires: 90 days. Until then the name is refused to every other principal but an
// account paired with its principal (SetSessionPairs), which takes it unless the configuration
// fixes it, and a pair does not chain: an account paired only with the principal's pair is
// refused too. After it, the sweep deletes the row and the name is anyone's to claim.
//
// It bounds how many names the table keeps, and it is safe to state because a name moves to a
// principal not paired with its own only once its principal has stopped using it for longer than
// any credential the exchange accepts can sit unused: the longest, a machine login, lapses at
// most 30 days after its last refresh (BROKER_LAUNCHER_CREDENTIAL_SECONDS' ceiling), a pod's
// projected token at most a day after it was minted, a workflow run's within the hour. A
// principal coming back after 90 days comes back through a new login approved by a person, a new
// pod or a new run, and nothing it held before is still spendable. It is far past how long a
// Legion role sits idle between assignments, which is hours or days.
//
// It is a constant rather than a setting because the bound follows from those ceilings, not from
// anything a deployment chooses: shorter would hand a name over while its principal could still
// return, and longer only keeps rows. An operator who must free a name sooner deletes its row
// from oauth_session_bindings.
const SessionRetention = 90 * 24 * time.Hour

// PodSessionRetention is how long a pod's registration to write under a session name
// (RegisterPodSession) is kept past the last write of it: 24 hours.
//
// It is the longest a subject token the exchange accepts from a pod can live
// (oauth.kubernetesTolerance, which admits a projected service-account token's whole life,
// Kubernetes bounding one at 24 hours), so a registration outlives every token the pod it was
// written for can still present. Shorter would refuse a pod still holding a token the exchange
// accepts; longer would let a pod write under the name after every token naming it had been
// refused for age anyway.
//
// It is a constant rather than a setting because the bound follows from that tolerance and from
// nothing a deployment chooses. What it means for an operator: the authority is the registrar's,
// so a daemon that stops registering costs its pods their writes within this window, and a
// registration is renewed by writing it again.
const PodSessionRetention = 24 * time.Hour

// heldByConfiguration is the expires_at of a session name an entry fixes, which ReserveSessions
// and a fixed entry's own BindSession write and nothing lets lapse: the name is its entry's for as
// long as the configuration says so, and Sweep's expires_at <= now() never selects it.
const heldByConfiguration = "'infinity'::timestamptz"

// claimActor is the RFC 8693 Section 4.1 'act' claim, which names the party acting for the
// subject in a delegation. The library spells it in a package its users cannot import.
const claimActor = "act"

// ExchangeSession is the session an exchanged token's request runs on: the *oauth2.DefaultSession
// every other grant of this provider uses, with the subject and actor token claims the RFC 8693
// handlers deposit on it beside it. It is what rfc8693.Session asks of a session, and embedding
// the default one means a row written from either kind reads back into either kind, so the
// machine login's stored sessions keep working unchanged.
//
// Both claim maps are stored, not dropped as the library's own session drops them: an
// introspection of an exchanged token answers from the row, and the claims the exchange decided
// on are part of what the row has to say.
type ExchangeSession struct {
	*oauth2.DefaultSession

	// SubjectToken is the verified claims of the subject token the exchange was made against: a
	// pod's service-account token, a workflow run's GitHub token, a machine's Google identity
	// token, a person's own Google identity token, or a machine access token.
	SubjectToken map[string]any `json:"subject_token,omitempty"`

	// ActorToken is the verified claims of the actor token of a delegation: the Google identity
	// token of the machine a person signed in on, which the exchange client is registered for
	// and no other kind of token is. It is empty for every other trade, none of which has an
	// actor, and it is stored because an introspection of the token answers from the row.
	ActorToken map[string]any `json:"actor_token,omitempty"`

	// Principal is who the session name the exchange decided on belongs to
	// (exchange.Claims.Principal). The exchange's decision sets it while the library validates
	// the request, and the provider claims the name for it once the library has set the issued
	// token's expiry. It is not a claim of the token and is not stored: the row the claim is
	// recorded in is oauth_session_bindings.
	Principal string `json:"-"`

	// Instance is the pod uid the exchange decided this token's writes come from
	// (exchange.Claims.Instance), empty for a caller no registration separates from the others of
	// its principal: a fixed-session entry, a workflow run or a dev box. The provider reads it
	// against oauth_pod_sessions in the exchange's own transaction, so a pod writes under a
	// session name only while a registration says it may. Like Principal it is no claim of the
	// token and is not stored.
	Instance string `json:"-"`

	// Fleet is the service the machine's Google account is listed for
	// (exchange.Claims.Fleet), empty for every other kind of caller. The provider records it on
	// the launcher credential it mints for that machine in the exchange's own transaction, which
	// is what lets the machine enroll afterwards. Like Principal it is no claim of the token and
	// is not stored.
	Fleet string `json:"-"`

	// AccessLifetime is the longest the access token this exchange issues may live
	// (exchange.Claims.Lifetime): a restricted member's machine login's is held to
	// exchange.RestrictedLifetime, and zero leaves every other token the provider's own lifetime.
	// The library sets the issued token's expiry through SetExpiresAt below, before it stores the
	// token or answers expires_in, so the row, the introspection's exp and the answer all carry
	// the shorter expiry. Like Principal it is no claim of the token and is not stored.
	AccessLifetime time.Duration `json:"-"`

	// AccessDeadline is the latest the access token may expire: AccessLifetime counted from the
	// moment the exchange's check of the token's projects against Dispatch returned
	// (StartAccessLifetime), not from the mint, which waits on the exchange's transaction after it.
	// So a token held to AccessLifetime ends that long after the last answer that admitted it,
	// however long the transaction took. Zero with a lifetime decided is a start that never ran, and
	// its token expires at once (SetExpiresAt). Like Principal it is not stored.
	AccessDeadline time.Time `json:"-"`
}

// NewExchangeSession is an empty exchange session with its extra claims ready to be written, the
// shape the provider's session factory hands every request.
func NewExchangeSession() *ExchangeSession {
	return &ExchangeSession{DefaultSession: &oauth2.DefaultSession{Extra: map[string]any{}}}
}

// StartAccessLifetime sets AccessDeadline, AccessLifetime from checked, the moment the exchange's
// project check returned, when the exchange decided a lifetime; it leaves every other token's alone.
func (s *ExchangeSession) StartAccessLifetime(checked time.Time) {
	if s.AccessLifetime > 0 {
		s.AccessDeadline = checked.UTC().Add(s.AccessLifetime)
	}
}

// SetExpiresAt sets the expiry of the token of kind key, as the embedded session does, except that
// an access token's is held to AccessDeadline when the expiry asked for is later: the library asks
// for its own lifetime, and a shorter one the exchange decided wins. It never lengthens an expiry,
// so a token's lifetime is the lesser of the two. An exchange that decided a lifetime and never
// started it (StartAccessLifetime) has no moment to count it from, so its token expires at once:
// a trade that skipped the start mints a token that lives no time, which every caller and row
// reading its expires_in sees, rather than one living the lifetime from its mint, past the bound.
func (s *ExchangeSession) SetExpiresAt(key oauth2.TokenType, exp time.Time) {
	if s.DefaultSession == nil {
		s.DefaultSession = &oauth2.DefaultSession{}
	}
	if key == oauth2.AccessToken && s.AccessLifetime > 0 {
		latest := s.AccessDeadline
		if latest.IsZero() {
			latest = time.Now().UTC()
		}
		if exp.After(latest) {
			exp = latest
		}
	}
	s.DefaultSession.SetExpiresAt(key, exp)
}

// SetSubjectToken records the subject token's verified claims.
func (s *ExchangeSession) SetSubjectToken(token map[string]any) { s.SubjectToken = token }

// GetSubjectToken is the subject token's verified claims, nil when none were set.
func (s *ExchangeSession) GetSubjectToken() map[string]any { return s.SubjectToken }

// SetActorToken records the actor token's verified claims.
func (s *ExchangeSession) SetActorToken(token map[string]any) { s.ActorToken = token }

// GetActorToken is the actor token's verified claims, nil when none were set.
func (s *ExchangeSession) GetActorToken() map[string]any { return s.ActorToken }

// SetClaimActor records the 'act' claim of a delegation among the session's extra claims, which
// is where this provider's opaque access tokens carry everything an introspection answers.
func (s *ExchangeSession) SetClaimActor(act map[string]any) {
	if s.DefaultSession == nil {
		s.DefaultSession = &oauth2.DefaultSession{}
	}
	if s.Extra == nil {
		s.Extra = map[string]any{}
	}
	s.Extra[claimActor] = act
}

// AccessTokenClaimsMap is what an exchanged access token says about itself: its subject, its
// username and every extra claim the exchange wrote, which is what an introspection answer is
// built from.
func (s *ExchangeSession) AccessTokenClaimsMap() map[string]any {
	claims := map[string]any{"sub": s.GetSubject(), "username": s.GetUsername()}
	if s.DefaultSession != nil {
		maps.Copy(claims, s.Extra)
	}
	return claims
}

// Clone is a copy of the session that shares nothing the caller can write through, which the
// library relies on when it carries one request's session onto another. The two claim maps are
// copied one level deep: they are a verified token's claims, which nothing changes once the
// handler that read them has set them.
func (s *ExchangeSession) Clone() oauth2.Session {
	if s == nil {
		return nil
	}
	cloned := &ExchangeSession{
		SubjectToken:   maps.Clone(s.SubjectToken),
		ActorToken:     maps.Clone(s.ActorToken),
		Principal:      s.Principal,
		Instance:       s.Instance,
		Fleet:          s.Fleet,
		AccessLifetime: s.AccessLifetime,
		AccessDeadline: s.AccessDeadline,
	}
	if s.DefaultSession != nil {
		cloned.DefaultSession, _ = s.DefaultSession.Clone().(*oauth2.DefaultSession)
	}
	return cloned
}

// BindSession claims the session name for principal until SessionRetention past expires, the
// moment the token the claim is made for expires, and reports ErrSessionHeld when another
// principal holds it and principal may not take it. fixed says the configuration of the process
// making the claim fixes the name for principal; the claim then holds the name with no expiry and
// takes it from whoever holds it. BindSession answers the principal it took the name from, one
// whose claim had not run out, by a fixed claim or a pair's (below); it answers "" for a name
// that was unheld, run out or already principal's.
//
// The name is the principal's from its first claim: the same principal re-claiming it extends
// the claim and never shortens it, any other principal is refused for as long as the claim
// stands, and an unheld name, or one whose claim has run out, is claimed outright. A name an
// entry fixes is held by ReserveSessions before its entry's first claim, so a stranger is refused
// it by this same rule, and never runs out. Without this, any caller that may exchange could name
// any session string, and a token naming another principal's session would be believed by
// everything downstream that believes the token.
//
// Beside a fixed claim, the one claim that takes a live name from its holder is that of an
// account paired with the holder (SetSessionPairs, PairedWith; with none set, none is paired),
// so a deployment whose pods move to another account keeps each role's session name, which the
// role's new pod would otherwise be refused. A pair works both ways: pods moved back take the
// names back the same way. It does not chain: a claim reads the claimant's own pairs, and every
// pair works both ways, so with A paired with B and B with C, C takes a name B holds and never one
// A holds. And it never takes a name the configuration fixes: one held with no expiry
// (ReserveSessions, a fixed claim), nor one Reserved fixes for anyone, though another principal
// still holds it as an ordinary claim, since such a name goes to its entry at the entry's own
// claim and to nobody else. A take holds the name for the taker's own claim, SessionRetention past
// expires, not for what was left of the holder's; only a principal re-claiming its own name keeps
// the later of the two expiries.
//
// The configuration decides here, at the claim, rather than at ReserveSessions: two replicas
// started with configurations that disagree, as during a rollout that re-points a fixed name,
// each give the name to the principal their own configuration fixes it for when that principal
// asks, so the entry the configuration fixes a name for is never refused its own name, and a
// re-point takes effect at the new entry's first exchange.
//
// It runs in the transaction ctx carries (WithTx, BeginTX), so an enrollment's claim commits or
// rolls back with the enrollment and an exchange's with the token it is made for, and in one of
// its own when ctx carries none. It locks the name's row first (select ... for update), inserting
// the claim when there is none, and then one update writes the claim; that update's where is the
// only place a claim is refused. The lock is what makes two claims racing for one name end with
// one holder: the second waits on the row the first holds, and under read committed then reads
// and decides on the row the first committed, so a claimant not paired with the new holder is
// refused and one paired with it takes the name in turn; under repeatable read it fails with a
// serialization error instead and writes nothing. The holder it answers is the one the lock read,
// which is the holder the deciding update read, so a claim that committed while this one waited
// is the one named.
func (s *Store) BindSession(ctx context.Context, session, principal string, fixed bool, expires time.Time) (string, error) {
	ctx, err := s.BeginTX(ctx)
	if err != nil {
		return "", fmt.Errorf("bind session name: %w", err)
	}
	defer s.Rollback(ctx)
	taken, err := s.bindSession(ctx, session, principal, fixed, expires)
	if err != nil {
		return "", err
	}
	if err := s.Commit(ctx); err != nil {
		return "", fmt.Errorf("bind session name: %w", err)
	}
	return taken, nil
}

// bindSession is BindSession's claim, made in the transaction ctx carries.
func (s *Store) bindSession(ctx context.Context, session, principal string, fixed bool, expires time.Time) (string, error) {
	db, until := s.db(ctx), expires.Add(SessionRetention)
	// holder and live are the name's row as its lock read it: the principal holding it, and
	// whether that hold had not run out.
	var holder string
	var live bool
	for {
		err := db.QueryRow(ctx, `select principal, expires_at > now() from oauth_session_bindings where session = $1 for update`,
			session).Scan(&holder, &live)
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("bind session name: %w", err)
		}
		// No row: this is the name's first claim, unless another claim inserted the row since
		// the select read, in which case the insert writes nothing and the next pass locks and
		// reads that claim's row.
		inserted, err := db.Exec(ctx, `insert into oauth_session_bindings (session, principal, expires_at)
			values ($1, $2, case when $4 then `+heldByConfiguration+` else $3 end)
			on conflict (session) do nothing`, session, principal, until, fixed)
		if err != nil {
			return "", fmt.Errorf("bind session name: %w", err)
		}
		if inserted.RowsAffected() == 1 {
			return "", nil
		}
	}
	_, reserved := s.Reserved[session]
	updated, err := db.Exec(ctx, `update oauth_session_bindings set principal = $2,
			expires_at = case when $4 then `+heldByConfiguration+` when principal = $2 then greatest(expires_at, $3) else $3 end
		where session = $1 and ($4 or principal = $2 or expires_at <= now()
			or (principal = any($5::text[]) and expires_at <> `+heldByConfiguration+` and not $6))`,
		session, principal, until, fixed, s.PairedWith(principal), reserved)
	if err != nil {
		return "", fmt.Errorf("bind session name: %w", err)
	}
	if updated.RowsAffected() == 0 {
		return "", fmt.Errorf("%w: %s", ErrSessionHeld, session)
	}
	if live && holder != principal {
		return holder, nil
	}
	return "", nil
}

// SetSessionPairs pairs the two principals of each of pairs for session names, each principal
// spelled as exchange.AccountPrincipal spells an account ("account:" and the ServiceAccount
// subject), and replaces whatever pairs were set before; nil or empty pairs nothing. Either
// principal of a pair takes a session name the other holds (BindSession), so it writes each pair
// both ways and the set is symmetric by construction. It is what lets an operator move a
// deployment's pods to another account without each role's new pod being refused its own session
// name, as cmd/broker reads BROKER_SESSION_ACCOUNT_PAIRS. It must be called before the store
// serves an enrollment or a token exchange, since the claims read the pairs with no lock.
func (s *Store) SetSessionPairs(pairs [][2]string) {
	paired := make(map[string][]string, 2*len(pairs))
	for _, pair := range pairs {
		paired[pair[0]] = append(paired[pair[0]], pair[1])
		paired[pair[1]] = append(paired[pair[1]], pair[0])
	}
	s.sessionPairs = paired
}

// PairedWith is every principal SetSessionPairs paired principal with, the pairs a claim by
// principal reads (BindSession): principal takes a session name any of them holds, and, since
// every pair works both ways, each of them takes one principal holds. A claim reads the
// claimant's own pairs alone, so a pair does not chain: with A paired with B and B with C, C takes
// a name B holds and never one A holds. It is nil for a principal in no pair, and the slice is the
// store's own, which the caller must not write to.
func (s *Store) PairedWith(principal string) []string {
	return s.sessionPairs[principal]
}

// ReserveSessions holds every session name reserved maps to a principal (the names the entries of
// the configuration fix, exchange.Set.Reservations) for that principal, with no expiry, unless
// another principal holds it, and answers each name it left with another principal mapped to that
// holder. A name an earlier configuration fixed and this one does not is handed back to the
// ordinary rule, its principal holding it for SessionRetention from now.
//
// It takes nothing: it is what refuses a stranger a fixed name before the entry's own caller has
// ever exchanged, and a name another principal holds already refuses strangers. Taking is
// BindSession's, at the fixed entry's own claim, so a replica started with an older configuration
// that reserves after a newer one cannot take back a name the newer one has re-pointed.
//
// It is one statement, so the names are reserved together or not at all, and a BindSession racing
// it waits on the row it is writing.
func (s *Store) ReserveSessions(ctx context.Context, reserved map[string]string) (map[string]string, error) {
	sessions, principals := make([]string, 0, len(reserved)), make([]string, 0, len(reserved))
	for session, principal := range reserved {
		sessions, principals = append(sessions, session), append(principals, principal)
	}
	rows, err := s.db(ctx).Query(ctx, `with released as (
			update oauth_session_bindings set expires_at = $3
			where expires_at = `+heldByConfiguration+` and session <> all($1)
		), held as (
			insert into oauth_session_bindings (session, principal, expires_at)
			select session, principal, `+heldByConfiguration+` from unnest($1::text[], $2::text[]) as r (session, principal)
			on conflict (session) do update set principal = excluded.principal, expires_at = excluded.expires_at
			where oauth_session_bindings.principal = excluded.principal or oauth_session_bindings.expires_at <= now()
			returning session
		)
		select session, principal from oauth_session_bindings
		where session = any($1) and session not in (select session from held)`,
		sessions, principals, time.Now().Add(SessionRetention))
	if err != nil {
		return nil, fmt.Errorf("reserve the session names the configuration fixes: %w", err)
	}
	defer rows.Close()
	left := map[string]string{}
	for rows.Next() {
		var session, holder string
		if err := rows.Scan(&session, &holder); err != nil {
			return nil, fmt.Errorf("reserve the session names the configuration fixes: %w", err)
		}
		left[session] = holder
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reserve the session names the configuration fixes: %w", err)
	}
	return left, nil
}

// ErrPodSessionBoundToAnotherKey is what PodSessionRegistered's refusal (*PodSessionBoundError)
// is: a live registration of the pod and session that is bound to a key other than the one the
// exchange's proof presented.
var ErrPodSessionBoundToAnotherKey = errors.New("the pod's registration for this session is bound to another key")

// PodSessionBoundError is PodSessionRegistered's refusal of a key other than the one the pod's
// live registration for the session is bound to. It is ErrPodSessionBoundToAnotherKey
// (errors.Is), and Bound is the thumbprint of the key the registration is bound to, which the
// exchange logs beside the one the refused trade proved.
type PodSessionBoundError struct{ Bound string }

func (e *PodSessionBoundError) Error() string {
	return ErrPodSessionBoundToAnotherKey.Error() + ": " + e.Bound
}

// Unwrap is ErrPodSessionBoundToAnotherKey.
func (e *PodSessionBoundError) Unwrap() error { return ErrPodSessionBoundToAnotherKey }

// ErrPodSessionReleased is what PodSessionRegistered's refusal (*PodSessionReleasedError) is: the
// pod's live registration for the session is one its registrar released.
var ErrPodSessionReleased = errors.New("the pod's registration for this session is released")

// PodSessionReleasedError is PodSessionRegistered's refusal of a live registration its registrar
// released. It is ErrPodSessionReleased (errors.Is), and Registrar is the registrar that wrote and
// released it, which the exchange logs and never answers: the pod asking is not that registrar's
// to tell about.
type PodSessionReleasedError struct{ Registrar string }

func (e *PodSessionReleasedError) Error() string {
	return ErrPodSessionReleased.Error() + " by its registrar, " + e.Registrar
}

// Unwrap is ErrPodSessionReleased.
func (e *PodSessionReleasedError) Unwrap() error { return ErrPodSessionReleased }

// ErrPodSessionHeld is what RegisterPodSession's refusal (*PodSessionHeldError) is: the session
// name is bound to another registrar, the daemon that first registered it, which holds it for
// good, whatever the name's live rows.
var ErrPodSessionHeld = errors.New("another registrar holds this session name")

// PodSessionHeldError is RegisterPodSession's refusal of a session name bound to another
// registrar: the one that first registered it, which holds it for good, whether its rows are
// live, lapsed, released or swept. It is ErrPodSessionHeld (errors.Is); Session is the name and
// Holder the registrar it is bound to, which the registration route names to the registrar it
// refuses.
type PodSessionHeldError struct{ Session, Holder string }

func (e *PodSessionHeldError) Error() string {
	return fmt.Sprintf("%s: %s holds %s", ErrPodSessionHeld, e.Holder, e.Session)
}

// Unwrap is ErrPodSessionHeld.
func (e *PodSessionHeldError) Unwrap() error { return ErrPodSessionHeld }

// ErrPodSessionNotThisRegistrars is what PodSessionRegistered's refusal
// (*PodSessionNotThisRegistrarsError) is: the pod's live registration for the session was written
// by a registrar other than the one the session name is bound to.
var ErrPodSessionNotThisRegistrars = errors.New("the pod's registration for this session is not the registrar's the name is bound to")

// PodSessionNotThisRegistrarsError is PodSessionRegistered's refusal of a live registration whose
// registrar is not the one its session name is bound to, or of a name bound to none. It is
// ErrPodSessionNotThisRegistrars (errors.Is). Session is the name, Registrar the registrar that
// wrote the registration, and Holder the registrar the name is bound to, "" when it is bound to
// none. The exchange logs each of them, Holder only when the name is bound, and answers only
// Session: neither daemon is the pod's to be told about.
type PodSessionNotThisRegistrarsError struct{ Session, Registrar, Holder string }

func (e *PodSessionNotThisRegistrarsError) Error() string {
	holder := e.Holder
	if holder == "" {
		holder = "no registrar"
	}
	return fmt.Sprintf("%s: %s wrote it, and %s is bound to %s", ErrPodSessionNotThisRegistrars, e.Registrar, e.Session, holder)
}

// Unwrap is ErrPodSessionNotThisRegistrars.
func (e *PodSessionNotThisRegistrarsError) Unwrap() error { return ErrPodSessionNotThisRegistrars }

// PodSessionRegistration is one pod's registration for one session name as the broker holds it
// after a write (RegisterPodSession), and what that write did to it.
type PodSessionRegistration struct {
	// When the broker stops honouring it unless it is written again.
	ExpiresAt time.Time
	// The thumbprint of the key it is bound to; empty for one bound to the pod alone.
	JKT string
	// The generation of the write that last set it; 0 for a registrar that sends none.
	Generation int64
	// Whether the write set it: false for a write of a lower generation than the row's, which
	// changed nothing.
	Applied bool
	// The thumbprint of the key the write took off the row: the key it held before this write
	// bound it to another, or before a write that took the row whole (RegisterPodSession) left it
	// bound to another key or to the pod alone. Empty when the write created the row, was not
	// applied, kept the key the row held (naming it, or naming no key of a row it did not take
	// whole), or bound a row that was bound to the pod alone.
	Replaced string
}

// RegisterPodSession records that the registrar registrar lets the pod podUID exchange under the
// session name session, until PodSessionRetention from now, and answers the registration as it
// now stands, with whether this write set it and the key it replaced. A pod uid already
// registered for the name has its registration extended rather than duplicated, so a registrar
// that writes the same row each time a pod starts, or renews it while the pod runs, keeps the pod
// writing and never shortens what another write left.
//
// It is what separates two pods of one listed account, which the exchange reads as one principal:
// the name belongs to the account, so nothing in the claim on the name can tell them apart, and
// without this row either pod could mint a token naming the other's session. A registration says
// only that this deployment started this pod under this name; who the name belongs to stays the
// claim's answer (BindSession).
//
// registrar is the login account of the daemon writing (the registration route's serviceAuth
// caller), and a session name is bound to the registrar that first registered it, for good,
// whatever its live rows: the write binds the name to registrar when no registrar is bound to it
// yet (bindPodSessionName, a row of pod_session_registrars), and a write of a name bound to
// another registrar is refused with a *PodSessionHeldError naming the name and that registrar,
// and writes nothing. No sweep deletes a binding and no route removes one, so the name stays its
// registrar's while its rows are live, once they have lapsed or been swept, and after its
// registrar released them. So two daemons listed for one pod account cannot each register a pod
// under one name, at once or one after the other, and the exchange, which admits a trade only
// for a live row that is not released and whose registrar is the one the name is bound to
// (PodSessionRegistered), admits under a name only the pods its registrar registered. The same
// registrar registering another pod for the name, as a relaunch does, is admitted. An empty
// registrar is refused, since a row naming none would admit nothing.
//
// A row of the pod and name that is released, that names another registrar, or that names none
// (one a broker of a build before registrars wrote: before this registrar bound the name, or after
// it, from a replica of that build while the broker rolls or from a broker rolled back to that
// build) is taken whole: its registrar, key, generation and expiry become this write's and its
// release mark is cleared. A release ends a registration, so neither its key nor its generation
// carries over to the next, whose claim may count its generations from lower; nor does another
// registrar's. A row of this registrar's own that it has not released, live or lapsed, keeps the
// rules below.
//
// jkt, when set, binds the registration to the key the session's own role proves, which is what
// separates the roles of one pod: they read one exchange token and are registered for one pod
// uid, so only the key tells a role's trade from a sibling's (PodSessionRegistered). A write that
// names a key replaces the one the row names, and one that names none keeps it, so a write
// without a thumbprint never removes a binding while the row is its registrar's; once the row
// lapses and the sweep deletes it, or a write takes it whole, that write stores what it names.
// Empty is a registration bound to the pod alone, as a session watch registers a session pod's
// one session, which runs no sibling role.
//
// generation is the registrar's own order of its writes for this pod and session: a write sets
// the expiry, the key and the generation only when its generation is at least the row's, and a
// write of a lower one changes nothing and is answered the row as it stands. The Legion daemon
// sends a role's launch generation, so a write its old incarnation sent, which the broker commits
// only after the new incarnation's, cannot put the old key back for a renewal interval. A
// registrar that sends none writes 0, as a session watch does, so its writes apply as before
// against rows that are 0 too. A registrar's generation for one pod and session must never go
// down: a lower one is not applied until the row has lapsed and the sweep has deleted it, or the
// registrar has released it.
//
// The write is one transaction. It first binds the name (bindPodSessionName), and its insert of
// the name's binding conflicts, on that table's primary key, with every other write's insert of
// it: of two registrars' first writes of one name, the second's insert waits for the first to
// commit, then does nothing, and the read that follows finds the first's registrar. Two first
// writes of two pods touch no row of oauth_pod_sessions in common, so that key is the one thing
// that keeps both from being admitted. It then holds the row from its read to its update
// (writePodSession), so the key it answers it replaced is the key the row held when this write
// changed it, and a write that waits on another reads the row as that one committed it. One
// insert ... on conflict do update statement keeps the generation rule as well, but the key it
// can read beside it is the statement's snapshot's, which a concurrent write's commit postdates:
// two writes changing one row's key at once name the key both started from. The transaction is
// its own and names read committed rather than taking default_transaction_isolation: the
// binding's read must see the binding the write it waited on committed, and two first writes of
// one pod under a bound name can both read no row, the second to insert waiting on the first's
// insert and then reading that row in a statement of its own; only read committed's snapshot per
// statement sees either. Under repeatable read each insert that waited fails to serialize instead
// (SQLSTATE 40001) once the write it waited on commits, as the one statement's update of a row
// committed after its snapshot does. So a ctx already carrying a transaction, whose level and
// lifetime the write would not choose, is refused; no caller passes one.
func (s *Store) RegisterPodSession(ctx context.Context, registrar, podUID, session, jkt string, generation int64) (PodSessionRegistration, error) {
	if registrar == "" {
		return PodSessionRegistration{}, errors.New("register the pod's session name: the write names no registrar, and a registration naming none admits nothing")
	}
	if _, joined := TxFrom(ctx); joined {
		return PodSessionRegistration{}, errors.New("register the pod's session name: the write opens a read-committed transaction of its own, and the context already carries one")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PodSessionRegistration{}, fmt.Errorf("register the pod's session name: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	txCtx := WithTx(ctx, tx)
	err = s.bindPodSessionName(txCtx, registrar, session)
	var stored PodSessionRegistration
	if err == nil {
		stored, err = s.writePodSession(txCtx, registrar, podUID, session, jkt, generation)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return PodSessionRegistration{}, fmt.Errorf("register the pod's session name: %w", err)
	}
	return stored, nil
}

// bindPodSessionName binds the session name to registrar, in the transaction ctx carries, when no
// registrar is bound to it yet, and refuses with a *PodSessionHeldError a name bound to another.
// The insert does nothing for a name already bound, so a binding never moves once written, and
// registered_at is the moment of the name's first registration. A first binding of the name that
// another write has inserted and not yet committed makes this insert wait on the primary key until
// that write commits or rolls back, and under read committed the read that follows, a statement
// of its own, sees what that write committed.
func (s *Store) bindPodSessionName(ctx context.Context, registrar, session string) error {
	if _, err := s.db(ctx).Exec(ctx, `insert into pod_session_registrars (session_name, registrar) values ($1, $2) on conflict (session_name) do nothing`, session, registrar); err != nil {
		return fmt.Errorf("bind the session name: %w", err)
	}
	var holder string
	err := s.db(ctx).QueryRow(ctx, `select registrar from pod_session_registrars where session_name = $1 and registrar <> $2`, session, registrar).Scan(&holder)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("read the registrar the session name is bound to: %w", err)
	}
	return &PodSessionHeldError{Session: session, Holder: holder}
}

// writePodSession is RegisterPodSession's write, in the transaction ctx carries, once
// bindPodSessionName has found the name bound to registrar. It locks the row and reads it,
// inserts it when there is none, takes it whole when it is released or not registrar's, and
// otherwise applies the generation rule to the row it locked and updates it. A row another write
// inserts after the read began is invisible to that read, but the insert waits for that write to
// commit and then finds the row, which is read again, locked, as that write left it.
func (s *Store) writePodSession(ctx context.Context, registrar, podUID, session, jkt string, generation int64) (PodSessionRegistration, error) {
	locked, found, err := s.lockPodSession(ctx, podUID, session)
	if err != nil {
		return PodSessionRegistration{}, err
	}
	expires := time.Now().Add(PodSessionRetention)
	if !found {
		stored := PodSessionRegistration{Applied: true}
		err := s.db(ctx).QueryRow(ctx, `insert into oauth_pod_sessions (pod_uid, session, expires_at, jkt, generation, registrar)
			values ($1, $2, $3, nullif($4, ''), $5, $6)
			on conflict (pod_uid, session) do nothing
			returning expires_at, coalesce(jkt, ''), generation`,
			podUID, session, expires, jkt, generation, registrar).Scan(&stored.ExpiresAt, &stored.JKT, &stored.Generation)
		switch {
		case err == nil:
			if s.testWriteHook != nil {
				s.testWriteHook(ctx)
			}
			return stored, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return PodSessionRegistration{}, err
		}
		// Another write inserted the row after the read above began.
		if locked, found, err = s.lockPodSession(ctx, podUID, session); err != nil {
			return PodSessionRegistration{}, err
		}
		if !found {
			return PodSessionRegistration{}, errors.New("the row another write inserted was deleted before this write read it")
		}
	}
	var row pgx.Row
	switch {
	case locked.released || locked.registrar != registrar:
		// A release ended the row's registration, or it is another registrar's or no registrar's
		// (RegisterPodSession says where a row of none comes from): nothing of it carries over, so
		// the write sets it as a first write would.
		row = s.db(ctx).QueryRow(ctx, `update oauth_pod_sessions
			set registrar = $3, expires_at = $4, jkt = nullif($5, ''), generation = $6, released_at = null
			where pod_uid = $1 and session = $2
			returning expires_at, coalesce(jkt, ''), generation`,
			podUID, session, registrar, expires, jkt, generation)
	case generation < locked.Generation:
		// The row's generation is higher, so the write changes nothing: answer the row as it is.
		return locked.PodSessionRegistration, nil
	default:
		row = s.db(ctx).QueryRow(ctx, `update oauth_pod_sessions
			set expires_at = greatest(expires_at, $3), jkt = coalesce(nullif($4, ''), jkt), generation = $5
			where pod_uid = $1 and session = $2
			returning expires_at, coalesce(jkt, ''), generation`,
			podUID, session, expires, jkt, generation)
	}
	stored := PodSessionRegistration{Applied: true}
	if err := row.Scan(&stored.ExpiresAt, &stored.JKT, &stored.Generation); err != nil {
		return PodSessionRegistration{}, err
	}
	if locked.JKT != "" && stored.JKT != locked.JKT {
		stored.Replaced = locked.JKT
	}
	return stored, nil
}

// lockedPodSession is a pod's registration for a session as writePodSession locked it: the
// registration, the registrar that wrote it ("" for a row that names none), and whether that
// registrar released it.
type lockedPodSession struct {
	PodSessionRegistration
	registrar string
	released  bool
}

// lockPodSession reads the pod's registration for session and locks it until the transaction ctx
// carries ends, first waiting for any write that holds it and reading the row as that write
// committed it; found is false when there is none.
func (s *Store) lockPodSession(ctx context.Context, podUID, session string) (locked lockedPodSession, found bool, err error) {
	err = s.db(ctx).QueryRow(ctx, `select expires_at, coalesce(jkt, ''), generation, coalesce(registrar, ''), released_at is not null
		from oauth_pod_sessions where pod_uid = $1 and session = $2 for update`, podUID, session).
		Scan(&locked.ExpiresAt, &locked.JKT, &locked.Generation, &locked.registrar, &locked.released)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedPodSession{}, false, nil
	}
	return locked, err == nil, err
}

// PodSessionRegistered reports whether a registration still says the pod podUID may exchange
// under session with a DPoP proof of the key whose thumbprint is jkt. It reads the pod's live row
// of the name beside the name's binding (pod_session_registrars, RegisterPodSession) and answers,
// in this order: false when no live row exists; false when the live row names no registrar,
// which a broker of the build before registrars wrote, so it is answered as no row is; false with
// a *PodSessionReleasedError naming the registrar when that registrar released the live row;
// false with a *PodSessionNotThisRegistrarsError when the live row's registrar is not the one the
// name is bound to, or the name is bound to none, so only a row of the daemon the name is bound
// to admits a trade, whichever daemon wrote the row; false with a *PodSessionBoundError naming
// the row's key when the live row names another; and true for a row that names no key or this
// one. It reads through s.db(ctx), so the exchange's own transaction asks it, and it asks the
// cluster nothing: a pod that no longer runs goes on being registered until its row runs out,
// exactly as its own token goes on verifying until its expiry.
func (s *Store) PodSessionRegistered(ctx context.Context, podUID, session, jkt string) (bool, error) {
	var bound, registrar, holder string
	var released bool
	err := s.db(ctx).QueryRow(ctx, `select coalesce(p.jkt, ''), coalesce(p.registrar, ''), p.released_at is not null, coalesce(b.registrar, '')
		from oauth_pod_sessions p left join pod_session_registrars b on b.session_name = p.session
		where p.pod_uid = $1 and p.session = $2 and p.expires_at > now()`, podUID, session).Scan(&bound, &registrar, &released, &holder)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the pod's session registration: %w", err)
	case registrar == "":
		return false, nil
	case released:
		return false, &PodSessionReleasedError{Registrar: registrar}
	case registrar != holder:
		return false, &PodSessionNotThisRegistrarsError{Session: session, Registrar: registrar, Holder: holder}
	case bound != "" && bound != jkt:
		return false, &PodSessionBoundError{Bound: bound}
	}
	return true, nil
}

// CredentialAccount is what a token minted from a credential records about it: the Google account
// it records, issuer then `sub`, empty for none, and when the credential was made, read from the
// broker database's own clock (the row's created_at, which its insert's default wrote). A resource
// server holds a token recording no account to that time (oauth.ClaimCredentialCreatedAt).
type CredentialAccount struct {
	Issuer, Subject string
	CreatedAt       time.Time
}

// CredentialApprover is what a token exchanged from the machine login credentialID names records
// about it: the Google account that approved the login (launcher_credentials' approver_issuer and
// approver_subject, migration 0040), as the approval Dispatch relayed named it, written once when
// the login was approved, and the login's created_at. The exchange reads it once, when it mints a
// token from that login's own, and writes it onto the token (oauth.Provider.approverAccount). The
// account is empty for a credential that records none, which is one made before 0040, one an older
// Dispatch approved, and every credential no person approved (a machine's own, a pod's); all of it
// is empty for a credential there is no row of.
func (s *Store) CredentialApprover(ctx context.Context, credentialID string) (CredentialAccount, error) {
	return s.credentialAccount(ctx, `select coalesce(approver_issuer, ''), coalesce(approver_subject, ''), created_at
		from launcher_credentials where id = $1::uuid`, credentialID)
}

// SignInCreatedAt is when the live sign-in of person on the box key thumbprint was made (the
// person_signins row's created_at), read in the trade's own transaction just after the trade wrote
// it (oauth.Provider.personSignIn), so a token the trade mints records it. The zero time for no
// live row.
func (s *Store) SignInCreatedAt(ctx context.Context, person, thumbprint string) (time.Time, error) {
	read, err := s.credentialAccount(ctx, `select '', '', created_at from person_signins
		where person = $1 and thumbprint = $2 and revoked_at is null`, person, thumbprint)
	return read.CreatedAt, err
}

// credentialAccount reads the one row query selects, issuer, subject and created_at, answering the
// zero CredentialAccount for no row.
func (s *Store) credentialAccount(ctx context.Context, query string, args ...any) (CredentialAccount, error) {
	var read CredentialAccount
	err := s.db(ctx).QueryRow(ctx, query, args...).Scan(&read.Issuer, &read.Subject, &read.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialAccount{}, nil
	}
	if err != nil {
		return CredentialAccount{}, fmt.Errorf("read what a token's credential records: %w", err)
	}
	return read, nil
}

// ReleasePodSessions marks released every live registration of each name in sessions that the
// registrar registrar wrote, and answers released: how many of registrar's live registrations of
// those names are released once it commits, by this call or an earlier one. It is what POST
// /v1/oauth/exchange/pod-sessions/release does for the daemon logged in as registrar, once that
// daemon has stopped writing the names it releases.
//
// The exchange refuses a released registration (PodSessionRegistered answers a
// *PodSessionReleasedError), and the name stays bound to registrar, as every name stays bound to
// the registrar that first registered it (RegisterPodSession), so no other registrar registers it
// after the release either. Nothing is deleted and no expiry moves, and the sweep deletes the row
// when it would have anyway. A row released before keeps the moment of its first release, so a
// repeat changes nothing and answers the same, and a retry after a lost answer is safe. Only
// registrar's own rows are released or counted: a row of the same name that another registrar,
// or none, wrote is neither touched nor revealed. registrar's next write of a released row's pod
// and name takes the row whole and makes that one registration live again (writePodSession).
//
// A name exchange.ValidSession refuses is one the registration route never wrote a row of, so it
// is dropped as matching nothing rather than refused, and a name given twice matches the same
// rows. With no name left it reads nothing and answers 0. The filter is also what keeps a NUL
// byte out of the query: JSON's \u0000 decodes to one, ValidSession refuses it as unprintable,
// and Postgres refuses it in text (invalid byte sequence for encoding "UTF8": 0x00), so without
// the filter a release naming it would fail, and the route would answer 500.
//
// It is one transaction of its own, the update then the count, and touches no binding: it inserts
// no row and moves no name, so it cannot race another registrar's first write of a name as two
// inserts of its binding can (bindPodSessionName), and a write of a row it updates locks that row
// (lockPodSession), so it waits for the release to commit and then finds the row released. The
// transaction names read committed, as RegisterPodSession's does, so that two releases of one
// name by one registrar both commit: the second's update waits on the first's rows, then finds
// them released and leaves them, and its count, a statement of its own, reads them as the first
// committed them; under repeatable read that update would fail to serialize instead (SQLSTATE
// 40001). So a ctx already carrying a transaction is refused; no caller passes one.
func (s *Store) ReleasePodSessions(ctx context.Context, registrar string, sessions []string) (released int, err error) {
	if _, joined := TxFrom(ctx); joined {
		return 0, errors.New("release the pod session names: the release opens a read-committed transaction of its own, and the context already carries one")
	}
	names := make([]string, 0, len(sessions))
	for _, session := range sessions {
		// More than a shortcut: it keeps a NUL byte, which Postgres refuses in text, out of the
		// query below, where it would fail the release.
		if exchange.ValidSession(session) {
			names = append(names, session)
		}
	}
	if len(names) == 0 {
		return 0, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("release the pod session names: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `update oauth_pod_sessions set released_at = now()
		where registrar = $1 and session = any($2) and expires_at > now() and released_at is null`, registrar, names)
	if err == nil {
		err = tx.QueryRow(ctx, `select count(*) from oauth_pod_sessions
			where registrar = $1 and session = any($2) and expires_at > now() and released_at is not null`, registrar, names).Scan(&released)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return 0, fmt.Errorf("release the pod session names: %w", err)
	}
	return released, nil
}

// SetTokenExchangeCustomJWT remembers a subject token's jti until exp plus jtiRetentionMargin and
// fails when it is already remembered, which is how the library is told a token is being
// presented a second time: it reads any error from here as a replay. The insert, or the takeover
// of a record whose token has expired, is one statement, so two exchanges presenting one token
// cannot both see it unused.
func (s *Store) SetTokenExchangeCustomJWT(ctx context.Context, jti string, exp time.Time) error {
	var recorded bool
	err := s.db(ctx).QueryRow(ctx, `insert into oauth_exchange_jtis (jti, expires_at) values ($1, $2)
		on conflict (jti) do update set expires_at = excluded.expires_at
		where oauth_exchange_jtis.expires_at <= now()
		returning true`, jti, exp.Add(jtiRetentionMargin)).Scan(&recorded)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("oauthstore: this subject token has been presented before")
	}
	if err != nil {
		return fmt.Errorf("record exchanged subject token: %w", err)
	}
	return nil
}

// GetSubjectForTokenExchange answers who the holder of a verified subject token is, for the token
// types whose subject the library cannot read off the token itself. The store decides nothing
// here: it calls ExchangeSubject, which the provider wiring sets to the exchange's own decision,
// and names it in whatever it refuses.
func (s *Store) GetSubjectForTokenExchange(ctx context.Context, request oauth2.Requester, subjectToken map[string]any) (string, error) {
	if s.ExchangeSubject == nil {
		return "", errors.New("oauthstore: the token exchange is composed without a subject decision (Store.ExchangeSubject)")
	}
	subject, err := s.ExchangeSubject(ctx, request, subjectToken)
	if err != nil {
		return "", fmt.Errorf("decide the subject token's subject: %w", err)
	}
	return subject, nil
}
