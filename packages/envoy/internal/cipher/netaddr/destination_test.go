package netaddr

import (
	"net/netip"
	"strings"
	"testing"
)

// The words of DestinationNever's answers, one for each of its rules, which the rows below name.
const (
	neverLoopback = "a loopback or link-local address"
	neverNone     = "an unspecified or multicast address"
	neverCast     = "the IPv4 broadcast address"
	neverThisNet  = "an address of 0.0.0.0/8"
	neverMetadata = "a cloud metadata server's address"
	neverCarries  = "an IPv6 address that carries an IPv4 address"
	neverLocalUse = "a local-use translation IPv6 address (64:ff9b:1::/48)"
)

// destinationRanges are the cluster ranges the rows below are read against: GKE's default Service
// range and a documentation IPv6 range, each global unicast, so only the ranges admit an address
// in them.
var destinationRanges = []netip.Prefix{netip.MustParsePrefix("34.118.224.0/20"), netip.MustParsePrefix("2001:db8:5::/48")}

// neverRows are every form of an address netaddr knows that is the private destination's on no
// broker, each with the words of its rule: loopback and link-local, unicast and multicast; the
// unspecified, multicast and IPv4 broadcast addresses; 0.0.0.0/8; the cloud metadata servers; each
// IPv6 form that carries an IPv4 address, carrying a refused, a private and a public address, and
// the local-use translation prefix; each in its IPv4-mapped and zoned spellings where it has them.
// Every address the round-6 security probe found taken at round 6's head is among them.
var neverRows = []struct{ addr, want string }{
	{"127.0.0.1", neverLoopback}, {"127.1.2.3", neverLoopback}, {"::1", neverLoopback}, {"::1%lo", neverLoopback},
	{"::ffff:127.0.0.1", neverLoopback}, {"169.254.169.254", neverLoopback}, {"::ffff:169.254.169.254", neverLoopback},
	{"169.254.0.1", neverLoopback}, {"fe80::1", neverLoopback}, {"fe80::1%eth0", neverLoopback},
	{"224.0.0.1", neverLoopback}, {"ff02::1", neverLoopback},
	{"0.0.0.0", neverNone}, {"::", neverNone}, {"239.1.2.3", neverNone}, {"ff05::2", neverNone},
	{"255.255.255.255", neverCast}, {"::ffff:255.255.255.255", neverCast},
	{"0.0.0.1", neverThisNet}, {"0.255.255.255", neverThisNet}, {"::ffff:0.1.2.3", neverThisNet},
	{"fd20:ce::254", neverMetadata}, {"fd00:ec2::254", neverMetadata}, {"fd20:ce::254%eth0", neverMetadata},
	// NAT64 (64:ff9b::/96) of 169.254.169.254, 127.0.0.1, 0.0.0.1, 224.0.0.1, 255.255.255.255, the
	// private 10.0.0.5 and the public 8.8.8.8: whatever it carries, no Service has such an address.
	{"64:ff9b::a9fe:a9fe", neverCarries}, {"64:ff9b::7f00:1", neverCarries}, {"64:ff9b::1", neverCarries},
	{"64:ff9b::e000:1", neverCarries}, {"64:ff9b::ffff:ffff", neverCarries}, {"64:ff9b::a00:5", neverCarries},
	{"64:ff9b::808:808", neverCarries},
	// 6to4 (2002::/16).
	{"2002:a9fe:a9fe::1", neverCarries}, {"2002:7f00:1::1", neverCarries}, {"2002:a00:5::1", neverCarries},
	// IPv4-compatible (::/96).
	{"::a9fe:a9fe", neverCarries}, {"::7f00:1", neverCarries}, {"::a00:5", neverCarries},
	// Teredo (2001::/32): a server of 169.254.169.254, and a client of 169.254.169.254 (the last 32
	// bits inverted).
	{"2001:0:a9fe:a9fe::1", neverCarries}, {"2001:0:4136:e378::5601:5601", neverCarries},
	// IPv4-translated (::ffff:0:0:0/96, RFC 2765) of 169.254.169.254, 127.0.0.1 and the private
	// 10.0.0.5; and ISATAP (an interface identifier 0:5efe or 200:5efe, RFC 5214) under a public,
	// a unique-local and a 6to4 /64, carrying 169.254.169.254, 10.0.0.5 and 8.8.8.8.
	{"::ffff:0:a9fe:a9fe", neverCarries}, {"::ffff:0:7f00:1", neverCarries}, {"::ffff:0:a00:5", neverCarries},
	{"2606:4700:4700:1:0:5efe:a9fe:a9fe", neverCarries}, {"2606:4700:4700:1:200:5efe:808:808", neverCarries},
	{"fd12:3456::5efe:a00:5", neverCarries}, {"2002:808:808:1:0:5efe:a9fe:a9fe", neverCarries},
	// The local-use translation prefix (64:ff9b:1::/48), whatever it seems to carry.
	{"64:ff9b:1::a9fe:a9fe", neverLocalUse}, {"64:ff9b:1::a00:5", neverLocalUse}, {"64:ff9b:1:ffff:ffff:ffff:ffff:ffff", neverLocalUse},
}

// takenRows are addresses the destination takes on every broker (DestinationPrivate): the IPv4
// private ranges at their edges, 100.64.0.0/10 at its edges, unique-local IPv6, and the mapped and
// zoned spellings of them.
var takenRows = []string{
	"10.0.0.5", "10.255.255.255", "172.16.0.1", "172.31.255.254", "192.168.1.1", "100.64.0.1", "100.127.255.254",
	"fd12:3456::5", "fc00::1", "::ffff:10.0.0.5", "fd12:3456::5%eth0",
}

// rangedRows are addresses only the cluster ranges admit: a ClusterIP of the Service range and an
// address of the IPv6 range, plain, mapped and zoned.
var rangedRows = []string{"34.118.224.10", "::ffff:34.118.224.10", "2001:db8:5::10", "2001:db8:5::10%eth0"}

// outsideRows are addresses in no range the destination takes and refused by none of
// DestinationNever's rules: public addresses, the address just past each private range, 0.0.0.0/8
// and 100.64.0.0/10, one just outside the Service range, and an address just outside each form
// that carries an IPv4 address, which therefore carries none.
var outsideRows = []string{
	"8.8.8.8", "203.0.113.5", "2001:4860:4860::8888", "1.0.0.0", "11.0.0.0", "172.32.0.0", "100.128.0.0",
	"34.118.240.1", "::1:a9fe:a9fe", "64:ff9b::1:a9fe:a9fe", "64:ff9b:2::a9fe:a9fe", "2001:1:a9fe:a9fe::", "2003::1",
	"::ffff:1:a9fe:a9fe", "2606:4700:4700:1:400:5efe:a9fe:a9fe", "2606:4700:4700:1:0:5eff:a9fe:a9fe",
}

// TestTheDestinationTakesTheRangesItNamesAlone: DestinationRefusal takes an address because it is
// in a range the destination may be in - private, 100.64.0.0/10 or the cluster ranges - and for no
// other reason. Every form DestinationNever names is refused in its rule's words, the ranged
// addresses are taken with the ranges and refused without them, and an address outside every
// range is refused naming BROKER_AIRLOCK_CLUSTER_RANGES.
func TestTheDestinationTakesTheRangesItNamesAlone(t *testing.T) {
	for _, row := range neverRows {
		addr := netip.MustParseAddr(row.addr)
		if got := DestinationRefusal(addr, destinationRanges); !strings.HasPrefix(got, row.want) {
			t.Errorf("DestinationRefusal(%s) = %q; want a refusal that starts %q", row.addr, got, row.want)
		}
		if got := DestinationNever(addr); !strings.HasPrefix(got, row.want) {
			t.Errorf("DestinationNever(%s) = %q; want %q", row.addr, got, row.want)
		}
	}
	for _, a := range takenRows {
		addr := netip.MustParseAddr(a)
		if got := DestinationRefusal(addr, nil); got != "" {
			t.Errorf("DestinationRefusal(%s, no ranges) = %q; want none: it is in a private range", a, got)
		}
		if !DestinationPrivate(addr) {
			t.Errorf("DestinationPrivate(%s) = false; want true", a)
		}
	}
	for _, a := range rangedRows {
		addr := netip.MustParseAddr(a)
		if got := DestinationRefusal(addr, destinationRanges); got != "" {
			t.Errorf("DestinationRefusal(%s, %v) = %q; want none: the ranges hold it", a, destinationRanges, got)
		}
		if got := DestinationRefusal(addr, nil); got != outsideTheRanges {
			t.Errorf("DestinationRefusal(%s, no ranges) = %q; want %q", a, got, outsideTheRanges)
		}
		if DestinationPrivate(addr) {
			t.Errorf("DestinationPrivate(%s) = true; want false: only the ranges admit it", a)
		}
	}
	for _, a := range outsideRows {
		if got := DestinationRefusal(netip.MustParseAddr(a), destinationRanges); got != outsideTheRanges {
			t.Errorf("DestinationRefusal(%s) = %q; want %q", a, got, outsideTheRanges)
		}
	}
	if got := DestinationRefusal(netip.Addr{}, nil); !strings.HasPrefix(got, "no IP address") {
		t.Errorf("DestinationRefusal(the zero address) = %q; want a refusal", got)
	}
}

// TestNoClusterRangeTakesAnAddressTheDestinationIsNever: the cluster ranges widen the private
// ranges and nothing else. With ranges that hold every IPv4 and every IPv6 address, each form
// DestinationNever names is still refused in the same words.
func TestNoClusterRangeTakesAnAddressTheDestinationIsNever(t *testing.T) {
	everything := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
	for _, row := range neverRows {
		if got := DestinationRefusal(netip.MustParseAddr(row.addr), everything); !strings.HasPrefix(got, row.want) {
			t.Errorf("DestinationRefusal(%s, every address as a cluster range) = %q; want %q", row.addr, got, row.want)
		}
	}
	for _, a := range outsideRows {
		if got := DestinationRefusal(netip.MustParseAddr(a), everything); got != "" {
			t.Errorf("DestinationRefusal(%s, every address as a cluster range) = %q; want none", a, got)
		}
	}
}

// TestTheDestinationsExceptionReachesNoAddressTheProxyDials: every address the destination takes
// is one the general rule refuses a command (DialRefusal), so the exception opens no address a
// tunnel could not already reach; and every address refused as outside the ranges is one the
// general rule takes, which is what that refusal's words, "which this proxy would also tunnel to",
// say.
func TestTheDestinationsExceptionReachesNoAddressTheProxyDials(t *testing.T) {
	for _, a := range append(append([]string{}, takenRows...), rangedRows...) {
		addr := netip.MustParseAddr(a)
		if DestinationRefusal(addr, destinationRanges) == "" && DialRefusal(addr, destinationRanges) == "" {
			t.Errorf("%s: the destination takes it and the proxy would dial it for a command", a)
		}
	}
	for _, a := range outsideRows {
		if got := DialRefusal(netip.MustParseAddr(a), destinationRanges); got != "" {
			t.Errorf("DialRefusal(%s) = %q; want none, as the destination's refusal of it says", a, got)
		}
	}
}
