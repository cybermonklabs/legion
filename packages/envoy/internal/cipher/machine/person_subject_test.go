package machine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// subjectIssuer is the identity provider of every account below.
const subjectIssuer = "https://accounts.google.com"

// pendingLoginOf opens a machine login whose login_hint is person, and answers its record id and
// code.
func pendingLoginOf(t *testing.T, svc *Service, person string) (recordID, code string) {
	t.Helper()
	code, err := openLogin(t, svc, signMachineLogin(t, person, "example-host-devbox", ""))
	if err != nil {
		t.Fatalf("LoginDevice: %v", err)
	}
	view, err := svc.LookupByCode(context.Background(), code)
	if err != nil {
		t.Fatalf("LookupByCode: %v", err)
	}
	return view.RecordID, code
}

// ageReplacements moves every replacement recorded at person's address back past
// ApprovalInFlightWindow. A replacement's replaced_at never moves (migration 0041's guard), so the
// fixture writes it with the guard off.
func ageReplacements(t *testing.T, svc *Service, person string) {
	t.Helper()
	storetest.TestOnlyWriteReplacedSubjects(t, svc.Store, `update replaced_person_subjects set replaced_at = replaced_at - make_interval(secs => $2) where person = $1`,
		person, (ApprovalInFlightWindow + time.Minute).Seconds())
}

// mintedFrom counts the launcher credentials minted from recordID.
func mintedFrom(t *testing.T, svc *Service, recordID string) int {
	t.Helper()
	var n int
	if err := svc.Store.Pool.QueryRow(context.Background(), `select count(*) from launcher_credentials where record_id = $1`, recordID).Scan(&n); err != nil {
		t.Fatalf("count the credentials minted from %s: %v", recordID, err)
	}
	return n
}

// TestAnApprovalAsAReplacedAccountIsRefusedAndRecordsTheHolders: after a revoke, an approval
// carrying no session generation, as an older Dispatch relays it, relayed as the account the
// revoke named replaced is ErrApproverReplaced and mints nothing, the login staying pending;
// approved as the holder it mints a credential recording the holder. An approval naming no
// account and carrying no generation is ErrApprovalNamesNoAccount (an ErrApproverReplaced) for
// ApprovalInFlightWindow after the revoke, the login staying pending, since it may be one checked
// before the address moved; once the window has passed, approved with no account, as an older
// Dispatch or a deployment whose sign-in records no account relays it, it mints one recording none.
func TestAnApprovalAsAReplacedAccountIsRefusedAndRecordsTheHolders(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()
	const person = "ada@example.com"
	replaced := store.IssuerSubject{Issuer: subjectIssuer, Subject: "109" + person}
	holder := store.IssuerSubject{Issuer: subjectIssuer, Subject: "209" + person}
	if _, err := svc.Enroll.RevokePersonSubject(ctx, person, subjectIssuer, replaced.Subject, holder.Subject, 1); err != nil {
		t.Fatalf("RevokePersonSubject: %v", err)
	}

	recordID, code := pendingLoginOf(t, svc, person)
	if _, _, err := applyDecision(ctx, svc, recordID, true, person, code, replaced, nil); !errors.Is(err, ErrApproverReplaced) {
		t.Fatalf("the approval as the replaced account = %v, want %v", err, ErrApproverReplaced)
	}
	if state, _, err := svc.recordState(ctx, recordID); err != nil || state != "pending" {
		t.Fatalf("the login after the refused approval is %q, %v; want pending", state, err)
	}
	_, credentialID, err := applyDecision(ctx, svc, recordID, true, person, code, holder, nil)
	if err != nil {
		t.Fatalf("the approval as the holder: %v", err)
	}
	var recorded store.IssuerSubject
	if err := svc.Store.Pool.QueryRow(ctx, `select approver_issuer, approver_subject from launcher_credentials where id = $1`, credentialID).
		Scan(&recorded.Issuer, &recorded.Subject); err != nil || recorded != holder {
		t.Fatalf("the holder's credential records %+v, %v; want %+v", recorded, err, holder)
	}

	recordID, code = pendingLoginOf(t, svc, person)
	if _, _, err := applyDecision(ctx, svc, recordID, true, person, code, store.IssuerSubject{}, nil); !errors.Is(err, ErrApprovalNamesNoAccount) || !errors.Is(err, ErrApproverReplaced) {
		t.Fatalf("an approval relaying no account just after the revoke = %v, want %v", err, ErrApprovalNamesNoAccount)
	}
	if state, _, err := svc.recordState(ctx, recordID); err != nil || state != "pending" {
		t.Fatalf("the login after the refused approval naming no account is %q, %v; want pending", state, err)
	}
	ageReplacements(t, svc, person)
	_, credentialID, err = applyDecision(ctx, svc, recordID, true, person, code, store.IssuerSubject{}, nil)
	if err != nil {
		t.Fatalf("an approval relaying no account once the window has passed: %v", err)
	}
	var none bool
	if err := svc.Store.Pool.QueryRow(ctx, `select approver_issuer is null and approver_subject is null from launcher_credentials where id = $1`, credentialID).
		Scan(&none); err != nil || !none {
		t.Fatalf("the credential of an approval relaying no account records one (%t, %v), want none", none, err)
	}
}

// TestAnApprovalCheckedBeforeAReplacementIsRefusedByItsGeneration: a revoke recorded the
// replacement at session generation 4. An approval carrying generation 3, the one a check before
// the replacement read, is ErrApprovalCheckedBeforeReplacement (an ErrApproverReplaced), whether it
// names no account or the holder, mints nothing and leaves the login pending, and stays refused once
// ApprovalInFlightWindow has passed, since a generation needs no window; one carrying 4 or more,
// checked after it, is approved, naming no account or the holder.
func TestAnApprovalCheckedBeforeAReplacementIsRefusedByItsGeneration(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()
	const person = "ada@example.com"
	holder := store.IssuerSubject{Issuer: subjectIssuer, Subject: "209" + person}
	if _, err := svc.Enroll.RevokePersonSubject(ctx, person, subjectIssuer, "109"+person, holder.Subject, 4); err != nil {
		t.Fatalf("RevokePersonSubject: %v", err)
	}
	for _, account := range []store.IssuerSubject{{}, holder} {
		recordID, code := pendingLoginOf(t, svc, person)
		_, _, err := applyDecision(ctx, svc, recordID, true, person, code, account, new(int64(3)))
		if !errors.Is(err, ErrApprovalCheckedBeforeReplacement) || !errors.Is(err, ErrApproverReplaced) {
			t.Fatalf("an approval naming %+v checked at generation 3, before the replacement at 4 = %v, want %v", account, err, ErrApprovalCheckedBeforeReplacement)
		}
		if state, _, err := svc.recordState(ctx, recordID); err != nil || state != "pending" || mintedFrom(t, svc, recordID) != 0 {
			t.Fatalf("the login after the refused approval naming %+v is %q, %v, with %d credential(s); want pending with none", account, state, err, mintedFrom(t, svc, recordID))
		}
	}
	ageReplacements(t, svc, person)
	recordID, code := pendingLoginOf(t, svc, person)
	if _, _, err := applyDecision(ctx, svc, recordID, true, person, code, store.IssuerSubject{}, new(int64(3))); !errors.Is(err, ErrApprovalCheckedBeforeReplacement) {
		t.Fatalf("an approval checked at generation 3, once the window after the replacement at 4 has passed = %v, want %v", err, ErrApprovalCheckedBeforeReplacement)
	}
	for _, approval := range []struct {
		account    store.IssuerSubject
		generation int64
	}{{store.IssuerSubject{}, 4}, {holder, 5}} {
		recordID, code := pendingLoginOf(t, svc, person)
		if _, _, err := applyDecision(ctx, svc, recordID, true, person, code, approval.account, new(approval.generation)); err != nil {
			t.Fatalf("an approval naming %+v checked at generation %d, at or after the replacement at 4: %v", approval.account, approval.generation, err)
		}
		if n := mintedFrom(t, svc, recordID); n != 1 {
			t.Fatalf("the approval naming %+v at generation %d minted %d credential(s), want 1", approval.account, approval.generation, n)
		}
	}
}

// TestARevokeAndAnApprovalByTheReplacedAccountNeverLeaveItALiveLogin races, many times over, the
// revoke of a replaced account against an approval whose identity check Dispatch passed before
// the address moved: relayed as that account, relayed naming no account as Dispatch relays a
// credential that records none, both with no session generation, as an older Dispatch relays them,
// and relayed naming no account carrying the generation the check read, the one before the
// generation the revoke carries. Whichever commits first, the login that approval mints is not
// live once both have: an approval that went first is ended by the revoke (which ends the
// address's credentials recording none too), and one that went second is refused, the one naming
// no account with no generation for the revoke's window (ApprovalInFlightWindow), the one carrying
// a generation for being below the revoke's. What makes that hold is the person's lock both take
// (store.LockPerson): without it the approval reads no replaced record while the revoke reads no
// credential, and both commit.
//
// Controls: make store.LockPerson take no lock, and rounds fail with a live credential the
// replaced account approved; drop ApplyDecision's refusal of an approval naming no account
// (ErrApprovalNamesNoAccount), and the second case fails with a live credential recording none;
// drop its refusal by the generation (ErrApprovalCheckedBeforeReplacement), and the third case
// fails with a live credential recording none.
func TestARevokeAndAnApprovalByTheReplacedAccountNeverLeaveItALiveLogin(t *testing.T) {
	for _, approver := range []struct {
		name    string
		account func(person string) store.IssuerSubject
		// checked is the generation the approval carries, nil for none; revoked is the one the
		// revoke carries.
		checked *int64
		revoked int64
	}{
		{"as the replaced account", func(person string) store.IssuerSubject {
			return store.IssuerSubject{Issuer: subjectIssuer, Subject: "109" + person}
		}, nil, 2},
		{"naming no account", func(string) store.IssuerSubject { return store.IssuerSubject{} }, nil, 2},
		{"naming no account, carrying the point the check read", func(string) store.IssuerSubject { return store.IssuerSubject{} },
			new(int64(1)), 2},
	} {
		t.Run(approver.name, func(t *testing.T) {
			svc := newFixture(t)
			ctx := context.Background()
			const rounds = 200
			var approvedFirst, refused int
			for i := range rounds {
				person := fmt.Sprintf("race-%d@example.com", i)
				recordID, code := pendingLoginOf(t, svc, person)
				start := make(chan struct{})
				var wg sync.WaitGroup
				var approveErr, revokeErr error
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					_, _, approveErr = applyDecision(ctx, svc, recordID, true, person, code, approver.account(person), approver.checked)
				}()
				go func() {
					defer wg.Done()
					<-start
					_, revokeErr = svc.Enroll.RevokePersonSubject(ctx, person, subjectIssuer, "109"+person, "209"+person, approver.revoked)
				}()
				close(start)
				wg.Wait()
				if revokeErr != nil {
					t.Fatalf("round %d: the revoke: %v", i, revokeErr)
				}
				switch {
				case approveErr == nil:
					approvedFirst++
				case errors.Is(approveErr, ErrApproverReplaced):
					refused++
				default:
					t.Fatalf("round %d: the approval: %v", i, approveErr)
				}
				var live int
				if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from launcher_credentials where record_id = $1 and revoked_at is null`, recordID).
					Scan(&live); err != nil {
					t.Fatalf("round %d: read what is live: %v", i, err)
				}
				if live != 0 {
					t.Fatalf("round %d: after the revoke and the approval (approval error %v) the login holds %d live credential(s), want none", i, approveErr, live)
				}
			}
			t.Logf("%d rounds: the approval committed first in %d and was refused in %d", rounds, approvedFirst, refused)
		})
	}
}
