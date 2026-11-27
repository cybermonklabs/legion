// cmd/cipherd/second_prefix_test.go
// Rows against REAL compiled brokers on one Postgres, behind one public URL as the pods of one
// Deployment are, while a deployment moves its Secret Manager store to another prefix: grants made
// by a broker on the old prefix are released, reused and connected through a broker on the new one
// whose BROKER_SECRETS_SECOND_PREFIX is the old one, which reads each value from the new store
// alone; with the setting empty, the same grants end, as they did before it.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest/podclient"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The two Secret Manager prefixes a move goes between, in one placeholder project, and the Cloud
// KMS key every secret of both is on.
const (
	moveOldPrefix  = "projects/000000000000/secrets/move-old-"
	moveNewPrefix  = "projects/000000000000/secrets/move-new-"
	moveKMSKeyName = "projects/example-project/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
)

// The rows' service, its accounts, the pod audience their brokers verify, the host the profile's
// proxy block binds, and the names they ask for: MOVE_TOKEN, the service's stored agent-tier
// secret, granted at once; MOVE_APP_READ, a profile of ada's human-tier github-app secret
// MOVE_APP behind the airlock, which ada approves; MOVE_ONLY_OLD, held under the old prefix alone
// in the new broker's store, which no grant holds; and MOVE_FRESH, which only the new broker's
// store holds, a new request for which freezes the new prefix.
const (
	movePodAudience  = "example-broker-pod"
	moveService      = "example-service"
	moveLoginAccount = "system:serviceaccount:example-system:example-service-login"
	movePodAccount   = "system:serviceaccount:example:example-worker"
	moveHost         = "move.airlock.test"
	moveToken        = "MOVE_TOKEN"
	moveApp          = "MOVE_APP"
	moveAppRead      = "MOVE_APP_READ"
	moveOnlyOld      = "MOVE_ONLY_OLD"
	moveFresh        = "MOVE_FRESH"
)

// TestSecondPrefixCarriesGrantsAcrossAStoreMove: every step starts a broker on one prefix and gets
// two grants released through it - MOVE_TOKEN's, a stored agent-tier secret, and MOVE_APP_READ's,
// a human-tier github-app profile behind the airlock that ada approves on the approval page - and
// then starts a second broker on the same database and the same public URL, whose own store holds
// those secrets under its prefix and nothing of them under the first's.
//
//  1. From the old prefix to the new one, with BROKER_SECRETS_SECOND_PREFIX the old prefix: the
//     second broker releases both grants again, each value read from its own store; the proxy it
//     runs reads the profile's grant on an airlock connection; and a Create for the same names
//     answers the same grants, writing no request. Neither grant ends.
//  2. The same with the setting empty: the second broker's first release of each grant ends it
//     for good, as every broker before the setting does.
//  3. The move back, from the new prefix to the old one with the second prefix the new one: as 1.
//
// In each step the second broker serves nothing else of the old prefix: a session's reread of
// MOVE_ONLY_OLD, which its store holds under the old prefix alone, answers it not served, an ask
// for it is refused UNKNOWN_SECRET, GET /v1/settings names the configured prefix alone, and a new
// request freezes a source under the configured prefix. It changes no request_secrets row's name or
// source. Every source a row freezes is a Secret Manager resource name, which the fake secrets
// file answers under gcp-secret-manager as Secret Manager does.
//
// The named reverts: compare a frozen source with the policy's by string alone, and step 1's
// second broker ends both grants; release the frozen source, and its releases are refused
// SECRET_NOT_IN_STORE; count a source under any prefix with the same remainder, and step 2's
// grants go on; and a fake secrets file that answers local ARNs fails each step's first read of
// the frozen sources.
//
// It skips when BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestSecondPrefixCarriesGrantsAcrossAStoreMove(t *testing.T) {
	t.Parallel()
	databaseURLs := []string{storetest.URL(t), storetest.URL(t), storetest.URL(t)}
	binary := buildBroker(t)
	for i, step := range []struct {
		name             string
		from, to, second string
		carries          bool
	}{
		{"1_from_the_old_prefix_to_the_new_with_the_old_as_second", moveOldPrefix, moveNewPrefix, moveOldPrefix, true},
		{"2_the_same_with_the_setting_empty", moveOldPrefix, moveNewPrefix, "", false},
		{"3_from_the_new_prefix_back_to_the_old_with_the_new_as_second", moveNewPrefix, moveOldPrefix, moveNewPrefix, true},
	} {
		t.Run(step.name, func(t *testing.T) {
			m := newStoreMove(t, binary, databaseURLs[i])
			m.move(step.from, step.to, step.second, step.carries)
		})
	}
}

// storeMove is one step's rig: a local issuer standing in for the cluster's token issuer and for
// the person's sign-in provider, a GitHub stand-in the github-app profile mints at, one airlock
// authority and one profiles file every broker of the step shares, a database of its own, and
// one public URL in front of whichever broker the step routes it to.
type storeMove struct {
	t                   *testing.T
	binary, databaseURL string
	issuer              *oidctest.Issuer
	clientFile          string
	certFile, keyFile   string
	profiles            string
	github              *proxyrulestest.Service
	front               *front
	db                  *pgx.Conn
	pods                podclient.Pods
}

func newStoreMove(t *testing.T, binary, databaseURL string) *storeMove {
	t.Helper()
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "pod-key")
	ciphertest.SignIn(issuer, issuer.PublishKey(t, "signin-key"))
	github, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatalf("start the GitHub stand-in: %v", err)
	}
	t.Cleanup(github.Close)
	github.AnswerWith(proxyrulestest.NewGitHubStand(proxyrulestest.GitHubOptions{Installations: map[string]int64{"acme-org": 7}}).Answer)
	certFile, keyFile := airlockCAFiles(t)
	m := &storeMove{
		t: t, binary: binary, databaseURL: databaseURL, issuer: issuer,
		clientFile: signInClientSecretFile(t), certFile: certFile, keyFile: keyFile, github: github,
		profiles: profilesFile(t, `{"profiles": [{"name": "`+moveAppRead+`", "source": "`+moveApp+`",
			"github": {"accounts": ["acme-org"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"},
			"proxy": {"hosts": ["`+moveHost+`"], "headers": ["authorization"], "allow": []}}]}`),
		front: newFront(t),
		db:    connectToBroker(t, databaseURL),
	}
	m.pods = podclient.Pods{T: t, Base: m.front.URL, Token: podclient.IssuerTokens(t, issuer, podKey, movePodAudience)}
	return m
}

// moveValue is MOVE_TOKEN's value under prefix: each store holds its own, so a release says which
// store it read.
func moveValue(prefix string) string { return "move-token-under-" + prefix }

// moveSecrets writes a fake secrets file of the step's own holding MOVE_TOKEN and MOVE_APP under
// prefix, and each of extra, one entry's JSON, and answers its path.
func (m *storeMove) moveSecrets(prefix string, extra ...string) string {
	m.t.Helper()
	appValue, err := json.Marshal(policytest.TypeValue(policy.TypeGitHubApp))
	if err != nil {
		m.t.Fatal(err)
	}
	entries := append([]string{
		moveStored(prefix, "move-token", moveValue(prefix)),
		`{"name": "` + prefix + `move-app", "kms_key_id": "` + moveKMSKeyName + `",
		 "tags": {"owner": "` + ada + `", "tier": "human", "` + policy.TagType + `": "` + policy.TypeGitHubApp + `", "` + policy.TagGitHubOwners + `": "acme-org"},
		 "value": ` + string(appValue) + `}`,
	}, extra...)
	path := filepath.Join(m.t.TempDir(), "fake-secrets.json")
	writeWhole(m.t, path, `{"secrets": [`+strings.Join(entries, ",\n")+`]}`)
	return path
}

// moveStored is the fake store's entry of the service's agent-tier stored secret slug under
// prefix, holding value.
func moveStored(prefix, slug, value string) string {
	return `{"name": "` + prefix + slug + `", "kms_key_id": "` + moveKMSKeyName + `",
		 "tags": {"owner": "` + moveService + `", "tier": "agent"}, "value": "` + value + `"}`
}

// start starts a broker of the step's on its database and public URL, on the Secret Manager
// store under prefix with second as BROKER_SECRETS_SECOND_PREFIX ("" for none), reading the fake
// secrets file secretsFile.
func (m *storeMove) start(prefix, second, secretsFile string) *brokerProcess {
	m.t.Helper()
	broker := startBroker(m.t, brokerOptions{Binary: m.binary, Env: []string{
		"BROKER_DATABASE_URL=" + m.databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=" + m.front.URL,
		"BROKER_FAKE_SECRETS_FILE=" + secretsFile,
		"BROKER_SECRETS_STORE=gcp-secret-manager",
		"BROKER_SECRETS_PREFIX=" + prefix,
		"BROKER_SECRETS_SECOND_PREFIX=" + second,
		"BROKER_SECRETS_KMS_KEY_ARN=",
		"BROKER_SECRETS_KMS_KEY_NAME=" + moveKMSKeyName,
		"BROKER_K8S_OIDC_ISSUER=" + m.issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + movePodAudience,
		"BROKER_SERVICE_LOGINS=" + moveService + "=" + moveLoginAccount,
		"BROKER_SERVICES=" + moveService + "=" + movePodAccount,
		"BROKER_SIGNIN_ISSUER=" + m.issuer.URL(),
		"BROKER_SIGNIN_CLIENT_ID=" + ciphertest.SignInClientID,
		"BROKER_SIGNIN_CLIENT_SECRET_FILE=" + m.clientFile,
		"BROKER_SIGNIN_DOMAIN=" + ciphertest.SignInDomain,
		"BROKER_PROFILES_FILE=" + m.profiles,
		"BROKER_AIRLOCK_PROXY_URL=http://127.0.0.1:13381",
		"BROKER_AIRLOCK_CA_CERT_FILE=" + m.certFile,
		"BROKER_AIRLOCK_CA_KEY_FILE=" + m.keyFile,
		"BROKER_AIRLOCK_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_AIRLOCK_TEST_RESOLVE=" + moveHost + "=" + rereadBoundAddress,
		"BROKER_AIRLOCK_TEST_GITHUB_API_BASE=" + m.github.URL(),
		"BROKER_DRAIN_SECONDS=0",
	}})
	requireHealthy(m.t, broker.Addr)
	return broker
}

// move is one step: grants made and released through a broker on from, then the same grants
// through a broker on to whose second prefix is second; carries says whether they go on there.
func (m *storeMove) move(from, to, second string, carries bool) {
	t := m.t
	first := m.start(from, "", m.moveSecrets(from))
	m.front.route(t, first)

	launch := m.pods.LogIn(moveService, moveLoginAccount)
	pod := m.pods.Enroll(launch, "move-pod", movePodAccount)
	token := m.pods.Ask(pod, moveToken)
	if token.State != "granted" || token.GrantID == nil {
		t.Fatalf("the pod's request for %s = %+v; want granted at once", moveToken, token)
	}
	asked := m.pods.Ask(pod, moveAppRead)
	if asked.State != "pending" || asked.RecordID == nil {
		t.Fatalf("the pod's request for %s = %+v; want pending on a record ada approves", moveAppRead, asked)
	}
	ciphertest.Decide(t, m.front.URL, m.issuer, ada, *asked.RecordID, true)
	app := m.pods.State(pod, asked.RequestID)
	if app.State != "granted" || app.GrantID == nil {
		t.Fatalf("the pod's request for %s after ada's approval = %+v; want granted", moveAppRead, app)
	}
	tokenGrant, appGrant := *token.GrantID, *app.GrantID

	// Released through the first broker before the second one starts, or the step proves nothing.
	if values := m.pods.Values(pod, tokenGrant); values[moveToken] != moveValue(from) {
		t.Fatalf("the first broker released %v for %s; want %s, the value under %s", values, moveToken, moveValue(from), from)
	}
	m.airlockRelease(pod, appGrant)
	before := m.readRows()
	for key, want := range map[string]string{
		token.RequestID + " " + moveToken:   from + "move-token",
		asked.RequestID + " " + moveAppRead: policy.TypeSource(policy.TypeGitHubApp, from+"move-app"),
	} {
		if got := before.sources[key]; got != want {
			t.Fatalf("the request_secrets row %s froze %q; want %q, a Secret Manager resource name under %s", key, got, want, from)
		}
	}
	for _, grant := range []string{tokenGrant, appGrant} {
		if revokedBy, held := before.revoked[grant]; !held || revokedBy != "" {
			t.Fatalf("grant %s before the second broker: held %t, revoked by %q; want live", grant, held, revokedBy)
		}
	}

	t.Logf("--- a broker on %s with BROKER_SECRETS_SECOND_PREFIX=%q, on the same database and public URL", to, second)
	next := m.start(to, second, m.moveSecrets(to,
		moveStored(from, "move-only-old", "held-under-the-old-prefix-alone"),
		moveStored(to, "move-fresh", "move-fresh-value")))
	m.front.route(t, next)

	if carries {
		if values := m.pods.Values(pod, tokenGrant); values[moveToken] != moveValue(to) {
			t.Fatalf("the second broker released %v for %s; want %s, the value its own store holds under %s", values, moveToken, moveValue(to), to)
		}
		password, placeholder := m.airlockRelease(pod, appGrant)
		reads := m.proxyReads(appGrant)
		m.callThroughProxy(next, password, placeholder)
		if got := m.proxyReads(appGrant); got != reads+1 {
			t.Fatalf("the second broker's proxy read grant %s's values %d times on one connection; want once", appGrant, got-reads)
		}
		for _, held := range []struct {
			name             string
			grant, requestID string
		}{{moveToken, tokenGrant, token.RequestID}, {moveAppRead, appGrant, asked.RequestID}} {
			again := m.pods.Ask(pod, held.name)
			if again.State != "granted" || again.GrantID == nil || *again.GrantID != held.grant || again.RequestID != held.requestID {
				t.Fatalf("a Create for %s on the second broker = %+v; want the live grant %s of request %s handed back", held.name, again, held.grant, held.requestID)
			}
		}
	} else {
		for name, grant := range map[string]string{moveToken: tokenGrant, moveAppRead: appGrant} {
			status, body := m.signed(pod, http.MethodPost, "/v1/grants/"+grant+"/values")
			if status != http.StatusForbidden || decodeError(t, body).Code != "GRANT_NOT_LIVE" {
				t.Fatalf("the second broker's release of %s with no second prefix = %d %s; want 403 GRANT_NOT_LIVE", name, status, body)
			}
		}
	}

	m.requireNothingElseOf(pod, from, to)
	after := m.readRows()
	for key, source := range before.sources {
		if got, held := after.sources[key]; !held || got != source {
			t.Fatalf("the request_secrets row %s is %q after the second broker (held %t); want it as it was, %q", key, got, held, source)
		}
	}
	for _, grant := range []string{tokenGrant, appGrant} {
		revokedBy := after.revoked[grant]
		switch {
		case carries && revokedBy != "":
			t.Fatalf("the second broker ended grant %s (revoked by %q); want it live", grant, revokedBy)
		case !carries && revokedBy != requests.GrantEndedActor:
			t.Fatalf("grant %s after the second broker with no second prefix: revoked by %q; want ended by %s", grant, revokedBy, requests.GrantEndedActor)
		}
	}
}

// requireNothingElseOf fails the step unless the broker the front routes to now, configured on
// configured, serves nothing under old: a session's reread of MOVE_ONLY_OLD, which its store holds
// under old alone, answers it not served; an ask for it is refused UNKNOWN_SECRET; GET
// /v1/settings names configured and not old; and a new request for MOVE_FRESH freezes its source
// under configured.
func (m *storeMove) requireNothingElseOf(pod podclient.Pod, old, configured string) {
	t := m.t
	t.Helper()
	status, body := m.signed(pod, http.MethodPost, "/v1/secrets/"+moveOnlyOld+"/reread")
	var reread struct {
		Served *bool `json:"served"`
	}
	_ = json.Unmarshal(body, &reread)
	if status != http.StatusOK || reread.Served == nil || *reread.Served {
		t.Fatalf("a session's reread of %s = %d %s; want 200 and not served: its store holds it under %s alone", moveOnlyOld, status, body, old)
	}
	request, err := record.Sign(pod.Key, m.front.URL, []record.AuthorizationDetail{
		{Type: record.KindAgentSecret, Identifier: moveOnlyOld, Actions: []string{"inject"}},
	}, pod.RuntimeID+" needs "+moveOnlyOld, "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, body = m.signedBody(pod, http.MethodPost, "/v1/requests", map[string]any{"request": request, "session_id": nil})
	if status != http.StatusBadRequest || decodeError(t, body).Code != "UNKNOWN_SECRET" {
		t.Fatalf("an ask for %s = %d %s; want 400 UNKNOWN_SECRET", moveOnlyOld, status, body)
	}
	status, body = accountCall(t, m.front.URL, http.MethodGet, "/v1/settings", nil, nil)
	var settings struct {
		Prefix string `json:"secrets_prefix"`
	}
	_ = json.Unmarshal(body, &settings)
	if status != http.StatusOK || settings.Prefix != configured || strings.Contains(string(body), old) {
		t.Fatalf("GET /v1/settings = %d %s; want secrets_prefix %s and nothing of %s", status, body, configured, old)
	}
	fresh := m.pods.Ask(pod, moveFresh)
	if fresh.State != "granted" {
		t.Fatalf("a new request for %s = %+v; want granted at once", moveFresh, fresh)
	}
	if got, want := m.readRows().sources[fresh.RequestID+" "+moveFresh], configured+"move-fresh"; got != want {
		t.Fatalf("the new request for %s froze %q; want %q, under the configured prefix", moveFresh, got, want)
	}
}

// moveRows is what the step's database holds of its grants and requests: each request_secrets
// row's frozen source, keyed "<request id> <name>", and each grant's revoked_by, "" for a live
// one.
type moveRows struct {
	sources map[string]string
	revoked map[string]string
}

func (m *storeMove) readRows() moveRows {
	m.t.Helper()
	ctx := context.Background()
	rows := moveRows{sources: map[string]string{}, revoked: map[string]string{}}
	secretRows, err := m.db.Query(ctx, `select request_id::text, name, source from request_secrets`)
	if err != nil {
		m.t.Fatalf("read request_secrets: %v", err)
	}
	for secretRows.Next() {
		var requestID, name, source string
		if err := secretRows.Scan(&requestID, &name, &source); err != nil {
			m.t.Fatalf("read request_secrets: %v", err)
		}
		rows.sources[requestID+" "+name] = source
	}
	if err := secretRows.Err(); err != nil {
		m.t.Fatalf("read request_secrets: %v", err)
	}
	grantRows, err := m.db.Query(ctx, `select id::text, coalesce(revoked_by, '') from grants`)
	if err != nil {
		m.t.Fatalf("read grants: %v", err)
	}
	for grantRows.Next() {
		var id, revokedBy string
		if err := grantRows.Scan(&id, &revokedBy); err != nil {
			m.t.Fatalf("read grants: %v", err)
		}
		rows.revoked[id] = revokedBy
	}
	if err := grantRows.Err(); err != nil {
		m.t.Fatalf("read grants: %v", err)
	}
	if len(rows.sources) == 0 || len(rows.revoked) == 0 {
		m.t.Fatalf("the step's database holds %d request_secrets rows and %d grants; a read that finds none read nothing", len(rows.sources), len(rows.revoked))
	}
	return rows
}

// signed sends method path with no body as pod, with its proof of its enrollment at the step's
// public URL.
func (m *storeMove) signed(pod podclient.Pod, method, path string) (int, []byte) {
	m.t.Helper()
	return m.signedBody(pod, method, path, nil)
}

// signedBody sends body as JSON to path as pod, with its proof of its enrollment at the step's
// public URL, and answers the status and the body.
func (m *storeMove) signedBody(pod podclient.Pod, method, path string, body any) (int, []byte) {
	m.t.Helper()
	header, err := proof.Sign(pod.Key, pod.EnrollmentID, method, m.front.URL+path, time.Now())
	if err != nil {
		m.t.Fatal(err)
	}
	return accountCall(m.t, m.front.URL, method, path, map[string]string{"Proof": header}, body)
}

// airlockRelease releases grantID, MOVE_APP_READ's, as pod, and answers what the release hands
// the command for the name behind the airlock: the proxy's password and the placeholder of the
// token's slot. A release that is no 200, or that carries no such name, fails the step.
func (m *storeMove) airlockRelease(pod podclient.Pod, grantID string) (password, placeholder string) {
	m.t.Helper()
	status, body := m.signed(pod, http.MethodPost, "/v1/grants/"+grantID+"/values")
	var release struct {
		Airlock *struct {
			ProxyPassword string `json:"proxy_password"`
			Names         map[string]struct {
				Slots map[string]string `json:"slots"`
			} `json:"names"`
		} `json:"airlock"`
	}
	_ = json.Unmarshal(body, &release)
	if status != http.StatusOK || release.Airlock == nil || release.Airlock.Names[moveAppRead].Slots[requests.SlotValue] == "" || release.Airlock.ProxyPassword == "" {
		m.t.Fatalf("the release of %s's grant %s = %d %s; want 200 with its placeholder behind the airlock", moveAppRead, grantID, status, body)
	}
	return release.Airlock.ProxyPassword, release.Airlock.Names[moveAppRead].Slots[requests.SlotValue]
}

// callThroughProxy opens an airlock connection on broker's proxy port with password and sends one
// request carrying placeholder to the profile's host. The proxy block allows nothing, so the
// airlock refuses it - once it has read the grant's values, which is what the step counts - and
// nothing is forwarded.
func (m *storeMove) callThroughProxy(broker *brokerProcess, password, placeholder string) {
	m.t.Helper()
	anchor := x509.NewCertPool()
	certificate, err := os.ReadFile(m.certFile)
	if err != nil {
		m.t.Fatal(err)
	}
	if !anchor.AppendCertsFromPEM(certificate) {
		m.t.Fatalf("the airlock authority's certificate did not parse: %s", m.certFile)
	}
	proxyURL, err := url.Parse("http://agent:" + password + "@" + airlockProxyAddr(m.t, broker))
	if err != nil {
		m.t.Fatal(err)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: anchor, MinVersion: tls.VersionTLS12},
	}}
	request, err := http.NewRequest(http.MethodGet, "https://"+moveHost+"/repos/acme-org/example", nil)
	if err != nil {
		m.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+placeholder)
	answer, err := client.Do(request)
	if err != nil {
		m.t.Fatalf("GET through the second broker's airlock: %v", err)
	}
	defer answer.Body.Close()
	body, _ := io.ReadAll(answer.Body)
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "not allowed by rule") {
		m.t.Fatalf("GET through the second broker's airlock = %d %q; want 403 naming \"not allowed by rule\", after it read the grant's values", answer.StatusCode, body)
	}
}

// proxyReads counts the airlock's reads of grantID's values: each writes one grant.used row
// marked airlock, which a session's own release does not mark.
func (m *storeMove) proxyReads(grantID string) int {
	m.t.Helper()
	var reads int
	if err := m.db.QueryRow(context.Background(), `select count(*) from audit
		where kind='grant.used' and grant_id=$1 and detail->>'airlock'='true'`, grantID).Scan(&reads); err != nil {
		m.t.Fatalf("count the proxy's reads of grant %s: %v", grantID, err)
	}
	return reads
}
