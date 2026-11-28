package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// An address is held by one account of the identity provider at a time, and Dispatch decides
// which. When another account takes an address Dispatch holds for a person, Dispatch names
// the account it replaced (POST /v1/person-subjects/revoke), and the broker records it in 0047's
// replaced_person_subjects: every credential that account holds under the address, and every
// session it holds on the broker's own approval page, ends in the revoke's own transaction
// (enroll.Service.RevokePersonSubject), and every later sign-in trade, machine-login approval and
// approval-page sign-in of that account under the address is refused, as is every decision on
// the page under a session the revoke deleted.
//
// Each of those five takes the one per-person lock (LockPerson) before it reads or writes what
// the others do, so nothing the replaced account makes can land after the revoke commits: a
// trade, an approval, a page sign-in or a page decision that took the lock first has committed
// before the revoke reads the person's credentials and sessions, and the revoke ends the
// credential or the session it made (a page decision stands, made while the account still held
// the address); one that waited reads the replaced account's row the revoke committed, or finds
// its session gone, and is refused. A time does not do this. A request whose check passed before
// the revoke can reach the broker at any moment after it, and a Google identity token minted
// before the address moved stays valid for an hour.

// IssuerSubject is one account of an identity provider: its issuer, as oidc.CanonicalIssuer spells
// it, and its `sub`. The zero value names no account: a row written before migration 0047, or a
// machine login whose approval an older Dispatch relayed without the approving account.
type IssuerSubject struct {
	Issuer  string
	Subject string
}

// Recorded reports whether the pair names an account at all.
func (a IssuerSubject) Recorded() bool { return a.Subject != "" }

// LockPerson takes, in tx and until it ends, the lock that serializes everything that writes
// person's credentials or approval-page sessions, or decides under one of those sessions, against
// the revoke of an account that no longer holds person's address: the sign-in trade
// (RecordPersonSignIn), a machine login's approval (machine.Service.ApplyDecision), a sign-in to
// the approval page (Store.CreateBrowserSession), a decision on that page
// (HoldBrowserSessionTx) and the revoke (enroll.Service.RevokePersonSubject). It is a
// transaction-level advisory lock on a hash of the person, so one person's trades, approvals,
// page sign-ins and page decisions wait for each other and for a revoke of that person, and
// nobody else's do. A hash collision with another person, or with another of the broker's
// advisory locks, makes two unrelated transactions wait for each other and changes no answer.
//
// Every statement after it reads with a snapshot taken once the lock is held, since the
// broker's transactions run under READ COMMITTED, so a reader that waited for a revoke sees what
// that revoke committed.
func LockPerson(ctx context.Context, tx pgx.Tx, person string) error {
	_, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('person-subject:' || $1, 0))`, person)
	return err
}

// ErrSubjectReplaced is a sign-in trade (RecordPersonSignIn), an approval-page sign-in
// (Store.CreateBrowserSession) or a machine login's approval (machine.Service.ApplyDecision) by an
// account a revoke recorded as one that no longer holds the person's address
// (RecordReplacedSubjectTx): the broker refuses that account every new credential and session
// there.
var ErrSubjectReplaced = errors.New("store: another account holds this person's address now")

// LockPersonAs takes LockPerson for person in tx and then answers ErrSubjectReplaced when account
// is recorded, in tx, as an account that no longer holds person's address; an account not recorded
// is never replaced. A sign-in trade, an approval-page sign-in and a machine login's approval each
// begin with it, so a revoke of that account either commits before the read, which then refuses
// them, or waits for them to commit and ends what they wrote. The lock is held until tx ends.
func LockPersonAs(ctx context.Context, tx pgx.Tx, person string, account IssuerSubject) error {
	if err := LockPerson(ctx, tx, person); err != nil {
		return err
	}
	if !account.Recorded() {
		return nil
	}
	var replaced bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from replaced_person_subjects
		where person = $1 and issuer = $2 and subject = $3)`, person, account.Issuer, account.Subject).Scan(&replaced); err != nil {
		return err
	}
	if replaced {
		return ErrSubjectReplaced
	}
	return nil
}

// SubjectReplacedWithin reports whether Dispatch has named any account replaced at person's
// address within the last window, as the broker's own clock reads it in tx (a revoke's
// replaced_at against the reading transaction's now()). The caller holds LockPerson, so a revoke
// that committed while it waited is read.
func SubjectReplacedWithin(ctx context.Context, tx pgx.Tx, person string, window time.Duration) (bool, error) {
	var replaced bool
	err := tx.QueryRow(ctx, `select exists (select 1 from replaced_person_subjects
		where person = $1 and replaced_at > now() - make_interval(secs => $2))`, person, window.Seconds()).Scan(&replaced)
	return replaced, err
}

// SubjectReplacedAfter reports whether Dispatch has named any account replaced at person's address
// after the session generation generation, the one a check of an approver at Dispatch read: a
// recorded replacement whose session_generation is greater. Every revoke carries one, so every
// replacement is placed against it. The caller holds LockPerson, so a revoke that committed while
// it waited is read.
func SubjectReplacedAfter(ctx context.Context, tx pgx.Tx, person string, generation int64) (bool, error) {
	var replaced bool
	err := tx.QueryRow(ctx, `select exists (select 1 from replaced_person_subjects where person = $1 and session_generation > $2)`,
		person, generation).Scan(&replaced)
	return replaced, err
}

// ErrSubjectIsHolder is a revoke naming one account as both the replaced one and the holder.
var ErrSubjectIsHolder = errors.New("store: the replaced account and the holder are one account")

// ReplacedSubjectDeleteSetting is the setting a delete from replaced_person_subjects must name its
// row in first, in the deleting transaction itself, as the JSON array
// jsonb_build_array(pg_current_xact_id()::text, person, issuer, subject)::text
// (replacedSubjectDeleteMarkSQL), before the row's guard lets the delete through (migration 0047,
// guard_replaced_person_subjects). The transaction's id makes a marker left on a pooled connection
// name nothing in any later transaction, and the array names one row whatever its fields hold,
// where fields joined by a separator could name two.
const ReplacedSubjectDeleteSetting = "broker.replaced_subject_delete"

// replacedSubjectDeleteMarkSQL names the row of person $2, issuer $3 and account $4 in
// ReplacedSubjectDeleteSetting ($1) for the rest of the transaction it runs in.
const replacedSubjectDeleteMarkSQL = `select set_config($1, jsonb_build_array(pg_current_xact_id()::text, $2::text, $3::text, $4::text)::text, true)`

// RecordReplacedSubjectTx records, in tx, that the account subject of issuer no longer holds
// person's address and that holder, of the same issuer, does: the first is refused every later
// trade, approval and page sign-in, and any earlier record of the second is deleted, since an
// address can come back to an account it was taken from. sessionGeneration is the address's
// session generation at Dispatch once the replacement committed, which every revoke carries: an
// approval checked at an earlier generation is refused (SubjectReplacedAfter). A repeat keeps the
// moment the account was first recorded and the greater generation. The delete names the holder's
// row in ReplacedSubjectDeleteSetting first, as the row's guard requires. The caller holds
// LockPerson.
//
// It first reads the greatest session generation a revoke it applied at person's address carried
// (person_replacement_generations). A revoke carrying a lower one was superseded by a later
// replacement at the address, so it writes nothing and answers that greatest generation, and the
// caller ends nothing either; migration 0047's comment says why such a revoke can arrive. A revoke
// at or above the greatest generation applies and raises it to its own. It answers nil when it
// applied.
func RecordReplacedSubjectTx(ctx context.Context, tx pgx.Tx, person, issuer, subject, holder string, sessionGeneration int64) (supersededBy *int64, err error) {
	if subject == holder {
		return nil, ErrSubjectIsHolder
	}
	var latest int64
	err = tx.QueryRow(ctx, `select session_generation from person_replacement_generations where person = $1`, person).Scan(&latest)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	case sessionGeneration < latest:
		return &latest, nil
	}
	if _, err := tx.Exec(ctx, `insert into replaced_person_subjects (person, issuer, subject, session_generation) values ($1, $2, $3, $4)
		on conflict (person, issuer, subject) do update
		set session_generation = greatest(replaced_person_subjects.session_generation, excluded.session_generation)
		where greatest(replaced_person_subjects.session_generation, excluded.session_generation)
			is distinct from replaced_person_subjects.session_generation`, person, issuer, subject, sessionGeneration); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, replacedSubjectDeleteMarkSQL,
		ReplacedSubjectDeleteSetting, person, issuer, holder); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `delete from replaced_person_subjects where person = $1 and issuer = $2 and subject = $3`,
		person, issuer, holder); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `insert into person_replacement_generations (person, session_generation) values ($1, $2)
		on conflict (person) do update set session_generation = excluded.session_generation
		where excluded.session_generation > person_replacement_generations.session_generation`, person, sessionGeneration); err != nil {
		return nil, err
	}
	return nil, nil
}
