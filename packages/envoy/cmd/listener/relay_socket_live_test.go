//go:build natslive

package main

// relay_socket_live_test.go holds rows 1-8 of the relay socket (relay_socket.go), and row 5b, an
// owner's Reset, against what a deployment runs:
//
//   - nats-server at testnats.HubImage whose only users are the two edge replicas', each holding
//     natsgrant.Edge for its machine id, and Dispatch's, holding natsgrant.Dispatch, which creates
//     the stream and publishes each row's messages; there is no session user and no no_auth_user.
//     These are the lists core.EdgePermissions and core.DispatchPermissions wrap
//     (infra/core's TestEdgePermissionsAreTheEdgeGrant holds the first pair equal);
//   - two envoy-listener processes in edge mode, serving the one project PRIV: replica A holds the
//     session's registration, so its consumer and its relay, and replica B serves its relay socket;
//   - broker tokens from the real broker (internal/cipher/ciphertest, BROKER_TEST_DATABASE_URL)
//     whose access tokens live 30 s, traded from a device login a person approved, with the
//     broker's project check answered by projectaccesstest. Row 7 alone needs an answer the test
//     changes, and takes it from ciphertokentest's stand-in.
//
// The binary is built with its sockets' check every second (relaySocketCheckBuild), so a row waits
// for one tick and the broker's live answer, which is reused for at most
// ciphertoken.AffirmativeCacheTTL.

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	natsgo "github.com/nats-io/nats.go"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

const (
	// relayLiveProject is the project both replicas serve, and the one every token lists.
	relayLiveProject = "PRIV"
	// relayLiveA and relayLiveB are the two replicas' machine ids, and relayLiveDispatch
	// Dispatch's.
	relayLiveA        = "edge-a"
	relayLiveB        = "edge-b"
	relayLiveDispatch = "edge-dispatch"
	// relayLivePerson and relayLiveOtherPerson are the two people whose device logins the rows
	// trade with: restricted members, each named on PRIV's access list.
	relayLivePerson      = "ada@example.com"
	relayLiveOtherPerson = "bob@example.com"
	// relayLiveTokenLife is BROKER_ACCESS_TOKEN_SECONDS for every row.
	relayLiveTokenLife = 30 * time.Second
	// relayLiveCheck is the sockets' check interval the binary is built with.
	relayLiveCheck = time.Second
	// relayLiveCloseBound is how long after the broker's answer changes an open socket may take to
	// close: one check, the broker's live answer reused for at most AffirmativeCacheTTL, and a
	// second for the close to reach the client.
	relayLiveCloseBound = relayLiveCheck + ciphertoken.AffirmativeCacheTTL + time.Second
)

// relayLiveViolation matches a permissions violation nats-server logs, capturing its kind and the
// subject it names.
var relayLiveViolation = regexp.MustCompile(`(Publish|Subscription) Violation - .*?Subject "([^"]*)"`)

// relayLiveRig is one row's deployment: the server, Dispatch's client on it, the broker, and the
// two edge replicas.
type relayLiveRig struct {
	server interface {
		Logs(context.Context) (io.ReadCloser, error)
	}
	uri      string
	dispatch *bus.Client
	binary   string
	// broker is the real broker, nil on a rig whose replicas ask stub. access is its project
	// check's Dispatch stand-in, and proxy the address the replicas call the broker at, which a
	// row closes to stop the broker for them.
	broker *ciphertest.Rig
	access *projectaccesstest.Server
	proxy  *httptest.Server
	stub   *ciphertokentest.StubBroker
	// a and b are the two replicas: a holds the sessions' registrations, b serves their sockets.
	a, b *listenerProcess
}

// relayLiveOptions changes what newRelayLiveRig starts.
type relayLiveOptions struct {
	// stub asks ciphertokentest's stand-in about every token in place of the real broker.
	stub bool
	// edgeProjects is ENVOY_EDGE_PROJECTS, relayLiveProject by default, comma-separated.
	edgeProjects string
}

func newRelayLiveRig(t *testing.T, options relayLiveOptions) *relayLiveRig {
	t.Helper()
	if options.edgeProjects == "" {
		options.edgeProjects = relayLiveProject
	}
	rig := &relayLiveRig{binary: cmdtest.BuildCommand(t, cmdtest.Command{
		Name:  "envoy-listener",
		Flags: []string{"-ldflags", "-X main.relaySocketCheckBuild=" + relayLiveCheck.String()},
	})}

	seeds := map[string]string{}
	var users []testnats.StaticUser
	grants := map[string]natsgrant.Permissions{relayLiveDispatch: natsgrant.Dispatch(relayLiveDispatch)}
	for _, machineID := range []string{relayLiveA, relayLiveB} {
		grant, err := natsgrant.Edge(machineID, strings.Split(options.edgeProjects, ","))
		if err != nil {
			t.Fatalf("render the grant of replica %s for %s: %v", machineID, options.edgeProjects, err)
		}
		grants[machineID] = grant
	}
	for name, grant := range grants {
		seed, public := testnats.User(t)
		seeds[name] = seed
		users = append(users, testnats.StaticUser{NKey: public, Publish: grant.Publish, Subscribe: grant.Subscribe})
	}
	rig.server, rig.uri = testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(users...), []string{"-js"}, false)
	dispatch, err := bus.ConnectOwningStream([]string{rig.uri}, bus.WithName(relayLiveDispatch), bus.WithEnvironment(func(name string) (string, bool) {
		if name == bus.NkeySeedEnvVar {
			return seeds[relayLiveDispatch], true
		}
		return "", false
	}))
	if err != nil {
		t.Fatalf("Dispatch's user creates the stream: %v", err)
	}
	t.Cleanup(dispatch.Close)
	rig.dispatch = dispatch
	// The edge's grant creates and raises no bucket's stream, so each replica finds each bucket
	// the shared listeners made at their start; Dispatch's user, whose grant holds each one's
	// create, makes them here before either replica starts, as edge_live_test.go does.
	for _, bucket := range bus.ListenerBuckets() {
		if _, err := dispatch.JS().CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bucket, Storage: natsgo.FileStorage}); err != nil {
			t.Fatalf("Dispatch's user makes the %s bucket: %v", bucket, err)
		}
	}

	account := filepath.Join(t.TempDir(), "account-token")
	var brokerURL, accountToken string
	if options.stub {
		rig.stub = ciphertokentest.NewStubBroker(t)
		brokerURL, accountToken = rig.stub.Server.URL, "edge-listener-account-token"
	} else {
		rig.access = projectaccesstest.NewServer(t,
			map[string]projectaccesstest.Row{relayLiveProject: {Visibility: "private", Owner: "owner@example.com", Listed: []string{relayLivePerson, relayLiveOtherPerson}}},
			map[string]string{relayLivePerson: "restricted", relayLiveOtherPerson: "restricted"})
		rig.broker = ciphertest.NewRig(t, ciphertest.WithAccessLifetime(relayLiveTokenLife),
			ciphertest.WithProjectAccess(projectaccess.New(rig.access.URL, rig.access.Account, nil)))
		target, err := url.Parse(rig.broker.URL)
		if err != nil {
			t.Fatalf("read the broker's URL: %v", err)
		}
		rig.proxy = httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
		t.Cleanup(rig.proxy.Close)
		brokerURL, accountToken = rig.proxy.URL, rig.broker.ListenerToken
	}
	if err := os.WriteFile(account, []byte(accountToken), 0o600); err != nil {
		t.Fatalf("write the replicas' account token: %v", err)
	}

	start := func(machineID string) *listenerProcess {
		held := cmdtest.HoldPort(t)
		cmd := exec.Command(rig.binary)
		cmd.Env = []string{
			"PORT=" + strconv.Itoa(held.Port),
			"ENVOY_LISTEN_HOST=127.0.0.1",
			"ENVOY_MACHINE_ID=" + machineID,
			"NATS_URLS=" + rig.uri,
			bus.NkeySeedEnvVar + "=" + seeds[machineID],
			config.EdgeProjectsEnvVar + "=" + options.edgeProjects,
			EnvBrokerURL + "=" + brokerURL,
			EnvBrokerAccountTokenFile + "=" + account,
			EnvPublicURL + "=http://127.0.0.1:" + strconv.Itoa(held.Port),
		}
		held.Pass(cmd, "ENVOY_LISTEN_FD")
		return &listenerProcess{Process: cmdtest.Start(t, "the edge listener "+machineID, cmd), port: held.Port}
	}
	rig.a, rig.b = start(relayLiveA), start(relayLiveB)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("replica A's output:\n%s", rig.a.Output.String())
			t.Logf("replica B's output:\n%s", rig.b.Output.String())
		}
	})
	rig.a.waitHealthy(t)
	rig.b.waitHealthy(t)
	return rig
}

// requireNoViolation fails the row naming every permissions violation the server logged.
func (r *relayLiveRig) requireNoViolation(t *testing.T) {
	t.Helper()
	logs, err := r.server.Logs(context.Background())
	if err != nil {
		t.Fatalf("read the server's log: %v", err)
	}
	defer logs.Close()
	text, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("read the server's log: %v", err)
	}
	if found := relayLiveViolation.FindAllString(string(text), -1); len(found) > 0 {
		t.Fatalf("the server refused %d requests under the rendered grants: %q", len(found), found)
	}
}

// relayLiveSession is one session of a person's machine: the key its device login and every
// token traded from it are bound to, and the client that signs a proof of that key for each
// request.
type relayLiveSession struct {
	id     string
	key    *ecdsa.PrivateKey
	signer *http.Client
	// login is the broker client the device login ran through, credential the launcher
	// credential its approval minted, machine its token pair, and renewed when that pair was last
	// issued; nil on a stub rig's session.
	login      *ciphertest.Client
	credential string
	machine    *oauth2.Token
	renewed    time.Time
}

// newSession is a session whose requests are signed with a key of its own, and no device login.
func newRelayLiveSession(t *testing.T, id string) *relayLiveSession {
	t.Helper()
	key := ciphertest.NewSigningKey(t)
	return &relayLiveSession{id: id, key: key, signer: &http.Client{Transport: cipherclient.NewDPoPTransport(key, http.DefaultTransport), Timeout: 30 * time.Second}}
}

// login is session id of person's machine, whose device login person approved.
func (r *relayLiveRig) login(t *testing.T, id, person string) *relayLiveSession {
	t.Helper()
	session := newRelayLiveSession(t, id)
	address, err := url.Parse(r.broker.URL)
	if err != nil {
		t.Fatalf("read the broker's URL: %v", err)
	}
	session.login = ciphertest.NewClient(t, r.broker.URL, address.Host)
	session.login.DPoP = cipherclient.NewDPoPTransport
	session.login.UIToken = r.broker.UIToken
	authorization := session.login.DeviceLogin(t, session.key, person, "edge-live-"+strings.ReplaceAll(id, "_", "-"))
	session.credential = session.login.ApproveMachineLogin(t, person, authorization.UserCode)
	session.machine, err = session.login.DeviceToken(t, session.key, authorization)
	if err != nil {
		t.Fatalf("redeem %s's device login: %v", person, err)
	}
	session.renewed = time.Now()
	return session
}

// tradeAnswer is what the broker answered a trade: the token and its expiry, or the refusal.
type tradeAnswer struct {
	status      int
	token       string
	expires     time.Time
	description string
}

// trade trades the session's device login for a token of the session listing relayLiveProject,
// renewing the login's own token first when it is near its 30 s end.
func (r *relayLiveRig) trade(t *testing.T, s *relayLiveSession) tradeAnswer {
	t.Helper()
	if time.Since(s.renewed) > relayLiveTokenLife/2 {
		renewed, err := s.login.Refresh(t, s.key, s.machine)
		if err != nil {
			t.Fatalf("renew %s's device login: %v", s.id, err)
		}
		s.machine, s.renewed = renewed, time.Now()
	}
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"client_id":            {oauthwire.ExchangeClientID},
		"subject_token":        {s.machine.AccessToken},
		"subject_token_type":   {oauthwire.SubjectTokenTypeAccessToken},
		"session":              {s.id},
		oauthwire.FormProjects: {relayLiveProject},
	}
	request, err := http.NewRequest(http.MethodPost, r.broker.URL+"/v1/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build the trade: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	asked := time.Now()
	response, err := s.signer.Do(request)
	if err != nil {
		t.Fatalf("trade %s's device login: %v", s.id, err)
	}
	defer response.Body.Close()
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		t.Fatalf("decode the trade's answer (%d): %v", response.StatusCode, err)
	}
	return tradeAnswer{status: response.StatusCode, token: answer.AccessToken,
		expires: asked.Add(time.Duration(answer.ExpiresIn) * time.Second), description: answer.Description}
}

// mustTrade is trade, failing the row unless the broker minted the token.
func (r *relayLiveRig) mustTrade(t *testing.T, s *relayLiveSession) tradeAnswer {
	t.Helper()
	answer := r.trade(t, s)
	if answer.status != http.StatusOK || answer.token == "" {
		t.Fatalf("the trade of %s was answered %d %q", s.id, answer.status, answer.description)
	}
	return answer
}

// revoke ends token at the broker, as Dispatch relays a person's revoke.
func (r *relayLiveRig) revoke(t *testing.T, token string) {
	t.Helper()
	form := url.Values{"token": {token}, "client_id": {oauthwire.ExchangeClientID}}
	request, err := http.NewRequest(http.MethodPost, r.broker.URL+"/v1/oauth/revoke", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build the revoke: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+r.broker.UIToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("revoke the token: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the revoke was answered %d %s", response.StatusCode, body)
	}
}

// revokeLogin ends s's device login at the broker, as Dispatch relays an owner's Reset or a
// removal from the project's list: the person's revoke of the machine login they approved, which
// ends every token traded from it.
func (r *relayLiveRig) revokeLogin(t *testing.T, s *relayLiveSession, person string) {
	t.Helper()
	if status, body := r.broker.UI(t, http.MethodPost, "/v1/launcher-credentials/"+s.credential+"/revoke-by-approver", map[string]any{"approver": person}); status != http.StatusOK {
		t.Fatalf("the revoke of %s's device login was answered %d %s", s.id, status, body)
	}
}

// call sends one request to replica edge as session s presenting token, signed as its real client
// signs one, and answers the status and the body.
func (r *relayLiveRig) call(t *testing.T, s *relayLiveSession, edge *listenerProcess, token, method, path, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, "http://127.0.0.1:"+strconv.Itoa(edge.port)+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "DPoP "+token)
	response, err := s.signer.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(answer)
}

// register registers s on replica A with token at epoch, so A holds its consumer and relays it.
func (r *relayLiveRig) register(t *testing.T, s *relayLiveSession, token, epoch string) {
	t.Helper()
	if status, answer := r.call(t, s, r.a, token, http.MethodPost, "/v1/interests/subscribe", consumerRegistration(s.id, epoch)); status != http.StatusOK {
		t.Fatalf("the registration of %s on A was answered %d %s", s.id, status, answer)
	}
}

// upgradeCapture keeps the request the DPoP transport signed, instead of sending it.
type upgradeCapture struct{ last *http.Request }

func (c *upgradeCapture) RoundTrip(r *http.Request) (*http.Response, error) {
	c.last = r
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
}

// dial opens s's relay socket on replica edge with token, answering the socket, nil when the
// upgrade was refused, and the upgrade's response.
func (r *relayLiveRig) dial(t *testing.T, s *relayLiveSession, edge *listenerProcess, token string) (*relaySocketClient, *http.Response, error) {
	t.Helper()
	origin := "127.0.0.1:" + strconv.Itoa(edge.port)
	capture := &upgradeCapture{}
	signing, err := http.NewRequest(http.MethodGet, "http://"+origin+relaySocketPath(s.id), nil)
	if err != nil {
		t.Fatalf("build the upgrade: %v", err)
	}
	signing.Header.Set("Authorization", "DPoP "+token)
	if _, err := (&http.Client{Transport: cipherclient.NewDPoPTransport(s.key, capture)}).Do(signing); err != nil {
		t.Fatalf("sign the upgrade: %v", err)
	}
	header := http.Header{"Authorization": {capture.last.Header.Get("Authorization")}, "DPoP": {capture.last.Header.Get("DPoP")}}
	dialer := websocket.Dialer{Subprotocols: []string{relaySubprotocol}, HandshakeTimeout: 10 * time.Second}
	conn, response, err := dialer.Dial("ws://"+origin+relaySocketPath(s.id), header)
	if err != nil {
		return nil, response, err
	}
	return startRelaySocketClient(t, conn), response, nil
}

// open opens s's socket on replica B with token and subscribes its relay subject, as the client
// does, returning once a ping sent after the sub has its pong: once the subscription is held.
func (r *relayLiveRig) open(t *testing.T, s *relayLiveSession, token string) *relaySocketClient {
	t.Helper()
	client, response, err := r.dial(t, s, r.b, token)
	if err != nil {
		t.Fatalf("open the relay socket of %s on B: %v (%s)", s.id, err, describeUpgrade(response))
	}
	client.sub(1, bus.DeliverSubject(s.id))
	client.ping()
	select {
	case <-client.pongs:
	case err := <-client.closed:
		t.Fatalf("the socket of %s closed before its first pong: %v", s.id, err)
	case <-time.After(10 * time.Second):
		t.Fatalf("the socket of %s answered no ping within 10 s", s.id)
	}
	return client
}

// publish stores a message to s's agent subject as Dispatch's user, and answers it.
func (r *relayLiveRig) publish(t *testing.T, session string) contracts.Envelope {
	t.Helper()
	subject := contracts.LegacyAgentSubject(session)
	item := listenerTestEnvelope(subject, "relay-live-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if _, err := r.dispatch.JS().Publish(subject, marshalListenerEnvelope(t, item)); err != nil {
		t.Fatalf("store a message to %s: %v", session, err)
	}
	return item
}

// envelopeOf is the envelope a msg frame carries.
func envelopeOf(t *testing.T, frame relayFrameRead) contracts.Envelope {
	t.Helper()
	if frame.Op != "msg" {
		t.Fatalf("the socket sent %s, want a msg", frame.raw)
	}
	var item contracts.Envelope
	if err := json.Unmarshal(frame.data(t), &item); err != nil {
		t.Fatalf("the frame %s carries no envelope: %v", frame.raw, err)
	}
	return item
}

// answer answers a msg frame received and then done, as a session that wrote it does.
func answer(client *relaySocketClient, frame relayFrameRead) {
	client.pub(frame.Reply, relayReplyReceived)
	client.pub(frame.Reply, relayReplyDone)
}

// requireClosed waits up to within for the server to close client with code and reason, passing
// over the frames it sends meanwhile, and answers when the close arrived; what names the change
// the close follows, which a failure repeats.
func requireClosed(t *testing.T, client *relaySocketClient, within time.Duration, code int, reason, what string) time.Time {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case err := <-client.closed:
			var closed *websocket.CloseError
			if !errors.As(err, &closed) {
				t.Fatalf("%s: the socket ended with %v, not a close frame", what, err)
			}
			if closed.Code != code || closed.Text != reason {
				t.Fatalf("%s: the socket closed %d %q, want %d %q", what, closed.Code, closed.Text, code, reason)
			}
			return time.Now()
		case <-client.frames:
		case <-deadline:
			t.Fatalf("%s: the socket was still open %s later, want it closed %d %q", what, within, code, reason)
			return time.Time{}
		}
	}
}

// Row 1: a message to S is stored, replica A relays it, replica B's socket forwards it with its
// receipt subject under bus.RelayReplyPrefix, the client answers done over the socket, B publishes
// that on the receipt subject, and A acks the message: nothing is sent again through the
// consumer's ack wait, the socket reopened with a fresh token whenever its token expires. The
// server refused nothing under the rendered grants. Controls: the replicas rendered
// natsgrant.Listener, and the row fails naming the server's refusal of the replica's own inbox
// prefix (natsgrant.EdgeInboxPrefix); the edge
// grant rendered without its subscribe of the receipts, and the row fails naming the frame sent
// again.
func TestARelaySocketCarriesFramesAndReceiptsUnderTheEdgeGrant(t *testing.T) {
	t.Parallel()
	rig := newRelayLiveRig(t, relayLiveOptions{})
	s := rig.login(t, "ses_live_carried", relayLivePerson)
	token := rig.mustTrade(t, s)
	client := rig.open(t, s, token.token)
	rig.register(t, s, token.token, "epoch-1")

	sent := rig.publish(t, s.id)
	frame := client.next(15 * time.Second)
	if got := envelopeOf(t, frame); got.EventID != sent.EventID || frame.sid(t) != 1 {
		t.Fatalf("the socket sent %s, want the message %s under sid 1", frame.raw, sent.EventID)
	}
	if !strings.HasPrefix(frame.Reply, bus.RelayReplyPrefix) {
		t.Fatalf("the frame's receipt subject is %q, want one under %s", frame.Reply, bus.RelayReplyPrefix)
	}
	answer(client, frame)
	answered := time.Now()

	until := answered.Add(bus.SessionConsumerAckWait + 5*time.Second)
	for time.Now().Before(until) {
		select {
		case again := <-client.frames:
			t.Fatalf("the message %s was sent again %s after it was answered done: %s", sent.EventID, time.Since(answered).Round(time.Millisecond), again.raw)
		case err := <-client.closed:
			// The token's 30 s ended; the client reopens with a fresh one, as it does.
			var closed *websocket.CloseError
			if !errors.As(err, &closed) || closed.Code != relaySocketCloseExpired {
				t.Fatalf("the socket closed %v while the row watched for the frame again, want only its token's expiry", err)
			}
			client = rig.open(t, s, rig.mustTrade(t, s).token)
		case <-time.After(time.Until(until)):
		}
	}
	rig.requireNoViolation(t)
}

// Row 2: a sub to another session's relay subject and to the session's own agent-stream control
// subject are each answered subject_not_granted, so a frame published to the other session never
// reaches the socket, and a pub to a receipt subject the socket never forwarded is
// publish_not_granted. Control: the subject check removed; B subscribes the other session's relay
// subject, and the row fails naming the frame that reached the socket.
func TestARelaySocketRefusesAnotherSessionsSubjects(t *testing.T) {
	t.Parallel()
	rig := newRelayLiveRig(t, relayLiveOptions{})
	s := rig.login(t, "ses_live_refused", relayLivePerson)
	const other = "ses_live_someone_else"
	client := rig.open(t, s, rig.mustTrade(t, s).token)

	for sid, subject := range map[int]string{2: bus.DeliverSubject(other), 3: "agentstream." + s.id + ".control"} {
		client.sub(sid, subject)
		select {
		case frame := <-client.frames:
			if frame.Op != "err" || frame.Code != relayErrSubjectNotGranted || frame.sid(t) != int64(sid) {
				t.Fatalf("a sub to %s was answered %s, want err %s for sid %d", subject, frame.raw, relayErrSubjectNotGranted, sid)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("a sub to %s was answered nothing within 3 s, want err %s", subject, relayErrSubjectNotGranted)
		}
	}
	stray := &natsgo.Msg{Subject: bus.DeliverSubject(other), Reply: bus.RelayReplyPrefix + "stray.1", Header: natsgo.Header{}, Data: []byte(`{"stray":true}`)}
	if err := rig.dispatch.Connection().PublishMsg(stray); err != nil {
		t.Fatalf("publish a frame of %s: %v", other, err)
	}
	select {
	case frame := <-client.frames:
		t.Fatalf("B subscribed %s for this socket: the other session's frame reached it as %s", bus.DeliverSubject(other), frame.raw)
	case <-time.After(2 * time.Second):
	}

	client.pub(bus.RelayReplyPrefix+"never.forwarded", relayReplyDone)
	if frame := client.next(5 * time.Second); frame.Op != "err" || frame.Code != relayErrPublishNotGranted {
		t.Fatalf("a pub to a receipt subject never forwarded was answered %s, want err %s", frame.raw, relayErrPublishNotGranted)
	}
	rig.requireNoViolation(t)
}

// Row 3: a send to S from a second session makes A probe S's relay subject, which B's socket
// subscribes; the send reads S connected, and the client is sent the message and no probe frame.
// The probe's reply is A's inbox, under the edge machine's inbox prefix and no relay receipt, so
// forward's filters of the probe's header and of the stray reply each keep it from the client:
// the control that makes the row fail is both removed ("the socket forwarded the connection
// probe"); either alone removed, the other stops the probe and the row passes. Its second half is
// its own control: the socket closed first, and a send reads S stopped once B's unsubscribe has
// reached the server, within 10 s.
func TestAProbeOfASocketSessionReadsConnected(t *testing.T) {
	t.Parallel()
	rig := newRelayLiveRig(t, relayLiveOptions{})
	s := rig.login(t, "ses_live_probed", relayLivePerson)
	sender := rig.login(t, "ses_live_sender", relayLivePerson)
	token := rig.mustTrade(t, s).token
	client := rig.open(t, s, token)
	rig.register(t, s, token, "epoch-1")
	senderToken := rig.mustTrade(t, sender).token

	send := func() string {
		t.Helper()
		status, body := rig.call(t, sender, rig.a, senderToken, http.MethodPost, "/v1/messages/send",
			`{"target_session":"`+s.id+`","source_session":"`+sender.id+`","source":"agent","message":"are you there?"}`)
		if status != http.StatusOK {
			t.Fatalf("the send to %s was answered %d %s", s.id, status, body)
		}
		var answer struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal([]byte(body), &answer); err != nil {
			t.Fatalf("decode the send's answer %s: %v", body, err)
		}
		return answer.State
	}
	if state := send(); state != sessionStateConnected {
		t.Fatalf("a send to %s, whose socket subscribes its relay subject, read %q, want %q", s.id, state, sessionStateConnected)
	}
	frame := client.next(15 * time.Second)
	if _, probe := frame.Headers[bus.ConnectionProbeHeader]; probe {
		t.Fatalf("the socket forwarded the connection probe: %s", frame.raw)
	}
	if item := envelopeOf(t, frame); item.PayloadSummary != "are you there?" {
		t.Fatalf("the socket sent %s, want the message the send stored", frame.raw)
	}
	answer(client, frame)
	client.none(2*time.Second, "after the message")

	if err := client.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("close the socket: %v", err)
	}
	<-client.closed
	var state string
	if !testwait.Holds(10*time.Second, 200*time.Millisecond, func() bool {
		state = send()
		return state == sessionStateStopped
	}) {
		t.Fatalf("a send to %s, whose socket closed, read %q for 10s, want %q", s.id, state, sessionStateStopped)
	}
	rig.requireNoViolation(t)
}

// Row 4: with the 30 s tokens and the check every second, the open socket closes 4401 within
// relayLiveCloseBound after its token's expiry and no sooner; a message stored after the close
// reaches S only once it opens again with a fresh token, and the message it held unanswered at the
// close is sent again, so no frame is lost. Control: the check loop removed; the socket stays open
// past the expiry, and the row fails naming how long it waited.
func TestAnOpenSocketClosesWhenItsTokenExpires(t *testing.T) {
	t.Parallel()
	rig := newRelayLiveRig(t, relayLiveOptions{})
	s := rig.login(t, "ses_live_expired", relayLivePerson)
	token := rig.mustTrade(t, s)
	client := rig.open(t, s, token.token)
	rig.register(t, s, token.token, "epoch-1")

	held := rig.publish(t, s.id)
	if got := envelopeOf(t, client.next(15*time.Second)); got.EventID != held.EventID {
		t.Fatalf("the socket sent %s, want %s", got.EventID, held.EventID)
	}
	closed := requireClosed(t, client, time.Until(token.expires)+relayLiveCloseBound, relaySocketCloseExpired, relaySocketReasonExpired,
		"its token expiring at "+token.expires.UTC().Format(time.RFC3339))
	if late := closed.Sub(token.expires); late > relayLiveCloseBound || late < -time.Second {
		t.Fatalf("the socket closed %s after its token's expiry, want within %s of it and not before", late.Round(time.Millisecond), relayLiveCloseBound)
	}
	after := rig.publish(t, s.id)

	fresh := rig.mustTrade(t, s)
	reopened := rig.open(t, s, fresh.token)
	rig.register(t, s, fresh.token, "epoch-2")
	seen := map[string]bool{}
	deadline := time.Now().Add(20 * time.Second)
	for !seen[held.EventID] || !seen[after.EventID] {
		if time.Now().After(deadline) {
			t.Fatalf("after the reopen the socket was sent %v, want both %s (unanswered at the close) and %s (stored after it)", seen, held.EventID, after.EventID)
		}
		frame := reopened.next(time.Until(deadline))
		seen[envelopeOf(t, frame).EventID] = true
		answer(reopened, frame)
	}
	rig.requireNoViolation(t)
}

// Row 5: the broker revokes S's token (POST /v1/oauth/revoke); the open socket closes 4403 within
// relayLiveCloseBound of the revoke, long before its expiry, and the revoked token opens no
// socket again. Control: the check loop removed, as row 4's.
func TestAnOpenSocketClosesWhenItsTokenIsRevoked(t *testing.T) {
	t.Parallel()
	rig := newRelayLiveRig(t, relayLiveOptions{})
	s := rig.login(t, "ses_live_revoked", relayLivePerson)
	token := rig.mustTrade(t, s)
	client := rig.open(t, s, token.token)
	rig.register(t, s, token.token, "epoch-1")

	rig.revoke(t, token.token)
	revoked := time.Now()
	closed := requireClosed(t, client, relayLiveCloseBound, relaySocketCloseRefused, relaySocketReasonRefused, "its token's revoke")
	if closed.After(token.expires) {
		t.Fatalf("the socket closed at %s, after its token's expiry %s: the revoke closed nothing", closed, token.expires)
	}
	t.Logf("the socket closed %s after the revoke", closed.Sub(revoked).Round(time.Millisecond))
	if _, response, err := rig.dial(t, s, rig.b, token.token); err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an upgrade with the revoked token = %v (%s), want 401", err, describeUpgrade(response))
	}
	rig.requireNoViolation(t)
}

// Row 5b: an owner's Reset, as a removal does, makes Dispatch relay the broker's revoke of each
// device login the person approved (POST /v1/launcher-credentials/{id}/revoke-by-approver), which
// ends every token traded from it. The open socket closes 4403 within relayLiveCloseBound of the
// revoke, long before its token's expiry; the traded token opens no socket again, and the login's
// refresh is refused, so the machine trades for no other token. Control, in the same row: the other
// person's socket, whose login stands, stays open; and the check loop removed, the socket stays
// open past the revoke and the row fails naming how long it waited, as row 4's.
func TestAnOpenSocketClosesAfterAResetRevokesItsLogin(t *testing.T) {
	t.Parallel()
	rig := newRelayLiveRig(t, relayLiveOptions{})
	reset := rig.login(t, "ses_live_reset", relayLivePerson)
	kept := rig.login(t, "ses_live_reset_kept", relayLiveOtherPerson)
	resetToken, keptToken := rig.mustTrade(t, reset), rig.mustTrade(t, kept)
	resetClient, keptClient := rig.open(t, reset, resetToken.token), rig.open(t, kept, keptToken.token)
	rig.register(t, reset, resetToken.token, "epoch-1")

	rig.revokeLogin(t, reset, relayLivePerson)
	revoked := time.Now()
	closed := requireClosed(t, resetClient, relayLiveCloseBound, relaySocketCloseRefused, relaySocketReasonRefused, relayLivePerson+"'s device login's revoke")
	if closed.After(resetToken.expires) {
		t.Fatalf("the socket closed at %s, after its token's expiry %s: the revoke closed nothing", closed, resetToken.expires)
	}
	t.Logf("the socket closed %s after the revoke of the device login", closed.Sub(revoked).Round(time.Millisecond))
	if _, response, err := rig.dial(t, reset, rig.b, resetToken.token); err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an upgrade with the token traded from the revoked login = %v (%s), want 401", err, describeUpgrade(response))
	}
	if _, err := reset.login.Refresh(t, reset.key, reset.machine); err == nil {
		t.Fatal("the revoked device login's refresh was answered with a token, want it refused")
	}
	if window := min(relayLiveCloseBound, time.Until(keptToken.expires)-time.Second); window > 0 {
		keptClient.stillOpen(window, relayLiveOtherPerson+"'s socket, whose login stands")
	}
	rig.requireNoViolation(t)
}

// Row 6: the project-access stand-in stops listing PRIV for S's person. The open socket stays open
// until its token's expiry, since the token stays live until then, and closes 4401, and the reopen's
// trade is refused naming the project once the broker's answer about the person ages out
// (projectaccess.CacheTTL from the removal, the bound the broker gives a removal). Control, in the
// same row: the other person, kept on the list, is traded a fresh token and opens the socket again.
func TestAnOpenSocketClosesWhenItsPersonLosesTheProject(t *testing.T) {
	t.Parallel()
	rig := newRelayLiveRig(t, relayLiveOptions{})
	removed := rig.login(t, "ses_live_removed", relayLivePerson)
	kept := rig.login(t, "ses_live_kept", relayLiveOtherPerson)
	removedToken, keptToken := rig.mustTrade(t, removed), rig.mustTrade(t, kept)
	removedClient, keptClient := rig.open(t, removed, removedToken.token), rig.open(t, kept, keptToken.token)
	rig.register(t, removed, removedToken.token, "epoch-1")
	rig.register(t, kept, keptToken.token, "epoch-1")

	rig.access.SetRow(relayLiveProject, projectaccesstest.Row{Visibility: "private", Owner: "owner@example.com", Listed: []string{relayLiveOtherPerson}})
	removedAt := time.Now()
	closed := requireClosed(t, removedClient, time.Until(removedToken.expires)+relayLiveCloseBound, relaySocketCloseExpired, relaySocketReasonExpired,
		relayLivePerson+"'s token, after the person left "+relayLiveProject)
	if early := removedToken.expires.Sub(closed); early > time.Second {
		t.Fatalf("the socket closed %s before its token's expiry, want it open until then", early.Round(time.Millisecond))
	}
	requireClosed(t, keptClient, time.Until(keptToken.expires)+relayLiveCloseBound, relaySocketCloseExpired, relaySocketReasonExpired, relayLiveOtherPerson+"'s token")

	refused := rig.trade(t, removed)
	for deadline := removedAt.Add(projectaccess.CacheTTL + 5*time.Second); refused.status != http.StatusBadRequest || !strings.Contains(refused.description, relayLiveProject); refused = rig.trade(t, removed) {
		if time.Now().After(deadline) {
			t.Fatalf("the reopen's trade for %s was answered %d %q %s after the removal, want 400 naming %s within %s", relayLivePerson, refused.status, refused.description, time.Since(removedAt).Round(time.Second), relayLiveProject, projectaccess.CacheTTL)
		}
		time.Sleep(time.Second)
	}
	t.Logf("the reopen's trade was refused %s after the removal", time.Since(removedAt).Round(time.Millisecond))
	rig.open(t, kept, rig.mustTrade(t, kept).token)
	rig.requireNoViolation(t)
}

// Row 7, on ciphertokentest's stand-in: the broker answers S's token first with [PRIV], then with
// [INFRA PRIV], and a second token first with [PRIV], then with [INFRA]; each change closes the
// socket 4403 within relayLiveCloseBound. The replicas serve INFRA and PRIV both, so every answer
// passes the edge's admission and only the comparison with the caller the socket was opened as
// closes it. Control: the comparison removed; the socket stays open, and the row fails naming the
// answer.
func TestAnOpenSocketClosesWhenTheAnswersProjectsChange(t *testing.T) {
	t.Parallel()
	rig := newRelayLiveRig(t, relayLiveOptions{stub: true, edgeProjects: "INFRA,PRIV"})
	s := newRelayLiveSession(t, "ses_live_changed")
	jkt, err := proof.Thumbprint(&s.key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint the session's key: %v", err)
	}
	says := func(token string, projects ...string) {
		rig.stub.Says(token, ciphertoken.Introspection{
			Active: true, Session: s.id, Person: relayLivePerson, ProjectScope: ciphertoken.ProjectScopePerson, Projects: projects,
			ClientID: ciphertoken.ExchangeClientID, Thumbprint: jkt, ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
	}
	const first, second = "access-token-first-answered-priv", "access-token-second-answered-priv"
	says(first, relayLiveProject)
	client := rig.open(t, s, first)
	rig.register(t, s, first, "epoch-1")
	says(first, "INFRA", relayLiveProject)
	requireClosed(t, client, relayLiveCloseBound, relaySocketCloseRefused, relaySocketReasonRefused, "the answer [INFRA PRIV]")

	says(second, relayLiveProject)
	client = rig.open(t, s, second)
	says(second, "INFRA")
	requireClosed(t, client, relayLiveCloseBound, relaySocketCloseRefused, relaySocketReasonRefused, "the answer [INFRA]")
	rig.requireNoViolation(t)
}

// Row 8: the broker stops answering the replicas; the open socket closes 1013 within
// relayLiveCloseBound, and a new upgrade is refused 503, so the client comes back rather than
// discards its token. Control: Live's error read as a live answer; the socket stays open, and the
// row fails naming how long it waited.
func TestAnOpenSocketClosesWhileTheBrokerCannotBeAsked(t *testing.T) {
	t.Parallel()
	rig := newRelayLiveRig(t, relayLiveOptions{})
	s := rig.login(t, "ses_live_unasked", relayLivePerson)
	token := rig.mustTrade(t, s)
	client := rig.open(t, s, token.token)
	rig.register(t, s, token.token, "epoch-1")

	rig.proxy.Close()
	requireClosed(t, client, relayLiveCloseBound, websocket.CloseTryAgainLater, relaySocketReasonBrokerUnavailable, "the broker stopped")
	if _, response, err := rig.dial(t, s, rig.b, token.token); err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an upgrade while the broker cannot be asked = %v (%s), want 503", err, describeUpgrade(response))
	}
	rig.requireNoViolation(t)
}
