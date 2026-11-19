// Package netaddr is the one reading of an IP address every address check of the broker makes:
// the airlock proxy's rules of where it may dial, the cluster ranges, the bound-host check, the
// dial the transport checks again, and the addresses config reads at boot.
//
// It exists because netip keeps two things a socket does not need. An IPv4-mapped IPv6 address
// (::ffff:34.118.224.1) is the IPv4 address it holds, and netip's predicates answer differently
// for the two spellings. A zone (2001:db8:5::10%eth0) is kept on the address, and
// netip.Prefix.Contains is false for any zoned address, while Linux connects to a zoned global
// address as if it had no zone. A check that reads either answers about an address the socket
// never goes to, so every check reads Canonical's answer, and the proxy dials that same address.
package netaddr

import "net/netip"

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
