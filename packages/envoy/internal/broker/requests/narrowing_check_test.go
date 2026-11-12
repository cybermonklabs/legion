// packages/envoy/internal/broker/requests/narrowing_check_test.go
// The verdict Machine.Create reaches on a GitHub App narrowing before it writes a row or asks
// anybody to approve the request (narrowing_check.go), driven against the GitHub stand-in the
// airlock's own checks are driven against and through the broker's real github-app minter: each
// refusal read here is one GitHub itself makes, and each is read where the policy would have
// sent the request to a person.
//
// These rows are an external test package, unlike the rest of this directory's: the stand-in
// lives in airlocktest, which imports requests, so a test inside the package could not reach it.
// Everything below therefore drives the broker through its exported surface alone, which is the
// surface the broker's own callers have. The fixture is this file's and the two beside it
// (narrowing_trouble_test.go, narrowing_unchanged_test.go).
package requests_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
)

const (
	// checkAudience is the broker's public URL every request object is signed for.
	checkAudience = "broker"
	// checkOperator is the person whose box the fixture's session runs on, and the owner and
	// approver of every secret below; checkOutsider is a second person, who owns none of them.
	checkOperator = "sami@example.com"
	checkOutsider = "alice@example.com"
)

// The accounts the fixture's App is installed on. Both are in the github-owners tag every
// policytest kind secret carries (policytest.FixtureGitHubOwners), so a narrowing to either is
// one the policy allows and GitHub alone decides: standInOwner is the account the stand-in holds
// an installation for and uninstalledOwner the one it does not, which is the App-has-no-
// installation case, no answer of the stand-in's own.
const (
	standInOwner      = "ada"
	uninstalledOwner  = "acme"
	standInRepository = "proofs"
)

// gitHubStand is the GitHub the fixture's github-app minter reaches: airlocktest's own stand-in
// on a plain service of its own, plus a hook over its token route for the trouble no stand-in
// answer holds — an answer that is no verdict on a narrowing (a 500, a rate limit) and one that
// comes after the check's own bound. The hook is held under the same lock as the count, since
// the minter calls the route from the goroutine under test while the test installs it.
type gitHubStand struct {
	*airlocktest.GitHubStand
	service *airlocktest.Service

	mu    sync.Mutex
	asked int
	hook  func(w http.ResponseWriter, nth int) bool
}

// newGitHubStand starts one, answering ordinarily for the installations it is given, and closes
// it when t ends.
func newGitHubStand(t *testing.T, installations map[string]int64) *gitHubStand {
	t.Helper()
	service, err := airlocktest.NewService(airlocktest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatalf("start the GitHub stand-in: %v", err)
	}
	t.Cleanup(service.Close)
	stand := &gitHubStand{
		GitHubStand: airlocktest.NewGitHubStand(airlocktest.GitHubOptions{Installations: installations}),
		service:     service,
	}
	service.AnswerWith(stand.answer)
	return stand
}

// answer is the stand-in's own answer, with this test's hook given each token request first: a
// hook that answered one is the whole answer, and every other request reaches the stand-in.
func (g *gitHubStand) answer(w http.ResponseWriter, r *http.Request, seen airlocktest.Request) bool {
	if isTokenRequest(seen) {
		g.mu.Lock()
		g.asked++
		nth, hook := g.asked, g.hook
		g.mu.Unlock()
		if hook != nil && hook(w, nth) {
			return true
		}
	}
	return g.GitHubStand.Answer(w, r, seen)
}

// answersTokensWith installs that hook: it is given each token request's number, counted from
// one, and reports whether it answered it.
func (g *gitHubStand) answersTokensWith(hook func(w http.ResponseWriter, nth int) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hook = hook
}

// tokenRequests is every token request the stand-in was sent, which is what a check that reached
// GitHub leaves behind and a check that never ran leaves none of.
func (g *gitHubStand) tokenRequests() []airlocktest.Request {
	var asked []airlocktest.Request
	for _, seen := range g.service.Requests() {
		if isTokenRequest(seen) {
			asked = append(asked, seen)
		}
	}
	return asked
}

// revokedTokens is every credential given back to the stand-in, in order: GitHub's revoke names
// the token itself in its Authorization header and nothing else, so this is exactly what a
// caller minted and did not keep.
func (g *gitHubStand) revokedTokens() []string {
	var given []string
	for _, seen := range g.service.Requests() {
		if seen.Method == http.MethodDelete && seen.Path == "/installation/token" {
			given = append(given, strings.TrimPrefix(seen.Header.Get("Authorization"), "Bearer "))
		}
	}
	return given
}

// isTokenRequest reports whether a recorded request is GitHub's access_tokens route, the one call
// that mints.
func isTokenRequest(seen airlocktest.Request) bool {
	return seen.Method == http.MethodPost && strings.HasSuffix(seen.Path, "/access_tokens")
}

// narrowingFixture is a broker on a schema of its own, with one live box session of checkOperator
// and the stand-in behind a real github-app minter. Its secrets are four github-app secrets of
// one App — APP_KEY and OTHER_APP_KEY, human tier, which the policy sends to their owner and
// whose narrowing is therefore checked; AUTO_APP_KEY, agent tier, which the policy grants at
// once; and UNLISTED_APP_KEY, which a requesters tag naming somebody else turns into a denial —
// beside PLAIN_KEY, a stored secret of the same owner and tier, which is the request that asks
// for no github-app secret at all.
type narrowingFixture struct {
	*requests.Machine
	enrollmentID string
	key          *ecdsa.PrivateKey
	stand        *gitHubStand
	// local is the Secrets Manager behind the policy and the broker's reads, kept so a row may
	// put a secret back or read one as its owner stored it.
	local *secrets.Local
}

// newNarrowingFixture builds one against a stand-in installed on installations.
func newNarrowingFixture(t *testing.T, installations map[string]int64) *narrowingFixture {
	t.Helper()
	st := storetest.Open(t)
	value := standInAppValue(t)
	unlisted := policytest.KindSecret("UNLISTED_APP_KEY", checkOperator, policy.TierAgent, policy.KindGitHubApp, value)
	unlisted.Tags[policy.TagRequesters] = checkOutsider
	local := secrets.NewLocal(
		policytest.KindSecret("APP_KEY", checkOperator, policy.TierHuman, policy.KindGitHubApp, value),
		policytest.KindSecret("OTHER_APP_KEY", checkOperator, policy.TierHuman, policy.KindGitHubApp, value),
		policytest.KindSecret("AUTO_APP_KEY", checkOperator, policy.TierAgent, policy.KindGitHubApp, value),
		unlisted,
		policytest.Secret("PLAIN_KEY", checkOperator, policy.TierHuman, "plain-v1"),
	)
	enrollmentID, key := newEnrollment(t, st)
	stand := newGitHubStand(t, installations)
	machine := &requests.Machine{
		Store:      st,
		Policy:     policytest.Current(t, local),
		Secrets:    policytest.Store(local),
		PendingTTL: 12 * time.Hour,
		Audience:   checkAudience,
		Skew:       time.Minute,
		Replay:     replayer(st),
		Minters:    map[string]requests.Minter{policy.KindGitHubApp: secrets.GitHubApp{Base: stand.service.URL()}.Mint},
	}
	machine.Chain = requests.NewChainVerifier(st, checkAudience, time.Minute, machine.Airlock)
	return &narrowingFixture{Machine: machine, enrollmentID: enrollmentID, key: key, stand: stand, local: local}
}

// standInAppValue is a github-app secret's stored value for the stand-in: the App id it answers
// for, and a key of this test's own, which the minter signs its App JWT with.
func standInAppValue(t *testing.T) string {
	t.Helper()
	_, pemText := githubapptest.Key(t)
	value, err := json.Marshal(map[string]string{"app_id": "123456", "private_key": pemText})
	if err != nil {
		t.Fatalf("encode the github-app secret: %v", err)
	}
	return string(value)
}

// newEnrollment inserts one live box session of checkOperator, under a launcher credential of
// their own, and answers its id and the key its request objects are signed with. The rows are
// written directly, as this package's own fixtures write them: enroll.Service is exercised by
// the enrollment routes' tests, and a request needs only a live row to be verified against.
func newEnrollment(t *testing.T, st *store.Store) (string, *ecdsa.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the enrollment key: %v", err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	credentialID := uuid.NewString()
	if _, err := st.Pool.Exec(ctx, `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ($1,$2,'devbox-test',$3,'{}'::jsonb, now() + interval '30 days')`, credentialID, checkOperator, uuid.NewString()); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	id := uuid.NewString()
	if _, err := st.Pool.Exec(ctx, `insert into enrollments (id, kind, runtime_id, operator, thumbprint, launcher_credential_id, lease_expires_at)
		values ($1,'session',$2,$3,$4,$5, now() + interval '1 hour')`, id, "box-"+t.Name(), checkOperator, thumbprint, credentialID); err != nil {
		t.Fatalf("insert enrollments: %v", err)
	}
	return id, key
}

// replayer is the Machine.Replay seam over the proof_jtis table: one request object is spent
// once, as the enrollment service spends it in production.
func replayer(st *store.Store) func(context.Context, string, time.Time) (bool, error) {
	return func(ctx context.Context, jti string, expires time.Time) (bool, error) {
		_, err := st.Pool.Exec(ctx, `insert into proof_jtis (jti, expires_at) values ($1,$2)`, jti, expires)
		if store.IsUniqueViolation(err) {
			return false, nil
		}
		return err == nil, err
	}
}

// create asks for names with narrowing, as a session does: a request object signed with this
// session's own key, naming no lifetime of its own.
func (f *narrowingFixture) create(t *testing.T, narrowing *secrets.GitHubNarrowing, names ...string) (requests.Request, error) {
	t.Helper()
	details := make([]record.AuthorizationDetail, len(names))
	for i, name := range names {
		details[i] = record.AuthorizationDetail{Type: record.KindAgentSecret, Identifier: name, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(f.key, checkAudience, details, "read the App key", "", nil, time.Now())
	if err != nil {
		t.Fatalf("sign the request object: %v", err)
	}
	return f.Create(t.Context(), f.enrollmentID, compact, "", narrowing)
}

// heldRows counts the request rows, grant rows and credential-request records the broker holds.
// storetest.Open gives each test a schema of its own, so these are this session's rows and
// nobody else's.
func heldRows(t *testing.T, machine *requests.Machine) (requestRows, grants, records int) {
	t.Helper()
	if err := machine.Store.Pool.QueryRow(context.Background(), `select (select count(*) from requests),
		(select count(*) from grants), (select count(*) from credential_requests)`).Scan(&requestRows, &grants, &records); err != nil {
		t.Fatalf("count what the broker holds: %v", err)
	}
	return requestRows, grants, records
}

// TestNarrowingBeforeApprovalCostsNobodyAnApproval: each narrowing the broker will not serve is
// refused by Create itself — before any request row is written, before any credential-request
// record opens, and so before anybody is asked to decide it — and each refusal says what its own
// cause was, so the caller fixes the narrowing rather than asks again. The three the policy
// decides (no github-app secret, an account the tag does not list, a name the airlock holds) are
// refused without asking GitHub anything; the three GitHub decides are refused on GitHub's own
// answer to a token request.
func TestNarrowingBeforeApprovalCostsNobodyAnApproval(t *testing.T) {
	for _, tc := range []struct {
		name string
		// answer is what the stand-in answers each token request with.
		answer airlocktest.GitHubAnswer
		// airlocked holds APP_KEY behind the airlock, as a rules file does.
		airlocked bool
		names     []string
		narrowing *secrets.GitHubNarrowing
		want      error
		// mentions and absent are what the refusal the caller reads must and must not say.
		mentions []string
		absent   []string
	}{
		{
			name:      "a repository the installation does not reach",
			answer:    airlocktest.GitHubAnswer{Mode: airlocktest.GitHubTokensRefusedRepository, Repository: "missing"},
			names:     []string{"APP_KEY"},
			narrowing: &secrets.GitHubNarrowing{Owner: standInOwner, Repositories: []string{"missing"}},
			want:      secrets.ErrMintNarrowingRefused,
			mentions:  []string{"the narrowing cannot be served", "APP_KEY", "missing"},
		},
		{
			name:      "the read access the installation does not hold",
			answer:    airlocktest.GitHubAnswer{Mode: airlocktest.GitHubTokensRefusedPermissions},
			names:     []string{"APP_KEY"},
			narrowing: &secrets.GitHubNarrowing{Owner: standInOwner, ReadOnly: true},
			want:      secrets.ErrMintNarrowingRefused,
			mentions:  []string{"the narrowing cannot be served", "APP_KEY", "read access"},
		},
		{
			name:      "an account the App has no installation on",
			names:     []string{"APP_KEY"},
			narrowing: &secrets.GitHubNarrowing{Owner: uninstalledOwner, ReadOnly: true},
			want:      secrets.ErrMintRefused,
			mentions:  []string{"has no installation on", uninstalledOwner, "APP_KEY"},
			// The App, not the narrowing, is what is at fault here, and a client reads the two
			// apart: this refusal must not read as the narrowing's own.
			absent: []string{"the narrowing cannot be served"},
		},
		{
			name:      "a request that asks for no github-app secret",
			names:     []string{"PLAIN_KEY"},
			narrowing: &secrets.GitHubNarrowing{Owner: standInOwner, ReadOnly: true},
			want:      requests.ErrNarrowingNotApplicable,
		},
		{
			name:      "an account the github-owners tag does not list",
			names:     []string{"APP_KEY"},
			narrowing: &secrets.GitHubNarrowing{Owner: "outsider", ReadOnly: true},
			want:      requests.ErrNarrowingOwnerNotListed,
			mentions:  []string{"APP_KEY", "outsider"},
		},
		{
			name:      "a requested name the airlock holds back",
			airlocked: true,
			names:     []string{"APP_KEY"},
			narrowing: &secrets.GitHubNarrowing{Owner: standInOwner, Repositories: []string{standInRepository}},
			want:      requests.ErrNarrowingBehindAirlock,
			mentions:  []string{"APP_KEY", "airlock"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNarrowingFixture(t, map[string]int64{standInOwner: 7})
			f.stand.Answers(tc.answer)
			if tc.airlocked {
				f.Airlock = airlock.Rules{Secrets: []airlock.SecretRule{{Name: "APP_KEY", Hosts: []string{"api.example.com"}}}}
			}

			request, err := f.create(t, tc.narrowing, tc.names...)
			if !errors.Is(err, tc.want) || request.ID != "" || request.RecordID != nil {
				t.Fatalf("Create%v narrowed to %+v = %+v, %v; want %v and no request", tc.names, tc.narrowing, request, err, tc.want)
			}
			for _, word := range tc.mentions {
				if !strings.Contains(err.Error(), word) {
					t.Errorf("the refusal says %q; want it to name %q", err, word)
				}
			}
			for _, word := range tc.absent {
				if strings.Contains(err.Error(), word) {
					t.Errorf("the refusal says %q; want it not to say %q", err, word)
				}
			}
			if rows, grants, records := heldRows(t, f.Machine); rows != 0 || grants != 0 || records != 0 {
				t.Fatalf("the refused request left %d request rows, %d grants and %d records; want none of any: a narrowing the broker will not serve costs nobody an approval",
					rows, grants, records)
			}
		})
	}
}

// TestNarrowingBeforeApprovalLetsAServableNarrowingThrough is the control of the refusals above:
// a narrowing the policy allows and the stand-in mints writes the pending request and its record
// exactly as a request with no narrowing does, and the check that said so released no token —
// the one it minted it gave straight back, so that string reaches nothing.
func TestNarrowingBeforeApprovalLetsAServableNarrowingThrough(t *testing.T) {
	f := newNarrowingFixture(t, map[string]int64{standInOwner: 7})

	request, err := f.create(t, &secrets.GitHubNarrowing{Owner: standInOwner, Repositories: []string{standInRepository}, ReadOnly: true}, "APP_KEY")
	if err != nil || request.State != "pending" || request.RecordID == nil {
		t.Fatalf("Create(APP_KEY) narrowed to a narrowing GitHub serves = %+v, %v; want it pending on its approver", request, err)
	}
	if rows, grants, records := heldRows(t, f.Machine); rows != 1 || grants != 0 || records != 1 {
		t.Fatalf("the served narrowing left %d request rows, %d grants and %d records; want the one pending request and its one record", rows, grants, records)
	}
	asked, revoked := f.stand.tokenRequests(), f.stand.revokedTokens()
	if len(asked) != 1 || len(revoked) != 1 {
		t.Fatalf("the check asked GitHub for %d tokens and gave %d back; want the one verdict mint and its revoke", len(asked), len(revoked))
	}
	if status, body := readStandInRepository(t, f.stand, revoked[0], standInOwner, standInRepository); status != http.StatusUnauthorized {
		t.Fatalf("the token the check minted reads %s/%s with HTTP %d (%s); want 401: a check releases no token it minted",
			standInOwner, standInRepository, status, body)
	}
}

// TestNarrowingBeforeApprovalAsksGitHubNothingWhereNobodyIsAsked: the check buys back an approval
// nobody should have spent, so it runs only where the policy would ask a person for one. A
// narrowed request the policy grants at once reaches GitHub for nothing; the same request again
// hands back that live grant and reaches it for nothing either; and a request the policy denies
// is answered with its denial, GitHub having been asked nothing about a narrowing no grant will
// ever apply.
func TestNarrowingBeforeApprovalAsksGitHubNothingWhereNobodyIsAsked(t *testing.T) {
	f := newNarrowingFixture(t, map[string]int64{standInOwner: 7})
	narrowing := &secrets.GitHubNarrowing{Owner: standInOwner, Repositories: []string{standInRepository}, ReadOnly: true}

	granted, err := f.create(t, narrowing, "AUTO_APP_KEY")
	if err != nil || granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("Create(AUTO_APP_KEY) narrowed = %+v, %v; want the grant the policy makes at once", granted, err)
	}
	again, err := f.create(t, narrowing, "AUTO_APP_KEY")
	if err != nil || again.GrantID == nil || *again.GrantID != *granted.GrantID {
		t.Fatalf("the same narrowed request again = %+v, %v; want the live grant %s handed back", again, err, *granted.GrantID)
	}
	denied, err := f.create(t, narrowing, "UNLISTED_APP_KEY")
	if err != nil || denied.State != "denied" {
		t.Fatalf("Create(UNLISTED_APP_KEY) narrowed = %+v, %v; want it denied, as its requesters tag denies it", denied, err)
	}
	if denied.Detail == nil || !strings.Contains(*denied.Detail, policy.ReasonRequesterNotListed) {
		t.Errorf("the denial says %v; want it to name the requesters rule that made it", denied.Detail)
	}
	if asked := f.stand.tokenRequests(); len(asked) != 0 {
		t.Fatalf("GitHub was asked for %d tokens over requests no person decides; want none", len(asked))
	}
}

// readStandInRepository reads owner/repository at the stand-in with credential, as a caller
// spends a released token: the status and what it answered.
func readStandInRepository(t *testing.T, stand *gitHubStand, credential, owner, repository string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, stand.service.URL()+"/repos/"+owner+"/"+repository, nil)
	if err != nil {
		t.Fatalf("build the repository read: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	answer, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("read %s/%s at the stand-in: %v", owner, repository, err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(io.LimitReader(answer.Body, 1<<10))
	if err != nil {
		t.Fatalf("read the stand-in's answer: %v", err)
	}
	return answer.StatusCode, string(body)
}
