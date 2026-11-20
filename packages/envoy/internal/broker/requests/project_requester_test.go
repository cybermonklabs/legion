// project_requester_test.go drives a capped secret of a placeholder App (the shape
// dispatch://INFRA-3885 needs) through the
// request machine: a secret whose requesters name one Dispatch project's pods of the legion-daemon
// service (legion-daemon/OSSV), and whose github-repositories and github-permissions tags cap
// every token it mints. Every Legion project's issue pods run as the one account legion-worker,
// and each project's daemon logs in as an account of its own, so the daemon's account is what
// names the pods' project; and a pod that asks for the token with no narrowing gets the capped
// token, not the App's whole one.
package requests

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// The accounts of the deployment these tests stand in for: legion/'s namespace, whose shared pod
// account every project's issue pods run as, OSSV's and INFRA's own pod accounts, and the two
// daemons' accounts, which log in as the legion-daemon service.
const (
	legionWorker      = "system:serviceaccount:legion:legion-worker"
	legionOssvWorker  = "system:serviceaccount:legion:legion-ossv-worker"
	legionInfraWorker = "system:serviceaccount:legion:legion-infra-worker"
	legionOssvDaemon  = "system:serviceaccount:legion:legion-ossv-daemon"
	legionInfraDaemon = "system:serviceaccount:legion:legion-infra-daemon"
	legionProof       = "system:serviceaccount:legion:legion-stage4a-proof"
	legionDaemon      = "legion-daemon"
)

// cappedAppSecret is a github-app secret of a placeholder App under the example account acme-org,
// capped to its repository proofs and checks: write, asked for by OSSV's legion-daemon pods alone.
func cappedAppSecret() secrets.LocalSecret {
	s := policytest.TypeSecret("CAPPED_APP", legionDaemon, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	s.Tags[policy.TagGitHubOwners] = "acme-org"
	s.Tags[policy.TagGitHubRepositories] = "proofs"
	s.Tags[policy.TagGitHubPermissions] = "checks:write"
	s.Tags[policy.TagRequesters] = legionDaemon + "/OSSV"
	return s
}

// projectFixture is a Machine over CAPPED_APP with legion/'s accounts as the broker reads them
// from BROKER_SERVICES, BROKER_SERVICE_LOGINS and BROKER_EXCHANGE_ACCOUNTS, and a github-app minter
// that records every request it is given.
func projectFixture(t *testing.T) (*Machine, *[]secrets.MintRequest) {
	t.Helper()
	st := storetest.Open(t)
	local := secrets.NewLocal(cappedAppSecret())
	projects := map[string]string{
		legionOssvDaemon: "OSSV", legionInfraDaemon: "INFRA",
		legionOssvWorker: "OSSV", legionInfraWorker: "INFRA",
	}
	var minted []secrets.MintRequest
	m := &Machine{
		Store: st, Policy: policytest.Current(t, local, legionDaemon), Secrets: policytest.Store(local),
		PendingTTL: 12 * time.Hour, Audience: testAudience, Skew: time.Minute, Replay: replayer(st),
		ServicesByAccount: map[string]string{legionWorker: legionDaemon, legionOssvWorker: legionDaemon, legionInfraWorker: legionDaemon},
		ServiceLogins:     map[string]string{legionOssvDaemon: legionDaemon, legionInfraDaemon: legionDaemon, legionProof: legionDaemon},
		ProjectsForAccount: func(account string) ([]string, bool) {
			project, ok := projects[account]
			if !ok {
				return nil, false
			}
			return []string{project}, true
		},
		Minters: map[string]Minter{policy.TypeGitHubApp: func(_ context.Context, req secrets.MintRequest) (secrets.Minted, error) {
			minted = append(minted, req)
			return secrets.Minted{Value: "capped-token"}, nil
		}},
	}
	m.Chain = NewChainVerifier(st, testAudience, time.Minute, m.Airlock)
	return m, &minted
}

// podLaunchedBy enrols a pod running as account under a token login of launcher's for the
// legion-daemon service, the shape a Legion daemon's own login and its enrolment of a worker pod
// write: the credential names the service and its subject is the daemon's account.
func podLaunchedBy(t *testing.T, st *store.Store, launcher, account string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	credential := uuid.New()
	// One live credential per subject (launcher_credentials_live_subject): a daemon's later pods
	// enrol under the login it already holds.
	if err := st.Pool.QueryRow(ctx, `insert into launcher_credentials (id, service, host, key_thumbprint, public_jwk, subject, expires_at)
		values ($1,$2,'cluster',$3,'{}'::jsonb,$4, now() + interval '1 day')
		on conflict (subject) where subject is not null and revoked_at is null do update set expires_at = excluded.expires_at
		returning id`, credential, legionDaemon, uuid.NewString(), launcher).Scan(&credential); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	id := uuid.NewString()
	if _, err := st.Pool.Exec(ctx, `insert into enrollments (id, kind, runtime_id, thumbprint, subject, launcher_credential_id, lease_expires_at)
		values ($1,'pod',$2,$3,$4,$5, now() + interval '1 hour')`, id, "pod-"+uuid.NewString(), thumbprint, account, credential); err != nil {
		t.Fatalf("insert enrollments: %v", err)
	}
	return id, key
}

// TestAProjectsPodsAloneGetTheCappedToken: CAPPED_APP goes at once to a pod of legion-daemon
// whose project is OSSV — a legion-worker pod OSSV's daemon launched, or a pod running as OSSV's own
// account — and its release with no narrowing hands the minter the secret's cap, one repository
// and checks: write, so the token GitHub mints is capped whatever the pod asked. Every other pod is
// denied requester-not-listed with no grant and no mint: a legion-worker pod INFRA's daemon
// launched, an INFRA-account pod OSSV's daemon launched (the two accounts name two projects, so the
// pod has none), and a pod whose daemon's account BROKER_SERVICE_LOGINS no longer names.
func TestAProjectsPodsAloneGetTheCappedToken(t *testing.T) {
	ctx := context.Background()
	m, minted := projectFixture(t)
	wantCap := secrets.GitHubCap{Repositories: []string{"proofs"}, Permissions: map[string]string{"checks": "write"}}

	for _, pod := range []struct{ name, launcher, account string }{
		{"a legion-worker pod OSSV's daemon launched", legionOssvDaemon, legionWorker},
		{"an OSSV-account pod OSSV's daemon launched", legionOssvDaemon, legionOssvWorker},
	} {
		t.Run(pod.name, func(t *testing.T) {
			*minted = nil
			id, key := podLaunchedBy(t, m.Store, pod.launcher, pod.account)
			req, err := m.Create(ctx, id, signRequestFor(t, m, key, anHour, "CAPPED_APP"), "", nil)
			if err != nil || req.State != "granted" || req.GrantID == nil {
				t.Fatalf("Create = %+v, %v; want granted at once", req, err)
			}
			release, err := m.Values(ctx, *req.GrantID, id, nil, ReleaseCall{})
			if err != nil || release.Values["CAPPED_APP"] != "capped-token" {
				t.Fatalf("Values = %v, %v; want the minted token", release.Values, err)
			}
			if len(*minted) != 1 || (*minted)[0].GitHubNarrowing != nil || !reflect.DeepEqual((*minted)[0].GitHubCap, wantCap) {
				t.Fatalf("the minter was given %+v; want one mint carrying the cap %+v and no narrowing", *minted, wantCap)
			}
		})
	}

	for _, pod := range []struct{ name, launcher, account string }{
		{"a legion-worker pod INFRA's daemon launched", legionInfraDaemon, legionWorker},
		{"an INFRA-account pod OSSV's daemon launched", legionOssvDaemon, legionInfraWorker},
		{"a pod whose daemon's login is not configured", "system:serviceaccount:legion:legion-bet-daemon", legionWorker},
		// A login of the service that maps to no project (a live proof's) cannot hand a pod the
		// project its own account maps to: the launcher's project is what names the pod's.
		{"an OSSV-account pod a project-less login launched", legionProof, legionOssvWorker},
	} {
		t.Run(pod.name, func(t *testing.T) {
			*minted = nil
			id, key := podLaunchedBy(t, m.Store, pod.launcher, pod.account)
			req, err := m.Create(ctx, id, signRequestFor(t, m, key, anHour, "CAPPED_APP"), "", nil)
			const want = "CAPPED_APP: " + policy.ReasonRequesterNotListed
			if err != nil || req.State != "denied" || req.GrantID != nil || req.Detail == nil || *req.Detail != want {
				t.Fatalf("Create = %+v, %v; want denied with %q and no grant", req, err, want)
			}
			if len(*minted) != 0 {
				t.Fatalf("a denied pod reached the minter %d times", len(*minted))
			}
		})
	}
}

// TestAGrantEndsWhenThePodsProjectIsNoLongerNamed: a live grant of CAPPED_APP stops releasing
// once the deployment stops naming the pod's project, at its next release, as a service mapping
// dropped from BROKER_SERVICES does: the project is read on every release, not frozen at the grant.
func TestAGrantEndsWhenThePodsProjectIsNoLongerNamed(t *testing.T) {
	ctx := context.Background()
	m, minted := projectFixture(t)
	id, key := podLaunchedBy(t, m.Store, legionOssvDaemon, legionWorker)
	req, err := m.Create(ctx, id, signRequestFor(t, m, key, anHour, "CAPPED_APP"), "", nil)
	if err != nil || req.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want a grant", req, err)
	}
	if _, err := m.Values(ctx, *req.GrantID, id, nil, ReleaseCall{}); err != nil {
		t.Fatalf("Values before the change: %v", err)
	}
	m.ServiceLogins = map[string]string{}
	*minted = nil
	if release, err := m.Values(ctx, *req.GrantID, id, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || release.Values != nil {
		t.Fatalf("Values after the daemon's login was dropped = %v, %v; want ErrGrantNotLive and nothing", release.Values, err)
	}
	if len(*minted) != 0 {
		t.Fatalf("the minter ran %d times for a grant whose pod lost its project", len(*minted))
	}
}

// TestANarrowingPastTheCapIsRefusedBeforeAnyMint: a pod of the project that asks for more than the
// cap holds — a repository outside github-repositories, or read-only, which is not the cap's
// permission — is refused as its own narrowing before any minter runs, naming the secret and the
// cap. A narrowing within the cap reaches the minter beside the cap.
func TestANarrowingPastTheCapIsRefusedBeforeAnyMint(t *testing.T) {
	ctx := context.Background()
	m, minted := projectFixture(t)
	id, key := podLaunchedBy(t, m.Store, legionOssvDaemon, legionWorker)
	req, err := m.Create(ctx, id, signRequestFor(t, m, key, anHour, "CAPPED_APP"), "", nil)
	if err != nil || req.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want a grant", req, err)
	}
	for _, narrowing := range []*secrets.GitHubNarrowing{
		{Owner: "acme-org", Repositories: []string{"widgets"}},
		{Owner: "acme-org", ReadOnly: true},
	} {
		release, err := m.Values(ctx, *req.GrantID, id, narrowing, ReleaseCall{})
		if !errors.Is(err, secrets.ErrMintNarrowingRefused) || !strings.Contains(err.Error(), "CAPPED_APP") || release.Values != nil {
			t.Fatalf("Values(%+v) = %v, %v; want ErrMintNarrowingRefused naming CAPPED_APP, nothing released", narrowing, release.Values, err)
		}
	}
	if len(*minted) != 0 {
		t.Fatalf("a narrowing past the cap reached the minter %d times", len(*minted))
	}
	within := &secrets.GitHubNarrowing{Owner: "acme-org", Repositories: []string{"proofs"}}
	if _, err := m.Values(ctx, *req.GrantID, id, within, ReleaseCall{}); err != nil {
		t.Fatalf("Values within the cap: %v", err)
	}
	if len(*minted) != 1 || !reflect.DeepEqual((*minted)[0].GitHubNarrowing, within) || len((*minted)[0].GitHubCap.Permissions) == 0 {
		t.Fatalf("the minter was given %+v; want the narrowing beside the cap", *minted)
	}
}
