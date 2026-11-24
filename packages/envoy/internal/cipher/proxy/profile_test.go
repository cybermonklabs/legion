// packages/envoy/internal/cipher/proxy/profile_test.go
// The proxy in front of the broker's own release of a GitHub profile: a real requests.Machine on
// a schema of its own, whose github-app minter mints against proxyrulestest's GitHub stand-in, read
// by the rig's proxy where every other row of this package reads fakeGrants. The stand-in answers
// twice: on a plain service of its own, which the minter reaches, and as the service behind the
// bound host, which the command's calls reach through the proxy. Both are one GitHubStand, so a
// token the minter was answered is a token the repository route knows, with what it reaches: the
// stand-in's 200, its 404 and its 401 are what say which token the proxy swapped in and how far
// that token reaches.
//
// The proxy reads its rules from that broker (requests.Machine.AirlockRules): the profiles file's
// github kind on the rig's bound host, which the profile's source is behind by its proxy tag, and
// the profile's own allow, which narrows it. So what these rows read is the profile a person
// wrote, carried through every layer a release crosses: the policy, the target in the signed
// request, the minter, the split into slots and the swap.
package proxy

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// The profile every row serves, and what its request names. profileSource is the github-app
// secret it is a profile of, an agent-tier secret of profileOperator, so a request is granted at
// once; its github-owners tag is policytest.FixtureGitHubOwners, acme and ada, and the App is
// installed on both (profileInstallations); its proxy tag names profileKind and it carries
// proxy-narrowed=true, since its one profile has an allow of its own. The target is one
// repository of ada's.
const (
	profileName       = "GITHUB_READ"
	profileSource     = "PROFILE_APP_KEY"
	profileOperator   = "ada@example.com"
	profileAudience   = "broker"
	profileTarget     = "https://github.com/ada/proofs"
	profileRepository = "/repos/ada/proofs"
)

// profileInstallations is the App's installation on each account, by account: what the stand-in's
// token route reads an installation id as.
var profileInstallations = map[string]int64{"acme": 41, "ada": 42}

// profileKind is the kind the profile's source is behind: the github kind as a profiles file
// declares it, on the rig's bound host, with no allow of its own and the default header, so the
// profile's allow alone bounds where the token may go.
const profileKind = "github"

// profileKindsJSON is the kinds part of every profiles file of these rows.
const profileKindsJSON = `[{"name": "` + profileKind + `", "hosts": ["` + boundHost + `"], "ways": ["swap", "mint"]}]`

// readProfile is the profiles file of the one profile under profileKindsJSON: permissions
// (readOnly, contents and metadata read, unless a row edits them) on both accounts, the value form
// value ("" for none, which is installations), and allow, the profile's own allow as JSON.
func readProfile(value, permissions, allow string) string {
	github := `"accounts": ["acme", "ada"], "permissions": ` + permissions
	if value != "" {
		github += `, "value": "` + value + `"`
	}
	return policytest.KindsFileOf(profileKindsJSON, `{"name": "`+profileName+`", "source": "`+profileSource+`", "github": {`+github+`}, "allow": `+allow+`}`)
}

// profiledSource is the github-app secret stored as source holds it: of profileOperator at agent
// tier, holding value, behind profileKind by its proxy tag and narrowed by its one profile.
func profiledSource(source, secretType, value string) awstest.LocalSecret {
	return policytest.Narrowed(policytest.Proxied(policytest.TypeSecret(source, profileOperator, policy.TierAgent, secretType, value), profileKind))
}

// readOnly is the permissions every row's profile asks for until a row edits them: contents at
// read, and the metadata at read every GitHub profile names.
const readOnly = `{"contents": "read", "metadata": "read"}`

// rolledGrants is the proxy's GrantSource over the broker it reads now: one requests.Machine, and
// another in its place once a row rolls the profiles file (roll), as the broker a profiles change
// rolls in reads one file and the broker before it another. The proxy behind it keeps what it
// held, so a row asks what that held answer is worth once the profile it was read under is gone.
type rolledGrants struct {
	machine atomic.Pointer[requests.Machine]
}

func (g *rolledGrants) AirlockConnection(ctx context.Context, placeholder string) (requests.AirlockConnection, error) {
	return g.machine.Load().AirlockConnection(ctx, placeholder)
}

func (g *rolledGrants) AirlockValues(ctx context.Context, grantID string) (map[string]requests.AirlockSecret, string, error) {
	return g.machine.Load().AirlockValues(ctx, grantID)
}

func (g *rolledGrants) PolicyVersion() string { return g.machine.Load().PolicyVersion() }

func (g *rolledGrants) AirlockAccessPlaceholder(ctx context.Context, grantID, name, slot string) (string, error) {
	return g.machine.Load().AirlockAccessPlaceholder(ctx, grantID, name, slot)
}

func (g *rolledGrants) AirlockRules() proxyrules.Rules { return g.machine.Load().AirlockRules() }

// profileRig is the rig with a broker behind its proxy: the store, the stored App key, the GitHub
// stand-in and the plain service its minter reaches it at, the grant source the proxy reads, and
// one live session of profileOperator, which asks for the profile.
type profileRig struct {
	*rig
	st           *store.Store
	local        *awstest.Local
	stand        *proxyrulestest.GitHubStand
	minting      *proxyrulestest.Service
	grants       *rolledGrants
	enrollmentID string
	key          *ecdsa.PrivateKey
}

// newProfileRig builds one serving file, with o as newRig takes it but for its grant source, which
// is the broker that serves file and whose rules the proxy reads.
func newProfileRig(t *testing.T, file string, o Options) *profileRig {
	t.Helper()
	pr := &profileRig{
		st:     storetest.Open(t),
		local:  awstest.NewLocal(profiledSource(profileSource, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))),
		stand:  proxyrulestest.NewGitHubStand(proxyrulestest.GitHubOptions{Installations: profileInstallations, DistinctTokens: true}),
		grants: &rolledGrants{},
	}
	minting, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatalf("start the GitHub stand-in the minter reaches: %v", err)
	}
	t.Cleanup(minting.Close)
	minting.AnswerWith(pr.stand.Answer)
	pr.minting = minting
	pr.grants.machine.Store(pr.machine(t, file))
	o.Grants = pr.grants
	pr.rig = newRig(t, o)
	pr.answerWith(pr.stand.Answer)
	pr.enrollmentID, pr.key = ciphertest.InsertSession(t, pr.st.Pool, profileOperator)
	return pr
}

// machine is a broker serving file over the rig's store and stored App key, as cmd/cipherd wires
// one: the live policy's rules as its airlock's, and a github-app minter, the production one,
// aimed at the plain stand-in.
func (pr *profileRig) machine(t *testing.T, file string) *requests.Machine {
	t.Helper()
	m := &requests.Machine{
		Store: pr.st, Policy: policytest.ProfiledCurrent(t, pr.local, file), Secrets: policytest.Store(pr.local),
		PendingTTL: 12 * time.Hour, Audience: profileAudience, Skew: time.Minute,
		Replay:  (&enroll.Service{Store: pr.st}).Replay,
		Minters: map[string]requests.Minter{policy.TypeGitHubApp: secrets.GitHubApp{Base: pr.minting.URL()}.Mint},
		// Two strings the release carries to the session unread: the session in these rows is
		// the rig's own client, which is pointed at the rig's proxy and trusts its authority.
		AirlockProxyURL: "http://127.0.0.1:13381", ProxyRoots: func() string { return "-----BEGIN CERTIFICATE-----\nprofile-rig\n-----END CERTIFICATE-----\n" },
	}
	m.Chain = requests.NewChainVerifier(pr.st, profileAudience, time.Minute, m.Policy)
	return m
}

// roll has the proxy read a broker serving file in place of the one it reads, as the broker a
// profiles change rolls in does.
func (pr *profileRig) roll(t *testing.T, file string) {
	t.Helper()
	pr.grants.machine.Store(pr.machine(t, file))
}

// grant asks for the profile narrowed to profileTarget, as `cipher run GITHUB_READ --target
// ada/proofs` does, and releases the grant it gets at once as the session does: it answers the
// grant's id and what the release names behind the proxy for the profile.
func (pr *profileRig) grant(t *testing.T) (grantID string, released *requests.AirlockRelease) {
	t.Helper()
	m := pr.grants.machine.Load()
	signed, err := record.Sign(pr.key, profileAudience, []record.AuthorizationDetail{{
		Type: record.KindAgentSecret, Identifier: profileName, Actions: []string{"inject"}, Locations: []string{profileTarget},
	}}, "read the proofs repository", "", nil, time.Now())
	if err != nil {
		t.Fatalf("sign the request object: %v", err)
	}
	request, err := m.Create(t.Context(), pr.enrollmentID, signed, "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want a grant at once", request, err)
	}
	release, err := m.Values(t.Context(), *request.GrantID, pr.enrollmentID, requests.ReleaseCall{})
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if release.Airlock == nil {
		t.Fatalf("the release names nothing behind the proxy: %+v", release)
	}
	if _, inline := release.Values[profileName]; inline {
		t.Fatalf("the release hands over %s's value itself; want it behind the proxy alone", profileName)
	}
	return *request.GrantID, release.Airlock
}

// placeholderOf is the one placeholder the release names for the profile, failing t unless it
// names exactly one slot, slot.
func placeholderOf(t *testing.T, released *requests.AirlockRelease, slot string) string {
	t.Helper()
	slots := released.Names[profileName].Slots
	if got := slices.Sorted(maps.Keys(slots)); !slices.Equal(got, []string{slot}) {
		t.Fatalf("the release names %s's slots %v; want %s alone, the one account the target names", profileName, got, slot)
	}
	return slots[slot]
}

// mintAsked is one access_tokens request the minter sent the stand-in: the installation it asked
// of and the body it sent, the fields secrets' githubAccessTokensBody writes.
type mintAsked struct {
	path         string
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

// mintsAsked is every access_tokens request the minter has sent, in order.
func (pr *profileRig) mintsAsked(t *testing.T) []mintAsked {
	t.Helper()
	var asked []mintAsked
	for _, seen := range pr.minting.Requests() {
		if seen.Method != http.MethodPost || !strings.HasSuffix(seen.Path, "/access_tokens") {
			continue
		}
		read := mintAsked{path: seen.Path}
		if err := json.Unmarshal([]byte(seen.Body), &read); err != nil {
			t.Fatalf("the token request's body %q: %v", seen.Body, err)
		}
		asked = append(asked, read)
	}
	return asked
}

// reaches is the status the stand-in answers a direct read of path with token, past the proxy:
// 200 for a token it minted whose scope holds that repository, 404 for one whose scope does not,
// and 401 for a string it never minted.
func (pr *profileRig) reaches(t *testing.T, token, path string) int {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, pr.minting.URL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	answer, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("read %s from the stand-in: %v", path, err)
	}
	answer.Body.Close()
	return answer.StatusCode
}

// upstream is every request the service behind the bound host has been sent past the first
// from, as the stand-in recorded them.
func (pr *profileRig) upstream(from int) []seenRequest { return pr.requests()[from:] }

// TestTheProxyMintsTheProfilesScopeForTheTargetSlot: a GitHub profile behind the proxy is minted
// when the proxy first carries a call of its grant, with the profile's permissions and the
// target's one account and repository, and the release names that account's slot alone: for the
// default value form, the installation's slot; for a value-token profile, the one value slot of
// the bare token. The token the proxy swaps in reaches the target's repository and no other.
//
// The named revert: mint the source owner-wide in AirlockValues (drop the grant's target before
// its read), and the proxy's mint names no repository and both accounts, so this fails.
func TestTheProxyMintsTheProfilesScopeForTheTargetSlot(t *testing.T) {
	for _, c := range []struct {
		name, value, slot string
	}{
		{"installations", "", requests.SlotGitHubPrefix + "ada"},
		{"token", githubname.ValueToken, requests.SlotValue},
	} {
		t.Run(c.name, func(t *testing.T) {
			pr := newProfileRig(t, readProfile(c.value, readOnly, `[{"method": "GET", "pathPrefix": "/repos/"}]`), Options{})
			_, released := pr.grant(t)
			placeholder := placeholderOf(t, released, c.slot)
			before := len(pr.mintsAsked(t))
			client := pr.client(released.ProxyPassword)

			answer, body := pr.get(client, "https://"+boundHost+profileRepository, "Authorization", "Bearer "+placeholder)
			if answer.StatusCode != http.StatusOK {
				t.Fatalf("GET %s through the proxy = %d %s; want 200, the stand-in reading a token it minted for that repository", profileRepository, answer.StatusCode, body)
			}
			minted := pr.mintsAsked(t)[before:]
			if len(minted) != 1 {
				t.Fatalf("the proxy's read minted %d tokens: %+v; want one, of the target's account", len(minted), minted)
			}
			want := mintAsked{path: "/app/installations/42/access_tokens", Repositories: []string{"proofs"}, Permissions: map[string]string{"contents": "read", "metadata": "read"}}
			if got := minted[0]; got.path != want.path || !slices.Equal(got.Repositories, want.Repositories) || !maps.Equal(got.Permissions, want.Permissions) {
				t.Errorf("the proxy's mint asked %+v; want ada's installation, the target's repository and the profile's permissions, %+v", got, want)
			}
			for _, outside := range []string{"/repos/ada/widgets", "/repos/acme/proofs"} {
				if answer, body := pr.get(client, "https://"+boundHost+outside, "Authorization", "Bearer "+placeholder); answer.StatusCode != http.StatusNotFound {
					t.Errorf("GET %s through the proxy = %d %s; want 404, a repository the target's token does not reach", outside, answer.StatusCode, body)
				}
			}
			if again := pr.mintsAsked(t)[before:]; len(again) != 1 {
				t.Errorf("the proxy minted %d tokens over three calls; want the one it holds", len(again))
			}
		})
	}
}

// TestTheProxySwapsAProfileTokenOnlyIntoAllowedRequests: the profile's own allow is the whole of
// where its token may go, under a kind that allows everything. A call the profile allows has the
// placeholder in Authorization swapped for the minted token; a call of a method or a path outside
// the profile's allow is refused at the proxy, by the profile's rule, and reaches no service; and
// a header the kind does not name carries the placeholder on unchanged, so the token is in no
// header but the ones the kind names.
//
// The named revert: drop the profile's allow on its way into its binding (policy's proxyRules), or
// the decision's read of it (proxyrules.check), and the refused calls are carried with the token,
// so this fails.
func TestTheProxySwapsAProfileTokenOnlyIntoAllowedRequests(t *testing.T) {
	pr := newProfileRig(t, readProfile(githubname.ValueToken, readOnly, `[{"method": "GET", "pathPrefix": "/repos/ada/"}]`), Options{})
	_, released := pr.grant(t)
	placeholder := placeholderOf(t, released, requests.SlotValue)
	client := pr.client(released.ProxyPassword)

	answer, body := pr.get(client, "https://"+boundHost+profileRepository, "Authorization", "Bearer "+placeholder)
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("an allowed GET = %d %s; want 200, the stand-in reading the token the proxy swapped in", answer.StatusCode, body)
	}
	allowed := pr.upstream(0)
	if len(allowed) != 1 {
		t.Fatalf("the service was sent %d requests; want the allowed one", len(allowed))
	}
	token, swapped := strings.CutPrefix(allowed[0].Header.Get("Authorization"), "Bearer ")
	if !swapped || token == "" || token == placeholder || pr.reaches(t, token, profileRepository) != http.StatusOK {
		t.Fatalf("the allowed call reached the service with Authorization %q; want Bearer and a token the stand-in minted for the target", allowed[0].Header.Get("Authorization"))
	}

	for _, refused := range []struct{ name, method, path, reason string }{
		{"a method the profile does not allow", http.MethodPost, profileRepository, "not allowed by the profile's rule"},
		{"a path the profile does not allow", http.MethodGet, "/repos/acme/proofs", "not allowed by the profile's rule"},
		{"a dot segment back out of the allowed prefix", http.MethodGet, "/repos/ada/../acme/proofs", "path has a dot segment"},
	} {
		sent := len(pr.requests())
		refusals := len(pr.logs.refusals())
		answer, body := pr.do(client, refused.method, "https://"+boundHost+refused.path, "", "Authorization", "Bearer "+placeholder)
		if answer.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %s %s = %d %s; want 403 at the proxy", refused.name, refused.method, refused.path, answer.StatusCode, body)
		}
		if got := pr.upstream(sent); len(got) != 0 {
			t.Errorf("%s: the service was sent %+v; want nothing, since the proxy refused the call", refused.name, got)
		}
		lines := pr.logs.refusals()[refusals:]
		if len(lines) != 1 || lines[0].fields["code"] != codeNotAllowed || lines[0].fields["reason"] != refused.reason {
			t.Errorf("%s: the refusal logged %+v; want one line, %s, %q", refused.name, lines, codeNotAllowed, refused.reason)
		}
	}

	// The placeholder in a header the kind does not name: carried on as it is, with nothing
	// swapped into it and the token nowhere in what the service reads.
	sent := len(pr.requests())
	if answer, body := pr.get(client, "https://"+boundHost+profileRepository, "X-Api-Token", placeholder); answer.StatusCode != http.StatusUnauthorized {
		t.Errorf("a GET carrying the placeholder in X-Api-Token = %d %s; want the stand-in's 401, since no token was swapped in", answer.StatusCode, body)
	}
	unnamed := pr.upstream(sent)
	if len(unnamed) != 1 || unnamed[0].Header.Get("X-Api-Token") != placeholder {
		t.Fatalf("the service was sent %+v; want one request carrying the placeholder in X-Api-Token", unnamed)
	}
	for name, values := range unnamed[0].Header {
		for _, value := range values {
			if strings.Contains(value, token) {
				t.Errorf("the service read the token in %s: %q; want it in no header the kind does not name", name, value)
			}
		}
	}
}

// TestAnEditedProfileEndsTheProxysHeldToken: the proxy holds a grant's token under the policy
// version it was read under, and that version carries the profile's digest. Once the profiles
// file is edited - here the profile asks for one more permission - the proxy reads the grant
// again rather than spend the token it holds, and that read ends the grant, whose profile is no
// longer the one it was decided under: the next call is refused 407 and reaches no service, and
// nothing is minted for it.
//
// The negative control: Options.KeepValues holds the read answer past a moved version, and the
// call is then carried with the old token, so this fails.
func TestAnEditedProfileEndsTheProxysHeldToken(t *testing.T) {
	allow := `[{"method": "GET", "pathPrefix": "/repos/"}]`
	pr := newProfileRig(t, readProfile(githubname.ValueToken, readOnly, allow), Options{})
	grantID, released := pr.grant(t)
	placeholder := placeholderOf(t, released, requests.SlotValue)
	client := pr.client(released.ProxyPassword)
	if answer, body := pr.get(client, "https://"+boundHost+profileRepository, "Authorization", "Bearer "+placeholder); answer.StatusCode != http.StatusOK {
		t.Fatalf("the first call = %d %s; want 200", answer.StatusCode, body)
	}

	pr.roll(t, readProfile(githubname.ValueToken, `{"contents": "read", "metadata": "read", "pull_requests": "read"}`, allow))
	sent, minted, refusals := len(pr.requests()), len(pr.mintsAsked(t)), len(pr.logs.refusals())
	answer, body := pr.get(client, "https://"+boundHost+profileRepository, "Authorization", "Bearer "+placeholder)
	if answer.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("a call after the profile's edit = %d %s; want 407, the grant ended", answer.StatusCode, body)
	}
	if lines := pr.logs.refusals()[refusals:]; len(lines) != 1 || lines[0].fields["code"] != codeGrantNotLive {
		t.Errorf("the refusal logged %+v; want one line, %s", lines, codeGrantNotLive)
	}
	if got := pr.upstream(sent); len(got) != 0 {
		t.Errorf("the service was sent %+v after the edit; want nothing, the held token spent on no call", got)
	}
	if got := len(pr.mintsAsked(t)) - minted; got != 0 {
		t.Errorf("the read after the edit minted %d tokens; want none for a grant it ended", got)
	}
	var revokedBy *string
	if err := pr.st.Pool.QueryRow(t.Context(), `select revoked_by from grants where id = $1`, grantID).Scan(&revokedBy); err != nil {
		t.Fatalf("read the grant: %v", err)
	}
	if revokedBy == nil || *revokedBy != requests.GrantEndedActor {
		t.Errorf("the grant's revoked_by = %v; want %s, the release check that ended it", revokedBy, requests.GrantEndedActor)
	}
}

// TestAGitBasicAuthPlaceholderIsSwappedForAProfileToken: git sends a GitHub token as the password
// of a Basic credential whose user is x-access-token, which is how a session's clone runs under a
// value-token profile through the proxy. The proxy swaps the placeholder inside that credential
// (airlock.substitute) and re-encodes it, so the service reads x-access-token and the minted
// token, a token of the target's repository, and no placeholder.
func TestAGitBasicAuthPlaceholderIsSwappedForAProfileToken(t *testing.T) {
	pr := newProfileRig(t, readProfile(githubname.ValueToken, readOnly, `[{"method": "GET", "pathPrefix": "/ada/proofs.git/"}]`), Options{})
	_, released := pr.grant(t)
	held := placeholderOf(t, released, requests.SlotValue)
	client := pr.client(released.ProxyPassword)

	credential := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+held))
	// The stand-in serves no git route, and answers it 404: what this row reads is what the
	// service was sent, which the proxy carried.
	if answer, body := pr.get(client, "https://"+boundHost+"/ada/proofs.git/info/refs?service=git-upload-pack", "Authorization", credential); answer.StatusCode != http.StatusNotFound {
		t.Fatalf("git's GET = %d %s; want the stand-in's 404 for a route it does not serve, carried by the proxy", answer.StatusCode, body)
	}
	sent := pr.upstream(0)
	if len(sent) != 1 {
		t.Fatalf("the service was sent %d requests; want git's one", len(sent))
	}
	scheme, encoded, _ := strings.Cut(sent[0].Header.Get("Authorization"), " ")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if scheme != "Basic" || err != nil {
		t.Fatalf("the service read Authorization %q; want a Basic credential", sent[0].Header.Get("Authorization"))
	}
	user, token, _ := strings.Cut(string(decoded), ":")
	if user != "x-access-token" || token == "" || token == held {
		t.Fatalf("the service read the credential %q; want x-access-token and the minted token", decoded)
	}
	if got := pr.reaches(t, token, profileRepository); got != http.StatusOK {
		t.Errorf("the swapped token reads %s = %d; want 200, a token the stand-in minted for the target", profileRepository, got)
	}
	if got := pr.reaches(t, token, "/repos/ada/widgets"); got != http.StatusNotFound {
		t.Errorf("the swapped token reads /repos/ada/widgets = %d; want 404, outside the target", got)
	}
	for name, values := range sent[0].Header {
		for _, value := range values {
			if strings.Contains(value, placeholder.Prefix) {
				t.Errorf("the service read a placeholder in %s: %q", name, value)
			}
		}
	}
}
