package store

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
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
// it records the address's first account (a row recording none, or no row at all, which
// a broker token can mint a personal token without), it revokes the address's personal tokens that
// recorded another account at their mint, or none (revokeOtherAccountsTokensSQL): an account that
// held the address at the broker and never signed in to Dispatch, whose tokens record it, and a
// credential bound to no account, whose tokens predate this recording; those that recorded this
// one stand. When the store owes
// broker revocations (WithBrokerRevocations: a broker is configured), it also records that
// Dispatch owes the secrets broker a revocation of the replaced account, whatever member kind the
// row held, and on a first recording one of each account whose token it revoked
// (recordBrokerRevocationSQL, its last statements): the broker ends every credential the
// replaced `sub` holds under email, and every one that records no account, whenever issued,
// refuses it any new one, leaves subject's, and deletes any record that subject was itself an
// account the address was taken from, which only a call made after this commit can do
// (BrokerRevocations, the server's revocation loop). A restricted member's account is revoked as a
// full member's is: the hourly confirmation restricts an account under the same `sub`
// (ConfirmSubject), so a restricted row can name an account that made broker credentials while it
// was full, and the broker may still refuse subject for a revocation an earlier sign-in recorded
// and a pass paid, which only a revoke naming subject as the holder lifts. Dispatch itself refuses
// the replaced account's credentials from the commit, and holds those recording no account to when
// they were made against the time this sign-in records (api.brokerPersonAdmitted,
// api.tokenAccountReplaced); the new holder's own tokens, which record its account, act throughout.
// Whenever it records a revocation, it first settles every revocation of email still owed for
// subject itself (settleReclaimedBrokerRevocationsSQL): an account that takes the address back is
// owed nothing, so no pass ends what it makes once it holds the address again. When kind is not the
// member kind the row held under its lock, it announces the change in the same transaction
// (announceMemberKindChange). A revoke that fails rolls the sign-in back, leaving the old subject
// for the next sign-in to revoke again; one that commits is logged at WARN with the address, and
// with how many tokens it revoked when it replaced an account or revoked another account's tokens.
//
// A restricted sign-in (a person from outside the domain) never takes a full member's address
// (takesAMembersAddress): it writes nothing and returns auth.ErrAddressHeldByMember, read once
// before the plain token's retirement (refuseAMembersAddress), so the member's token is never
// revoked, and again under the row's lock, so a member's sign-in that commits in between is
// refused too. A full sign-in reads and writes what it always did.
func (s *PgPeopleStore) SignInSubject(ctx context.Context, email, subject, refreshToken string, kind model.MemberKind) error {
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
	if _, err := tx.Exec(ctx, addressLockSQL, email); err != nil {
		return fmt.Errorf("record sign-in of %q: lock the address: %w", email, err)
	}
	inserted, err := tx.Exec(ctx, `insert into people (email) values ($1) on conflict (email) do nothing`, email)
	if err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	var stored, storedKind string
	if err := tx.QueryRow(ctx, `select coalesce(sub, ''), member_kind from people where email = $1 for update`, email).Scan(&stored, &storedKind); err != nil {
		return fmt.Errorf("read the subject the sign-in of %q replaces: %w", email, err)
	}
	existed := inserted.RowsAffected() == 0
	if existed && takesAMembersAddress(kind, stored, storedKind, subject) {
		return fmt.Errorf("record sign-in of %q: %w", email, auth.ErrAddressHeldByMember)
	}
	replaced := stored != "" && stored != subject
	firstRecording := stored == "" && subject != ""
	var tokensRevoked int64
	var owed []string
	switch {
	case replaced:
		tokens, err := tx.Exec(ctx, revokeAddressTokensSQL, email)
		if err != nil {
			return fmt.Errorf("revoke the personal tokens of %q, which another Google account signs in to now: %w", email, err)
		}
		tokensRevoked = tokens.RowsAffected()
		owed = []string{stored}
	case firstRecording:
		if tokensRevoked, owed, err = revokeOtherAccountsTokens(ctx, tx, email, subject); err != nil {
			return err
		}
	}
	// Changing sub stamps people.sub_recorded_at from the database's own clock, the address's first
	// recording included, and ends every session of the address (the triggers of migrations
	// 20261117175133 and 20261118041916, which no statement can bypass): a credential recording no
	// account made before it is refused as the person, and no cookie issued before it acts. A
	// sign-in under the account the row already records leaves both.
	if _, err := tx.Exec(ctx, `
		update people
		set signed_in_at = now(), refresh_token = $2, confirmed_at = now(), sub = $3, member_kind = $4
		where email = $1
	`, email, s.seal.seal(email, refreshToken), subject, string(kind)); err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	if err := announceMemberKindChange(ctx, tx, storedKind, kind); err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	if len(owed) > 0 && s.brokerRevocationIssuer != "" {
		if _, err := tx.Exec(ctx, settleReclaimedBrokerRevocationsSQL, email, s.brokerRevocationIssuer, subject); err != nil {
			return fmt.Errorf("settle the broker revocations of %q owed for the Google account that takes it back: %w", email, err)
		}
		// Last, so the moment each records is as close to the commit as a statement can read it:
		// the order a later replacement of the address is paid after.
		for _, account := range owed {
			if _, err := tx.Exec(ctx, recordBrokerRevocationSQL, email, s.brokerRevocationIssuer, account, subject); err != nil {
				return fmt.Errorf("record the broker revocation of %q, which another Google account signs in to now: %w", email, err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record sign-in of %q: %w", email, err)
	}
	switch {
	case replaced:
		slog.Warn("dispatch: a sign-in under another Google account ended every session and revoked every personal token of the address", "email", email, "tokens_revoked", tokensRevoked)
	case tokensRevoked > 0:
		slog.Warn("dispatch: the first Google account recorded for the address revoked every personal token of it that records another account or none", "email", email, "tokens_revoked", tokensRevoked, "accounts", len(owed))
	case existed && firstRecording:
		// A row this sign-in's own insert created had no cookie of its own to end, so only a row
		// that existed is logged; the database ended the sessions either way.
		slog.Warn("dispatch: a sign-in under another Google account ended every session of the address", "email", email)
	}
	return nil
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
// or wrapped in a tab or a no-break space, is the address's too. That comparison reads every token
// rather than agent_tokens_owner_created_at, which costs nothing that matters: SignInSubject runs it
// only when another Google account replaces the one the address recorded, under the people row's
// lock, in the transaction that ends the address's sessions.
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
// revokeAddressTokensSQL does. It reads every token as that does, only on the address's first
// recording, under the people row's lock.
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
// without the value. The read writes nothing: the next boot retires a token stored in plain text
// (RetirePlainRefreshTokens), and the person's next sign-in replaces whatever the row holds. The
// subject and the member kind are read whatever the refresh token holds.
func (s *PgPeopleStore) Membership(ctx context.Context, email string) (auth.PersonMembership, bool, error) {
	var sealed *string
	var age *time.Duration
	var kind string
	membership := auth.PersonMembership{}
	err := s.pool.QueryRow(ctx, `select refresh_token, now() - confirmed_at, coalesce(sub, ''), member_kind from people where email = $1`, email).Scan(&sealed, &age, &membership.Subject, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.PersonMembership{}, false, nil
	}
	if err != nil {
		return auth.PersonMembership{}, false, fmt.Errorf("read membership of %q: %w", email, err)
	}
	membership.Kind = model.MemberKind(kind)
	// people's check pairs the two: a row holds both or neither.
	if sealed == nil {
		return membership, true, nil
	}
	refreshToken, err := s.seal.open(email, *sealed)
	if err != nil {
		slog.Warn("dispatch: a stored refresh token did not open; the person signs in again", "email", email, "reason", err.Error())
		return membership, true, nil
	}
	membership.RefreshToken, membership.Confirmed, membership.ConfirmedAge = refreshToken, true, *age
	return membership, true, nil
}

// ConfirmSubject records a refresh that confirmed email's membership at the database's now(), as
// kind, with refreshToken to use next, refused as SignInSubject refuses it
// (auth.PeopleStore.ConfirmSubject): a Google sign-in's under its subject, and a sign-in pool's
// under the empty subject, since a pool's row records none. It writes only while the row, under its
// lock, still records subject (empty for none) and still holds readToken, the refresh token the
// refresh read (opened as Membership opens it, one that does not open being none, and compared in
// constant time); a row another account signed in to since, or the same person again with a new
// refresh token, is left as it is, its member kind and confirmation included, and the confirmation
// still returns nil. That is safe: the one request whose refresh it was passed its generation check
// before the refresh, and a sign-in by another account advanced the generation, so that browser's
// next request fails the check; a sign-in by the same person leaves its newer sign-in to be
// confirmed by its own refresh. When kind is not the member kind the row held under its lock (the
// rule restricts a member whose account left the Workspace, or admits a restricted one as full), it
// announces the change in the same transaction (announceMemberKindChange), so a person the
// confirmation restricts keeps no open socket or live view of a project that does not name them.
func (s *PgPeopleStore) ConfirmSubject(ctx context.Context, email, subject, readToken, refreshToken string, kind model.MemberKind) error {
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
	if _, err := tx.Exec(ctx, `update people set refresh_token = $2, confirmed_at = now(), member_kind = $3 where email = $1`, email, s.seal.seal(email, refreshToken), string(kind)); err != nil {
		return fmt.Errorf("confirm membership of %q: %w", email, err)
	}
	if err := announceMemberKindChange(ctx, tx, storedKind, kind); err != nil {
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

// End forgets email's refresh token and confirmation, keeping the person.
func (s *PgPeopleStore) End(ctx context.Context, email string) error {
	if _, err := s.pool.Exec(ctx, `update people set refresh_token = null, confirmed_at = null where email = $1`, email); err != nil {
		return fmt.Errorf("end membership of %q: %w", email, err)
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
// is none), and only then advances the session generation (revokeSessionsSQL) and forgets the
// refresh token and confirmation, all in one transaction, so a sign-in cannot land between the
// check and the writes. Comparing the token as well as the subject keeps a failed refresh that read
// the row before the same account signed in again, and was refused after (a stale invalid_grant),
// from ending that newer sign-in: its refresh token is not the one the refresh read.
func (s *PgPeopleStore) EndSubject(ctx context.Context, email, subject, refreshToken string) (bool, error) {
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
	if _, err := tx.Exec(ctx, `update people set refresh_token = null, confirmed_at = null where email = $1`, email); err != nil {
		return false, fmt.Errorf("end membership of %q: %w", email, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("end the sign-in of %q: %w", email, err)
	}
	return true, nil
}
