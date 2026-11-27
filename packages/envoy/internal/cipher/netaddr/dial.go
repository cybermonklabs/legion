// packages/envoy/internal/cipher/netaddr/dial.go
// Where the airlock proxy may dial for a command (DialRefusal). The proxy
// (internal/cipher/proxy) resolves a name, holds each of its addresses to DialRefusal, and dials
// the addresses it checked; this file decides the rule and nothing else: no name, socket or log
// line is here. The forms of an IPv6 address that carry an IPv4 address are read in one place,
// CarriedIPv4, which both of the package's rules read.
package netaddr

import "net/netip"

// clusterRange is 100.64.0.0/10, the shared address space of RFC 6598. Kubernetes clusters and
// cloud networks hand node and pod addresses out of it, and netip's own predicates do not name
// it, so it is spelled out here.
var clusterRange = netip.MustParsePrefix("100.64.0.0/10")

// thisNetworkRange is 0.0.0.0/8, which RFC 1122 reserves for "this network". netip's
// IsGlobalUnicast refuses only 0.0.0.0 of it, and Linux routes the rest as unicast since 5.3, so
// it is spelled out here.
var thisNetworkRange = netip.MustParsePrefix("0.0.0.0/8")

// DialRefusal names the rule that refuses a dial of addr by the airlock proxy for a command - a
// tunnel's, a bound host's request's and a plain-HTTP request's alike - and "" for an address the
// proxy may dial. Each answer names the rule and the address family, because an operator reading
// a refusal has to know which of a name's addresses it was about: a name with one public IPv4
// address and one link-local IPv6 address is refused, and the line says which half did it.
//
// clusterRanges are the broker cluster's own ranges (BROKER_AIRLOCK_CLUSTER_RANGES), read after
// the fixed rules: a Service range such as 34.118.224.0/20 is global unicast, so it is the one
// rule that stands between a tunnel and a ClusterIP of the cluster the broker runs in, and an
// address a fixed rule already refuses keeps that rule's more particular name.
//
// An IPv6 address that carries an IPv4 address (CarriedIPv4) is refused by every rule that
// refuses the address it carries, and the answer names that address and its rule, unless a rule
// above already refuses the IPv6 address itself, whose name the answer then keeps.
func DialRefusal(addr netip.Addr, clusterRanges []netip.Prefix) string {
	// A 4-in-6 address is the IPv4 address it holds, for every predicate below and for the
	// family each answer names.
	addr = Canonical(addr)
	family := "IPv6"
	if addr.Is4() {
		family = "IPv4"
	}
	switch {
	case addr.IsLoopback():
		// 127.0.0.0/8 and ::1: the broker's own pod.
		return "a loopback " + family + " address"
	case clusterRange.Contains(addr):
		// 100.64.0.0/10: the cluster's own nodes and pods.
		return "a cluster " + family + " address"
	case addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast():
		// 169.254.0.0/16 and fe80::/10: the node's metadata server among them.
		return "a link-local " + family + " address"
	case addr.IsPrivate():
		// 10/8, 172.16/12 and 192.168/16; fc00::/7, which is unique-local rather than private.
		if family == "IPv6" {
			return "a unique-local IPv6 address"
		}
		return "a private IPv4 address"
	case LocalUseTranslation.Contains(addr):
		// 64:ff9b:1::/48: a translator inside the network, whose layout the address does not say.
		return "a local-use translation IPv6 address (64:ff9b:1::/48), whose IPv4 address the address alone does not say"
	case !addr.IsGlobalUnicast():
		// Everything left that is not a host on the public internet: the unspecified address,
		// multicast, the IPv4 broadcast address, and the interface-local scopes.
		return "not a global unicast " + family + " address"
	case thisNetworkRange.Contains(addr):
		// 0.0.0.1 to 0.255.255.255: reserved for this network, and no host on the internet.
		return "an IPv4 address of 0.0.0.0/8, which is reserved for this network"
	}
	for _, own := range clusterRanges {
		if own.Contains(addr) {
			return "an " + family + " address inside BROKER_AIRLOCK_CLUSTER_RANGES, the broker cluster's own addresses"
		}
	}
	held, n := CarriedIPv4(addr)
	for _, c := range held[:n] {
		if rule := DialRefusal(c.Addr, clusterRanges); rule != "" {
			return c.Form + " " + c.Addr.String() + ", " + rule
		}
	}
	return ""
}
