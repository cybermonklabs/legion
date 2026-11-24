package boot_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/enroll/enrolltest"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/github"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// recordedPrincipalsFile is what enroll's recorder wrote on the commit named in it: every
// identity string today's broker stores in the four columns that name one.
const recordedPrincipalsFile = "testdata/stored-principals.json"

// The four columns the recorder read back, each of which some code of the new broker must still
// accept the stored text of.
const (
	columnBinding = "oauth_session_bindings.principal"
	columnSubject = "launcher_credentials.subject"
	columnCarry   = "enrollments.carry_principal"
	columnOwner   = "enrollments.owner"
)

// recordedCompute is the Compute Engine instance a machine's token named, as the file writes it.
type recordedCompute struct {
	Project    string `json:"project"`
	Zone       string `json:"zone"`
	Instance   string `json:"instance"`
	InstanceID string `json:"instance_id"`
}

// recordedCaller is the facts that made a row: enough to make the same caller again.
type recordedCaller struct {
	Kind            string           `json:"kind"`
	Account         string           `json:"account"`
	PodUID          string           `json:"pod_uid"`
	Repository      string           `json:"repository"`
	Workflow        string           `json:"workflow"`
	Ref             string           `json:"ref"`
	Person          string           `json:"person"`
	MachineAccount  string           `json:"machine_account"`
	Compute         *recordedCompute `json:"compute"`
	Thumbprint      string           `json:"thumbprint"`
	Service         string           `json:"service"`
	Claim           string           `json:"claim"`
	Slot            string           `json:"slot"`
	Session         string           `json:"session"`
	LauncherAccount string           `json:"launcher_account"`
}

type recordedRow struct {
	Column string         `json:"column"`
	Case   string         `json:"case"`
	Caller recordedCaller `json:"caller"`
	Value  string         `json:"value"`
}

type recordedPrincipals struct {
	RecordedAt string        `json:"recorded_at"`
	Rows       []recordedRow `json:"rows"`
}

// The identities the recorded rows were made under, which the providers here list again.
const (
	storedPerson       = "ada@example.com"
	storedSignInDomain = "example.com"
	storedFleet        = "example-fleet"
	storedPodService   = "legion-daemon"
	storedProject      = "ACME"
)

// TestStoredPrincipalsStayValid replays every identity string today's broker stored and
// authenticates it with the new code: a session name binds under the stored principal, a launcher
// credential of the stored subject authenticates and the provider spells that subject again, an
// enrollment's grants follow the stored carry principal to the session's next enrollment, and an
// enrollment's owner is written as it was. It fails when the file holds no row of one of the four
// columns: a replay that enumerated nothing has not passed.
func TestStoredPrincipalsStayValid(t *testing.T) {
	raw, err := os.ReadFile(recordedPrincipalsFile)
	if err != nil {
		t.Fatalf("read %s: %v", recordedPrincipalsFile, err)
	}
	var recorded recordedPrincipals
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatalf("decode %s: %v", recordedPrincipalsFile, err)
	}
	counts := map[string]int{}
	for _, row := range recorded.Rows {
		counts[row.Column]++
	}
	for _, column := range []string{columnBinding, columnSubject, columnCarry, columnOwner} {
		if counts[column] == 0 {
			t.Fatalf("%s holds no row of %s, so this test enumerated nothing for it; record it again at %s", recordedPrincipalsFile, column, recorded.RecordedAt)
		}
	}
	for _, row := range recorded.Rows {
		t.Run(row.Column+"/"+row.Case, func(t *testing.T) {
			switch row.Column {
			case columnBinding:
				bindingStaysValid(t, row)
			case columnSubject:
				subjectStaysValid(t, row)
			case columnCarry:
				carryStaysValid(t, row)
			case columnOwner:
				ownerStaysValid(t, row)
			default:
				t.Fatalf("the file names the column %s, which this test does not replay", row.Column)
			}
		})
	}
}

// storedProviders is the registry of the three providers listing the row's caller, so the new
// code decides that caller as today's broker decided it.
func storedProviders(t *testing.T, row recordedRow) *identity.Registry {
	t.Helper()
	var accounts []kubernetes.AccountEntry
	var reads []kubernetes.ReadEntry
	var workflows []github.WorkflowEntry
	var machines []google.GoogleEntry
	c := row.Caller
	switch {
	case c.Kind == "kubernetes" && c.Service != "" && c.PodUID != "" && c.Person == "" && row.Column == columnBinding:
		reads = append(reads, kubernetes.ReadEntry{Account: c.Account, Service: c.Service, Session: c.Session})
	case c.Kind == "kubernetes" && c.Account != "":
		accounts = append(accounts, kubernetes.AccountEntry{Account: c.Account, Project: storedProject, Person: storedPerson})
	case c.Kind == "github":
		workflows = append(workflows, github.WorkflowEntry{Repository: c.Repository, Workflow: c.Workflow, Ref: c.Ref, Project: storedProject, Person: c.Person})
	}
	if account := machineAccountOf(c); account != "" {
		machines = append(machines, google.GoogleEntry{Account: account, Service: c.Service, Project: storedProject, Person: storedPerson})
	}
	pods, err := kubernetes.New(kubernetes.Config{Accounts: accounts, Reads: reads})
	if err != nil {
		t.Fatalf("kubernetes.New: %v", err)
	}
	fleets, err := google.New(google.Config{Machines: machines, SignInDomain: storedSignInDomain})
	if err != nil {
		t.Fatalf("google.New: %v", err)
	}
	runs, err := github.New(github.Config{Workflows: workflows})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	registry, err := identity.NewRegistry([]identity.Provider{pods, fleets, runs}, nil)
	if err != nil {
		t.Fatalf("identity.NewRegistry: %v", err)
	}
	return registry
}

// machineAccountOf is the Google account a row's caller runs as: a machine's own, or the machine
// a person signed in on; "" for a caller of another kind.
func machineAccountOf(c recordedCaller) string {
	switch {
	case c.MachineAccount != "":
		return c.MachineAccount
	case c.Kind == "google":
		return c.Account
	}
	return ""
}

// identityOf is the Google identity token's claims the row's machine presented.
func identityOf(c recordedCaller) google.Identity {
	id := google.Identity{Account: machineAccountOf(c), EmailVerified: true}
	if c.Compute != nil {
		id.Compute = google.ComputeEngine{Project: c.Compute.Project, Zone: c.Compute.Zone, Instance: c.Compute.Instance, InstanceID: c.Compute.InstanceID}
	}
	return id
}

// decided is the claims the new code decides for the row's caller: its provider's own decision,
// and for a dev box session, which no provider verifies, the set's (exchange.Set.Machine).
func decided(t *testing.T, row recordedRow, registry *identity.Registry) exchange.Claims {
	t.Helper()
	c := row.Caller
	trade := identity.Trade{Session: c.Session}
	var kind identity.Kind
	switch c.Kind {
	case "devbox":
		claims, err := registry.Policy().Machine("00000000-0000-4000-8000-000000000001", c.Person, c.Session, nil)
		if err != nil {
			t.Fatalf("the set's decision of a dev box session: %v", err)
		}
		return claims
	case "kubernetes":
		kind, trade.TokenType = kubernetes.Kind, kubernetes.SubjectTokenType
		trade.Subject = map[string]any{"sub": c.Account, "kubernetes.io": map[string]any{"pod": map[string]any{"uid": c.PodUID}}}
	case "github":
		run := c.Repository + "/" + c.Workflow + "@" + c.Ref
		kind, trade.TokenType = github.Kind, github.SubjectTokenType
		trade.Subject = map[string]any{"repository": c.Repository, "workflow_ref": run, "job_workflow_ref": run, "ref": c.Ref}
	case "google":
		kind = google.Kind
		machine := googleClaims(identityOf(c))
		if c.Person != "" {
			trade.TokenType, trade.Subject, trade.Actor = google.PersonTokenType, map[string]any{
				"sub": "109ada", "email": c.Person, "email_verified": true, "hd": storedSignInDomain,
			}, machine
		} else {
			trade.TokenType, trade.Subject = google.SubjectTokenType, machine
		}
	default:
		t.Fatalf("the row names the caller kind %q, which this test does not replay", c.Kind)
	}
	provider, held := registry.Provider(kind)
	if !held {
		t.Fatalf("this build holds no %s provider", kind)
	}
	claims, err := provider.Decide(trade)
	if err != nil {
		t.Fatalf("the %s provider's decision of %q: %v", kind, row.Case, err)
	}
	return claims
}

// googleClaims is a machine's Google identity token as the exchange is handed it.
func googleClaims(id google.Identity) map[string]any {
	claims := map[string]any{"email": id.Account, "email_verified": true}
	if !id.Compute.Empty() {
		claims["google"] = map[string]any{"compute_engine": map[string]any{
			"project_id": id.Compute.Project, "zone": id.Compute.Zone,
			"instance_name": id.Compute.Instance, "instance_id": id.Compute.InstanceID,
		}}
	}
	return claims
}

// bindingStaysValid puts the stored binding back and binds the same session under the principal
// the new code decides: the store answers no error and no other holder, which is what says the
// new principal is the stored one.
func bindingStaysValid(t *testing.T, row recordedRow) {
	ctx := context.Background()
	registry := storedProviders(t, row)
	svc := enrolltest.NewService(t, registry, registry.Policy(), nil)
	bindings := oauthstore.New(svc.Store.Pool)
	expires := time.Now().Add(time.Hour)
	if _, err := svc.Store.Pool.Exec(ctx, `insert into oauth_session_bindings (session, principal, expires_at) values ($1,$2,$3)`,
		row.Caller.Session, row.Value, expires); err != nil {
		t.Fatalf("put the stored binding back: %v", err)
	}
	claims := decided(t, row, registry)
	taken, err := bindings.BindSession(ctx, row.Caller.Session, claims.Principal, false, expires)
	if err != nil || taken != "" {
		t.Fatalf("the new code's principal %q does not hold the stored name %q: BindSession = %q, %v; the stored row is %q",
			claims.Principal, row.Caller.Session, taken, err, row.Value)
	}
}

// subjectStaysValid issues a launcher credential of the stored subject and authenticates it with
// the new code, and spells the subject again from the row's own facts.
func subjectStaysValid(t *testing.T, row recordedRow) {
	ctx := context.Background()
	c := row.Caller
	registry := storedProviders(t, row)
	svc := enrolltest.NewService(t, registry, registry.Policy(), nil)
	// A Kubernetes token login's subject is the account itself, held to BROKER_SERVICE_LOGINS; a
	// machine's and a sign-in's are held to the fleet its entry lists (the policy above).
	if c.Kind == "kubernetes" {
		svc.ServiceLogins = map[string]string{c.Account: c.Service}
	}
	cred := enrolltest.TokenLogin(t, svc, enroll.TokenLogin{
		Subject: row.Value, Service: c.Service, Operator: c.Person, Host: "example-host", PodUID: c.PodUID,
	})
	// AuthenticateLauncher answers the credential's key thumbprint and whether it still stands:
	// true here is the new code accepting the stored subject, which it reads through Subjects
	// and holds to the policy's listing (tokenLoginListed).
	thumbprint, ok, err := svc.AuthenticateLauncher(ctx, cred.ID.String())
	if err != nil || !ok || thumbprint == "" {
		t.Fatalf("the stored subject %q no longer authenticates: AuthenticateLauncher = %q, %t, %v", row.Value, thumbprint, ok, err)
	}
	// The same subject, spelled again by the code that writes one today.
	var spelled string
	switch {
	case c.Kind == "kubernetes":
		spelled = c.Account
	case c.Person != "":
		spelled = google.PersonSubject(c.Person, c.MachineAccount, c.Thumbprint)
	default:
		spelled = google.MachineSubject(identityOf(c), c.Thumbprint)
	}
	if spelled != row.Value {
		t.Fatalf("the new code spells the subject %q, and the stored one is %q", spelled, row.Value)
	}
}

// carryStaysValid puts the stored carry principal on an enrollment of the row's caller, ends it
// dormant with a live grant, and enrols the same caller under the same session again: the grant
// moves to the new enrollment, which is what says the new code's principal matches the stored
// text.
func carryStaysValid(t *testing.T, row recordedRow) {
	ctx := context.Background()
	svc, cred, in, mintPodToken := storedEnrollment(t, row)
	first, err := svc.Create(ctx, cred, in)
	if err != nil {
		t.Fatalf("the first enrollment: %v", err)
	}
	enrolltest.SetCarryPrincipal(t, svc, first.ID, row.Value)
	grant := enrolltest.InsertLiveGrant(t, svc, first.ID)
	if err := svc.Revoke(ctx, cred, first.ID.String(), "launcher"); err != nil {
		t.Fatalf("end the first enrollment: %v", err)
	}
	next := in
	next.RuntimeID = in.RuntimeID + "-next"
	next.Thumbprint = in.Thumbprint + "-next"
	if mintPodToken != nil {
		next.PodToken = mintPodToken(next.RuntimeID)
		next.Subject = in.Subject
	}
	second, err := svc.Create(ctx, cred, next)
	if err != nil {
		t.Fatalf("the second enrollment: %v", err)
	}
	if holder := enrolltest.GrantHolder(t, svc, grant); holder != second.ID {
		t.Fatalf("the grant stayed on %s rather than moving to %s: the new code's carry principal is %q, and the stored one is %q",
			holder, second.ID, enrolltest.CarryPrincipal(t, svc, second.ID), row.Value)
	}
}

// ownerStaysValid enrols the row's caller and reads the owner the new code wrote.
func ownerStaysValid(t *testing.T, row recordedRow) {
	svc, cred, in, _ := storedEnrollment(t, row)
	made, err := svc.Create(context.Background(), cred, in)
	if err != nil {
		t.Fatalf("the enrollment: %v", err)
	}
	if owner := enrolltest.Owner(t, svc, made.ID); owner != row.Value {
		t.Fatalf("the new code writes the owner %q, and the stored one is %q", owner, row.Value)
	}
}

// storedEnrollment is the service, the credential, the enrollment the row's caller makes, and
// how that caller mints a pod token for another pod of its own (nil for a caller that is no
// pod): a pod enrols under its daemon's token login, with a real projected token the kubernetes
// provider verifies; a machine or a sign-in enrols under its own launcher credential.
func storedEnrollment(t *testing.T, row recordedRow) (*enroll.Service, enroll.Credential, enroll.Enrollment, func(podUID string) string) {
	t.Helper()
	c := row.Caller
	if c.Kind != "kubernetes" {
		registry := storedProviders(t, row)
		svc := enrolltest.NewService(t, registry, registry.Policy(), nil)
		subject := google.MachineSubject(identityOf(c), c.Thumbprint)
		if c.Person != "" {
			subject = google.PersonSubject(c.Person, c.MachineAccount, c.Thumbprint)
		}
		cred := enrolltest.TokenLogin(t, svc, enroll.TokenLogin{
			Subject: subject, Service: c.Service, Operator: c.Person, Host: "example-host",
		})
		session := c.Session
		return svc, cred, enroll.Enrollment{
			Kind: "session", RuntimeID: "stored-" + c.Thumbprint[:8], Thumbprint: "tp-stored-" + c.Thumbprint[:8],
			SessionID: &session,
		}, nil
	}
	// A pod: the cluster's issuer stands in for the real one, and its daemon logs in as the
	// launcher, exactly as the recorder's rig did.
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "cluster-key")
	keys, err := oidc.NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatal(err)
	}
	pods, err := kubernetes.New(kubernetes.Config{
		Issuer: issuer.URL(), Keys: keys,
		Accounts: []kubernetes.AccountEntry{{Account: c.Account, Project: storedProject, Person: storedPerson}},
	})
	if err != nil {
		t.Fatalf("kubernetes.New: %v", err)
	}
	registry, err := identity.NewRegistry([]identity.Provider{pods}, nil)
	if err != nil {
		t.Fatalf("identity.NewRegistry: %v", err)
	}
	svc := enrolltest.NewService(t, registry, registry.Policy(), pods.PodVerifier(storedPodAudience))
	svc.ServiceLogins = map[string]string{c.LauncherAccount: c.Service}
	svc.ServicesByAccount = map[string]string{c.Account: c.Service}
	cred := enrolltest.TokenLogin(t, svc, enroll.TokenLogin{
		Subject: c.LauncherAccount, Service: c.Service, Host: "legion/" + c.LauncherAccount,
	})
	session, subject := c.Session, c.Account
	return svc, cred, enroll.Enrollment{
		Kind: "pod", RuntimeID: c.PodUID, Thumbprint: "tp-" + c.PodUID,
		PodToken: podToken(t, issuer, key, c.Account, c.PodUID), Subject: &subject,
		SessionID: &session, Claim: c.Claim, Slot: c.Slot,
	}, func(podUID string) string {
		return podToken(t, issuer, key, c.Account, podUID)
	}
}

// storedPodAudience is the audience the pod tokens of this test carry.
const storedPodAudience = "stored-broker"

// podToken is a projected service-account token of account bound to podUID, signed by issuer.
func podToken(t *testing.T, issuer *oidctest.Issuer, key *oidctest.Key, account, podUID string) string {
	t.Helper()
	claims := issuer.Claims(account, storedPodAudience)
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	return issuer.Mint(t, key, claims)
}
