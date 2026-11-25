// packages/envoy/internal/cipher/api/github_app_field_test.go
// The github_app field an agent-secrets of a release before --target sends with --github-owner,
// --github-repo, --github-read-only and --github-token, replayed from the exact bytes released
// clients send (oldClientBodies), against a broker serving profiles shaped as a deployment's: the
// field is answered by default as legion-envoy-v33.7.0 answered it, BROKER_GITHUB_APP_FIELD=off
// refuses it 400 GITHUB_APP_FIELD_REMOVED as before, and a --target request is the same either way.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// oldClientBody is what one released agent-secrets sent for one run, captured byte for byte by a
// stand-in broker: the POST /v1/requests body, whose signed request object is replaced by
// signedRequestObject (the test signs a fresh one naming the same one secret, with no reason, no
// lifetime and no locations, as the captured object did), and the POST /v1/grants/{id}/values
// body, unchanged.
type oldClientBody struct {
	client, run, name string
	request, release  string
}

// signedRequestObject stands where the captured create body held its signed request object.
const signedRequestObject = "SIGNED_REQUEST_OBJECT"

// oldClientBodies are the bodies of `agent-secrets NAME --github-owner acme --github-repo widgets
// [--github-read-only] --github-token -- sh -c 'printf %s "$NAME"'`, run with
// ENVOY_SESSION_ID=example-session by the agent-secrets of legion-v20.42.5 and legion-v22.16.2
// (cmd/agent-secrets, the Legion pods' client) and of legion-envoy-v32.28.1 (cmd/cipher, a dev
// box's), each built from its tag and pointed at a stand-in broker that answered an automatic grant
// and the installations value. LEGION_IMPLEMENT_APP is the shared App a pod asks for; GITHUB_APP_ADA
// stands for a person's own App, which that person's box asks for.
var oldClientBodies = func() []oldClientBody {
	podRun := func(client, run, github string) oldClientBody {
		return oldClientBody{client, run, "LEGION_IMPLEMENT_APP",
			`{"request":"SIGNED_REQUEST_OBJECT","session_id":"example-session","github_app":` + github + `}`,
			`{"github_app":` + github + `,"command":["sh","-c","printf %s \"$LEGION_IMPLEMENT_APP\""]}`}
	}
	const readOnly, write = `{"owner":"acme","repositories":["widgets"],"read_only":true}`, `{"owner":"acme","repositories":["widgets"]}`
	var bodies []oldClientBody
	for _, client := range []string{"legion-v20.42.5", "legion-v22.16.2", "legion-envoy-v32.28.1"} {
		bodies = append(bodies, podRun(client, "pod read-only", readOnly), podRun(client, "pod write", write))
	}
	for _, box := range []struct{ run, github string }{{"box read-only", readOnly}, {"box write", write}} {
		bodies = append(bodies, oldClientBody{"legion-envoy-v32.28.1", box.run, "GITHUB_APP_ADA",
			`{"request":"SIGNED_REQUEST_OBJECT","session_id":"example-session","github_app":` + box.github + `}`,
			`{"github_app":` + box.github + `,"command":["sh","-c","printf %s \"$GITHUB_APP_ADA\""]}`})
	}
	return bodies
}()

// The read and write permissions of the deployment's GitHub profiles of the shared App.
const (
	readPermissions  = `{"actions": "read", "attestations": "read", "checks": "read", "contents": "read", "issues": "read", "metadata": "read", "pull_requests": "read", "statuses": "read"}`
	writePermissions = `{"actions": "write", "attestations": "read", "checks": "read", "contents": "write", "issues": "write", "metadata": "read", "pull_requests": "write", "security_events": "read", "statuses": "write", "workflows": "write"}`
	adaPermissions   = `{"contents": "write", "issues": "write", "metadata": "read", "pull_requests": "write", "workflows": "write"}`
	proxyBlock       = `"proxy": {"hosts": ["api.github.com", "github.com", "uploads.github.com"], "headers": ["Authorization"]}`
)

// oldClientService is the service a Legion issue pod and a session pod enrol as, which the
// deployment names in BROKER_GITHUB_APP_FIELD_REQUESTERS; otherService is another registered
// service of the same deployment, whose pods run a client with --target and whom the list does
// not name.
const (
	oldClientService = "legion-daemon"
	otherService     = "example-worker"
)

// boxPerson is the person whose box asks for GITHUB_APP_ADA, their own App.
const boxPerson = "ada@example.com"

// oldClientProfiles are a deployment's profiles of its two github-app secrets: the shared
// LEGION_IMPLEMENT_APP, served by its own name to one person, as GITHUB_READ to everyone and as
// GITHUB_WRITE to Legion's pods and that person, the last two as bare tokens behind the airlock;
// and a person's GITHUB_APP_ADA, served to that person alone, and as GITHUB_APP_ADA_AGENT, a bare
// token behind the airlock that every agent-tier caller may ask for, as a deployment serves a
// person's App to the agents that act for them.
var oldClientProfiles = policytest.ProfilesFile(
	`{"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP", "tier": "agent", "github": {"accounts": ["acme", "ada", "cybermonklabs"], "permissions": `+readPermissions+`, "value": "token"}, `+proxyBlock+`}`,
	`{"name": "GITHUB_WRITE", "source": "LEGION_IMPLEMENT_APP", "tier": "agent", "requesters": ["legion-daemon", "ada@example.com"], "github": {"accounts": ["acme", "ada", "cybermonklabs"], "permissions": `+writePermissions+`, "value": "token"}, `+proxyBlock+`}`,
	`{"name": "LEGION_IMPLEMENT_APP", "requesters": ["ada@example.com"], "github": {"accounts": ["acme", "ada", "cybermonklabs"], "permissions": `+writePermissions+`, "value": "installations"}}`,
	`{"name": "LEGION_IMPLEMENT_APP_READ", "source": "LEGION_IMPLEMENT_APP", "requesters": ["ada@example.com"], "github": {"accounts": ["acme", "ada", "cybermonklabs"], "permissions": `+readPermissions+`, "value": "installations"}}`,
	`{"name": "GITHUB_APP_ADA", "requesters": ["ada@example.com"], "github": {"accounts": ["acme", "ada"], "permissions": `+adaPermissions+`, "value": "installations"}}`,
	`{"name": "GITHUB_APP_ADA_READ", "source": "GITHUB_APP_ADA", "requesters": ["ada@example.com"], "github": {"accounts": ["acme", "ada"], "permissions": {"contents": "read", "metadata": "read"}, "value": "installations"}}`,
	`{"name": "GITHUB_APP_ADA_AGENT", "source": "GITHUB_APP_ADA", "tier": "agent", "github": {"accounts": ["acme", "ada"], "permissions": `+adaPermissions+`, "value": "token"}, `+proxyBlock+`}`,
	`{"name": "GITHUB_APPROVED", "source": "LEGION_IMPLEMENT_APP", "tier": "human", "github": {"accounts": ["acme", "ada", "cybermonklabs"], "permissions": `+writePermissions+`, "value": "token"}, `+proxyBlock+`}`,
)

// gitHubAppFieldServer is a test broker serving oldClientProfiles behind an airlock, whose
// github-app minter records each request and answers the value of the form the scope asks for.
type gitHubAppFieldServer struct {
	*testServer
	mu     sync.Mutex
	minted []secrets.MintRequest
}

func newGitHubAppFieldServer(t *testing.T, off bool) *gitHubAppFieldServer {
	t.Helper()
	s := &gitHubAppFieldServer{}
	s.testServer = newTestServerWith(t, func(d *api.Deps) {
		local := d.Machine.Secrets.(secrets.AWS).Client.(*secrets.Local)
		// The shared App's installations, which its profiles may name: the two of every github-app
		// fixture and the account the approval-shadow pod's call asks for, since a profile naming
		// an account its source's github-owners tag leaves out is refused profile-block-mismatch.
		// A deployment's own shared App carries that tag the same way, naming each account the App
		// is installed on, so this fixture is no wider than one.
		shared := policytest.TypeSecret("LEGION_IMPLEMENT_APP", policy.OwnerShared, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
		shared.Tags[policy.TagGitHubOwners] = policytest.FixtureGitHubOwners + " cybermonklabs"
		local.Put(shared)
		local.Put(policytest.TypeSecret("GITHUB_APP_ADA", boxPerson, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)))
		cur := policytest.ProfiledCurrent(t, local, oldClientProfiles, oldClientService, otherService)
		d.Policy, d.Machine.Policy, d.MachineLogin.Policy = cur, cur, cur
		d.Machine.ServicesByAccount = map[string]string{workerAccount: oldClientService, otherAccount: otherService}
		d.Machine.Airlock = policytest.Profiles(t, oldClientProfiles, oldClientService, otherService).ProxyRules()
		d.Machine.AirlockProxyURL = "http://airlock.example:3128"
		d.Machine.AirlockCACertificate = "the airlock's authority"
		d.Machine.GitHubAppFieldOff = off
		// BROKER_GITHUB_APP_FIELD_REQUESTERS: the deployment's own list, the service whose pods
		// run a released client and the person whose box runs one. A caller it does not name is
		// answered no field but for their own App, which
		// TestTheOldBodyIsAnsweredForNoRequesterTheDeploymentDoesNotName holds; with no list at
		// all the broker answers no service.
		d.Machine.GitHubAppFieldRequesters = oldClientService + "," + boxPerson
		// BROKER_GITHUB_APP_FIELD_SECRETS: the stored Apps this deployment's earlier broker
		// handed over raw. An App outside it is refused, which
		// TestAnAppTheDeploymentDoesNotNameIsRefused holds.
		d.Machine.GitHubAppFieldSecrets = "LEGION_IMPLEMENT_APP,GITHUB_APP_ADA"
		d.Machine.Minters = map[string]requests.Minter{policy.TypeGitHubApp: s.mint}
	})
	return s
}

// mint records req and answers a token of each account its scope names: the bare token of the
// one account for the token form, else the installations object a release hands over.
func (s *gitHubAppFieldServer) mint(_ context.Context, req secrets.MintRequest) (secrets.Minted, error) {
	s.mu.Lock()
	s.minted = append(s.minted, req)
	s.mu.Unlock()
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if req.GitHub.Value == githubname.ValueToken {
		return secrets.Minted{Value: "ghs_fixture_" + req.GitHub.Accounts[0], Expires: expires}, nil
	}
	installations := map[string]any{}
	for i, account := range req.GitHub.Accounts {
		installations[account] = map[string]any{"installation_id": 7 + i, "token": "ghs_fixture_" + account, "expires_at": expires.Format(time.RFC3339)}
	}
	value, err := json.Marshal(map[string]any{"installations": installations})
	return secrets.Minted{Value: string(value), Expires: expires}, err
}

// take returns the requests the minter was handed since the last take, and forgets them.
func (s *gitHubAppFieldServer) take() []secrets.MintRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	minted := s.minted
	s.minted = nil
	return minted
}

// caller is one enrolled session: its enrollment and its key.
type caller struct {
	id  string
	key *ecdsa.PrivateKey
}

// legionPod enrols a pod of oldClientService running as workerAccount, with claim, the issue
// pod's role claim, or session, a session pod's name, as a Legion daemon's launcher enrols one.
func (s *gitHubAppFieldServer) legionPod(t *testing.T, claim, session string) caller {
	t.Helper()
	return s.servicePod(t, oldClientService, workerAccount, claim, session)
}

// servicePod enrols a pod of service running as account, with claim, an issue pod's role claim,
// or session, a session pod's name, as that service's launcher enrols one.
func (s *gitHubAppFieldServer) servicePod(t *testing.T, service, account, claim, session string) caller {
	t.Helper()
	ctx := context.Background()
	key := newSigningKey(t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	credentialID := uuid.New()
	if _, err := s.Store.Pool.Exec(ctx, `insert into launcher_credentials (id, service, host, key_thumbprint, public_jwk, expires_at)
		values ($1,$2,'cluster.example',$3,'{}'::jsonb, now() + interval '30 days')`, credentialID, service, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if _, err := s.Store.Pool.Exec(ctx, `insert into enrollments (id, kind, runtime_id, thumbprint, subject, launcher_credential_id, lease_expires_at, claim, session_id)
		values ($1,'pod',$2,$3,$4,$5, now() + interval '1 hour', nullif($6,''), nullif($7,''))`, id, uuid.NewString(), thumbprint, account, credentialID, claim, session); err != nil {
		t.Fatal(err)
	}
	return caller{id, key}
}

// box enrols a session of person's box.
func (s *gitHubAppFieldServer) box(t *testing.T, person string) caller {
	t.Helper()
	id, key := s.newSessionEnrollment(t, "box-"+uuid.NewString(), person)
	return caller{id, key}
}

// create posts body to POST /v1/requests as c, its signed request object signed now for name.
func (s *gitHubAppFieldServer) create(t *testing.T, c caller, name, body string) (int, []byte) {
	t.Helper()
	signed := signAgentSecretRequest(t, c.key, s.URL, "", name)
	return s.session(t, c.key, c.id, http.MethodPost, "/v1/requests", json.RawMessage(strings.Replace(body, signedRequestObject, signed, 1)))
}

// release posts body to POST /v1/grants/{id}/values as c.
func (s *gitHubAppFieldServer) release(t *testing.T, c caller, grantID, body string) (int, []byte) {
	t.Helper()
	return s.session(t, c.key, c.id, http.MethodPost, "/v1/grants/"+grantID+"/values", json.RawMessage(body))
}

// grantedName is the one name the request row of requestID recorded, which is the profile the
// broker served the old body as.
func (s *gitHubAppFieldServer) grantedName(t *testing.T, requestID string) string {
	t.Helper()
	names := s.column(t, `select name from request_secrets where request_id=$1`, requestID)
	if len(names) != 1 {
		t.Fatalf("request %s recorded %v, want one name", requestID, names)
	}
	return names[0]
}

// bodyOf is the captured body of client's run, which a test names rather than indexes; a name no
// body has is a test bug.
func bodyOf(t *testing.T, run string) oldClientBody {
	t.Helper()
	for _, body := range oldClientBodies {
		if body.run == run {
			return body
		}
	}
	t.Fatalf("no captured body of the run %q", run)
	return oldClientBody{}
}

// oldReleaseAnswer is a values answer as a released client reads it.
type oldReleaseAnswer struct {
	Values    map[string]string `json:"values"`
	GitHubApp json.RawMessage   `json:"github_app"`
	Airlock   json.RawMessage   `json:"airlock"`
	Locations []string          `json:"locations"`
}

// TestOldClientBodiesAreAnsweredAsBefore replays each released client's bodies with no setting
// set. Each request is granted at once to the requester the old form served: an issue pod and a
// session pod of legion-daemon for the shared App, and its person's box for a person's own App.
// Each release answers what that client checks before it runs its command: the github_app it asked
// for, echoed, and the one name it asked for, holding the installations object of the one account
// it named. The mint is narrowed to that account's one repository, outside the airlock, from a
// profile the profiles file grants the requester at once; a read-only body is cut to contents and
// metadata at read, whatever the profile holds, and a write body gets the profile's permissions.
func TestOldClientBodiesAreAnsweredAsBefore(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (policytest.CaptureLog, log.SetFlags at policytest.go:329).
	s := newGitHubAppFieldServer(t, false)
	logged := policytest.CaptureLog(t)
	requesters := map[string]func() caller{
		"issue pod":   func() caller { return s.legionPod(t, "claim-"+uuid.NewString(), "") },
		"session pod": func() caller { return s.legionPod(t, "", "session-"+uuid.NewString()) },
		"box":         func() caller { return s.box(t, boxPerson) },
	}
	for _, body := range oldClientBodies {
		kinds := []string{"issue pod", "session pod"}
		if strings.HasPrefix(body.run, "box") {
			kinds = []string{"box"}
		}
		for _, kind := range kinds {
			t.Run(body.client+", "+body.run+", "+kind, func(t *testing.T) {
				c := requesters[kind]()
				status, raw := s.create(t, c, body.name, body.request)
				created := decode[wireCreateRequestResponse](t, raw)
				if status != http.StatusOK || created.State != "granted" || created.GrantID == nil {
					t.Fatalf("POST /v1/requests = %d %s, want an automatic grant", status, raw)
				}
				served := s.grantedName(t, created.RequestID)
				want := map[string]string{"pod read-only": "GITHUB_READ", "pod write": "GITHUB_WRITE", "box read-only": "GITHUB_APP_ADA", "box write": "GITHUB_APP_ADA"}[body.run]
				if served != want {
					t.Fatalf("the old body was served as %s, want %s", served, want)
				}
				s.take()

				status, raw = s.release(t, c, *created.GrantID, body.release)
				if status != http.StatusOK {
					t.Fatalf("POST values = %d %s, want 200", status, raw)
				}
				answer := decode[oldReleaseAnswer](t, raw)
				var asked struct {
					GitHubApp json.RawMessage `json:"github_app"`
				}
				if err := json.Unmarshal([]byte(body.release), &asked); err != nil {
					t.Fatal(err)
				}
				if string(answer.GitHubApp) != string(asked.GitHubApp) {
					t.Fatalf("the answer's github_app is %s, want the one asked for, %s: the client refuses any other", answer.GitHubApp, asked.GitHubApp)
				}
				if answer.Airlock != nil || answer.Locations != nil || !slices.Equal(slices.Sorted(maps.Keys(answer.Values)), []string{body.name}) {
					t.Fatalf("the answer is %s, want the one value %s and no airlock or locations", raw, body.name)
				}
				var value struct {
					Installations map[string]struct {
						InstallationID *int64 `json:"installation_id"`
						Token          string `json:"token"`
						ExpiresAt      string `json:"expires_at"`
					} `json:"installations"`
				}
				if err := json.Unmarshal([]byte(answer.Values[body.name]), &value); err != nil || len(value.Installations) != 1 {
					t.Fatalf("%s = %s, want the installations object of one account", body.name, answer.Values[body.name])
				}
				if one := value.Installations["acme"]; one.InstallationID == nil || one.Token != "ghs_fixture_acme" || one.ExpiresAt == "" {
					t.Fatalf("%s = %s, want acme's installation, token and expiry", body.name, answer.Values[body.name])
				}

				minted := s.take()
				if len(minted) != 1 {
					t.Fatalf("the release minted %d times, want once", len(minted))
				}
				scope := minted[0].GitHub
				if minted[0].Name != want || !slices.Equal(scope.Accounts, []string{"acme"}) || !slices.Equal(scope.Repositories, []string{"acme/widgets"}) || scope.Value != githubname.ValueInstallations {
					t.Fatalf("the mint was %s for %+v, want %s narrowed to acme/widgets in the installations form", minted[0].Name, scope, want)
				}
				// A read-only body gets the earlier broker's read set, each permission the
				// profile holds: all six of the shared App's read profile, and the three of the
				// person's own App. Any other body gets the profile's own permissions.
				wantPermissions := map[string]string{}
				switch body.run {
				case "pod read-only":
					wantPermissions = map[string]string{"actions": "read", "checks": "read", "contents": "read", "metadata": "read", "pull_requests": "read", "statuses": "read"}
				case "box read-only":
					wantPermissions = map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"}
				case "pod write":
					json.Unmarshal([]byte(writePermissions), &wantPermissions)
				case "box write":
					json.Unmarshal([]byte(adaPermissions), &wantPermissions)
				}
				if !maps.Equal(scope.Permissions, wantPermissions) {
					t.Fatalf("the mint asked for %v, want %v", scope.Permissions, wantPermissions)
				}
			})
		}
	}
	for _, line := range []string{
		"broker: answered the github_app field of an earlier agent-secrets at=request name=LEGION_IMPLEMENT_APP profile=GITHUB_READ",
		"broker: answered the github_app field of an earlier agent-secrets at=release name=LEGION_IMPLEMENT_APP profile=GITHUB_WRITE",
		"broker: answered the github_app field of an earlier agent-secrets at=request name=GITHUB_APP_ADA profile=GITHUB_APP_ADA",
	} {
		if !strings.Contains(logged.String(), line) {
			t.Errorf("the log holds no line %q:\n%s", line, logged)
		}
	}
	if !strings.Contains(logged.String(), "service=legion-daemon") || !strings.Contains(logged.String(), "person=ada@example.com") ||
		!strings.Contains(logged.String(), "owner=acme repositories=[widgets] read_only=true") {
		t.Errorf("the answered lines do not name the requester and the narrowing:\n%s", logged)
	}
}

// TestOldClientBodiesAreRefusedWhenTheSettingIsOff is the control of every answer above: with
// BROKER_GITHUB_APP_FIELD=off each caller kind — an issue pod, a session pod and the App owner's
// box — is refused 400 GITHUB_APP_FIELD_REMOVED with the message of a broker before the answer,
// on both routes, and nothing is written or minted. The release half runs on a --target grant of
// that caller, since with the setting off no narrowed grant can be made at all.
func TestOldClientBodiesAreRefusedWhenTheSettingIsOff(t *testing.T) {
	t.Parallel()
	s := newGitHubAppFieldServer(t, true)
	refused := func(t *testing.T, what string, status int, raw []byte) {
		t.Helper()
		if reply := decode[wireError](t, raw); status != http.StatusBadRequest || reply.Code != "GITHUB_APP_FIELD_REMOVED" || reply.Error != gitHubAppFieldRemoved || strings.Contains(string(raw), `"values"`) {
			t.Fatalf("%s = %d %s, want 400 GITHUB_APP_FIELD_REMOVED with the message of a broker before the answer", what, status, raw)
		}
	}
	for _, tc := range []struct {
		kind    string
		caller  func() caller
		name    string
		profile string
	}{
		{"issue pod", func() caller { return s.legionPod(t, "claim-off-"+uuid.NewString(), "") }, "LEGION_IMPLEMENT_APP", "GITHUB_READ"},
		{"session pod", func() caller { return s.legionPod(t, "", "session-off-"+uuid.NewString()) }, "LEGION_IMPLEMENT_APP", "GITHUB_READ"},
		{"box", func() caller { return s.box(t, boxPerson) }, "GITHUB_APP_ADA", "GITHUB_APP_ADA_AGENT"},
	} {
		for _, body := range oldClientBodies {
			if body.name != tc.name {
				continue
			}
			t.Run(tc.kind+", create, "+body.client+", "+body.run, func(t *testing.T) {
				status, raw := s.create(t, tc.caller(), body.name, body.request)
				refused(t, "POST /v1/requests", status, raw)
			})
		}
		t.Run(tc.kind+", release", func(t *testing.T) {
			c := tc.caller()
			created := s.requestTargeted(t, c, tc.profile, "https://github.com/acme/widgets")
			s.take()
			status, raw := s.release(t, c, *created.GrantID, oldClientBodies[0].release)
			refused(t, "POST values", status, raw)
			if minted := s.take(); len(minted) != 0 {
				t.Fatalf("the refused release minted %d times, want none", len(minted))
			}
		})
	}
	if rows := s.column(t, `select id::text from requests where id in (select request_id from request_secrets where github_app_field)`); len(rows) != 0 {
		t.Fatalf("the refused bodies wrote %d narrowed requests, want none", len(rows))
	}
}

// TestTheOldBodyIsAnsweredForNoRequesterTheDeploymentDoesNotName: the profiles file alone is
// wider than the broker the field comes from, since a profile with no requesters list is granted
// to every agent-tier caller and the answer hands over a real token outside the airlock. So the
// field is answered only for a caller BROKER_GITHUB_APP_FIELD_REQUESTERS names — here the one
// service — and for a person asking for their own App. A pod of another registered service, and
// another person's box, are each refused 400 GITHUB_APP_FIELD_REMOVED for the shared App and for
// a person's App, though GITHUB_READ and GITHUB_APP_ADA_AGENT are granted to both at once; each
// of them is still served its own --target request, which the airlock holds. Nothing is minted
// for a refused body.
func TestTheOldBodyIsAnsweredForNoRequesterTheDeploymentDoesNotName(t *testing.T) {
	t.Parallel()
	s := newGitHubAppFieldServer(t, false)
	other := s.servicePod(t, otherService, otherAccount, "claim-other", "")
	stranger := s.box(t, "bob@example.com")
	for _, tc := range []struct {
		what string
		c    caller
		name string
	}{
		{"a pod of another service, the shared App", other, "LEGION_IMPLEMENT_APP"},
		{"a pod of another service, a person's App", other, "GITHUB_APP_ADA"},
		{"another person's box, the shared App", stranger, "LEGION_IMPLEMENT_APP"},
		{"another person's box, a person's App", stranger, "GITHUB_APP_ADA"},
	} {
		body := bodyOf(t, "pod write")
		if tc.name == "GITHUB_APP_ADA" {
			body = bodyOf(t, "box write")
		}
		status, raw := s.create(t, tc.c, tc.name, body.request)
		reply := decode[wireError](t, raw)
		if status != http.StatusBadRequest || reply.Code != "GITHUB_APP_FIELD_REMOVED" || !strings.Contains(reply.Error, "BROKER_GITHUB_APP_FIELD_REQUESTERS") {
			t.Errorf("%s: POST /v1/requests = %d %s, want 400 GITHUB_APP_FIELD_REMOVED naming the setting", tc.what, status, raw)
		}
	}
	if minted := s.take(); len(minted) != 0 {
		t.Fatalf("the minter ran %d times for refused bodies, want none", len(minted))
	}
	// The control: each of them is served its own --target request of a profile it is granted,
	// which the airlock holds back, so what the refusal takes away is the raw token alone.
	for _, tc := range []struct {
		what    string
		c       caller
		profile string
	}{
		{"a pod of another service", other, "GITHUB_READ"},
		{"another person's box", stranger, "GITHUB_READ"},
	} {
		created := s.requestTargeted(t, tc.c, tc.profile, "https://github.com/acme/widgets")
		status, raw := s.session(t, tc.c.key, tc.c.id, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", nil)
		answer := decode[struct {
			Values  map[string]string `json:"values"`
			Airlock *struct {
				Names map[string]json.RawMessage `json:"names"`
			} `json:"airlock"`
		}](t, raw)
		if status != http.StatusOK || len(answer.Values) != 0 || answer.Airlock == nil || answer.Airlock.Names[tc.profile] == nil {
			t.Errorf("%s: its own --target release = %d %s, want %s behind the airlock", tc.what, status, raw, tc.profile)
		}
	}
}

// TestAGitHubAppFieldReleasesNoTargetGrant: a release body's github_app releases only a grant
// made from a github_app field, so no grant of a --target client — automatic, or approved by a
// person — is handed over as a raw token outside the airlock by adding the field to its release.
// Both are refused 400 GITHUB_APP_FIELD_REMOVED, nothing is released and nothing is minted, and
// each grant still releases as it did, behind the airlock, afterwards.
func TestAGitHubAppFieldReleasesNoTargetGrant(t *testing.T) {
	t.Parallel()
	s := newGitHubAppFieldServer(t, false)
	pod := s.legionPod(t, "claim-no-steal", "")
	automatic := s.requestTargeted(t, pod, "GITHUB_READ", "https://github.com/acme/widgets")

	// The person-approved grant: a human-tier profile of the same App, which anyone signed in
	// decides, approved on the broker's own page.
	details := []record.AuthorizationDetail{{Type: record.KindAgentSecret, Identifier: "GITHUB_APPROVED", Actions: []string{"inject"}, Locations: []string{"https://github.com/acme/widgets"}}}
	signed, err := record.Sign(pod.key, s.URL, details, "write to a repository", "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, raw := s.session(t, pod.key, pod.id, http.MethodPost, "/v1/requests", map[string]any{"request": signed})
	pending := decode[wireCreateRequestResponse](t, raw)
	if status != http.StatusOK || pending.State != "pending" || pending.RecordID == nil {
		t.Fatalf("POST /v1/requests for GITHUB_APPROVED = %d %s, want a pending request", status, raw)
	}
	s.decide(t, boxPerson, *pending.RecordID, true)
	approved := s.grantOf(t, pending.RequestID)
	s.take()

	for _, tc := range []struct {
		what  string
		grant string
	}{
		{"an automatic --target grant", *automatic.GrantID},
		{"a --target grant a person approved", approved},
	} {
		status, raw := s.release(t, pod, tc.grant, oldClientBodies[0].release)
		reply := decode[wireError](t, raw)
		if status != http.StatusBadRequest || reply.Code != "GITHUB_APP_FIELD_REMOVED" || strings.Contains(string(raw), `"values"`) {
			t.Errorf("%s released with a github_app field = %d %s, want 400 GITHUB_APP_FIELD_REMOVED and no value", tc.what, status, raw)
		}
		if minted := s.take(); len(minted) != 0 {
			t.Errorf("%s: the refused release minted %d times, want none", tc.what, len(minted))
		}
		status, raw = s.session(t, pod.key, pod.id, http.MethodPost, "/v1/grants/"+tc.grant+"/values", nil)
		if answer := decode[oldReleaseAnswer](t, raw); status != http.StatusOK || answer.Airlock == nil || len(answer.Values) != 0 {
			t.Errorf("%s: its own release after the refusal = %d %s, want the name behind the airlock", tc.what, status, raw)
		}
		s.take()
	}
}

// TestANarrowedGrantIsReleasedOnlyThroughItsField: a grant made from a github_app field is
// released only with that field, of the same read_only. Dropping the field is refused, so the
// profile's own wider value is never handed over in place of the cut one; asking for another
// read_only is refused, so a read-only run's grant never releases a write token. Nothing is
// released or minted for either, and the release with its own field still answers.
func TestANarrowedGrantIsReleasedOnlyThroughItsField(t *testing.T) {
	t.Parallel()
	s := newGitHubAppFieldServer(t, false)
	pod := s.legionPod(t, "claim-one-way", "")
	readOnly, write := oldClientBodies[0], oldClientBodies[1]
	status, raw := s.create(t, pod, readOnly.name, readOnly.request)
	created := decode[wireCreateRequestResponse](t, raw)
	if status != http.StatusOK || created.GrantID == nil {
		t.Fatalf("POST /v1/requests = %d %s, want a grant", status, raw)
	}
	s.take()
	for _, tc := range []struct {
		what string
		body any
	}{
		{"with no github_app field", nil},
		{"with the field's read_only changed", json.RawMessage(write.release)},
	} {
		status, raw := s.session(t, pod.key, pod.id, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", tc.body)
		reply := decode[wireError](t, raw)
		if status != http.StatusBadRequest || reply.Code != "GITHUB_APP_FIELD_REMOVED" || strings.Contains(string(raw), `"values"`) {
			t.Errorf("the narrowed grant released %s = %d %s, want 400 GITHUB_APP_FIELD_REMOVED and no value", tc.what, status, raw)
		}
		if minted := s.take(); len(minted) != 0 {
			t.Errorf("released %s: the minter ran %d times, want none", tc.what, len(minted))
		}
	}
	status, raw = s.release(t, pod, *created.GrantID, readOnly.release)
	if status != http.StatusOK {
		t.Fatalf("the narrowed grant released with its own field = %d %s, want 200", status, raw)
	}
	readSet := map[string]string{"actions": "read", "checks": "read", "contents": "read", "metadata": "read", "pull_requests": "read", "statuses": "read"}
	if minted := s.take(); len(minted) != 1 || !maps.Equal(minted[0].GitHub.Permissions, readSet) {
		t.Fatalf("the mint was %+v, want the read-only cut %v", minted, readSet)
	}
}

// TestNeitherFormReusesTheOthersGrant: a live grant answers a second ask only for a request of
// the same kind. A --target request does not take a narrowed grant, whose release hands a token
// past the airlock, and a narrowed request does not take a --target grant, whose release the
// airlock holds. Each ask writes a grant of its own, and each releases as its own kind does.
func TestNeitherFormReusesTheOthersGrant(t *testing.T) {
	t.Parallel()
	s := newGitHubAppFieldServer(t, false)
	pod := s.legionPod(t, "claim-no-reuse", "")
	body := oldClientBodies[1] // the write form, served as GITHUB_WRITE
	status, raw := s.create(t, pod, body.name, body.request)
	narrowed := decode[wireCreateRequestResponse](t, raw)
	if status != http.StatusOK || narrowed.GrantID == nil {
		t.Fatalf("POST /v1/requests = %d %s, want a grant", status, raw)
	}
	targeted := s.requestTargeted(t, pod, "GITHUB_WRITE", "https://github.com/acme/widgets")
	if targeted.Coalesced || *targeted.GrantID == *narrowed.GrantID {
		t.Fatalf("the --target request took the narrowed grant %s", *narrowed.GrantID)
	}
	status, raw = s.create(t, pod, body.name, body.request)
	again := decode[wireCreateRequestResponse](t, raw)
	if status != http.StatusOK || again.GrantID == nil || *again.GrantID == *targeted.GrantID {
		t.Fatalf("the narrowed request = %d %s; it must not take the --target grant %s", status, raw, *targeted.GrantID)
	}
	if *again.GrantID != *narrowed.GrantID {
		t.Errorf("the narrowed request got grant %s, want its own kind's live grant %s", *again.GrantID, *narrowed.GrantID)
	}
}

// requestTargeted requests name narrowed to locations as c, failing t on anything but an
// automatic grant.
func (s *gitHubAppFieldServer) requestTargeted(t *testing.T, c caller, name string, locations ...string) wireCreateRequestResponse {
	t.Helper()
	details := []record.AuthorizationDetail{{Type: record.KindAgentSecret, Identifier: name, Actions: []string{"inject"}, Locations: locations}}
	signed, err := record.Sign(c.key, s.URL, details, "read a repository", "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, raw := s.session(t, c.key, c.id, http.MethodPost, "/v1/requests", map[string]any{"request": signed})
	created := decode[wireCreateRequestResponse](t, raw)
	if status != http.StatusOK || created.State != "granted" || created.GrantID == nil {
		t.Fatalf("POST /v1/requests for %s at %v = %d %s, want an automatic grant", name, locations, status, raw)
	}
	return created
}

// TestATargetRequestIsTheSameWithTheFieldOnAndOff: a pod's request of GITHUB_READ --target
// acme/widgets is granted, and its release holds the name behind the airlock with the target it
// signed, minted as the profile's bare token of acme's widgets, with the answer on and off.
func TestATargetRequestIsTheSameWithTheFieldOnAndOff(t *testing.T) {
	t.Parallel()
	for _, off := range []bool{false, true} {
		s := newGitHubAppFieldServer(t, off)
		pod := s.legionPod(t, "claim-target", "")
		created := s.requestTargeted(t, pod, "GITHUB_READ", "https://github.com/acme/widgets")
		s.take()
		status, raw := s.session(t, pod.key, pod.id, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", nil)
		answer := decode[struct {
			Values    map[string]string `json:"values"`
			Locations []string          `json:"locations"`
			Airlock   *struct {
				Names map[string]json.RawMessage `json:"names"`
			} `json:"airlock"`
			GitHubApp json.RawMessage `json:"github_app"`
		}](t, raw)
		if status != http.StatusOK || len(answer.Values) != 0 || answer.Airlock == nil || answer.Airlock.Names["GITHUB_READ"] == nil ||
			!slices.Equal(answer.Locations, []string{"https://github.com/acme/widgets"}) || answer.GitHubApp != nil {
			t.Fatalf("off=%v: POST values = %d %s, want GITHUB_READ behind the airlock at acme/widgets", off, status, raw)
		}
		minted := s.take()
		if len(minted) != 1 || minted[0].GitHub.Value != githubname.ValueToken || !slices.Equal(minted[0].GitHub.Repositories, []string{"acme/widgets"}) || len(minted[0].GitHub.Permissions) != 8 {
			t.Fatalf("off=%v: the mint was %+v, want GITHUB_READ's bare token of acme/widgets with its eight permissions", off, minted)
		}
	}
}

// TestTheOldBodyKeepsTheProfilesRefusals: an old body is served only from a profile the profiles
// file grants the requester at once and whose scope holds the account and repositories it names,
// and the one shared check decides that for the request and for its release alike
// (checkGitHubAppField). A pod asking for a person's App is refused, not
// denied, since the requester rule comes before the profiles file; an account outside every
// profile of the App is refused for a caller the field is answered for, with nothing minted; and
// a release whose github_app names another target than its grant's is refused. A name the field
// may not narrow at all — a stored secret served by its own name, with no github block — is
// refused outright: there is no fallback the release would not admit.
func TestTheOldBodyKeepsTheProfilesRefusals(t *testing.T) {
	t.Parallel()
	s := newGitHubAppFieldServer(t, false)
	// WORKER_TOKEN is named here so the one case below reaches the servability refusal rather
	// than the secrets setting's, which TestAnAppTheDeploymentDoesNotNameIsRefused holds.
	s.machine.GitHubAppFieldSecrets += ",WORKER_TOKEN"
	pod := s.legionPod(t, "claim-refused", "")
	box := s.box(t, boxPerson)
	ada := bodyOf(t, "box write")
	outside := strings.ReplaceAll(oldClientBodies[1].request, `"owner":"acme"`, `"owner":"outsider"`)
	stored := strings.ReplaceAll(oldClientBodies[1].request, "LEGION_IMPLEMENT_APP", "WORKER_TOKEN")

	for _, tc := range []struct {
		what  string
		c     caller
		name  string
		body  string
		words string
	}{
		{"a pod asks for a person's own App", pod, ada.name, ada.request, "no profile of GITHUB_APP_ADA is granted"},
		{"a pod asks for an account outside the App's profiles", pod, "LEGION_IMPLEMENT_APP", outside, "no profile of LEGION_IMPLEMENT_APP is granted"},
		{"a box asks for an account outside the App's profiles", box, "LEGION_IMPLEMENT_APP", outside, "no profile of LEGION_IMPLEMENT_APP is granted"},
		{"a pod asks for a stored secret that is no GitHub profile", pod, "WORKER_TOKEN", stored, "no github-app profile"},
	} {
		status, raw := s.create(t, tc.c, tc.name, tc.body)
		reply := decode[wireError](t, raw)
		if status != http.StatusBadRequest || reply.Code != "GITHUB_APP_FIELD_REMOVED" || !strings.Contains(reply.Error, tc.words) {
			t.Errorf("%s: POST /v1/requests = %d %s, want 400 GITHUB_APP_FIELD_REMOVED naming %q", tc.what, status, raw, tc.words)
		}
	}

	created := s.requestTargeted(t, pod, "GITHUB_READ", "https://github.com/acme/gadgets")
	status, raw := s.release(t, pod, *created.GrantID, oldClientBodies[0].release)
	if reply := decode[wireError](t, raw); status != http.StatusBadRequest || reply.Code != "GITHUB_APP_FIELD_REMOVED" || strings.Contains(string(raw), `"values"`) {
		t.Errorf("a release narrowed to another target than its grant's = %d %s, want 400 GITHUB_APP_FIELD_REMOVED", status, raw)
	}
	if minted := s.take(); len(minted) != 0 {
		t.Fatalf("the minter ran %d times for refused bodies, want none", len(minted))
	}
}

// shadowSessionBody is the request and release an agent-session pod's agent-secrets of a release
// before --target sends for `agent-secrets LEGION_IMPLEMENT_APP --github-owner cybermonklabs
// --github-repo legion --github-read-only`: the call a deployment's approval-shadow session makes,
// and the one this whole change exists for.
var shadowSessionBody = oldClientBody{
	client: "legion-v22.16.2", run: "approval shadow", name: "LEGION_IMPLEMENT_APP",
	request: `{"request":"` + signedRequestObject + `","session_id":"example-session","github_app":{"owner":"cybermonklabs","repositories":["legion"],"read_only":true}}`,
	release: `{"github_app":{"owner":"cybermonklabs","repositories":["legion"],"read_only":true},"command":["sh","-c","printf %s \"$LEGION_IMPLEMENT_APP\""]}`,
}

// TestTheApprovalShadowPodsCallIsAnswered replays that one call. A session pod of the listed
// service asks for the stored secret LEGION_IMPLEMENT_APP, whose own profile lists one person and
// no service, so the policy would deny the pod requester-not-listed if it decided the stored name:
// the answer is that createChecked's narrowing arm swaps the name for the profile
// gitHubAppProfile chose BEFORE the transaction's evaluation reads it, so the pod is granted
// GITHUB_READ, which lists nobody and names that account. Reordering the two — evaluating the
// stored name first — fails this test, which is why the order is named here. The release is the
// read-only cut of that profile for cybermonklabs/legion, outside the airlock. Its controls are
// the same call with BROKER_GITHUB_APP_FIELD=off and the same call from a requester
// BROKER_GITHUB_APP_FIELD_REQUESTERS does not name: each refused, with nothing minted.
func TestTheApprovalShadowPodsCallIsAnswered(t *testing.T) {
	t.Parallel()
	s := newGitHubAppFieldServer(t, false)
	pod := s.legionPod(t, "", "session-approval-shadow")
	status, raw := s.create(t, pod, shadowSessionBody.name, shadowSessionBody.request)
	created := decode[wireCreateRequestResponse](t, raw)
	if status != http.StatusOK || created.State != "granted" || created.GrantID == nil {
		t.Fatalf("POST /v1/requests = %d %s, want an automatic grant, not a denial of LEGION_IMPLEMENT_APP's own requesters list", status, raw)
	}
	if served := s.grantedName(t, created.RequestID); served != "GITHUB_READ" {
		t.Fatalf("the call was served as %s, want GITHUB_READ", served)
	}
	s.take()

	status, raw = s.release(t, pod, *created.GrantID, shadowSessionBody.release)
	answer := decode[oldReleaseAnswer](t, raw)
	if status != http.StatusOK || answer.Airlock != nil || len(answer.Values) != 1 || answer.Values["LEGION_IMPLEMENT_APP"] == "" {
		t.Fatalf("POST values = %d %s, want the one value of LEGION_IMPLEMENT_APP outside the airlock", status, raw)
	}
	if !strings.Contains(string(answer.GitHubApp), `"owner":"cybermonklabs"`) {
		t.Fatalf("the answer's github_app is %s, want the narrowing the call asked for", answer.GitHubApp)
	}
	minted := s.take()
	if len(minted) != 1 || !slices.Equal(minted[0].GitHub.Accounts, []string{"cybermonklabs"}) ||
		!slices.Equal(minted[0].GitHub.Repositories, []string{"cybermonklabs/legion"}) ||
		minted[0].GitHub.Value != githubname.ValueInstallations ||
		!maps.Equal(minted[0].GitHub.Permissions, map[string]string{"actions": "read", "checks": "read", "contents": "read", "metadata": "read", "pull_requests": "read", "statuses": "read"}) {
		t.Fatalf("the mint was %+v, want cybermonklabs/legion in the installations form, cut to the earlier broker's read set", minted)
	}

	off := newGitHubAppFieldServer(t, true)
	offPod := off.legionPod(t, "", "session-approval-shadow-off")
	status, raw = off.create(t, offPod, shadowSessionBody.name, shadowSessionBody.request)
	if reply := decode[wireError](t, raw); status != http.StatusBadRequest || reply.Code != "GITHUB_APP_FIELD_REMOVED" || reply.Error != gitHubAppFieldRemoved {
		t.Errorf("with BROKER_GITHUB_APP_FIELD=off: POST /v1/requests = %d %s, want 400 GITHUB_APP_FIELD_REMOVED", status, raw)
	}
	if minted := off.take(); len(minted) != 0 {
		t.Errorf("with the setting off the minter ran %d times, want none", len(minted))
	}

	other := s.servicePod(t, otherService, otherAccount, "", "session-approval-shadow-other")
	status, raw = s.create(t, other, shadowSessionBody.name, shadowSessionBody.request)
	if reply := decode[wireError](t, raw); status != http.StatusBadRequest || reply.Code != "GITHUB_APP_FIELD_REMOVED" || !strings.Contains(reply.Error, "BROKER_GITHUB_APP_FIELD_REQUESTERS") {
		t.Errorf("from a requester the list does not name: POST /v1/requests = %d %s, want 400 GITHUB_APP_FIELD_REMOVED naming the setting", status, raw)
	}
	if minted := s.take(); len(minted) != 0 {
		t.Errorf("the refused call minted %d times, want none", len(minted))
	}
}

// TestAReadOnlyBodyGetsTheReadSetOfTheEarlierBroker: a read-only body asks for each of the read
// set legion-envoy-v33.7.0 asked GitHub for — actions, checks, contents, metadata, pull_requests
// and statuses — at read where the profile holds it, so a command that reads a pull request, an
// Actions run or a commit status runs as it did. A profile that holds fewer of them asks for
// fewer, as that broker left out what the installation did not hold.
func TestAReadOnlyBodyGetsTheReadSetOfTheEarlierBroker(t *testing.T) {
	t.Parallel()
	s := newGitHubAppFieldServer(t, false)
	pod := s.legionPod(t, "claim-read-set", "")
	status, raw := s.create(t, pod, shadowSessionBody.name, shadowSessionBody.request)
	created := decode[wireCreateRequestResponse](t, raw)
	if status != http.StatusOK || created.GrantID == nil {
		t.Fatalf("POST /v1/requests = %d %s, want a grant", status, raw)
	}
	s.take()
	if status, raw = s.release(t, pod, *created.GrantID, shadowSessionBody.release); status != http.StatusOK {
		t.Fatalf("POST values = %d %s, want 200", status, raw)
	}
	minted := s.take()
	want := map[string]string{"actions": "read", "checks": "read", "contents": "read", "metadata": "read", "pull_requests": "read", "statuses": "read"}
	if len(minted) != 1 || !maps.Equal(minted[0].GitHub.Permissions, want) {
		t.Fatalf("the read-only mint asked for %+v, want %v", minted, want)
	}

	// A profile that holds fewer of them asks for fewer: the box's own App holds no actions,
	// checks or statuses, so its read-only token is contents, metadata and pull_requests at read.
	box := s.box(t, boxPerson)
	body := bodyOf(t, "box read-only")
	status, raw = s.create(t, box, body.name, body.request)
	created = decode[wireCreateRequestResponse](t, raw)
	if status != http.StatusOK || created.GrantID == nil {
		t.Fatalf("POST /v1/requests for the box = %d %s, want a grant", status, raw)
	}
	s.take()
	if status, raw = s.release(t, box, *created.GrantID, body.release); status != http.StatusOK {
		t.Fatalf("POST values for the box = %d %s, want 200", status, raw)
	}
	boxWant := map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"}
	if minted := s.take(); len(minted) != 1 || !maps.Equal(minted[0].GitHub.Permissions, boxWant) {
		t.Fatalf("the box's read-only mint asked for %+v, want %v", minted, boxWant)
	}
}

// TestAnAppTheDeploymentDoesNotNameIsRefused: the field hands a token past the airlock, which the
// broker it comes from did for the Apps its airlock did not hold, so the deployment names the
// stored Apps it may hand over (BROKER_GITHUB_APP_FIELD_SECRETS). A listed pod asking for an App
// outside that list is refused 400 GITHUB_APP_FIELD_REMOVED naming the setting, with nothing
// written and nothing minted, though the profiles file grants it that App's read profile at once
// — which its own --target request still gets, behind the airlock.
func TestAnAppTheDeploymentDoesNotNameIsRefused(t *testing.T) {
	t.Parallel()
	s := newGitHubAppFieldServer(t, false)
	s.machine.GitHubAppFieldSecrets = "GITHUB_APP_ADA"
	pod := s.legionPod(t, "claim-unnamed-app", "")
	status, raw := s.create(t, pod, shadowSessionBody.name, shadowSessionBody.request)
	reply := decode[wireError](t, raw)
	if status != http.StatusBadRequest || reply.Code != "GITHUB_APP_FIELD_REMOVED" || !strings.Contains(reply.Error, "BROKER_GITHUB_APP_FIELD_SECRETS") {
		t.Fatalf("POST /v1/requests = %d %s, want 400 GITHUB_APP_FIELD_REMOVED naming the setting", status, raw)
	}
	if minted := s.take(); len(minted) != 0 {
		t.Fatalf("the refused body minted %d times, want none", len(minted))
	}
	created := s.requestTargeted(t, pod, "GITHUB_READ", "https://github.com/cybermonklabs/legion")
	if status, raw := s.session(t, pod.key, pod.id, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", nil); status != http.StatusOK || !strings.Contains(string(raw), `"airlock"`) {
		t.Fatalf("its own --target release = %d %s, want GITHUB_READ behind the airlock", status, raw)
	}
}

// TestAnAppServedOnlyByOtherNamesWithAProjectTagIsAnswered: a stored App the profiles file serves
// only through profiles of other names holds no entry of its own, so the policy lists no secret
// under its name and reads no dispatch-project tag for it. The profiles of it carry that tag from
// their source, so the choice reads what Dispatch holds for the candidates rather than for the
// stored name: without that read every candidate's evaluation answers policy.ErrNoProjectState
// and the request is refused, which is the case the choice exists to serve. The pod's old body is
// granted the read profile of that App and released, narrowed, as any other is.
func TestAnAppServedOnlyByOtherNamesWithAProjectTagIsAnswered(t *testing.T) {
	t.Parallel()
	access := &projectaccesstest.Swappable{}
	access.Serve(projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PUB": {Visibility: "public", Owner: testApprover},
	}, nil))
	profiles := policytest.ProfilesFile(
		`{"name": "TAGGED_READ", "source": "TAGGED_APP", "tier": "agent", "github": {"accounts": ["acme", "ada"], "permissions": ` + readPermissions + `, "value": "installations"}}`,
	)
	s := &gitHubAppFieldServer{}
	s.testServer = newTestServerWith(t, func(d *api.Deps) {
		local := d.Machine.Secrets.(secrets.AWS).Client.(*secrets.Local)
		app := policytest.TypeSecret("TAGGED_APP", policy.OwnerShared, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
		app.Tags[policy.TagProject] = "PUB"
		local.Put(app)
		loader := policytest.Loader(local, oldClientService)
		loader.Profiles = policytest.Profiles(t, profiles, oldClientService)
		loader.ProjectChecks = true
		current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
		if err != nil {
			t.Fatalf("policy.NewCurrent: %v", err)
		}
		d.Policy, d.Machine.Policy, d.MachineLogin.Policy = current, current, current
		d.Machine.Secrets, d.Machine.Projects = policytest.Store(local), access
		d.Machine.ServicesByAccount = map[string]string{workerAccount: oldClientService}
		d.Machine.GitHubAppFieldRequesters = oldClientService
		d.Machine.GitHubAppFieldSecrets = "TAGGED_APP"
		d.Machine.Minters = map[string]requests.Minter{policy.TypeGitHubApp: s.mint}
	})
	pod := s.legionPod(t, "claim-tagged", "")
	body := strings.ReplaceAll(oldClientBodies[0].request, "LEGION_IMPLEMENT_APP", "TAGGED_APP")
	status, raw := s.create(t, pod, "TAGGED_APP", body)
	created := decode[wireCreateRequestResponse](t, raw)
	if status != http.StatusOK || created.State != "granted" || created.GrantID == nil {
		t.Fatalf("POST /v1/requests = %d %s, want an automatic grant of the App's read profile", status, raw)
	}
	if served := s.grantedName(t, created.RequestID); served != "TAGGED_READ" {
		t.Fatalf("the body was served as %s, want TAGGED_READ", served)
	}
	s.take()
	release := strings.ReplaceAll(oldClientBodies[0].release, "LEGION_IMPLEMENT_APP", "TAGGED_APP")
	if status, raw = s.release(t, pod, *created.GrantID, release); status != http.StatusOK {
		t.Fatalf("POST values = %d %s, want 200", status, raw)
	}
	if answer := decode[oldReleaseAnswer](t, raw); answer.Values["TAGGED_APP"] == "" {
		t.Fatalf("the answer %s holds no value for TAGGED_APP", raw)
	}
	if minted := s.take(); len(minted) != 1 || !slices.Equal(minted[0].GitHub.Repositories, []string{"acme/widgets"}) {
		t.Fatalf("the mint was %+v, want acme/widgets", minted)
	}
}

// TestTakingAnAppOutOfTheListRefusesItsReleases: the one gate reads the deployment's list of Apps
// at every release too, not at the request alone, so an operator who takes an App out of
// BROKER_GITHUB_APP_FIELD_SECRETS stops every narrowed release of it at once — the rollback step
// of "Earlier clients' github_app field" in operate.md. A grant made while its App was listed is
// refused 400 GITHUB_APP_FIELD_REMOVED naming the setting once it is not, with nothing released
// and nothing minted, and releases again when the App is listed again.
func TestTakingAnAppOutOfTheListRefusesItsReleases(t *testing.T) {
	t.Parallel()
	s := newGitHubAppFieldServer(t, false)
	pod := s.legionPod(t, "claim-rollback", "")
	body := bodyOf(t, "pod read-only")
	status, raw := s.create(t, pod, body.name, body.request)
	created := decode[wireCreateRequestResponse](t, raw)
	if status != http.StatusOK || created.GrantID == nil {
		t.Fatalf("POST /v1/requests = %d %s, want a grant", status, raw)
	}
	s.take()

	s.machine.GitHubAppFieldSecrets = "GITHUB_APP_ADA"
	status, raw = s.release(t, pod, *created.GrantID, body.release)
	reply := decode[wireError](t, raw)
	if status != http.StatusBadRequest || reply.Code != "GITHUB_APP_FIELD_REMOVED" ||
		!strings.Contains(reply.Error, "BROKER_GITHUB_APP_FIELD_SECRETS") || strings.Contains(string(raw), `"values"`) {
		t.Fatalf("the release of a grant whose App was taken out = %d %s, want 400 GITHUB_APP_FIELD_REMOVED naming the setting and no value", status, raw)
	}
	if minted := s.take(); len(minted) != 0 {
		t.Fatalf("the refused release minted %d times, want none", len(minted))
	}

	s.machine.GitHubAppFieldSecrets = "LEGION_IMPLEMENT_APP,GITHUB_APP_ADA"
	if status, raw = s.release(t, pod, *created.GrantID, body.release); status != http.StatusOK {
		t.Fatalf("the release once the App is listed again = %d %s, want 200", status, raw)
	}
	if minted := s.take(); len(minted) != 1 {
		t.Fatalf("that release minted %d times, want once", len(minted))
	}
}
