// person_pod_test.go drives a person's pod login (PersonPodLogin): a token login of a Kubernetes
// account that BROKER_POD_OPERATORS maps to a person, from a token bound to a pod, which carries
// that person as its operator. It enrols its own pod alone, as its person, and nothing else. It
// ends only the pod enrollment that it made of its own pod. Its person alone lists it and revokes
// it. A stale one, whose entry the setting dropped or moved, acts on nothing. Each test of the
// login runs on both schemas (personPodSchemas). The tests of the projects a person's pod names
// run on the schema this release ships alone (newPersonPodProjectsRig says why).
package enroll

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

const (
	// adaPerson is the person that BROKER_POD_OPERATORS maps adaPodAccount to. gracePerson is
	// another person.
	adaPerson   = "ada@example.com"
	gracePerson = "grace@example.com"
	// adaPodAccount is the account that ada's pods run as. adaLoginService is the login service
	// that BROKER_SERVICE_LOGINS gives that account, and it is ada's alone.
	adaPodAccount   = "system:serviceaccount:people:ada"
	adaLoginService = "person-ada"
	// adaBatchAccount is a second account that the setting maps to ada, with a login service of
	// its own.
	adaBatchAccount      = "system:serviceaccount:people:ada-batch"
	adaBatchLoginService = "person-ada-batch"
	// adaBoxSession is the session name of ada's session on her dev box.
	adaBoxSession = "ada-devbox-session"
)

// personPodSchema is one of the two schemas that each test of a person's pod login runs on.
type personPodSchema struct {
	name string
	rig  func(t *testing.T) podRig
	// shared is true on the schema this release ships, where every pod of one account shares
	// the account's one live credential row (IssueTokenLoginTx).
	shared bool
}

// shippedPersonPodSchema is the schema this release ships.
var shippedPersonPodSchema = personPodSchema{name: "the schema this release ships", rig: newPodRig, shared: true}

func personPodSchemas() []personPodSchema {
	return []personPodSchema{
		shippedPersonPodSchema,
		{name: "the per-pod login schema", rig: newPerPodRig, shared: false},
	}
}

// newPersonPodRig is a podRig whose settings map adaPodAccount to ada: BROKER_SERVICE_LOGINS
// gives the account ada's own login service, and BROKER_POD_OPERATORS maps it to her.
func newPersonPodRig(t *testing.T, schema personPodSchema) podRig {
	t.Helper()
	r := schema.rig(t)
	r.svc.ServiceLogins[adaPodAccount] = adaLoginService
	r.svc.PodOperators = map[string]string{adaPodAccount: adaPerson}
	return r
}

// personLogin issues the launcher credential of a person's pod login, as
// machine.Service.TokenLogin issues it for a token of a mapped account bound to pod podUID: under
// the account's login service, with person as its operator, and with a fresh key. It reads the
// credential back as Create sees it.
func personLogin(t *testing.T, r podRig, account, podUID, person string) Credential {
	t.Helper()
	ctx := context.Background()
	key := newLoginKey(t)
	login := r.tokenLoginOf(account, podUID, key)
	login.Operator = person
	id, err := r.svc.IssueTokenLogin(ctx, login)
	if err != nil {
		t.Fatalf("IssueTokenLogin(%s, pod %q, operator %s): %v", account, podUID, person, err)
	}
	cred, err := r.svc.CredentialForKey(ctx, id.String(), key.thumbprint)
	if err != nil {
		t.Fatalf("CredentialForKey(%s): %v", id, err)
	}
	return cred
}

// personsSignIn issues the launcher credential of a person's sign-in on a machine, as the token
// exchange issues it: a subject that exchange.PersonSubject writes, the fleet's service, and the
// person as its operator. It reads the credential back as Create sees it.
func personsSignIn(t *testing.T, svc *Service, person string) Credential {
	t.Helper()
	ctx := context.Background()
	key := newLoginKey(t)
	expires := time.Now().Add(time.Hour)
	id, err := svc.IssueTokenLogin(ctx, TokenLogin{
		Subject: exchange.PersonSubject(person, machineAccountEmail, key.thumbprint), Service: machineFleet,
		Operator: person, Host: "example-project/example-zone-a/example-machine-1",
		TokenExpires: expires, Thumbprint: key.thumbprint, JWK: key.jwk, Expires: expires,
	})
	if err != nil {
		t.Fatalf("IssueTokenLogin(%s's sign-in): %v", person, err)
	}
	cred, err := svc.CredentialForKey(ctx, id.String(), key.thumbprint)
	if err != nil {
		t.Fatalf("CredentialForKey(%s): %v", id, err)
	}
	return cred
}

// adaBoxSessionWithGrant enrols ada's session on her dev box under her own machine login, and
// gives it a live grant. It asserts both, because that grant is what an enrollment under the
// session's name would take (adopt).
func adaBoxSessionWithGrant(t *testing.T, r podRig) (Enrollment, uuid.UUID) {
	t.Helper()
	box := mintApprovedCredential(t, r.svc, adaPerson, nil, "ada-devbox")
	enr, err := r.svc.Create(context.Background(), box, Enrollment{
		Kind: "session", RuntimeID: "ada-devbox", Thumbprint: "tp-ada-devbox", SessionID: str(adaBoxSession),
	})
	if err != nil {
		t.Fatalf("Create(ada's box session) = %v", err)
	}
	if textOf(enr.Operator) != adaPerson {
		t.Fatalf("ada's box session has operator %s, want %s", textOf(enr.Operator), adaPerson)
	}
	grant := insertLiveGrant(t, r.svc, enr.ID)
	requireLiveGrant(t, r, "ada's box session before any attempt on it", enr.ID, grant)
	return enr, grant
}

// requireLiveGrant fails t unless grant is live and hangs off holder, and holder is a live
// enrollment. That is the state that makes taking the grant worth a refusal, and the state that a
// refusal leaves.
func requireLiveGrant(t *testing.T, r podRig, when string, holder, grant uuid.UUID) {
	t.Helper()
	var on uuid.UUID
	var live bool
	if err := r.svc.Store.Pool.QueryRow(context.Background(),
		`select enrollment_id, revoked_at is null and (expires_at is null or expires_at > now()) from grants where id=$1`, grant).Scan(&on, &live); err != nil {
		t.Fatalf("read grant %s: %v", grant, err)
	}
	if ended := r.revoked(t, holder.String()); on != holder || !live || ended {
		t.Fatalf("%s: grant %s hangs off %s and is live %v, and enrollment %s ended %v; want a live grant on the live enrollment %s",
			when, grant, on, live, holder, ended, holder)
	}
}

// ownLiveCredentialIDs is OwnLiveCredentials(person) reduced to its ids, in its order.
func ownLiveCredentialIDs(t *testing.T, svc *Service, person string) []uuid.UUID {
	t.Helper()
	creds, err := svc.OwnLiveCredentials(context.Background(), person)
	if err != nil {
		t.Fatalf("OwnLiveCredentials(%s): %v", person, err)
	}
	ids := make([]uuid.UUID, len(creds))
	for i, c := range creds {
		ids[i] = c.ID
	}
	return ids
}

// textOf is *p, or "<none>" for nil, for a comparison and a failure message alike.
func textOf(p *string) string {
	if p == nil {
		return "<none>"
	}
	return *p
}

// TestAPersonPodLoginEnrolsItsOwnPodAsItsPerson: a pod of an account that BROKER_POD_OPERATORS
// maps logs in with its own pod-bound token, and its credential carries the person. That
// credential is a person's pod login, and the setting admits it. It enrols its own pod. The
// enrollment records the person as its operator and the account as its subject, and it names no
// session. It records the person as its owner (ownerKey), as a box of the person does, so the
// pod's requests are the person's to read and cancel.
func TestAPersonPodLoginEnrolsItsOwnPodAsItsPerson(t *testing.T) {
	for _, schema := range personPodSchemas() {
		t.Run(schema.name, func(t *testing.T) {
			r := newPersonPodRig(t, schema)
			cred := personLogin(t, r, adaPodAccount, "pod-uid-ada", adaPerson)
			if !PersonPodLogin(cred) || !r.svc.livePersonPodLogin(cred) || textOf(cred.Operator) != adaPerson || cred.PodUID != "pod-uid-ada" {
				t.Fatalf("ada's pod login = %+v with operator %s; want a live person's pod login of %s bound to pod-uid-ada", cred, textOf(cred.Operator), adaPerson)
			}

			enr, err := r.enroll(t, cred, adaPodAccount, "pod-uid-ada")
			if err != nil {
				t.Fatalf("Create(ada's own pod) = %v; want it enrolled", err)
			}
			if textOf(enr.Operator) != adaPerson || textOf(enr.Subject) != adaPodAccount {
				t.Fatalf("Create(ada's own pod) = operator %s, subject %s; want %s and %s", textOf(enr.Operator), textOf(enr.Subject), adaPerson, adaPodAccount)
			}
			var kind string
			var operator, subject, session, owner *string
			if err := r.svc.Store.Pool.QueryRow(context.Background(), `select kind, operator, subject, session_id, owner from enrollments where id=$1`, enr.ID).
				Scan(&kind, &operator, &subject, &session, &owner); err != nil {
				t.Fatalf("read enrollment %s: %v", enr.ID, err)
			}
			if kind != "pod" || textOf(operator) != adaPerson || textOf(subject) != adaPodAccount || session != nil {
				t.Fatalf("the enrollment row = kind %s, operator %s, subject %s, session %s; want a pod of %s with subject %s and no session",
					kind, textOf(operator), textOf(subject), textOf(session), adaPerson, adaPodAccount)
			}
			if want := exchange.PersonPrincipal(adaPerson); textOf(owner) != want {
				t.Fatalf("the enrollment row records owner %s; want %s, the owner of ada's box, so that the pod's requests are ada's", textOf(owner), want)
			}
		})
	}
}

// TestAPersonPodLoginIsRefusedEveryEnrollmentButItsOwnPod: a person's pod login enrols its own pod
// alone. It is refused another pod of its account, and a pod of another account. It is refused a
// pod of an account that BROKER_SERVICES binds to its own login service, which shows that it never
// reads BROKER_SERVICES. It is refused a session name on its own pod. It is refused an enrollment
// of kind session under the name of its person's box session, which holds a live grant that such
// an enrollment would adopt. No refusal writes an enrollment, and the box keeps its grant. The
// same login then enrols its own pod, so each refusal is the rule's and not the login's.
func TestAPersonPodLoginIsRefusedEveryEnrollmentButItsOwnPod(t *testing.T) {
	const adaWorkerAccount = "system:serviceaccount:people:ada-worker"
	for _, schema := range personPodSchemas() {
		t.Run(schema.name, func(t *testing.T) {
			r := newPersonPodRig(t, schema)
			ctx := context.Background()
			cred := personLogin(t, r, adaPodAccount, "pod-uid-ada", adaPerson)
			r.svc.ServicesByAccount[adaWorkerAccount] = adaLoginService
			if !r.svc.launches(cred.Service, adaWorkerAccount) {
				t.Fatalf("BROKER_SERVICES does not bind %s to %s; the row of that account needs the binding to show that the rule never reads it", adaWorkerAccount, adaLoginService)
			}

			for _, tc := range []struct {
				name, account, podUID, session string
				want                           error
				names                          []string
			}{
				{"another pod of its account", adaPodAccount, "pod-uid-ada-sibling", "", ErrPodSelfEnrollOnly, []string{"pod-uid-ada", "pod-uid-ada-sibling"}},
				{"a pod of another account", strangerPodAccount, "pod-uid-stranger", "", ErrPodSelfEnrollOnly, []string{adaPodAccount, strangerPodAccount}},
				{"a pod of an account BROKER_SERVICES binds to its login service", adaWorkerAccount, "pod-uid-ada-worker", "", ErrPodSelfEnrollOnly, []string{adaPodAccount, adaWorkerAccount}},
				{"its own pod under a session name", adaPodAccount, "pod-uid-ada", "ada-pod-session", ErrPodSelfEnrollNoSession, nil},
			} {
				var enr Enrollment
				var err error
				if tc.session == "" {
					enr, err = r.enroll(t, cred, tc.account, tc.podUID)
				} else {
					enr, err = r.enrollSession(t, cred, tc.account, tc.podUID, tc.session)
				}
				if !errors.Is(err, tc.want) {
					t.Errorf("Create(%s) = enrollment %s, %v; want %v", tc.name, enr.ID, err, tc.want)
					continue
				}
				for _, name := range tc.names {
					if !strings.Contains(err.Error(), name) {
						t.Errorf("Create(%s) = %q; want the refusal to name %s", tc.name, err, name)
					}
				}
				if got := r.rows(t, tc.podUID); got != 0 {
					t.Errorf("enrollments naming %s after Create(%s) = %d, want none", tc.podUID, tc.name, got)
				}
			}

			box, grant := adaBoxSessionWithGrant(t, r)
			enr, err := r.svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "pod-uid-ada", Thumbprint: "tp-ada-pod-as-session", SessionID: str(adaBoxSession)})
			if !errors.Is(err, ErrOperatorMismatch) {
				t.Errorf("Create(kind session under ada's box session %q) = enrollment %s, %v; want ErrOperatorMismatch: it would adopt grant %s off the box session %s",
					adaBoxSession, enr.ID, err, grant, box.ID)
			}
			requireLiveGrant(t, r, "after the session enrollment under ada's box session name", box.ID, grant)
			if got := r.rows(t, "pod-uid-ada"); got != 0 {
				t.Fatalf("enrollments naming pod-uid-ada after the refusals = %d, want none", got)
			}

			if _, err := r.enroll(t, cred, adaPodAccount, "pod-uid-ada"); err != nil {
				t.Fatalf("Create(ada's own pod, no session name) = %v; want it enrolled", err)
			}
		})
	}
}

// TestAPersonPodLoginReadsTheAccountThenThePodThenTheSessionName: a request with more than one
// fault gets the refusal of its first fault, in this order: the pod's account, the pod UID, the
// session name. Another pod of its account under a session name is the pod UID's
// ErrPodSelfEnrollOnly. A pod of another account under a session name is the account's
// ErrPodSelfEnrollOnly.
func TestAPersonPodLoginReadsTheAccountThenThePodThenTheSessionName(t *testing.T) {
	for _, schema := range personPodSchemas() {
		t.Run(schema.name, func(t *testing.T) {
			r := newPersonPodRig(t, schema)
			cred := personLogin(t, r, adaPodAccount, "pod-uid-ada", adaPerson)

			_, err := r.enrollSession(t, cred, adaPodAccount, "pod-uid-ada-sibling", "ada-pod-session")
			if !errors.Is(err, ErrPodSelfEnrollOnly) || !strings.Contains(err.Error(), "pod-uid-ada-sibling") {
				t.Errorf("Create(another pod of its account, under a session name) = %v; want the pod UID's ErrPodSelfEnrollOnly, naming pod-uid-ada-sibling", err)
			}
			_, err = r.enrollSession(t, cred, strangerPodAccount, "pod-uid-stranger", "ada-pod-session")
			if !errors.Is(err, ErrPodSelfEnrollOnly) || !strings.Contains(err.Error(), strangerPodAccount) {
				t.Errorf("Create(a pod of another account, under a session name) = %v; want the account's ErrPodSelfEnrollOnly, naming %s", err, strangerPodAccount)
			}
			for _, podUID := range []string{"pod-uid-ada-sibling", "pod-uid-stranger"} {
				if got := r.rows(t, podUID); got != 0 {
					t.Errorf("enrollments naming %s = %d, want none", podUID, got)
				}
			}
		})
	}
}

// TestAPersonPodRuleLeavesAMachineLoginAndASignInRefusedAPod: the rule of a person's pod login
// admits no other credential with an operator to a pod. A person's own machine login has no
// subject. A person's sign-in on a machine has a subject of its own kind. Neither is a person's
// pod login, and each is refused ada's pod, though the setting maps the pod's account to ada.
// Ada's pod login of that account then enrols the same pod.
func TestAPersonPodRuleLeavesAMachineLoginAndASignInRefusedAPod(t *testing.T) {
	for _, schema := range personPodSchemas() {
		t.Run(schema.name, func(t *testing.T) {
			r := newPersonPodRig(t, schema)
			box := mintApprovedCredential(t, r.svc, adaPerson, nil, "ada-devbox")
			if box.Subject != nil || textOf(box.Operator) != adaPerson {
				t.Fatalf("ada's machine login = subject %s, operator %s; want no subject and operator %s", textOf(box.Subject), textOf(box.Operator), adaPerson)
			}
			signIn := personsSignIn(t, r.svc, adaPerson)
			if signIn.Subject == nil || exchange.ReadSubject(*signIn.Subject).Kind != exchange.SubjectSignIn || textOf(signIn.Operator) != adaPerson {
				t.Fatalf("ada's sign-in = subject %s, operator %s; want a sign-in's subject and operator %s", textOf(signIn.Subject), textOf(signIn.Operator), adaPerson)
			}
			for _, tc := range []struct {
				name string
				cred Credential
			}{{"ada's machine login", box}, {"ada's sign-in on a machine", signIn}} {
				if PersonPodLogin(tc.cred) {
					t.Errorf("PersonPodLogin(%s) = true, want false", tc.name)
				}
				if enr, err := r.enroll(t, tc.cred, adaPodAccount, "pod-uid-ada"); !errors.Is(err, ErrOperatorMismatch) {
					t.Errorf("Create(ada's pod) under %s = enrollment %s, %v; want ErrOperatorMismatch", tc.name, enr.ID, err)
				}
			}
			if got := r.rows(t, "pod-uid-ada"); got != 0 {
				t.Fatalf("enrollments naming pod-uid-ada = %d, want none", got)
			}

			cred := personLogin(t, r, adaPodAccount, "pod-uid-ada", adaPerson)
			if _, err := r.enroll(t, cred, adaPodAccount, "pod-uid-ada"); err != nil {
				t.Fatalf("Create(ada's pod) under ada's pod login = %v; want it enrolled", err)
			}
		})
	}
}

// TestAPersonPodLoginEndsItsOwnPodAlone: a person's pod login ends the pod enrollment that it made
// of its own pod. It ends no other enrollment of its person: not a sibling pod's of its account,
// not a pod of the person's second mapped account, and not the person's box session.
//
// On the schema this release ships, the sibling pod's login takes the account's one credential
// over, and the first pod's next login takes it back with the same id. So the two pods share one
// credential id, and only the pod UID refuses the sibling's enrollment. On the per-pod schema,
// each pod holds a credential of its own, and the enrolling credential refuses it as well.
func TestAPersonPodLoginEndsItsOwnPodAlone(t *testing.T) {
	for _, schema := range personPodSchemas() {
		t.Run(schema.name, func(t *testing.T) {
			r := newPersonPodRig(t, schema)
			ctx := context.Background()
			r.svc.ServiceLogins[adaBatchAccount] = adaBatchLoginService
			r.svc.PodOperators[adaBatchAccount] = adaPerson

			first := personLogin(t, r, adaPodAccount, "pod-uid-ada-one", adaPerson)
			mine, err := r.enroll(t, first, adaPodAccount, "pod-uid-ada-one")
			if err != nil {
				t.Fatalf("Create(the first pod's own enrollment) = %v", err)
			}
			second := personLogin(t, r, adaPodAccount, "pod-uid-ada-two", adaPerson)
			sibling, err := r.enroll(t, second, adaPodAccount, "pod-uid-ada-two")
			if err != nil {
				t.Fatalf("Create(the sibling pod's own enrollment) = %v", err)
			}
			again := personLogin(t, r, adaPodAccount, "pod-uid-ada-one", adaPerson)
			if again.ID != first.ID || again.PodUID != "pod-uid-ada-one" {
				t.Fatalf("the first pod's next login = %+v, want credential %s on pod-uid-ada-one", again, first.ID)
			}
			if schema.shared && second.ID != first.ID {
				t.Fatalf("the sibling pod's credential = %s, want the first pod's %s: this schema holds one live credential per account, which is what makes the sibling case reachable", second.ID, first.ID)
			}
			if !schema.shared && second.ID == first.ID {
				t.Fatalf("the sibling pod took credential %s over, want one credential per pod on this schema", first.ID)
			}

			batch := personLogin(t, r, adaBatchAccount, "pod-uid-ada-batch", adaPerson)
			batchPod, err := r.enroll(t, batch, adaBatchAccount, "pod-uid-ada-batch")
			if err != nil {
				t.Fatalf("Create(the pod of ada's second mapped account) = %v", err)
			}
			if textOf(batchPod.Operator) != textOf(again.Operator) {
				t.Fatalf("the second account's pod has operator %s, want the login's %s, so that the operator test admits the revoke below and the enrolling credential decides it",
					textOf(batchPod.Operator), textOf(again.Operator))
			}
			box, grant := adaBoxSessionWithGrant(t, r)

			by := record.LauncherActor(again.ID.String())
			for _, tc := range []struct {
				name string
				id   uuid.UUID
				want error
			}{
				{"a sibling pod's enrollment", sibling.ID, ErrNotEnrollingCredential},
				{"the pod of ada's second mapped account", batchPod.ID, ErrNotEnrollingCredential},
				{"ada's box session", box.ID, ErrOperatorMismatch},
			} {
				if err := r.svc.Revoke(ctx, again, tc.id.String(), by); !errors.Is(err, tc.want) {
					t.Errorf("Revoke(%s, enrollment %s) = %v; want %v", tc.name, tc.id, err, tc.want)
				}
				if r.revoked(t, tc.id.String()) {
					t.Errorf("Revoke(%s) ended enrollment %s; want it untouched", tc.name, tc.id)
				}
			}
			requireLiveGrant(t, r, "after the revoke of ada's box session", box.ID, grant)

			if err := r.svc.Revoke(ctx, again, mine.ID.String(), by); err != nil {
				t.Fatalf("Revoke(its own pod's enrollment) = %v; want it ended", err)
			}
			if !r.revoked(t, mine.ID.String()) {
				t.Fatalf("enrollment %s is still live, want it ended by its own pod", mine.ID)
			}
		})
	}
}

// TestAStalePersonPodLoginActsOnNothing: BROKER_POD_OPERATORS drops ada's account, or moves it to
// another person, while her pod's login still carries her. The row keeps its operator until the
// pod logs in again. That stale login enrols nothing and ends nothing. It is refused its own pod,
// and an enrollment of kind session under the name of ada's box session, which holds a live
// grant. It is refused a revoke of that box session and of its own pod's enrollment. No rule
// reads it as ada's machine login, which would admit the session enrollment and the grant with
// it.
func TestAStalePersonPodLoginActsOnNothing(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(r podRig)
	}{
		{"the setting drops the account", func(r podRig) { delete(r.svc.PodOperators, adaPodAccount) }},
		{"the setting moves the account to another person", func(r podRig) { r.svc.PodOperators[adaPodAccount] = gracePerson }},
	} {
		for _, schema := range personPodSchemas() {
			t.Run(change.name+", "+schema.name, func(t *testing.T) {
				r := newPersonPodRig(t, schema)
				ctx := context.Background()
				cred := personLogin(t, r, adaPodAccount, "pod-uid-ada", adaPerson)
				own, err := r.enroll(t, cred, adaPodAccount, "pod-uid-ada")
				if err != nil {
					t.Fatalf("Create(its own pod) while the setting maps the account to ada = %v", err)
				}
				box, grant := adaBoxSessionWithGrant(t, r)

				change.apply(r)
				var stored *string
				if err := r.svc.Store.Pool.QueryRow(ctx, `select operator from launcher_credentials where id=$1`, cred.ID).Scan(&stored); err != nil {
					t.Fatalf("read credential %s: %v", cred.ID, err)
				}
				if textOf(stored) != adaPerson || !PersonPodLogin(cred) || r.svc.livePersonPodLogin(cred) {
					t.Fatalf("after %s, the login's row names %s, PersonPodLogin %v, live %v; want ada's stale pod login, which the setting no longer admits",
						change.name, textOf(stored), PersonPodLogin(cred), r.svc.livePersonPodLogin(cred))
				}

				if enr, err := r.enroll(t, cred, adaPodAccount, "pod-uid-ada"); !errors.Is(err, ErrOperatorMismatch) {
					t.Errorf("Create(its own pod) under the stale login = enrollment %s (existing %v), %v; want ErrOperatorMismatch", enr.ID, enr.Existing, err)
				}
				enr, err := r.svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "pod-uid-ada", Thumbprint: "tp-ada-pod-as-session", SessionID: str(adaBoxSession)})
				if !errors.Is(err, ErrOperatorMismatch) {
					t.Errorf("Create(kind session under ada's box session %q) under the stale login = enrollment %s, %v; want ErrOperatorMismatch: it would adopt grant %s off the box session %s",
						adaBoxSession, enr.ID, err, grant, box.ID)
				}
				requireLiveGrant(t, r, "after the stale login's session enrollment", box.ID, grant)

				by := record.LauncherActor(cred.ID.String())
				for _, tc := range []struct {
					name string
					id   uuid.UUID
				}{{"ada's box session", box.ID}, {"its own pod's enrollment", own.ID}} {
					if err := r.svc.Revoke(ctx, cred, tc.id.String(), by); !errors.Is(err, ErrOperatorMismatch) {
						t.Errorf("Revoke(%s, enrollment %s) under the stale login = %v; want ErrOperatorMismatch", tc.name, tc.id, err)
					}
					if r.revoked(t, tc.id.String()) {
						t.Errorf("the stale login's Revoke(%s) ended enrollment %s; want it untouched", tc.name, tc.id)
					}
				}
				requireLiveGrant(t, r, "after the stale login's revoke of ada's box session", box.ID, grant)
			})
		}
	}
}

// TestAPersonPodLoginIsListedAndRevokedAsItsPersonsOwn: a token login of an account that
// BROKER_POD_OPERATORS maps is its person's own machine login. LiveCredentials and
// OwnLiveCredentials list it to that person, and to no other person, though the other person's
// list holds every service's login. A login of the account from a token bound to no pod writes no
// operator. On the schema this release ships, that login moves the account's one row to no pod
// and no operator, and the login stays the person's alone. Another person's revoke is
// ErrNotApprover and ends nothing. The person's RevokeCredential ends the pod's enrollment, and the
// person's RevokeOwnCredential of the pod's next login ends the next enrollment.
func TestAPersonPodLoginIsListedAndRevokedAsItsPersonsOwn(t *testing.T) {
	for _, schema := range personPodSchemas() {
		t.Run(schema.name, func(t *testing.T) {
			r := newPersonPodRig(t, schema)
			ctx := context.Background()
			cred := personLogin(t, r, adaPodAccount, "pod-uid-ada", adaPerson)
			pod, err := r.enroll(t, cred, adaPodAccount, "pod-uid-ada")
			if err != nil {
				t.Fatalf("Create(ada's own pod) = %v", err)
			}
			daemon := r.login(t, launcherAccount, "pod-uid-daemon")

			listed := func(when string, ids ...uuid.UUID) {
				t.Helper()
				adaAll, adaOwn := liveCredentialIDs(t, r.svc, adaPerson), ownLiveCredentialIDs(t, r.svc, adaPerson)
				graceAll, graceOwn := liveCredentialIDs(t, r.svc, gracePerson), ownLiveCredentialIDs(t, r.svc, gracePerson)
				if !slices.Contains(graceAll, daemon.ID) || slices.Contains(adaOwn, daemon.ID) {
					t.Fatalf("%s: LiveCredentials(grace) = %v and OwnLiveCredentials(ada) = %v; want the daemon's login %s in the first and not in the second, as for any service's login",
						when, graceAll, adaOwn, daemon.ID)
				}
				for _, id := range ids {
					if !slices.Contains(adaAll, id) || !slices.Contains(adaOwn, id) {
						t.Errorf("%s: LiveCredentials(ada) = %v and OwnLiveCredentials(ada) = %v; want ada's pod login %s in both", when, adaAll, adaOwn, id)
					}
					if slices.Contains(graceAll, id) || slices.Contains(graceOwn, id) {
						t.Errorf("%s: LiveCredentials(grace) = %v and OwnLiveCredentials(grace) = %v; want ada's pod login %s in neither", when, graceAll, graceOwn, id)
					}
				}
			}
			listed("ada's pod login", cred.ID)

			podless := r.login(t, adaPodAccount, "")
			if podless.Operator != nil || podless.PodUID != "" {
				t.Fatalf("the login from a token bound to no pod = operator %s, pod %q; want no operator and no pod, as machine.Service.TokenLogin issues it", textOf(podless.Operator), podless.PodUID)
			}
			if schema.shared && podless.ID != cred.ID {
				t.Fatalf("the login from a token bound to no pod = credential %s, want the account's one row %s moved: that move is what makes this case reachable", podless.ID, cred.ID)
			}
			if !schema.shared && podless.ID == cred.ID {
				t.Fatalf("the login from a token bound to no pod moved the pod's credential %s, want a row of its own on this schema", cred.ID)
			}
			listed("after a login of the account from a token bound to no pod", cred.ID, podless.ID)

			for _, revoke := range []struct {
				name string
				fn   func(ctx context.Context, id, person, actor string) error
			}{{"RevokeCredential", r.svc.RevokeCredential}, {"RevokeOwnCredential", r.svc.RevokeOwnCredential}} {
				if err := revoke.fn(ctx, cred.ID.String(), gracePerson, record.HumanActor(gracePerson)); !errors.Is(err, ErrNotApprover) {
					t.Errorf("%s(ada's pod login %s) by grace = %v; want ErrNotApprover", revoke.name, cred.ID, err)
				}
			}
			if r.revoked(t, pod.ID.String()) {
				t.Fatalf("grace's refused revokes ended ada's pod enrollment %s", pod.ID)
			}

			if err := r.svc.RevokeCredential(ctx, cred.ID.String(), adaPerson, record.HumanActor(adaPerson)); err != nil {
				t.Fatalf("RevokeCredential(ada's pod login) by ada = %v; want it ended", err)
			}
			if !r.revoked(t, pod.ID.String()) {
				t.Fatalf("enrollment %s is still live, want it ended with ada's pod login", pod.ID)
			}

			next := personLogin(t, r, adaPodAccount, "pod-uid-ada", adaPerson)
			if next.ID == cred.ID {
				t.Fatalf("the pod's next login = credential %s, want a new one: the revoke ended %s", next.ID, cred.ID)
			}
			nextPod, err := r.enroll(t, next, adaPodAccount, "pod-uid-ada")
			if err != nil {
				t.Fatalf("Create(ada's own pod) under the next login = %v", err)
			}
			if err := r.svc.RevokeOwnCredential(ctx, next.ID.String(), adaPerson, record.HumanActor(adaPerson)); err != nil {
				t.Fatalf("RevokeOwnCredential(ada's next pod login) by ada = %v; want it ended", err)
			}
			if !r.revoked(t, nextPod.ID.String()) {
				t.Fatalf("enrollment %s is still live, want it ended with ada's next pod login", nextPod.ID)
			}
		})
	}
}

// newPersonPodProjectsRig is newPersonPodRig on the schema this release ships, whose service holds
// a client of a Dispatch stand-in in which ada is a member of kind kind. PRIV is private, owned by
// grace, and its access list names ada. ELSE is private, owned by grace, and names nobody else.
// PUB is public, which a restricted member does not see, since such a member sees only the
// projects that name them. It answers the stand-in too, so that a test can take it down.
//
// The tests of the projects a person's pod names run on this one schema, and not on both as the
// tests of the login do. Their rule, checkProjects, reads the list the pod names and the person
// Create copies from the credential's operator, and nothing else of the credential. The two
// schemas differ only in whether the pods of one account share one credential row
// (IssueTokenLoginTx), and the login's upsert writes the person as the row's operator on both.
// So a second schema would run the same rule on the same input.
func newPersonPodProjectsRig(t *testing.T, kind string) (podRig, *projectaccesstest.Server) {
	t.Helper()
	r := newPersonPodRig(t, shippedPersonPodSchema)
	dispatch := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PRIV": {Visibility: "private", Owner: gracePerson, Listed: []string{adaPerson}},
		"ELSE": {Visibility: "private", Owner: gracePerson},
		"PUB":  {Visibility: "public", Owner: gracePerson},
	}, map[string]string{adaPerson: kind})
	r.svc.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
	return r, dispatch
}

// enrollNaming is enroll naming projects, as `agent-secrets enroll --pod` sends the list that
// AGENT_SECRETS_PROJECTS or --projects holds.
func (r podRig) enrollNaming(t *testing.T, cred Credential, account, podUID string, projects ...string) (Enrollment, error) {
	t.Helper()
	in := r.podEnrollment(t, cred, account, podUID)
	in.Projects = projects
	return r.svc.Create(context.Background(), cred, in)
}

// TestARestrictedMembersPodNamesItsProjectsAsItsSessionDoes: ada is a restricted member, and her
// pod acts for her. Her pod's login names no project, and it is refused ErrProjectsRequired, the
// refusal her own session gets. The refusal says to set AGENT_SECRETS_PROJECTS, and it is not
// ErrProjectsNeedAPerson. The same login then names PRIV, which her access list gives her, and the
// pod is enrolled with that list, as her session would be. So the refusal's words lead to an
// enrollment, and not to a second refusal.
func TestARestrictedMembersPodNamesItsProjectsAsItsSessionDoes(t *testing.T) {
	r, _ := newPersonPodProjectsRig(t, projectaccess.MemberRestricted)
	cred := personLogin(t, r, adaPodAccount, "pod-uid-ada", adaPerson)

	_, err := r.enrollNaming(t, cred, adaPodAccount, "pod-uid-ada")
	if !errors.Is(err, ErrProjectsRequired) || errors.Is(err, ErrProjectsNeedAPerson) || !strings.Contains(err.Error(), "AGENT_SECRETS_PROJECTS") {
		t.Fatalf("Create(ada's pod, naming no project) = %v; want ErrProjectsRequired saying to set AGENT_SECRETS_PROJECTS", err)
	}
	if got := r.rows(t, "pod-uid-ada"); got != 0 {
		t.Fatalf("enrollments naming pod-uid-ada after the refusal = %d, want none", got)
	}

	enr, err := r.enrollNaming(t, cred, adaPodAccount, "pod-uid-ada", "PRIV")
	if err != nil {
		t.Fatalf("Create(ada's pod, naming PRIV) = %v; want it enrolled, as ada's session naming PRIV is", err)
	}
	if !slices.Equal(enr.Projects, []string{"PRIV"}) || textOf(enr.Operator) != adaPerson {
		t.Fatalf("Create(ada's pod, naming PRIV) = projects %v, operator %s; want [PRIV] and %s", enr.Projects, textOf(enr.Operator), adaPerson)
	}
	read, err := r.svc.Get(context.Background(), enr.ID.String())
	if err != nil || !slices.Equal(read.Projects, []string{"PRIV"}) {
		t.Fatalf("Get(ada's pod enrollment) = projects %v, %v; want [PRIV]", read.Projects, err)
	}
}

// TestAPersonsPodNamingAProjectItsPersonCannotSeeIsRefused: ada is a restricted member. Her pod
// naming ELSE, whose access list does not name her, is refused ErrProjectNotVisible naming ELSE.
// So is a list of PRIV and ELSE, and a list naming PUB, a public project that a restricted member
// does not see. Nothing is enrolled. Her session gets the same refusals.
func TestAPersonsPodNamingAProjectItsPersonCannotSeeIsRefused(t *testing.T) {
	r, _ := newPersonPodProjectsRig(t, projectaccess.MemberRestricted)
	cred := personLogin(t, r, adaPodAccount, "pod-uid-ada", adaPerson)
	for _, c := range []struct {
		projects []string
		unseen   string
	}{
		{[]string{"ELSE"}, "ELSE"},
		{[]string{"PRIV", "ELSE"}, "ELSE"},
		{[]string{"PUB"}, "PUB"},
	} {
		_, err := r.enrollNaming(t, cred, adaPodAccount, "pod-uid-ada", c.projects...)
		if !errors.Is(err, ErrProjectNotVisible) || !strings.Contains(err.Error(), c.unseen) {
			t.Errorf("Create(ada's pod, naming %v) = %v; want ErrProjectNotVisible naming %s", c.projects, err, c.unseen)
		}
	}
	if got := r.rows(t, "pod-uid-ada"); got != 0 {
		t.Fatalf("enrollments naming pod-uid-ada after the refusals = %d, want none", got)
	}
}

// TestAFullMembersPodEnrollsAsBefore: ada is a full member. Her pod naming no project is enrolled
// with no project, as before. Her next pod names PUB and PRIV, which she sees, and it is enrolled
// with both, as her session naming them would be.
func TestAFullMembersPodEnrollsAsBefore(t *testing.T) {
	r, _ := newPersonPodProjectsRig(t, projectaccess.MemberFull)
	first := personLogin(t, r, adaPodAccount, "pod-uid-ada", adaPerson)
	enr, err := r.enrollNaming(t, first, adaPodAccount, "pod-uid-ada")
	if err != nil || enr.Projects != nil {
		t.Fatalf("Create(ada's pod, naming no project) = projects %v, %v; want it enrolled naming none", enr.Projects, err)
	}

	next := personLogin(t, r, adaPodAccount, "pod-uid-ada-two", adaPerson)
	named, err := r.enrollNaming(t, next, adaPodAccount, "pod-uid-ada-two", "PUB", "PRIV")
	if err != nil || !slices.Equal(named.Projects, []string{"PRIV", "PUB"}) {
		t.Fatalf("Create(ada's next pod, naming PUB and PRIV) = projects %v, %v; want it enrolled naming [PRIV PUB]", named.Projects, err)
	}
}

// TestAPersonsPodNamingProjectsIsRefusedWhileDispatchCannotCheckThem: while Dispatch is down,
// ada's pod naming PRIV is refused ErrProjectsUnchecked and nothing is enrolled, as her session
// naming PRIV is. Her pod naming no project is enrolled, as her session naming none is. On a
// broker with no client of Dispatch's project access, her pod naming PRIV is refused
// ErrProjectsUnchecked too.
func TestAPersonsPodNamingProjectsIsRefusedWhileDispatchCannotCheckThem(t *testing.T) {
	r, dispatch := newPersonPodProjectsRig(t, projectaccess.MemberRestricted)
	dispatch.SetDown(true)
	cred := personLogin(t, r, adaPodAccount, "pod-uid-ada", adaPerson)
	if _, err := r.enrollNaming(t, cred, adaPodAccount, "pod-uid-ada", "PRIV"); !errors.Is(err, ErrProjectsUnchecked) {
		t.Fatalf("Create(ada's pod, naming PRIV, Dispatch down) = %v; want ErrProjectsUnchecked", err)
	}
	if got := r.rows(t, "pod-uid-ada"); got != 0 {
		t.Fatalf("enrollments naming pod-uid-ada after the refusal = %d, want none", got)
	}
	if enr, err := r.enrollNaming(t, cred, adaPodAccount, "pod-uid-ada"); err != nil || enr.Projects != nil {
		t.Fatalf("Create(ada's pod, naming no project, Dispatch down) = projects %v, %v; want it enrolled naming none, as her session is", enr.Projects, err)
	}

	r.svc.Projects = nil
	next := personLogin(t, r, adaPodAccount, "pod-uid-ada-two", adaPerson)
	if _, err := r.enrollNaming(t, next, adaPodAccount, "pod-uid-ada-two", "PRIV"); !errors.Is(err, ErrProjectsUnchecked) {
		t.Fatalf("Create(ada's pod, naming PRIV, no client) = %v; want ErrProjectsUnchecked", err)
	}
	if got := r.rows(t, "pod-uid-ada-two"); got != 0 {
		t.Fatalf("enrollments naming pod-uid-ada-two after the refusal = %d, want none", got)
	}
}
