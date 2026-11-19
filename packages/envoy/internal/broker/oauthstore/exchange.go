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
}

// NewExchangeSession is an empty exchange session with its extra claims ready to be written, the
// shape the provider's session factory hands every request.
func NewExchangeSession() *ExchangeSession {
	return &ExchangeSession{DefaultSession: &oauth2.DefaultSession{Extra: map[string]any{}}}
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
		SubjectToken: maps.Clone(s.SubjectToken),
		ActorToken:   maps.Clone(s.ActorToken),
		Principal:    s.Principal,
		Instance:     s.Instance,
		Fleet:        s.Fleet,
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
	// The thumbprint of the key the write replaced with another. Empty when the write created the
	// row, was not applied, named no key, named the key the row held, or bound a row that was
	// bound to the pod alone.
	Replaced string
}

// RegisterPodSession records that the pod podUID may exchange under the session name session,
// until PodSessionRetention from now, and answers the registration as it now stands, with whether
// this write set it and the key it replaced. A pod uid already registered for the name has its
// registration extended rather than duplicated, so a registrar that writes the same row each time
// a pod starts, or renews it while the pod runs, keeps the pod writing and never shortens what
// another write left.
//
// It is what separates two pods of one listed account, which the exchange reads as one principal:
// the name belongs to the account, so nothing in the claim on the name can tell them apart, and
// without this row either pod could mint a token naming the other's session. A registration says
// only that this deployment started this pod under this name; who the name belongs to stays the
// claim's answer (BindSession).
//
// jkt, when set, binds the registration to the key the session's own role proves, which is what
// separates the roles of one pod: they read one exchange token and are registered for one pod
// uid, so only the key tells a role's trade from a sibling's (PodSessionRegistered). A write that
// names a key replaces the one the row names, and one that names none keeps it, so a write
// without a thumbprint never removes a binding while the row lives; once the row lapses and the
// sweep deletes it, a later write stores what it names. Empty is a registration bound to the pod
// alone, as a session watch registers a session pod's one session, which runs no sibling role.
//
// generation is the registrar's own order of its writes for this pod and session: a write sets
// the expiry, the key and the generation only when its generation is at least the row's, and a
// write of a lower one changes nothing and is answered the row as it stands. The Legion daemon
// sends a role's launch generation, so a write its old incarnation sent, which the broker commits
// only after the new incarnation's, cannot put the old key back for a renewal interval. A
// registrar that sends none writes 0, as a session watch does, so its writes apply as before
// against rows that are 0 too. A registrar's generation for one pod and session must never go
// down: a lower one is not applied until the row has lapsed and the sweep has deleted it.
//
// The write is one transaction that holds the row from its read to its update
// (writePodSession), so the key it answers it replaced is the key the row held when this write
// changed it, and a write that waits on another reads the row as that one committed it. One
// insert ... on conflict do update statement keeps the generation rule as well, but the key it
// can read beside it is the statement's snapshot's, which a concurrent write's commit postdates:
// two writes changing one row's key at once name the key both started from. The transaction is
// its own and names read committed rather than taking default_transaction_isolation: two first
// writes can both read no row, and the second to insert waits on the first's insert and then
// reads that row in a statement of its own, which only read committed's snapshot per statement
// sees. Under repeatable read that insert fails to serialize instead (SQLSTATE 40001), as the one
// statement's update of a row committed after its snapshot does. So a ctx already carrying a
// transaction, whose level and lifetime the write would not choose, is refused; no caller passes
// one.
func (s *Store) RegisterPodSession(ctx context.Context, podUID, session, jkt string, generation int64) (PodSessionRegistration, error) {
	if _, joined := TxFrom(ctx); joined {
		return PodSessionRegistration{}, errors.New("register the pod's session name: the write opens a read-committed transaction of its own, and the context already carries one")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PodSessionRegistration{}, fmt.Errorf("register the pod's session name: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stored, err := s.writePodSession(WithTx(ctx, tx), podUID, session, jkt, generation)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return PodSessionRegistration{}, fmt.Errorf("register the pod's session name: %w", err)
	}
	return stored, nil
}

// writePodSession is RegisterPodSession's write, in the transaction ctx carries. It locks the row
// and reads it, inserts it when there is none, and otherwise applies the generation rule to the
// row it locked and updates it. A row another write inserts after the read began is invisible to
// that read, but the insert waits for that write to commit and then finds the row, which is read
// again, locked, as that write left it.
func (s *Store) writePodSession(ctx context.Context, podUID, session, jkt string, generation int64) (PodSessionRegistration, error) {
	held, found, err := s.lockPodSession(ctx, podUID, session)
	if err != nil {
		return PodSessionRegistration{}, err
	}
	expires := time.Now().Add(PodSessionRetention)
	if !found {
		stored := PodSessionRegistration{Applied: true}
		err := s.db(ctx).QueryRow(ctx, `insert into oauth_pod_sessions (pod_uid, session, expires_at, jkt, generation)
			values ($1, $2, $3, nullif($4, ''), $5)
			on conflict (pod_uid, session) do nothing
			returning expires_at, coalesce(jkt, ''), generation`,
			podUID, session, expires, jkt, generation).Scan(&stored.ExpiresAt, &stored.JKT, &stored.Generation)
		switch {
		case err == nil:
			return stored, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return PodSessionRegistration{}, err
		}
		// Another write inserted the row after the read above began.
		if held, found, err = s.lockPodSession(ctx, podUID, session); err != nil {
			return PodSessionRegistration{}, err
		}
		if !found {
			return PodSessionRegistration{}, errors.New("the row another write inserted was deleted before this write read it")
		}
	}
	if generation < held.Generation {
		// The row's generation is higher, so the write changes nothing: answer the row as it is.
		return held, nil
	}
	stored := PodSessionRegistration{Applied: true}
	err = s.db(ctx).QueryRow(ctx, `update oauth_pod_sessions
		set expires_at = greatest(expires_at, $3), jkt = coalesce(nullif($4, ''), jkt), generation = $5
		where pod_uid = $1 and session = $2
		returning expires_at, coalesce(jkt, ''), generation`,
		podUID, session, expires, jkt, generation).Scan(&stored.ExpiresAt, &stored.JKT, &stored.Generation)
	if err != nil {
		return PodSessionRegistration{}, err
	}
	if held.JKT != "" && stored.JKT != held.JKT {
		stored.Replaced = held.JKT
	}
	return stored, nil
}

// lockPodSession reads the pod's registration for session and locks it until the transaction ctx
// carries ends, first waiting for any write that holds it and reading the row as that write
// committed it; found is false when there is none.
func (s *Store) lockPodSession(ctx context.Context, podUID, session string) (held PodSessionRegistration, found bool, err error) {
	err = s.db(ctx).QueryRow(ctx, `select expires_at, coalesce(jkt, ''), generation from oauth_pod_sessions
		where pod_uid = $1 and session = $2 for update`, podUID, session).Scan(&held.ExpiresAt, &held.JKT, &held.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return PodSessionRegistration{}, false, nil
	}
	return held, err == nil, err
}

// PodSessionRegistered reports whether a registration still says the pod podUID may exchange
// under session with a DPoP proof of the key whose thumbprint is jkt: true when a live row names
// no key or names this one, false when no live row exists, and false with a *PodSessionBoundError
// naming the row's key when the live row names another. It reads through s.db(ctx), so the
// exchange's own transaction asks it, and it asks the cluster nothing: a pod that no longer runs
// goes on being registered until its row runs out, exactly as its own token goes on verifying
// until its expiry.
func (s *Store) PodSessionRegistered(ctx context.Context, podUID, session, jkt string) (bool, error) {
	var bound string
	err := s.db(ctx).QueryRow(ctx, `select coalesce(jkt, '') from oauth_pod_sessions
		where pod_uid = $1 and session = $2 and expires_at > now()`, podUID, session).Scan(&bound)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the pod's session registration: %w", err)
	case bound != "" && bound != jkt:
		return false, &PodSessionBoundError{Bound: bound}
	}
	return true, nil
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
