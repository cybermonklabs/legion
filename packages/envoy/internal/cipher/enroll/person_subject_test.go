package enroll

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/store"
)

// raceIssuer and raceMachine are the identity provider and the machine of every trade the race
// below makes.
const (
	raceIssuer  = "https://accounts.google.com"
	raceMachine = "example-fleet@example-project.iam.gserviceaccount.com"
)

// tradeSignIn makes one sign-in trade of person on a box key of its own as account, the two
// writes the token exchange makes in its transaction: the sign-in (RecordPersonSignIn) and the
// launcher credential it holds (IssueTokenLoginTx). It answers the key's thumbprint and the
// trade's error, store.ErrSubjectReplaced among them.
func tradeSignIn(ctx context.Context, svc *Service, person string, account store.IssuerSubject) (string, error) {
	key, err := proof.NewKey()
	if err != nil {
		return "", err
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		return "", err
	}
	jwk, err := (jose.JSONWebKey{Key: &key.PublicKey}).MarshalJSON()
	if err != nil {
		return "", err
	}
	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		return thumbprint, err
	}
	defer tx.Rollback(ctx)
	if err := RecordPersonSignIn(ctx, tx, store.SignInTrade{
		Person: person, Thumbprint: thumbprint, MachineAccount: raceMachine, Account: account, EndUnrecorded: true,
	}, google.PersonSubject(person, raceMachine, thumbprint)); err != nil {
		return thumbprint, err
	}
	if _, err := svc.IssueTokenLoginTx(ctx, tx, TokenLogin{
		Subject: google.PersonSubject(person, raceMachine, thumbprint), Service: "example-fleet",
		Host: "example-project/example-zone-a/example-vm-7", Operator: person,
		TokenExpires: time.Now().Add(time.Hour), Thumbprint: thumbprint, JWK: jwk, Expires: time.Now().Add(time.Hour),
	}); err != nil {
		return thumbprint, err
	}
	return thumbprint, tx.Commit(ctx)
}

// TestARevokeAndATradeOfTheReplacedAccountNeverLeaveItALiveSignIn races, many times over, the
// revoke of a replaced account against a sign-in trade of that account on a box key it never
// traded — the trade a person token minted before the address moved makes, which no row conflicts
// with. Whichever commits first, nothing of the replaced account is live once both have: a trade
// that went first is ended by the revoke, sign-in and credential, and one that went second is
// refused. What makes that hold is the person's lock both take (store.LockPerson): without it the
// trade reads no replaced record while the revoke reads no sign-in, and both commit.
//
// Control: make store.LockPerson take no lock, and rounds fail with a live sign-in of the
// replaced account.
func TestARevokeAndATradeOfTheReplacedAccountNeverLeaveItALiveSignIn(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	const rounds = 200
	var tradedFirst, refused int
	for i := range rounds {
		person := fmt.Sprintf("race-%d@example.com", i)
		replaced := store.IssuerSubject{Issuer: raceIssuer, Subject: "109" + person}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var thumbprint string
		var tradeErr, revokeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			thumbprint, tradeErr = tradeSignIn(ctx, svc, person, replaced)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, revokeErr = svc.RevokePersonSubject(ctx, person, raceIssuer, replaced.Subject, "209"+person, 1)
		}()
		close(start)
		wg.Wait()
		if revokeErr != nil {
			t.Fatalf("round %d: the revoke: %v", i, revokeErr)
		}
		switch {
		case tradeErr == nil:
			tradedFirst++
		case errors.Is(tradeErr, store.ErrSubjectReplaced):
			refused++
		default:
			t.Fatalf("round %d: the trade: %v", i, tradeErr)
		}
		var signIns, credentials int
		if err := svc.Store.Pool.QueryRow(ctx, `select
			(select count(*) from person_signins where person = $1 and revoked_at is null),
			(select count(*) from launcher_credentials where key_thumbprint = $2 and revoked_at is null)`,
			person, thumbprint).Scan(&signIns, &credentials); err != nil {
			t.Fatalf("round %d: read what is live: %v", i, err)
		}
		if signIns != 0 || credentials != 0 {
			t.Fatalf("round %d: after the revoke and the trade (trade error %v) the replaced account holds %d live sign-in(s) and %d live credential(s), want none",
				i, tradeErr, signIns, credentials)
		}
	}
	t.Logf("%d rounds: the trade committed first in %d and was refused in %d", rounds, tradedFirst, refused)
}

// TestARevokeAndAPageSignInOfTheReplacedAccountNeverLeaveItALiveSession races, many times over,
// the revoke of a replaced account against that account's sign-in to the broker's own approval
// page, the callback of a sign-in the provider completed before the revoke reached the broker.
// Whichever commits first, the replaced account holds no approval-page session once both have: a
// sign-in that went first writes a session the revoke deletes, and one that went second is
// refused. What makes that hold is the person's lock both take (store.LockPerson): without it the
// sign-in reads no replaced record while the revoke deletes no session, and both commit.
//
// Control: make store.LockPerson take no lock, and rounds fail with a live session of the
// replaced account.
func TestARevokeAndAPageSignInOfTheReplacedAccountNeverLeaveItALiveSession(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	const rounds = 200
	var signedInFirst, refused int
	for i := range rounds {
		person := fmt.Sprintf("page-race-%d@example.com", i)
		replaced := store.IssuerSubject{Issuer: raceIssuer, Subject: "109" + person}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var signInErr, revokeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			signInErr = svc.Store.CreateBrowserSession(ctx, store.BrowserSignIn{
				Hash: sha256.Sum256([]byte(person)), Login: person, Account: replaced, Expires: time.Now().Add(time.Hour),
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, revokeErr = svc.RevokePersonSubject(ctx, person, raceIssuer, replaced.Subject, "209"+person, 1)
		}()
		close(start)
		wg.Wait()
		if revokeErr != nil {
			t.Fatalf("round %d: the revoke: %v", i, revokeErr)
		}
		switch {
		case signInErr == nil:
			signedInFirst++
		case errors.Is(signInErr, store.ErrSubjectReplaced):
			refused++
		default:
			t.Fatalf("round %d: the sign-in: %v", i, signInErr)
		}
		var sessions int
		if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from browser_sessions where login = $1`, person).Scan(&sessions); err != nil {
			t.Fatalf("round %d: read what is live: %v", i, err)
		}
		if sessions != 0 {
			t.Fatalf("round %d: after the revoke and the page sign-in (sign-in error %v) the replaced account holds %d approval-page session(s), want none",
				i, signInErr, sessions)
		}
	}
	t.Logf("%d rounds: the sign-in committed first in %d and was refused in %d", rounds, signedInFirst, refused)
}

// TestALateRevokeBelowTheAppliedGenerationChangesNothing replays a revoke that reached the broker
// late, after the replacement that superseded it. B takes person's address from A at session
// generation 1 (the revoke of A, holder B, carrying 1, applies); A takes it back at generation 2
// (the revoke of B, holder A, carrying 2, applies); A trades. Then the first revoke of A, carrying
// 1, arrives late, as one Dispatch gave up on and the broker still committed: it answers
// superseded by generation 2 and writes and ends nothing, so A, which holds the address, trades
// and keeps its sign-ins, and B stays refused.
//
// Control: make store.RecordReplacedSubjectTx skip its read of the applied generation, and the late
// revoke applies: A is refused and B is not.
func TestALateRevokeBelowTheAppliedGenerationChangesNothing(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	const person = "late@example.com"
	a := store.IssuerSubject{Issuer: raceIssuer, Subject: "109" + person}
	b := store.IssuerSubject{Issuer: raceIssuer, Subject: "209" + person}
	// revoke revokes replaced in favour of holder at person, carrying generation, failing the test
	// on an error.
	revoke := func(person string, replaced, holder store.IssuerSubject, generation int64) SubjectRevoked {
		t.Helper()
		revoked, err := svc.RevokePersonSubject(ctx, person, raceIssuer, replaced.Subject, holder.Subject, generation)
		if err != nil {
			t.Fatalf("the revoke of %s, holder %s, at %s: %v", replaced.Subject, holder.Subject, person, err)
		}
		return revoked
	}
	if revoked := revoke(person, a, b, 1); revoked.SupersededBy != nil {
		t.Fatalf("B's replacement of A at generation 1 answered superseded by %d, want it applied", *revoked.SupersededBy)
	}
	if revoked := revoke(person, b, a, 2); revoked.SupersededBy != nil {
		t.Fatalf("A's taking the address back at generation 2 answered superseded by %d, want it applied", *revoked.SupersededBy)
	}
	if _, err := tradeSignIn(ctx, svc, person, a); err != nil {
		t.Fatalf("A's trade once it took the address back: %v", err)
	}

	late := revoke(person, a, b, 1)
	if late.SupersededBy == nil || *late.SupersededBy != 2 || late.SignIns != 0 || late.MachineLogins != 0 || late.Tokens != 0 || late.ApprovalSessions != 0 {
		var by any = "nothing"
		if late.SupersededBy != nil {
			by = *late.SupersededBy
		}
		t.Errorf("the late revoke of A at generation 1 ended %d sign-in(s), %d machine login(s), %d token(s) and %d approval session(s), superseded by %v; want superseded by 2 with nothing ended",
			late.SignIns, late.MachineLogins, late.Tokens, late.ApprovalSessions, by)
	}
	if _, err := tradeSignIn(ctx, svc, person, a); err != nil {
		t.Errorf("after the late revoke, A's trade = %v, want it recorded: A holds the address", err)
	}
	if _, err := tradeSignIn(ctx, svc, person, b); !errors.Is(err, store.ErrSubjectReplaced) {
		t.Errorf("after the late revoke, B's trade = %v, want %v: A took the address from B", err, store.ErrSubjectReplaced)
	}
	var live int
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from person_signins where person = $1 and subject = $2 and revoked_at is null`,
		person, a.Subject).Scan(&live); err != nil || live != 2 {
		t.Errorf("after the late revoke, A holds %d live sign-in(s), %v; want its 2, none ended", live, err)
	}
}
