// session_name_holder_test.go is the rule a carry rests on: a grant follows a session name under
// one principal, so the name belongs to one principal (dispatch://INFRA-1167). Create binds the
// name it enrols to the principal its caller proved, in the enrollment's own transaction and on
// the one table the token exchange binds every token's name in (oauth_session_bindings), and a
// name another principal holds is refused outright. What the carry does once the name is one
// principal's is session_adoption_test.go's.
package enroll

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// sessionNameHolder is the principal the broker records as holding a session name, "" for a name
// no principal holds. It reads oauth_session_bindings itself, because what is being measured is
// that an enrollment writes the same row the token exchange writes.
func sessionNameHolder(t *testing.T, svc *Service, session string) string {
	t.Helper()
	var principal string
	err := svc.Store.Pool.QueryRow(context.Background(),
		`select coalesce((select principal from oauth_session_bindings where session=$1), '')`, session).Scan(&principal)
	if err != nil {
		t.Fatalf("read the holder of session name %s: %v", session, err)
	}
	return principal
}

// enrollmentsNaming counts the enrollment rows carrying a session name, so a refused enrolment
// shows as having written none.
func enrollmentsNaming(t *testing.T, svc *Service, session string) int {
	t.Helper()
	var rows int
	if err := svc.Store.Pool.QueryRow(context.Background(),
		`select count(*) from enrollments where session_id=$1`, session).Scan(&rows); err != nil {
		t.Fatalf("count the enrollments naming session %s: %v", session, err)
	}
	return rows
}

// oldPodAccount and movedPodAccount are the two ServiceAccounts of a deployment an operator
// moves: its pods ran as the first and run as the second now. A test that pairs them says so
// (pairAccounts), and nothing else pairs them.
const (
	oldPodAccount   = "system:serviceaccount:legion:worker"
	movedPodAccount = "system:serviceaccount:legion-next:worker"
)

// pairAccounts pairs the ServiceAccount subjects a and b for session names, both ways, as
// cmd/broker reads one pair of BROKER_SESSION_ACCOUNT_PAIRS into the store an enrollment binds its
// name through (oauthstore.Store.SessionPairs).
func pairAccounts(t *testing.T, svc *Service, a, b string) {
	t.Helper()
	binder, ok := svc.Sessions.(*oauthstore.Store)
	if !ok {
		t.Fatalf("the service binds session names through %T, want the *oauthstore.Store cmd/broker wires", svc.Sessions)
	}
	first, second := exchange.AccountPrincipal(a), exchange.AccountPrincipal(b)
	binder.SessionPairs = map[string][]string{first: {second}, second: {first}}
}

// enrollmentTookName is the line Create logs once an enrollment that took its session name from
// the account paired with its own has committed.
const enrollmentTookName = "broker: an enrollment took a session name from the account paired with its own"

// nameTake is one enrollmentTookName line, as its level and attributes say it.
type nameTake struct {
	Level                         slog.Level
	Session, From, To, Enrollment string
}

// nameTakes is every enrollmentTookName line capture holds, in the order they were logged.
func nameTakes(capture *logtest.Recorder) []nameTake {
	var takes []nameTake
	for _, r := range capture.Records() {
		if r.Message != enrollmentTookName {
			continue
		}
		take := nameTake{Level: r.Level}
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "session":
				take.Session = a.Value.String()
			case "from":
				take.From = a.Value.String()
			case "to":
				take.To = a.Value.String()
			case "enrollment":
				take.Enrollment = a.Value.String()
			}
			return true
		})
		takes = append(takes, take)
	}
	return takes
}

// adoption is one enrollment.adopted row: the predecessor it took from, the rule it moved by,
// and how many grants, requests and withheld names moved.
type adoption struct {
	From                       uuid.UUID
	Rule                       string
	Grants, Requests, Withheld int64
}

// adoptionsOf is every enrollment.adopted row on enrollment to, in the order of the predecessor
// each names.
func adoptionsOf(t *testing.T, svc *Service, to uuid.UUID) []adoption {
	t.Helper()
	rows, err := svc.Store.Pool.Query(context.Background(), `select (detail->>'from')::uuid, coalesce(detail->>'rule',''),
			(detail->>'grants')::bigint, (detail->>'requests')::bigint, (detail->>'withheld_secrets')::bigint
		from audit where kind='enrollment.adopted' and enrollment_id=$1 order by (detail->>'from')::uuid`, to)
	if err != nil {
		t.Fatalf("read the enrollment.adopted rows of %s: %v", to, err)
	}
	adoptions, err := pgx.CollectRows(rows, pgx.RowToStructByPos[adoption])
	if err != nil {
		t.Fatalf("read the enrollment.adopted rows of %s: %v", to, err)
	}
	return adoptions
}

// withheldFromReplacedPod is the name withheld from the replaced pod's session (replacedPod).
const withheldFromReplacedPod = "AUTO_TOKEN"

// replacedPod is a role's pod running as oldPodAccount that enrolled session, holds a live grant,
// waits on a request nobody has answered yet and has had withheldFromReplacedPod taken back from
// it, and was then replaced: its launcher revoked its enrollment, as the daemon does when it lets
// the role's process go, which leaves the row dormant holding all three for the session's next
// enrollment. It answers the launcher's credential, the pod's enrollment, its grant and its
// pending request.
func replacedPod(t *testing.T, svc *Service, pod func(account, uid string) Enrollment) (Credential, Enrollment, uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	cluster := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
	old, err := svc.Create(ctx, cluster, pod(oldPodAccount, "pod-uid-old"))
	if err != nil {
		t.Fatalf("the old account's pod enrolment: %v", err)
	}
	grant := insertLiveGrant(t, svc, old.ID)
	request := insertPendingRequest(t, svc, old.ID)
	withholdSecret(t, svc, old.ID, withheldFromReplacedPod)
	if err := svc.Revoke(ctx, cluster, old.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if left := readCarryState(t, svc, old.ID); !left.Dormant || left.LiveGrants != 1 || left.Pending != 1 {
		t.Fatalf("the replaced pod's enrollment is %+v, want it dormant holding its grant and its pending request: this test measures what its successor takes", left)
	}
	return cluster, old, grant, request
}

// tookAllOfIt fails t unless enrollment moved holds everything the replaced pod old held — its
// grant, its pending request, still pending, and its withheld name — and old holds none of it and
// is no longer dormant, with the trail the session rule writes: one enrollment.adopted row from
// old by rule session and one grant.moved row naming the grant and old.
func tookAllOfIt(t *testing.T, svc *Service, old, moved Enrollment, grant uuid.UUID, request string) {
	t.Helper()
	if holder := grantHolder(t, svc, grant); holder != moved.ID {
		t.Fatalf("grant %s hangs off %s, want the moved account's pod %s: it takes the grant the replaced pod held", grant, holder, moved.ID)
	}
	if holder := requestHolder(t, svc, request); holder != moved.ID {
		t.Fatalf("request %s hangs off %s, want the moved account's pod %s", request, holder, moved.ID)
	}
	if state, decidedBy, cancellations, _ := requestState(t, svc, request); state != "pending" || decidedBy != "" || cancellations != 0 {
		t.Fatalf("request %s is %s, decided by %q, with %d cancellations; want it still pending: the move answers nothing", request, state, decidedBy, cancellations)
	}
	if got, want := withheldNames(t, svc, moved.ID), []string{withheldFromReplacedPod}; !slices.Equal(got, want) {
		t.Fatalf("the moved account's pod withholds %v, want %v: a new account does not give back what an operator took back from the session", got, want)
	}
	if got := withheldNames(t, svc, old.ID); len(got) != 0 {
		t.Fatalf("the replaced pod still withholds %v, want nothing: every withheld row moves to the enrollment the session runs on now", got)
	}
	if left := readCarryState(t, svc, old.ID); !left.Ended || left.Dormant || left.LiveGrants != 0 || left.Pending != 0 {
		t.Fatalf("the replaced pod is %+v, want it ended, no longer dormant and holding nothing, so the carry sweep has nothing of it to revoke", left)
	}
	// The grant's request moves beside the pending one: insertLiveGrant writes the granted
	// request its grant was issued for, and the session rule moves every request off the row.
	want := []adoption{{From: old.ID, Rule: "session", Grants: 1, Requests: 2, Withheld: 1}}
	if got := adoptionsOf(t, svc, moved.ID); !slices.Equal(got, want) {
		t.Fatalf("the moved account's pod has enrollment.adopted rows %+v, want %+v: the paired take is the session rule's, with its trail", got, want)
	}
	if rows := auditRows(t, svc, moved.ID, "grant.moved"); rows != 1 {
		t.Fatalf("the moved account's pod has %d grant.moved rows, want one for the one grant", rows)
	}
	var from string
	if err := svc.Store.Pool.QueryRow(context.Background(), `select detail->>'from' from audit where kind='grant.moved' and grant_id=$1 and enrollment_id=$2`,
		grant, moved.ID).Scan(&from); err != nil || from != old.ID.String() {
		t.Fatalf("grant %s is recorded as moved from %q (%v), want the replaced pod %s", grant, from, err, old.ID)
	}
}

// TestASessionNameAnotherPrincipalHoldsIsRefusedAndTakesNothing: the second principal reaching for
// a name the first holds is refused the whole enrollment — ErrSessionHeld, no row of its own, and
// nothing adopted — and the grant the first principal's session was approved for stays exactly
// where it was, held by the box the session left for its own session to take.
//
// The two arms are the two ways a second principal turns up under one name: another fleet's
// Google account, whose machine enrols a session of its own, and another person, signed in on a
// box of their own.
func TestASessionNameAnotherPrincipalHoldsIsRefusedAndTakesNothing(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		boxes func(t *testing.T, svc *Service) (Credential, Credential)
	}{
		{
			name: "a second Google account of its own fleet",
			boxes: func(t *testing.T, svc *Service) (Credential, Credential) {
				t.Helper()
				other := "other-fleet@example-project.iam.gserviceaccount.com"
				first := machineCredentialFor(t, svc,
					exchange.MachineSubject(exchange.GoogleIdentity{Account: machineAccountEmail}, "tp-first"), "tp-first")
				second := machineCredentialFor(t, svc,
					exchange.MachineSubject(exchange.GoogleIdentity{Account: other}, "tp-second"), "tp-second")
				return first, second
			},
		},
		{
			name: "a second person on a box of their own",
			boxes: func(t *testing.T, svc *Service) (Credential, Credential) {
				t.Helper()
				return mintCredential(t, svc, str("ada@example.com"), nil, "ada-devbox"),
					mintCredential(t, svc, str("grace@example.com"), nil, "grace-devbox")
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := newService(t)
			session := "one-name-two-principals"
			first, second := c.boxes(t, svc)
			owner, err := svc.Create(ctx, first, Enrollment{Kind: "session", RuntimeID: "box-1", Thumbprint: "tp-box-1", SessionID: str(session)})
			if err != nil {
				t.Fatalf("the first principal's enrollment: %v", err)
			}
			held := carryPrincipalOf(t, svc, owner.ID)
			if got := sessionNameHolder(t, svc, session); got != held {
				t.Fatalf("the name %s is held by %q, want the first principal %q: an enrolment binds the name it enrols", session, got, held)
			}
			grant := insertLiveGrant(t, svc, owner.ID)
			if err := svc.Revoke(ctx, first, owner.ID.String(), "launcher"); err != nil {
				t.Fatalf("Revoke: %v", err)
			}

			_, err = svc.Create(ctx, second, Enrollment{Kind: "session", RuntimeID: "box-2", Thumbprint: "tp-box-2", SessionID: str(session)})
			if !errors.Is(err, ErrSessionHeld) {
				t.Fatalf("the second principal's enrollment = %v, want ErrSessionHeld: a session name belongs to one principal", err)
			}
			if !strings.Contains(err.Error(), session) {
				t.Fatalf("the refusal reads %q, want it to name the session %s the caller reached for", err, session)
			}
			// The refusal gives the refused caller nothing of whoever holds the name: neither the
			// principal the broker recorded nor the login or account inside it, which is the half
			// a reader would recognise. What the refusal says beyond the session is nobody's to
			// depend on, so none of its wording is pinned here.
			_, identity, split := strings.Cut(held, ":")
			if !split || identity == "" {
				t.Fatalf("the first principal reads %q, want <kind>:<identity>: both halves are measured against the refusal", held)
			}
			for _, leaked := range []string{held, identity} {
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("the refusal reads %q, want nothing of who holds the name in it (%q)", err, leaked)
				}
			}
			if rows := enrollmentsNaming(t, svc, session); rows != 1 {
				t.Fatalf("%d enrollments name %s, want the first principal's alone: a refused enrolment writes nothing", rows, session)
			}
			if got := sessionNameHolder(t, svc, session); got != held {
				t.Fatalf("the name %s is held by %q after the refusal, want the first principal %q still", session, got, held)
			}
			if holder := grantHolder(t, svc, grant); holder != owner.ID {
				t.Fatalf("grant %s moved to enrollment %s, want it left on %s: a grant follows one principal's name and no other's", grant, holder, owner.ID)
			}
			if left := readCarryState(t, svc, owner.ID); !left.Dormant || left.LiveGrants != 1 {
				t.Fatalf("the first principal's enrollment is %+v, want it still dormant holding its grant: the refused box took nothing, so the carry sweep is what ends it", left)
			}
		})
	}
}

// TestTheSamePrincipalsSecondBoxTakesTheNameAndWhatItHeld is the boundary this design accepts:
// within one principal the name is asserted and not proved, so the same person's second box
// enrolling the same name is refused nothing and adopts what the box it replaced held. That is
// the carry itself, and binding the name must not stand in its way.
func TestTheSamePrincipalsSecondBoxTakesTheNameAndWhatItHeld(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	session := "one-principal-two-boxes"
	cred, old := carryEnrollment(t, svc, issuer, key, "session", session)
	grant := insertLiveGrant(t, svc, old.ID)
	if err := svc.Revoke(ctx, cred, old.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	_, moved := movedTo(t, svc, issuer, key, "session", session, session+"-2")
	if holder := grantHolder(t, svc, grant); holder != moved.ID {
		t.Fatalf("grant %s hangs off %s, want the second box's enrollment %s: one principal's second box takes the name and what it held", grant, holder, moved.ID)
	}
	if got, want := sessionNameHolder(t, svc, session), carryPrincipalOf(t, svc, moved.ID); got != want {
		t.Fatalf("the name %s is held by %q, want %q: both boxes are the one principal's", session, got, want)
	}
}

// TestAnEnrollmentNamingNoSessionBindsNothing: the pair a grant follows is a name and a
// principal, so an enrolment that states no name has no name to bind. Nothing is written to the
// table of holders and nothing is refused, however many such enrolments one credential makes.
func TestAnEnrollmentNamingNoSessionBindsNothing(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	var bound int
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from oauth_session_bindings`).Scan(&bound); err != nil {
		t.Fatalf("count the session names held: %v", err)
	}
	for i, name := range []*string{nil, str("")} {
		runtime := "no-name-" + sessionIDName(name)
		if _, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: runtime, Thumbprint: "tp-" + runtime, SessionID: name}); err != nil {
			t.Fatalf("Create(session id %s): %v, want it refused nothing: an enrolment that names no session binds no name", sessionIDName(name), err)
		}
		var now int
		if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from oauth_session_bindings`).Scan(&now); err != nil {
			t.Fatalf("count the session names held: %v", err)
		}
		if now != bound {
			t.Fatalf("after enrolment %d, which named no session, %d session names are held, want the %d held before it", i+1, now, bound)
		}
	}
}

// TestAPodsEnrollmentAndTheExchangeHoldItsNameUnderTheSameAccount is the one thing this refusal
// may not break. A Legion role's pod trades a token under its session name before or after it
// enrols under that same name: the exchange binds the name to exchange.AccountPrincipal of the
// ServiceAccount subject the pod's projected token proved (exchange.Set.Pod), and the enrolment
// binds it to exchange.AccountPrincipal of the subject the same kind of token proved
// (carryPrincipal). Those are one string, and this measures it both ways — by comparing the
// principal the exchange decides with the one the enrolment recorded, and by letting each of the
// two claim a name the other has already claimed, which the table itself would refuse if they
// disagreed.
func TestAPodsEnrollmentAndTheExchangeHoldItsNameUnderTheSameAccount(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	const subject = "system:serviceaccount:legion:worker"
	// The exchange as the cluster's deployment lists this account: the entry fixes no session
	// name, so the name the pod asks under is the name it writes, which is the name it enrols.
	set, err := exchange.NewSet([]exchange.AccountEntry{{Account: subject, Project: "LEGION", Person: "ada@example.com"}}, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	podToken := func(uid string) map[string]any {
		return map[string]any{"sub": subject, "kubernetes.io": map[string]any{"pod": map[string]any{"uid": uid}}}
	}
	cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
	binder := oauthstore.New(svc.Store.Pool)
	hour := time.Now().Add(time.Hour)

	t.Run("the enrolment records the principal the exchange decides", func(t *testing.T) {
		const session, uid = "implementer-g1", "pod-uid-enrolled-first"
		enr, err := svc.Create(ctx, cred, Enrollment{Kind: "pod", RuntimeID: uid, Thumbprint: "tp-" + uid, SessionID: str(session),
			PodToken: mintPodToken(t, issuer, key, subject, uid)})
		if err != nil {
			t.Fatalf("the pod's enrolment: %v", err)
		}
		claims, err := set.Pod(podToken(uid), session, nil)
		if err != nil {
			t.Fatalf("the exchange's reading of the same pod's token: %v", err)
		}
		recorded := carryPrincipalOf(t, svc, enr.ID)
		if claims.Principal != recorded {
			t.Fatalf("the exchange calls this pod %q and its enrolment calls it %q, want one spelling: a pod that has exchanged under its name must not be refused that name by its own enrolment",
				claims.Principal, recorded)
		}
		if got := sessionNameHolder(t, svc, session); got != claims.Principal {
			t.Fatalf("the name %s is held by %q, want the exchange's own %q", session, got, claims.Principal)
		}
		// And the exchange's own claim of the name the enrolment has bound is no refusal.
		if _, err := binder.BindSession(ctx, session, claims.Principal, false, hour); err != nil {
			t.Fatalf("the pod's trade under the name it enrolled = %v, want it claimed: the pod exchanges under its own name after enrolling", err)
		}
	})

	t.Run("the pod enrols the name it has already traded under", func(t *testing.T) {
		const session, uid = "implementer-g2", "pod-uid-traded-first"
		claims, err := set.Pod(podToken(uid), session, nil)
		if err != nil {
			t.Fatalf("the exchange's reading of the pod's token: %v", err)
		}
		if _, err := binder.BindSession(ctx, session, claims.Principal, false, hour); err != nil {
			t.Fatalf("the pod's first trade: %v", err)
		}
		enr, err := svc.Create(ctx, cred, Enrollment{Kind: "pod", RuntimeID: uid, Thumbprint: "tp-" + uid, SessionID: str(session),
			PodToken: mintPodToken(t, issuer, key, subject, uid)})
		if err != nil {
			t.Fatalf("the pod's enrolment after its trade = %v, want it enrolled: the name is already its own", err)
		}
		if got := carryPrincipalOf(t, svc, enr.ID); got != claims.Principal {
			t.Fatalf("the enrolment records %q, want the principal its trade bound the name to, %q", got, claims.Principal)
		}
	})
}

// TestARevokedSignInTradesNothingSoItsBoxReachesNoEnrollment: a box whose key the person revoked
// never reaches an enrolment to be refused one. The revoke ends both halves at once
// (RevokePersonSignIn): the sign-in row, so the box's next trade is store.ErrSignInRevoked and it
// is minted no credential to enrol with, and the launcher credential that sign-in's trades had
// already minted, so the credential it still holds authenticates nothing either
// (ErrUnauthenticated). Nothing about a session name is involved in either.
func TestARevokedSignInTradesNothingSoItsBoxReachesNoEnrollment(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "signin-box")
	signIn := insertSignIn(t, svc, cred, "ada@example.com")
	var thumbprint string
	if err := svc.Store.Pool.QueryRow(ctx, `select key_thumbprint from launcher_credentials where id=$1`, cred.ID).Scan(&thumbprint); err != nil {
		t.Fatalf("read the box key's thumbprint: %v", err)
	}
	if _, err := svc.RevokePersonSignIn(ctx, signIn, "ada@example.com", "human:ada@example.com"); err != nil {
		t.Fatalf("RevokePersonSignIn: %v", err)
	}

	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := store.RecordPersonSignIn(ctx, tx, "ada@example.com", thumbprint, "fleet@example.iam.gserviceaccount.com"); !errors.Is(err, store.ErrSignInRevoked) {
		t.Fatalf("the revoked box key's next trade = %v, want %v: the box trades nothing, so it is minted no credential to enrol with", err, store.ErrSignInRevoked)
	}
	if _, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "revoked-box", Thumbprint: "tp-revoked-box", SessionID: str("revoked-box-session")}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an enrolment under the revoked sign-in's credential = %v, want ErrUnauthenticated", err)
	}
	if got := sessionNameHolder(t, svc, "revoked-box-session"); got != "" {
		t.Fatalf("the name revoked-box-session is held by %q, want nobody: the refused enrolment bound nothing", got)
	}
}

// TestAPairedAccountsPodTakesTheNameAndWhatItCarries is the move BROKER_SESSION_ACCOUNT_PAIRS
// exists for: an operator moves a deployment's pods from one ServiceAccount to another, and a
// role's next pod, running as the second account, enrols under the session name the role's
// replaced pod held as the first. Unpaired, that is a name another principal holds, refused as it
// always was and taking nothing. Paired, the new pod takes the name and everything the replaced
// pod's enrollment of it held — its grant, its pending request and its withheld name — as the
// same account's next pod would (TestTheSamePrincipalsSecondBoxTakesTheNameAndWhatItHeld), with
// the session rule's trail, and the broker logs the take once it has committed, naming the name,
// the account it took the name from and the account that took it.
func TestAPairedAccountsPodTakesTheNameAndWhatItCarries(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	const session = "implementer-g1"
	pod := func(account, uid string) Enrollment {
		t.Helper()
		return Enrollment{Kind: "pod", RuntimeID: uid, Thumbprint: "tp-" + uid, SessionID: str(session),
			PodToken: mintPodToken(t, issuer, key, account, uid)}
	}
	oldPrincipal, movedPrincipal := exchange.AccountPrincipal(oldPodAccount), exchange.AccountPrincipal(movedPodAccount)
	cluster, old, grant, request := replacedPod(t, svc, pod)

	_, err := svc.Create(ctx, cluster, pod(movedPodAccount, "pod-uid-new"))
	if !errors.Is(err, ErrSessionHeld) {
		t.Fatalf("the moved account's pod enrolment with no pair = %v, want ErrSessionHeld: an unpaired account is refused a name another holds, as before", err)
	}
	if rows := enrollmentsNaming(t, svc, session); rows != 1 {
		t.Fatalf("%d enrollments name %s after the refusal, want the replaced pod's alone", rows, session)
	}
	if got := sessionNameHolder(t, svc, session); got != oldPrincipal {
		t.Fatalf("the name %s is held by %q after the refusal, want the old account %q still", session, got, oldPrincipal)
	}
	if holder := grantHolder(t, svc, grant); holder != old.ID {
		t.Fatalf("grant %s hangs off %s after the refusal, want the replaced pod's %s", grant, holder, old.ID)
	}
	if holder := requestHolder(t, svc, request); holder != old.ID {
		t.Fatalf("request %s hangs off %s after the refusal, want the replaced pod's %s", request, holder, old.ID)
	}
	if got := withheldNames(t, svc, old.ID); !slices.Equal(got, []string{withheldFromReplacedPod}) {
		t.Fatalf("the replaced pod withholds %v after the refusal, want [%s]", got, withheldFromReplacedPod)
	}
	if takes := nameTakes(capture); len(takes) != 0 {
		t.Fatalf("the refused enrolment logged takes %+v, want none: it took nothing", takes)
	}

	pairAccounts(t, svc, oldPodAccount, movedPodAccount)
	moved, err := svc.Create(ctx, cluster, pod(movedPodAccount, "pod-uid-new"))
	if err != nil {
		t.Fatalf("the moved account's pod enrolment with the pair = %v, want it enrolled: a paired account takes the name its pair holds", err)
	}
	if got := sessionNameHolder(t, svc, session); got != movedPrincipal {
		t.Fatalf("the name %s is held by %q, want the moved account %q", session, got, movedPrincipal)
	}
	tookAllOfIt(t, svc, old, moved, grant, request)
	want := []nameTake{{Level: slog.LevelWarn, Session: session, From: oldPrincipal, To: movedPrincipal, Enrollment: moved.ID.String()}}
	if got := nameTakes(capture); !slices.Equal(got, want) {
		t.Fatalf("the take logged %+v, want exactly %+v: one line naming the name, the account it was taken from and the one that took it", got, want)
	}
}

// TestAPairedAccountsEnrollmentAfterItsExchangeTookTheNameTakesWhatItCarries: a Legion pod
// trades a token under its session name before or after it enrols
// (TestAPodsEnrollmentAndTheExchangeHoldItsNameUnderTheSameAccount), so the moved account's pod
// may take the name through the exchange's claim, the same oauthstore.Store.BindSession call,
// before its enrolment makes one. That enrolment then claims a name its own account already
// holds: it takes the name from nobody, so it logs no take, and it still takes what the old
// account's enrollment of the name holds, because adopt's session rule matches the paired account
// at every enrolment and not only at the one whose bind took the name.
func TestAPairedAccountsEnrollmentAfterItsExchangeTookTheNameTakesWhatItCarries(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	const session = "implementer-g1"
	pod := func(account, uid string) Enrollment {
		t.Helper()
		return Enrollment{Kind: "pod", RuntimeID: uid, Thumbprint: "tp-" + uid, SessionID: str(session),
			PodToken: mintPodToken(t, issuer, key, account, uid)}
	}
	oldPrincipal, movedPrincipal := exchange.AccountPrincipal(oldPodAccount), exchange.AccountPrincipal(movedPodAccount)
	cluster, old, grant, request := replacedPod(t, svc, pod)
	pairAccounts(t, svc, oldPodAccount, movedPodAccount)

	taken, err := svc.Sessions.BindSession(ctx, session, movedPrincipal, false, time.Now().Add(time.Hour))
	if err != nil || taken != oldPrincipal {
		t.Fatalf("the moved account's trade under %s = %q, %v; want it to take the name from %q", session, taken, err, oldPrincipal)
	}
	moved, err := svc.Create(ctx, cluster, pod(movedPodAccount, "pod-uid-new"))
	if err != nil {
		t.Fatalf("the moved account's pod enrolment after its trade = %v, want it enrolled: the name is already its account's", err)
	}
	if got := sessionNameHolder(t, svc, session); got != movedPrincipal {
		t.Fatalf("the name %s is held by %q, want the moved account %q", session, got, movedPrincipal)
	}
	tookAllOfIt(t, svc, old, moved, grant, request)
	if takes := nameTakes(capture); len(takes) != 0 {
		t.Fatalf("the enrolment logged takes %+v, want none: its account held the name already, so it took the name from nobody", takes)
	}
}
