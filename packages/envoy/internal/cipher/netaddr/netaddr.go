// Package netaddr is the one reading of an IP address every address check of the broker makes:
// the airlock proxy's rules of where it may dial, the cluster ranges, the bound-host check, the
// dial the transport checks again, and the addresses config reads at boot; the IPv4 address an
// IPv6 address carries (CarriedIPv4); and the one list of addresses a private destination may
// never be (DestinationRefusal). It is also the one reading of the client address a load balancer
// forwards (ForwardedClient), which an edge reached through Google's load balancer keys its
// buckets on.
//
// It exists because netip keeps two things a socket does not need. An IPv4-mapped IPv6 address
// (::ffff:34.118.224.1) is the IPv4 address it holds, and netip's predicates answer differently
// for the two spellings. A zone (2001:db8:5::10%eth0) is kept on the address, and
// netip.Prefix.Contains is false for any zoned address, while Linux connects to a zoned global
// address as if it had no zone. A check that reads either answers about an address the socket
// never goes to, so every check reads Canonical's answer, and the proxy dials that same address.
package netaddr

import (
	"errors"
	"net/netip"
	"strings"
)

// Canonical is addr as every address check reads it and as the proxy dials it: an IPv4-mapped
// address is the IPv4 address it holds, and a zone is dropped. A zoned link-local or loopback
// address is still link-local or loopback without its zone, so every rule that refuses one still
// refuses it.
func Canonical(addr netip.Addr) netip.Addr {
	return addr.Unmap().WithZone("")
}

// Host is host with an IP literal written as Canonical writes it, and any other host as it is.
// A host string is what the bound-host check compares and what the proxy's certificate names, so
// the zoned or mapped spelling of an address is that address there too.
func Host(host string) string {
	if addr, err := netip.ParseAddr(host); err == nil {
		return Canonical(addr).String()
	}
	return host
}

// metadataServers are the cloud metadata servers' IPv6 addresses. Each is a unique-local address
// (fc00::/7), which is private and so admitted by every rule that takes a private address, but it
// is the metadata server of the node a pod runs on, as 169.254.169.254 is over IPv4:
//   - fd20:ce::254: Google Compute Engine's metadata server on an IPv6-only instance
//     (https://docs.cloud.google.com/compute/docs/metadata/querying-metadata, "Metadata server
//     endpoints");
//   - fd00:ec2::254: the instance metadata service of Amazon EC2 on IPv6
//     (https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-IMDS-existing-instances.html,
//     "The IMDS has two endpoints on an instance").
var metadataServers = []netip.Addr{netip.MustParseAddr("fd20:ce::254"), netip.MustParseAddr("fd00:ec2::254")}

// The IPv6 forms that carry an IPv4 address (CarriedIPv4). A network with a translator or relay
// for a form sends a packet for such an address to the IPv4 address inside it: a NAT64
// translator rewrites it as a packet to that address, and a 6to4, Teredo or ISATAP relay, or a
// host that still routes the IPv4-compatible form, carries it there inside an IPv4 packet. So
// where the network passes the address on, a tunnel to 64:ff9b::a9fe:a9fe is a connection to
// 169.254.169.254, the node's metadata server. The IPv4-translated form of SIIT names an IPv6
// node rather than an IPv4 destination (RFC 2765 section 2.1), but a translator or host that
// reads its last 32 bits reaches the IPv4 address there, so it is refused the same way. Every
// check refuses each such address by the address it carries rather than rely on every translator
// and relay to refuse it. netip's predicates read none of these as the address they carry, and
// the IPv4-mapped form is read as its IPv4 address already (Canonical).
var (
	// 64:ff9b::/96, NAT64's well-known prefix (RFC 6052): the IPv4 address is the last 32 bits.
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	// 2002::/16, 6to4 (RFC 3056): the IPv4 address is the 32 bits after the prefix.
	sixToFourPrefix = netip.MustParsePrefix("2002::/16")
	// 2001::/32, Teredo (RFC 4380): the server's IPv4 address is the 32 bits after the prefix,
	// and the client's is the last 32 bits with every bit inverted.
	teredoPrefix = netip.MustParsePrefix("2001::/32")
	// ::/96, the IPv4-compatible form RFC 4291 deprecated: the IPv4 address is the last 32 bits.
	// :: and ::1 are inside it, and every check's fixed rules refuse each as itself first.
	ipv4CompatiblePrefix = netip.MustParsePrefix("::/96")
	// ::ffff:0:0:0/96, the IPv4-translated form of SIIT (RFC 2765), which RFC 6145 replaced: the
	// IPv4 address is the last 32 bits. It is not the IPv4-mapped ::ffff:0:0/96, which Canonical
	// reads as its IPv4 address.
	ipv4TranslatedPrefix = netip.MustParsePrefix("::ffff:0:0:0/96")
)

// LocalUseTranslation is 64:ff9b:1::/48, the local-use translation prefix (RFC 8215), which IANA
// lists as not globally reachable. A network places translation prefixes of its own choosing
// inside it, for mechanisms of its own choosing, so the address alone does not say where an IPv4
// address sits in it, or whether one does; RFC 8215 section 5 forbids a host to assume either. So
// no reading of the address finds what it carries, and every check refuses the whole prefix.
var LocalUseTranslation = netip.MustParsePrefix("64:ff9b:1::/48")

// Carried is an IPv4 address an IPv6 address carries (CarriedIPv4), and the words of a refusal
// that say how. The address is always IPv4, for which CarriedIPv4 answers nothing, so a check's
// reading of a carried address goes one level deep and no further.
type Carried struct {
	Addr netip.Addr
	Form string
}

// CarriedIPv4 answers the IPv4 addresses addr carries in one of the forms above or in an ISATAP
// interface identifier, n of them: a Teredo address's server and client, the address one other
// form carries, and beside either the address an ISATAP identifier carries; none for an IPv4
// address or an IPv6 address in no such form. addr is read as Canonical answers it: a zoned
// address is inside no prefix (netip.Prefix.Contains), so it would carry no address of the prefix
// forms. An array rather than a slice, since every address of every dial is read here.
func CarriedIPv4(addr netip.Addr) (held [3]Carried, n int) {
	addr = Canonical(addr)
	if !addr.Is6() {
		return held, 0
	}
	a := addr.As16()
	at := func(i int) netip.Addr { return netip.AddrFrom4([4]byte(a[i : i+4])) }
	switch {
	case nat64Prefix.Contains(addr):
		held[0] = Carried{at(12), "the NAT64 form (64:ff9b::/96) of"}
		n = 1
	case sixToFourPrefix.Contains(addr):
		held[0] = Carried{at(2), "the 6to4 form (2002::/16) of"}
		n = 1
	case teredoPrefix.Contains(addr):
		held[0] = Carried{at(4), "a Teredo address (2001::/32) whose server is"}
		held[1] = Carried{netip.AddrFrom4([4]byte{^a[12], ^a[13], ^a[14], ^a[15]}), "a Teredo address (2001::/32) whose client is"}
		n = 2
	case ipv4CompatiblePrefix.Contains(addr):
		held[0] = Carried{at(12), "the IPv4-compatible form (::/96) of"}
		n = 1
	case ipv4TranslatedPrefix.Contains(addr):
		held[0] = Carried{at(12), "the IPv4-translated form (::ffff:0:0:0/96) of"}
		n = 1
	}
	// ISATAP (RFC 5214): the interface identifier 0:5efe, or 200:5efe for a globally unique IPv4
	// address, then that address, under any /64 prefix, a 6to4 one among them. So it is read
	// beside the prefixes above, and an address may carry one IPv4 address in each.
	if a[8]&^0x02 == 0 && a[9] == 0 && a[10] == 0x5e && a[11] == 0xfe {
		held[n] = Carried{at(12), "the ISATAP form (interface identifier 0:5efe or 200:5efe) of"}
		n++
	}
	return held, n
}

// DestinationRefusal answers why addr can be no address of the airlock's private destination
// (BROKER_PROXY_PRIVATE_UPSTREAM), whatever the cluster ranges say, or "" when it can be one: a
// loopback or link-local address, unicast or multicast, where the broker's own pod and the node's
// metadata server answer; an unspecified or multicast address, which is no one service; a cloud
// metadata server's IPv6 address (metadataServers); a local-use translation address
// (LocalUseTranslation), whose carried address the address does not say; and an IPv6 address that
// carries an address refused here (CarriedIPv4), named by its form and that address. The answer
// completes "... resolves to". addr is read as Canonical reads it. The proxy's check of every
// dial to the destination (proxy.Server.upstreamAddresses) and config's start check of a
// destination URL that names an address read this one answer, so the two cannot disagree.
func DestinationRefusal(addr netip.Addr) string {
	addr = Canonical(addr)
	switch {
	case addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast():
		return "a loopback or link-local address, which the airlock never dials"
	case addr.IsUnspecified() || addr.IsMulticast():
		return "an unspecified or multicast address, which the airlock never dials"
	case LocalUseTranslation.Contains(addr):
		return "a local-use translation IPv6 address (64:ff9b:1::/48), whose IPv4 address the address alone does not say, which the airlock never dials"
	}
	for _, server := range metadataServers {
		if addr == server {
			return "a cloud metadata server's address, which the airlock never dials"
		}
	}
	held, n := CarriedIPv4(addr)
	for _, c := range held[:n] {
		if why := DestinationRefusal(c.Addr); why != "" {
			return c.Form + " " + c.Addr.String() + ", " + why
		}
	}
	// No tunnelled or translated address is a destination inside the broker's own cluster,
	// whatever it carries: the network sends it to the IPv4 address inside it, which a check of
	// the IPv6 address alone does not read. So every carried form is refused, the first it holds
	// named, and so is an ISATAP address (RFC 5214), whose interface id carries an IPv4 address
	// inside any prefix, a unique-local one included.
	if n > 0 {
		return held[0].Form + " " + held[0].Addr.String() + ", a tunnelled or translated address, which is no private destination"
	}
	return ""
}

// The ways a request carries no client address a load balancer forwarded (ForwardedClient).
var (
	ErrForwardedLines   = errors.New("the request carries other than one X-Forwarded-For line")
	ErrForwardedEntries = errors.New("its X-Forwarded-For line holds fewer than two entries")
	ErrForwardedAddress = errors.New("the second-to-last entry of its X-Forwarded-For line is no IP address")
)

// ForwardedClient is the client address that lines, the values of a request's X-Forwarded-For
// header, name when a load balancer that writes `<what the client sent>,<client>,<itself>` as one
// line sent it, as Google's external Application Load Balancer does, and it is the address as a
// bucket's key: the second-to-last entry of the one line, an IPv4 address (one written as IPv6
// included, Canonical), or for IPv6 the /64 holding it, since one host or one site holds a whole
// /64 and could take a fresh bucket from each of its addresses.
//
// The last entry is the balancer's own address, the same for every caller, and every entry before
// the second-to-last is the client's own text, which it could change to choose a bucket. So other
// than one line is ErrForwardedLines: Google documents one, and which of several lines a balancer
// appends to is not documented, so a line the client sent could be the one read. A line of fewer
// than two entries is ErrForwardedEntries, and a second-to-last entry that is no IP address
// ErrForwardedAddress: no such balancer wrote either. Whether the request came through the balancer
// at all is the caller's to know (its connection's address), since any client can write the header.
func ForwardedClient(lines []string) (string, error) {
	if len(lines) != 1 {
		return "", ErrForwardedLines
	}
	hops := strings.Split(lines[0], ",")
	if len(hops) < 2 {
		return "", ErrForwardedEntries
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(hops[len(hops)-2]))
	if err != nil {
		return "", ErrForwardedAddress
	}
	addr = Canonical(addr)
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().String(), nil
	}
	return addr.String(), nil
}
