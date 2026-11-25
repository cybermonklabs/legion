package config

import (
	"maps"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// TestTheResolveSettingMayNameThePortItsDialReaches: an entry of BROKER_AIRLOCK_TEST_RESOLVE may
// name a port beside its address, which the proxy dials in place of the CONNECT's own: a stand-in
// on loopback needs no privileged port 443, as a test of the real broker's airlock does. An entry
// with no port leaves the CONNECT's. An address given two ports, or the port 0, is refused, naming
// it: a dial to the address could not tell which port it means.
func TestTheResolveSettingMayNameThePortItsDialReaches(t *testing.T) {
	e := validEnv()
	e["BROKER_AIRLOCK_TEST_RESOLVE"] = "s.airlock.test=127.0.0.2:8443 t.airlock.test=[::1]:9443 u.airlock.test=127.0.0.3"
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatalf("Load with ports in BROKER_AIRLOCK_TEST_RESOLVE: %v", err)
	}
	wantHosts := map[string]netip.Addr{
		"s.airlock.test": netip.MustParseAddr("127.0.0.2"),
		"t.airlock.test": netip.MustParseAddr("::1"),
		"u.airlock.test": netip.MustParseAddr("127.0.0.3"),
	}
	if !maps.Equal(cfg.AirlockTestResolve, wantHosts) {
		t.Errorf("the resolve map reads %v; want %v", cfg.AirlockTestResolve, wantHosts)
	}
	wantPorts := map[netip.Addr]uint16{netip.MustParseAddr("127.0.0.2"): 8443, netip.MustParseAddr("::1"): 9443}
	if !maps.Equal(cfg.ProxyTestDialPorts, wantPorts) {
		t.Errorf("the dial ports read %v; want %v, and none for an entry that names no port", cfg.ProxyTestDialPorts, wantPorts)
	}
	for value, want := range map[string]string{
		"s.airlock.test=127.0.0.2:8443 t.airlock.test=127.0.0.2:9443": "127.0.0.2",
		"s.airlock.test=127.0.0.2:0":                                  "127.0.0.2:0",
	} {
		e := validEnv()
		e["BROKER_AIRLOCK_TEST_RESOLVE"] = value
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_AIRLOCK_TEST_RESOLVE") || !strings.Contains(err.Error(), want) {
			t.Errorf("Load(BROKER_AIRLOCK_TEST_RESOLVE=%q) = %v; want a refusal naming the setting and %s", value, err, want)
		}
	}
}

// TestAPrivateUpstreamSettingIsRefusedUnlessEachEntryIsOne: BROKER_PROXY_PRIVATE_UPSTREAM is
// whitespace-separated <host>=<url> entries, each read as that host and that URL in the order
// written, and unset is none. A host named twice, the setting with no profiles file, an entry not
// of that form, a host that is not one lower-case host name, and a URL that does not parse, is not
// http or https, names no host or a port outside 1 to 65535, carries a user, a path, a query or a
// fragment, or names a host every dial of the proxy refuses (localhost, a name under .localhost, a
// loopback, link-local, unspecified or multicast address, a cloud metadata server's IPv6 address,
// an IPv6 form carrying one) each refuse the start, naming the setting and the fault. No refusal
// quotes the URL's user, path, query or fragment, where a credential can sit. A loopback URL that
// BROKER_AIRLOCK_TEST_DIAL_ALLOW names with its port, the local stack's stand-in of Dispatch, is
// read.
func TestAPrivateUpstreamSettingIsRefusedUnlessEachEntryIsOne(t *testing.T) {
	const setting = "BROKER_PROXY_PRIVATE_UPSTREAM"
	if cfg, err := Load(env(airlockEnv())); err != nil || cfg.ProxyPrivateUpstreams != nil {
		t.Fatalf("Load with %s unset = %+v, %v; want none", setting, cfg.ProxyPrivateUpstreams, err)
	}
	for _, row := range []struct {
		value, dialAllow string
		want             []string // host=URL, in order
	}{
		{"dispatch.test=http://dispatch.dispatch.svc.cluster.local:8766", "", []string{"dispatch.test=http://dispatch.dispatch.svc.cluster.local:8766"}},
		{" dispatch.test=https://10.0.0.5/ ", "", []string{"dispatch.test=https://10.0.0.5/"}},
		{"dispatch.test=http://127.0.0.1:8766", "127.0.0.1:8766", []string{"dispatch.test=http://127.0.0.1:8766"}},
		{"dispatch.test=http://localhost", "localhost:80", []string{"dispatch.test=http://localhost"}},
		{"dispatch.test=http://10.0.0.5:8766\n results.test=https://10.0.0.6", "", []string{"dispatch.test=http://10.0.0.5:8766", "results.test=https://10.0.0.6"}},
	} {
		e := airlockEnv()
		e[setting] = row.value
		if row.dialAllow != "" {
			e["BROKER_AIRLOCK_TEST_DIAL_ALLOW"] = row.dialAllow
		}
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatalf("Load(%s=%q, BROKER_AIRLOCK_TEST_DIAL_ALLOW=%q): %v", setting, row.value, row.dialAllow, err)
		}
		var got []string
		for _, upstream := range cfg.ProxyPrivateUpstreams {
			got = append(got, upstream.Host+"="+upstream.URL.String())
		}
		if !slices.Equal(got, row.want) {
			t.Fatalf("Load(%s=%q) read %q; want %q", setting, row.value, got, row.want)
		}
	}
	// The URL's user, path, query and fragment carry a word a refusal must not quote; each URL is
	// put together here, so the word is one constant the check below reads too.
	const userWord = "secret-word"
	for _, row := range []struct{ name, value, dialAllow, want string }{
		{"a host named twice", "dispatch.test=http://10.0.0.5:8766 dispatch.test=http://10.0.0.6:8766", "", "names the host dispatch.test twice"},
		{"a second entry with no URL", "dispatch.test=http://10.0.0.5:8766 results.test=", "", "must be <host>=<url> entries"},
		{"an entry with no =", "dispatch.test", "", "must be <host>=<url> entries"},
		{"an entry with no URL", "dispatch.test=", "", "must be <host>=<url> entries"},
		{"a host that is a pattern", "*.test=http://10.0.0.5:8766", "", "in lower case, with no"},
		{"a host with a capital letter", "Dispatch.test=http://10.0.0.5:8766", "", "in lower case, with no"},
		{"a URL that does not parse", "dispatch.test=http://[10.0.0.5", "", "does not parse"},
		{"an ftp URL", "dispatch.test=ftp://10.0.0.5:21/" + userWord, "", "must be http or https"},
		{"a URL with no host", "dispatch.test=http://:8766/" + userWord, "", "names no host"},
		{"a URL with port 0", "dispatch.test=http://10.0.0.5:0/" + userWord, "", "port outside 1 to 65535"},
		{"a URL with port 65536", "dispatch.test=http://10.0.0.5:65536", "", "port outside 1 to 65535"},
		{"a URL with a user", "dispatch.test=http://u:" + userWord + "@10.0.0.5:8766", "", "carries a user"},
		{"a URL with a path", "dispatch.test=http://10.0.0.5:8766/" + userWord, "", "has a path"},
		{"a URL with a query", "dispatch.test=http://10.0.0.5:8766?q=" + userWord, "", "has a query"},
		{"a URL with a fragment", "dispatch.test=http://10.0.0.5:8766#" + userWord, "", "has a fragment"},
		{"a URL with an empty fragment", "dispatch.test=http://10.0.0.5:8766#", "", "has a fragment"},
		{"localhost", "dispatch.test=http://localhost:8766", "", "resolves to loopback"},
		{"a name under .localhost", "dispatch.test=http://dispatch.localhost:8766", "", "resolves to loopback"},
		{"a loopback address", "dispatch.test=http://127.0.0.1:8766", "", "a loopback or link-local address"},
		{"a loopback IPv6 address", "dispatch.test=http://[::1]:8766", "", "a loopback or link-local address"},
		{"the IPv4 metadata server", "dispatch.test=http://169.254.169.254", "", "a loopback or link-local address"},
		{"an unspecified address", "dispatch.test=http://0.0.0.0:8766", "", "an unspecified or multicast address"},
		{"Google's IPv6 metadata server", "dispatch.test=http://[fd20:ce::254]", "", "a cloud metadata server's address"},
		{"Amazon's IPv6 metadata server", "dispatch.test=http://[fd00:ec2::254]", "", "a cloud metadata server's address"},
		{"the NAT64 form of the metadata server", "dispatch.test=http://[64:ff9b::a9fe:a9fe]", "", "an IPv6 address that carries an IPv4 address"},
		{"a loopback address the dial allow names on another port", "dispatch.test=http://127.0.0.1:8766", "127.0.0.1:8767", "a loopback or link-local address"},
		{"a second entry at loopback", "dispatch.test=http://10.0.0.5:8766 results.test=http://127.0.0.1:8766", "", "a loopback or link-local address"},
	} {
		e := airlockEnv()
		e[setting] = row.value
		if row.dialAllow != "" {
			e["BROKER_AIRLOCK_TEST_DIAL_ALLOW"] = row.dialAllow
		}
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), setting) || !strings.Contains(err.Error(), row.want) {
			t.Errorf("%s: Load(%s=%q) = %v; want a refusal naming the setting and %q", row.name, setting, row.value, err, row.want)
			continue
		}
		if strings.Contains(err.Error(), userWord) {
			t.Errorf("%s: the refusal quotes a part of the URL a credential can sit in: %v", row.name, err)
		}
	}
	e := validEnv()
	e[setting] = "dispatch.test=http://10.0.0.5:8766"
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), setting) || !strings.Contains(err.Error(), "needs BROKER_PROFILES_FILE") {
		t.Errorf("Load(%s with no profiles file) = %v; want a refusal naming the setting and BROKER_PROFILES_FILE", setting, err)
	}
}

// mintedEntry is a proxy block of kind dpop named name: bound to dispatch.test alone, carried in
// Authorization alone, with an allow list.
func mintedEntry(name string) proxyrules.SecretRule {
	return proxyrules.SecretRule{
		Name: name, Hosts: []string{"dispatch.test"}, Headers: []string{"Authorization"}, Kind: proxyrules.KindDPoP,
		Allow: []proxyrules.Allow{{Method: "POST", PathPrefix: "/api/v1/"}},
	}
}

// dispatchUpstream is BROKER_PROXY_PRIVATE_UPSTREAM=dispatch.test=http://dispatch.dispatch.svc.cluster.local:8766.
func dispatchUpstream(t *testing.T) PrivateUpstream {
	t.Helper()
	return upstreamsOf(t, "dispatch.test=http://dispatch.dispatch.svc.cluster.local:8766")[0]
}

// upstreamsOf is the private destinations value names.
func upstreamsOf(t *testing.T, value string) []PrivateUpstream {
	t.Helper()
	upstreams, err := ParsePrivateUpstreams(value)
	if err != nil {
		t.Fatal(err)
	}
	return upstreams
}

// TestEveryDPoPEntryIsHeldToItsOwnDestinationsHost: every proxy block of kind dpop - each a
// capability the broker mints and holds, so a token minted for its grant goes to every host it
// binds - binds one exact host and no "*." name, has a non-empty allow list none of whose entries
// is every path, is carried in Authorization alone (no headers is the default, Authorization), and
// binds a private destination's host, so the token goes to that service's own address and nowhere
// a name resolves to; no block of another kind binds that host (proxyrules.HostBound, so a "*."
// block over it counts); every destination needs a dpop block to bind its host; and dpop blocks
// need a destination. Two dpop blocks may share one destination's host, each its own capability,
// and two destinations each take the blocks of their own host. MintedEntryRules, the part the
// profiles file alone decides, refuses the shape faults with no destination given; with no
// destination and no dpop block, any rules are taken.
func TestEveryDPoPEntryIsHeldToItsOwnDestinationsHost(t *testing.T) {
	upstream := []PrivateUpstream{dispatchUpstream(t)}
	both := upstreamsOf(t, "dispatch.test=http://10.0.0.5:8766 results.test=http://10.0.0.6:8766")
	two := proxyrules.Rules{Secrets: []proxyrules.SecretRule{mintedEntry("FIRST_TOKEN"), mintedEntry("SECOND_TOKEN")}}
	if err := ProxyMintedRules(two, upstream); err != nil {
		t.Fatalf("two dpop blocks on the destination's host: %v", err)
	}
	results := mintedEntry("RESULTS_TOKEN")
	results.Hosts = []string{"results.test"}
	eachOwn := proxyrules.Rules{Secrets: []proxyrules.SecretRule{mintedEntry("FIRST_TOKEN"), results}}
	if err := ProxyMintedRules(eachOwn, both); err != nil {
		t.Fatalf("two destinations, each bound by a dpop block of its own host: %v", err)
	}
	defaultHeader := mintedEntry("FIRST_TOKEN")
	defaultHeader.Headers = nil
	if err := ProxyMintedRules(proxyrules.Rules{Secrets: []proxyrules.SecretRule{defaultHeader}}, upstream); err != nil {
		t.Fatalf("the block with the default header: %v", err)
	}
	plain := proxyrules.Rules{Secrets: []proxyrules.SecretRule{{Name: "PLAIN_TOKEN", Hosts: []string{"api.example.com"}}}}
	if err := ProxyMintedRules(plain, nil); err != nil {
		t.Fatalf("no destination and no dpop block: %v", err)
	}
	wildcard := proxyrules.SecretRule{Name: "WILDCARD_TOKEN", Hosts: []string{"*.test"}}
	for _, c := range []struct {
		name, says string
		shape      bool // the profiles file alone decides it, so MintedEntryRules refuses it too
		upstreams  []PrivateUpstream
		spoil      func(*proxyrules.Rules)
	}{
		{"a dpop block of two hosts", "one exact host", true, upstream, func(r *proxyrules.Rules) { r.Secrets[1].Hosts = []string{"dispatch.test", "other.test"} }},
		{"a dpop block of a *. host", "one exact host", true, upstream, func(r *proxyrules.Rules) { r.Secrets[1].Hosts = []string{"*.test"} }},
		{"a dpop block with no allow list", "allow list", true, upstream, func(r *proxyrules.Rules) { r.Secrets[1].Allow = nil }},
		{"a dpop block with an empty allow list", "allow list", true, upstream, func(r *proxyrules.Rules) { r.Secrets[1].Allow = []proxyrules.Allow{} }},
		{"a dpop block allowing the path prefix \"\"", "path prefix", true, upstream, func(r *proxyrules.Rules) {
			r.Secrets[1].Allow = []proxyrules.Allow{{Method: "POST", PathPrefix: "/api/v1/"}, {Method: "POST", PathPrefix: ""}}
		}},
		{"a dpop block allowing the path prefix /", "path prefix", true, upstream, func(r *proxyrules.Rules) {
			r.Secrets[1].Allow = []proxyrules.Allow{{Method: "GET", PathPrefix: "/"}}
		}},
		{"a dpop block carried in X-Token", "Authorization header alone", true, upstream, func(r *proxyrules.Rules) { r.Secrets[1].Headers = []string{"X-Token"} }},
		{"a dpop block carried in Authorization and X-Token", "Authorization header alone", true, upstream, func(r *proxyrules.Rules) { r.Secrets[1].Headers = []string{"Authorization", "X-Token"} }},
		{"a block of no kind over the host", "WILDCARD_TOKEN", true, upstream, func(r *proxyrules.Rules) { r.Secrets = append(r.Secrets, wildcard) }},
		{"a dpop block of another host", "other.test", false, upstream, func(r *proxyrules.Rules) { r.Secrets[1].Hosts = []string{"other.test"} }},
		{"a dpop block of a host no destination of two names", "other.test", false, both, func(r *proxyrules.Rules) {
			r.Secrets[1].Hosts = []string{"other.test"}
			r.Secrets = append(r.Secrets, results)
		}},
		{"dpop blocks and no destination", "BROKER_PROXY_PRIVATE_UPSTREAM", false, nil, func(*proxyrules.Rules) {}},
		{"a destination no dpop block binds", "names the host dispatch.test", false, upstream, func(r *proxyrules.Rules) { *r = plain }},
		{"the second of two destinations no dpop block binds", "names the host results.test", false, both, func(*proxyrules.Rules) {}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rules := proxyrules.Rules{Secrets: append([]proxyrules.SecretRule(nil), two.Secrets...)}
			c.spoil(&rules)
			err := ProxyMintedRules(rules, c.upstreams)
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("ProxyMintedRules = %v; want a refusal naming %q", err, c.says)
			}
			if shapeErr := MintedEntryRules(rules); (shapeErr != nil) != c.shape {
				t.Fatalf("MintedEntryRules = %v; want a refusal %v", shapeErr, c.shape)
			}
		})
	}
}

// TestTheMintedEntryDigestCoversTheProfileAndTheDestination: a minted grant records the digest of
// the dpop block it was made under and the private destination its token goes to, so a broker of
// another block or destination on the same database serves none of its grants. The digest is 64
// hex digits; it moves with the block's own digest, with the destination's scheme and with its
// host; and it is "" for a block the profiles hold no digest of or a broker with no destination,
// which serves no minted grant.
func TestTheMintedEntryDigestCoversTheProfileAndTheDestination(t *testing.T) {
	upstream := dispatchUpstream(t)
	digest := MintedEntryDigest("profile-digest-a", upstream)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
		t.Fatalf("MintedEntryDigest = %q; want 64 hex digits", digest)
	}
	if again := MintedEntryDigest("profile-digest-a", dispatchUpstream(t)); again != digest {
		t.Fatalf("MintedEntryDigest of the same block and destination = %q, then %q; want one digest", digest, again)
	}
	other := func(raw string) PrivateUpstream {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return PrivateUpstream{Host: upstream.Host, URL: u}
	}
	for name, moved := range map[string]string{
		"another block":            MintedEntryDigest("profile-digest-b", upstream),
		"the destination's scheme": MintedEntryDigest("profile-digest-a", other("https://dispatch.dispatch.svc.cluster.local:8766")),
		"the destination's host":   MintedEntryDigest("profile-digest-a", other("http://evil.dispatch.svc.cluster.local:8766")),
		"the destination's port":   MintedEntryDigest("profile-digest-a", other("http://dispatch.dispatch.svc.cluster.local:8767")),
	} {
		if moved == digest || moved == "" {
			t.Errorf("MintedEntryDigest with %s = %q; want a digest other than %q", name, moved, digest)
		}
	}
	if got := MintedEntryDigest("", upstream); got != "" {
		t.Errorf("MintedEntryDigest of no block = %q; want none", got)
	}
	if got := MintedEntryDigest("profile-digest-a", PrivateUpstream{}); got != "" {
		t.Errorf("MintedEntryDigest with no destination = %q; want none", got)
	}
}
