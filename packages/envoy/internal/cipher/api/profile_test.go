package api_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// gitHubAppFieldRemoved is the message C-5 gives the refusal of a body carrying github_app.
const gitHubAppFieldRemoved = "github_app was removed: name a profile and narrow it with --target ACCOUNT[/REPOSITORY]"

// profileServer is a test broker whose github-app minter answers what mint answers, with one
// session of testApprover and two github-app secrets, each served through a profile of its own
// name: APP_KEY, agent tier, which the session is granted at once, and HUMAN_APP_KEY, human tier,
// which testApprover decides and whose profile therefore gets the check mint before anybody is
// asked.
type profileServer struct {
	*testServer
	key          *ecdsa.PrivateKey
	enrollmentID string

	mu     sync.Mutex
	minted []secrets.MintRequest
	mint   func(secrets.MintRequest) (secrets.Minted, error)
}

func newProfileServer(t *testing.T, mint func(secrets.MintRequest) (secrets.Minted, error)) *profileServer {
	t.Helper()
	return newProfileServerOf(t, policytest.ProfilesFile(policytest.AppProfile("APP_KEY"), policytest.AppProfile("HUMAN_APP_KEY")), mint, appSources(), "APP_KEY", "HUMAN_APP_KEY")
}

// appSources are the two github-app secrets of a profileServer: APP_KEY, agent tier, and
// HUMAN_APP_KEY, human tier, both testApprover's.
func appSources() []secrets.LocalSecret {
	return []secrets.LocalSecret{
		policytest.TypeSecret("APP_KEY", testApprover, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)),
		policytest.TypeSecret("HUMAN_APP_KEY", testApprover, policy.TierHuman, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)),
	}
}

// newProfileServerOf is newProfileServer whose deployment's profiles file is file and whose
// store holds sources, which the broker serves once each name of reread is reread, as a
// session's reread makes the broker serve a secret stored after its last load.
func newProfileServerOf(t *testing.T, file string, mint func(secrets.MintRequest) (secrets.Minted, error), sources []secrets.LocalSecret, reread ...string) *profileServer {
	t.Helper()
	return newProfileServerAdjusted(t, file, mint, sources, func(*api.Deps) {}, reread...)
}

// newProfileServerAdjusted is newProfileServerOf with adjust applied to the deps it registers.
func newProfileServerAdjusted(t *testing.T, file string, mint func(secrets.MintRequest) (secrets.Minted, error), sources []secrets.LocalSecret, adjust func(*api.Deps), reread ...string) *profileServer {
	t.Helper()
	s := &profileServer{mint: mint}
	s.testServer = newProfiledTestServer(t, file, func(d *api.Deps) {
		adjust(d)
		d.Machine.Minters = map[string]requests.Minter{
			policy.TypeGitHubApp: func(_ context.Context, req secrets.MintRequest) (secrets.Minted, error) {
				s.mu.Lock()
				s.minted = append(s.minted, req)
				mint := s.mint
				s.mu.Unlock()
				return mint(req)
			},
		}
	})
	for _, source := range sources {
		s.Secrets.Put(source)
	}
	for _, name := range reread {
		if status, body := s.sessionReread(t, name); status != http.StatusOK {
			t.Fatalf("reread %s = %d %s", name, status, body)
		}
	}
	s.enrollmentID, s.key = s.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	return s
}

// setMint makes the minter answer what mint answers from the next mint on. The minter runs on
// the server's goroutine, so the answer is swapped under the lock it reads it under.
func (s *profileServer) setMint(mint func(secrets.MintRequest) (secrets.Minted, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mint = mint
}

// refusing answers every mint with err.
func refusing(err error) func(secrets.MintRequest) (secrets.Minted, error) {
	return func(secrets.MintRequest) (secrets.Minted, error) { return secrets.Minted{}, err }
}

// take returns the requests the minter was handed since the last take, and forgets them.
func (s *profileServer) take() []secrets.MintRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	minted := s.minted
	s.minted = nil
	return minted
}

// request posts body to POST /v1/requests as the session, with a signed request object for name
// under "request" unless body already holds one.
func (s *profileServer) request(t *testing.T, name string, body map[string]any) (int, []byte) {
	t.Helper()
	if _, set := body["request"]; !set {
		body["request"] = signAgentSecretRequest(t, s.key, s.URL, "read the App key", name)
	}
	return s.session(t, s.key, s.enrollmentID, http.MethodPost, "/v1/requests", body)
}

// grant requests APP_KEY for the session and answers the automatic grant's id.
func (s *profileServer) grant(t *testing.T) string {
	t.Helper()
	created := s.requestAs(t, s.key, s.enrollmentID, "APP_KEY")
	if created.GrantID == nil {
		t.Fatalf("POST /v1/requests for APP_KEY = %+v, want an automatic grant", created)
	}
	return *created.GrantID
}

// values releases grantID with body (nil for none) and answers the status and raw answer.
func (s *profileServer) values(t *testing.T, grantID string, body any) (int, []byte) {
	t.Helper()
	return s.session(t, s.key, s.enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/values", body)
}

// pendingRows counts the request rows and credential-request records the broker holds.
func (s *profileServer) pendingRows(t *testing.T) (requestRows, records int) {
	t.Helper()
	if err := s.Store.Pool.QueryRow(context.Background(),
		`select (select count(*) from requests), (select count(*) from credential_requests)`).Scan(&requestRows, &records); err != nil {
		t.Fatalf("count the request rows: %v", err)
	}
	return requestRows, records
}

// minting answers every mint with the value token and nothing left out.
func minting(secrets.MintRequest) (secrets.Minted, error) {
	return secrets.Minted{Value: "token"}, nil
}

// TestTheGitHubAppFieldIsRefusedWithItsOwnCode: with BROKER_GITHUB_APP_FIELD=off, a body of POST
// /v1/requests or of POST /v1/grants/{id}/values that carries github_app — any value of it, null
// too — and no field the route does not take is refused 400 GITHUB_APP_FIELD_REMOVED with C-5's
// message, and never INVALID_REQUEST, which a command line of an earlier release reads as a broker
// that does not know the field and answers by sending its request again without it. Neither route
// writes a request or runs a minter for it. A body that also carries a field the route does not
// take is refused at that field first, as the route's own unknown field, since the route reads the
// body once: no released command line sends github_app beside such a field, so none of them reads
// that refusal as a broker from before github_app. A body that carries no github_app keeps its own
// route's refusal of a field it does not take. github_app_field_test.go holds the answer the field
// gets with the setting unset.
func TestTheGitHubAppFieldIsRefusedWithItsOwnCode(t *testing.T) {
	t.Parallel()
	s := newProfileServerAdjusted(t, policytest.ProfilesFile(policytest.AppProfile("APP_KEY"), policytest.AppProfile("HUMAN_APP_KEY")), minting, appSources(),
		func(d *api.Deps) { d.Machine.GitHubAppFieldOff = true }, "APP_KEY", "HUMAN_APP_KEY")
	grantID := s.grant(t)
	s.take()

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"a narrowing", map[string]any{"github_app": map[string]any{"owner": "ada", "read_only": true}}},
		// encoding/json hands a JSON null to a json.RawMessage field as the bytes "null", so a
		// null is a github_app the route carries like any other value.
		{"null", map[string]any{"github_app": nil}},
		{"a value of no narrowing's shape", map[string]any{"github_app": "ada"}},
	} {
		t.Run("POST /v1/requests, "+tc.name, func(t *testing.T) {
			status, body := s.request(t, "HUMAN_APP_KEY", tc.body)
			reply := decode[wireError](t, body)
			if status != http.StatusBadRequest || reply.Code != "GITHUB_APP_FIELD_REMOVED" || reply.Error != gitHubAppFieldRemoved {
				t.Fatalf("POST /v1/requests = %d %s, want 400 GITHUB_APP_FIELD_REMOVED: %s", status, body, gitHubAppFieldRemoved)
			}
			if rows, records := s.pendingRows(t); rows != 1 || records != 0 {
				t.Fatalf("the refused body left %d request rows and %d records; want the one grant's request and no record", rows, records)
			}
		})
		t.Run("POST /v1/grants/{id}/values, "+tc.name, func(t *testing.T) {
			delete(tc.body, "request")
			status, body := s.values(t, grantID, tc.body)
			reply := decode[wireError](t, body)
			if status != http.StatusBadRequest || reply.Code != "GITHUB_APP_FIELD_REMOVED" || reply.Error != gitHubAppFieldRemoved || strings.Contains(string(body), `"values"`) {
				t.Fatalf("POST values = %d %s, want 400 GITHUB_APP_FIELD_REMOVED and no value", status, body)
			}
		})
	}
	if minted := s.take(); len(minted) != 0 {
		t.Fatalf("the minter ran %d times for bodies carrying github_app, want none", len(minted))
	}

	// The accepted trade-off of the one read: github_app beside a field the route does not take
	// is that route's unknown field, as the same field is without github_app.
	for _, body := range []map[string]any{
		{"github_app": map[string]any{"owner": "ada"}, "narrowing": true},
		{"narrowing": true},
	} {
		status, answer := s.request(t, "HUMAN_APP_KEY", body)
		if reply := decode[wireError](t, answer); status != http.StatusBadRequest || reply.Code != "INVALID_REQUEST" || !strings.Contains(reply.Error, "narrowing") {
			t.Fatalf("POST /v1/requests with %v = %d %s; want 400 INVALID_REQUEST naming the field the route does not take", body, status, answer)
		}
		delete(body, "request")
		status, answer = s.values(t, grantID, body)
		if reply := decode[wireError](t, answer); status != http.StatusBadRequest || reply.Code != "INVALID_RELEASE" || !strings.Contains(reply.Error, "narrowing") || strings.Contains(string(answer), `"values"`) {
			t.Fatalf("POST values with %v = %d %s; want 400 INVALID_RELEASE naming the field the route does not take, and no value", body, status, answer)
		}
	}
	if rows, records := s.pendingRows(t); rows != 1 || records != 0 {
		t.Fatalf("the refused bodies left %d request rows and %d records; want the one grant's request and no record", rows, records)
	}
	if minted := s.take(); len(minted) != 0 {
		t.Fatalf("the minter ran %d times for refused bodies, want none", len(minted))
	}
}

// TestAProfileMintRefusalIs422ProfileMintRefused: a profile's scope the minter refuses
// (secrets.ErrMintNarrowingRefused: an empty cut, or GitHub's 422) is answered 422
// PROFILE_MINT_REFUSED on both routes: at the release, with nothing released, never the 502
// MINT_FAILED of an App that failed; and at POST /v1/requests, where the check mint refuses it
// before anybody is asked, as it refuses an account with no installation
// (secrets.ErrMintNoInstallation). Each is logged at WARN as a refused scope, never as the
// operator's mint failure, beside the record of the refusal the answer recorder writes for it.
func TestAProfileMintRefusalIs422ProfileMintRefused(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (policytest.CaptureLog, log.SetFlags).
	logged := policytest.CaptureLog(t)
	s := newProfileServer(t, minting)
	grantID := s.grant(t)
	s.take()

	s.setMint(refusing(fmt.Errorf("%w: GitHub answered HTTP 422 to the profile's token for ada: likely a repository it names (missing) that the installation does not reach", secrets.ErrMintNarrowingRefused)))
	status, body := s.values(t, grantID, nil)
	reply := decode[wireError](t, body)
	if status != http.StatusUnprocessableEntity || reply.Code != "PROFILE_MINT_REFUSED" || !strings.Contains(reply.Error, "missing") || strings.Contains(string(body), `"values"`) {
		t.Fatalf("POST values = %d %s, want 422 PROFILE_MINT_REFUSED naming the repository and no value", status, body)
	}
	s.take()

	for _, tc := range []struct {
		name     string
		refusal  error
		mentions string
	}{
		{"an empty cut", fmt.Errorf("%w: github app 123456: its installation on ada holds none of administration: write", secrets.ErrMintEmptyCut), "administration: write"},
		{"an account with no installation", fmt.Errorf("%w: github app 123456 has no installation on ada", secrets.ErrMintNoInstallation), "has no installation on ada"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.setMint(refusing(tc.refusal))
			status, body := s.request(t, "HUMAN_APP_KEY", map[string]any{})
			reply := decode[wireError](t, body)
			if status != http.StatusUnprocessableEntity || reply.Code != "PROFILE_MINT_REFUSED" || !strings.Contains(reply.Error, tc.mentions) || !strings.Contains(reply.Error, "HUMAN_APP_KEY") {
				t.Fatalf("POST /v1/requests = %d %s, want 422 PROFILE_MINT_REFUSED naming HUMAN_APP_KEY and %q", status, body, tc.mentions)
			}
			if minted := s.take(); len(minted) != 1 || !minted[0].Check || minted[0].Name != "HUMAN_APP_KEY" {
				t.Fatalf("the minter was asked %d times; want the one check mint of HUMAN_APP_KEY", len(minted))
			}
			if rows, records := s.pendingRows(t); rows != 1 || records != 0 {
				t.Fatalf("the refused request left %d request rows and %d records; want only the grant's request and no record", rows, records)
			}
		})
	}
	if strings.Contains(logged.String(), requests.MintFailedMessage) {
		t.Fatalf("a refused scope was logged as a mint failure: %s", logged.String())
	}
	recorded := 0
	for line := range strings.Lines(logged.String()) {
		if strings.Contains(line, "WARN "+answer.RefusalMessage) && strings.Contains(line, "code=PROFILE_MINT_REFUSED") {
			recorded++
		}
	}
	if recorded != 3 {
		t.Fatalf("logged %d %q records of PROFILE_MINT_REFUSED, want 3: %s", recorded, answer.RefusalMessage, logged.String())
	}
}

// TestTheValuesAnswerNamesWhatTheCutLeftOut: the values answer carries github_left_out, for each
// profile and account, every entry the cut of the profile's permissions to what the installation
// holds left out, and no github_app; a release whose cut left nothing out carries neither key.
func TestTheValuesAnswerNamesWhatTheCutLeftOut(t *testing.T) {
	t.Parallel()
	s := newProfileServer(t, minting)
	grantID := s.grant(t)

	s.setMint(func(secrets.MintRequest) (secrets.Minted, error) {
		return secrets.Minted{Value: "token", LeftOut: map[string][]string{"ada": {"contents:write", "members"}}}, nil
	})
	status, body := s.values(t, grantID, nil)
	if status != http.StatusOK {
		t.Fatalf("POST values = %d %s, want 200", status, body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	var got map[string]map[string][]string
	if err := json.Unmarshal(raw["github_left_out"], &got); err != nil {
		t.Fatalf("github_left_out %s: %v", raw["github_left_out"], err)
	}
	if want := map[string]map[string][]string{"APP_KEY": {"ada": {"contents:write", "members"}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("github_left_out = %v, want %v", got, want)
	}
	if _, set := raw["github_app"]; set {
		t.Fatalf("the values answer holds github_app: %s", body)
	}

	s.setMint(minting)
	status, body = s.values(t, grantID, nil)
	raw = nil
	if err := json.Unmarshal(body, &raw); status != http.StatusOK || err != nil {
		t.Fatalf("POST values = %d %s (%v), want 200", status, body, err)
	}
	for _, key := range []string{"github_left_out", "github_app"} {
		if _, set := raw[key]; set {
			t.Fatalf("a release whose cut left nothing out holds %s: %s", key, body)
		}
	}
}

// TestAReleaseForAnAccountTheAppHasNoInstallationOnIsAMintFailure: a release whose mint finds no
// installation of the App on the profile's accounts answers 502 MINT_FAILED with the minter's
// words, as the release always has, and not the 422 PROFILE_MINT_REFUSED the request route answers
// for the same cause before the approval: the release wraps that refusal in requests.ErrMintFailed
// and still carries secrets.ErrMintNoInstallation, which only createRequest answers 422.
func TestAReleaseForAnAccountTheAppHasNoInstallationOnIsAMintFailure(t *testing.T) {
	t.Parallel()
	s := newProfileServer(t, minting)
	grantID := s.grant(t)
	s.setMint(refusing(fmt.Errorf("%w: github app 123456 has no installation on ada", secrets.ErrMintNoInstallation)))
	status, body := s.values(t, grantID, nil)
	reply := decode[wireError](t, body)
	if status != http.StatusBadGateway || reply.Code != "MINT_FAILED" || !strings.Contains(reply.Error, "has no installation on ada") || strings.Contains(string(body), `"values"`) {
		t.Fatalf("POST values = %d %s, want 502 MINT_FAILED naming the account with no installation and no value", status, body)
	}
}
