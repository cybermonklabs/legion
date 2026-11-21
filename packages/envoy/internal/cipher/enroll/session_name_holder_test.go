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

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
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

// pairAccounts pairs the ServiceAccount subjects a and b for session names, as cmd/cipherd reads
// one pair of BROKER_SESSION_ACCOUNT_PAIRS into the store an enrollment binds its name through
// (oauthstore.Store.SetSessionPairs, which writes the pair both ways).
func pairAccounts(t *testing.T, svc *Service, a, b string) {
	t.Helper()
	binder, ok := svc.Sessions.(*oauthstore.Store)
	if !ok {
		t.Fatalf("the service binds session names through %T, want the *oauthstore.Store cmd/cipherd wires", svc.Sessions)
	}
	binder.SetSessionPairs([][2]string{{exchange.AccountPrincipal(a), exchange.AccountPrincipal(b)}})
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

// replacedSession is the session name the replaced pod enrolled and the role's next pod states
// (replacedPod).
const replacedSession = "implementer-g1"

// pairedMove is what the paired-move tests start from (replacedPod): a Service with a pod
// verifier, whose log capture holds; the launcher credential cluster, which enrolled the replaced
// pod and enrols the role's next one; the replaced pod's enrollment old, its grant and its pending
// request; and the principals of the account the replaced pod ran as and of the one the next pod
// runs as (exchange.AccountPrincipal).
type pairedMove struct {
	svc                          *Service
	issuer                       *oidctest.Issuer
	key                          *oidctest.Key
	capture                      *logtest.Recorder
	cluster                      Credential
	old                          Enrollment
	grant                        uuid.UUID
	request                      string
	oldPrincipal, movedPrincipal string
}

// replacedPod is a role's pod running as workerPodAccount that enrolled replacedSession, holds a
// live grant, waits on a request nobody has answered yet and has had withheldFromReplacedPod taken
// back from it, and was then replaced: its launcher revoked its enrollment, as the daemon does
// when it lets the role's process go, which leaves the row dormant holding all three for the
// session's next enrollment. Its launcher is the daemon's token login (daemonLogin), which
// launches the pods of both accounts of the pair, as one daemon does across a move of its pods'
// account: a pod's grants follow the pair of its account and that login (carryPrincipal). The
// service it answers records every log line from before that enrolment on, and pairs no accounts:
// a test pairs workerPodAccount with pairedWorkerPodAccount (pairAccounts) where what it proves
// needs the pair.
func replacedPod(t *testing.T) pairedMove {
	t.Helper()
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	cluster := daemonLogin(t, svc)
	svc.ServicesByAccount[workerPodAccount] = "legion-daemon"
	svc.ServicesByAccount[pairedWorkerPodAccount] = "legion-daemon"
	m := pairedMove{svc: svc, issuer: issuer, key: key, capture: capture, cluster: cluster,
		oldPrincipal: exchange.AccountPrincipal(workerPodAccount), movedPrincipal: exchange.AccountPrincipal(pairedWorkerPodAccount)}
	old, err := svc.Create(ctx, m.cluster, m.pod(t, workerPodAccount, "pod-uid-old"))
	if err != nil {
		t.Fatalf("the old account's pod enrolment: %v", err)
	}
	m.old = old
	m.grant = insertLiveGrant(t, svc, old.ID)
	m.request = insertPendingRequest(t, svc, old.ID)
	withholdSecret(t, svc, old.ID, withheldFromReplacedPod)
	if err := svc.Revoke(ctx, m.cluster, old.ID.String(), "launcher"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if left := readCarryState(t, svc, old.ID); !left.Dormant || left.LiveGrants != 1 || left.Pending != 1 {
		t.Fatalf("the replaced pod's enrollment is %+v, want it dormant holding its grant and its pending request: this test measures what its successor takes", left)
	}
	return m
}

// pod is the enrollment the pod uid, running as account, presents under replacedSession.
func (m pairedMove) pod(t *testing.T, account, uid string) Enrollment {
	t.Helper()
	return Enrollment{Kind: "pod", RuntimeID: uid, Thumbprint: "tp-" + uid, SessionID: str(replacedSession),
		PodToken: mintPodToken(t, m.issuer, m.key, account, uid)}
}

// next is the enrollment the role's next pod, running as pairedWorkerPodAccount, presents.
func (m pairedMove) next(t *testing.T) Enrollment {
	t.Helper()
	return m.pod(t, pairedWorkerPodAccount, "pod-uid-new")
}

// otherDaemon is another daemon's token login: an account of its own logged in to the daemon
// service the replaced pod's daemon logs in to (daemonLogin), which BROKER_SERVICES binds both
// accounts of the pair to, so it launches the pods of either account under a pair of its own
// (carryPrincipal), as the daemon of another project whose pods run as the same accounts does.
func (m pairedMove) otherDaemon(t *testing.T) Credential {
	t.Helper()
	const account = "system:serviceaccount:other-project:legion-daemon"
	m.svc.ServiceLogins[account] = ServiceLogin{Service: "legion-daemon", Issuers: []string{""}}
	return podRig{svc: m.svc}.login(t, account, "")
}

// tookAllOfIt fails t unless enrollment moved holds everything the replaced pod held — its grant,
// its pending request, still pending, and its withheld name — and the replaced pod holds none of
// it and is no longer dormant, with the trail the session rule writes: one enrollment.adopted row
// from the replaced pod by rule session and one grant.moved row naming the grant and the replaced
// pod.
func (m pairedMove) tookAllOfIt(t *testing.T, moved Enrollment) {
	t.Helper()
	if holder := grantHolder(t, m.svc, m.grant); holder != moved.ID {
		t.Fatalf("grant %s hangs off %s, want the moved account's pod %s: it takes the grant the replaced pod held", m.grant, holder, moved.ID)
	}
	if holder := requestHolder(t, m.svc, m.request); holder != moved.ID {
		t.Fatalf("request %s hangs off %s, want the moved account's pod %s", m.request, holder, moved.ID)
	}
	if state, decidedBy, cancellations, _ := requestState(t, m.svc, m.request); state != "pending" || decidedBy != "" || cancellations != 0 {
		t.Fatalf("request %s is %s, decided by %q, with %d cancellations; want it still pending: the move answers nothing", m.request, state, decidedBy, cancellations)
	}
	if got, want := withheldNames(t, m.svc, moved.ID), []string{withheldFromReplacedPod}; !slices.Equal(got, want) {
		t.Fatalf("the moved account's pod withholds %v, want %v: a new account does not give back what an operator took back from the session", got, want)
	}
	if got := withheldNames(t, m.svc, m.old.ID); len(got) != 0 {
		t.Fatalf("the replaced pod still withholds %v, want nothing: every withheld row moves to the enrollment the session runs on now", got)
	}
	if left := readCarryState(t, m.svc, m.old.ID); !left.Ended || left.Dormant || left.LiveGrants != 0 || left.Pending != 0 {
		t.Fatalf("the replaced pod is %+v, want it ended, no longer dormant and holding nothing, so the carry sweep has nothing of it to revoke", left)
	}
	// The grant's request moves beside the pending one: insertLiveGrant writes the granted
	// request its grant was issued for, and the session rule moves every request off the row.
	want := []adoption{{From: m.old.ID, Rule: "session", Grants: 1, Requests: 2, Withheld: 1}}
	if got := adoptionsOf(t, m.svc, moved.ID); !slices.Equal(got, want) {
		t.Fatalf("the moved account's pod has enrollment.adopted rows %+v, want %+v: the paired take is the session rule's, with its trail", got, want)
	}
	if rows := auditRows(t, m.svc, moved.ID, "grant.moved"); rows != 1 {
		t.Fatalf("the moved account's pod has %d grant.moved rows, want one for the one grant", rows)
	}
	var from string
	if err := m.svc.Store.Pool.QueryRow(context.Background(), `select detail->>'from' from audit where kind='grant.moved' and grant_id=$1 and enrollment_id=$2`,
		m.grant, moved.ID).Scan(&from); err != nil || from != m.old.ID.String() {
		t.Fatalf("grant %s is recorded as moved from %q (%v), want the replaced pod %s", m.grant, from, err, m.old.ID)
	}
}

// tookNoneOfIt fails t unless enrollment moved, the paired account's pod another daemon's login
// enrolled (otherDaemon), took the session name and nothing the replaced pod held: the name is
// held by the moved account; the grant and the pending request still hang off the replaced pod,
// which still withholds withheldFromReplacedPod and is still dormant holding both for the next pod
// its own daemon enrols; and moved withholds nothing and has no enrollment.adopted row.
func (m pairedMove) tookNoneOfIt(t *testing.T, moved Enrollment) {
	t.Helper()
	if got := sessionNameHolder(t, m.svc, replacedSession); got != m.movedPrincipal {
		t.Fatalf("the name %s is held by %q, want the moved account %q: the pair lets any pod of the moved account take the name, which is held under the account alone", replacedSession, got, m.movedPrincipal)
	}
	if holder := grantHolder(t, m.svc, m.grant); holder != m.old.ID {
		t.Fatalf("grant %s hangs off %s, want the replaced pod's %s: another daemon's pod takes none of its grants", m.grant, holder, m.old.ID)
	}
	if holder := requestHolder(t, m.svc, m.request); holder != m.old.ID {
		t.Fatalf("request %s hangs off %s, want the replaced pod's %s: another daemon's pod takes none of its requests", m.request, holder, m.old.ID)
	}
	if got, want := withheldNames(t, m.svc, m.old.ID), []string{withheldFromReplacedPod}; !slices.Equal(got, want) {
		t.Fatalf("the replaced pod withholds %v, want %v still: another daemon's pod takes none of its withheld names", got, want)
	}
	if got := withheldNames(t, m.svc, moved.ID); len(got) != 0 {
		t.Fatalf("another daemon's pod withholds %v, want nothing: it took nothing of the session", got)
	}
	if got := adoptionsOf(t, m.svc, moved.ID); len(got) != 0 {
		t.Fatalf("another daemon's pod has enrollment.adopted rows %+v, want none: it adopted from no predecessor", got)
	}
	if left := readCarryState(t, m.svc, m.old.ID); !left.Dormant || left.LiveGrants != 1 || left.Pending != 1 {
		t.Fatalf("the replaced pod is %+v, want it dormant holding its grant and its pending request for its own daemon's next pod", left)
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
// (namePrincipal), even though the enrolment's grants follow the pair of that account and the
// daemon's login (carryPrincipal). Those are one string, and this measures it both ways — by
// comparing the principal the exchange decides with the one the enrolment bound the name to, and
// by letting each of the two claim a name the other has already claimed, which the table itself
// would refuse if they disagreed.
func TestAPodsEnrollmentAndTheExchangeHoldItsNameUnderTheSameAccount(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	const subject = "system:serviceaccount:legion:worker"
	// The exchange as the cluster's deployment lists this account: the entry fixes no session
	// name, so the name the pod asks under is the name it writes, which is the name it enrols.
	// It names the cluster's one issuer, which, written as a bare URL, has the empty name.
	set, err := exchange.NewSet([]exchange.AccountEntry{{Account: subject, Project: "LEGION", Person: "ada@example.com", Issuers: []string{""}}}, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	podToken := func(uid string) map[string]any {
		return map[string]any{"sub": subject, "kubernetes.io": map[string]any{"pod": map[string]any{"uid": uid}}}
	}
	cred := daemonLogin(t, svc)
	binder := oauthstore.New(svc.Store.Pool)
	hour := time.Now().Add(time.Hour)

	t.Run("the enrolment records the principal the exchange decides", func(t *testing.T) {
		const session, uid = "implementer-g1", "pod-uid-enrolled-first"
		enr, err := svc.Create(ctx, cred, Enrollment{Kind: "pod", RuntimeID: uid, Thumbprint: "tp-" + uid, SessionID: str(session),
			PodToken: mintPodToken(t, issuer, key, subject, uid)})
		if err != nil {
			t.Fatalf("the pod's enrolment: %v", err)
		}
		claims, err := set.Pod(podToken(uid), "", session, nil)
		if err != nil {
			t.Fatalf("the exchange's reading of the same pod's token: %v", err)
		}
		if recorded, want := carryPrincipalOf(t, svc, enr.ID), exchange.PodLaunchPrincipal(subject, exchange.AccountPrincipal(carryDaemonAccount)); recorded != want {
			t.Fatalf("the enrolment's grants follow %q, want the pair of the pod's account and its daemon's login, %q", recorded, want)
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
		claims, err := set.Pod(podToken(uid), "", session, nil)
		if err != nil {
			t.Fatalf("the exchange's reading of the pod's token: %v", err)
		}
		if _, err := binder.BindSession(ctx, session, claims.Principal, false, hour); err != nil {
			t.Fatalf("the pod's first trade: %v", err)
		}
		_, err = svc.Create(ctx, cred, Enrollment{Kind: "pod", RuntimeID: uid, Thumbprint: "tp-" + uid, SessionID: str(session),
			PodToken: mintPodToken(t, issuer, key, subject, uid)})
		if err != nil {
			t.Fatalf("the pod's enrolment after its trade = %v, want it enrolled: the name is already its own", err)
		}
		if got := sessionNameHolder(t, svc, session); got != claims.Principal {
			t.Fatalf("the name %s is held by %q after the enrolment, want the principal its trade bound the name to, %q", session, got, claims.Principal)
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
	m := replacedPod(t)

	_, err := m.svc.Create(ctx, m.cluster, m.next(t))
	if !errors.Is(err, ErrSessionHeld) {
		t.Fatalf("the moved account's pod enrolment with no pair = %v, want ErrSessionHeld: an unpaired account is refused a name another holds, as before", err)
	}
	if rows := enrollmentsNaming(t, m.svc, replacedSession); rows != 1 {
		t.Fatalf("%d enrollments name %s after the refusal, want the replaced pod's alone", rows, replacedSession)
	}
	if got := sessionNameHolder(t, m.svc, replacedSession); got != m.oldPrincipal {
		t.Fatalf("the name %s is held by %q after the refusal, want the old account %q still", replacedSession, got, m.oldPrincipal)
	}
	if holder := grantHolder(t, m.svc, m.grant); holder != m.old.ID {
		t.Fatalf("grant %s hangs off %s after the refusal, want the replaced pod's %s", m.grant, holder, m.old.ID)
	}
	if holder := requestHolder(t, m.svc, m.request); holder != m.old.ID {
		t.Fatalf("request %s hangs off %s after the refusal, want the replaced pod's %s", m.request, holder, m.old.ID)
	}
	if got := withheldNames(t, m.svc, m.old.ID); !slices.Equal(got, []string{withheldFromReplacedPod}) {
		t.Fatalf("the replaced pod withholds %v after the refusal, want [%s]", got, withheldFromReplacedPod)
	}
	if takes := nameTakes(m.capture); len(takes) != 0 {
		t.Fatalf("the refused enrolment logged takes %+v, want none: it took nothing", takes)
	}

	pairAccounts(t, m.svc, workerPodAccount, pairedWorkerPodAccount)
	moved, err := m.svc.Create(ctx, m.cluster, m.next(t))
	if err != nil {
		t.Fatalf("the moved account's pod enrolment with the pair = %v, want it enrolled: a paired account takes the name its pair holds", err)
	}
	if got := sessionNameHolder(t, m.svc, replacedSession); got != m.movedPrincipal {
		t.Fatalf("the name %s is held by %q, want the moved account %q", replacedSession, got, m.movedPrincipal)
	}
	m.tookAllOfIt(t, moved)
	want := []nameTake{{Level: slog.LevelWarn, Session: replacedSession, From: m.oldPrincipal, To: m.movedPrincipal, Enrollment: moved.ID.String()}}
	if got := nameTakes(m.capture); !slices.Equal(got, want) {
		t.Fatalf("the take logged %+v, want exactly %+v: one line naming the name, the account it was taken from and the one that took it", got, want)
	}
}

// TestAPairedAccountsPodAnotherDaemonLaunchedTakesTheNameAndNothingItCarries holds the pair to the
// rule a pod's grants follow (carryPrincipal): the pair of its account and the login that enrolled
// it. The pods of several daemons run as one account, so a pair that let any pod of the paired
// account take what the session holds would hand one daemon's grants to another daemon's role. A
// pod of the paired account that another daemon's login enrols takes the name, which is held under
// the account alone, as an unpaired pod of another daemon is admitted under a name its own account
// holds, and takes none of the grant, the pending request and the withheld name the replaced pod's
// enrollment holds: they stay on it, dormant, for the next pod its own daemon enrols
// (tookNoneOfIt).
func TestAPairedAccountsPodAnotherDaemonLaunchedTakesTheNameAndNothingItCarries(t *testing.T) {
	ctx := context.Background()
	m := replacedPod(t)
	pairAccounts(t, m.svc, workerPodAccount, pairedWorkerPodAccount)

	moved, err := m.svc.Create(ctx, m.otherDaemon(t), m.next(t))
	if err != nil {
		t.Fatalf("another daemon's pod of the paired account enrolling = %v, want it enrolled: its account takes the name the pair lets it take", err)
	}
	m.tookNoneOfIt(t, moved)
}

// TestAPairedAccountsPodTakesWhatItsPairsLegacyRowHolds: a row an older broker replica wrote while
// the broker rolls carries the pod's account alone (spellAccountAlone), and migration 0037 rewrites
// only the rows it finds when it runs. Such a row of the account the pods move off is still the
// session rule's predecessor for the moved account's pod, where a login of the same account
// enrolled both, as a row of the pair is: the pod takes its grant, its pending request and its
// withheld name with the same trail (tookAllOfIt). Another daemon's login takes the name and none
// of the rest, whichever spelling the row carries, held to every check a row of the pair is held
// to (tookNoneOfIt).
func TestAPairedAccountsPodTakesWhatItsPairsLegacyRowHolds(t *testing.T) {
	ctx := context.Background()
	t.Run("the same daemon's login takes all of it", func(t *testing.T) {
		m := replacedPod(t)
		spellAccountAlone(t, m.svc, m.old.ID)
		pairAccounts(t, m.svc, workerPodAccount, pairedWorkerPodAccount)
		moved, err := m.svc.Create(ctx, m.cluster, m.next(t))
		if err != nil {
			t.Fatalf("the moved account's pod enrolment with the pair = %v, want it enrolled", err)
		}
		m.tookAllOfIt(t, moved)
	})
	t.Run("another daemon's login takes none of it", func(t *testing.T) {
		m := replacedPod(t)
		spellAccountAlone(t, m.svc, m.old.ID)
		pairAccounts(t, m.svc, workerPodAccount, pairedWorkerPodAccount)
		moved, err := m.svc.Create(ctx, m.otherDaemon(t), m.next(t))
		if err != nil {
			t.Fatalf("another daemon's pod of the paired account enrolling = %v, want it enrolled: its account takes the name the pair lets it take", err)
		}
		m.tookNoneOfIt(t, moved)
	})
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
	m := replacedPod(t)
	pairAccounts(t, m.svc, workerPodAccount, pairedWorkerPodAccount)

	taken, err := m.svc.Sessions.BindSession(ctx, replacedSession, m.movedPrincipal, false, time.Now().Add(time.Hour))
	if err != nil || taken != m.oldPrincipal {
		t.Fatalf("the moved account's trade under %s = %q, %v; want it to take the name from %q", replacedSession, taken, err, m.oldPrincipal)
	}
	moved, err := m.svc.Create(ctx, m.cluster, m.next(t))
	if err != nil {
		t.Fatalf("the moved account's pod enrolment after its trade = %v, want it enrolled: the name is already its account's", err)
	}
	if got := sessionNameHolder(t, m.svc, replacedSession); got != m.movedPrincipal {
		t.Fatalf("the name %s is held by %q, want the moved account %q", replacedSession, got, m.movedPrincipal)
	}
	m.tookAllOfIt(t, moved)
	if takes := nameTakes(m.capture); len(takes) != 0 {
		t.Fatalf("the enrolment logged takes %+v, want none: its account held the name already, so it took the name from nobody", takes)
	}
}
