// cmd/cipherd/private_slack_fence_test.go
// The airlock's public Slack fence on a broker that has just started, against the REAL compiled
// broker: its project-access client is pointed at a Dispatch stand-in that holds PRIV private with
// a Slack channel. No answer this process has read from Dispatch has called a project private yet,
// as on every pod after a rollout, so the fence must read Dispatch's list at the first release,
// whether or not BROKER_PRIVATE_PROJECTS names PRIV.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// boundCallMessage is the line the airlock's proxy logs for every call to a bound host it carried
// upstream or failed to (proxy.callMessage); a call it refused logs the refusal's line instead.
const boundCallMessage = "airlock proxy: a call to a bound host"

// TestAFreshPodWithDeclaredPrivateProjectsFencesBeforeAnyPrivateAnswer: a pod of a service, whose
// session names no project, is granted the Slack bot token, tagged project-channels=slack, behind
// the airlock, on a broker that declares PRIV private (BROKER_PRIVATE_PROJECTS=PRIV), and on one
// that declares no key while Dispatch holds PRIV private. On each, the pod's first read of PRIV's
// channel through the proxy is refused PRIVATE_CHANNEL and nothing is carried towards Slack: the
// release that read was fenced from PRIV's channel from the start. The only call the broker made
// to Dispatch was the list of private projects: it asked no project's or person's access, so no
// answer had called a project private before the fence was read. Run with the fence's read
// waiting for a declaration or an answer that called a project private, the read is carried
// towards Slack on the broker that declares no key, and the test names the channel.
func TestAFreshPodWithDeclaredPrivateProjectsFencesBeforeAnyPrivateAnswer(t *testing.T) {
	t.Run("PRIV declared in BROKER_PRIVATE_PROJECTS", func(t *testing.T) { freshPodFirstRead(t, true) })
	t.Run("no key declared, PRIV private in Dispatch alone", func(t *testing.T) { freshPodFirstRead(t, false) })
}

// freshPodFirstRead is one row of that test: a broker that declares PRIV private when declare is
// true, and no key otherwise.
func freshPodFirstRead(t *testing.T, declare bool) {
	const (
		podAudience  = "example-broker-pod"
		service      = "example-service"
		loginAccount = "system:serviceaccount:example-system:example-service-login"
		podAccount   = "system:serviceaccount:example:example-worker"
		secretName   = "SLACK_BOT_TOKEN"
		privProject  = "PRIV"
		privChannel  = "C0PRIV00001"
		// slackHost stands in for slack.com: a name no resolver answers, which
		// BROKER_AIRLOCK_TEST_RESOLVE gives slackAddress, RFC 5737's TEST-NET-1, where no service
		// answers and from which nothing reaches the internet. A call the fence carried would be
		// dialled there, and logs boundCallMessage whether or not the dial answers.
		slackHost    = "slack.airlock.test"
		slackAddress = "192.0.2.1"
	)
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	dispatch := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		privProject: {Visibility: "private", Owner: "carol@example.com", SlackChannels: []string{privChannel}},
	}, nil)
	tokenFile := filepath.Join(t.TempDir(), "dispatch-access-token")
	if err := os.WriteFile(tokenFile, []byte(dispatch.Token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &requesterStore{path: filepath.Join(t.TempDir(), "fake-secrets.json"), rows: []secrets.LocalSecret{
		requesterRow(secretName, service, policy.TierAgent, "the-real-slack-bot-token", map[string]string{policy.TagProjectChannels: policy.ProjectChannelsSlack}),
	}}
	store.write(t)
	certFile, keyFile := airlockCAFiles(t)
	env := []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + store.path,
		"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + podAudience,
		"BROKER_SERVICE_LOGINS=" + service + "=" + loginAccount,
		"BROKER_SERVICES=" + service + "=" + podAccount,
		"BROKER_PROJECT_ACCESS_URL=" + dispatch.URL,
		"BROKER_PROJECT_ACCESS_TOKEN_FILE=" + tokenFile,
		"BROKER_AIRLOCK_PROXY_URL=http://127.0.0.1:13381",
		"BROKER_AIRLOCK_CA_CERT_FILE=" + certFile,
		"BROKER_AIRLOCK_CA_KEY_FILE=" + keyFile,
		"BROKER_AIRLOCK_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_AIRLOCK_TEST_RESOLVE=" + slackHost + "=" + slackAddress,
		"BROKER_AIRLOCK_RULES_FILE=" + airlockRulesFile(t,
			`{"secrets": [{"name": "`+secretName+`", "hosts": ["`+slackHost+`"], "headers": ["authorization"]}]}`),
		"BROKER_DRAIN_SECONDS=0",
	}
	if declare {
		env = append(env, "BROKER_PRIVATE_PROJECTS="+privProject)
	}
	broker := startBroker(t, brokerOptions{Env: env})
	requireHealthy(t, broker.Addr)
	broker.AwaitRecord(t, "project access check", map[string]any{"state": "on"})
	if strings.Contains(broker.Boot, "agent secret policy refused") {
		t.Fatalf("the broker refused the Slack token at boot; a broker with project access serves a project-channels secret:\n%s", broker.Boot)
	}
	proxyAddr := airlockProxyAddr(t, broker)

	api := podBroker{t: t, base: "http://" + broker.Addr, issuer: issuer, key: podKey, audience: podAudience}
	p := api.enroll(api.logIn(service, loginAccount), "fence-pod", podAccount, "")
	granted := api.ask(p, secretName)
	if granted.GrantID == nil {
		t.Fatalf("the pod's request for %s = %+v, want a grant at once", secretName, granted)
	}
	password, slots := airlockOf(api, p, *granted.GrantID)
	placeholder := slots[requests.SlotValue]
	if password == "" || placeholder == "" {
		t.Fatalf("the release carries no proxy password or no value placeholder: %q, %v", password, slots)
	}

	// The command's own client: the proxy as its HTTPS proxy, the broker's airlock authority as
	// the one thing it trusts, and the placeholder where the token would be.
	anchor := x509.NewCertPool()
	certificate, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if !anchor.AppendCertsFromPEM(certificate) {
		t.Fatalf("the airlock authority's certificate did not parse: %s", certFile)
	}
	proxyURL, err := url.Parse("http://agent:" + password + "@" + proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: anchor, MinVersion: tls.VersionTLS12},
	}}
	read := "https://" + slackHost + "/api/conversations.replies?channel=" + privChannel + "&ts=1731700000.000100"
	request, err := http.NewRequest(http.MethodGet, read, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+placeholder)
	reply, err := client.Do(request)
	if err != nil {
		t.Fatalf("the pod's first read of %s through the airlock: %v; want 403 PRIVATE_CHANNEL from the proxy at once: a read with no answer was carried towards Slack, to %s, where nothing answers", privChannel, err, slackAddress)
	}
	body, _ := io.ReadAll(reply.Body)
	reply.Body.Close()
	if reply.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "this Slack channel belongs to a private project") {
		t.Fatalf("the pod's first read of %s through the airlock = %d %q; want 403 PRIVATE_CHANNEL: PRIV, private in Dispatch, fences its channel before any answer of Dispatch's has called it private",
			privChannel, reply.StatusCode, body)
	}
	// The broker's log reaches this test through a pipe that a goroutine of the harness copies, so
	// the refusal's record can arrive after the answer has: the record is awaited, as the
	// package's other log readers await theirs, before the log is read. Once it is there, every
	// line the broker wrote before it is too, and one call is either refused or carried, never both.
	broker.AwaitRecord(t, answer.RefusalMessage, map[string]any{"code": "PRIVATE_CHANNEL"})
	log := broker.Log()
	if refusals, carried := recordsWith(log, answer.RefusalMessage, map[string]any{"code": "PRIVATE_CHANNEL"}), recordsWith(log, boundCallMessage, nil); len(refusals) != 1 || len(carried) != 0 {
		t.Fatalf("the broker logged %d PRIVATE_CHANNEL refusal(s) and %d call(s) carried towards Slack for the read of %s; want 1 and none:\n%s", len(refusals), len(carried), privChannel, log)
	}
	// The fence was read from Dispatch's list alone: the broker asked no project's and no
	// person's access, so nothing it read before the list had called a project private.
	if lists, calls := dispatch.PrivateProjectsCalls(), dispatch.Calls(); lists == 0 || calls != lists {
		t.Fatalf("the broker asked Dispatch %d time(s), %d of them for the list of private projects; want the list alone, at least once", calls, lists)
	}
}
