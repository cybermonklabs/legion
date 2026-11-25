package enroll

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/store"
)

// SubjectReplacedActor is the actor every row RevokePersonSubject writes records: the broker,
// ending what an account holds once another account holds its address.
const SubjectReplacedActor = "broker:person-subject-replaced"

// subjectReplacedReason is what each enrollment the revoke ends records as the reason.
const subjectReplacedReason = "another account holds its person's address now"

// SubjectRevoked is what one RevokePersonSubject ended.
type SubjectRevoked struct {
	// SignIns is how many of the account's sign-ins on a machine it ended.
	SignIns int
	// MachineLogins is how many of the account's own machines' logins it ended.
	MachineLogins int
	// Tokens is how many live access tokens ended with those.
	Tokens int
	// ApprovalSessions is how many of the account's sessions on the approval page it ended.
	ApprovalSessions int
	// SupersededBy is, for a revoke a later replacement at the address superseded, the greatest
	// session generation a revoke the broker applied there carried, which is above this revoke's:
	// such a revoke wrote and ended nothing (store.RecordReplacedSubjectTx). Nil when it applied.
	SupersededBy *int64
}

// RevokePersonSubject records that the account subject of issuer no longer holds person's
// address, which the account holder of the same issuer holds now, and ends everything the first
// holds under the address, in one transaction: the broker's half of POST
// /v1/person-subjects/revoke, which Dispatch sends when another Google account takes an address
// it holds for a person, whatever that person's member kind.
//
// Which credentials and sessions are the replaced account's is read off the account each records,
// never off when it was issued: a request whose identity check passed before the address moved
// can reach the broker at any later moment, and a Google identity token minted for the replaced
// account before the move stays valid for an hour, so a cutoff by time keeps some of what that
// account made and ends some of what the new holder made. Four things happen together:
//
//   - the account is recorded replaced under the address (store.RecordReplacedSubjectTx), with
//     sessionGeneration, the address's session generation at Dispatch once the replacement
//     committed, so every later sign-in trade of it
//     (store.RecordPersonSignIn), every later approval of a machine login by it, and every
//     approval Dispatch checked at an earlier generation, whatever account it names
//     (machine.Service.ApplyDecision), and every later sign-in of it to the approval page
//     (store.Store.CreateBrowserSession) is refused, and any such record of the holder is deleted,
//     since an address can come back to an account it was taken from;
//   - every session of person on the broker's own approval page that the account signed in, or
//     that records no account, is deleted (store.DeleteSubjectBrowserSessionsTx), so its cookie
//     opens no page and decides no request from then on, a decision already posted under it
//     included (store.HoldBrowserSessionTx);
//   - every live sign-in of person on a machine that the account made, or that records no account
//     (store.SubjectSignInsTx), ends exactly as the person's own revoke ends one (endSignIn): the
//     row, the access tokens its trades minted, and the launcher credential it holds with every
//     session that credential enrolled;
//   - every unrevoked login of person's own machines — minted from a launcher_credential record
//     person approves, naming no service — that the account approved, or that records no
//     approving account, ends exactly as revoking a machine login ends one (endCredential). A
//     service's login is never touched: it acts for no person, and whoever approved it, it is not
//     what the address holds.
//
// A row recording no account is one from before migration 0042, or a machine login whose approval
// an older Dispatch relayed without the approving account: it might be the replaced account's, so
// it ends, and if it was the holder's, the holder's machine signs in or logs in again, and the
// holder signs in to the approval page again.
//
// It takes the person's lock first (store.LockPerson), which every trade, approval and page
// sign-in of person takes before it reads whether its account is replaced, and every decision on
// the page before it reads its session again. So one of the replaced account's either committed
// before this read the person's credentials and sessions, and the credential or session it made
// is ended here, or waits for this to commit and is refused: nothing the account makes outlives
// the revoke, and none of its decisions commits after it.
//
// A repeat ends nothing and answers zeros; it raises the recorded session generation to the
// greater of the two, never lowering it. A revoke carrying a session generation below the greatest
// one a revoke applied at the address carried writes and ends nothing, and answers SupersededBy:
// a later replacement at the address has been applied since (store.RecordReplacedSubjectTx). holder
// equal to subject is store.ErrSubjectIsHolder.
func (s *Service) RevokePersonSubject(ctx context.Context, person, issuer, subject, holder string, sessionGeneration int64) (SubjectRevoked, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return SubjectRevoked{}, err
	}
	defer tx.Rollback(ctx)
	if err := store.LockPerson(ctx, tx, person); err != nil {
		return SubjectRevoked{}, err
	}
	supersededBy, err := store.RecordReplacedSubjectTx(ctx, tx, person, issuer, subject, holder, sessionGeneration)
	if err != nil {
		return SubjectRevoked{}, err
	}
	if supersededBy != nil {
		return SubjectRevoked{SupersededBy: supersededBy}, nil
	}
	account := store.IssuerSubject{Issuer: issuer, Subject: subject}
	var revoked SubjectRevoked
	if revoked.ApprovalSessions, err = store.DeleteSubjectBrowserSessionsTx(ctx, tx, person, account); err != nil {
		return SubjectRevoked{}, err
	}
	signIns, err := store.SubjectSignInsTx(ctx, tx, person, account)
	if err != nil {
		return SubjectRevoked{}, err
	}
	for _, id := range signIns {
		ended, tokens, err := endSignIn(ctx, tx, id, person, SubjectReplacedActor, subjectReplacedReason)
		if err != nil {
			return SubjectRevoked{}, err
		}
		if ended {
			revoked.SignIns++
			revoked.Tokens += tokens
		}
	}
	logins, err := subjectMachineLogins(ctx, tx, person, account)
	if err != nil {
		return SubjectRevoked{}, err
	}
	for _, login := range logins {
		tokens, err := endCredential(ctx, tx, login.id, login.host, nil, SubjectReplacedActor, subjectReplacedReason)
		if err != nil {
			return SubjectRevoked{}, err
		}
		revoked.MachineLogins++
		revoked.Tokens += tokens
	}
	return revoked, tx.Commit(ctx)
}

// subjectLogin is one machine login subjectMachineLogins holds.
type subjectLogin struct{ id, host string }

// subjectMachineLogins takes, in tx, the row of every unrevoked login of person's own machines
// that account approved or that records no approving account, and answers them. A person's own
// machine's login is one minted from a launcher_credential record person approves and naming no
// service, the logins revokeCredential lets person revoke without services: a service's login,
// and a sign-in's credential, which has no record, are not among them. Expired logins are, since
// the sessions a login enrolled outlive its expiry and revoking it ends them.
func subjectMachineLogins(ctx context.Context, tx pgx.Tx, person string, account store.IssuerSubject) ([]subjectLogin, error) {
	rows, err := tx.Query(ctx, `select c.id::text, c.host from launcher_credentials c
		join credential_requests r on r.id = c.record_id and r.kind = 'launcher_credential'
		where r.approver = $1 and c.service is null and c.revoked_at is null
			and (c.approver_subject is null or (c.approver_issuer, c.approver_subject) = ($2, $3))
		order by c.id for no key update of c`, person, account.Issuer, account.Subject)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (subjectLogin, error) {
		var login subjectLogin
		return login, row.Scan(&login.id, &login.host)
	})
}
