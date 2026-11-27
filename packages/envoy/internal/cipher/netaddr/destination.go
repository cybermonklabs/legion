// packages/envoy/internal/cipher/netaddr/destination.go
// The addresses the airlock's private destinations (BROKER_PROXY_PRIVATE_UPSTREAM) may be: the
// one rule the proxy's dial of each destination (proxy.Server.upstreamAddresses), config's start
// check of a destination URL that names an address, and `profiles check --private-upstream` read,
// so none of them can take an address another refuses.
//
// The rule is positive. An address is a destination's because it is in a range named here - a
// private range, 100.64.0.0/10, or the broker cluster's own ranges - and never because another
// rule refuses it: the general rule of where the proxy may dial (DialRefusal) refuses more forms
// of an address as it learns them, and each such form read as "private" would be one a
// destination takes. So every form an address can take is decided here, once: the IPv4-mapped
// form and a zone (Canonical), each form that carries an IPv4 address (CarriedIPv4) and the
// local-use translation prefix (LocalUseTranslation), the unspecified and the broadcast addresses,
// 0.0.0.0/8, loopback, link-local, multicast and the cloud metadata servers.
package netaddr

import "net/netip"

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

// ipv4Broadcast is 255.255.255.255, the IPv4 limited broadcast address, which no one service has.
// netip has no predicate of its own for it: it is only not global unicast.
var ipv4Broadcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})

// outsideTheRanges is DestinationRefusal's answer for an address no fixed rule refuses and no
// range of the destination's holds. Every such address is one DialRefusal takes, so the proxy
// would tunnel to it for a command (TestEveryAddressOutsideTheRangesIsOneTheProxyDials).
const outsideTheRanges = "an address outside the private ranges and BROKER_AIRLOCK_CLUSTER_RANGES, which this proxy would also tunnel to: list its range in BROKER_AIRLOCK_CLUSTER_RANGES"

// DestinationNever answers why addr can be the private destination's on no broker, whatever its
// cluster ranges say, or "" when it can be one on some broker. The answer completes "... resolves
// to" and names the rule and never the address, since the proxy's answer to the command, which a
// CI step's public log holds, ends with it. addr is read as Canonical reads it, so an IPv4-mapped
// address is the IPv4 address it holds and a zoned address is that address. It refuses:
//   - a loopback or link-local address, unicast or multicast, where the broker's own pod and the
//     node's metadata server answer;
//   - an unspecified or multicast address, the IPv4 broadcast address and 0.0.0.0/8 ("this
//     network", RFC 1122), which no one service has;
//   - a cloud metadata server's IPv6 address (metadataServers), which is unique-local;
//   - an IPv6 address that carries an IPv4 address (CarriedIPv4: the NAT64, 6to4, Teredo,
//     IPv4-compatible, IPv4-translated and ISATAP forms), whatever address it carries, and the
//     local-use translation prefix 64:ff9b:1::/48 (LocalUseTranslation), whose IPv4 address the
//     address alone does not say: the network sends a packet for such an address to an IPv4
//     address that is no address of the destination's own, and no Service of a cluster has one.
func DestinationNever(addr netip.Addr) string {
	addr = Canonical(addr)
	switch {
	case !addr.IsValid():
		return "no IP address, which the airlock never dials"
	case addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast():
		return "a loopback or link-local address, which the airlock never dials"
	case addr.IsUnspecified() || addr.IsMulticast():
		return "an unspecified or multicast address, which the airlock never dials"
	case addr == ipv4Broadcast:
		return "the IPv4 broadcast address, which the airlock never dials"
	case thisNetworkRange.Contains(addr):
		return "an address of 0.0.0.0/8, which is reserved for this network and which the airlock never dials"
	case LocalUseTranslation.Contains(addr):
		return "a local-use translation IPv6 address (64:ff9b:1::/48), which is never the private destination's"
	}
	for _, server := range metadataServers {
		if addr == server {
			return "a cloud metadata server's address, which the airlock never dials"
		}
	}
	if _, n := CarriedIPv4(addr); n > 0 {
		return "an IPv6 address that carries an IPv4 address (a NAT64, 6to4, Teredo, IPv4-compatible, IPv4-translated or ISATAP form), which is never the private destination's"
	}
	return ""
}

// DestinationPrivate reports whether addr is in a range the private destination may be in on
// every broker: an IPv4 address of 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16, a unique-local
// IPv6 address (fc00::/7), or an address of 100.64.0.0/10, the shared address space a cluster
// hands its nodes and pods out of. It is one half of DestinationRefusal's positive test, the half
// that needs no setting; the other is the broker's cluster ranges. It decides nothing
// DestinationNever decides: a metadata server's unique-local address is in a range here and
// refused there, and DestinationRefusal reads DestinationNever first.
func DestinationPrivate(addr netip.Addr) bool {
	addr = Canonical(addr)
	return addr.IsPrivate() || clusterRange.Contains(addr)
}

// DestinationRefusal answers why addr can be no address of the private destination on a broker
// whose cluster ranges are clusterRanges (BROKER_AIRLOCK_CLUSTER_RANGES), or "" for an address
// every dial to the destination may open: DestinationNever's answer when it has one; else "" for
// an address DestinationPrivate takes or one inside clusterRanges, since a Service range such as
// 34.118.224.0/20 is global unicast and Dispatch's ClusterIP can be in it; else a refusal naming
// BROKER_AIRLOCK_CLUSTER_RANGES. Each address the destination's host resolves to is held to it at
// every dial, and a URL whose host is an address is held to it at the broker's start. Every
// address it takes is one DialRefusal refuses, so the destination's exception reaches no address
// the proxy would tunnel to for a command.
func DestinationRefusal(addr netip.Addr, clusterRanges []netip.Prefix) string {
	if why := DestinationNever(addr); why != "" {
		return why
	}
	if DestinationPrivate(addr) {
		return ""
	}
	addr = Canonical(addr)
	for _, own := range clusterRanges {
		if own.Contains(addr) {
			return ""
		}
	}
	return outsideTheRanges
}
