// cmd/cipherd/airlock_reread_test.go
// Rows against the REAL compiled broker: a reread of a secret's name reaches the airlock's proxy,
// so the values the proxy holds for a grant are read again before the next request is decided.
// They are driven through the reread route a person actually uses — POST
// /v1/secrets/{name}/reread on the broker's own port — rather than by calling the proxy's
// DropName, because what these rows are about is the wiring between the two.
//
// What they read is the audit trail: every read the proxy makes writes one grant.used row marked
// airlock, which is the same row a session's own release writes. Every stored secret of the rig is
// behind the one kind its profiles file declares (rereadKind), whose allow list is present and
// empty, so it allows no method or path at all: every request through the proxy is refused by the
// airlock's own decision — which runs after the values are read — and the rows need no service
// for the proxy to reach.
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

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// airlockProxyLine is the line main logs once the proxy port is bound, whose addr field is the
// address it took. The address the release names is BROKER_AIRLOCK_PROXY_URL, which a deployment
// sets to the Service a command reaches; a test binds port 0 and so reads the address the
// process actually took.
const airlockProxyLine = "airlock proxy listening"

// airlockProxyAddr is the address the broker bound its proxy port to.
func airlockProxyAddr(t *testing.T, broker *brokerProcess) string {
	t.Helper()
	record := broker.AwaitRecord(t, airlockProxyLine, nil)
	addr, _ := record["addr"].(string)
	if addr == "" {
		t.Fatalf("the broker's %q line names no address: %v", airlockProxyLine, record)
	}
	return addr
}

// airlockOf is the airlock half of a grant's release: the password every connection to the proxy
// opens with, and each name's slots, which hold the placeholders the command puts in its
// requests. A grant whose one name is behind the airlock gets no value at all, so this is all
// the session has.
func airlockOf(b podBroker, p pod, grantID string) (password string, slots map[string]string) {
	b.t.Helper()
	status, answer := b.signed(p, "/v1/grants/"+grantID+"/values", nil)
	if status != http.StatusOK {
		b.t.Fatalf("%s's grant values = %d %s, want 200", p.runtimeID, status, answer)
	}
	var release struct {
		Airlock *struct {
			ProxyPassword string `json:"proxy_password"`
			Names         map[string]struct {
				Slots map[string]string `json:"slots"`
			} `json:"names"`
		} `json:"airlock"`
	}
	b.decode(answer, &release)
	if release.Airlock == nil || len(release.Airlock.Names) != 1 {
		b.t.Fatalf("the release carries no airlock for one name: %s", answer)
	}
	for _, held := range release.Airlock.Names {
		slots = held.Slots
	}
	return release.Airlock.ProxyPassword, slots
}

// The rows' service, its accounts and the pod audience their broker verifies.
const (
	rereadPodAudience  = "example-broker-pod"
	rereadService      = "example-service"
	rereadLoginAccount = "system:serviceaccount:example-system:example-service-login"
	rereadPodAccount   = "system:serviceaccount:example:example-worker"
	// rereadBoundAddress is RFC 5737's TEST-NET-1, which BROKER_AIRLOCK_TEST_RESOLVE gives each
	// bound host, names no resolver answers, so that the CONNECT a row makes is neither refused
	// as a local address nor dependent on a resolver this process may not have, and no request
	// of these rows can reach the internet. A kind binds the name rather than the address,
	// since the rules validation refuses an address literal: a TLS client sends no server name
	// for one, and the airlock refuses a request whose server name is not its CONNECT host.
	rereadBoundAddress = "192.0.2.1"
	// rereadKind is the one kind of the rig's profiles file (rereadKinds), which the proxy tag of
	// every stored secret of the rig names.
	rereadKind = "airlock"
)

// airlockRereadRig is one compiled broker with an airlock, the one stored secret AIRLOCK_TOKEN
// (the service's, agent tier, so its pod is granted at once, and behind rereadKind) and the
// profiles it was started with, and a pod of the service enrolled on it.
type airlockRereadRig struct {
	t                   *testing.T
	broker              *brokerProcess
	api                 podBroker
	pod                 pod
	databaseURL         string
	db                  *pgx.Conn
	certFile, proxyAddr string
	// fakeSecretsFile is the broker's fake store, which writeSecrets writes and the broker reads
	// again at each call it makes of the store.
	fakeSecretsFile string
}

// startAirlockReread starts the rig's broker serving profiles, each one entry's JSON, in a
// profiles file that declares rereadKind on hosts (rereadKinds), with each of hosts resolved to
// rereadBoundAddress.
func startAirlockReread(t *testing.T, hosts []string, profiles ...string) *airlockRereadRig {
	t.Helper()
	return startAirlockRereadWith(t, airlockRereadExtras{}, hosts, profiles...)
}

// rereadKinds is the kinds list of the rig's profiles file: rereadKind, binding hosts, carried in
// Authorization, served by its stored value or by a token the proxy mints from an App's key, with
// an allow list that is present and empty and so allows nothing.
func rereadKinds(t *testing.T, hosts []string) string {
	t.Helper()
	bound, err := json.Marshal(hosts)
	if err != nil {
		t.Fatal(err)
	}
	return `[{"name": "` + rereadKind + `", "hosts": ` + string(bound) + `, "headers": ["authorization"], "ways": ["swap", "mint"], "allow": []}]`
}

// airlockRereadExtras is what a row adds to the rig's broker: entries of its fake store beside
// AIRLOCK_TOKEN's, each one entry's JSON, and settings beside the rig's own.
type airlockRereadExtras struct {
	secrets []string
	env     []string
}

// startAirlockRereadWith is startAirlockReread with extras added to the broker it starts.
func startAirlockRereadWith(t *testing.T, extras airlockRereadExtras, hosts []string, profiles ...string) *airlockRereadRig {
	t.Helper()
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	r := &airlockRereadRig{t: t, databaseURL: databaseURL, fakeSecretsFile: filepath.Join(t.TempDir(), "fake-secrets.json")}
	r.writeSecrets(extras.secrets...)
	resolve := make([]string, len(hosts))
	for i, host := range hosts {
		resolve[i] = host + "=" + rereadBoundAddress
	}
	certFile, keyFile := airlockCAFiles(t)
	broker := startBroker(t, brokerOptions{Env: append([]string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + r.fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + rereadPodAudience,
		"BROKER_SERVICE_LOGINS=" + rereadService + "=" + rereadLoginAccount,
		"BROKER_SERVICES=" + rereadService + "=" + rereadPodAccount,
		"BROKER_AIRLOCK_PROXY_URL=http://127.0.0.1:13381",
		"BROKER_AIRLOCK_CA_CERT_FILE=" + certFile,
		"BROKER_AIRLOCK_CA_KEY_FILE=" + keyFile,
		"BROKER_AIRLOCK_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_AIRLOCK_TEST_RESOLVE=" + strings.Join(resolve, " "),
		"BROKER_PROFILES_FILE=" + profilesFile(t, policytest.KindsFileOf(rereadKinds(t, hosts), profiles...)),
	}, extras.env...)})
	requireHealthy(t, broker.Addr)
	r.broker, r.certFile, r.proxyAddr = broker, certFile, airlockProxyAddr(t, broker)
	r.api = podBroker{t: t, base: "http://" + broker.Addr, issuer: issuer, key: podKey, audience: rereadPodAudience}
	r.pod = r.api.enroll(r.api.logIn(rereadService, rereadLoginAccount), "airlock-pod", rereadPodAccount, "")
	r.db = connectToBroker(t, databaseURL)
	return r
}

// writeSecrets writes the rig's fake store whole: AIRLOCK_TOKEN, behind rereadKind, and each of
// extra, one entry's JSON. A value of other bytes than the one before it is a new version of that
// secret.
func (r *airlockRereadRig) writeSecrets(extra ...string) {
	r.t.Helper()
	entries := append([]string{`{"name": "` + testSecretsPrefix + `airlock-token", "kms_key_id": "` + testSecretsKMSKeyARN + `",
		 "tags": {"owner": "` + rereadService + `", "tier": "agent", "` + policy.TagProxy + `": "` + rereadKind + `"}, "value": "the-real-airlock-token"}`}, extra...)
	writeWhole(r.t, r.fakeSecretsFile, `{"secrets": [`+strings.Join(entries, ",\n")+`]}`)
}

// proxied is a grant of name the rig's pod is granted at once, and a command's own client of it:
// the proxy as its HTTPS_PROXY with the grant's password, the broker's airlock authority as the
// one thing it trusts, and the placeholder it puts where the token would be.
type proxied struct {
	grantID, placeholder string
	client               *http.Client
}

// grant asks for name as the rig's pod and opens the command's client of the grant.
func (r *airlockRereadRig) grant(name string) proxied {
	r.t.Helper()
	granted := r.api.ask(r.pod, name)
	if granted.GrantID == nil {
		r.t.Fatalf("the pod's request for %s = %+v, want a grant at once", name, granted)
	}
	password, slots := airlockOf(r.api, r.pod, *granted.GrantID)
	placeholder := slots[requests.SlotValue]
	if password == "" || placeholder == "" {
		r.t.Fatalf("the release of %s carries no proxy password or no value placeholder: %q, %v", name, password, slots)
	}
	anchor := x509.NewCertPool()
	certificate, err := os.ReadFile(r.certFile)
	if err != nil {
		r.t.Fatal(err)
	}
	if !anchor.AppendCertsFromPEM(certificate) {
		r.t.Fatalf("the airlock authority's certificate did not parse: %s", r.certFile)
	}
	proxyURL, err := url.Parse("http://agent:" + password + "@" + r.proxyAddr)
	if err != nil {
		r.t.Fatal(err)
	}
	return proxied{grantID: *granted.GrantID, placeholder: placeholder, client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: anchor, MinVersion: tls.VersionTLS12},
	}}}
}

// call sends GET https://host/path through the proxy on g's client. The kind's allow list is
// empty, which allows nothing, so the decision refuses every request - after the values the
// decision needs have been read, which is what these rows count - and nothing is forwarded or
// dialled.
func (r *airlockRereadRig) call(g proxied, host, path string) {
	r.t.Helper()
	request, err := http.NewRequest(http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+g.placeholder)
	answer, err := g.client.Do(request)
	if err != nil {
		r.t.Fatalf("GET %s through the airlock's proxy: %v", path, err)
	}
	defer answer.Body.Close()
	body, _ := io.ReadAll(answer.Body)
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "not allowed by rule") {
		r.t.Fatalf("GET %s through the airlock's proxy = %d %q, want 403 naming \"not allowed by rule\": the kind allows nothing", path, answer.StatusCode, body)
	}
}

// proxyReads counts the proxy's reads of g's values: every read the proxy makes writes one
// grant.used row marked airlock; a session's own release writes one without that mark, which is
// why the mark is in the predicate.
func (r *airlockRereadRig) proxyReads(g proxied) int {
	r.t.Helper()
	var reads int
	if err := r.db.QueryRow(context.Background(), `select count(*) from audit
		where kind='grant.used' and grant_id=$1 and detail->>'airlock'='true'`, g.grantID).Scan(&reads); err != nil {
		r.t.Fatalf("count the proxy's reads of grant %s: %v", g.grantID, err)
	}
	return reads
}

// reread rereads name through the route a session uses, failing r's test unless the broker
// serves the stored secret it read.
func (r *airlockRereadRig) reread(name string) {
	r.t.Helper()
	if status, _, reread := r.broker.sessionReread(r.t, r.databaseURL, name); status != http.StatusOK || reread["served"] != true {
		r.t.Fatalf("a session's reread of %s = status %d, %v; want 200 and served", name, status, reread)
	}
}

// requireReads fails r's test unless the proxy has read g's values want times, saying when.
func (r *airlockRereadRig) requireReads(g proxied, want int, when string) {
	r.t.Helper()
	if reads := r.proxyReads(g); reads != want {
		r.t.Fatalf("the proxy read grant %s's values %d times %s; want %d", g.grantID, reads, when, want)
	}
}

// TestARereadOfASecretReachesTheAirlocksProxy: a new version of a stored value moves neither the
// policy's version nor the set of names it serves, so the reread of the name is the only signal
// the broker gets that the value its proxy is holding is the old one. This row drives that
// signal through the real route and reads, in the audit trail, that the proxy read the grant's
// values again.
func TestARereadOfASecretReachesTheAirlocksProxy(t *testing.T) {
	t.Parallel()
	const boundHost = "reread.airlock.test"
	r := startAirlockReread(t, []string{boundHost})
	token := r.grant("AIRLOCK_TOKEN")

	r.call(token, boundHost, "/v1/first")
	r.requireReads(token, 1, "for its first request")
	r.call(token, boundHost, "/v1/second")
	r.requireReads(token, 1, "for two requests: the answer is held")

	r.reread("AIRLOCK_TOKEN")
	r.call(token, boundHost, "/v1/third")
	r.requireReads(token, 2, "after the reread: the reread of the name is what says the value may be new")
}

// TestARereadOfASourceReachesTheProxysValuesOfEachProfileOfIt: the proxy holds a grant's values
// by the name the grant was released under, which for a profile of another name is the
// profile's and not its source's. So a reread of the source AIRLOCK_TOKEN, which a `secret put`
// of a new value makes, has the proxy read the values of a grant of its profile AIRLOCK_READ
// again, and a reread of AIRLOCK_READ, which rereads that same stored value, has it read the
// values of a grant of the sibling profile AIRLOCK_SECOND again too, rather than going on
// swapping the old value into their requests until each grant ends: a stored value has no expiry.
func TestARereadOfASourceReachesTheProxysValuesOfEachProfileOfIt(t *testing.T) {
	t.Parallel()
	const readHost, secondHost = "read.airlock.test", "second.airlock.test"
	r := startAirlockReread(t, []string{readHost, secondHost},
		`{"name": "AIRLOCK_READ", "source": "AIRLOCK_TOKEN"}`,
		`{"name": "AIRLOCK_SECOND", "source": "AIRLOCK_TOKEN"}`)
	read, second := r.grant("AIRLOCK_READ"), r.grant("AIRLOCK_SECOND")
	r.call(read, readHost, "/v1/first")
	r.call(second, secondHost, "/v1/first")
	r.requireReads(read, 1, "for its first request")
	r.requireReads(second, 1, "for its first request")

	r.reread("AIRLOCK_TOKEN")
	r.call(read, readHost, "/v1/second")
	r.requireReads(read, 2, "after a reread of its source")
	r.call(second, secondHost, "/v1/second")
	r.requireReads(second, 2, "after a reread of its source")

	r.reread("AIRLOCK_READ")
	r.call(second, secondHost, "/v1/third")
	r.requireReads(second, 3, "after a reread of its sibling profile, which rereads their one source")
}

// rereadAppSecret is the fake store's entry of AIRLOCK_APP, a github-app secret of the rig's
// service at agent tier, behind rereadKind, whose github-owners tag names acme-org, holding value.
func rereadAppSecret(t *testing.T, value string) string {
	t.Helper()
	quoted, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return `{"name": "` + testSecretsPrefix + `airlock-app", "kms_key_id": "` + testSecretsKMSKeyARN + `",
		 "tags": {"owner": "` + rereadService + `", "tier": "agent", "` + policy.TagType + `": "` + policy.TypeGitHubApp + `", "` + policy.TagGitHubOwners + `": "acme-org", "` + policy.TagProxy + `": "` + rereadKind + `"},
		 "value": ` + string(quoted) + `}`
}

// TestARereadOfAGitHubAppSourceDropsItsProfilesTokensOnlyWhenItMovedItsValue: a proxied GitHub
// profile's held value is an installation token the proxy minted from its source's stored App
// key, so a reread of the source through the public route drops it only when the reread moved
// the source's value version (policy.Lookup.Moved). A reread that finds the value as the
// broker held it leaves the proxy spending the token it holds: no read of the grant's values and
// no token request to GitHub. A reread that finds a new version of the value has the proxy read
// the grant's values again, which mints from the new one.
func TestARereadOfAGitHubAppSourceDropsItsProfilesTokensOnlyWhenItMovedItsValue(t *testing.T) {
	t.Parallel()
	const boundHost = "app.airlock.test"
	service, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatalf("start the GitHub stand-in: %v", err)
	}
	t.Cleanup(service.Close)
	service.AnswerWith(proxyrulestest.NewGitHubStand(proxyrulestest.GitHubOptions{Installations: map[string]int64{"acme-org": 7}}).Answer)
	tokenRequests := func() int {
		asked := 0
		for _, seen := range service.Requests() {
			if seen.Method == http.MethodPost && strings.HasSuffix(seen.Path, "/access_tokens") {
				asked++
			}
		}
		return asked
	}
	value := policytest.TypeValue(policy.TypeGitHubApp)
	r := startAirlockRereadWith(t, airlockRereadExtras{
		secrets: []string{rereadAppSecret(t, value)},
		env:     []string{"BROKER_AIRLOCK_TEST_GITHUB_API_BASE=" + service.URL()},
	}, []string{boundHost}, `{"name": "AIRLOCK_APP_READ", "source": "AIRLOCK_APP",
		"github": {"accounts": ["acme-org"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"}}`)
	app := r.grant("AIRLOCK_APP_READ")
	r.call(app, boundHost, "/v1/first")
	r.requireReads(app, 1, "for its first request")
	minted := tokenRequests()

	r.reread("AIRLOCK_APP")
	r.call(app, boundHost, "/v1/second")
	r.requireReads(app, 1, "after a reread that found the App's value as the broker held it: the token the proxy holds is still the one that value mints")
	if asked := tokenRequests(); asked != minted {
		t.Fatalf("GitHub was asked for %d tokens after that reread; want none: the proxy spends the token it holds", asked-minted)
	}

	// A new version of the App's value (a trailing space, which the decoder still reads).
	r.writeSecrets(rereadAppSecret(t, value+" "))
	r.reread("AIRLOCK_APP")
	r.call(app, boundHost, "/v1/third")
	r.requireReads(app, 2, "after a reread that found a new version of the App's value")
	if asked := tokenRequests(); asked != minted+1 {
		t.Fatalf("GitHub was asked for %d tokens after that reread; want one, the proxy's mint from the new value", asked-minted)
	}
}
