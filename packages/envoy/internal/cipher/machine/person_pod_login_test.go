// person_pod_login_test.go drives TokenLogin for an account that BROKER_POD_OPERATORS maps to a
// person. A token of that account bound to a pod logs in with the person as the credential's
// operator, so the credential is a person's pod login (enroll.PersonPodLogin). Every other token
// login has no operator: a token bound to no pod, an account that the setting does not map, and a
// daemon account that BROKER_EXCHANGE_ACCOUNTS maps to a person. The setting is read at each
// login, so the next login of the same pod writes the operator that the setting gives then.
package machine

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

const (
	// adaPerson is the person that BROKER_POD_OPERATORS maps adaPodAccount to. gracePerson is
	// another person.
	adaPerson   = "ada@example.com"
	gracePerson = "grace@example.com"
	// adaPodAccount is the account that ada's pods run as. adaLoginService is the login service
	// that BROKER_SERVICE_LOGINS gives that account. adaPodUID is one pod of that account.
	adaPodAccount   = "system:serviceaccount:people:ada"
	adaLoginService = "person-ada"
	adaPodUID       = "pod-uid-ada-one"
	// adaWorkerAccount is a daemon's account that a BROKER_EXCHANGE_ACCOUNTS entry maps to ada,
	// such as system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com.
	// BROKER_SERVICE_LOGINS gives it adaWorkerService. BROKER_POD_OPERATORS does not name it.
	adaWorkerAccount = "system:serviceaccount:example-ns:example-worker"
	adaWorkerService = "example-worker"
	// daemonPodUID is one pod of daemonTokenAccount.
	daemonPodUID = "pod-uid-daemon-one"
)

// newPersonPodFixture is newFixture with the settings of a deployment that maps one account to a
// person. BROKER_SERVICE_LOGINS gives adaPodAccount ada's own login service, gives
// daemonTokenAccount legion-daemon, and gives adaWorkerAccount its own service.
// BROKER_POD_OPERATORS maps adaPodAccount to ada, and no other account.
func newPersonPodFixture(t *testing.T) *Service {
	t.Helper()
	svc := newFixture(t)
	svc.Enroll.ServiceLogins = map[string]string{
		adaPodAccount:      adaLoginService,
		daemonTokenAccount: "legion-daemon",
		adaWorkerAccount:   adaWorkerService,
	}
	svc.Enroll.PodOperators = map[string]string{adaPodAccount: adaPerson}
	return svc
}

// personPodTokenLogin logs account in as POST /v1/launcher-credentials/token does. svc's verifier
// answers a token of account bound to podUID ("" for a token bound to no pod), VerifyServiceToken
// maps that token, and TokenLogin issues the credential under a fresh key, from a request object
// that names service. It answers VerifyServiceToken's answer and the credential as
// enroll.Service.Create reads it, by the id and the key of this login.
func personPodTokenLogin(t *testing.T, svc *Service, account, podUID, service string) (ServiceAccount, enroll.Credential) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	svc.Enroll.Pod = fakeTokens{claims: enroll.AccountClaims{Subject: account, PodUID: podUID, IssuedAt: now, Expiry: now.Add(time.Hour)}}
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	compact, err := record.Sign(key, testAudience, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: service, Service: service},
	}, "", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := svc.VerifyServiceToken(ctx, "a token of "+account)
	if err != nil {
		t.Fatalf("VerifyServiceToken(%s, pod %q): %v", account, podUID, err)
	}
	issued, err := svc.TokenLogin(ctx, verified, compact)
	if err != nil {
		t.Fatalf("TokenLogin(%s, pod %q): %v", account, podUID, err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := svc.Enroll.CredentialForKey(ctx, issued.ID, thumbprint)
	if err != nil {
		t.Fatalf("CredentialForKey(%s, the key of the login of %s): %v", issued.ID, account, err)
	}
	return verified, cred
}

// wantOperator fails t unless cred, the credential of what, carries the operator want. want ""
// is no operator, which is a null operator column. A credential that carries a person must also
// be a person's pod login (enroll.PersonPodLogin).
func wantOperator(t *testing.T, what string, cred enroll.Credential, want string) {
	t.Helper()
	if want == "" {
		if cred.Operator != nil {
			t.Fatalf("%s: credential %s carries operator %q, so it would act as that person; want no operator", what, cred.ID, *cred.Operator)
		}
		return
	}
	if cred.Operator == nil || *cred.Operator != want {
		t.Fatalf("%s: credential %s carries operator %q (\"\" is none); want %q", what, cred.ID, deref(cred.Operator), want)
	}
	if !enroll.PersonPodLogin(cred) {
		t.Fatalf("%s: credential %s (operator %q, subject %q) is not a person's pod login (enroll.PersonPodLogin)", what, cred.ID, want, deref(cred.Subject))
	}
}

// TestPersonPodTokenLoginWritesThePersonAsOperator pins that a token of a mapped account bound to
// a pod logs in with the person as the credential's operator. The credential names the account
// and that pod, and it is a person's pod login (enroll.PersonPodLogin).
func TestPersonPodTokenLoginWritesThePersonAsOperator(t *testing.T) {
	svc := newPersonPodFixture(t)
	account, cred := personPodTokenLogin(t, svc, adaPodAccount, adaPodUID, adaLoginService)
	if account.Person != adaPerson || account.Service != adaLoginService {
		t.Fatalf("VerifyServiceToken(%s) = person %q, service %q; want %q, %q", adaPodAccount, account.Person, account.Service, adaPerson, adaLoginService)
	}
	if cred.Subject == nil || *cred.Subject != adaPodAccount || cred.PodUID != adaPodUID || cred.Service == nil || *cred.Service != adaLoginService {
		t.Fatalf("credential %s = subject %q, pod %q, service %q; want %s, %s, %s", cred.ID, deref(cred.Subject), cred.PodUID, deref(cred.Service), adaPodAccount, adaPodUID, adaLoginService)
	}
	wantOperator(t, "a pod-bound token of "+adaPodAccount, cred, adaPerson)
}

// TestPersonPodTokenBoundToNoPodLogsInWithNoOperator pins that a token of a mapped account bound
// to no pod, such as one `kubectl create token` mints, logs in with no operator. The test first
// asserts that VerifyServiceToken still names the person, so it is TokenLogin that writes no
// operator.
func TestPersonPodTokenBoundToNoPodLogsInWithNoOperator(t *testing.T) {
	svc := newPersonPodFixture(t)
	account, cred := personPodTokenLogin(t, svc, adaPodAccount, "", adaLoginService)
	if account.Person != adaPerson {
		t.Fatalf("VerifyServiceToken(%s, no pod) = person %q; want %q", adaPodAccount, account.Person, adaPerson)
	}
	if cred.PodUID != "" {
		t.Fatalf("credential %s names pod %q; want none", cred.ID, cred.PodUID)
	}
	wantOperator(t, "a token of "+adaPodAccount+" bound to no pod", cred, "")
}

// TestPersonPodOperatorsGiveAnUnmappedAccountNoOperator pins that a pod-bound token of an account
// that BROKER_POD_OPERATORS does not map logs in with no operator: with the setting unset, and
// with the setting mapping another account.
func TestPersonPodOperatorsGiveAnUnmappedAccountNoOperator(t *testing.T) {
	for _, c := range []struct {
		name         string
		podOperators map[string]string
	}{
		{"the setting unset", nil},
		{"the setting maps another account", map[string]string{adaPodAccount: adaPerson}},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := newPersonPodFixture(t)
			svc.Enroll.PodOperators = c.podOperators
			account, cred := personPodTokenLogin(t, svc, daemonTokenAccount, daemonPodUID, "legion-daemon")
			if account.Person != "" {
				t.Fatalf("VerifyServiceToken(%s) = person %q; want none", daemonTokenAccount, account.Person)
			}
			wantOperator(t, "a pod-bound token of "+daemonTokenAccount, cred, "")
		})
	}
}

// TestPersonPodOperatorsGiveADaemonOfAPersonNoOperator pins that a pod-bound token of a daemon's
// account, adaWorkerAccount, logs in with no operator. BROKER_SERVICE_LOGINS lists the account.
// BROKER_POD_OPERATORS does not map it, while the setting maps another account to ada
// (newPersonPodFixture). This package does not read BROKER_EXCHANGE_ACCOUNTS, the setting that
// maps a daemon's account to a person for Dispatch. config's
// TestPodOperatorsUnsetMapsNoDaemonToAPerson checks that half: with BROKER_EXCHANGE_ACCOUNTS
// mapping each daemon's account to a person and BROKER_POD_OPERATORS unset, Load maps no account
// to a person.
func TestPersonPodOperatorsGiveADaemonOfAPersonNoOperator(t *testing.T) {
	svc := newPersonPodFixture(t)
	account, cred := personPodTokenLogin(t, svc, adaWorkerAccount, "pod-uid-worker-one", adaWorkerService)
	if account.Person != "" {
		t.Fatalf("VerifyServiceToken(%s) = person %q; want none", adaWorkerAccount, account.Person)
	}
	wantOperator(t, "a pod-bound token of "+adaWorkerAccount, cred, "")
}

// TestPersonPodLoginReadsTheSettingAtEachLogin pins that each login of the same pod writes the
// operator that BROKER_POD_OPERATORS gives at that login. The first login writes ada. The test
// then changes the setting, as a broker restart with an edited setting does. After the setting
// drops the account, the next login of the pod writes no operator. After the setting moves the
// account to grace, the next login writes grace. Each later login moves the first login's
// credential, so the id stays the same, and the subject holds one live credential.
func TestPersonPodLoginReadsTheSettingAtEachLogin(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(podOperators map[string]string)
		want   string
	}{
		{"the setting drops the account", func(m map[string]string) { delete(m, adaPodAccount) }, ""},
		{"the setting moves the account to another person", func(m map[string]string) { m[adaPodAccount] = gracePerson }, gracePerson},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := newPersonPodFixture(t)
			ctx := context.Background()
			_, first := personPodTokenLogin(t, svc, adaPodAccount, adaPodUID, adaLoginService)
			wantOperator(t, "the first login of pod "+adaPodUID, first, adaPerson)

			c.change(svc.Enroll.PodOperators)
			account, next := personPodTokenLogin(t, svc, adaPodAccount, adaPodUID, adaLoginService)
			if account.Person != c.want {
				t.Fatalf("VerifyServiceToken(%s) after %s = person %q; want %q", adaPodAccount, c.name, account.Person, c.want)
			}
			if next.ID != first.ID {
				t.Fatalf("the next login of pod %s issued credential %s; want it to move credential %s", adaPodUID, next.ID, first.ID)
			}
			wantOperator(t, "the next login of pod "+adaPodUID+" after "+c.name, next, c.want)

			var operator *string
			var live int
			if err := svc.Store.Pool.QueryRow(ctx, `select operator, (select count(*) from launcher_credentials where subject=$2 and revoked_at is null)
				from launcher_credentials where id=$1`, first.ID, adaPodAccount).Scan(&operator, &live); err != nil {
				t.Fatalf("read launcher_credentials %s: %v", first.ID, err)
			}
			if c.want == "" && operator != nil {
				t.Fatalf("launcher_credentials %s has operator %q after %s; want null", first.ID, *operator, c.name)
			}
			if c.want != "" && (operator == nil || *operator != c.want) {
				t.Fatalf("launcher_credentials %s has operator %q (\"\" is null) after %s; want %q", first.ID, deref(operator), c.name, c.want)
			}
			if live != 1 {
				t.Fatalf("%s holds %d live credentials after the next login of pod %s; want 1", adaPodAccount, live, adaPodUID)
			}
		})
	}
}
