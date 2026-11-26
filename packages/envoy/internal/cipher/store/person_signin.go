package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
)

// A person's sign-ins are rows of 0025's person_signins, one per box key: the person, the RFC
// 7638 thumbprint of the key `agent-secrets login` made on that machine, the Google account of
// the machine, the account of the person's own Google identity token that made it (0045: its
// issuer and `sub`, none on a row from before), and the times the row was made, last used and
// revoked.
//
// The exchange writes through RecordPersonSignIn in its own transaction, so the row and the
// access token the trade mints land together or not at all; the routes Dispatch relays list
// through PersonSignIns on the pool and revoke through RevokePersonSignInTx, which takes the
// caller's transaction because the launcher credential the sign-in held ends in it too.
//
// A revoked row is never deleted and never re-used: it is what refuses that box key for good, so
// a person who revokes a sign-in is not undone by the box trading the very key again. A person
// signing in on a second box is a second thumbprint and a row of its own, which is why revoking
// one leaves the others working. The one row ever deleted is a live one from before migration
// 0045, which records no account: the next trade under its key ends it and everything it holds
// and writes a row of its own (RecordPersonSignIn), so a row's account is never changed.
//
// The three vocabularies - a lowercase login, an RFC 7638 thumbprint and a lowercase Google
// account - are closed by the migration's own check constraints, which are the one place they
// are written down, so nothing a caller spells reaches a person's own list through this table.

// ErrSignInRevoked is a trade whose person has revoked the box key it proved.
var ErrSignInRevoked = errors.New("store: the person has revoked this machine's sign-in")

// ErrSignInMachineChanged is a trade presenting a sign-in's box key from a machine other than
// the one the sign-in was made on. The sign-in is the person, the key and the machine together,
// and the launcher credential it holds is keyed by all three (exchange.PersonSubject), so a row
// that followed the key to a new machine would ask for a subject no live credential has and
// every trade from then on would collide on launcher_credentials_live_key. The box signs in
// again, which makes a key of its own.
var ErrSignInMachineChanged = errors.New("store: this sign-in was made on another machine")

// ErrNoSignIn is a sign-in id that is not that person's, or is no sign-in at all.
var ErrNoSignIn = errors.New("store: no sign-in of this person has that id")

// ErrSignInSubjectChanged is a trade presenting a sign-in's box key with a person token of
// another account than the one that made the sign-in. A sign-in is the account that made it as
// well as the person, the key and the machine, since that account is what a revoke of a replaced
// account ends it by; the box signs in again, which makes a key of its own.
var ErrSignInSubjectChanged = errors.New("store: this sign-in was made by another account")

// SignInTrade is one trade of a person's sign-in as the exchange hands it over.
type SignInTrade struct {
	// Person is the login the person's own Google identity token names.
	Person string
	// Thumbprint is the RFC 7638 thumbprint of the box key the trade's DPoP proof presented.
	Thumbprint string
	// MachineAccount is the Google account of the machine, as BROKER_VM_SERVICES lists it.
	MachineAccount string
	// Account is the account the person's own Google identity token was minted for: its `iss`,
	// as oidc.CanonicalIssuer spells it, and its `sub`. Every trade names one.
	Account IssuerSubject
	// EndUnrecorded is whether a live row of this person and key from before migration 0045,
	// which records no account, is ended so this trade writes a row of its own: true where the
	// broker asks Dispatch (BROKER_PROJECT_ACCESS_URL), whose binding of an address to one account
	// is what a row's account is for. False keeps that row, records no account on it and ends
	// nothing, as before the release; a revoke of any account at the address still ends it
	// (SubjectSignInsTx), since it records none.
	EndUnrecorded bool
}

// PersonSignIn is one live person_signins row as a person reads it on their own list.
type PersonSignIn struct {
	// ID is the row's id, which the revoke route takes.
	ID string
	// Person is the login the sign-in was made as.
	Person string
	// Thumbprint is the RFC 7638 thumbprint of the box key the sign-in is bound to.
	Thumbprint string
	// MachineAccount is the Google account of the machine the sign-in was made on, as
	// BROKER_VM_SERVICES lists it.
	MachineAccount string
	// CreatedAt is when the sign-in was first recorded, and LastUsedAt when its box key last
	// traded a token.
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// RecordPersonSignIn records, in tx, that in.Person signed in on the machine in.MachineAccount
// names with the box key in.Thumbprint, as the account in.Account, and answers whether it ended a
// sign-in that recorded no account to do so. It answers ErrSubjectReplaced when that
// account no longer holds the person's address, ErrSignInRevoked when that person has revoked
// that key, ErrSignInMachineChanged when the sign-in that key belongs to was made on another
// machine, and ErrSignInSubjectChanged when it was made by another account.
//
// A row's account is written once, when the row is inserted, and never changes: migration 0046's
// trigger refuses any update of issuer or subject
// (TestTheDatabaseRefusesAnyChangeOfARecordedAccount). The first trade of a person and a box key
// inserts the row, recording the account; every later trade of that pair by that account is the
// same sign-in, so it moves last_used_at and nothing else. A live row from
// before migration 0045 records none, and might be any account's. With in.EndUnrecorded a trade
// under its key ends it rather than adopting it: the row is deleted, which frees the pair's slot
// for the new row, and every live access token of that person bound to that key ends in this same
// transaction (endSignInTokensSQL), as revoking the sign-in ends them. The caller ends the launcher
// credential the deleted row held, and every session enrolled under it, in this transaction too
// (enroll.RecordPersonSignIn), which is what the answer is for. Without it the trade is that row's,
// which goes on recording no account: it moves last_used_at and nothing else, and ends nothing.
// The token this trade mints is
// written after this returns (oauth.Provider.exchangeGrant's NewAccessResponse), so it is not
// among those ended. It takes tx rather than the pool because the exchange must write this and the
// access token it is minting together: a revoke that commits first refuses the trade, and one that
// commits after it ends the token the trade minted (RevokePersonSignIn).
//
// It takes the person's lock first and then reads whether the account is replaced (LockPersonAs),
// so a revoke of that account (enroll.Service.RevokePersonSubject) either commits before the read,
// which then refuses the trade, or waits for this trade to commit and ends the row it wrote. The
// read is what refuses a box key the person never traded before: no row conflicts with one. A
// revoke of the sign-in itself (RevokePersonSignInTx) takes the row's lock: one that commits first
// leaves a revoked row the delete's `where` does not match and the insert's conflict refuses, and
// one that comes after finds no row.
//
// The insert's own conflict is what decides the rest, in one statement. A person holds one row per
// box key whether that row is live or revoked, so a trade of a revoked key, of a key on another
// machine, or of a key another account signed in with, conflicts with the row that holds the pair
// and the update's own `where` leaves it untouched: nothing is returned, and that is the refusal.
// Reading those rows first would not do, since under READ COMMITTED that read takes its snapshot
// before a concurrent revoke of the sign-in commits while the insert waits on the live index entry,
// and the trade would add a second live row behind the revoke's back.
//
// Which of the three refusals it is comes from a read of its own, after the conflict has already
// decided, so a revoke that commits mid-flight changes the words and never the verdict. It is
// a statement of its own because a sub-select of the first would read the first's snapshot,
// which is the one taken before that revoke committed.
//
// person is stored and compared as its caller wrote it: every caller passes a login the one ASCII
// fold made (oidc.WorkspaceLogin for the trade, record.CanonicalLogin for the list and the
// revoke), and Postgres's lower() folds Unicode, which would store U+212A KELVIN SIGN's login as
// another person's k. The table's check refuses any login that is not lowercase. machineAccount,
// a service account's address, is lowered here as it always was.
func RecordPersonSignIn(ctx context.Context, tx pgx.Tx, in SignInTrade) (bool, error) {
	if err := LockPersonAs(ctx, tx, in.Person, in.Account); err != nil {
		return false, err
	}
	deleted, err := tx.Exec(ctx, `delete from person_signins
		where $4 and person = $1 and thumbprint = $2 and revoked_at is null and subject is null and machine_account = lower($3)`,
		in.Person, in.Thumbprint, in.MachineAccount, in.EndUnrecorded)
	if err != nil {
		return false, err
	}
	ended := deleted.RowsAffected() > 0
	if ended {
		if _, err := tx.Exec(ctx, endSignInTokensSQL, in.Person, in.Thumbprint); err != nil {
			return false, err
		}
	}
	var recorded int
	err = tx.QueryRow(ctx, `insert into person_signins (person, thumbprint, machine_account, issuer, subject)
		values ($1, $2, lower($3), $4, $5)
		on conflict (person, thumbprint) do update
			set last_used_at = now()
			where person_signins.revoked_at is null
				and person_signins.machine_account = excluded.machine_account
				and ((person_signins.issuer, person_signins.subject) = (excluded.issuer, excluded.subject)
					or (not $6 and person_signins.subject is null))
		returning 1`, in.Person, in.Thumbprint, in.MachineAccount, in.Account.Issuer, in.Account.Subject, in.EndUnrecorded).Scan(&recorded)
	if err == nil {
		return ended, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var revoked, moved bool
	if err := tx.QueryRow(ctx, `select revoked_at is not null, machine_account <> lower($3) from person_signins
		where person = $1 and thumbprint = $2`, in.Person, in.Thumbprint, in.MachineAccount).Scan(&revoked, &moved); err != nil {
		return false, err
	}
	switch {
	case revoked:
		return false, ErrSignInRevoked
	case moved:
		return false, ErrSignInMachineChanged
	}
	return false, ErrSignInSubjectChanged
}

// endSignInTokensSQL ends every live access token of person $1 bound to box key $2: the tokens
// whose stored session names that person and that key, which together are one sign-in and no
// other, as RevokePersonSignInTx selects them.
const endSignInTokensSQL = `update oauth_access_tokens set active = false
	where active and (request #>> '{session,extra,person}', request #>> '{session,jwk_thumbprint}') = ($1, $2)`

// PersonSignIns lists the live sign-ins of person, newest first: one per machine the person has
// signed in on and not revoked. A person with none has an empty list and no error.
func (s *Store) PersonSignIns(ctx context.Context, person string) ([]PersonSignIn, error) {
	rows, err := s.Pool.Query(ctx, `select id::text, person, thumbprint, machine_account, created_at, last_used_at
		from person_signins where person = $1 and revoked_at is null order by created_at desc, id`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonSignIn
	for rows.Next() {
		var in PersonSignIn
		if err := rows.Scan(&in.ID, &in.Person, &in.Thumbprint, &in.MachineAccount, &in.CreatedAt, &in.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// SubjectSignInsTx answers, in tx, the ids of person's live sign-ins that account may have made:
// those recorded as its, and those recorded as nobody's, rows from before migration 0045 that no
// trade has traded since, which might be any account's. A sign-in another account made is not
// among them. The caller holds LockPerson, so no trade of person adds a row while it reads.
func SubjectSignInsTx(ctx context.Context, tx pgx.Tx, person string, account IssuerSubject) ([]string, error) {
	rows, err := tx.Query(ctx, `select id::text from person_signins
		where person = $1 and revoked_at is null and (subject is null or (issuer, subject) = ($2, $3))
		order by id`, person, account.Issuer, account.Subject)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// RevokePersonSignInTx ends sign-in id of person in tx and answers the box key thumbprint it
// revoked and how many of that person's live access tokens end with it. A row already revoked is
// revoked again by nobody: it answers an empty thumbprint, no tokens and no error, as revoking a
// machine login twice answers the same. An id no sign-in of that person has is ErrNoSignIn, so
// one person cannot learn another's ids by trying them.
//
// It takes tx rather than the pool because ending the sign-in is only half of it: the launcher
// credential that sign-in's trades mint is ended in the same transaction
// (enroll.Service.RevokePersonSignIn), and the thumbprint answered here is what names it.
//
// Its own half is one statement, and that is the point. Revoking the sign-in alone would leave
// every token already minted under it writing for up to BROKER_ACCESS_TOKEN_SECONDS, and a
// resource server's affirmative introspection answer for as long again, so the tokens are ended
// here, exactly as revoking a machine login ends the tokens its credential holds
// (oauthstore.EndCredential). Which tokens those are is written in each row's stored session:
// the person it names and the key it is bound to, which together are this sign-in and no other,
// so the person's sign-ins on their other boxes keep every token they hold. Only access tokens
// are ended, because an exchanged trade is answered no refresh token at all.
//
// The revoke is dated clock_timestamp(), the statement's own moment, not now(), its
// transaction's start. A transaction that waited for a trade to commit, as the revoke of a
// replaced account waits on the person's lock (enroll.Service.RevokePersonSubject), began before
// the row it ends was made, and the table's check refuses a revoke dated before its row.
//
// A token a read trade of the sign-in narrowed to reading is bound to its holder's key, not to the
// box key, and names the launcher credential on the box key instead: it is live only while that
// credential is, so it ends when the caller revokes the credential, and nothing here writes it.
// The count takes in each such token that is live now, read through oauthstore.NarrowedLive, the
// rule introspection ends it by, so it is every token the revoke ends: the ones this statement
// ends, and the read tokens that end with the credential.
func RevokePersonSignInTx(ctx context.Context, tx pgx.Tx, id, person string) (string, int, error) {
	var found, ended int
	var thumbprint *string
	err := tx.QueryRow(ctx, `with target as (
			select id from person_signins where id = $1 and person = $2 for no key update
		), revoked as (
			update person_signins set revoked_at = clock_timestamp()
			where id in (select id from target) and revoked_at is null
			returning person, thumbprint
		), tokens as (
			update oauth_access_tokens set active = false
			where active and (request #>> '{session,extra,person}', request #>> '{session,jwk_thumbprint}')
				in (select person, thumbprint from revoked)
			returning 1
		), narrowed as (
			select 1 from oauth_narrowed_access_tokens n
			join launcher_credentials credential on credential.id = n.credential_id
			where credential.key_thumbprint in (select thumbprint from revoked) and `+oauthstore.NarrowedLive+`
		)
		select (select count(*) from target), (select count(*) from tokens) + (select count(*) from narrowed),
			(select thumbprint from revoked)`, id, person).Scan(&found, &ended, &thumbprint)
	if err != nil {
		return "", 0, err
	}
	if found == 0 {
		return "", 0, ErrNoSignIn
	}
	if thumbprint == nil {
		return "", 0, nil
	}
	return *thumbprint, ended, nil
}
