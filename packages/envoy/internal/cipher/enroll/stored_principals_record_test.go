//go:build recordprincipals

// stored_principals_record_test.go records, on the commit it runs at, the text each of today's
// writers stores in the four columns that name an identity: oauth_session_bindings.principal,
// launcher_credentials.subject, enrollments.carry_principal and enrollments.owner. Nothing in
// the broker migrates those rows, so whatever code reads them later must read these spellings.
// It writes ../identity/boot/testdata/stored-principals.json, which
// TestStoredPrincipalsStayValid replays against the code of a later commit. It runs only under
// the recordprincipals tag, by hand, on the commit whose spellings it records:
//
//	BROKER_TEST_DATABASE_URL=… go test -tags recordprincipals -run TestRecordStoredPrincipals -count=1 ./internal/cipher/enroll/
package enroll

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
)

// recordedPrincipalsFile is where the recorder writes, relative to this package's directory:
// beside TestStoredPrincipalsStayValid, which reads it.
const recordedPrincipalsFile = "../identity/boot/testdata/stored-principals.json"

// recordedCompute is the Compute Engine instance a machine's token names.
type recordedCompute struct {
	Project    string `json:"project"`
	Zone       string `json:"zone"`
	Instance   string `json:"instance"`
	InstanceID string `json:"instance_id"`
}

// recordedCaller is the facts that made a row: enough to make the same caller again. Kind is
// the provider whose tokens the caller presented ("kubernetes", "github", "google"), or
// "devbox" for a dev box's machine login, which no provider verifies.
type recordedCaller struct {
	Kind            string           `json:"kind"`
	Account         string           `json:"account,omitempty"`
	PodUID          string           `json:"pod_uid,omitempty"`
	Repository      string           `json:"repository,omitempty"`
	Workflow        string           `json:"workflow,omitempty"`
	Ref             string           `json:"ref,omitempty"`
	Person          string           `json:"person,omitempty"`
	MachineAccount  string           `json:"machine_account,omitempty"`
	Compute         *recordedCompute `json:"compute,omitempty"`
	Thumbprint      string           `json:"thumbprint,omitempty"`
	Service         string           `json:"service,omitempty"`
	Claim           string           `json:"claim,omitempty"`
	Slot            string           `json:"slot,omitempty"`
	Session         string           `json:"session,omitempty"`
	LauncherAccount string           `json:"launcher_account,omitempty"`
}

type recordedRow struct {
	Column string         `json:"column"`
	Case   string         `json:"case"`
	Caller recordedCaller `json:"caller"`
	Value  string         `json:"value"`
}

type recordedPrincipals struct {
	RecordedAt string        `json:"recorded_at"`
	RecordedBy string        `json:"recorded_by"`
	Rows       []recordedRow `json:"rows"`
}

// recordCase is one row to record: write runs today's writer, and read selects the column back
// by the row's own key ("" with pgx.ErrNoRows when write wrote nothing).
type recordCase struct {
	column string
	name   string
	caller recordedCaller
	write  func(t *testing.T)
	read   func(ctx context.Context) (*string, error)
}

const (
	recordPerson        = "ada@example.com"
	recordSignInDomain  = "example.com"
	recordPodAccount    = "system:serviceaccount:example-ns:example-worker"
	recordReadAccount   = "system:serviceaccount:example-ns:example-reader"
	recordReadService   = "example-reader"
	recordRepository    = "example-org/example-repo"
	recordWorkflow      = ".github/workflows/release.yaml"
	recordRef           = "refs/heads/main"
	recordWorkflowRef   = recordRepository + "/" + recordWorkflow + "@" + recordRef
	recordDevBoxCredID  = "00000000-0000-4000-8000-000000000001"
	recordTokenLoginAcc = "system:serviceaccount:example-ns:example-daemon"
)

var recordComputeEngine = exchange.ComputeEngine{Project: "example-project", Zone: "example-zone-a", Instance: "example-machine-1", InstanceID: "1234567890"}

func recordedComputeOf(c exchange.ComputeEngine) *recordedCompute {
	return &recordedCompute{Project: c.Project, Zone: c.Zone, Instance: c.Instance, InstanceID: c.InstanceID}
}

// TestRecordStoredPrincipals drives each writer of the four identity columns once per spelling,
// reads each column back with a select, and writes the rows to recordedPrincipalsFile. It fails,
// writing nothing, when any case reads back no row or a null: a recorder that enumerated nothing
// must not leave a file a later test passes against.
func TestRecordStoredPrincipals(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	issuer, key := withPodVerifier(t, svc)
	pods := podRig{svc: svc, issuer: issuer, key: key}
	svc.ServiceLogins = map[string]string{launcherAccount: podLoginService, recordTokenLoginAcc: podLoginService}
	svc.ServicesByAccount = map[string]string{workerPodAccount: podLoginService}
	bindings := oauthstore.New(svc.Store.Pool)
	expires := time.Now().Add(time.Hour)

	set, err := exchange.NewSet(
		[]exchange.AccountEntry{{Account: recordPodAccount, Person: recordPerson}},
		[]exchange.ReadEntry{{Account: recordReadAccount, Service: recordReadService, Session: "record-read-session"}},
		[]exchange.WorkflowEntry{{Repository: recordRepository, Workflow: recordWorkflow, Ref: recordRef, Project: "ACME", Person: recordPerson}},
		[]exchange.GoogleEntry{{Account: machineAccountEmail, Service: machineFleet}},
		recordSignInDomain,
	)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	reserved := map[string]string{}
	for _, r := range set.Reservations() {
		reserved[r.Session] = r.Principal
	}
	// bind claims session for claims' principal as the token exchange does (oauth's
	// claimSession): fixed when the configuration reserves the name for that principal.
	bind := func(t *testing.T, decide func() (exchange.Claims, error)) {
		t.Helper()
		claims, err := decide()
		if err != nil {
			t.Fatalf("decide: %v", err)
		}
		if _, err := bindings.BindSession(ctx, claims.Session, claims.Principal, reserved[claims.Session] == claims.Principal, expires); err != nil {
			t.Fatalf("BindSession(%s, %s): %v", claims.Session, claims.Principal, err)
		}
	}
	bindingOf := func(session string) func(context.Context) (*string, error) {
		return func(ctx context.Context) (*string, error) {
			var value *string
			err := svc.Store.Pool.QueryRow(ctx, `select principal from oauth_session_bindings where session=$1`, session).Scan(&value)
			return value, err
		}
	}
	podToken := func(account, uid string) map[string]any {
		return map[string]any{"sub": account, "kubernetes.io": map[string]any{"pod": map[string]any{"uid": uid}}}
	}
	machineToken := map[string]any{
		"email": machineAccountEmail, "email_verified": true,
		"google": map[string]any{"compute_engine": map[string]any{
			"project_id": recordComputeEngine.Project, "zone": recordComputeEngine.Zone,
			"instance_name": recordComputeEngine.Instance, "instance_id": recordComputeEngine.InstanceID,
		}},
	}

	// subjectOf selects a token login's subject back by the login's key, which IssueTokenLoginTx
	// writes inside the exchange's transaction (issue).
	subjectOf := func(k *loginKey) func(context.Context) (*string, error) {
		return func(ctx context.Context) (*string, error) {
			var value *string
			err := svc.Store.Pool.QueryRow(ctx, `select subject from launcher_credentials where key_thumbprint=$1`, k.thumbprint).Scan(&value)
			return value, err
		}
	}
	issue := func(t *testing.T, login TokenLogin) Credential {
		t.Helper()
		tx, err := svc.Store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		id, err := svc.IssueTokenLoginTx(ctx, tx, login)
		if err != nil {
			t.Fatalf("IssueTokenLoginTx(%s): %v", login.Subject, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		cred, err := svc.CredentialForKey(ctx, id.String(), login.Thumbprint)
		if err != nil {
			t.Fatalf("CredentialForKey(%s): %v", id, err)
		}
		return cred
	}
	machineLogin := func(subject string, k loginKey, operator string) TokenLogin {
		return TokenLogin{
			Subject: subject, Service: machineFleet, Operator: operator,
			Host:         recordComputeEngine.Project + "/" + recordComputeEngine.Zone + "/" + recordComputeEngine.Instance,
			TokenExpires: expires, Thumbprint: k.thumbprint, JWK: k.jwk, Expires: expires,
		}
	}
	enrollmentColumn := func(column, runtimeID, slot string) func(context.Context) (*string, error) {
		return func(ctx context.Context) (*string, error) {
			var value *string
			err := svc.Store.Pool.QueryRow(ctx, `select `+column+` from enrollments where runtime_id=$1 and coalesce(slot,'')=$2`, runtimeID, slot).Scan(&value)
			return value, err
		}
	}
	create := func(t *testing.T, cred Credential, in Enrollment) {
		t.Helper()
		if _, err := svc.Create(ctx, cred, in); err != nil {
			t.Fatalf("Create(%s %s): %v", in.Kind, in.RuntimeID, err)
		}
	}

	ceKey, poolKey, signInKey, k8sKey := newLoginKey(t), newLoginKey(t), newLoginKey(t), newLoginKey(t)
	machineCarryKey, machineOwnerKey, signInCarryKey, signInOwnerKey := newLoginKey(t), newLoginKey(t), newLoginKey(t), newLoginKey(t)
	ceIdentity := exchange.GoogleIdentity{Account: machineAccountEmail, EmailVerified: true, Compute: recordComputeEngine}
	poolIdentity := exchange.GoogleIdentity{Account: machineAccountEmail, EmailVerified: true}
	carryMachineSubject := exchange.MachineSubject(poolIdentity, machineCarryKey.thumbprint)
	ownerMachineSubject := exchange.MachineSubject(poolIdentity, machineOwnerKey.thumbprint)

	cases := []recordCase{
		{
			column: "oauth_session_bindings.principal", name: "a pod of an account entry trades for a session name",
			caller: recordedCaller{Kind: "kubernetes", Account: recordPodAccount, PodUID: "record-pod-uid-1", Person: recordPerson, Session: "record-pod-session"},
			write: func(t *testing.T) {
				bind(t, func() (exchange.Claims, error) {
					return set.Pod(podToken(recordPodAccount, "record-pod-uid-1"), "record-pod-session", nil)
				})
			},
			read: bindingOf("record-pod-session"),
		},
		{
			column: "oauth_session_bindings.principal", name: "a read entry's pod trades for the session its entry fixes",
			caller: recordedCaller{Kind: "kubernetes", Account: recordReadAccount, PodUID: "record-reader-uid-1", Service: recordReadService, Session: "record-read-session"},
			write: func(t *testing.T) {
				bind(t, func() (exchange.Claims, error) {
					return set.Pod(podToken(recordReadAccount, "record-reader-uid-1"), "", nil)
				})
			},
			read: bindingOf("record-read-session"),
		},
		{
			column: "oauth_session_bindings.principal", name: "a workflow run trades for a session name",
			caller: recordedCaller{Kind: "github", Repository: recordRepository, Workflow: recordWorkflow, Ref: recordRef, Person: recordPerson, Session: "record-workflow-session"},
			write: func(t *testing.T) {
				bind(t, func() (exchange.Claims, error) {
					return set.Workflow(map[string]any{
						"repository": recordRepository, "workflow_ref": recordWorkflowRef, "job_workflow_ref": recordWorkflowRef, "ref": recordRef,
					}, "record-workflow-session", nil)
				})
			},
			read: bindingOf("record-workflow-session"),
		},
		{
			column: "oauth_session_bindings.principal", name: "a machine trades its Google identity token for a session name",
			caller: recordedCaller{Kind: "google", Account: machineAccountEmail, Compute: recordedComputeOf(recordComputeEngine), Service: machineFleet, Session: "record-machine-session"},
			write: func(t *testing.T) {
				bind(t, func() (exchange.Claims, error) {
					return set.Google(machineToken, "record-machine-session", nil)
				})
			},
			read: bindingOf("record-machine-session"),
		},
		{
			column: "oauth_session_bindings.principal", name: "a person's sign-in on a machine trades for a session name",
			caller: recordedCaller{Kind: "google", Person: recordPerson, MachineAccount: machineAccountEmail, Compute: recordedComputeOf(recordComputeEngine), Service: machineFleet, Session: "record-signin-session"},
			write: func(t *testing.T) {
				bind(t, func() (exchange.Claims, error) {
					return set.Person(map[string]any{
						"sub": "1234567890", "email": recordPerson, "email_verified": true, "hd": recordSignInDomain,
					}, machineToken, "record-signin-session", nil)
				})
			},
			read: bindingOf("record-signin-session"),
		},
		{
			column: "oauth_session_bindings.principal", name: "a dev box's machine login trades for a session name",
			caller: recordedCaller{Kind: "devbox", Person: recordPerson, Session: "record-devbox-session"},
			write: func(t *testing.T) {
				bind(t, func() (exchange.Claims, error) {
					return set.Machine(recordDevBoxCredID, recordPerson, "record-devbox-session", nil)
				})
			},
			read: bindingOf("record-devbox-session"),
		},
		{
			column: "launcher_credentials.subject", name: "a machine on a Compute Engine instance trades for its launcher credential",
			caller: recordedCaller{Kind: "google", Account: machineAccountEmail, Compute: recordedComputeOf(recordComputeEngine), Thumbprint: ceKey.thumbprint, Service: machineFleet},
			write: func(t *testing.T) {
				issue(t, machineLogin(exchange.MachineSubject(ceIdentity, ceKey.thumbprint), ceKey, ""))
			},
			read: subjectOf(&ceKey),
		},
		{
			column: "launcher_credentials.subject", name: "a machine with no Compute Engine claims trades for its launcher credential",
			caller: recordedCaller{Kind: "google", Account: machineAccountEmail, Thumbprint: poolKey.thumbprint, Service: machineFleet},
			write: func(t *testing.T) {
				issue(t, machineLogin(exchange.MachineSubject(poolIdentity, poolKey.thumbprint), poolKey, ""))
			},
			read: subjectOf(&poolKey),
		},
		{
			column: "launcher_credentials.subject", name: "a person's sign-in on a machine trades for its launcher credential",
			caller: recordedCaller{Kind: "google", Person: recordPerson, MachineAccount: machineAccountEmail, Compute: recordedComputeOf(recordComputeEngine), Thumbprint: signInKey.thumbprint, Service: machineFleet},
			write: func(t *testing.T) {
				issue(t, machineLogin(exchange.PersonSubject(recordPerson, machineAccountEmail, signInKey.thumbprint), signInKey, recordPerson))
			},
			read: subjectOf(&signInKey),
		},
		{
			column: "launcher_credentials.subject", name: "a Kubernetes service account logs in with its token",
			caller: recordedCaller{Kind: "kubernetes", Account: recordTokenLoginAcc, PodUID: "record-daemon-uid-1", Thumbprint: k8sKey.thumbprint, Service: podLoginService},
			write: func(t *testing.T) {
				// machine.Service.TokenLogin writes the verified token's subject as it is.
				issue(t, pods.tokenLoginOf(recordTokenLoginAcc, "record-daemon-uid-1", k8sKey))
			},
			read: subjectOf(&k8sKey),
		},
		{
			column: "enrollments.carry_principal", name: "a pod its daemon's login enrols under a session name",
			caller: recordedCaller{Kind: "kubernetes", Account: workerPodAccount, PodUID: "record-carry-pod-uid", Service: podLoginService, Session: "record-carry-pod-session", LauncherAccount: launcherAccount},
			write: func(t *testing.T) {
				daemon := pods.login(t, launcherAccount, "")
				in := pods.podEnrollment(t, daemon, workerPodAccount, "record-carry-pod-uid")
				in.SessionID = str("record-carry-pod-session")
				create(t, daemon, in)
			},
			read: enrollmentColumn("carry_principal", "record-carry-pod-uid", ""),
		},
		{
			column: "enrollments.carry_principal", name: "a machine enrols its own session under a session name",
			caller: recordedCaller{Kind: "google", Account: machineAccountEmail, Thumbprint: machineCarryKey.thumbprint, Service: machineFleet, Session: "record-carry-machine-session"},
			write: func(t *testing.T) {
				cred := issue(t, machineLogin(carryMachineSubject, machineCarryKey, ""))
				create(t, cred, Enrollment{Kind: "session", RuntimeID: "record-carry-machine", Thumbprint: "tp-record-carry-machine", SessionID: str("record-carry-machine-session")})
			},
			read: enrollmentColumn("carry_principal", "record-carry-machine", ""),
		},
		{
			column: "enrollments.carry_principal", name: "a person's sign-in enrols a session under a session name",
			caller: recordedCaller{Kind: "google", Person: recordPerson, MachineAccount: machineAccountEmail, Thumbprint: signInCarryKey.thumbprint, Service: machineFleet, Session: "record-carry-signin-session"},
			write: func(t *testing.T) {
				cred := issue(t, machineLogin(exchange.PersonSubject(recordPerson, machineAccountEmail, signInCarryKey.thumbprint), signInCarryKey, recordPerson))
				create(t, cred, Enrollment{Kind: "session", RuntimeID: "record-carry-signin", Thumbprint: "tp-record-carry-signin", SessionID: str("record-carry-signin-session")})
			},
			read: enrollmentColumn("carry_principal", "record-carry-signin", ""),
		},
		{
			column: "enrollments.owner", name: "a pod its launcher enrols under a claim",
			caller: recordedCaller{Kind: "kubernetes", Account: workerPodAccount, PodUID: "record-claim-pod-uid", Service: podLoginService, Claim: plannerClaim, Session: "record-claim-session", LauncherAccount: launcherAccount},
			write: func(t *testing.T) {
				launcher := pods.login(t, launcherAccount, "record-launcher-uid")
				in := pods.podEnrollment(t, launcher, workerPodAccount, "record-claim-pod-uid")
				in.SessionID, in.Claim = str("record-claim-session"), plannerClaim
				create(t, launcher, in)
			},
			read: enrollmentColumn("owner", "record-claim-pod-uid", ""),
		},
		{
			column: "enrollments.owner", name: "a slot of a pod that states no claim",
			caller: recordedCaller{Kind: "kubernetes", Account: workerPodAccount, PodUID: "record-slot-pod-uid", Service: podLoginService, Slot: "planner-g1", Session: "record-slot-session", LauncherAccount: launcherAccount},
			write: func(t *testing.T) {
				launcher := pods.login(t, launcherAccount, "record-slot-launcher-uid")
				in := pods.podEnrollment(t, launcher, workerPodAccount, "record-slot-pod-uid")
				in.SessionID, in.Slot = str("record-slot-session"), "planner-g1"
				create(t, launcher, in)
			},
			read: enrollmentColumn("owner", "record-slot-pod-uid", "planner-g1"),
		},
		{
			column: "enrollments.owner", name: "a machine enrols its own session",
			caller: recordedCaller{Kind: "google", Account: machineAccountEmail, Thumbprint: machineOwnerKey.thumbprint, Service: machineFleet, Session: "record-owner-machine-session"},
			write: func(t *testing.T) {
				cred := issue(t, machineLogin(ownerMachineSubject, machineOwnerKey, ""))
				create(t, cred, Enrollment{Kind: "session", RuntimeID: "record-owner-machine", Thumbprint: "tp-record-owner-machine", SessionID: str("record-owner-machine-session")})
			},
			read: enrollmentColumn("owner", "record-owner-machine", ""),
		},
		{
			column: "enrollments.owner", name: "a person's sign-in enrols a session",
			caller: recordedCaller{Kind: "google", Person: recordPerson, MachineAccount: machineAccountEmail, Thumbprint: signInOwnerKey.thumbprint, Service: machineFleet, Session: "record-owner-signin-session"},
			write: func(t *testing.T) {
				cred := issue(t, machineLogin(exchange.PersonSubject(recordPerson, machineAccountEmail, signInOwnerKey.thumbprint), signInOwnerKey, recordPerson))
				create(t, cred, Enrollment{Kind: "session", RuntimeID: "record-owner-signin", Thumbprint: "tp-record-owner-signin", SessionID: str("record-owner-signin-session")})
			},
			read: enrollmentColumn("owner", "record-owner-signin", ""),
		},
	}

	var rows []recordedRow
	for _, c := range cases {
		c.write(t)
		value, err := c.read(ctx)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			t.Errorf("%s / %s: no row was written to read back", c.column, c.name)
			continue
		case err != nil:
			t.Fatalf("%s / %s: read back: %v", c.column, c.name, err)
		case value == nil || *value == "":
			t.Errorf("%s / %s: the row holds no value", c.column, c.name)
			continue
		}
		rows = append(rows, recordedRow{Column: c.column, Case: c.name, Caller: c.caller, Value: *value})
	}
	if t.Failed() {
		t.Fatalf("recorded %d of %d cases; the file is left as it was", len(rows), len(cases))
	}
	slices.SortFunc(rows, func(a, b recordedRow) int {
		return cmp.Or(strings.Compare(a.Column, b.Column), strings.Compare(a.Case, b.Case))
	})

	head, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	out, err := json.MarshalIndent(recordedPrincipals{
		RecordedAt: strings.TrimSpace(string(head)),
		RecordedBy: "BROKER_TEST_DATABASE_URL=… go test -tags recordprincipals -run TestRecordStoredPrincipals -count=1 ./internal/cipher/enroll/",
		Rows:       rows,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(recordedPrincipalsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordedPrincipalsFile, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("recorded %d rows at %s into %s", len(rows), strings.TrimSpace(string(head)), recordedPrincipalsFile)
}
