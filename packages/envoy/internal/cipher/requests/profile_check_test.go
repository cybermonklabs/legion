// packages/envoy/internal/cipher/requests/profile_check_test.go
// The check mint Machine.Create makes of each GitHub profile a request names before it writes a
// row or asks anybody to approve the request (profile_check.go), driven against the GitHub
// stand-in the airlock's own checks are driven against and through the broker's real github-app
// minter: each refusal read here is one GitHub itself makes, or the minter's own cut of the
// profile to what the installation holds, and each is read where the policy would have sent the
// request to a person.
//
// These rows are an external test package, unlike the rest of this directory's: the stand-in
// lives in proxyrulestest, which imports requests, so a test inside the package could not reach it.
// Everything below therefore drives the broker through its exported surface alone, which is the
// surface the broker's own callers have. The fixture is this file's and profile_check_trouble_test.go's.
package requests_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
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

// The accounts the fixture's App can be installed on. Both are in the github-owners tag every
// policytest type secret carries (policytest.FixtureGitHubOwners), so a profile of either is one
// the policy serves and GitHub alone decides. standInOwner is the account every fixture below
// installs the App on. otherOwner is the second account that tag lists, which each test installs
// or leaves alone as it needs.
const (
	standInOwner      = "ada"
	otherOwner        = "acme"
	standInRepository = "proofs"
)

// gitHubStand is the GitHub the fixture's github-app minter reaches: proxyrulestest's own stand-in
// on a plain service of its own, plus a hook over its token route for the trouble no stand-in
// answer holds — an answer that is no verdict on a profile's scope (a 500, a rate limit) and one
// that comes after the check's own bound. The hook is held under the same lock as the count,
// since the minter calls the route from the goroutine under test while the test installs it.
type gitHubStand struct {
	*proxyrulestest.GitHubStand
	service *proxyrulestest.Service

	mu    sync.Mutex
	asked int
	hook  func(w http.ResponseWriter, nth int) bool
}

// newGitHubStand starts one, answering ordinarily for the installations it is given, and closes
// it when t ends.
func newGitHubStand(t *testing.T, installations map[string]int64) *gitHubStand {
	t.Helper()
	service, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatalf("start the GitHub stand-in: %v", err)
	}
	t.Cleanup(service.Close)
	stand := &gitHubStand{
		GitHubStand: proxyrulestest.NewGitHubStand(proxyrulestest.GitHubOptions{Installations: installations}),
		service:     service,
	}
	service.AnswerWith(stand.answer)
	return stand
}

// answer is the stand-in's own answer, with this test's hook given each token request first: a
// hook that answered one is the whole answer, and every other request reaches the stand-in.
func (g *gitHubStand) answer(w http.ResponseWriter, r *http.Request, seen proxyrulestest.Request) bool {
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
func (g *gitHubStand) tokenRequests() []proxyrulestest.Request {
	var asked []proxyrulestest.Request
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
func isTokenRequest(seen proxyrulestest.Request) bool {
	return seen.Method == http.MethodPost && strings.HasSuffix(seen.Path, "/access_tokens")
}

// profileFixture is a broker on a schema of its own, with one live box session of checkOperator
// and the stand-in behind a real github-app minter. Its secrets are four github-app secrets of
// one App — APP_KEY and OTHER_APP_KEY, human tier, which the policy sends to their owner and whose
// profiles are therefore checked; AUTO_APP_KEY, agent tier, which the policy grants at once; and
// UNLISTED_APP_KEY, which a requesters tag naming somebody else turns into a denial — beside
// PLAIN_KEY, a stored secret of the same owner and tier, which is the request that names no GitHub
// profile at all. Each App is served through a profile of its own name: the github block a test
// gives it (profileFixtureOptions.github), else policytest.AppProfile's.
type profileFixture struct {
	*requests.Machine
	enrollmentID string
	key          *ecdsa.PrivateKey
	stand        *gitHubStand
}

// appNames are the github-app secrets of every profileFixture.
var appNames = []string{"APP_KEY", "OTHER_APP_KEY", "AUTO_APP_KEY", "UNLISTED_APP_KEY"}

// newProfileFixture builds one against a stand-in installed on installations, serving each App by
// a profile of its own name whose github block is github's entry for it, or policytest.AppProfile's.
func newProfileFixture(t *testing.T, installations map[string]int64, github map[string]string) *profileFixture {
	t.Helper()
	st := storetest.Open(t)
	value := standInAppValue(t)
	unlisted := policytest.TypeSecret("UNLISTED_APP_KEY", checkOperator, policy.TierAgent, policy.TypeGitHubApp, value)
	unlisted.Tags[policy.TagRequesters] = checkOutsider
	local := awstest.NewLocal(
		policytest.TypeSecret("APP_KEY", checkOperator, policy.TierHuman, policy.TypeGitHubApp, value),
		policytest.TypeSecret("OTHER_APP_KEY", checkOperator, policy.TierHuman, policy.TypeGitHubApp, value),
		policytest.TypeSecret("AUTO_APP_KEY", checkOperator, policy.TierAgent, policy.TypeGitHubApp, value),
		unlisted,
		policytest.Secret("PLAIN_KEY", checkOperator, policy.TierHuman, "plain-v1"),
	)
	profiles := make([]string, len(appNames))
	for i, name := range appNames {
		profiles[i] = policytest.AppProfile(name)
		if block, set := github[name]; set {
			profiles[i] = `{"name": "` + name + `", "github": ` + block + `}`
		}
	}
	enrollmentID, key := newEnrollment(t, st)
	stand := newGitHubStand(t, installations)
	machine := &requests.Machine{
		Store:      st,
		Policy:     policytest.ProfiledCurrent(t, local, policytest.ProfilesFile(profiles...)),
		Secrets:    policytest.Store(local),
		PendingTTL: 12 * time.Hour,
		Audience:   checkAudience,
		Skew:       time.Minute,
		Replay:     replayer(st),
		Minters:    map[string]requests.Minter{policy.TypeGitHubApp: secrets.GitHubApp{Base: stand.service.URL()}.Mint},
	}
	machine.Chain = requests.NewChainVerifier(st, checkAudience, time.Minute, machine.Policy)
	return &profileFixture{Machine: machine, enrollmentID: enrollmentID, key: key, stand: stand}
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

// create asks for names as a session does: a request object signed with this session's own key,
// naming no lifetime of its own.
func (f *profileFixture) create(t *testing.T, names ...string) (requests.Request, error) {
	t.Helper()
	details := make([]record.AuthorizationDetail, len(names))
	for i, name := range names {
		details[i] = record.AuthorizationDetail{Type: record.KindAgentSecret, Identifier: name, Actions: []string{"inject"}}
	}
	return f.createWith(t, details)
}

// createTargeted is create for the one name, narrowed to the target locations name (C-4's
// https://github.com/<account>[/<repository>] entries).
func (f *profileFixture) createTargeted(t *testing.T, name string, locations ...string) (requests.Request, error) {
	t.Helper()
	return f.createWith(t, []record.AuthorizationDetail{{Type: record.KindAgentSecret, Identifier: name, Actions: []string{"inject"}, Locations: locations}})
}

func (f *profileFixture) createWith(t *testing.T, details []record.AuthorizationDetail) (requests.Request, error) {
	t.Helper()
	compact, err := record.Sign(f.key, checkAudience, details, "read the App key", "", nil, time.Now())
	if err != nil {
		t.Fatalf("sign the request object: %v", err)
	}
	return f.Create(t.Context(), f.enrollmentID, compact, "")
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

// everyMintGivenBack checks that the stand-in minted at least one token for the check and that
// the check gave back each one it minted, so the stand-in holds no token of the check's live.
func everyMintGivenBack(t *testing.T, stand *gitHubStand) {
	t.Helper()
	minted := 0
	for _, seen := range stand.service.Requests() {
		if isTokenRequest(seen) && !strings.Contains(seen.Body, `"missing"`) {
			minted++
		}
	}
	if revoked := stand.revokedTokens(); minted == 0 || len(revoked) != minted {
		t.Fatalf("the stand-in minted %d tokens for the check and was given back %d; want the check's mint and a revoke of each", minted, len(revoked))
	}
}

// TestAPendingRequestGetsTheCheckMintBeforeAPersonIsAsked: a human-tier profile whose signed
// target names a repository the profile allows and the installation does not reach is refused by
// Create itself — before any request row is written, before any credential-request record opens,
// and so before anybody is asked to decide it — as the scope GitHub refused
// (secrets.ErrMintNarrowingRefused, which the API answers 422 PROFILE_MINT_REFUSED), naming that
// repository. The check mints the profile's token for the scope in effect, the target's
// repositories, and then each repository alone to find which one GitHub refused; each token
// GitHub minted for it is revoked at once. The profile itself names no repository, so every
// repository of its account is one it allows, and a check that minted the profile's own scope
// instead of the target's would be served and send the request to its approver.
func TestAPendingRequestGetsTheCheckMintBeforeAPersonIsAsked(t *testing.T) {
	f := newProfileFixture(t, map[string]int64{standInOwner: 7}, map[string]string{
		"APP_KEY": `{"accounts": ["ada"], "permissions": {"contents": "read", "metadata": "read"}}`,
	})
	f.stand.Answers(proxyrulestest.GitHubAnswer{Mode: proxyrulestest.GitHubTokensRefusedRepository, Repository: "missing"})

	request, err := f.createTargeted(t, "APP_KEY", "https://github.com/ada/lean", "https://github.com/ada/missing")
	if !errors.Is(err, secrets.ErrMintNarrowingRefused) || request.ID != "" || request.RecordID != nil {
		t.Fatalf("Create(APP_KEY) targeting ada/lean and ada/missing = %+v, %v; want ErrMintNarrowingRefused and no request", request, err)
	}
	for _, word := range []string{"APP_KEY", "missing", "does not reach"} {
		if !strings.Contains(err.Error(), word) {
			t.Errorf("the refusal says %q; want it to name %q", err, word)
		}
	}
	if rows, grants, records := heldRows(t, f.Machine); rows != 0 || grants != 0 || records != 0 {
		t.Fatalf("the refused request left %d request rows, %d grants and %d records; want none of any", rows, grants, records)
	}
	asked := f.stand.tokenRequests()
	if len(asked) == 0 || !strings.Contains(asked[0].Body, `"repositories":["lean","missing"]`) {
		t.Fatalf("the check's token requests were %+v; want the first to ask for the target's repositories, lean and missing", asked)
	}
	everyMintGivenBack(t, f.stand)
}

// TestAPendingRequestWithAnAccountTargetIsCheckedToo: the check mint runs for every pending
// request that names a GitHub profile, whether or not its scope names repositories, for the
// account in effect: the profile's single account, or the one account of a multi-account profile
// that the request's target names. An account the App is installed on is minted, revoked at once
// and the request goes on to its approver; an account the App has no installation on is refused
// before anybody is asked (secrets.ErrMintNoInstallation, naming the account). A target naming
// one account of two that are both installed is minted on that account's installation alone.
func TestAPendingRequestWithAnAccountTargetIsCheckedToo(t *testing.T) {
	for _, tc := range []struct {
		name string
		// installations are where the App is installed, accounts the profile's, as the
		// profiles file writes them, and target the request's locations, none for a request that
		// names no target. account is the account in effect.
		installations map[string]int64
		accounts      string
		target        []string
		account       string
		// want is the refusal Create answers, nil where it writes the request pending; held is
		// how many request rows and credential-request records the request leaves behind.
		want error
		held int
	}{
		{name: "an account the App is installed on", installations: map[string]int64{standInOwner: 7}, accounts: `["ada"]`, account: standInOwner, held: 1},
		{name: "an account the App has no installation on", installations: map[string]int64{standInOwner: 7}, accounts: `["acme"]`, account: otherOwner, want: secrets.ErrMintNoInstallation},
		{name: "a target naming one installed account of two", installations: map[string]int64{standInOwner: 7, otherOwner: 8}, accounts: `["acme", "ada"]`, target: []string{"https://github.com/ada"}, account: standInOwner, held: 1},
		{name: "a target naming the account of two the App has no installation on", installations: map[string]int64{standInOwner: 7}, accounts: `["acme", "ada"]`, target: []string{"https://github.com/acme"}, account: otherOwner, want: secrets.ErrMintNoInstallation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := policytest.CaptureLog(t)
			f := newProfileFixture(t, tc.installations, map[string]string{
				"APP_KEY": `{"accounts": ` + tc.accounts + `, "permissions": {"contents": "read", "metadata": "read"}}`,
			})

			request, err := f.createTargeted(t, "APP_KEY", tc.target...)
			switch {
			case tc.want == nil && (err != nil || request.State != "pending" || request.RecordID == nil):
				t.Fatalf("Create(APP_KEY) of %s = %+v, %v; want it pending on its approver", tc.account, request, err)
			case tc.want != nil && (!errors.Is(err, tc.want) || request.ID != "" || !strings.Contains(err.Error(), "has no installation on "+tc.account)):
				t.Fatalf("Create(APP_KEY) of %s = %+v, %v; want %v naming the account, and no request", tc.account, request, err, tc.want)
			}
			if rows, grants, records := heldRows(t, f.Machine); rows != tc.held || grants != 0 || records != tc.held {
				t.Fatalf("the request left %d request rows, %d grants and %d records; want %d, none and %d", rows, grants, records, tc.held, tc.held)
			}
			listed := 0
			for _, seen := range f.stand.service.Requests() {
				if seen.Method == http.MethodGet && seen.Path == "/app/installations" {
					listed++
				}
			}
			if listed != 1 {
				t.Fatalf("the check listed the App's installations %d times; want once: the check ran for a profile naming no repository", listed)
			}
			if tc.want == nil {
				everyMintGivenBack(t, f.stand)
				onAccount := fmt.Sprintf("/app/installations/%d/access_tokens", tc.installations[tc.account])
				for _, asked := range f.stand.tokenRequests() {
					if asked.Path != onAccount {
						t.Fatalf("the check asked for a token at %s; want only %s, the installation on %s, the account in effect", asked.Path, onAccount, tc.account)
					}
				}
			} else if asked := f.stand.tokenRequests(); len(asked) != 0 {
				t.Fatalf("the check asked GitHub for %d tokens for an account with no installation; want none", len(asked))
			}
			if strings.Contains(logged.String(), requests.NarrowingCheckSkippedMessage) {
				t.Fatalf("the check logged %q; want its verdict reached: %s", requests.NarrowingCheckSkippedMessage, logged.String())
			}
		})
	}
}

// TestAPendingRequestWithAnEmptyCutIsRefusedBeforeAPersonIsAsked: a profile whose permissions the
// installation holds none of but metadata is refused by Create before anybody is asked to approve
// it (secrets.ErrMintEmptyCut, inside secrets.ErrMintNarrowingRefused), naming the profile and the
// permission it lacks, and GitHub is asked for no token. Every GitHub profile names metadata at
// read, and every installation GitHub lists holds it, so the installation here holds that alone,
// the shape GitHub lists: a cut that kept it would mint a token that does nothing the profile is
// for, and the person asked would approve a grant of no use.
func TestAPendingRequestWithAnEmptyCutIsRefusedBeforeAPersonIsAsked(t *testing.T) {
	f := newProfileFixture(t, map[string]int64{standInOwner: 7}, map[string]string{
		"APP_KEY": `{"accounts": ["ada"], "repositories": ["ada/missing", "ada/proofs"], "permissions": {"contents": "read", "metadata": "read"}}`,
	})
	f.stand.GitHubStand = proxyrulestest.NewGitHubStand(proxyrulestest.GitHubOptions{
		Installations: map[string]int64{standInOwner: 7},
		Permissions:   map[string]map[string]string{standInOwner: {"metadata": "read"}},
	})

	request, err := f.create(t, "APP_KEY")
	if !errors.Is(err, secrets.ErrMintEmptyCut) || !errors.Is(err, secrets.ErrMintNarrowingRefused) || request.ID != "" || request.RecordID != nil {
		t.Fatalf("Create(APP_KEY) of a profile the installation holds nothing of but metadata = %+v, %v; want ErrMintEmptyCut and no request", request, err)
	}
	for _, word := range []string{"APP_KEY", "holds none of contents: read"} {
		if !strings.Contains(err.Error(), word) {
			t.Errorf("the refusal says %q; want it to name %q", err, word)
		}
	}
	if strings.Contains(err.Error(), "missing") {
		t.Errorf("the refusal says %q; an empty cut is no repository's fault", err)
	}
	if rows, grants, records := heldRows(t, f.Machine); rows != 0 || grants != 0 || records != 0 {
		t.Fatalf("the refused request left %d request rows, %d grants and %d records; want none of any", rows, grants, records)
	}
	if asked := f.stand.tokenRequests(); len(asked) != 0 {
		t.Fatalf("GitHub was asked for %d tokens for an empty cut; want none", len(asked))
	}
}

// TestAPendingRequestForABareTokenOfAnAccountWithTwoInstallationsIsRefusedBeforeAPersonIsAsked: a
// value-token profile releases one installation's bare token, and the App is installed on Ada and
// on ada, logins GitHub compares as one account. Create refuses it before anybody is asked
// (secrets.ErrMintSeveralInstallations, inside secrets.ErrMintNarrowingRefused), naming the
// installations and no repository, since no repository of the profile is at fault, and GitHub
// is asked for no token.
func TestAPendingRequestForABareTokenOfAnAccountWithTwoInstallationsIsRefusedBeforeAPersonIsAsked(t *testing.T) {
	f := newProfileFixture(t, map[string]int64{standInOwner: 7, "Ada": 8}, map[string]string{
		"APP_KEY": `{"accounts": ["ada"], "repositories": ["ada/lean", "ada/proofs"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"}`,
	})

	request, err := f.create(t, "APP_KEY")
	if !errors.Is(err, secrets.ErrMintSeveralInstallations) || !errors.Is(err, secrets.ErrMintNarrowingRefused) || request.ID != "" || request.RecordID != nil {
		t.Fatalf("Create(APP_KEY) of a bare token of an account with two installations = %+v, %v; want ErrMintSeveralInstallations and no request", request, err)
	}
	for _, word := range []string{"APP_KEY", "installation 7", "installation 8"} {
		if !strings.Contains(err.Error(), word) {
			t.Errorf("the refusal says %q; want it to name %q", err, word)
		}
	}
	if strings.Contains(err.Error(), "repository") {
		t.Errorf("the refusal says %q; two installations are no repository's fault", err)
	}
	if rows, grants, records := heldRows(t, f.Machine); rows != 0 || grants != 0 || records != 0 {
		t.Fatalf("the refused request left %d request rows, %d grants and %d records; want none of any", rows, grants, records)
	}
	if asked := f.stand.tokenRequests(); len(asked) != 0 {
		t.Fatalf("GitHub was asked for %d tokens for a bare token of two installations; want none", len(asked))
	}
}

// TestACheckMintGitHubServesLetsTheRequestThrough is the control of the refusals above: a profile
// the stand-in mints writes the pending request and its record exactly as a request naming no
// GitHub profile does, and the check that said so released no token — the one it minted it gave
// straight back, so that string reaches nothing. A request that names no GitHub profile is not
// checked at all.
func TestACheckMintGitHubServesLetsTheRequestThrough(t *testing.T) {
	f := newProfileFixture(t, map[string]int64{standInOwner: 7}, map[string]string{
		"APP_KEY": `{"accounts": ["ada"], "repositories": ["ada/proofs"], "permissions": {"contents": "read", "metadata": "read"}}`,
	})

	request, err := f.create(t, "APP_KEY")
	if err != nil || request.State != "pending" || request.RecordID == nil {
		t.Fatalf("Create(APP_KEY) of a profile GitHub serves = %+v, %v; want it pending on its approver", request, err)
	}
	if rows, grants, records := heldRows(t, f.Machine); rows != 1 || grants != 0 || records != 1 {
		t.Fatalf("the served profile left %d request rows, %d grants and %d records; want the one pending request and its one record", rows, grants, records)
	}
	asked, revoked := f.stand.tokenRequests(), f.stand.revokedTokens()
	if len(asked) != 1 || len(revoked) != 1 {
		t.Fatalf("the check asked GitHub for %d tokens and gave %d back; want the one verdict mint and its revoke", len(asked), len(revoked))
	}
	if status, body := readStandInRepository(t, f.stand, revoked[0], standInOwner, standInRepository); status != http.StatusUnauthorized {
		t.Fatalf("the token the check minted reads %s/%s with HTTP %d (%s); want 401: a check releases no token it minted",
			standInOwner, standInRepository, status, body)
	}

	plain, err := f.create(t, "PLAIN_KEY")
	if err != nil || plain.State != "pending" {
		t.Fatalf("Create(PLAIN_KEY) = %+v, %v; want it pending on its approver", plain, err)
	}
	if again := f.stand.tokenRequests(); len(again) != len(asked) {
		t.Fatalf("a request naming no GitHub profile asked GitHub for %d tokens; want none", len(again)-len(asked))
	}
}

// TestAProfileOfRepositoriesOfTwoAccountsIsCheckedAndReleasedWithNoTarget: a profile naming
// repositories of two accounts, asked for with no target, is minted for each account with that
// account's repositories alone, by name: by the check mint before the approver is asked, and by
// the release after the approval, which holds both accounts' tokens. Neither refuses it for want
// of a target, which only a value-token profile of more than one account needs.
func TestAProfileOfRepositoriesOfTwoAccountsIsCheckedAndReleasedWithNoTarget(t *testing.T) {
	installations := map[string]int64{standInOwner: 7, otherOwner: 8}
	f := newProfileFixture(t, installations, map[string]string{
		"APP_KEY": `{"accounts": ["acme", "ada"], "repositories": ["acme/site", "ada/proofs"], "permissions": {"contents": "read", "metadata": "read"}}`,
	})

	pending, err := f.create(t, "APP_KEY")
	if err != nil || pending.State != "pending" || pending.RecordID == nil {
		t.Fatalf("Create(APP_KEY) of repositories of two accounts = %+v, %v; want it pending on its approver", pending, err)
	}
	decision, err := f.ApplyDecision(t.Context(), *pending.RecordID, true, checkOperator)
	if err != nil || decision.GrantID == "" {
		t.Fatalf("the approver's decision = %+v, %v; want the grant their approval makes", decision, err)
	}
	release, err := f.Values(t.Context(), decision.GrantID, f.enrollmentID, requests.ReleaseCall{})
	if err != nil {
		t.Fatalf("Values of repositories of two accounts: %v", err)
	}
	var held struct {
		Installations map[string]struct {
			Token string `json:"token"`
		} `json:"installations"`
	}
	if err := json.Unmarshal([]byte(release.Values["APP_KEY"]), &held); err != nil || held.Installations[standInOwner].Token == "" || held.Installations[otherOwner].Token == "" || len(held.Installations) != 2 {
		t.Fatalf("the release holds %q (%v); want a token of each account", release.Values["APP_KEY"], err)
	}
	want := map[string]string{
		fmt.Sprintf("/app/installations/%d/access_tokens", installations[standInOwner]): `"repositories":["proofs"]`,
		fmt.Sprintf("/app/installations/%d/access_tokens", installations[otherOwner]):   `"repositories":["site"]`,
	}
	asked := f.stand.tokenRequests()
	if len(asked) != 4 {
		t.Fatalf("GitHub was asked for %d tokens; want one of each account for the check and one of each for the release", len(asked))
	}
	for _, seen := range asked {
		if repositories, ok := want[seen.Path]; !ok || !strings.Contains(seen.Body, repositories) {
			t.Errorf("the token request at %s sent %s; want it to name that account's repositories alone, %s", seen.Path, seen.Body, repositories)
		}
	}
}

// TestTheCheckMintAsksGitHubNothingWhereNobodyIsAsked: the check buys back an approval nobody
// should have spent, so it runs only where the policy would ask a person for one. A request the
// policy grants at once reaches GitHub for nothing; the same request again hands back that live
// grant and reaches it for nothing either; and a request the policy denies is answered with its
// denial, GitHub having been asked nothing about a profile no grant will ever release.
func TestTheCheckMintAsksGitHubNothingWhereNobodyIsAsked(t *testing.T) {
	f := newProfileFixture(t, map[string]int64{standInOwner: 7}, nil)

	granted, err := f.create(t, "AUTO_APP_KEY")
	if err != nil || granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("Create(AUTO_APP_KEY) = %+v, %v; want the grant the policy makes at once", granted, err)
	}
	again, err := f.create(t, "AUTO_APP_KEY")
	if err != nil || again.GrantID == nil || *again.GrantID != *granted.GrantID {
		t.Fatalf("the same request again = %+v, %v; want the live grant %s handed back", again, err, *granted.GrantID)
	}
	denied, err := f.create(t, "UNLISTED_APP_KEY")
	if err != nil || denied.State != "denied" {
		t.Fatalf("Create(UNLISTED_APP_KEY) = %+v, %v; want it denied, as its requesters tag denies it", denied, err)
	}
	if denied.Detail == nil || !strings.Contains(*denied.Detail, policy.ReasonRequesterNotListed) {
		t.Errorf("the denial says %v; want it to name the requesters rule that made it", denied.Detail)
	}
	if asked := f.stand.tokenRequests(); len(asked) != 0 {
		t.Fatalf("GitHub was asked for %d tokens over requests no person decides; want none", len(asked))
	}
}

// TestTheCheckMintAsksGitHubNothingForTheGrantAPersonApproved: the check runs only once Create has
// found no live grant to hand back (reuseLiveGrant), never before it looks. The policy puts a
// human-tier secret's request to a person every time, so here only the order keeps the check
// away: the first request is checked on its way to its approver, the approver grants it, and the
// same request again, for the same lifetime, gets that grant back with GitHub asked nothing — no
// token, and not even the App's installations.
func TestTheCheckMintAsksGitHubNothingForTheGrantAPersonApproved(t *testing.T) {
	f := newProfileFixture(t, map[string]int64{standInOwner: 7}, nil)

	pending, err := f.create(t, "APP_KEY")
	if err != nil || pending.State != "pending" || pending.RecordID == nil {
		t.Fatalf("Create(APP_KEY) = %+v, %v; want it pending on its approver", pending, err)
	}
	if asked := f.stand.tokenRequests(); len(asked) != 1 {
		t.Fatalf("the first request's check asked GitHub for %d tokens; want the one verdict mint, which shows the check runs for this profile", len(asked))
	}
	decision, err := f.ApplyDecision(t.Context(), *pending.RecordID, true, checkOperator)
	if err != nil || decision.GrantID == "" {
		t.Fatalf("the approver's decision = %+v, %v; want the grant their approval makes", decision, err)
	}

	mark := len(f.stand.service.Requests())
	again, err := f.create(t, "APP_KEY")
	if err != nil || again.ID != pending.ID || again.GrantID == nil || *again.GrantID != decision.GrantID {
		t.Fatalf("the same request again = %+v, %v; want request %s and its approved grant %s handed back", again, err, pending.ID, decision.GrantID)
	}
	if made := f.stand.service.Requests()[mark:]; len(made) != 0 {
		t.Fatalf("handing back the approved grant made %d calls to GitHub; want none: the check runs only after reuseLiveGrant found no live grant", len(made))
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
