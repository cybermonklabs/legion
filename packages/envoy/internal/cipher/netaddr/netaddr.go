// Package netaddr is the one reading of an IP address every address check of the broker makes:
// the airlock proxy's rules of where it may dial, the cluster ranges, the bound-host check, the
// dial the transport checks again, and the addresses config reads at boot. It is also the one
// reading of the client address a load balancer forwards (ForwardedClient), which an edge reached
// through Google's load balancer keys its buckets on.
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
