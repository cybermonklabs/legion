// edge_test.go drives the broker's edge (api.RegisterEdge) against the real broker of
// ciphertest.Rig, mounted on a second httptest.Server beside the Rig's own address: a machine
// outside the deployment logs in at the edge, through the load balancer as it writes
// X-Forwarded-For, and its person decides the login on the routes Dispatch relays to on the
// broker's own address.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// userCodeShape is the shape of every user code the provider mints.
var userCodeShape = regexp.MustCompile(`^[0-9A-Z]{4}-[0-9A-Z]{4}$`)

// edgeBalancer is the address the edge's load balancer appends to X-Forwarded-For after the
// client's: the same for every caller, which is why the edge never keys a bucket on it.
const edgeBalancer = "35.191.0.1"

// outsidePerson is a person outside the Workspace a Rig's edge names in a login.
const outsidePerson = "outside@outside.example"

// throughBalancer is a RoundTripper that sends each request on as the edge's load balancer
// forwards it: X-Forwarded-For holding what the client sent (spoofed, none when empty), then the
// client's address, then edgeBalancer.
type throughBalancer struct {
	client, spoofed string
	next            http.RoundTripper
}

func (b throughBalancer) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Set("X-Forwarded-For", forwardedFor(b.spoofed, b.client))
	next := b.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(out)
}

// forwardedFor is X-Forwarded-For as the load balancer writes it for client, after what the
// client itself sent, spoofed.
func forwardedFor(spoofed, client string) string {
	if spoofed == "" {
		return client + ", " + edgeBalancer
	}
	return spoofed + ", " + client + ", " + edgeBalancer
}

// newEdgeMachine is a machine logging in at rig's edge through via: its OAuth client's endpoints
// are the edge's, and each of its requests carries a DPoP proof of its key naming the edge's URL.
func newEdgeMachine(t *testing.T, rig *ciphertest.Rig, via throughBalancer) *deviceMachine {
	t.Helper()
	key := newSigningKey(t)
	return &deviceMachine{
		rig: rig, key: key,
		config: &oauth2.Config{
			ClientID: oauthwire.ClientID, Scopes: []string{"machine"},
			Endpoint: oauth2.Endpoint{
				DeviceAuthURL: rig.EdgeURL + "/v1/oauth/device_authorization",
				TokenURL:      rig.EdgeURL + "/v1/oauth/token",
				AuthStyle:     oauth2.AuthStyleInParams,
			},
		},
		ctx: context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: cipherclient.NewDPoPTransport(key, via)}),
	}
}

// edgeLogin asks the edge for a device code naming loginHint, with a request object signed for
// the edge's own address.
func (m *deviceMachine) edgeLogin(t *testing.T, loginHint string) (*oauth2.DeviceAuthResponse, error) {
	t.Helper()
	return m.authorize(signMachineLoginRequest(t, m.key, m.rig.EdgeURL, loginHint, "example-host-devbox", ""), m.thumbprint(t))
}

// edgeDeviceForm is a device authorization's form naming loginHint, signed by a key of its own
// for audience.
func edgeDeviceForm(t *testing.T, audience, loginHint string) url.Values {
	t.Helper()
	key := newSigningKey(t)
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return url.Values{
		"client_id": {oauthwire.ClientID}, "scope": {oauthwire.Scope}, "dpop_jkt": {jkt},
		"request": {signMachineLoginRequest(t, key, audience, loginHint, "example-host-devbox", "")},
	}
}

// postEdge posts form to path on base with headers and answers the status and the OAuth body.
func postEdge(t *testing.T, base, path string, form url.Values, headers map[string]string) (int, map[string]any) {
	t.Helper()
	all := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	maps.Copy(all, headers)
	answer := send(t, http.DefaultClient, request(t, http.MethodPost, base+path, strings.NewReader(form.Encode()), all))
	var body map[string]any
	if err := json.Unmarshal(answer.body, &body); err != nil {
		t.Fatalf("POST %s answered %d %s, which is no JSON object: %v", path, answer.status, answer.body, err)
	}
	return answer.status, body
}

// loginRows counts the rows a device login writes: its credential request and its device code.
func loginRows(t *testing.T, rig *ciphertest.Rig) int {
	t.Helper()
	var n int
	if err := rig.Store.Pool.QueryRow(context.Background(),
		`select (select count(*) from credential_requests) + (select count(*) from oauth_device_codes)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// refusedWith fails t unless err is an OAuth refusal with status and the broker's code.
func refusedWith(t *testing.T, what string, err error, status int, code string) {
	t.Helper()
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) || re.Response.StatusCode != status || !strings.HasPrefix(re.ErrorDescription, code+": ") {
		t.Fatalf("%s = %v, want %d %s", what, err, status, code)
	}
}

// TestTheEdgeKeysBucketsOnTheGoogleClientAddress: every bucket of the edge keys on the address
// the load balancer appends for the client it accepted, the second-to-last X-Forwarded-For entry,
// and on nothing the caller wrote: a client that spends its address's burst is refused the next
// login whatever it puts in front of that entry, and however it writes its address, while another
// client behind the same balancer is served from a bucket of its own. A request whose header
// holds fewer than two entries did not come through the balancer, and is refused 400
// REQUEST_INVALID before it writes anything.
//
// The named reverts: key the edge on the last entry (clientAddress), and the second client is
// refused; key it on the first, and the spoofed request is served.
func TestTheEdgeKeysBucketsOnTheGoogleClientAddress(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	const client = "203.0.113.7"
	burst := api.DefaultLauncherLimits.PerAddress.Burst
	for i := range burst {
		m := newEdgeMachine(t, rig, throughBalancer{client: client, spoofed: fmt.Sprintf("198.51.100.%d", i+1)})
		if _, err := m.edgeLogin(t, fmt.Sprintf("person-%d@outside.example", i)); err != nil {
			t.Fatalf("edge login %d of %d from %s: %v", i+1, burst, client, err)
		}
	}
	for _, spoofed := range []string{"", "192.0.2.99", "198.51.100.200, 192.0.2.1"} {
		m := newEdgeMachine(t, rig, throughBalancer{client: client, spoofed: spoofed})
		_, err := m.edgeLogin(t, "late@outside.example")
		refusedWith(t, fmt.Sprintf("an edge login from %s past its burst, X-Forwarded-For %q", client, forwardedFor(spoofed, client)), err, http.StatusTooManyRequests, "RATE_LIMITED")
	}
	m := newEdgeMachine(t, rig, throughBalancer{client: "::ffff:" + client})
	_, err := m.edgeLogin(t, "late@outside.example")
	refusedWith(t, "an edge login from "+client+" written as IPv6", err, http.StatusTooManyRequests, "RATE_LIMITED")

	other := newEdgeMachine(t, rig, throughBalancer{client: "203.0.113.8"})
	if _, err := other.edgeLogin(t, "other@outside.example"); err != nil {
		t.Fatalf("an edge login from another client behind the same balancer = %v, want it served from that client's own bucket", err)
	}

	before := loginRows(t, rig)
	for _, header := range []map[string]string{{}, {"X-Forwarded-For": "203.0.113.9"}, {"X-Forwarded-For": "not-an-address, " + edgeBalancer}} {
		status, body := postEdge(t, rig.EdgeURL, "/v1/oauth/device_authorization", edgeDeviceForm(t, rig.EdgeURL, "third@outside.example"), header)
		if status != http.StatusBadRequest || body["code"] != "REQUEST_INVALID" {
			t.Errorf("an edge login with %v = %d %v, want 400 REQUEST_INVALID", header, status, body)
		}
	}
	if after := loginRows(t, rig); after != before {
		t.Fatalf("a refused edge login wrote %d rows, want none", after-before)
	}
}

// TestTheEdgeRefusesMoreThanOneForwardedForLine: a login request that carries X-Forwarded-For as
// more than one header line is refused 400 REQUEST_INVALID before it spends a bucket or writes a
// row, whichever line holds the balancer's entries, since which line a balancer appends to is not
// documented. One real client sends more than its burst of logins, each with the balancer's
// entries on the first line and a line of its own after them: every one is refused, none is
// served and none is rate-limited. The control is the same count sent as one line by another
// client: the burst is served and the rest are 429 RATE_LIMITED, so the one-line edge is as before.
//
// The named revert: read the last line of several, and the client's own trailing line picks a
// new bucket for each login, so all of them are served past the burst.
func TestTheEdgeRefusesMoreThanOneForwardedForLine(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	burst := api.DefaultLauncherLimits.PerAddress.Burst
	login := func(person string, lines ...string) answered {
		t.Helper()
		req := request(t, http.MethodPost, rig.EdgeURL+"/v1/oauth/device_authorization", strings.NewReader(edgeDeviceForm(t, rig.EdgeURL, person).Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		for _, line := range lines {
			req.Header.Add("X-Forwarded-For", line)
		}
		return send(t, http.DefaultClient, req)
	}
	const client, control = "203.0.113.150", "203.0.113.151"
	before := loginRows(t, rig)
	for i := range burst + 5 {
		answer := login(fmt.Sprintf("trailing-line-%d@outside.example", i), forwardedFor("", client), fmt.Sprintf("198.51.100.%d, 198.51.100.200", i+1))
		if answer.status != http.StatusBadRequest || !strings.Contains(string(answer.body), `"code":"REQUEST_INVALID"`) {
			t.Errorf("edge login %d of %d from %s with the balancer's line first and a line of the client's after it = %d %s, want 400 REQUEST_INVALID", i+1, burst+5, client, answer.status, answer.body)
		}
	}
	for _, lines := range [][]string{{"198.51.100.7, 198.51.100.8", forwardedFor("", client)}, {forwardedFor("", client), forwardedFor("", client)}} {
		if answer := login("two-lines@outside.example", lines...); answer.status != http.StatusBadRequest || !strings.Contains(string(answer.body), `"code":"REQUEST_INVALID"`) {
			t.Errorf("an edge login with X-Forwarded-For lines %q = %d %s, want 400 REQUEST_INVALID", lines, answer.status, answer.body)
		}
	}
	if after := loginRows(t, rig); after != before {
		t.Fatalf("the refused edge logins wrote %d rows, want none", after-before)
	}
	served, limited := 0, 0
	for i := range burst + 5 {
		answer := login(fmt.Sprintf("one-line-%d@outside.example", i), fmt.Sprintf("198.51.100.%d, %s, %s", i+1, control, edgeBalancer))
		switch {
		case answer.status == http.StatusOK:
			served++
		case answer.status == http.StatusTooManyRequests && strings.Contains(string(answer.body), `"code":"RATE_LIMITED"`):
			limited++
		default:
			t.Errorf("one-line edge login %d from %s = %d %s, want 200 or 429 RATE_LIMITED", i+1, control, answer.status, answer.body)
		}
	}
	// A token the bucket refills while the loop runs may serve one login past the burst, so the
	// control asks for the burst served and the rest refused, and not for an exact split.
	if served < burst || limited == 0 || served+limited != burst+5 {
		t.Fatalf("%d one-line edge logins from %s: %d served and %d rate-limited, want the burst of %d served and the rest 429", burst+5, control, served, limited, burst)
	}
}

// TestTheEdgeGateReadsTheRequestObjectTheLoginIsMadeOf: a device authorization may carry its
// request parameter twice, and the edge's Workspace gate reads the value the login is made of.
// With a Workspace person's object first, the gate refuses it 400 EDGE_WORKSPACE_LOGIN; with an
// outside person's object first and a Workspace person's after it, signed by the same key, no
// login of the Workspace person is opened, and any login the broker opens is the outside
// person's.
//
// The named revert: read the last request value in the gate, and the first row is served and
// opens a login awaiting the Workspace person.
func TestTheEdgeGateReadsTheRequestObjectTheLoginIsMadeOf(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	header := map[string]string{"X-Forwarded-For": forwardedFor("", "203.0.113.90")}
	workspace := "ada@" + ciphertest.SignInDomain
	const outside = "two-values@outside.example"
	key := newSigningKey(t)
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	form := func(first, second string) url.Values {
		return url.Values{
			"client_id": {oauthwire.ClientID}, "scope": {oauthwire.Scope}, "dpop_jkt": {jkt},
			"request": {
				signMachineLoginRequest(t, key, rig.EdgeURL, first, "example-host-devbox", ""),
				signMachineLoginRequest(t, key, rig.EdgeURL, second, "example-host-devbox", ""),
			},
		}
	}
	pending := func(approver string) int {
		t.Helper()
		status, body := rig.UI(t, http.MethodGet, "/v1/pending?approver="+url.QueryEscape(approver), nil)
		listed := decode[struct {
			Pending []struct {
				RecordID string `json:"record_id"`
			} `json:"pending"`
		}](t, body)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/pending for %s = %d %s", approver, status, body)
		}
		return len(listed.Pending)
	}
	if status, body := postEdge(t, rig.EdgeURL, "/v1/oauth/device_authorization", form(workspace, outside), header); status != http.StatusBadRequest || body["code"] != "EDGE_WORKSPACE_LOGIN" {
		t.Errorf("an edge login whose first request names %s = %d %v, want 400 EDGE_WORKSPACE_LOGIN", workspace, status, body)
	}
	status, body := postEdge(t, rig.EdgeURL, "/v1/oauth/device_authorization", form(outside, workspace), header)
	if n := pending(workspace); n != 0 {
		t.Fatalf("the edge's two logins with two request values each (the second answered %d %v) left %d logins awaiting %s, want none", status, body, n, workspace)
	}
	if status == http.StatusOK {
		if n := pending(outside); n != 1 {
			t.Fatalf("the edge served the login whose first request names %s, and %d logins await that person, want 1", outside, n)
		}
	}
	t.Logf("an edge login whose first request names %s and second %s: %d %v", outside, workspace, status, body)
}

// TestAnEdgeFloodLeavesTheTailnetLoginBucket: the edge's address and login buckets are its own.
// A caller that spends a person's login bucket at the edge, and then its own address's bucket
// there, under the very address the broker's own address keys the same caller on, leaves the
// person and that address their whole bucket on the broker's own address: a person of the
// deployment logging a machine in there is never slowed by what the internet sends the edge.
//
// The named revert: build the edge's server on the launcher limiter of the broker's own address,
// and the login on the broker's own address is refused 429.
func TestAnEdgeFloodLeavesTheTailnetLoginBucket(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	const person = "flooded@outside.example"
	// 127.0.0.1 is the address the broker's own address keys this test's requests on: the Rig
	// sets no BROKER_TRUSTED_PROXY_HEADER, so it keys on r.RemoteAddr.
	via := throughBalancer{client: "127.0.0.1"}
	perLogin, perAddress := api.DefaultLauncherLimits.PerLogin.Burst, api.DefaultLauncherLimits.PerAddress.Burst
	spent := 0
	for range perLogin {
		if _, err := newEdgeMachine(t, rig, via).edgeLogin(t, person); err != nil {
			t.Fatalf("edge login naming %s: %v", person, err)
		}
		spent++
	}
	_, err := newEdgeMachine(t, rig, via).edgeLogin(t, person)
	refusedWith(t, "an edge login naming "+person+" past its burst", err, http.StatusTooManyRequests, "RATE_LIMITED")
	spent++
	for i := spent; i < perAddress; i++ {
		if _, err := newEdgeMachine(t, rig, via).edgeLogin(t, fmt.Sprintf("filler-%d@outside.example", i)); err != nil {
			t.Fatalf("edge login %d from 127.0.0.1: %v", i+1, err)
		}
	}
	_, err = newEdgeMachine(t, rig, via).edgeLogin(t, "one-more@outside.example")
	refusedWith(t, "an edge login from 127.0.0.1 past its burst", err, http.StatusTooManyRequests, "RATE_LIMITED")

	for i := range perLogin {
		m := newDeviceMachine(t, rig)
		if _, err := m.authorize(m.request(t, person, ""), m.thumbprint(t)); err != nil {
			t.Fatalf("login %d of %d naming %s on the broker's own address after the edge's flood = %v, want it served", i+1, perLogin, person, err)
		}
	}
}

// TestTheEdgeRefusesAWorkspaceLoginHint: a device authorization at the edge naming a person of
// the Workspace — an address on BROKER_SIGNIN_DOMAIN or a BROKER_SIGNIN_WORKSPACE_DOMAINS domain,
// on a subdomain of one, in capitals, with a trailing dot, in fullwidth letters IDNA maps onto the
// domain, with an empty label, or a login that is no address at all — is 400 EDGE_WORKSPACE_LOGIN.
// None of them spends a bucket or writes a row: after them all the same client's whole burst of
// logins for people outside the Workspace is served, and no row was written for the refused ones.
// A body of any type but a form is refused 400 REQUEST_INVALID before it is read, since the
// provider's library would read a multipart body the gate had not.
//
// The named revert: take the Workspace check out of the edge's device gate, and every row is
// served 200.
func TestTheEdgeRefusesAWorkspaceLoginHint(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	const client = "203.0.113.20"
	header := map[string]string{"X-Forwarded-For": forwardedFor("", client)}
	for _, hint := range []string{
		"ada@" + ciphertest.SignInDomain,
		"bob@" + ciphertest.EdgeWorkspaceDomain,
		"carol@eng." + ciphertest.SignInDomain,
		" Ada@EXAMPLE.com ",
		"ada@example.com.",
		"ada@ｅｘａｍｐｌｅ.com",
		"ada@example.com..",
		"ada",
	} {
		status, body := postEdge(t, rig.EdgeURL, "/v1/oauth/device_authorization", edgeDeviceForm(t, rig.EdgeURL, hint), header)
		if status != http.StatusBadRequest || body["error"] != "invalid_request" || body["code"] != "EDGE_WORKSPACE_LOGIN" {
			t.Errorf("an edge login naming %q = %d %v, want 400 invalid_request EDGE_WORKSPACE_LOGIN", hint, status, body)
		}
	}
	multipartBody, contentType := multipartForm(t, edgeDeviceForm(t, rig.EdgeURL, "ada@"+ciphertest.SignInDomain))
	answer := send(t, http.DefaultClient, request(t, http.MethodPost, rig.EdgeURL+"/v1/oauth/device_authorization", multipartBody,
		map[string]string{"Content-Type": contentType, "X-Forwarded-For": forwardedFor("", client)}))
	if answer.status != http.StatusBadRequest || !strings.Contains(string(answer.body), "REQUEST_INVALID") {
		t.Errorf("a multipart edge login naming a person of the Workspace = %d %s, want 400 REQUEST_INVALID", answer.status, answer.body)
	}
	if rows := loginRows(t, rig); rows != 0 {
		t.Fatalf("the refused edge logins wrote %d rows, want none", rows)
	}
	for i := range api.DefaultLauncherLimits.PerAddress.Burst {
		m := newEdgeMachine(t, rig, throughBalancer{client: client})
		if _, err := m.edgeLogin(t, fmt.Sprintf("person-%d@outside.example", i)); err != nil {
			t.Fatalf("edge login %d from %s after the refusals = %v, want the whole burst served: a refusal spent the address's bucket", i+1, client, err)
		}
	}
}

// multipartForm is form as a multipart/form-data body, with its Content-Type.
func multipartForm(t *testing.T, form url.Values) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, name := range slices.Sorted(maps.Keys(form)) {
		if err := writer.WriteField(name, form.Get(name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, writer.FormDataContentType()
}

// TestTheEdgeTokenRouteRefusesPodAndWorkflowTrades: the edge's token route takes the device code,
// a refresh token and the trade of one of the broker's own access tokens with no actor token, and
// answers every other grant 400 unsupported_grant_type naming EDGE_GRANT_REFUSED: a pod's, a
// workflow run's, a machine's and a person's Google trades, an access token traded with an actor
// of either kind, a subject token type given twice or not at all, every other grant RFC 6749
// names, and a grant_type given twice or as a list. The pod's trade, which the broker's own address
// serves, is refused at the edge with a DPoP proof of its URL as well.
//
// The named revert: take the grant check out of the edge's token gate, and the pod's trade mints a
// token at the edge.
func TestTheEdgeTokenRouteRefusesPodAndWorkflowTrades(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	exchange := func(subjectToken, subjectType string) url.Values {
		return url.Values{
			"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "client_id": {oauthwire.ExchangeClientID},
			"subject_token": {subjectToken}, oauth.FormSubjectTokenType: {subjectType}, oauthwire.FormSession: {ciphertest.ExchangeSession},
		}
	}
	podTrade := func() url.Values {
		return exchange(rig.MintSubjectToken(t, ciphertest.ExchangeAccount, uuid.NewString()), oauthwire.SubjectTokenTypeKubernetes)
	}
	with := func(form url.Values, set map[string][]string) url.Values {
		maps.Copy(form, set)
		return form
	}
	rows := []struct {
		name string
		form url.Values
	}{
		{"a pod's trade", podTrade()},
		{"a workflow run's trade", exchange(rig.MintWorkflowToken(t, ciphertest.ExchangeRepository, ciphertest.ExchangeWorkflow, ciphertest.ExchangeRef, "1"), oauthwire.SubjectTokenTypeGitHub)},
		{"a machine's Google trade", exchange("a-google-identity-token", oauthwire.SubjectTokenTypeGoogle)},
		{"a person's Google trade", with(exchange("a-person-token", oauthwire.SubjectTokenTypePerson), map[string][]string{"actor_token": {"a-machine-token"}, "actor_token_type": {oauthwire.ActorTokenTypeMachine}})},
		{"an access token traded with an actor", with(exchange("an-access-token", oauthwire.SubjectTokenTypeAccessToken), map[string][]string{"actor_token": {"a-machine-token"}, "actor_token_type": {oauthwire.ActorTokenTypeMachine}})},
		{"an access token traded with an actor token type", with(exchange("an-access-token", oauthwire.SubjectTokenTypeAccessToken), map[string][]string{"actor_token_type": {oauthwire.ActorTokenTypeMachine}})},
		{"an access token traded with an empty actor token", with(exchange("an-access-token", oauthwire.SubjectTokenTypeAccessToken), map[string][]string{"actor_token": {""}})},
		{"two subject token types", with(exchange("an-access-token", oauthwire.SubjectTokenTypeAccessToken), map[string][]string{oauth.FormSubjectTokenType: {oauthwire.SubjectTokenTypeAccessToken, oauthwire.SubjectTokenTypeKubernetes}})},
		{"no subject token type", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "client_id": {oauthwire.ExchangeClientID}, "subject_token": {"an-access-token"}}},
		{"client credentials", url.Values{"grant_type": {"client_credentials"}, "client_id": {oauthwire.ExchangeClientID}}},
		{"an authorization code", url.Values{"grant_type": {"authorization_code"}, "code": {"x"}, "client_id": {oauthwire.ClientID}}},
		{"a password", url.Values{"grant_type": {"password"}, "username": {"x"}, "password": {"y"}, "client_id": {oauthwire.ClientID}}},
		{"a JWT bearer", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {"x"}}},
		{"two grant types", url.Values{"grant_type": {"refresh_token", "urn:ietf:params:oauth:grant-type:device_code"}, "client_id": {oauthwire.ClientID}}},
		{"a list of grant types", url.Values{"grant_type": {"refresh_token urn:ietf:params:oauth:grant-type:device_code"}, "client_id": {oauthwire.ClientID}}},
		{"no grant type", url.Values{"client_id": {oauthwire.ClientID}}},
	}
	header := map[string]string{"X-Forwarded-For": forwardedFor("", "203.0.113.30")}
	for _, row := range rows {
		status, body := postEdge(t, rig.EdgeURL, "/v1/oauth/token", row.form, header)
		if status != http.StatusBadRequest || body["error"] != "unsupported_grant_type" || body["code"] != "EDGE_GRANT_REFUSED" {
			t.Errorf("%s at the edge = %d %v, want 400 unsupported_grant_type EDGE_GRANT_REFUSED", row.name, status, body)
		}
	}

	// The control: the broker's own address mints a token for the pod's trade, so what refuses
	// it at the edge is the edge's gate and not the trade.
	key := newSigningKey(t)
	tailnet := send(t, &http.Client{Transport: cipherclient.NewDPoPTransport(key, http.DefaultTransport)},
		request(t, http.MethodPost, rig.URL+"/v1/oauth/token", strings.NewReader(podTrade().Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
	if tailnet.status != http.StatusOK || !strings.Contains(string(tailnet.body), `"access_token"`) {
		t.Fatalf("the pod's trade on the broker's own address = %d %s, want a token", tailnet.status, tailnet.body)
	}
	edge := send(t, &http.Client{Transport: cipherclient.NewDPoPTransport(key, throughBalancer{client: "203.0.113.30"})},
		request(t, http.MethodPost, rig.EdgeURL+"/v1/oauth/token", strings.NewReader(podTrade().Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
	if edge.status != http.StatusBadRequest || !strings.Contains(string(edge.body), "unsupported_grant_type") {
		t.Fatalf("the pod's trade at the edge with a proof of the edge's URL = %d %s, want 400 unsupported_grant_type", edge.status, edge.body)
	}
}

// TestTheEdgeAnswersTheOutsideVerificationURI: a device authorization at the edge sends its person
// to BROKER_EDGE_MACHINE_LOGIN_URL, the page a person outside the deployment can reach, and one on
// the broker's own address goes on sending its person to BROKER_MACHINE_LOGIN_URL.
//
// The named revert: answer the provider's own verification URL at the edge.
func TestTheEdgeAnswersTheOutsideVerificationURI(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	da, err := newEdgeMachine(t, rig, throughBalancer{client: "203.0.113.40"}).edgeLogin(t, outsidePerson)
	if err != nil {
		t.Fatalf("edge login: %v", err)
	}
	if da.VerificationURI != ciphertest.EdgeMachineLoginURL || da.VerificationURIComplete != "" {
		t.Fatalf("the edge's verification_uri = %q, verification_uri_complete = %q; want %q and none", da.VerificationURI, da.VerificationURIComplete, ciphertest.EdgeMachineLoginURL)
	}
	tailnet := newDeviceMachine(t, rig).login(t)
	if tailnet.VerificationURI != ciphertest.MachineLoginURL {
		t.Fatalf("the broker's own address answers verification_uri %q, want %q", tailnet.VerificationURI, ciphertest.MachineLoginURL)
	}
}

// TestTheEdgeAnswersTheSameShapeForAKnownAndAnUnknownLoginHint: a device authorization at the edge
// tells its caller nothing of whether the person it names is one the broker knows. A person whose
// machine login the broker approved and who holds a launcher credential, and a person the broker
// has never seen, are each answered 200 with the same members, the same page, the same lifetime
// and polling interval, and a user code of the same shape.
func TestTheEdgeAnswersTheSameShapeForAKnownAndAnUnknownLoginHint(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	const known, unknown = "known@outside.example", "unknown@outside.example"
	first, err := newEdgeMachine(t, rig, throughBalancer{client: "203.0.113.50"}).edgeLogin(t, known)
	if err != nil {
		t.Fatalf("the known person's first edge login: %v", err)
	}
	if credentialID := rig.DecideMachineLoginAs(t, known, first.UserCode, true); credentialID == "" {
		t.Fatal("the known person's first login minted no credential")
	}
	header := map[string]string{"X-Forwarded-For": forwardedFor("", "203.0.113.51")}
	answers := map[string]map[string]any{}
	for _, person := range []string{known, unknown} {
		status, body := postEdge(t, rig.EdgeURL, "/v1/oauth/device_authorization", edgeDeviceForm(t, rig.EdgeURL, person), header)
		if status != http.StatusOK {
			t.Fatalf("an edge login naming %s = %d %v, want 200", person, status, body)
		}
		answers[person] = body
	}
	knownKeys, unknownKeys := slices.Sorted(maps.Keys(answers[known])), slices.Sorted(maps.Keys(answers[unknown]))
	if !slices.Equal(knownKeys, unknownKeys) {
		t.Fatalf("the edge answers %s with %v and %s with %v, want the same members", known, knownKeys, unknown, unknownKeys)
	}
	for _, member := range []string{"verification_uri", "expires_in", "interval"} {
		if answers[known][member] != answers[unknown][member] {
			t.Errorf("%s is %v for %s and %v for %s, want the same", member, answers[known][member], known, answers[unknown][member], unknown)
		}
	}
	for _, person := range []string{known, unknown} {
		if code, _ := answers[person]["user_code"].(string); !userCodeShape.MatchString(code) {
			t.Errorf("the user code for %s is %q, want one of the shape %s", person, code, userCodeShape)
		}
	}
}

// TestTheEdgeLoginIsDecidedOnTheTailnetAndUsedAtTheEdge drives one outside person's machine
// through the edge end to end. Its login is listed for that person and looked up and approved on
// the routes Dispatch relays to on the broker's own address; its machine polls and refreshes at the
// edge, and trades its access token there; and its launcher credential enrols a session on the
// broker's own address, through the chain every credential is verified by. A proof at the edge
// names the edge's URL: one naming the broker's own is refused there. A request object is signed
// for the listener it is sent to: one signed for the broker's own address is refused at the edge,
// and one signed for the edge on the broker's own address.
func TestTheEdgeLoginIsDecidedOnTheTailnetAndUsedAtTheEdge(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	via := throughBalancer{client: "203.0.113.60"}
	m := newEdgeMachine(t, rig, via)
	da, err := m.edgeLogin(t, outsidePerson)
	if err != nil {
		t.Fatalf("edge login: %v", err)
	}
	status, body := rig.UI(t, http.MethodGet, "/v1/pending?approver="+url.QueryEscape(outsidePerson), nil)
	if pending := decode[struct {
		Pending []struct {
			RecordID string `json:"record_id"`
			Kind     string `json:"kind"`
		} `json:"pending"`
	}](t, body); status != http.StatusOK || len(pending.Pending) != 1 || pending.Pending[0].Kind != "launcher_credential" {
		t.Fatalf("GET /v1/pending for %s = %d %s, want the edge's login listed", outsidePerson, status, body)
	}
	credentialID := rig.DecideMachineLoginAs(t, outsidePerson, da.UserCode, true)
	tok := m.poll(t, da)
	if got, _ := tok.Extra("credential_id").(string); got != credentialID {
		t.Fatalf("the edge's token names credential %v, want %s", tok.Extra("credential_id"), credentialID)
	}
	refreshed, err := m.refresh(tok)
	if err != nil {
		t.Fatalf("refresh at the edge: %v", err)
	}

	// A refresh with a proof naming the broker's own address, sent to the edge.
	edgeURL, err := url.Parse(rig.EdgeURL)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshed.RefreshToken}, "client_id": {oauthwire.ClientID}}
	crossed := send(t, &http.Client{Transport: cipherclient.NewDPoPTransport(m.key, throughBalancer{client: via.client, next: rewriteTo{base: edgeURL}})},
		request(t, http.MethodPost, rig.URL+"/v1/oauth/token", strings.NewReader(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
	if crossed.status != http.StatusBadRequest || !strings.Contains(string(crossed.body), "invalid_dpop_proof") {
		t.Fatalf("a refresh at the edge with a proof of the broker's own URL = %d %s, want 400 invalid_dpop_proof", crossed.status, crossed.body)
	}

	trade := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "client_id": {oauthwire.ExchangeClientID},
		"subject_token": {refreshed.AccessToken}, oauth.FormSubjectTokenType: {oauthwire.SubjectTokenTypeAccessToken},
		oauthwire.FormSession: {"outside-session"},
	}
	traded := send(t, &http.Client{Transport: cipherclient.NewDPoPTransport(m.key, via)},
		request(t, http.MethodPost, rig.EdgeURL+"/v1/oauth/token", strings.NewReader(trade.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
	if traded.status != http.StatusOK || !strings.Contains(string(traded.body), `"access_token"`) {
		t.Fatalf("the access token's trade at the edge = %d %s, want a token", traded.status, traded.body)
	}
	// The token that trade minted is the exchange client's own, as a Google machine's and a person
	// sign-in's token is: the library refuses the exchange client a trade of its own tokens
	// (invalid_grant), before the edge's own trade check (edgeTokenHooks), which refuses any token
	// but a device login's EDGE_GRANT_REFUSED behind it.
	var minted struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(traded.body, &minted); err != nil {
		t.Fatal(err)
	}
	retraded := tradeAt(t, rig.EdgeURL, cipherclient.NewDPoPTransport(m.key, via), minted.AccessToken, "outside-session-again")
	if retraded.status != http.StatusBadRequest || strings.Contains(string(retraded.body), `"access_token"`) {
		t.Fatalf("the trade at the edge of a token another trade minted = %d %s, want 400 and no token", retraded.status, retraded.body)
	}

	sessionKey := newSigningKey(t)
	sessionThumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	launcherProof, err := proof.SignLauncher(m.key, credentialID, http.MethodPost, rig.URL+"/v1/enrollments", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if status, body := rig.Req(t, http.MethodPost, "/v1/enrollments", map[string]string{"Proof": launcherProof}, map[string]any{
		"kind": "session", "runtime_id": "session-" + uuid.NewString(), "operator": outsidePerson, "thumbprint": sessionThumbprint,
	}); status != http.StatusCreated {
		t.Fatalf("POST /v1/enrollments on the broker's own address with the edge login's credential = %d %s, want 201", status, body)
	}

	header := map[string]string{"X-Forwarded-For": forwardedFor("", "203.0.113.61")}
	if status, body := postEdge(t, rig.EdgeURL, "/v1/oauth/device_authorization", edgeDeviceForm(t, rig.URL, outsidePerson), header); status != http.StatusBadRequest || body["code"] != "REQUEST_INVALID" {
		t.Errorf("an edge login signed for the broker's own address = %d %v, want 400 REQUEST_INVALID", status, body)
	}
	if status, body := postEdge(t, rig.URL, "/v1/oauth/device_authorization", edgeDeviceForm(t, rig.EdgeURL, outsidePerson), nil); status != http.StatusBadRequest || body["code"] != "REQUEST_INVALID" {
		t.Errorf("a login on the broker's own address signed for the edge = %d %v, want 400 REQUEST_INVALID", status, body)
	}
}

// tradeAt trades accessToken, one of the broker's own access tokens, at base through transport,
// for a token of session, and answers what the broker answered.
func tradeAt(t *testing.T, base string, transport http.RoundTripper, accessToken, session string) answered {
	t.Helper()
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "client_id": {oauthwire.ExchangeClientID},
		"subject_token": {accessToken}, oauth.FormSubjectTokenType: {oauthwire.SubjectTokenTypeAccessToken},
		oauthwire.FormSession: {session},
	}
	return send(t, &http.Client{Transport: transport},
		request(t, http.MethodPost, base+"/v1/oauth/token", strings.NewReader(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
}

// TestTheEdgeRefusesAWorkspacePersonsMachineLoginOpenedOnTheTailnet: a machine login of a person
// of the Workspace, opened and approved on the broker's own address, is used there alone. At the
// edge, through the load balancer and with proofs of the edge's URL made by the login's own key,
// its refresh and the poll of its device code are refused 400 invalid_grant EDGE_WORKSPACE_LOGIN,
// and the trade of its access token 400 EDGE_WORKSPACE_LOGIN; the refused refresh moves nothing,
// so the login refreshes and trades on the broker's own address afterwards as before.
//
// The named revert: drop edgeTokenHooks from RegisterEdge, and the edge refreshes the login and
// trades its token for one acting as the Workspace person.
func TestTheEdgeRefusesAWorkspacePersonsMachineLoginOpenedOnTheTailnet(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	via := throughBalancer{client: "198.51.100.9"}
	atEdge := func(m *deviceMachine) *deviceMachine {
		edge := newEdgeMachine(t, rig, via)
		edge.key = m.key
		edge.ctx = context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: cipherclient.NewDPoPTransport(m.key, via)})
		return edge
	}

	m := newDeviceMachine(t, rig)
	da := m.login(t)
	rig.DecideMachineLogin(t, da.UserCode, true)
	tok := m.poll(t, da)

	_, err := atEdge(m).refresh(tok)
	refusedWith(t, fmt.Sprintf("a refresh at the edge of %s's machine login opened on the broker's own address", rig.Operator), err, http.StatusBadRequest, "EDGE_WORKSPACE_LOGIN")
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode != "invalid_grant" {
		t.Errorf("the edge refused the refresh with %q, want invalid_grant", re.ErrorCode)
	}
	traded := tradeAt(t, rig.EdgeURL, cipherclient.NewDPoPTransport(m.key, via), tok.AccessToken, "ada-from-the-internet")
	if traded.status != http.StatusBadRequest || !strings.Contains(string(traded.body), `"code":"EDGE_WORKSPACE_LOGIN"`) {
		t.Errorf("the trade at the edge of %s's access token = %d %s, want 400 EDGE_WORKSPACE_LOGIN", rig.Operator, traded.status, traded.body)
	}

	// The poll of a device code: a second login of the same person, opened and approved on the
	// broker's own address, polled at the edge.
	second := newDeviceMachine(t, rig)
	secondDA := second.login(t)
	rig.DecideMachineLogin(t, secondDA.UserCode, true)
	secondDA.Interval = 1
	edge := atEdge(second)
	_, err = edge.config.DeviceAccessToken(edge.ctx, secondDA)
	refusedWith(t, fmt.Sprintf("the poll at the edge of %s's device code approved on the broker's own address", rig.Operator), err, http.StatusBadRequest, "EDGE_WORKSPACE_LOGIN")

	// On the broker's own address the first login still refreshes and trades.
	refreshed, err := m.refresh(tok)
	if err != nil {
		t.Fatalf("the refresh on the broker's own address after the edge refused one: %v", err)
	}
	ownTrade := tradeAt(t, rig.URL, cipherclient.NewDPoPTransport(m.key, http.DefaultTransport), refreshed.AccessToken, "ada-on-the-tailnet")
	if ownTrade.status != http.StatusOK || !strings.Contains(string(ownTrade.body), `"access_token"`) {
		t.Fatalf("the trade on the broker's own address of %s's access token = %d %s, want a token", rig.Operator, ownTrade.status, ownTrade.body)
	}
}
