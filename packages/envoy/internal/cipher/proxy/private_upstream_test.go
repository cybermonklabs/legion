// packages/envoy/internal/cipher/proxy/private_upstream_test.go
//
// The rows of the private destination (Options.PrivateUpstream, BROKER_PROXY_PRIVATE_UPSTREAM):
// the one bound host whose allowed requests go to an address inside the broker's own cluster, in
// place of the host's public address. The rig is rig_test.go's with no DialAllow: the destination's
// name resolves to an address the proxy refuses to every other request, and the rig sends a dial of
// that address to a stand-in of Dispatch on loopback, a plain-HTTP server that checks each write's
// token and proof with the verifier Dispatch itself runs (ciphertoken.Verifier), for Dispatch's
// public origin.
package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/testport"
)

// The private destination of every row here: Dispatch's public host, which the rules bind, and
// the address it has inside the cluster, the URL's host and port.
const (
	privateHost = "dispatch.test"
	privateName = "dispatch.internal.example"
	privatePort = "8766"
)

// privateAddress is where privateName resolves in the rows that send a write: a private address,
// which the proxy refuses to every request but one to the destination's bound host.
var privateAddress = netip.MustParseAddr("10.0.0.5")

// publicAddress is where privateHost itself resolves: its tailnet address, which the proxy never
// dials (100.64.0.0/10), as Dispatch's public name resolves inside the tailnet.
var publicAddress = netip.MustParseAddr("100.100.1.1")

// privateDestination is BROKER_PROXY_PRIVATE_UPSTREAM=dispatch.test=http://dispatch.internal.example:8766.
func privateDestination() PrivateUpstream {
	return PrivateUpstream{Host: privateHost, URL: &url.URL{Scheme: "http", Host: net.JoinHostPort(privateName, privatePort)}}
}

// privateRules is the rig's rules with TEST_MINTED_TOKEN beside its WORKER_TOKEN: a dpop block's
// inline binding, on the destination's host, carried in Authorization alone, allowing POST under
// the projects route.
func privateRules() proxyrules.Rules {
	rules := testRules()
	rules.Names = append(rules.Names, dpopBinding(mintedName, privateHost))
	return rules
}

// dispatchWrite is one request the stand-in of Dispatch received: the Host it was sent with, its
// Authorization headers, how many DPoP headers it carried, the person Dispatch's verifier read
// from the token, and what that verifier answered.
type dispatchWrite struct {
	host          string
	authorization []string
	proofs        int
	person        string
	verified      error
}

// String is the write as a failure message reads it, with the verifier's own words for a token or
// proof it refused.
func (w dispatchWrite) String() string {
	verdict := "accepted"
	if w.verified != nil {
		verdict = w.verified.Error()
	}
	return fmt.Sprintf("{Host %s, Authorization %q, %d DPoP header(s), person %q, the verifier: %s}", w.host, w.authorization, w.proofs, w.person, verdict)
}

// dispatchStandIn is Dispatch behind its in-cluster address: plain HTTP, the verifier Dispatch runs
// with Dispatch's public origin, and the broker's introspection answering the grant's token as a
// token acting for its person, bound to the pod's key. It answers 201 to a write the verifier
// accepts and 401 to any other, and keeps what it received.
type dispatchStandIn struct {
	addr string
	mu   sync.Mutex
	seen []dispatchWrite
}

func (d *dispatchStandIn) writes() []dispatchWrite {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dispatchWrite(nil), d.seen...)
}

// spentProofs is the verifier's replay store, in memory: each proof is taken once.
type spentProofs struct {
	mu    sync.Mutex
	spent map[string]bool
}

func (s *spentProofs) CheckAndSetDPoPProofUsed(_ context.Context, jti, jkt, _, htm, htu string, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.Join([]string{jti, jkt, htm, htu}, " ")
	if s.spent[key] {
		return true, nil
	}
	s.spent[key] = true
	return false, nil
}

// newDispatchStandIn serves the stand-in until the test ends. key is the pod's: the broker binds
// the grant's token to its thumbprint.
func newDispatchStandIn(t *testing.T, key *ecdsa.PrivateKey) *dispatchStandIn {
	t.Helper()
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	broker := ciphertokentest.NewStubBroker(t)
	broker.Says(mintedToken, ciphertoken.Introspection{
		Active: true, Session: mintedSession, Person: "ada@example.com", ProjectScope: ciphertoken.ProjectScopePerson,
		ExpiresAt: time.Now().Add(time.Hour).Unix(), ClientID: ciphertoken.ExchangeClientID, Thumbprint: jkt,
	})
	accountFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(accountFile, []byte("dispatchs-own-account-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	verifier, err := ciphertoken.New(ciphertoken.Options{
		BrokerURL: broker.Server.URL,
		Account:   ciphertoken.NewAccountToken(accountFile),
		Origins:   []string{"https://" + privateHost},
		Replay:    &spentProofs{spent: map[string]bool{}},
		Address:   func(r *http.Request) string { return r.RemoteAddr },
	})
	if err != nil {
		t.Fatalf("Dispatch's verifier: %v", err)
	}
	d := &dispatchStandIn{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := verifier.Verify(r.Context(), r)
		d.mu.Lock()
		d.seen = append(d.seen, dispatchWrite{
			host: r.Host, authorization: r.Header.Values("Authorization"), proofs: len(r.Header.Values("DPoP")),
			person: caller.Person, verified: err,
		})
		d.mu.Unlock()
		if err != nil {
			http.Error(w, "the write's token or proof did not verify", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "stored")
	}))
	t.Cleanup(server.Close)
	d.addr = server.Listener.Addr().String()
	return d
}

// newPrivateRig is the rig with the private destination set and the pod's key, holding the one
// minted grant and the token minted for it beside the rig's own grant, with ranges as its cluster
// ranges. The destination's name resolves to at, whose dial on the destination's port reaches the
// stand-in of Dispatch; the destination's bound host resolves to its tailnet address alone.
func newPrivateRig(t *testing.T, at netip.Addr, ranges []netip.Prefix) (*rig, *dispatchStandIn) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := newRuledRig(t, privateRules(), Options{DPoPKey: key, ClusterRanges: ranges, PrivateUpstreams: []PrivateUpstream{privateDestination()}})
	r.grants.setConnection(mintedPlaceholder, mintedConnection())
	r.grants.setValues(mintedGrantID, storedValues(mintedName, mintedToken))
	r.addresses[privateHost] = []netip.Addr{publicAddress}
	r.addresses[privateName] = []netip.Addr{at}
	standIn := newDispatchStandIn(t, key)
	r.dialTo[net.JoinHostPort(at.String(), privatePort)] = standIn.addr
	return r, standIn
}

// writeMinted is the minted grant's allowed write through the proxy: POST under the projects route of
// the destination's bound host, with the grant's placeholder as its one Authorization header. It
// answers the error of a write whose CONNECT the proxy refused, which net/http's client reports
// with no answer.
func (r *rig) writeMinted() (*http.Response, string, error) {
	r.t.Helper()
	request, err := http.NewRequest(http.MethodPost, "https://"+privateHost+mintedPath, strings.NewReader(`{"verdict":"pass"}`))
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Authorization", "DPoP "+mintedPlaceholder)
	answer, err := r.client(mintedPlaceholder).Do(request)
	if err != nil {
		return nil, "", err
	}
	return answer, bodyOf(r.t, answer), nil
}

// writeRefusedAtConnect fails t unless the allowed write is refused at its CONNECT: the command's
// client gets the proxy's 403, the proxy's refusal line names the destination's bound host, the
// code ADDRESS_REFUSED and a reason holding want, and nothing is dialled or reaches Dispatch. What
// the command reads names the rule and never the destination's in-cluster name or the address it
// resolved to (at): the refusal line's reason, which is the command's answer, holds neither, and
// the broker's line of the dial that did not happen holds both.
func writeRefusedAtConnect(t *testing.T, r *rig, standIn *dispatchStandIn, at netip.Addr, want string) {
	t.Helper()
	answer, body, err := r.writeMinted()
	if err == nil {
		t.Fatalf("the write answered %d %q after the dials %v; want its CONNECT refused 403 naming %q", answer.StatusCode, body, r.dialedAddresses(), want)
	}
	if !strings.Contains(err.Error(), http.StatusText(http.StatusForbidden)) {
		t.Fatalf("the write failed with %v; want the proxy's 403 to its CONNECT", err)
	}
	refusals := r.logs.refusals()
	if len(refusals) == 0 {
		t.Fatal("the proxy logged no refusal")
	}
	line := refusals[len(refusals)-1].fields
	reason, _ := line["reason"].(string)
	if line["status"] != int64(http.StatusForbidden) || line["code"] != codeAddressRefused || line["method"] != http.MethodConnect || line["host"] != privateHost || !strings.Contains(reason, want) {
		t.Fatalf("the refusal line reads %v; want 403 %s of the CONNECT to %s with a reason naming %q", line, codeAddressRefused, privateHost, want)
	}
	if strings.Contains(reason, privateName) || strings.Contains(reason, at.String()) || !strings.Contains(reason, "BROKER_PROXY_PRIVATE_UPSTREAM") {
		t.Fatalf("the command is told %q; want the rule (BROKER_PROXY_PRIVATE_UPSTREAM) and neither the destination %s nor its address %s", reason, privateName, at)
	}
	failures := r.logs.dialFailures()
	if len(failures) == 0 {
		t.Fatal("the proxy logged no dial that did not happen")
	}
	detail, _ := failures[len(failures)-1].fields["reason"].(string)
	if !strings.Contains(detail, privateName) || !strings.Contains(detail, at.String()) || !strings.Contains(detail, want) {
		t.Fatalf("the broker's line of the refused dial reads %q; want the destination %s, its address %s and %q", detail, privateName, at, want)
	}
	if dialed := r.dialedAddresses(); len(dialed) != 0 {
		t.Fatalf("the proxy dialled %v; want nothing", dialed)
	}
	if seen := standIn.writes(); len(seen) != 0 {
		t.Fatalf("Dispatch received %d writes; want none", len(seen))
	}
}

// connectRefused fails t unless a CONNECT to target with password as the proxy's password is
// refused 403 with nothing dialled, and answers the refusal's Cipher-Proxy-Code and body.
func connectRefused(t *testing.T, r *rig, target, password string) (string, string) {
	t.Helper()
	before := r.dials.Load()
	answer, _, _ := r.connectRaw(target, basic(password))
	// The status first: a CONNECT answered 200 is an open tunnel, whose body never ends.
	if answer.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT %s = %d after %d dial(s) to %v; want 403 and none", target, answer.StatusCode, r.dials.Load()-before, r.dialedAddresses())
	}
	body := bodyOf(t, answer)
	if after := r.dials.Load(); after != before {
		t.Fatalf("CONNECT %s dialled %d time(s); want none", target, after-before)
	}
	return answer.Header.Get("Cipher-Proxy-Code"), body
}

// TestThePrivateUpstreamTakesTheBoundRouteAlone: the allowed write to the destination's bound host
// reaches Dispatch at its private address with Host dispatch.test, the grant's token in place of the
// placeholder, and a proof that Dispatch's own verifier accepts for its public origin, although the
// write was sent to http://dispatch.internal.example:8766. Nothing else reaches that address or any
// other the proxy refuses: a request the rules do not allow, a CONNECT to the destination's name or
// address, a plain-HTTP request to it, a CONNECT to each kind of private, loopback, link-local and
// cluster address, and a grant whose names do not bind dispatch.test sending to it are each refused
// 403 with nothing dialled. The minted grant's CONNECT and plain request anywhere but its bound host
// are refused by its one-host rule (MINTED_GRANT_HOST_ONLY) before anything is resolved, so the
// address rule that keeps every other grant from the destination's address is read through a
// secret grant's, which the airlock lets tunnel to any host its names do not bind: refused
// ADDRESS_REFUSED.
func TestThePrivateUpstreamTakesTheBoundRouteAlone(t *testing.T) {
	r, standIn := newPrivateRig(t, privateAddress, []netip.Prefix{serviceRange})

	answer, body, err := r.writeMinted()
	seen := standIn.writes()
	if err != nil || answer.StatusCode != http.StatusCreated || len(seen) != 1 || seen[0].host != privateHost ||
		len(seen[0].authorization) != 1 || seen[0].authorization[0] != "DPoP "+mintedToken || seen[0].proofs != 1 ||
		seen[0].verified != nil || seen[0].person != "ada@example.com" {
		var got any = err
		if err == nil {
			got = []any{answer.StatusCode, body}
		}
		t.Fatalf("the allowed write answered %v and Dispatch received %v; want 201 and one write with Host %s, the grant's token and one proof Dispatch's verifier accepts, acting for ada@example.com",
			got, seen, privateHost)
	}
	if dialed := r.dialedAddresses(); len(dialed) != 1 || dialed[0] != net.JoinHostPort(privateAddress.String(), privatePort) {
		t.Fatalf("the proxy dialled %v; want the destination's address %s:%s alone", dialed, privateAddress, privatePort)
	}

	before := r.dials.Load()
	answer, body = r.get(r.client(mintedPlaceholder), "https://"+privateHost+"/api/v1/issues", "Authorization", "DPoP "+mintedPlaceholder)
	refusedAs(t, "GET /api/v1/issues", answer, body, codeNotAllowed)
	if after := r.dials.Load(); after != before {
		t.Fatalf("GET /api/v1/issues dialled %d time(s); want none", after-before)
	}

	for _, target := range []string{net.JoinHostPort(privateName, privatePort), net.JoinHostPort(privateAddress.String(), privatePort)} {
		if code, body := connectRefused(t, r, target, mintedPlaceholder); code != codeMintedHostOnly {
			t.Errorf("CONNECT %s with the minted grant was refused %s (%q); want %s", target, code, body, codeMintedHostOnly)
		}
		if code, body := connectRefused(t, r, target, workerPlaceholder); code != codeAddressRefused {
			t.Errorf("CONNECT %s with a secret grant was refused %s (%q); want %s", target, code, body, codeAddressRefused)
		}
	}

	for _, c := range []struct{ password, code string }{{mintedPlaceholder, codeMintedHostOnly}, {workerPlaceholder, codeAddressRefused}} {
		before = r.dials.Load()
		answer, body = r.get(r.client(c.password), "http://"+net.JoinHostPort(privateName, privatePort)+"/healthz")
		if answer.StatusCode != http.StatusForbidden || answer.Header.Get("Cipher-Proxy-Code") != c.code {
			t.Fatalf("plain http to %s answered %d %q; want 403 %s", privateName, answer.StatusCode, body, c.code)
		}
		if after := r.dials.Load(); after != before {
			t.Fatalf("plain http to %s dialled %d time(s); want none", privateName, after-before)
		}
	}

	for _, addr := range []string{"10.11.12.13", "172.16.0.1", "192.168.1.1", "100.64.0.1", "fd00::1", "127.0.0.1", "169.254.169.254", "34.118.224.10"} {
		for _, c := range []struct{ password, code string }{{mintedPlaceholder, codeMintedHostOnly}, {workerPlaceholder, codeAddressRefused}} {
			if code, body := connectRefused(t, r, net.JoinHostPort(addr, "443"), c.password); code != c.code {
				t.Errorf("CONNECT %s:443 was refused %s (%q); want %s", addr, code, body, c.code)
			}
		}
	}

	// The rig's own grant, whose one name binds example.com: dispatch.test is no host of it, so
	// its CONNECT is read as any other host's, by dispatch.test's own address, which the proxy
	// never dials.
	before = r.dials.Load()
	other, _, _ := r.connectRaw(net.JoinHostPort(privateHost, "443"), basic(workerPlaceholder))
	if other.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT %s with a grant that does not bind it = %d after the dials %v; want 403", privateHost, other.StatusCode, r.dialedAddresses())
	}
	if text := bodyOf(t, other); !strings.Contains(text, publicAddress.String()) {
		t.Errorf("the refusal of %s with a grant that does not bind it reads %q; want it to name %s, the host's own address", privateHost, text, publicAddress)
	}
	if after := r.dials.Load(); after != before {
		t.Fatalf("CONNECT %s with a grant that does not bind it dialled %d time(s); want none", privateHost, after-before)
	}
	if seen := standIn.writes(); len(seen) != 1 {
		t.Fatalf("Dispatch received %d writes; want the one allowed write", len(seen))
	}
}

// TestAnUpstreamTheGeneralRuleWouldDialIsRefused: a destination whose name resolves to an address
// the proxy would tunnel to - a ClusterIP with no cluster ranges set - is refused, naming
// BROKER_AIRLOCK_CLUSTER_RANGES, and nothing is dialled: the exception holds only for an address
// every other request is refused.
func TestAnUpstreamTheGeneralRuleWouldDialIsRefused(t *testing.T) {
	at := netip.MustParseAddr(clusterIPs[0])
	r, standIn := newPrivateRig(t, at, nil)
	writeRefusedAtConnect(t, r, standIn, at, "which this proxy would also tunnel to: list its range in BROKER_AIRLOCK_CLUSTER_RANGES")
}

// TestAnUpstreamOnLoopbackOrLinkLocalIsRefused: a destination whose name resolves to a loopback or
// link-local address, IPv4 or IPv6, is refused and nothing is dialled, though the proxy refuses
// such an address to every other request too: the broker's own pod and the node's metadata server
// are no destination.
func TestAnUpstreamOnLoopbackOrLinkLocalIsRefused(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "169.254.169.254", "fe80::1"} {
		at := netip.MustParseAddr(addr)
		r, standIn := newPrivateRig(t, at, []netip.Prefix{serviceRange})
		writeRefusedAtConnect(t, r, standIn, at, "a loopback or link-local address, which the airlock never dials")
	}
}

// TestAnUpstreamAtACloudMetadataServerIsRefused: a destination whose name resolves to a cloud
// metadata server's IPv6 address - fd20:ce::254, Google Compute Engine's, or fd00:ec2::254, Amazon
// EC2's (netaddr.DestinationRefusal) - is refused and nothing is dialled, although each is a
// unique-local address, private, which every other request is refused and the exception would
// otherwise take: the node's metadata server is no destination, as 169.254.169.254 is none.
func TestAnUpstreamAtACloudMetadataServerIsRefused(t *testing.T) {
	for _, addr := range []string{"fd20:ce::254", "fd00:ec2::254"} {
		at := netip.MustParseAddr(addr)
		r, standIn := newPrivateRig(t, at, []netip.Prefix{serviceRange})
		writeRefusedAtConnect(t, r, standIn, at, "a cloud metadata server's address, which the airlock never dials")
	}
}

// TestAnUpstreamAtTheIPv4BroadcastAddressIsRefused: a destination whose name resolves to
// 255.255.255.255 is refused and nothing is dialled, although every other request is refused it
// (not global unicast) and the exception would otherwise take it: no one service has it.
func TestAnUpstreamAtTheIPv4BroadcastAddressIsRefused(t *testing.T) {
	at := netip.MustParseAddr("255.255.255.255")
	r, standIn := newPrivateRig(t, at, []netip.Prefix{serviceRange})
	writeRefusedAtConnect(t, r, standIn, at, "the IPv4 broadcast address, which the airlock never dials")
}

// TestAnUpstreamAtAnAddressThatCarriesAnIPv4AddressIsRefused: a destination whose name resolves to
// an IPv6 address that carries an IPv4 address - the NAT64, 6to4, IPv4-compatible, Teredo,
// IPv4-translated and ISATAP forms of the metadata server, loopback, 0.0.0.1, multicast and the
// broadcast address, and an address of the local-use translation prefix - or to 0.0.0.1 is
// refused, whatever address it carries, and nothing is dialled. The general rule refuses each of
// them too, by the address it carries, and the destination's rule takes an address only for the
// ranges it names (netaddr.DestinationRefusal), so a form the general rule refuses is never read
// as private: before that rule, each row here was dialled and its write stored. Control: the
// private address the rig's other tests take, in the same run, stores its write.
func TestAnUpstreamAtAnAddressThatCarriesAnIPv4AddressIsRefused(t *testing.T) {
	const carries = "an IPv6 address that carries an IPv4 address (a NAT64, 6to4, Teredo, IPv4-compatible, IPv4-translated or ISATAP form), which is never the private destination's"
	for _, row := range []struct{ name, addr, want string }{
		{"0.0.0.1, of 0.0.0.0/8", "0.0.0.1", "an address of 0.0.0.0/8, which is reserved for this network"},
		{"NAT64 of 169.254.169.254", "64:ff9b::a9fe:a9fe", carries},
		{"NAT64 of 127.0.0.1", "64:ff9b::7f00:1", carries},
		{"NAT64 of 0.0.0.1", "64:ff9b::1", carries},
		{"NAT64 of 224.0.0.1", "64:ff9b::e000:1", carries},
		{"NAT64 of 255.255.255.255", "64:ff9b::ffff:ffff", carries},
		{"6to4 of 169.254.169.254", "2002:a9fe:a9fe::1", carries},
		{"6to4 of 127.0.0.1", "2002:7f00:1::1", carries},
		{"IPv4-compatible 169.254.169.254", "::a9fe:a9fe", carries},
		{"IPv4-compatible 127.0.0.1", "::7f00:1", carries},
		{"Teredo whose server is 169.254.169.254", "2001:0:a9fe:a9fe::1", carries},
		{"IPv4-translated 169.254.169.254", "::ffff:0:a9fe:a9fe", carries},
		{"ISATAP of 169.254.169.254 under a unique-local /64", "fd12:3456::5efe:a9fe:a9fe", carries},
		{"ISATAP of 10.0.0.5 under a unique-local /64", "fd12:3456::200:5efe:a00:5", carries},
		{"local-use translation of 169.254.169.254", "64:ff9b:1::a9fe:a9fe", "a local-use translation IPv6 address (64:ff9b:1::/48), which is never the private destination's"},
	} {
		t.Run(row.name, func(t *testing.T) {
			at := netip.MustParseAddr(row.addr)
			r, standIn := newPrivateRig(t, at, []netip.Prefix{serviceRange})
			writeRefusedAtConnect(t, r, standIn, at, row.want)
		})
	}
	r, standIn := newPrivateRig(t, privateAddress, []netip.Prefix{serviceRange})
	if answer, body, err := r.writeMinted(); err != nil || answer.StatusCode != http.StatusCreated || len(standIn.writes()) != 1 {
		t.Fatalf("control: the write to %s = %v %v %q; want 201", privateAddress, answer, err, body)
	}
}

// The second private destination of TestEachPrivateDestinationTakesItsOwnHostAlone: a second bound
// host, its in-cluster name and address, and the minted grant whose dpop block binds it.
const (
	secondHost    = "results.test"
	secondName    = "results.internal.example"
	secondMinted  = "SECOND_MINTED_TOKEN"
	secondGrantID = "secondkind:00000000-0000-4000-8000-000000000002"
)

var (
	secondAddress     = netip.MustParseAddr("10.0.0.6")
	secondPlaceholder = placeholder.MintedPrefix("secondkind") + "AaBbCcDdEeFfGgHhIiJjKkLlMmNnOoPp"
)

// TestEachPrivateDestinationTakesItsOwnHostAlone: with two private destinations, each bound host's
// allowed write goes to its own destination's address and to no other: the first host's write
// reaches the first stand-in alone, with the first host as its Host, and the second's reaches the
// second alone. Each destination's transport resolves and dials only its own name, so a write to
// one host never reaches the other's service whatever the order of the writes.
func TestEachPrivateDestinationTakesItsOwnHostAlone(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rules := privateRules()
	rules.Names = append(rules.Names, dpopBinding(secondMinted, secondHost))
	second := PrivateUpstream{Host: secondHost, URL: &url.URL{Scheme: "http", Host: net.JoinHostPort(secondName, privatePort)}}
	r := newRuledRig(t, rules, Options{DPoPKey: key, ClusterRanges: []netip.Prefix{serviceRange}, PrivateUpstreams: []PrivateUpstream{privateDestination(), second}})
	r.grants.setConnection(mintedPlaceholder, mintedConnection())
	r.grants.setValues(mintedGrantID, storedValues(mintedName, mintedToken))
	r.grants.setConnection(secondPlaceholder, requests.AirlockConnection{
		GrantID: secondGrantID, SessionID: mintedSession, Minted: true,
		Names: map[string]requests.AirlockGrantName{secondMinted: {Slots: map[string]string{requests.SlotValue: secondPlaceholder}}},
	})
	r.grants.setValues(secondGrantID, storedValues(secondMinted, mintedToken))
	r.addresses[privateHost], r.addresses[secondHost] = []netip.Addr{publicAddress}, []netip.Addr{publicAddress}
	r.addresses[privateName], r.addresses[secondName] = []netip.Addr{privateAddress}, []netip.Addr{secondAddress}
	first, other := newDispatchStandIn(t, key), newDispatchStandIn(t, key)
	r.dialTo[net.JoinHostPort(privateAddress.String(), privatePort)] = first.addr
	r.dialTo[net.JoinHostPort(secondAddress.String(), privatePort)] = other.addr
	write := func(host, held string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, "https://"+host+mintedPath, strings.NewReader(`{"verdict":"pass"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "DPoP "+held)
		answer, err := r.client(held).Do(request)
		if err != nil {
			t.Fatalf("the write to %s failed: %v", host, err)
		}
		bodyOf(t, answer)
		return answer
	}
	for _, c := range []struct{ host, held string }{{secondHost, secondPlaceholder}, {privateHost, mintedPlaceholder}} {
		if answer := write(c.host, c.held); answer.Header.Get("Cipher-Proxy-Forwarded") != "true" {
			t.Fatalf("the write to %s = %d, Cipher-Proxy-Code %q; want it forwarded", c.host, answer.StatusCode, answer.Header.Get("Cipher-Proxy-Code"))
		}
	}
	if seen := first.writes(); len(seen) != 1 || seen[0].host != privateHost {
		t.Errorf("the first destination received %v; want the one write to %s", seen, privateHost)
	}
	if seen := other.writes(); len(seen) != 1 || seen[0].host != secondHost {
		t.Errorf("the second destination received %v; want the one write to %s", seen, secondHost)
	}
}

// TestAnUpstreamThatRebindsAfterItsConnectIsRefusedAtTheDial: the destination's name resolves to
// a private address when the CONNECT is checked, and to a loopback or link-local address when the
// write is dialled - a name rebound between the two lookups. The dial checks the addresses it is
// about to open again (dialUpstream, upstreamAddresses), so the write fails 502 with nothing
// dialled at the rebound address and nothing reaching Dispatch, though a dial there would reach it,
// and the broker's line of the refused dial names the rebound address.
func TestAnUpstreamThatRebindsAfterItsConnectIsRefusedAtTheDial(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "169.254.169.254"} {
		rebound := netip.MustParseAddr(addr)
		r, standIn := newPrivateRig(t, privateAddress, []netip.Prefix{serviceRange})
		r.answers = map[string][][]netip.Addr{privateName: {{privateAddress}, {rebound}}}
		r.dialTo[net.JoinHostPort(rebound.String(), privatePort)] = standIn.addr
		answer, body, err := r.writeMinted()
		if err != nil || answer.StatusCode != http.StatusBadGateway {
			t.Fatalf("the write after %s rebound to %s = %v %v %q, Dispatch received %d writes; want 502 and none", privateName, rebound, answer, err, body, len(standIn.writes()))
		}
		if seen := standIn.writes(); len(seen) != 0 {
			t.Fatalf("Dispatch received %d writes after the name rebound to %s; want none", len(seen), rebound)
		}
		for _, dialed := range r.dialedAddresses() {
			if dialed == net.JoinHostPort(rebound.String(), privatePort) {
				t.Fatalf("the proxy dialled %v; want nothing at the rebound address %s", r.dialedAddresses(), rebound)
			}
		}
		failures := r.logs.dialFailures()
		if len(failures) == 0 {
			t.Fatalf("the proxy logged no refused dial after the name rebound to %s", rebound)
		}
		if detail, _ := failures[len(failures)-1].fields["reason"].(string); !strings.Contains(detail, rebound.String()) {
			t.Fatalf("the broker's line of the refused dial reads %q; want it to name the rebound address %s", detail, rebound)
		}
	}
}

// TestAnUnreachableUpstreamIsNotNamedToTheCommand: a destination at an address the proxy may dial,
// where nothing answers, fails the write 502 UPSTREAM_UNREACHABLE, whose words name the rule and
// nothing of the dial: neither the destination's in-cluster name nor its address, nor the address
// and port the dial opened, nor the transport's error ("dial tcp", "connection refused"). The rig
// sends the dial of the destination's address to a loopback port nothing listens on, so the
// transport's error names that loopback address and port, never 10.0.0.5: the answer is read for
// both, so an answer that carried the transport's error fails here as it would carry the
// destination's in-cluster address in production. The broker's line of the call it could not
// carry holds the error, with the address dialled. Control: the same rig with the stand-in
// answering stores the write.
func TestAnUnreachableUpstreamIsNotNamedToTheCommand(t *testing.T) {
	r, standIn := newPrivateRig(t, privateAddress, []netip.Prefix{serviceRange})
	if answer, body, err := r.writeMinted(); err != nil || answer.StatusCode != http.StatusCreated || len(standIn.writes()) != 1 {
		t.Fatalf("control: the write to an answering destination = %v %v %q; want 201", answer, err, body)
	}
	r, _ = newPrivateRig(t, privateAddress, []netip.Prefix{serviceRange})
	// The destination's address now dials a loopback port the test holds bound and never
	// listening (testport.Refused), which the transport's error names. It refuses every dial:
	// this is not a closed listener's port, which any process can take and answer.
	destination := net.JoinHostPort(privateAddress.String(), privatePort)
	dialled := testport.Refused(t)
	_, dialledPort, err := net.SplitHostPort(dialled)
	if err != nil {
		t.Fatal(err)
	}
	r.dialTo[destination] = dialled
	answer, body, err := r.writeMinted()
	if err != nil || answer.StatusCode != http.StatusBadGateway || answer.Header.Get("Cipher-Proxy-Code") != codeUpstream {
		t.Fatalf("the write to a destination where nothing answers = %v %v %q; want 502 %s", answer, err, body, codeUpstream)
	}
	if !strings.Contains(body, "BROKER_PROXY_PRIVATE_UPSTREAM") {
		t.Fatalf("the command reads %q; want the rule BROKER_PROXY_PRIVATE_UPSTREAM", body)
	}
	for _, leak := range []string{privateName, privateAddress.String(), dialled, ":" + dialledPort, "dial tcp", "connection refused"} {
		if strings.Contains(body, leak) {
			t.Errorf("the command reads %q, which holds %q; want the rule alone and nothing of the dial of %s, which the rig sent to %s", body, leak, destination, dialled)
		}
	}
	lines := r.logs.lines(upstreamMessage)
	if len(lines) == 0 {
		t.Fatal("the proxy logged no call it could not carry")
	}
	if detail, _ := lines[len(lines)-1].fields["error"].(string); !strings.Contains(detail, dialled) {
		t.Fatalf("the broker's line of the call it could not carry reads %v; want the transport's error, naming the address dialled %s", lines[len(lines)-1].fields, dialled)
	}
}
