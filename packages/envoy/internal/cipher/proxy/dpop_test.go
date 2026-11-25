// packages/envoy/internal/cipher/proxy/dpop_test.go
//
// The rows of a host whose rules entry is of kind dpop: a capability the broker mints and holds
// (requests.MintedKind). The command holds the minted grant's placeholder alone and sends it as
// its one Authorization header, DPoP <placeholder>; the proxy swaps in the grant's token, signs a
// DPoP proof of the pod's own key for the bound host's origin, and sends the request without the
// command's forwarding headers. The rig is rig_test.go's, with the fake grant source holding one
// minted grant.
package proxy

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/dpopproof"
)

// The one minted grant the dpop rows are served by: its grant id and session, the token the
// broker minted for it, and a path its entry allows.
const (
	mintedName    = "TEST_MINTED_TOKEN"
	mintedGrantID = "testkind:00000000-0000-4000-8000-000000000001"
	mintedSession = "ses_minted_test"
	// mintedCheckRunID is the check run of the CI job the grant was made for (the grant's ci
	// claim), which every line of its calls names.
	mintedCheckRunID = "97"
	mintedToken      = "the-token-the-broker-minted-for-the-grant"
	mintedPath       = "/api/v1/projects/ACME/results"
)

// mintedPlaceholder is a minted grant's placeholder in the shape the broker mints them.
var mintedPlaceholder = placeholder.MintedPrefix("testkind") + "QqRrSsTtUuVvWwXxYyZz0123456789ab"

// dpopRules is the rules file of a broker whose airlock holds TEST_MINTED_TOKEN: kind dpop,
// bound to the rig's host, carried in Authorization alone, allowing POST under the projects
// route.
func dpopRules() proxyrules.Rules {
	return proxyrules.Rules{Secrets: []proxyrules.SecretRule{{
		Name: mintedName, Hosts: []string{boundHost}, Headers: []string{"Authorization"},
		Allow: []proxyrules.Allow{{Method: http.MethodPost, PathPrefix: "/api/v1/projects/"}},
		Kind:  proxyrules.KindDPoP,
	}}}
}

// mintedConnection is what the broker answers for a minted grant's placeholder: the grant, its
// session, the minted mark, no enrollment, and the one name whose one slot is the placeholder.
func mintedConnection() requests.AirlockConnection {
	return requests.AirlockConnection{
		GrantID: mintedGrantID, SessionID: mintedSession, CheckRunID: mintedCheckRunID, Minted: true,
		Names: map[string]requests.AirlockGrantName{mintedName: {Slots: map[string]string{requests.SlotValue: mintedPlaceholder}}},
	}
}

// newDPoPRig is the rig behind the dpop rules, with the pod's own key, and the fake grant source
// holding the one minted grant and the token minted for it.
func newDPoPRig(t *testing.T) (*rig, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, Options{Rules: dpopRules(), DPoPKey: key})
	r.grants.setConnection(mintedPlaceholder, mintedConnection())
	r.grants.setValues(mintedGrantID, storedValues(mintedName, mintedToken))
	return r, key
}

// postMinted is one request of the minted grant through the proxy, with the headers given as
// name/value pairs.
func (r *rig) postMinted(headers ...string) (*http.Response, string) {
	r.t.Helper()
	return r.do(r.client(mintedPlaceholder), http.MethodPost, "https://"+boundHost+mintedPath, `{"verdict":"pass"}`, headers...)
}

// refusedAs fails t unless the answer is the proxy's own 403 with code, saying nothing was sent.
func refusedAs(t *testing.T, what string, answer *http.Response, body, code string) {
	t.Helper()
	if answer.StatusCode != http.StatusForbidden || answer.Header.Get("Cipher-Proxy-Code") != code || answer.Header.Get("Cipher-Proxy-Forwarded") != "false" {
		t.Errorf("%s = %d, Cipher-Proxy-Code %q, Cipher-Proxy-Forwarded %q (%s); want 403 %s, not forwarded", what, answer.StatusCode, answer.Header.Get("Cipher-Proxy-Code"), answer.Header.Get("Cipher-Proxy-Forwarded"), body, code)
	}
}

// checkProof fails t unless the request the stand-in received carries one DPoP header: a proof
// signed by key, naming the method POST, the bound host's origin and the allowed path, and the
// hash of the grant's token.
func checkProof(t *testing.T, seen seenRequest, key *ecdsa.PrivateKey) {
	t.Helper()
	proofs := seen.Header.Values("DPoP")
	if len(proofs) != 1 {
		t.Fatalf("the service received %d DPoP headers %q, want one", len(proofs), proofs)
	}
	signed, err := jose.ParseSigned(proofs[0], []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(signed.Signatures) != 1 || signed.Signatures[0].Protected.JSONWebKey == nil {
		t.Fatalf("the DPoP header is no proof with an embedded key: %v", err)
	}
	embedded := signed.Signatures[0].Protected.JSONWebKey
	got, err := embedded.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	want, err := (&jose.JSONWebKey{Key: &key.PublicKey}).Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("the proof's key is not the pod's")
	}
	payload, err := signed.Verify(embedded)
	if err != nil {
		t.Fatalf("the proof does not verify against its own key: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["htm"] != http.MethodPost || claims["htu"] != "https://"+boundHost+mintedPath || claims["ath"] != dpopproof.TokenHash(mintedToken) {
		t.Errorf("the proof's claims = %v, want htm POST, htu https://%s%s and the hash of the grant's token", claims, boundHost, mintedPath)
	}
}

// TestARequestWithoutThePlaceholderIsRefused: on a dpop host, a request with no Authorization,
// with DPoP and a string that is not the placeholder, with the placeholder as a Bearer token, and
// with two Authorization headers, the placeholder and another, is each refused 403 NO_PLACEHOLDER,
// and the service receives none of them; DPoP <placeholder> reaches it.
func TestARequestWithoutThePlaceholderIsRefused(t *testing.T) {
	r, _ := newDPoPRig(t)
	for _, c := range []struct {
		name    string
		headers []string
	}{
		{"no Authorization", nil},
		{"DPoP and another string", []string{"Authorization", "DPoP not-the-placeholder"}},
		{"the placeholder as a Bearer token", []string{"Authorization", "Bearer " + mintedPlaceholder}},
	} {
		answer, body := r.postMinted(c.headers...)
		refusedAs(t, c.name, answer, body, codeNoPlaceholder)
	}
	request, err := http.NewRequest(http.MethodPost, "https://"+boundHost+mintedPath, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Add("Authorization", "DPoP "+mintedPlaceholder)
	request.Header.Add("Authorization", "DPoP not-the-placeholder")
	answer, err := r.client(mintedPlaceholder).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	refusedAs(t, "two Authorization headers", answer, bodyOf(t, answer), codeNoPlaceholder)
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service received %d requests, want none", len(seen))
	}
	if answer, body := r.postMinted("Authorization", "DPoP "+mintedPlaceholder); answer.StatusCode != http.StatusOK || len(r.requests()) != 1 {
		t.Fatalf("DPoP <placeholder> = %d %s and %d requests received, want 200 and one", answer.StatusCode, body, len(r.requests()))
	}
}

// commandForwardingHeaders are the six forwarding headers a command may send, as name/value pairs.
var commandForwardingHeaders = []string{
	"Forwarded", "for=203.0.113.9", "X-Forwarded-For", "203.0.113.9", "X-Forwarded-Host", "evil.example",
	"X-Forwarded-Proto", "http", "X-Real-IP", "203.0.113.9", "Via", "1.1 evil.example",
}

// TestADPoPRequestLeavesWithoutTheCommandsForwardingHeaders: an allowed write carrying the six
// forwarding headers reaches the service with none of them, so the service reads the broker's
// address and not one the command wrote; the same headers sent to a host whose entry has no kind
// reach it as they were sent.
func TestADPoPRequestLeavesWithoutTheCommandsForwardingHeaders(t *testing.T) {
	r, _ := newDPoPRig(t)
	answer, body := r.postMinted(append([]string{"Authorization", "DPoP " + mintedPlaceholder}, commandForwardingHeaders...)...)
	if answer.StatusCode != http.StatusOK || len(r.requests()) != 1 {
		t.Fatalf("the write = %d %s, want 200 and one request received", answer.StatusCode, body)
	}
	seen := r.requests()[0]
	for i := 0; i < len(commandForwardingHeaders); i += 2 {
		if got := seen.Header.Values(commandForwardingHeaders[i]); len(got) != 0 {
			t.Errorf("the service received %s %q, want it dropped", commandForwardingHeaders[i], got)
		}
	}

	plain := newRig(t, Options{})
	answer, body = plain.get(plain.client(workerPlaceholder), "https://"+boundHost+"/v1/x", append([]string{"Authorization", "Bearer " + workerPlaceholder}, commandForwardingHeaders...)...)
	if answer.StatusCode != http.StatusOK || len(plain.requests()) != 1 {
		t.Fatalf("the plain entry's request = %d %s, want 200 and one request received", answer.StatusCode, body)
	}
	for i := 0; i < len(commandForwardingHeaders); i += 2 {
		if got := plain.requests()[0].Header.Get(commandForwardingHeaders[i]); got != commandForwardingHeaders[i+1] {
			t.Errorf("the plain entry's service received %s %q, want %q as the command sent it", commandForwardingHeaders[i], got, commandForwardingHeaders[i+1])
		}
	}
}

// TestAnUpgradeToADPoPHostIsRefused: a write asking to upgrade, with Connection: Upgrade and
// Upgrade: websocket, and with Upgrade: websocket alone, each carrying the placeholder, is refused
// 403 PROTOCOL_SWITCH_REFUSED and reaches no service. An HTTP/2 client sends no Upgrade header,
// so the row speaks HTTP/1.1 under both runs of the suite.
func TestAnUpgradeToADPoPHostIsRefused(t *testing.T) {
	r, _ := newDPoPRig(t)
	r.protocol = protocolHTTP1
	for _, c := range []struct {
		name    string
		headers []string
	}{
		{"Connection: Upgrade with Upgrade: websocket", []string{"Connection", "Upgrade", "Upgrade", "websocket"}},
		{"Upgrade: websocket alone", []string{"Upgrade", "websocket"}},
	} {
		answer, body := r.postMinted(append([]string{"Authorization", "DPoP " + mintedPlaceholder}, c.headers...)...)
		refusedAs(t, c.name, answer, body, codeProtocolSwitch)
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service received %d requests, want none", len(seen))
	}
}

// TestACommandsOwnDPoPProofNeverReachesDispatch: a write carrying a DPoP header of the command's
// own reaches the service with one DPoP header, the proof of the pod's key, and the grant's token in
// Authorization in place of the placeholder.
func TestACommandsOwnDPoPProofNeverReachesDispatch(t *testing.T) {
	r, key := newDPoPRig(t)
	answer, body := r.postMinted("Authorization", "DPoP "+mintedPlaceholder, "DPoP", "the.commands.own-proof")
	if answer.StatusCode != http.StatusOK || len(r.requests()) != 1 {
		t.Fatalf("the write = %d %s, want 200 and one request received", answer.StatusCode, body)
	}
	seen := r.requests()[0]
	if got := seen.Header.Values("Authorization"); len(got) != 1 || got[0] != "DPoP "+mintedToken {
		t.Fatalf("the service received Authorization %q, want the grant's token alone", got)
	}
	checkProof(t, seen, key)
}

// TestNoKindLeavesADPoPHostItsKind: under Options.NoKind, the negative control that reads every
// other entry's kind as none, a dpop host keeps its kind: the allowed write still arrives with one
// DPoP proof of the pod's key and none of the command's forwarding headers, and a write without
// the placeholder is still refused 403 NO_PLACEHOLDER with nothing sent.
func TestNoKindLeavesADPoPHostItsKind(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, Options{Rules: dpopRules(), DPoPKey: key, NoKind: true})
	r.grants.setConnection(mintedPlaceholder, mintedConnection())
	r.grants.setValues(mintedGrantID, storedValues(mintedName, mintedToken))
	answer, body := r.postMinted()
	refusedAs(t, "a write with no placeholder under NoKind", answer, body, codeNoPlaceholder)
	answer, body = r.postMinted(append([]string{"Authorization", "DPoP " + mintedPlaceholder}, commandForwardingHeaders...)...)
	if answer.StatusCode != http.StatusOK || len(r.requests()) != 1 {
		t.Fatalf("the write under NoKind = %d %s and %d requests received, want 200 and one", answer.StatusCode, body, len(r.requests()))
	}
	seen := r.requests()[0]
	checkProof(t, seen, key)
	for i := 0; i < len(commandForwardingHeaders); i += 2 {
		if got := seen.Header.Values(commandForwardingHeaders[i]); len(got) != 0 {
			t.Errorf("the service received %s %q under NoKind, want it dropped", commandForwardingHeaders[i], got)
		}
	}
}

// TestAProxyWithNoDPoPKeySendsNothingToADPoPHost: a proxy given no DPoP key can make no proof, so a
// write to a dpop host is refused 403 NO_VALUE_RELEASED, saying nothing was sent, and the service
// receives nothing.
func TestAProxyWithNoDPoPKeySendsNothingToADPoPHost(t *testing.T) {
	r := newRig(t, Options{Rules: dpopRules()})
	r.grants.setConnection(mintedPlaceholder, mintedConnection())
	r.grants.setValues(mintedGrantID, storedValues(mintedName, mintedToken))
	answer, body := r.postMinted("Authorization", "DPoP "+mintedPlaceholder)
	refusedAs(t, "a write through a proxy with no DPoP key", answer, body, codeNoValue)
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service received %d requests, want none", len(seen))
	}
}

// TestAProxyLineNamesTheGrant: the line of a minted grant's request names the grant, the check
// run of the CI job it was made for and no enrollment, and a refusal's line names the grant alone
// as its caller, with that check run.
func TestAProxyLineNamesTheGrant(t *testing.T) {
	r, _ := newDPoPRig(t)
	if answer, body := r.postMinted("Authorization", "DPoP "+mintedPlaceholder); answer.StatusCode != http.StatusOK {
		t.Fatalf("the write = %d %s, want 200", answer.StatusCode, body)
	}
	call := r.logs.callsAtLeast(t, 1)[0].fields
	if call["grant"] != mintedGrantID || call["ci_check_run_id"] != mintedCheckRunID || call["secret"] != mintedName || call["path"] != mintedPath || call["enrollment"] != "" {
		t.Errorf("the call line = %v, want grant %s, ci_check_run_id %s, secret %s, path %s and no enrollment", call, mintedGrantID, mintedCheckRunID, mintedName, mintedPath)
	}
	answer, body := r.postMinted()
	refusedAs(t, "a write with no placeholder", answer, body, codeNoPlaceholder)
	refusals := r.logs.refusals()
	if len(refusals) != 1 || refusals[0].fields["caller"] != mintedGrantID || refusals[0].fields["ci_check_run_id"] != mintedCheckRunID {
		t.Fatalf("the refusal lines = %v, want one naming the caller %s and ci_check_run_id %s", refusals, mintedGrantID, mintedCheckRunID)
	}
}

// TestAProxysOwnAnswerSaysWhetherItForwarded: the proxy's own refusal says Cipher-Proxy-Forwarded:
// false and its code, and a forwarded answer says Cipher-Proxy-Forwarded: true and carries no
// Cipher-Proxy-Code, even when the service sent both headers to pass for the proxy.
func TestAProxysOwnAnswerSaysWhetherItForwarded(t *testing.T) {
	r, _ := newDPoPRig(t)
	answer, body := r.postMinted()
	refusedAs(t, "a write with no placeholder", answer, body, codeNoPlaceholder)
	r.answerWith(func(w http.ResponseWriter, _ *http.Request, _ proxyrulestest.Request) bool {
		w.Header().Set("Cipher-Proxy-Forwarded", "false")
		w.Header().Set("Cipher-Proxy-Code", "SPOOFED")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"stored":true}`)
		return true
	})
	answer, body = r.postMinted("Authorization", "DPoP "+mintedPlaceholder)
	if answer.StatusCode != http.StatusCreated || answer.Header.Get("Cipher-Proxy-Forwarded") != "true" || answer.Header.Values("Cipher-Proxy-Code") != nil {
		t.Fatalf("the forwarded answer = %d, Cipher-Proxy-Forwarded %q, Cipher-Proxy-Code %q (%s); want 201, true and none", answer.StatusCode, answer.Header.Get("Cipher-Proxy-Forwarded"), answer.Header.Values("Cipher-Proxy-Code"), body)
	}
}

// TestAnAnswerAfterTheWriteMayHaveLeftSaysItsOutcomeIsUnknown: an answer the proxy writes after
// the command's request may have left it says Cipher-Proxy-Forwarded: unknown, with its code, so the
// command reads it as a write that may have landed and asks again under its key; one written
// before anything left says false. The rows: the service reads the whole write and aborts its
// answer, as a reset after the write was sent does (502); the service holds the whole write and
// the grant ends before it answers (407); and a dial that fails, which sends nothing (502, false).
func TestAnAnswerAfterTheWriteMayHaveLeftSaysItsOutcomeIsUnknown(t *testing.T) {
	t.Run("the service read the write and aborted its answer", func(t *testing.T) {
		r, _ := newDPoPRig(t)
		r.answerWith(func(http.ResponseWriter, *http.Request, proxyrulestest.Request) bool {
			panic(http.ErrAbortHandler)
		})
		answer, body := r.postMinted("Authorization", "DPoP "+mintedPlaceholder)
		if received := len(r.requests()); received != 1 {
			t.Fatalf("the service received %d writes, want the one", received)
		}
		if answer.StatusCode != http.StatusBadGateway || answer.Header.Get("Cipher-Proxy-Forwarded") != "unknown" || answer.Header.Get("Cipher-Proxy-Code") != codeUpstream {
			t.Errorf("a write the service received and whose answer it aborted = %d, Cipher-Proxy-Forwarded %q, Cipher-Proxy-Code %q (%s); want 502, unknown, %s", answer.StatusCode, answer.Header.Get("Cipher-Proxy-Forwarded"), answer.Header.Get("Cipher-Proxy-Code"), body, codeUpstream)
		}
	})
	t.Run("the grant ended while the service held the write", func(t *testing.T) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		r := newRig(t, Options{Rules: dpopRules(), DPoPKey: key, Watch: watchFast})
		r.grants.setConnection(mintedPlaceholder, mintedConnection())
		r.grants.setValues(mintedGrantID, storedValues(mintedName, mintedToken))
		r.answerWith(func(_ http.ResponseWriter, request *http.Request, _ proxyrulestest.Request) bool {
			<-request.Context().Done()
			return true
		})
		type result struct {
			answer *http.Response
			body   string
		}
		done := make(chan result, 1)
		go func() {
			answer, body := r.postMinted("Authorization", "DPoP "+mintedPlaceholder)
			done <- result{answer, body}
		}()
		for deadline := time.Now().Add(cutWait); len(r.requests()) == 0; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the write never reached the service")
			}
		}
		r.grants.refuseConnection(mintedPlaceholder, requests.ErrGrantNotLive)
		var got result
		select {
		case got = <-done:
		case <-time.After(2 * cutWait):
			t.Fatal("the write was not answered after its grant ended")
		}
		if got.answer.StatusCode != http.StatusProxyAuthRequired || got.answer.Header.Get("Cipher-Proxy-Forwarded") != "unknown" || got.answer.Header.Get("Cipher-Proxy-Code") != codeGrantNotLive {
			t.Errorf("a write the service held when its grant ended = %d, Cipher-Proxy-Forwarded %q, Cipher-Proxy-Code %q (%s); want 407, unknown, %s", got.answer.StatusCode, got.answer.Header.Get("Cipher-Proxy-Forwarded"), got.answer.Header.Get("Cipher-Proxy-Code"), got.body, codeGrantNotLive)
		}
	})
	t.Run("a dial that failed sent nothing", func(t *testing.T) {
		r, _ := newDPoPRig(t)
		delete(r.dialTo, net.JoinHostPort(boundHost, "443"))
		answer, body := r.postMinted("Authorization", "DPoP "+mintedPlaceholder)
		if received := len(r.requests()); received != 0 {
			t.Fatalf("the service received %d writes, want none", received)
		}
		if answer.StatusCode != http.StatusBadGateway || answer.Header.Get("Cipher-Proxy-Forwarded") != "false" || answer.Header.Get("Cipher-Proxy-Code") != codeUpstream {
			t.Errorf("a write whose dial failed = %d, Cipher-Proxy-Forwarded %q, Cipher-Proxy-Code %q (%s); want 502, false, %s", answer.StatusCode, answer.Header.Get("Cipher-Proxy-Forwarded"), answer.Header.Get("Cipher-Proxy-Code"), body, codeUpstream)
		}
	})
	t.Run("a plain-HTTP forward whose service aborted its answer", func(t *testing.T) {
		const plainHost = "plain.example.net"
		r := newRig(t, Options{})
		var received atomic.Int32
		service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			received.Add(1)
			panic(http.ErrAbortHandler)
		}))
		t.Cleanup(service.Close)
		r.addresses[plainHost] = []netip.Addr{netip.MustParseAddr("93.184.216.37")}
		r.dialTo[net.JoinHostPort(plainHost, "80")] = service.Listener.Addr().String()
		answer, body := r.do(r.client(workerPlaceholder), http.MethodPost, "http://"+plainHost+"/write", `{"n":1}`)
		if received.Load() != 1 {
			t.Fatalf("the plain service received %d requests, want the one", received.Load())
		}
		if answer.StatusCode != http.StatusBadGateway || answer.Header.Get("Cipher-Proxy-Forwarded") != "unknown" || answer.Header.Get("Cipher-Proxy-Code") != codeUpstream {
			t.Errorf("a plain request the service received and whose answer it aborted = %d, Cipher-Proxy-Forwarded %q, Cipher-Proxy-Code %q (%s); want 502, unknown, %s", answer.StatusCode, answer.Header.Get("Cipher-Proxy-Forwarded"), answer.Header.Get("Cipher-Proxy-Code"), body, codeUpstream)
		}
	})
}

// TestAMintedGrantReachesNoHostItsGrantDoesNotBind: a minted capability's grant carries its
// requests to the one host its grant binds, so its placeholder as the proxy password opens no
// tunnel and no plain-HTTP forward anywhere else: a CONNECT to a host no entry binds, a CONNECT to
// the bound host on another port, and a plain-HTTP request to an unbound host are each refused 403
// MINTED_GRANT_HOST_ONLY, naming the rule, before any dial. A secret grant's CONNECT to the same
// unbound host still opens its tunnel, the airlock's rule for every other grant.
func TestAMintedGrantReachesNoHostItsGrantDoesNotBind(t *testing.T) {
	r, _ := newDPoPRig(t)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "the plain service answered")
	}))
	t.Cleanup(plain.Close)
	r.addresses["plain.example.net"] = []netip.Addr{netip.MustParseAddr("93.184.216.50")}
	r.dialTo["plain.example.net:80"] = plain.Listener.Addr().String()
	const rule = "a minted capability's grant carries its requests to the host its grant binds and nowhere else"
	refused := func(what string, answer *http.Response, body string, dialsBefore int64) {
		t.Helper()
		if answer.StatusCode != http.StatusForbidden || answer.Header.Get("Cipher-Proxy-Code") != codeMintedHostOnly || !strings.Contains(body, rule) || r.dials.Load() != dialsBefore {
			t.Errorf("%s with a minted grant = %d, Cipher-Proxy-Code %q (%q) after %d new dial(s); want 403 %s naming the rule, and no dial", what, answer.StatusCode, answer.Header.Get("Cipher-Proxy-Code"), body, r.dials.Load()-dialsBefore, codeMintedHostOnly)
		}
	}
	for _, target := range []string{net.JoinHostPort(echoHost, "443"), net.JoinHostPort(boundHost, "8443")} {
		before := r.dials.Load()
		// connectRaw reads the answer's head alone, and a refusal's body is read whole; the body
		// of a tunnel that opened never ends, so its 200 is judged by its head.
		answer, _, _ := r.connectRaw(target, basic(mintedPlaceholder))
		body := ""
		if answer.StatusCode != http.StatusOK {
			body = bodyOf(t, answer)
		}
		refused("a CONNECT to "+target, answer, body, before)
	}
	before := r.dials.Load()
	answer, body := r.plainRaw("plain.example.net", "/anything", basic(mintedPlaceholder))
	refused("a plain-HTTP request to plain.example.net", answer, body, before)

	// The control: the same tunnel under a secret grant opens, as the airlock blocks nothing it
	// does not bind for one.
	answer, _, _ = r.connectRaw(net.JoinHostPort(echoHost, "443"), basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Errorf("a secret grant's CONNECT to %s:443 = %d, want its tunnel (200)", echoHost, answer.StatusCode)
	}
}

// recordingRoundTripper counts what it was handed, keeps the last, and sends nothing.
type recordingRoundTripper struct {
	sent int
	last *http.Request
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.sent++
	r.last = req
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: req}, nil
}

// TestTheMintedTransportSendsNoPlaceholder: the transport behind a dpop host refuses, sending
// nothing, a request whose Authorization is not one DPoP <token> or whose token is a placeholder,
// a second guard behind the swap; it signs and sends a request carrying the grant's token.
func TestTheMintedTransportSendsNoPlaceholder(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	base := &recordingRoundTripper{}
	transport := &mintedTransport{key: key, origin: "https://" + boundHost, base: base}
	for _, authorization := range [][]string{nil, {"DPoP " + mintedPlaceholder}, {"Bearer " + mintedToken}, {"DPoP " + mintedToken, "DPoP " + mintedToken}} {
		request, err := http.NewRequest(http.MethodPost, "https://"+boundHost+mintedPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header["Authorization"] = authorization
		if _, err := transport.RoundTrip(request); err == nil || base.sent != 0 {
			t.Fatalf("Authorization %q: err = %v and %d sent, want a refusal and nothing sent", authorization, err, base.sent)
		}
	}
	request, err := http.NewRequest(http.MethodPost, "https://"+boundHost+mintedPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "DPoP "+mintedToken)
	if _, err := transport.RoundTrip(request); err != nil || base.sent != 1 || base.last.Header.Get("DPoP") == "" || request.Header.Get("DPoP") != "" {
		t.Fatalf("the grant's token: err = %v, %d sent; want it sent signed, and the caller's request left as it was", err, base.sent)
	}
}
