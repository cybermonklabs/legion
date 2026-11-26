package approval_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/approval"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/store"
)

// TestTheReplacedAccountsApprovalPageEndsWithTheRevoke: when another Google account takes ada's
// address and Dispatch names the account it replaced (POST /v1/person-subjects/revoke), the
// replaced account's session on the approval page ends in the revoke. The page it already has
// open decides nothing, Approve or Deny; a decision whose route read the session before the
// revoke and whose body arrived after it decides nothing; and opening the page again starts a
// sign-in. A new sign-in of the replaced account is refused 403 naming why, and makes no session.
// The session of the account that holds the address now survives the revoke and decides.
//
// Controls: leave the sessions out of the revoke (enroll.Service.RevokePersonSubject), and the
// old page approves; drop the read of the replaced account from the page's sign-in
// (store.Store.CreateBrowserSession), and the replaced account signs in again; drop the read of
// the session from the decision's own transaction (store.HoldBrowserSessionTx), and the held
// decision approves.
func TestTheReplacedAccountsApprovalPageEndsWithTheRevoke(t *testing.T) {
	r := newRig(t)
	enrollmentID, key := r.benSession(t, "ben-box")
	p := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")
	page := "/approve/" + p.recordID
	// as is ada's claims at the provider as the Google account sub: the account Google gave her
	// address first, and the one it gave it to after, share the address and differ in `sub`.
	const replacedSub, holderSub = "sub-replaced-" + ada, "sub-holder-" + ada
	as := func(sub string) map[string]any {
		claims := person(ada)
		claims["sub"] = sub
		return claims
	}

	old := r.browser(t)
	old.SignIn(t, p.recordID, as(replacedSub))
	form := decisionForm(t, old.Page(t, p.recordID).Body, p.recordID)
	if form == nil {
		t.Fatal("the replaced account's page offers no decision before the revoke")
	}
	// held is an Approve from the old page whose route has read the session, live, and whose body
	// the page has not read yet: the session the route hands the page is this one.
	held := httptest.NewRequest(http.MethodPost, r.url+"/approve", strings.NewReader(decision(form, "approve").Encode()))
	held.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	held.Header.Set("Origin", r.url)
	held.AddCookie(&http.Cookie{Name: "__Host-broker-session", Value: old.Cookie(t, "__Host-broker-session")})
	heldSession, ok := r.page.Authenticate(httptest.NewRecorder(), held)
	if !ok || heldSession.Login != ada {
		t.Fatalf("the held decision's session before the revoke = %q (%t), want ada's", heldSession.Login, ok)
	}
	holder := r.browser(t)
	holder.SignIn(t, p.recordID, as(holderSub))

	if ended := r.revokeSubject(t, ada, replacedSub, holderSub); ended != 1 {
		t.Errorf("the revoke ended %d approval-page session(s), want 1: the replaced account's, and not the holder's", ended)
	}

	t.Run("the old page decides nothing", func(t *testing.T) {
		for _, value := range []string{"approve", "deny"} {
			requireSignInLink(t, old.FromPage(t, "/approve", decision(form, value)), p.recordID)
		}
		r.requirePending(t, p.recordID)
		if res := old.Get(t, page); res.Status != http.StatusFound {
			t.Errorf("the old browser opening the page again = %d, want 302 to the sign-in provider: its session ended", res.Status)
		}
	})

	t.Run("a decision whose body arrives after the revoke decides nothing", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.page.ServeDecide(rec, held, heldSession)
		requireSignInLink(t, ciphertest.Response{Status: rec.Code, Header: rec.Header(), Body: rec.Body.String()}, p.recordID)
		r.requirePending(t, p.recordID)
	})

	t.Run("a new sign-in of the replaced account is refused", func(t *testing.T) {
		before := r.sessions(t)
		fresh := r.browser(t)
		res := fresh.Get(t, fresh.Authorize(t, fresh.StartSignIn(t, page), as(replacedSub)))
		if res.Status != http.StatusForbidden || !strings.Contains(pageText(t, res.Body), "no longer holds "+ada+": another account holds that address now") {
			t.Fatalf("the replaced account's sign-in after the revoke = %d, want 403 saying another account holds %s:\n%s", res.Status, ada, res.Body)
		}
		if res.Cookie("__Host-broker-session") != nil {
			t.Error("the refused sign-in set a session cookie")
		}
		if after := r.sessions(t); after != before {
			t.Errorf("the refused sign-in left %d sessions, want the %d before it", after, before)
		}
		r.requirePending(t, p.recordID)
	})

	t.Run("the holder's session survives and decides", func(t *testing.T) {
		holder.Decide(t, p.recordID, true)
		if state, grants := r.state(t, p.recordID), r.grants(t, p.recordID); state != "approved" || grants != 1 {
			t.Fatalf("after the holder's approval the record is %s with %d grants, want approved with 1", state, grants)
		}
	})
}

// TestARevokeAndAPageDecisionOfTheReplacedAccountNeverLeaveADecisionAfterIt races, many times
// over, the revoke of a replaced account against an Approve that account's approval-page session
// posts, the route having read the session live before the race starts. Whichever commits first,
// no decision of the replaced account commits after the revoke: a decision that went first has
// committed before the revoke reads the person's sessions, and one that went second reads the
// session the revoke deleted and decides nothing. The record is read the moment the revoke
// returns, so a record still pending then and approved once both are done was approved by a
// decision that committed after the revoke. What makes that hold is the person's lock the
// decision's transaction takes before it reads its session again (store.HoldBrowserSessionTx):
// without it the decision reads the session live before the revoke commits and commits after it.
//
// Controls: make store.HoldBrowserSessionTx take no lock, or ignore the session it reads, and
// rounds fail with the record approved after the revoke.
func TestARevokeAndAPageDecisionOfTheReplacedAccountNeverLeaveADecisionAfterIt(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	enrollmentID, key := r.benSession(t, "ben-box")
	const rounds = 200
	// lifetime is the grant each round asks for. An approved round leaves a live grant, which an
	// ask of the same lifetime would be handed back instead of a record (requests.Machine.Create
	// reuses it), so the round after asks for a second more; a round that decided nothing leaves
	// its request pending, which the next ask, of the same lifetime, joins.
	lifetime := 600
	var decidedFirst, refused int
	for i := range rounds {
		replaced, holder := fmt.Sprintf("sub-replaced-%d", i), fmt.Sprintf("sub-holder-%d", i)
		p := r.request(t, enrollmentID, key, "deploy", &lifetime, "", "TEST_SECRET")
		secret := make([]byte, 32)
		_, _ = rand.Read(secret) // crypto/rand.Read never fails.
		if err := r.st.CreateBrowserSession(ctx, store.BrowserSignIn{
			Hash: sha256.Sum256(secret), Login: ada, Account: store.IssuerSubject{Issuer: r.issuer.URL(), Subject: replaced},
			Expires: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("round %d: sign the replaced account in to the page: %v", i, err)
		}
		expires := time.Now().Add(time.Hour).Unix()
		form := url.Values{
			"decision": {"approve"}, "record": {p.recordID}, "printed": {p.recordID},
			"expires": {strconv.FormatInt(expires, 10)}, "token": {approval.PageToken(secret, expires, []string{p.recordID})},
		}
		post := httptest.NewRequest(http.MethodPost, r.url+"/approve", strings.NewReader(form.Encode()))
		post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		post.Header.Set("Origin", r.url)
		post.AddCookie(&http.Cookie{Name: "__Host-broker-session", Value: base64.RawURLEncoding.EncodeToString(secret)})
		session, ok := r.page.Authenticate(httptest.NewRecorder(), post)
		if !ok || session.Login != ada {
			t.Fatalf("round %d: the decision's session before the race = %q (%t), want ada's", i, session.Login, ok)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		answer := httptest.NewRecorder()
		var revokeErr, readErr error
		var atRevoke string
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			r.page.ServeDecide(answer, post, session)
		}()
		go func() {
			defer wg.Done()
			<-start
			// The decision reads its form and checks its token before its transaction starts, so
			// a revoke started with it nearly always takes the lock first; a head start of up to
			// three milliseconds, by round, lets the decision take it first in some rounds too.
			time.Sleep(time.Duration(i%4) * time.Millisecond)
			if _, revokeErr = r.enrollments.RevokePersonSubject(ctx, ada, r.issuer.URL(), replaced, holder, 1); revokeErr != nil {
				return
			}
			detail, err := r.machine.ReadRecord(ctx, p.recordID)
			atRevoke, readErr = detail.State, err
		}()
		close(start)
		wg.Wait()
		if revokeErr != nil || readErr != nil {
			t.Fatalf("round %d: the revoke: %v; the read after it: %v", i, revokeErr, readErr)
		}
		final := r.state(t, p.recordID)
		if atRevoke == "pending" && final != "pending" {
			t.Fatalf("round %d: the record was pending when the revoke returned and is %s now (answer %d): the replaced account's decision committed after the revoke",
				i, final, answer.Code)
		}
		switch final {
		case "approved":
			decidedFirst++
			lifetime++
		case "pending":
			refused++
			requireSignInLink(t, ciphertest.Response{Status: answer.Code, Header: answer.Header(), Body: answer.Body.String()}, p.recordID)
		default:
			t.Fatalf("round %d: the record is %s after the race, want approved or still pending", i, final)
		}
	}
	t.Logf("%d rounds: the decision committed first in %d and decided nothing in %d", rounds, decidedFirst, refused)
}
