package api

import (
	"net/http"
	"strings"
	"testing"

	gws "github.com/gorilla/websocket"
)

// The two Kubernetes service accounts the allow-list test signs tokens for: one the deployment
// names in DISPATCH_OIDC_ACCOUNTS, and one it does not. Both are verified tokens of the same
// issuer and audience, so only the list tells them apart.
const (
	listedServiceAccount   = "system:serviceaccount:dispatch:envoy-broker"
	unlistedServiceAccount = "system:serviceaccount:legion:legion-worker"
)

// TestOnlyListedServiceAccountsSignIn (c-service-accounts) holds R13: a deployment that names the
// service accounts it admits refuses every other account's verified token before it reads or
// writes anything, on a read, on the live socket and on a write; and a deployment that names
// none admits every verified account, as it did before the list existed. Each refusal is 401
// SERVICE_ACCOUNT_NOT_ADMITTED naming the subject, and the events table does not move under the
// refused write.
func TestOnlyListedServiceAccountsSignIn(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	token := func(subject string) string {
		return issuer.Mint(t, issuer.signing, issuer.Claims(subject, serviceTokenAudience))
	}

	for _, deployment := range []struct {
		name     string
		accounts []string
		admitted map[string]bool
	}{
		{"one account listed", []string{listedServiceAccount}, map[string]bool{listedServiceAccount: true, unlistedServiceAccount: false}},
		{"no account listed", nil, map[string]bool{listedServiceAccount: true, unlistedServiceAccount: true}},
	} {
		t.Run(deployment.name, func(t *testing.T) {
			rig := newLiveSocketRig(t, testServerOptions{oidc: issuer.verifier(t), oidcAccounts: deployment.accounts}, nil)
			seedServiceTokenIssue(t, rig.handler)

			for _, subject := range []string{listedServiceAccount, unlistedServiceAccount} {
				bearer := token(subject)
				admitted := deployment.admitted[subject]
				eventsBefore := countRows(t, rig.database, `select count(*) from events`)

				read := agentRequest(t, rig.handler, http.MethodGet, "/api/v1/issues", nil, bearer)
				write := agentRequest(t, rig.handler, http.MethodPost, "/api/v1/issues", map[string]any{
					"project": "TEST", "title": "Written by " + subject,
					"actor": map[string]any{"kind": "session", "id": "ses_service_account"},
				}, bearer)
				conn, upgrade, dialErr := gws.DefaultDialer.Dial(rig.url, withBearer(bearer))
				if conn != nil {
					_ = conn.Close()
				}

				if admitted {
					if read.Code != http.StatusOK {
						t.Errorf("%s: GET /api/v1/issues = %d %s, want 200: the deployment admits this account", subject, read.Code, read.Body.String())
					}
					if write.Code != http.StatusCreated {
						t.Errorf("%s: POST /api/v1/issues = %d %s, want 201: the deployment admits this account", subject, write.Code, write.Body.String())
					}
					if dialErr != nil {
						t.Errorf("%s: GET /ws/live refused (%v, %s), want the upgrade: the deployment admits this account", subject, dialErr, statusOf(upgrade))
					}
					continue
				}

				for _, refused := range []struct {
					route  string
					status int
					body   string
				}{
					{"GET /api/v1/issues", read.Code, read.Body.String()},
					{"POST /api/v1/issues", write.Code, write.Body.String()},
				} {
					if refused.status != http.StatusUnauthorized || !strings.Contains(refused.body, `"code":"SERVICE_ACCOUNT_NOT_ADMITTED"`) ||
						!strings.Contains(refused.body, subject) {
						t.Errorf("%s: %s = %d %s, want 401 SERVICE_ACCOUNT_NOT_ADMITTED naming the account: the deployment lists only %v",
							subject, refused.route, refused.status, refused.body, deployment.accounts)
					}
				}
				if dialErr == nil || upgrade == nil || upgrade.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s: GET /ws/live = %v (%s), want the upgrade refused 401: the deployment lists only %v",
						subject, dialErr, statusOf(upgrade), deployment.accounts)
				}
				if after := countRows(t, rig.database, `select count(*) from events`); after != eventsBefore {
					t.Errorf("%s: the events table moved from %d to %d rows under a refused account's write", subject, eventsBefore, after)
				}
			}
		})
	}
}

// statusOf names a refused upgrade's status, or says the dial had no response at all.
func statusOf(response *http.Response) string {
	if response == nil {
		return "no response"
	}
	return response.Status
}
