package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// A browser session is a person signed in to the broker's approval page (0022's browser_sessions).
// Each function takes the SHA-256 of the session's cookie secret, never the secret itself, and
// the caller's now: a session is live while its expires_at is after now, so the lookup and the
// sweep draw the same line, as the sweeper's other expiries do on the time it passes them. Each
// session records the Google account that signed in (0045's issuer and subject, none on a row
// from before), which is what the revoke of an account that no longer holds the person's address
// ends it by (DeleteSubjectBrowserSessionsTx). A decision made under a session reads it again in
// its own transaction, under the person's lock (HoldBrowserSessionTx).

// ErrBrowserSessionEnded is a decision whose approval-page session is no longer live when its
// transaction reads it under the person's lock (HoldBrowserSessionTx): signed out, ended by
// `broker end-sessions`, expired, or deleted by the revoke of the account that signed it in.
var ErrBrowserSessionEnded = errors.New("store: the approval-page session has ended")

// BrowserSessionRef is one approval-page session as a decision made under it names it: the
// SHA-256 of its cookie secret and the login the page read it as.
type BrowserSessionRef struct {
	Hash  [sha256.Size]byte
	Login string
}

// BrowserSignIn is one sign-in to the approval page as its callback hands it over.
type BrowserSignIn struct {
	// Hash is the SHA-256 of the new session's cookie secret.
	Hash [sha256.Size]byte
	// Login is the person's canonical login, as oidc.WorkspaceLogin folds it.
	Login string
	// Account is the account the sign-in's ID token names: its `iss`, as oidc.CanonicalIssuer
	// spells it, and its `sub`. Every sign-in names one.
	Account IssuerSubject
	// Expires is the session's absolute expiry, never extended.
	Expires time.Time
	// Previous is the SHA-256 of the session the signing-in browser held, which the sign-in
	// ends so that no session lingers behind the new one; nil when it held none.
	Previous *[sha256.Size]byte
}

// CreateBrowserSession writes in's session and deletes the browser's previous one, in one
// transaction. It answers ErrSubjectReplaced, writing and deleting nothing, when
// in.Account no longer holds in.Login's address.
//
// It takes the person's lock first and then reads whether the account is replaced (LockPersonAs),
// as a sign-in trade does (RecordPersonSignIn): a revoke of that account
// (enroll.Service.RevokePersonSubject) either commits before the read, which then refuses the
// sign-in, or waits for this one to commit and deletes the session it wrote. Without the lock a
// sign-in whose read came before the revoke's commit would write a session the revoke never saw,
// live for BROKER_SIGNIN_SESSION_SECONDS.
func (s *Store) CreateBrowserSession(ctx context.Context, in BrowserSignIn) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := LockPersonAs(ctx, tx, in.Login, in.Account); err != nil {
		return err
	}
	if in.Previous != nil {
		if _, err := tx.Exec(ctx, `delete from browser_sessions where secret_hash = $1`, in.Previous[:]); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `insert into browser_sessions (secret_hash, login, issuer, subject, expires_at)
		values ($1, $2, $3, $4, $5)`, in.Hash[:], in.Login, in.Account.Issuer, in.Account.Subject, in.Expires); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// BrowserSession answers the login of the session whose secret hashes to hash while it is live at
// now. A session past its expiry is absent, as one never made or logged out is: each answers
// pgx.ErrNoRows.
func (s *Store) BrowserSession(ctx context.Context, hash [sha256.Size]byte, now time.Time) (login string, err error) {
	err = s.Pool.QueryRow(ctx, `select login from browser_sessions where secret_hash = $1 and expires_at > $2`,
		hash[:], now).Scan(&login)
	return login, err
}

// DeleteBrowserSession deletes the session whose secret hashes to hash, its logout. A session
// already gone is no error.
func (s *Store) DeleteBrowserSession(ctx context.Context, hash [sha256.Size]byte) error {
	_, err := s.Pool.Exec(ctx, `delete from browser_sessions where secret_hash = $1`, hash[:])
	return err
}

// DeleteExpiredBrowserSessions deletes every session no longer live at now and answers how many
// it deleted.
func (s *Store) DeleteExpiredBrowserSessions(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `delete from browser_sessions where expires_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteBrowserSessionsForLogin deletes every session signed in as login, live or expired, and
// answers how many it deleted: the `broker end-sessions <login>` subcommand, an operator ending
// one person's sign-ins everywhere at once. A login with no session is no error and zero.
func (s *Store) DeleteBrowserSessionsForLogin(ctx context.Context, login string) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `delete from browser_sessions where login = $1`, login)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteSubjectBrowserSessionsTx deletes, in tx, every session of person, live or expired, that
// account signed in or that records no account, a session from before migration 0045 that might
// be any account's, and answers how many it deleted: the approval-page half of the revoke of an
// account that no longer holds person's address (enroll.Service.RevokePersonSubject). A session
// another account signed in is not among them. The caller holds LockPerson, so no sign-in of
// person writes a session while it deletes, and no decision under one of these sessions is in
// flight (HoldBrowserSessionTx).
func DeleteSubjectBrowserSessionsTx(ctx context.Context, tx pgx.Tx, person string, account IssuerSubject) (int, error) {
	tag, err := tx.Exec(ctx, `delete from browser_sessions
		where login = $1 and (subject is null or (issuer, subject) = ($2, $3))`, person, account.Issuer, account.Subject)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// HoldBrowserSessionTx takes, in tx and until it ends, the lock of session's person (LockPerson),
// and then reads whether session is still live at now and still signed in as that person,
// answering ErrBrowserSessionEnded when it is not: the first thing a decision on the approval page
// does in its transaction (requests.Machine.ApplyPageDecision), before it takes any row's lock.
//
// The revoke of the account that signed the session in (enroll.Service.RevokePersonSubject)
// deletes it under that same lock, so a decision either commits before the revoke reads the
// person's sessions, or waits for the revoke to commit, reads no session, and decides nothing.
// Without the lock a decision that read the session just before the revoke committed would
// commit after it, a decision by an account the broker had already recorded as replaced.
func HoldBrowserSessionTx(ctx context.Context, tx pgx.Tx, session BrowserSessionRef, now time.Time) error {
	if err := LockPerson(ctx, tx, session.Login); err != nil {
		return err
	}
	var live bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from browser_sessions
		where secret_hash = $1 and login = $2 and expires_at > $3)`, session.Hash[:], session.Login, now).Scan(&live); err != nil {
		return err
	}
	if !live {
		return ErrBrowserSessionEnded
	}
	return nil
}
