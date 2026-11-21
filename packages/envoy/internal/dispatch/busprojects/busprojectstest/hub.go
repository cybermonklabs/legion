package busprojectstest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// DispatchAccount is the account the stand-in auth service admits a hub-admission proof for, as
// the auth service admits it only for Dispatch's own account.
const DispatchAccount = "system:serviceaccount:dispatch:dispatch"

// The stand-in's grant to an admitted proof connection: one reply-inbox prefix of its own and one
// ping subject, and nothing else, so the connection holds no project subject, no JetStream and no
// KV. They stand in for the names the auth service's hub-admission grant gives the connection's
// identity.
const (
	ProofInboxPrefix = "_INBOX_hub-admission"
	ProofPingSubject = "envoy.hub-admission.ping"
)

// The refusals the stand-in answers a proof with, labelled as the auth service labels them.
const (
	// RefusedUntagged is a proof on a hub whose authorization request carries no
	// natsgrant.PrivateProjectsServerTag: a hub not rendered with private-project support.
	RefusedUntagged = "hub_admission_untagged"
	// RefusedNotDispatch is a proof whose token names an account other than DispatchAccount.
	RefusedNotDispatch = "hub_admission_not_dispatch"
	// RefusedNotAProof is any other credential: the stand-in admits nothing but proofs. The auth
	// service refuses only some of these: it admits a service's token for its own audience
	// (BROKER_NATS_K8S_AUDIENCE) as an ordinary service connection on every hub
	// (natsgrant.HubAdmissionAudience), so a test asserts this label only for a credential the auth
	// service refuses too, a token of an audience it takes no NATS token at, say.
	RefusedNotAProof = "not_a_hub_admission_proof"
)

// Hub is a NATS server, in this process, with an auth callout, and the stand-in auth service that
// answers its authorization requests. The stand-in admits a connection presenting a hub-admission
// proof token (ProofToken with natsgrant.HubAdmissionAudience) for DispatchAccount, and only when
// the request carries natsgrant.PrivateProjectsServerTag, which the server's configuration carries
// while the hub is tagged; it answers every other connection with a refusal naming its label.
//
// It stands in for the auth service's private-hub admission, which is not written at this head:
// natsgrant names what it will read (natsgrant.HubAdmissionAudience, PrivateProjectsServerTag).
// It decides the proof's rules as natsgrant.HubAdmissionAudience states them, and admits nothing
// else; its grant to a proof stands in for the auth service's hub-admission grant. A test that
// runs the real service there in its place replaces it.
type Hub struct {
	// Server is the hub's server, which Retag restarts.
	Server *testnats.Server
	// URL is the hub's client URL, the one a proof client dials.
	URL string

	keys    testnats.CalloutKeys
	service *natsgo.Conn
	// identity is the configuration lines that name the server and its cluster, which every start
	// of the server keeps (StartClusteredHub), or "" for a lone hub.
	identity string

	mu       sync.Mutex
	refusals []string
	admitted int
}

// StartHub starts a hub, tagged or not, with its stand-in auth service connected and answering.
// Both stop when the test ends.
func StartHub(t testing.TB, tagged bool) *Hub {
	t.Helper()
	return startHub(t, tagged, "")
}

// StartClusteredHub starts the servers of a clustered hub called name, one per entry of tagged,
// tagged or not as it says, each a Hub with a stand-in auth service of its own. Server i is named
// <name>-<i>, after its pod, and reports cluster name in its INFO with no other server, as a
// clustered hub's server does: the hub's cluster advertises no server to its clients
// (no_advertise), so a client's INFO from any server names only that server and the cluster. The
// servers are not routed to each other: a client sees nothing of the routes, and the proof reads
// only the INFO. All stop when the test ends.
func StartClusteredHub(t testing.TB, name string, tagged ...bool) []*Hub {
	t.Helper()
	hubs := make([]*Hub, len(tagged))
	for i, tag := range tagged {
		hubs[i] = startHub(t, tag, fmt.Sprintf("server_name: %q\ncluster { name: %q }\n", fmt.Sprintf("%s-%d", name, i), name))
	}
	return hubs
}

// startHub is StartHub with identity, configuration lines naming the server and its cluster, in
// every start of the server.
func startHub(t testing.TB, tagged bool, identity string) *Hub {
	t.Helper()
	h := &Hub{keys: testnats.NewCalloutKeys(t), identity: identity}
	h.Server, h.URL = testnats.StartWithCalloutConfig(t, h.keys, h.config(tagged))
	curve, err := nkeys.FromCurveSeed([]byte(h.keys.XKeySeed))
	if err != nil {
		t.Fatalf("read the callout's curve seed: %v", err)
	}
	issuer, err := nkeys.FromSeed([]byte(h.keys.IssuerSeed))
	if err != nil {
		t.Fatalf("read the callout's issuer seed: %v", err)
	}
	// testnats.ConnectSeed turns reconnecting off; the stand-in turns it back on, so it answers the
	// hub again after Retag restarts the server.
	reconnect := func(o *natsgo.Options) error { o.AllowReconnect = true; return nil }
	h.service, err = testnats.ConnectSeed(t, h.URL, h.keys.ServiceSeed, reconnect, natsgo.MaxReconnects(-1), natsgo.ReconnectWait(50*time.Millisecond))
	if err != nil {
		t.Fatalf("connect the stand-in auth service: %v", err)
	}
	t.Cleanup(h.service.Close)
	if _, err := h.service.Subscribe("$SYS.REQ.USER.AUTH", func(m *natsgo.Msg) { h.answer(t, curve, issuer, m) }); err != nil {
		t.Fatalf("subscribe the stand-in auth service: %v", err)
	}
	h.awaitService(t)
	return h
}

func (h *Hub) config(tagged bool) string {
	config := testnats.CalloutConfig(h.keys) + h.identity
	if tagged {
		config += fmt.Sprintf("server_tags: [%q]\n", natsgrant.PrivateProjectsServerTag)
	}
	return config
}

// Retag restarts the hub's server tagged or not, as a hub's render changes its tag and rolls its
// server, and returns once the stand-in auth service answers it again. Every connection drops and
// reconnects, and each is decided again.
func (h *Hub) Retag(t testing.TB, tagged bool) {
	t.Helper()
	h.Server.Restart(t, h.config(tagged))
	h.awaitService(t)
}

// StopAuthService closes the stand-in auth service's connection, as every replica of the auth
// service stopping does: a connection the hub admitted stays up, and the hub refuses each new one
// at its authorization timeout.
func (h *Hub) StopAuthService(t testing.TB) {
	t.Helper()
	h.service.Close()
}

// Refusals are the labels of every refusal the stand-in has answered, in order.
func (h *Hub) Refusals() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.refusals)
}

// Admitted is how many connections the stand-in has admitted.
func (h *Hub) Admitted() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.admitted
}

// awaitService waits, within 30 s, until the stand-in's connection is up and the hub has its
// subscription: a round trip after a reconnect follows the subscription nats.go sends again.
func (h *Hub) awaitService(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if h.service.Status() == natsgo.CONNECTED && h.service.FlushTimeout(time.Second) == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stand-in auth service is not connected to %s after 30 s (status %v)", h.URL, h.service.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *Hub) answer(t testing.TB, curve, issuer nkeys.KeyPair, m *natsgo.Msg) {
	opened, err := curve.Open(m.Data, m.Header.Get("Nats-Server-Xkey"))
	if err != nil {
		t.Errorf("stand-in auth service: open the sealed authorization request: %v", err)
		return
	}
	request, err := jwt.DecodeAuthorizationRequestClaims(string(opened))
	if err != nil {
		t.Errorf("stand-in auth service: decode the authorization request: %v", err)
		return
	}
	response := jwt.NewAuthorizationResponseClaims(request.UserNkey)
	response.Audience = request.Server.ID
	if refusal, why := decide(request); refusal != "" {
		response.Error = refusal + ": " + why
		h.mu.Lock()
		h.refusals = append(h.refusals, refusal)
		h.mu.Unlock()
	} else {
		user := jwt.NewUserClaims(request.UserNkey)
		user.Audience = "$G"
		// The stand-in's grant, standing in for the auth service's hub-admission grant: a test
		// of what an admitted proof connection can reach (TestTheProofConnectionHoldsNothing)
		// holds this grant, not a grant the auth service ships.
		user.Pub.Allow.Add(ProofPingSubject)
		user.Sub.Allow.Add(ProofInboxPrefix + ".>")
		user.Expires = time.Now().Add(time.Hour).Unix()
		if response.Jwt, err = user.Encode(issuer); err != nil {
			t.Errorf("stand-in auth service: sign the proof's user: %v", err)
			return
		}
		h.mu.Lock()
		h.admitted++
		h.mu.Unlock()
	}
	token, err := response.Encode(issuer)
	if err != nil {
		t.Errorf("stand-in auth service: sign the answer: %v", err)
		return
	}
	sealed, err := curve.Seal([]byte(token), request.Server.XKey)
	if err != nil {
		t.Errorf("stand-in auth service: seal the answer: %v", err)
		return
	}
	if err := m.Respond(sealed); err != nil && !strings.Contains(err.Error(), "connection closed") {
		t.Errorf("stand-in auth service: answer the authorization request: %v", err)
	}
}

// decide is the stand-in's rule for one request: the refusal label and why, or "" to admit.
func decide(request *jwt.AuthorizationRequestClaims) (refusal, why string) {
	connect := request.ConnectOptions
	if connect.Username != natscred.ServiceUser {
		return RefusedNotAProof, fmt.Sprintf("the CONNECT user is %q, not a service's", connect.Username)
	}
	claims, err := tokenClaims(connect.Password)
	if err != nil {
		return RefusedNotAProof, err.Error()
	}
	if claims.Audience != natsgrant.HubAdmissionAudience {
		return RefusedNotAProof, fmt.Sprintf("the token's audience is %q, not %q", claims.Audience, natsgrant.HubAdmissionAudience)
	}
	if !slices.Contains(request.Server.Tags, natsgrant.PrivateProjectsServerTag) {
		return RefusedUntagged, fmt.Sprintf("the hub's request carries no %q tag (tags %v)", natsgrant.PrivateProjectsServerTag, request.Server.Tags)
	}
	if claims.Subject != DispatchAccount {
		return RefusedNotDispatch, fmt.Sprintf("the token names %q, not Dispatch's account", claims.Subject)
	}
	return "", ""
}

// proofClaims are the claims of a ProofToken the stand-in reads, unverified.
type proofClaims struct {
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	Expiry   int64  `json:"exp"`
}

// tokenClaims reads raw's claims, a JWT the stand-in does not verify.
func tokenClaims(raw string) (proofClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return proofClaims{}, fmt.Errorf("the password is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return proofClaims{}, fmt.Errorf("the token's payload is not base64url: %v", err)
	}
	var claims proofClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return proofClaims{}, fmt.Errorf("the token's payload is not JSON: %v", err)
	}
	return claims, nil
}

// ProofToken is a token in the shape of a projected Kubernetes ServiceAccount token naming account,
// for audience, lapsing in an hour: unsigned, since the stand-in reads its claims without
// verifying them, as natscred reads a token's expiry.
func ProofToken(account, audience string) string {
	encode := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	header := encode(map[string]string{"alg": "none", "typ": "JWT"})
	claims := encode(proofClaims{Subject: account, Audience: audience, Expiry: time.Now().Add(time.Hour).Unix()})
	return header + "." + claims + ".unsigned"
}

// ProofClient is the hub-admission client of the hub at url, its client URL or several
// comma-separated as nats.go takes a server list (NewProofClient), and fails the test when it
// cannot be built.
func ProofClient(t testing.TB, url, token string) *bus.Client {
	t.Helper()
	client, err := NewProofClient(t, strings.Split(url, ","), token)
	if err != nil {
		t.Fatalf("make the hub-admission client of %s: %v", url, err)
	}
	return client
}

// NewProofClient is the hub-admission client of the hub whose servers are servers, or why it
// cannot be built, as cmd/dispatch is to build one per hub of DISPATCH_PRIVATE_HUBS (it builds
// none at this head): a bus client that connects as a service presenting token, which it reads
// from a file at each connect, and that connects in the background, so a hub that refuses it
// leaves it retrying rather than failing its construction. It stays inside the proof's grant, so
// the hub refuses it nothing and it logs no permissions violation: its reply inboxes are under
// ProofInboxPrefix (bus.WithInboxPrefix), and it never reads the stream's subjects, a publish the
// grant refuses (bus.WithoutStreamSubjects). A client it builds closes when the test ends.
func NewProofClient(t testing.TB, servers []string, token string) (*bus.Client, error) {
	t.Helper()
	return newProofClient(t, servers, token, nil)
}

// ProofClientThrough is ProofClient for the hub whose servers are servers, reached through the
// SOCKS5 proxy at proxy (bus.SOCKS5ProxyEnvVar, with bus.AllowRemoteEnvVar set), which resolves
// each server's name: a test names a hub's servers by names no resolver knows and has the proxy
// route each one (testsocks5.StartRouted).
func ProofClientThrough(t testing.TB, proxy string, servers []string, token string) *bus.Client {
	t.Helper()
	client, err := newProofClient(t, servers, token, map[string]string{bus.SOCKS5ProxyEnvVar: proxy, bus.AllowRemoteEnvVar: "1"})
	if err != nil {
		t.Fatalf("make the hub-admission client of %v through %s: %v", servers, proxy, err)
	}
	return client
}

// newProofClient is NewProofClient with environment's variables set besides the token file's.
func newProofClient(t testing.TB, servers []string, token string, environment map[string]string) (*bus.Client, error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "hub-admission-token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatalf("write the hub-admission token: %v", err)
	}
	environment = maps.Clone(environment)
	if environment == nil {
		environment = map[string]string{}
	}
	environment[bus.ServiceTokenFileEnvVar] = file
	client, err := bus.Connect(servers,
		bus.WithName("envoy-dispatch-hub-admission"),
		bus.WithEnvironment(func(name string) (string, bool) { value, ok := environment[name]; return value, ok }),
		bus.WithInboxPrefix(ProofInboxPrefix),
		bus.WithoutStreamSubjects(),
		bus.WithConnectInBackground())
	if err != nil {
		return nil, err
	}
	t.Cleanup(client.Close)
	return client, nil
}
