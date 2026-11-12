// packages/envoy/internal/broker/api/narrowing_request_test.go
// POST /v1/requests takes the narrowing its grant will be released with, and answers each one
// the broker will not serve before it opens the approval request a person decides. This file is
// the status and the code a session reads for each of those refusals, over the real route, with
// the broker's own github-app minter pointed at the GitHub stand-in: what the refusals are and
// why is requests/narrowing_check_test.go's subject, and what a client does with each status is
// settled by the status alone.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
)

// The accounts the stand-in's App is installed on, both of them in the github-owners tag
// policytest gives a github-app secret: narrowingOwner is the account it holds an installation
// for and narrowingUninstalled the one it does not.
const (
	narrowingOwner       = "ada"
	narrowingUninstalled = "acme"
)

// narrowingRequestServer is a broker whose github-app minter is the real one, reaching the
// GitHub stand-in rather than GitHub, with one live session and two human-tier secrets its
// person approves: NARROW_APP_KEY, a github-app secret, and DEEL_API_KEY, newTestServer's own
// stored secret, which is the request that asks for no github-app secret at all.
type narrowingRequestServer struct {
	*testServer
	key          *ecdsa.PrivateKey
	enrollmentID string
	stand        *airlocktest.GitHubStand
}

// newNarrowingRequestServer builds one whose stand-in answers each token request with answer. An
// airlocked server holds NARROW_APP_KEY behind the airlock, as a rules file does.
func newNarrowingRequestServer(t *testing.T, answer airlocktest.GitHubAnswer, airlocked bool) *narrowingRequestServer {
	t.Helper()
	service, err := airlocktest.NewService(airlocktest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatalf("start the GitHub stand-in: %v", err)
	}
	t.Cleanup(service.Close)
	stand := airlocktest.NewGitHubStand(airlocktest.GitHubOptions{
		Installations: map[string]int64{narrowingOwner: 7}, Answer: answer,
	})
	service.AnswerWith(stand.Answer)

	s := &narrowingRequestServer{stand: stand}
	s.testServer = newTestServerWith(t, func(d *api.Deps) {
		d.Machine.Minters = map[string]requests.Minter{policy.KindGitHubApp: secrets.GitHubApp{Base: service.URL()}.Mint}
		if airlocked {
			d.Machine.Airlock = airlock.Rules{Secrets: []airlock.SecretRule{{Name: "NARROW_APP_KEY", Hosts: []string{"api.example.com"}}}}
		}
	})
	s.Secrets.Put(policytest.KindSecret("NARROW_APP_KEY", testApprover, policy.TierHuman, policy.KindGitHubApp, standInAppValue(t)))
	if status, body := s.req(t, http.MethodPost, "/v1/secrets/NARROW_APP_KEY/reread", nil, nil); status != http.StatusOK {
		t.Fatalf("reread NARROW_APP_KEY = %d %s", status, body)
	}
	s.enrollmentID, s.key = s.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	return s
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

// request asks for name with the github_app body, as a session does, and answers the status and
// the raw body.
func (s *narrowingRequestServer) request(t *testing.T, narrowing map[string]any, name string) (int, []byte) {
	t.Helper()
	body := map[string]any{"request": signAgentSecretRequest(t, s.key, s.URL, "read the App key", name)}
	if narrowing != nil {
		body["github_app"] = narrowing
	}
	return s.session(t, s.key, s.enrollmentID, http.MethodPost, "/v1/requests", body)
}

// pendingRows counts the request rows and credential-request records the broker holds, which a
// refusal before the approval request leaves at none.
func (s *narrowingRequestServer) pendingRows(t *testing.T) (requestRows, records int) {
	t.Helper()
	if err := s.Store.Pool.QueryRow(context.Background(),
		`select (select count(*) from requests), (select count(*) from credential_requests)`).Scan(&requestRows, &records); err != nil {
		t.Fatalf("count what the broker holds: %v", err)
	}
	return requestRows, records
}

// TestNarrowingBeforeApprovalAnswersItsOwnStatusAndCode: each narrowing POST /v1/requests will
// not serve is answered with the status and code of its own cause — the caller's own input, the
// account the secret does not serve, the name the airlock holds, GitHub's refusal — and none of
// them opens the approval request a person would have had to decide.
func TestNarrowingBeforeApprovalAnswersItsOwnStatusAndCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		// answer is what the stand-in answers each token request with.
		answer airlocktest.GitHubAnswer
		// airlocked holds NARROW_APP_KEY behind the airlock.
		airlocked bool
		secret    string
		narrowing map[string]any
		status    int
		code      string
		mentions  string
	}{
		{
			name:      "a narrowing the parser refuses",
			secret:    "NARROW_APP_KEY",
			narrowing: map[string]any{"owner": narrowingOwner, "repositories": []string{narrowingOwner + "/proofs"}},
			status:    http.StatusBadRequest,
			code:      "INVALID_NARROWING",
			mentions:  "github_app.repositories",
		},
		{
			name:      "a request that asks for no github-app secret",
			secret:    "DEEL_API_KEY",
			narrowing: map[string]any{"owner": narrowingOwner, "read_only": true},
			status:    http.StatusBadRequest,
			code:      "NARROWING_NOT_APPLICABLE",
			mentions:  "github-app secret",
		},
		{
			name:      "an account the github-owners tag does not list",
			secret:    "NARROW_APP_KEY",
			narrowing: map[string]any{"owner": "outsider", "read_only": true},
			status:    http.StatusForbidden,
			code:      "NARROWING_OWNER_NOT_LISTED",
			mentions:  "outsider",
		},
		{
			name:      "a requested name the airlock holds back",
			airlocked: true,
			secret:    "NARROW_APP_KEY",
			narrowing: map[string]any{"owner": narrowingOwner, "read_only": true},
			status:    http.StatusConflict,
			code:      "NARROWING_BEHIND_AIRLOCK",
			mentions:  "NARROW_APP_KEY",
		},
		{
			name:      "a repository the installation does not reach",
			answer:    airlocktest.GitHubAnswer{Mode: airlocktest.GitHubTokensRefusedRepository, Repository: "missing"},
			secret:    "NARROW_APP_KEY",
			narrowing: map[string]any{"owner": narrowingOwner, "repositories": []string{"missing"}},
			status:    http.StatusUnprocessableEntity,
			code:      "NARROWING_REFUSED",
			mentions:  "missing",
		},
		{
			name:      "the read access the installation does not hold",
			answer:    airlocktest.GitHubAnswer{Mode: airlocktest.GitHubTokensRefusedPermissions},
			secret:    "NARROW_APP_KEY",
			narrowing: map[string]any{"owner": narrowingOwner, "read_only": true},
			status:    http.StatusUnprocessableEntity,
			code:      "NARROWING_REFUSED",
			mentions:  "read access",
		},
		{
			// An App with no installation on the account reads as the narrowing's own refusal
			// here: the check reached GitHub and learned which account has none.
			name:      "an account the App has no installation on",
			secret:    "NARROW_APP_KEY",
			narrowing: map[string]any{"owner": narrowingUninstalled, "read_only": true},
			status:    http.StatusUnprocessableEntity,
			code:      "NARROWING_REFUSED",
			mentions:  "has no installation on",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newNarrowingRequestServer(t, tc.answer, tc.airlocked)

			status, body := s.request(t, tc.narrowing, tc.secret)
			reply := decode[wireError](t, body)
			if status != tc.status || reply.Code != tc.code || !strings.Contains(reply.Error, tc.mentions) {
				t.Fatalf("POST /v1/requests = %d %s; want %d %s naming %q", status, body, tc.status, tc.code, tc.mentions)
			}
			if rows, records := s.pendingRows(t); rows != 0 || records != 0 {
				t.Fatalf("the refused request left %d request rows and %d records; want none of either: nobody is asked to approve a narrowing the broker will not serve", rows, records)
			}
		})
	}
}

// TestNarrowingBeforeApprovalOpensTheApprovalRequestForAServableNarrowing is the control of the
// refusals above: a narrowing the policy allows and GitHub would mint is answered exactly as a
// request with no narrowing is — the pending request, its record and the page its approver
// decides it on.
func TestNarrowingBeforeApprovalOpensTheApprovalRequestForAServableNarrowing(t *testing.T) {
	s := newNarrowingRequestServer(t, airlocktest.GitHubAnswer{}, false)

	status, body := s.request(t, map[string]any{"owner": narrowingOwner, "repositories": []string{"proofs"}, "read_only": true}, "NARROW_APP_KEY")
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests narrowed to one GitHub serves = %d %s; want 200", status, body)
	}
	created := decode[wireCreateRequestResponse](t, body)
	if created.State != "pending" || created.RecordID == nil || created.ApproveURL == nil {
		t.Fatalf("POST /v1/requests = %s; want it pending on a record with the page its approver decides it on", body)
	}
	if rows, records := s.pendingRows(t); rows != 1 || records != 1 {
		t.Fatalf("the served narrowing left %d request rows and %d records; want the one pending request and its one record", rows, records)
	}
}
