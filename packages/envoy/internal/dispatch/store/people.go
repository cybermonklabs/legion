package store

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/oidc"
)

// RefreshTokenRevoker revokes a refresh token at the sign-in pool that issued it; *oidc.CodeFlow
// is the one Dispatch uses. Its error never carries the token.
type RefreshTokenRevoker interface {
	Revoke(ctx context.Context, refreshToken string) error
}

// PgPeopleStore records the people who have signed in to Dispatch and each sign-in's membership
// (migration 0068), its refresh token sealed (refreshTokenSeal), with the Google account that last
// signed in to the address for a Workspace domain (migration 0112).
type PgPeopleStore struct {
	pool    *Pool
	seal    refreshTokenSeal
	revoker RefreshTokenRevoker
	// brokerRevocationIssuer is the sign-in issuer, as oidc.CanonicalIssuer spells it, that a
	// sign-in replacing an address's recorded account records the replaced account under when it
	// records that Dispatch owes the secrets broker a revocation of that account; empty records
	// none (WithBrokerRevocations).
	brokerRevocationIssuer string
}

// PeopleOption configures a PgPeopleStore.
type PeopleOption func(*PgPeopleStore)

// WithBrokerRevocations makes a sign-in that replaces the account an address's row records, of
// either member kind, record that Dispatch owes the secrets broker a revocation of that account
// (recordBrokerRevocationSQL), named by issuer, the sign-in issuer every Google account it records
// is of (cmd/dispatch: DISPATCH_SIGNIN_ISSUER), and its `sub`. main gives it a store only where a
// broker is configured (DISPATCH_AGENT_SECRETS_URL), since only the loop of a process with a broker
// pays the debt. A revocation names the replaced account alone, so recording one is safe whenever
// it happened: the broker ends that account's credentials and those that record no account, and
// nobody else's. The gap is a fleet whose processes disagree: a replacement served by a process
// without DISPATCH_AGENT_SECRETS_URL records nothing, and no later pass revokes the replaced
// account at the broker, so every process of a deployment with a broker sets it.
func WithBrokerRevocations(issuer string) PeopleOption {
	return func(s *PgPeopleStore) { s.brokerRevocationIssuer = oidc.CanonicalIssuer(issuer) }
}

// NewPgPeopleStore returns a PeopleStore backed by pool that seals each refresh token under a key
// derived from the current key of keys, the keys session cookies are signed with
// (DISPATCH_SIGNING_KEY), opens one sealed under any of them, and retires one stored in plain text
// by revoking it through revoker, the sign-in pool's code flow; a nil revoker (no sign-in pool)
// revokes nothing, so such a token stays. keys must not be empty.
func NewPgPeopleStore(pool *Pool, keys auth.SigningKeys, revoker RefreshTokenRevoker, options ...PeopleOption) *PgPeopleStore {
	s := &PgPeopleStore{pool: pool, seal: newRefreshTokenSeal(keys), revoker: revoker}
	for _, option := range options {
		option(s)
	}
	return s
}

// Record notes that email has signed in, keeping any membership it already holds.
func (s *PgPeopleStore) Record(ctx context.Context, email string) error {
	if _, err := s.pool.Exec(ctx, `insert into people (email) values ($1) on conflict (email) do nothing`, email); err != nil {
		// The error reaches the sign-in callback's log line, so it names no person
		// (TestARecordFailureNamesNoPerson).
		return fmt.Errorf("record a person's sign-in: %w", err)
	}
	return nil
}

// retirePlainBeforeSignIn retires email's refresh token when the row holds it in plain text, so a
// sign-in that replaces it (SignIn or SignInSubject) never forgets one the pool still accepts.
// When the pool does not revoke it, this logs that at ERROR, naming the person to sign out at the
// pool since no later boot will see the token, and proceeds.
func (s *PgPeopleStore) retirePlainBeforeSignIn(ctx context.Context, email string) error {
	var stored *string
	err := s.pool.QueryRow(ctx, `select refresh_token from people where email = $1`, email).Scan(&stored)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read the refresh token the sign-in of %q replaces: %w", email, err)
	}
	if stored != nil && !strings.HasPrefix(*stored, sealedRefreshTokenPrefix) {
		switch err := s.retire(ctx, email, *stored); {
		case errors.Is(err, errNotRevoked):
			slog.Error("dispatch: a sign-in replaced a refresh token the pool did not revoke; sign this person out at the pool", "email", email, "error", err)
		case err != nil:
			return err
		}
	}
	return nil
}

// SignIn records a sign-in through a Cognito sign-in pool, replacing any membership email held
// before (auth.PeopleStore.SignIn), confirmed at the database's now(), as a full member; the stored
// subject is neither read nor written. A refresh token the row holds in plain text is retired first
// (retirePlainBeforeSignIn), so the replacement never forgets one the pool still accepts. It
// creates email's row when there is none and then locks it, as SignInSubject does, so it reads the
// member kind the row held and announces a change of it (announceMemberKindChange): a row that a
// Google sign-in recorded as restricted reads full from this commit.
func (s *PgPeopleStore) SignIn(ctx context.Context, email, refreshToken string) error {
	if err := s.retirePlainBeforeSignIn(ctx, email); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into people (email) values ($1) on conflict (email) do nothing`, email); err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	var storedKind string
	if err := tx.QueryRow(ctx, `select member_kind from people where email = $1 for update`, email).Scan(&storedKind); err != nil {
		return fmt.Errorf("read the member kind the sign-in of %q replaces: %w", email, err)
	}
	if _, err := tx.Exec(ctx, `
		update people
		set signed_in_at = now(), refresh_token = $2, confirmed_at = now(), member_kind = $3
		where email = $1
	`, email, s.seal.seal(email, refreshToken), string(model.MemberFull)); err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	if err := announceMemberKindChange(ctx, tx, storedKind, model.MemberFull); err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	return nil
}

// announceMemberKindChange announces on AccessChangedChannel, in tx, that a people row whose
// member kind read stored, under the row's lock, now reads written, when the two differ: what the
// person sees has moved (a restricted member reads only the projects that name them), so every
// process's room sync closes each document socket of theirs on a project they no longer read, and
// every live delivery hub reads their views' reader again. Postgres delivers it once tx commits,
// and never for a write that rolls back. A write that leaves the kind as it was announces nothing.
func announceMemberKindChange(ctx context.Context, tx pgx.Tx, stored string, written model.MemberKind) error {
	if stored == string(written) {
		return nil
	}
	return AnnounceAccessChange(ctx, tx)
}

// projectsSeenAs answers, in tx, every project email sees as a person of the member kind stored
// (PersonSeesProjectSQL), in key order, when written makes the person restricted from another
// kind, and none otherwise. A write that changes the member kind a people row holds reads it under
// the row's lock and before its update, and then hands it to RecordReachedProjects with email,
// after its update, so every session tied to email keeps each of those projects the kind written
// no longer sees: a full member a write makes restricted (no sign-in rule writes one now) loses
// every public project and every one a group admitted them to, unless it names them, and their
// sessions' transcripts keep what
// they read of each. A write that makes a restricted member full takes nothing from them, since a
// full member sees every project a restricted one does (PersonSeesProjectSQL's arms), so it reads
// nothing, and RecordReachedProjects, handed no project, takes no lock and writes nothing.
func projectsSeenAs(ctx context.Context, tx pgx.Tx, email, stored string, written model.MemberKind) ([]string, error) {
	if stored == string(written) || written != model.MemberRestricted {
		return nil, nil
	}
	var seen []string
	if err := tx.QueryRow(ctx, `select array(select p.key from projects p where `+
		PersonSeesProjectSQL("$1::text", "$2::text", "p")+` order by p.key collate "C")`, email, stored).Scan(&seen); err != nil {
		return nil, fmt.Errorf("read the projects %q sees before its member kind changes: %w", email, err)
	}
	return seen, nil
}

// signInMemberKind refuses a member kind a sign-in cannot record: only model.MemberFull and
// model.MemberRestricted name a person who signed in.
func signInMemberKind(email string, kind model.MemberKind) error {
	if kind != model.MemberFull && kind != model.MemberRestricted {
		return fmt.Errorf("record sign-in of %q: member kind %q is neither %q nor %q", email, kind, model.MemberFull, model.MemberRestricted)
	}
	return nil
}

// SignInSubject records a Google sign-in in one transaction (auth.PeopleStore.SignInSubject),
// replacing any membership email held before and confirmed at the database's now(), as kind, the
// member kind the sign-in rule named, refused before any write unless it is full or restricted; a
// refresh token the row holds in plain text is retired first (retirePlainBeforeSignIn), as
// SignIn's. It takes the address's advisory lock first (addressLockSQL), which a mint and a
// machine login's decision take shared before they read whether their caller still holds the
// address (AddressLockSharedSQL), so either waits for this sign-in to commit and then reads what
// it recorded, on an address with no row too, where a row lock would find nothing to wait on. It
// creates email's row when there is none and then locks it, so two sign-ins to one address take
// turns, two first sign-ins included: the second waits on the first, then reads the subject it
// committed. Whenever the account the row records changes, a fresh row's first recording included,
// the database ends every session of email in the same transaction (migration 20261118041916's
// people_account_ends_sessions and people_first_account_ends_sessions, which advance
// user_sessions.generation): a row recording no Google account (a Cognito-era, header- or
// dev-recorded row, or one some statement deleted and this sign-in recreates) may hold a cookie
// from before a switch to domain mode, and a replaced account's cookies are the replaced account's.
// When the row recorded another Google account, which this sign-in replaces, it also revokes every
// personal token of email (revokeAddressTokensSQL), whatever member kind the row held and the
// sign-in records, so nothing the replaced account signed in or minted acts as the new one. When
// it records the address's first account (a row recording none, or no row at all), it revokes the
// address's personal tokens that recorded another account at their mint, or none
// (revokeOtherAccountsTokensSQL): an account that held the address at the broker and never signed
// in to Dispatch, whose tokens record it (its broker token minted them, before no broker token
// held the approve scope a mint needs), and a credential bound to no account, whose tokens predate
// this recording; those that recorded this one stand. When the store owes
// broker revocations (WithBrokerRevocations: a broker is configured), it also records that
// Dispatch owes the secrets broker a revocation of the replaced account, whatever member kind the
// row held, and on a first recording one of each account whose token it revoked
// (recordBrokerRevocationSQL, its last statements): the broker ends every credential the
// replaced `sub` holds under email, and every one that records no account, whenever issued,
// refuses it any new one, leaves subject's, and deletes any record that subject was itself an
// account the address was taken from, which only a call made after this commit can do
// (BrokerRevocations, the server's revocation loop). A restricted member's account is revoked as a
// full member's is: it makes broker credentials too (the machine logins it approves), and the
// broker may still refuse subject for a revocation an earlier sign-in recorded and a pass paid,
// which only a revoke naming subject as the holder lifts. Dispatch itself refuses the replaced
// account's credentials from the commit, and holds those recording no account to when
// they were made against the time this sign-in records (api.brokerPersonAdmitted,
// api.tokenAccountReplaced); the new holder's own tokens, which record its account, act throughout.
// Whenever it records a revocation, it first settles every revocation of email still owed for
// subject itself (settleReclaimedBrokerRevocationsSQL): an account that takes the address back is
// owed nothing, so no pass ends what it makes once it holds the address again. When kind is not the
// member kind the row held under its lock, it announces the change in the same transaction
// (announceMemberKindChange), and keeps on every session tied to email each project the kind the
// row held saw and kind does not (projectsSeenAs, RecordReachedProjects): a session of a full
// member it restricts may have read such a project, and neither that member nor any other viewer
// who does not see the project watches it after (access.SessionContentClause). A revoke that fails
// rolls the sign-in back, leaving the old subject for the next sign-in to revoke again; one that
// commits is logged at WARN with the address, and with how many tokens it revoked when it replaced
// an account or revoked another account's tokens. The sign-in records hostedDomain, the ID token's
// `hd`, in hosted_domain (null for none), and sets held_since and signed_out_at to null, so it
// releases a recorded hold and ends a sign-out that kept the refresh token.
//
// All of that revoking and owing is the account binding's, and it runs only while the binding
// holds over the fleet (AccountBindingHolds, read under the address's lock, which keeps the
// binding's gate to the commit): while a build without the binding is in the fleet, which revokes
// nothing when another account signs in, the sign-in records the account and ends the sessions as
// that build does (the database's triggers end them, and stamp sub_recorded_at, whichever build
// changes sub) and revokes and owes nothing, logged at WARN with the account it replaced, so no
// revocation stands on a replacement that build would have recorded without it. Such a sign-in
// keeps its change, a replacement or the address's first recording, in its own transaction
// (keepAccountChangeSQL, account_binding_replacements, migration 20261121121717), by the reading
// it revoked nothing on, so the binding catches it up once it holds. It marks its transaction
// (markBindingWriterSQL) before it writes sub, so the database's trigger, which keeps every change
// a writer without the mark makes (a build without the binding's, whatever the fleet reads), keeps
// nothing for it. A sign-in that finds the binding holding catches the address up first
// (CatchUpAccountBinding, logged at WARN: its kept changes, and the tokens the binding refuses),
// then revokes and owes its own change itself.
//
// A restricted sign-in (a person from outside the domain) never takes a full member's address
// (takesAMembersAddress): it writes nothing and returns auth.ErrAddressHeldByMember, read once
// before the plain token's retirement (refuseAMembersAddress), so the member's token is never
// revoked, and again under the row's lock, so a member's sign-in that commits in between is
// refused too. A full sign-in reads and writes what it always did.
func (s *PgPeopleStore) SignInSubject(ctx context.Context, email, subject, refreshToken, hostedDomain string, kind model.MemberKind) error {
	if err := signInMemberKind(email, kind); err != nil {
		return err
	}
	if err := s.refuseAMembersAddress(ctx, email, subject, kind); err != nil {
		return err
	}
	if err := s.retirePlainBeforeSignIn(ctx, email); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	defer tx.Rollback(ctx)
	sealed := s.seal.seal(email, refreshToken)
	recorded, err := s.signInAccountTx(ctx, tx, email, signInAccount{subject: subject, hostedDomain: hostedDomain}, &sealed, kind)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	recorded.log(email)
	return nil
}

// signInAccount is the account a sign-in records for an address: issuer, the issuer of its `sub`
// (people.sub_issuer), empty for the deployment's sign-in issuer (DISPATCH_SIGNIN_ISSUER, which
// WithBrokerRevocations names) and the outside origin for a passkey's subject; subject, the
// account's `sub`, empty for none; and hostedDomain, the Google ID token's `hd`, empty for none
// and for a passkey's.
type signInAccount struct {
	issuer, subject, hostedDomain string
}

// signInRecorded is what a sign-in's transaction did that the commit logs (signInAccountTx):
// revoked is whether it revoked and owed its own change (signInAccountTx's revokesNow), stored the
// account it replaced, and caught the catch-up it ran first.
type signInRecorded struct {
	existed, replaced, firstRecording, revoked bool
	tokensRevoked                              int64
	owed                                       int
	stored, subject                            string
	caught                                     AccountBindingCatchUp
}

// log logs, once the sign-in's transaction committed, what it caught up, ended and revoked, in the
// words the README's account-binding section quotes (a passkey's account included, so the log has
// one line for each event whoever's account it is).
func (r signInRecorded) log(email string) {
	if r.caught.Found() {
		LogAccountBindingCatchUp(email, r.caught)
	}
	switch {
	case !r.revoked && r.replaced:
		slog.Warn("dispatch: a sign-in under another Google account ended every session of the address and revoked nothing: a build without the account binding is in the fleet", "email", email, "replaced_subject", r.stored)
	case !r.revoked && r.firstRecording:
		slog.Warn("dispatch: the first Google account recorded for the address revoked nothing of it: a build without the account binding is in the fleet", "email", email, "subject", r.subject)
	case r.replaced:
		slog.Warn("dispatch: a sign-in under another Google account ended every session and revoked every personal token of the address", "email", email, "tokens_revoked", r.tokensRevoked)
	case r.tokensRevoked > 0:
		slog.Warn("dispatch: the first Google account recorded for the address revoked every personal token of it that records another account or none", "email", email, "tokens_revoked", r.tokensRevoked, "accounts", r.owed)
	case r.existed && r.firstRecording:
		// A row this sign-in's own insert created had no cookie of its own to end, so only a row
		// that existed is logged; the database ended the sessions either way.
		slog.Warn("dispatch: a sign-in under another Google account ended every session of the address", "email", email)
	}
}

// signInAccountTx is the account binding of a sign-in, in tx (SignInSubject, and a passkey's
// enrollment and sign-in, PgPasskeys): it takes the address's advisory lock, reads whether the
// binding holds over the fleet (AccountBindingHolds), creates and locks the address's row, refuses
// a restricted sign-in that would take a full member's address (auth.ErrAddressHeldByMember),
// catches the address up when the binding holds (CatchUpAccountBinding), revokes what the account
// it replaces or the address's first recording revokes, writes the sign-in (account, member kind,
// confirmation, hosted domain, and sealed, the sealed refresh token, nil for a passkey's sign-in,
// which holds none) and records the broker revocations it owes, each of the replaced account under
// that account's own issuer (coalesce(people.sub_issuer, the deployment's)); a change it does not
// revoke now it keeps for the catch-up (keepAccountChangeSQL). The caller commits and then logs
// what it did.
func (s *PgPeopleStore) signInAccountTx(ctx context.Context, tx pgx.Tx, email string, account signInAccount, sealed *string, kind model.MemberKind) (signInRecorded, error) {
	subject := account.subject
	if _, err := tx.Exec(ctx, addressLockSQL, email); err != nil {
		return signInRecorded{}, fmt.Errorf("record sign-in of %q: lock the address: %w", email, err)
	}
	binding, err := AccountBindingHolds(ctx, tx)
	if err != nil {
		return signInRecorded{}, fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	inserted, err := tx.Exec(ctx, `insert into people (email) values ($1) on conflict (email) do nothing`, email)
	if err != nil {
		return signInRecorded{}, fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	var stored, storedIssuer, storedKind string
	if err := tx.QueryRow(ctx, `select coalesce(sub, ''), coalesce(sub_issuer, ''), member_kind from people where email = $1 for update`, email).Scan(&stored, &storedIssuer, &storedKind); err != nil {
		return signInRecorded{}, fmt.Errorf("read the subject the sign-in of %q replaces: %w", email, err)
	}
	recorded := signInRecorded{existed: inserted.RowsAffected() == 0, stored: stored, subject: subject}
	if recorded.existed && takesAMembersAddress(kind, stored, storedKind, subject) {
		return signInRecorded{}, fmt.Errorf("record sign-in of %q: %w", email, auth.ErrAddressHeldByMember)
	}
	// An account is its issuer and its sub: the same sub under another issuer is another account.
	recorded.replaced = stored != "" && (stored != subject || storedIssuer != account.issuer)
	recorded.firstRecording = stored == "" && subject != ""
	// The account binding's gate, and the one place a passkey's account leaves it. A change between
	// two accounts of the sign-in issuer (Google's) revokes and owes only while the binding holds over
	// the fleet; while a build without the binding is in it, the change is kept for the catch-up, as
	// SignInSubject's doc says. A change whose new or old account is a passkey's (account.issuer or
	// storedIssuer set: a first enrollment, the owner's Reset, the end at the last list, a passkey's
	// sign-in replacing a stale account, or a Google sign-in replacing a passkey's account) revokes
	// and owes at once whatever the binding reads, through the same statements the binding runs when
	// it holds, the old account named under its own sub_issuer. Why: the gate is availability and
	// exactly-once bookkeeping, not a security boundary (the binding's owner), and revoking at once
	// only takes power away; no build without the binding has passkey code, so none makes, sees or
	// admits such an account differently; and the catch-up names every kept change under the
	// deployment's issuer (account_binding_replacements keeps no issuer), so it would owe a passkey's
	// account under the wrong one. The transaction is marked a writer that knows the binding either
	// way (markBindingWriterSQL, per transaction), so the trigger keeps nothing for this change and a
	// later catch-up owes nothing more for it, while an unmarked change by a build without the
	// binding, to the same address in the same window, is still kept and caught up. Its limit: the
	// catch-up owes the account that unmarked change replaced under the deployment's issuer, even
	// where that account was a passkey's, since account_binding_replacements keeps no issuer
	// (TestTheCatchUpOwesExactlyTheUnmarkedChangeBesideAMarkedPasskeyChange shows it). No build
	// without the binding signs a person from outside the Workspace in, so it has no Google sign-in
	// to make at such an address, but a write that did would be owed under the wrong issuer.
	revokesNow := binding || account.issuer != "" || storedIssuer != ""
	recorded.revoked = revokesNow
	if binding {
		if recorded.caught, err = CatchUpAccountBinding(ctx, tx, email, s.brokerRevocationIssuer); err != nil {
			return signInRecorded{}, err
		}
	}
	var owed []string
	switch {
	case revokesNow && recorded.replaced:
		tokens, err := tx.Exec(ctx, revokeAddressTokensSQL, email)
		if err != nil {
			return signInRecorded{}, fmt.Errorf("revoke the personal tokens of %q, which another account signs in to now: %w", email, err)
		}
		recorded.tokensRevoked = tokens.RowsAffected()
		owed = []string{stored}
	case revokesNow && recorded.firstRecording:
		if recorded.tokensRevoked, owed, err = revokeOtherAccountsTokens(ctx, tx, email, subject); err != nil {
			return signInRecorded{}, err
		}
	}
	// An account of another issuer than the one now signing in held the address: a passkey's
	// account (people.sub_issuer is the outside origin) that a Google account replaces, or a
	// passkey's under another outside origin. Its passkeys go with it, whatever the binding reads
	// (no catch-up deletes them), so none signs in again and none counts as another owner's passkey
	// of the address (HeldByAnotherOwnersPasskey, ProjectNamesAnotherOwnersPasskey). An enrollment
	// and a passkey's sign-in lock its passkey rows after the address, as this does.
	if recorded.replaced && storedIssuer != "" && storedIssuer != account.issuer {
		if _, err := tx.Exec(ctx, `delete from passkeys where email = $1`, email); err != nil {
			return signInRecorded{}, fmt.Errorf("delete the passkeys of the account that %q's sign-in replaces: %w", email, err)
		}
	}
	recorded.owed = len(owed)
	seen, err := projectsSeenAs(ctx, tx, email, storedKind, kind)
	if err != nil {
		return signInRecorded{}, err
	}
	// Changing sub stamps people.sub_recorded_at from the database's own clock, the address's first
	// recording included, and ends every session of the address (the triggers of migrations
	// 20261117175133 and 20261118041916, which no statement can bypass): a credential recording no
	// account made before it is refused as the person, and no cookie issued before it acts. A
	// sign-in under the account the row already records leaves both, and its issuer
	// (20261120165538's rule: sub_issuer moves only with sub). The mark first, so the trigger that
	// keeps a change of a writer without the binding keeps nothing for this one, which keeps or
	// revokes its own change below.
	if _, err := tx.Exec(ctx, markBindingWriterSQL); err != nil {
		return signInRecorded{}, fmt.Errorf("record sign-in of %q: mark the transaction a writer that knows the account binding: %w", email, err)
	}
	if _, err := tx.Exec(ctx, `
		update people
		set signed_in_at = now(), refresh_token = $2, confirmed_at = now(), sub = $3, member_kind = $4,
			sub_issuer = nullif($5, ''), hosted_domain = nullif($6, ''), held_since = null, signed_out_at = null
		where email = $1
	`, email, sealed, subject, string(kind), account.issuer, account.hostedDomain); err != nil {
		return signInRecorded{}, fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	if err := announceMemberKindChange(ctx, tx, storedKind, kind); err != nil {
		return signInRecorded{}, fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	if !revokesNow && (recorded.replaced || recorded.firstRecording) {
		if _, err := tx.Exec(ctx, keepAccountChangeSQL, email, stored); err != nil {
			return signInRecorded{}, fmt.Errorf("record sign-in of %q: keep the account change it revokes and owes nothing for: %w", email, err)
		}
	}
	// After the address's lock and the row's, as the access package's writes take it after theirs.
	if err := RecordReachedProjects(ctx, tx, seen, []string{email}); err != nil {
		return signInRecorded{}, fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	if len(owed) > 0 && s.brokerRevocationIssuer != "" {
		holderIssuer := s.revocationIssuer(account.issuer)
		if _, err := tx.Exec(ctx, settleReclaimedBrokerRevocationsSQL, email, holderIssuer, subject); err != nil {
			return signInRecorded{}, fmt.Errorf("settle the broker revocations of %q owed for the account that takes it back: %w", email, err)
		}
		// The replaced account is named under its own issuer; an account a first recording owes a
		// revocation of is one a personal token recorded, made under the deployment's.
		owedIssuer := s.brokerRevocationIssuer
		if recorded.replaced {
			owedIssuer = s.revocationIssuer(storedIssuer)
		}
		// Last, so the moment each records is as close to the commit as a statement can read it:
		// the order a later replacement of the address is paid after.
		for _, replaced := range owed {
			if _, err := tx.Exec(ctx, recordBrokerRevocationSQL, email, owedIssuer, replaced, subject); err != nil {
				return signInRecorded{}, fmt.Errorf("record the broker revocation of %q, which another account signs in to now: %w", email, err)
			}
		}
	}
	return recorded, nil
}

// revocationIssuer is the issuer a broker revocation names an account of people.sub_issuer issuer
// under: the deployment's sign-in issuer for none (WithBrokerRevocations), else issuer itself.
func (s *PgPeopleStore) revocationIssuer(issuer string) string {
	if issuer == "" {
		return s.brokerRevocationIssuer
	}
	return issuer
}

// LogAccountBindingCatchUp logs, at WARN, a catch-up of what the account binding did not revoke at
// email while it was off (CatchUpAccountBinding), by a sign-in or by the revocation loop: the
// accounts its kept changes replaced, whether one was the address's first recording, whether a
// personal token the binding refuses outlived the address's recording, how many personal tokens it
// revoked, the accounts it owes the broker a revocation of, and the holder.
func LogAccountBindingCatchUp(email string, caught AccountBindingCatchUp) {
	slog.Warn("dispatch: the account binding holds and caught up what it did not revoke at the address while it was off: it revoked the personal tokens of the address a sign-in with the binding on would have, and owes the broker a revocation of each account they replaced",
		"email", email, "replaced_subjects", caught.Replaced, "first_recording", caught.FirstRecording, "refused_live", caught.RefusedLive,
		"tokens_revoked", caught.TokensRevoked, "owed", caught.Owed, "holder", caught.Holder)
}

// addressLockSQL takes the advisory lock on $1, an address, in the form people.email holds it
// (canonical_login), for the transaction: SignInSubject takes it before it reads or writes the
// address's row, so a reader that takes it shared (AddressLockSharedSQL) waits for a sign-in in
// flight and then reads what it committed. A row lock cannot stand in for it: a first sign-in's
// row is its own insert, which no other transaction sees until it commits, so a `for share` on it
// finds no row and waits for nothing.
const addressLockSQL = `select pg_advisory_xact_lock(hashtext('dispatch-person-address'), hashtext(canonical_login($1)))`

// AddressLockSharedSQL takes addressLockSQL's lock shared, for the transaction: what a personal
// token's mint and a machine login's decision take before they read whether their caller still
// holds the address (api.createAgentToken, api.approverStanding), so a credential a sign-in is
// replacing, or that records no account and was made before the account the sign-in records, is
// read after that sign-in commits and refused, never minted for or relayed while it is in flight.
// Readers do not wait for each other.
const AddressLockSharedSQL = `select pg_advisory_xact_lock_shared(hashtext('dispatch-person-address'), hashtext(canonical_login($1)))`

// revokeAddressTokensSQL revokes every personal token (agent_tokens) of $1, an address as
// people.email holds it, that is not revoked yet, stamping revoked_at as a token's own revoke does
// (DELETE /api/v1/me/agent-tokens/{id}), so each is 401 TOKEN_REVOKED from then on; its
// RowsAffected is how many it revoked. A token's owner is compared in the form people.email holds,
// model.CanonicalLogin's (canonical_login, migration 20261117000831: every Unicode space trimmed,
// A-Z lowered and nothing else folded), so a token whose owner some writer stored in another case,
// or wrapped in a tab or a no-break space, is the address's too. That comparison can read the
// address's own tokens through agent_tokens_canonical_owner (migration 20261119020200), which
// agent_tokens_owner_created_at, on the owner as stored, cannot serve.
const revokeAddressTokensSQL = `
	update agent_tokens set revoked_at = now()
	where canonical_login(owner) = $1 and revoked_at is null
`

// otherAccountsSQL is every Google account other than $2 that a personal token of $1, an address as
// people.email holds it, recorded at its mint, a revoked token's included, each once and in the
// order of its first token: every account Dispatch knows held the address before $2, the account
// the address's first Google sign-in records (PgPeopleStore.SignInSubject). A token its owner
// revoked is as much evidence the account held the address as a live one, and its broker
// credentials may still be live. Owners are compared as revokeAddressTokensSQL compares them.
const otherAccountsSQL = `
	select subject from agent_tokens
	where canonical_login(owner) = $1 and subject is not null and subject <> $2
	group by subject order by min(created_at), subject
`

// revokeOtherAccountsTokensSQL revokes every personal token of $1 that is not revoked yet and
// recorded an account other than $2, or recorded none, stamping revoked_at as
// revokeAddressTokensSQL does. It reads the address's tokens as that does, only on the address's
// first recording, under the people row's lock.
//
// A token recording no account was minted by a credential bound to none, before this recording,
// so it is one of the accounts that held the address before $2 as much as a token naming one.
// Until it was revoked here, what refused it was api.tokenAccountReplaced's
// `agent_tokens.created_at < people.sub_recorded_at`: the mint's now() against the recording's
// clock_timestamp(), two readings of one clock with no margin, so a backward step of the
// database's clock larger than the time between them made the token read as minted after the
// recording, and it acted as the account recorded here. Revoking it needs no clock. Under a clock
// that only moves forward this changes nothing: every such token predates the recording and was
// already refused.
const revokeOtherAccountsTokensSQL = `
	update agent_tokens set revoked_at = now()
	where canonical_login(owner) = $1 and revoked_at is null and subject is distinct from $2
`

// revokeOtherAccountsTokens reads in tx every other account a token of email recorded
// (otherAccountsSQL), revokes the live tokens of those accounts and of every token of email that
// records none (revokeOtherAccountsTokensSQL), and answers how many it revoked and the accounts,
// each of which SignInSubject owes the broker a revocation of. A token recording no account names
// no account to revoke at the broker, so it adds nothing to that list; the broker has no
// credential of it either, since a credential the broker holds records the account that approved
// it or made it.
func revokeOtherAccountsTokens(ctx context.Context, tx pgx.Tx, email, subject string) (int64, []string, error) {
	rows, err := tx.Query(ctx, otherAccountsSQL, email, subject)
	if err != nil {
		return 0, nil, fmt.Errorf("read the Google accounts other personal tokens of %q recorded: %w", email, err)
	}
	accounts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, nil, fmt.Errorf("read the Google accounts other personal tokens of %q recorded: %w", email, err)
	}
	revoked, err := tx.Exec(ctx, revokeOtherAccountsTokensSQL, email, subject)
	if err != nil {
		return 0, nil, fmt.Errorf("revoke the personal tokens of %q that another Google account minted: %w", email, err)
	}
	return revoked.RowsAffected(), accounts, nil
}

// takesAMembersAddress reports whether a sign-in of subject as kind would take an address whose
// existing row records a member of storedKind under stored, empty for no Google account: a
// restricted sign-in to a full member's row under another account or none. Such an address is a
// member's of the Workspace, often on another domain it owns, which a person from outside can be
// listed at; were it taken, that person would end the member's sessions and read every project
// that names the member.
func takesAMembersAddress(kind model.MemberKind, stored, storedKind, subject string) bool {
	return kind == model.MemberRestricted && storedKind == string(model.MemberFull) && stored != subject
}

// refuseAMembersAddress is auth.ErrAddressHeldByMember when a restricted sign-in of subject would
// take email's row (takesAMembersAddress), read with no lock: SignInSubject asks it before it
// retires a refresh token the row holds in plain text, which would revoke the member's. A full
// sign-in reads nothing here.
func (s *PgPeopleStore) refuseAMembersAddress(ctx context.Context, email, subject string, kind model.MemberKind) error {
	if kind != model.MemberRestricted {
		return nil
	}
	var stored, storedKind string
	err := s.pool.QueryRow(ctx, `select coalesce(sub, ''), member_kind from people where email = $1`, email).Scan(&stored, &storedKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the member the sign-in of %q replaces: %w", email, err)
	}
	if takesAMembersAddress(kind, stored, storedKind, subject) {
		return fmt.Errorf("record sign-in of %q: %w", email, auth.ErrAddressHeldByMember)
	}
	return nil
}

// Membership reads email's membership; found is false for a person who never signed in. The
// confirmation's age is the database's: now() minus confirmed_at, both on its clock, so no server
// process's clock takes part and every process judges a confirmation alike. A refresh token that
// does not open (sealed under no key of the signing key set, for another person, or stored in plain
// text) is no refresh token: the read answers none, so the person signs in again, and logs that
// without the value; it answers Unopenable, with the confirmation's age, so the unopenable rule
// can judge the row. The read writes nothing: the next boot retires a token stored in plain text
// (RetirePlainRefreshTokens), and the person's next sign-in replaces whatever the row holds. The
// subject, the member kind, the recorded hold (held_since later than signed_in_at), the sign-out
// that kept the token (signed_out_at later than signed_in_at), the hosted domain, signed_in_at and
// the Directory columns (DirectoryHeld: held_since later than directory_confirmed_at, or set where
// that is null) are read whatever the refresh token holds. ReadAt is the database's now() in the
// one statement, read for a person with no row too, so an answer that rests on the read can carry
// its time. A passkey's row (its account of another issuer, people.sub_issuer) holds no refresh
// token: it is confirmed while confirmed_at holds a time (people_sign_in_stands, migration
// 20261120165538).
func (s *PgPeopleStore) Membership(ctx context.Context, email string) (auth.PersonMembership, bool, error) {
	var found bool
	var sealed *string
	var age *time.Duration
	var signedInAt *time.Time
	var kind string
	membership := auth.PersonMembership{}
	err := s.pool.QueryRow(ctx, `
		select p.email is not null, p.refresh_token, now() - p.confirmed_at, coalesce(p.sub, ''),
			coalesce(p.sub_issuer, ''), coalesce(p.member_kind, ''), coalesce(p.held_since > p.signed_in_at, false),
			coalesce(p.signed_out_at > p.signed_in_at, false), coalesce(p.hosted_domain, ''),
			p.signed_in_at, now(),
			coalesce(p.held_since > p.directory_confirmed_at, p.held_since is not null),
			p.directory_confirmed_at, p.not_held_until, coalesce(p.directory_user_id, '')
		from (select 1) as one
		left join people p on p.email = $1
	`, email).Scan(&found, &sealed, &age, &membership.Subject, &membership.Issuer, &kind, &membership.Held,
		&membership.SignedOut, &membership.HostedDomain, &signedInAt, &membership.ReadAt,
		&membership.DirectoryHeld, &membership.DirectoryConfirmedAt, &membership.NotHeldUntil, &membership.DirectoryUserID)
	if err != nil {
		return auth.PersonMembership{}, false, fmt.Errorf("read membership of %q: %w", email, err)
	}
	if !found {
		return auth.PersonMembership{ReadAt: membership.ReadAt}, false, nil
	}
	membership.Kind = model.MemberKind(kind)
	if signedInAt != nil {
		membership.SignedInAt = *signedInAt
	}
	if membership.Issuer != "" {
		if age != nil {
			membership.Confirmed, membership.ConfirmedAge = true, *age
		}
		return membership, true, nil
	}
	// people's check pairs the two for an account of the sign-in issuer: a row holds both or
	// neither.
	if sealed == nil {
		return membership, true, nil
	}
	refreshToken, err := s.seal.open(email, *sealed)
	if err != nil {
		slog.Warn("dispatch: a stored refresh token did not open; the person signs in again", "email", email, "reason", err.Error())
		membership.Unopenable, membership.ConfirmedAge = true, *age
		return membership, true, nil
	}
	membership.RefreshToken, membership.Confirmed, membership.ConfirmedAge = refreshToken, true, *age
	return membership, true, nil
}

// ConfirmSubject records a refresh that confirmed email's membership at the database's now(), as
// kind, with refreshToken to use next and hostedDomain, the refreshed ID token's `hd`, in
// hosted_domain (null for none), refused as SignInSubject refuses it
// (auth.PeopleStore.ConfirmSubject): a Google sign-in's under its subject, and a sign-in pool's
// under the empty subject, since a pool's row records none. It writes only while the row, under its
// lock, still records subject (empty for none) and still holds readToken, the refresh token the
// refresh read (opened as Membership opens it, one that does not open being none, and compared in
// constant time); a row another account signed in to since, or the same person again with a new
// refresh token, is left as it is, its member kind and confirmation included, and the confirmation
// still returns nil. That is safe: the one request whose refresh it was passed its generation check
// before the refresh, and a sign-in by another account advanced the generation, so that browser's
// next request fails the check; a sign-in by the same person leaves its newer sign-in to be
// confirmed by its own refresh. When kind is not the member kind the row held under its lock, it
// announces the change in the same transaction (announceMemberKindChange), so a person it
// restricts keeps no open socket or live view of a project that does not name them, and keeps on
// every session tied to email each project the kind the row held saw and kind does not
// (projectsSeenAs, RecordReachedProjects), as SignInSubject does. No sign-in rule confirms a
// changed kind now: Google's confirmation names every account it admits a full member
// (identity.DomainRule.Person), and the sign-in pool's names a full member too.
func (s *PgPeopleStore) ConfirmSubject(ctx context.Context, email, subject, readToken, refreshToken, hostedDomain string, kind model.MemberKind) error {
	if err := signInMemberKind(email, kind); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("confirm membership of %q: %w", email, err)
	}
	defer tx.Rollback(ctx)
	var stored, storedKind string
	var sealed *string
	err = tx.QueryRow(ctx, `select coalesce(sub, ''), refresh_token, member_kind from people where email = $1 for update`, email).Scan(&stored, &sealed, &storedKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the sign-in the confirmation of %q expects: %w", email, err)
	}
	if stored != subject || !s.holds(email, sealed, readToken) {
		return nil
	}
	seen, err := projectsSeenAs(ctx, tx, email, storedKind, kind)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update people set refresh_token = $2, confirmed_at = now(), member_kind = $3, hosted_domain = nullif($4, '') where email = $1`, email, s.seal.seal(email, refreshToken), string(kind), hostedDomain); err != nil {
		return fmt.Errorf("confirm membership of %q: %w", email, err)
	}
	if err := announceMemberKindChange(ctx, tx, storedKind, kind); err != nil {
		return fmt.Errorf("confirm membership of %q: %w", email, err)
	}
	// After the row's lock, as the access package's writes take it after theirs.
	if err := RecordReachedProjects(ctx, tx, seen, []string{email}); err != nil {
		return fmt.Errorf("confirm membership of %q: %w", email, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("confirm membership of %q: %w", email, err)
	}
	return nil
}

// holds reports whether sealed, a people row's refresh_token, is refreshToken once opened as
// Membership opens it: a null one, or one that does not open, is no refresh token, so it holds only
// the empty one. The comparison takes the same time wherever the two differ.
func (s *PgPeopleStore) holds(email string, sealed *string, refreshToken string) bool {
	var held string
	if sealed != nil {
		if opened, err := s.seal.open(email, *sealed); err == nil {
			held = opened
		}
	}
	return subtle.ConstantTimeCompare([]byte(held), []byte(refreshToken)) == 1
}

// End forgets email's refresh token and confirmation, keeping the person, and ends a sign-out that
// kept the token (signed_out_at).
func (s *PgPeopleStore) End(ctx context.Context, email string) error {
	if _, err := s.pool.Exec(ctx, `update people set refresh_token = null, confirmed_at = null, signed_out_at = null where email = $1`, email); err != nil {
		return fmt.Errorf("end membership of %q: %w", email, err)
	}
	return nil
}

// KeepAfterSignOut marks email's row signed out (signed_out_at = clock_timestamp()) and keeps its
// refresh token and confirmation for membership checks (auth.PeopleStore.KeepAfterSignOut), only
// while the row still holds a refresh token and its signed_in_at is still signedInAt, the one the
// sign-out read before it asked whether the person has access that outlives the browser: a sign-in
// that committed meanwhile keeps its token and is not signed out.
func (s *PgPeopleStore) KeepAfterSignOut(ctx context.Context, email string, signedInAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `update people set signed_out_at = clock_timestamp() where email = $1 and refresh_token is not null and signed_in_at = $2`, email, signedInAt); err != nil {
		return fmt.Errorf("keep the refresh token of %q after its sign-out: %w", email, err)
	}
	return nil
}

// ForgetAfterSignOut forgets email's refresh token and confirmation, as End does, only while its
// signed_in_at is still signedInAt (auth.PeopleStore.ForgetAfterSignOut): a sign-in that committed
// while the sign-out asked the broker keeps its token.
func (s *PgPeopleStore) ForgetAfterSignOut(ctx context.Context, email string, signedInAt time.Time) error {
	if _, err := s.pool.Exec(ctx, `update people set refresh_token = null, confirmed_at = null, signed_out_at = null where email = $1 and signed_in_at = $2`, email, signedInAt); err != nil {
		return fmt.Errorf("forget the refresh token of %q after its sign-out: %w", email, err)
	}
	return nil
}

// RetirePlainRefreshTokens retires every refresh token people holds in plain text (retire): one
// stored before Dispatch sealed them, or by a task of that earlier release since, during a roll or
// after a rollback. Dispatch runs it at every boot. It returns how many the pool revoked and how
// many it did not, each of those logged at ERROR naming the person and left in its row for the next
// boot; once ctx is done, each token not yet revoked fails at once. The error is the database's,
// and stops the sweep.
func (s *PgPeopleStore) RetirePlainRefreshTokens(ctx context.Context) (retired, failed int, err error) {
	rows, err := s.pool.Query(ctx, `select email, refresh_token from people where refresh_token is not null and refresh_token not like '`+sealedRefreshTokenPrefix+`%' order by email`)
	if err != nil {
		return 0, 0, fmt.Errorf("read the refresh tokens stored in plain text: %w", err)
	}
	type plain struct{ email, token string }
	tokens, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (plain, error) {
		var p plain
		return p, row.Scan(&p.email, &p.token)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("read the refresh tokens stored in plain text: %w", err)
	}
	for _, p := range tokens {
		switch err := s.retire(ctx, p.email, p.token); {
		case errors.Is(err, errNotRevoked):
			slog.Error("dispatch: the sign-in pool did not revoke a refresh token stored in plain text", "email", p.email, "error", err)
			failed++
		case err != nil:
			return retired, failed, err
		default:
			retired++
		}
	}
	return retired, failed, nil
}

// errNotRevoked is a refresh token stored in plain text that the sign-in pool did not revoke.
var errNotRevoked = errors.New("the sign-in pool did not revoke the refresh token stored in plain text")

// errNoSignInPool is a revocation asked of a store with no sign-in pool to revoke at.
var errNoSignInPool = errors.New("no sign-in pool is configured (DISPATCH_SIGNIN_*) to revoke it at")

// retire revokes plain, a refresh token email's row holds in plain text, at the sign-in pool, then
// forgets it with its confirmation while the row still holds it, so a sign-in recorded since keeps
// its own. Anyone holding a copy of a plain token (a database backup, say) can redeem it with the
// client's secret until the pool revokes it, which forgetting alone never does. A revocation that
// fails leaves the row as it is and returns errNotRevoked wrapping the pool's error, which never
// carries the token; the caller logs it. Only a value without the sealed format's prefix reaches
// it: a sealed one is never sent to the pool.
func (s *PgPeopleStore) retire(ctx context.Context, email, plain string) error {
	if s.revoker == nil {
		return fmt.Errorf("%w: %w", errNotRevoked, errNoSignInPool)
	}
	if err := s.revoker.Revoke(ctx, plain); err != nil {
		return fmt.Errorf("%w: %w", errNotRevoked, err)
	}
	if _, err := s.pool.Exec(ctx, `update people set refresh_token = null, confirmed_at = null where email = $1 and refresh_token = $2`, email, plain); err != nil {
		return fmt.Errorf("forget the revoked refresh token of %q: %w", email, err)
	}
	return nil
}

var _ auth.PeopleStore = (*PgPeopleStore)(nil)

// EndSubject ends email's domain-mode sign-in only while its row records subject, empty meaning
// none, and holds refreshToken, empty meaning none (auth.PeopleStore.EndSubject). It locks the row,
// compares its subject and its refresh token, opened as Membership opens it (one that does not open
// is none), and only then advances the session generation (revokeSessionsSQL) and writes what mode
// says (endSubjectSQL), all in one transaction, so a sign-in cannot land between the check and the
// writes. Comparing the token as well as the subject keeps a failed refresh that read the row
// before the same account signed in again, and was refused after (a stale invalid_grant), from
// ending, or holding, that newer sign-in: its refresh token is not the one the refresh read.
func (s *PgPeopleStore) EndSubject(ctx context.Context, email, subject, refreshToken string, mode auth.EndMode) (bool, error) {
	update, ok := endSubjectSQL[mode]
	if !ok {
		return false, fmt.Errorf("end the sign-in of %q: no end mode %d", email, mode)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("end the sign-in of %q: %w", email, err)
	}
	defer tx.Rollback(ctx)
	var stored string
	var sealed *string
	err = tx.QueryRow(ctx, `select coalesce(sub, ''), refresh_token from people where email = $1 for update`, email).Scan(&stored, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the subject the end of %q expects: %w", email, err)
	}
	if stored != subject || !s.holds(email, sealed, refreshToken) {
		return false, nil
	}
	if _, err := tx.Exec(ctx, revokeSessionsSQL, email); err != nil {
		return false, fmt.Errorf("end the sessions of %q: %w", email, err)
	}
	if _, err := tx.Exec(ctx, update, email); err != nil {
		return false, fmt.Errorf("end membership of %q: %w", email, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("end the sign-in of %q: %w", email, err)
	}
	return true, nil
}

// endSubjectSQL is EndSubject's write of $1's row for each auth.EndMode. EndForget forgets the
// refresh token and confirmation, as on main. EndHold does that and records a hold at the
// database's clock_timestamp(), which is later than the signed_in_at of every sign-in committed
// before it (a sign-in's now() is its transaction's start).
var endSubjectSQL = map[auth.EndMode]string{
	auth.EndForget: `update people set refresh_token = null, confirmed_at = null where email = $1`,
	auth.EndHold:   `update people set refresh_token = null, confirmed_at = null, held_since = clock_timestamp() where email = $1`,
}

// ClaimOverdueConfirmation stamps confirm_tried_at = clock_timestamp() on one row that has a
// stored refresh token and is due: greatest(confirmed_at, confirm_tried_at), which ignores a null,
// is older than age (coalesce the result to '-infinity'). The row due longest is taken first
// (order by that greatest value). It answers its email; ok is false when none is due, which ends
// the membership pass's refresh step. One statement, whose inner select locks the row it picks and
// skips a row another transaction holds, so two passes never claim one row, and a row is stamped
// only when the pass is about to refresh it.
func (s *PgPeopleStore) ClaimOverdueConfirmation(ctx context.Context, age time.Duration) (email string, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `
		update people set confirm_tried_at = clock_timestamp()
		where email = (
			select email from people
			where refresh_token is not null
				and coalesce(greatest(confirmed_at, confirm_tried_at), '-infinity') < now() - make_interval(secs => $1)
			order by coalesce(greatest(confirmed_at, confirm_tried_at), '-infinity'), email
			limit 1
			for update skip locked
		)
		returning email
	`, age.Seconds()).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("claim a person whose membership confirmation is due: %w", err)
	}
	return email, true, nil
}

// ClaimSignedOutHolder stamps outliving_checked_at = clock_timestamp() on one row that is signed
// out (signed_out_at later than signed_in_at), has a refresh token, and whose outliving_checked_at
// is null or older than age, nulls first, then oldest first, and answers its email and the
// signed_out_at and signed_in_at it read; ok is false when none is due, which ends the membership
// pass's sign-out step. It claims as ClaimOverdueConfirmation does, so two passes never claim one
// row.
func (s *PgPeopleStore) ClaimSignedOutHolder(ctx context.Context, age time.Duration) (email string, signedOutAt, signedInAt time.Time, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `
		update people set outliving_checked_at = clock_timestamp()
		where email = (
			select email from people
			where signed_out_at > signed_in_at
				and refresh_token is not null
				and (outliving_checked_at is null or outliving_checked_at < now() - make_interval(secs => $1))
			order by outliving_checked_at nulls first, email
			limit 1
			for update skip locked
		)
		returning email, signed_out_at, signed_in_at
	`, age.Seconds()).Scan(&email, &signedOutAt, &signedInAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, time.Time{}, false, fmt.Errorf("claim a signed-out person whose kept refresh token is due a check: %w", err)
	}
	return email, signedOutAt, signedInAt, true, nil
}

// ForgetSignedOut forgets email's refresh token and confirmation only while its row's
// signed_out_at and signed_in_at are still the ones the claim read (ClaimSignedOutHolder), and the
// row still reads signed out; a sign-in (an older build's sets only signed_in_at) or another
// sign-out since leaves the row as it is. It answers whether it forgot the token: false when that
// guard missed.
func (s *PgPeopleStore) ForgetSignedOut(ctx context.Context, email string, signedOutAt, signedInAt time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		update people set refresh_token = null, confirmed_at = null, signed_out_at = null
		where email = $1 and signed_out_at = $2 and signed_in_at = $3 and signed_out_at > signed_in_at
	`, email, signedOutAt, signedInAt)
	if err != nil {
		return false, fmt.Errorf("forget the kept refresh token of %q: %w", email, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ForgetHostedDomains sets every hosted_domain to null and answers how many rows it changed: an
// intended change of the Workspace's domain (envoy-dispatch forget-hosted-domains), after which
// the boot's domain check has no recorded domain to compare until each row's next confirmation.
func (s *PgPeopleStore) ForgetHostedDomains(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `update people set hosted_domain = null where hosted_domain is not null`)
	if err != nil {
		return 0, fmt.Errorf("forget every recorded hosted domain: %w", err)
	}
	return tag.RowsAffected(), nil
}

// BootPeople is the boot's one read of people (PgPeopleStore.BootPeople).
type BootPeople struct {
	// Checkable has a stored refresh token, whether or not it opens, and a recorded Google account
	// (a subject); Held has a recorded hold; Uncheckable is every other row. Each row is in one
	// count: a row with a recorded hold is Held, whatever else it holds.
	Checkable, Held, Uncheckable int
	// FullMemberDomains is each distinct hosted_domain of a full member's row that has a
	// refresh token, sorted byte for byte.
	FullMemberDomains []string
}

// BootPeople is the boot's one read of people, in one statement: the three counts of the people
// census and the domains full members' rows that hold a refresh token record, which the boot's
// domain check compares with DISPATCH_SIGNIN_DOMAIN.
func (s *PgPeopleStore) BootPeople(ctx context.Context) (BootPeople, error) {
	var census BootPeople
	err := s.pool.QueryRow(ctx, `
		select
			count(*) filter (where not coalesce(held_since > signed_in_at, false)
				and refresh_token is not null and coalesce(sub, '') <> ''),
			count(*) filter (where coalesce(held_since > signed_in_at, false)),
			count(*) filter (where not coalesce(held_since > signed_in_at, false)
				and not (refresh_token is not null and coalesce(sub, '') <> '')),
			coalesce(array_agg(distinct hosted_domain order by hosted_domain)
				filter (where member_kind = 'full' and refresh_token is not null and hosted_domain is not null), '{}')
		from people
	`).Scan(&census.Checkable, &census.Held, &census.Uncheckable, &census.FullMemberDomains)
	if err != nil {
		return BootPeople{}, fmt.Errorf("read the people census: %w", err)
	}
	// The database's collation may order letters across case; the domain check compares bytes.
	slices.Sort(census.FullMemberDomains)
	return census, nil
}
