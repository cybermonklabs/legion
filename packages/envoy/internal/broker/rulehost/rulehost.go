// Package rulehost is the one grammar an airlock rules host is written in, with the reason each
// host outside it is refused. Every reader of a host that can name a reason reads it here: the
// broker's validation of its rules file (cmd/broker's validateRulesFile, which runs at start and
// in airlock-check) and the local stack's stand-in token authority
// (config's BROKER_AIRLOCK_TEST_AZURE_AUTHORITY).
//
// Its first test is airlock.HostShapeOK, the Lean-proved shape airlock.Validate holds every host
// to, so no host Refusal accepts is one HostShapeOK refuses: the refusals past that test only add
// to it. It lives outside the airlock package so the Lean model of that package is untouched.
package rulehost

import (
	"net"
	"net/netip"
	"strings"

	"golang.org/x/net/idna"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/netaddr"
	"github.com/sjawhar/envoy/internal/fold"
	"github.com/sjawhar/envoy/internal/weborigin"
)

// The longest name and the longest label DNS carries.
const (
	maxName  = 253
	maxLabel = 63
)

// The reasons that are no one rule's: noDNSName is the reason of a host airlock.HostShapeOK
// refuses that no rule below names, so every such host is refused whatever the rules below miss;
// writeAHostName is the advice after a reason that names no form.
const (
	noDNSName      = "is no DNS name: a host is labels of letters, digits and hyphens between single dots, none empty and none opening or closing with a hyphen, after an optional leading wildcard label, so the airlock binds it nowhere"
	writeAHostName = "write a host name of letters, digits and hyphens between single dots, with at most one leading \"*.\""
)

// Refusal is why h is no rules host the broker serves, or "" for one it serves. The grammar is
// the one spelling every request for a host is compared in. airlock.HostBound compares an entry's
// hosts with a request's host as text, folding ASCII case; the proxy reads that host after
// net.SplitHostPort has taken its port and brackets off and netaddr.Host has written an address in
// its canonical form; and a client sends a name without a trailing dot in its TLS server name, an
// internationalized name as its punycode, and no server name at all for an address.
//
// A host airlock.HostShapeOK refuses is refused with the first reason that names what is wrong
// with it, or as no DNS name: a scheme, read before net.SplitHostPort makes it a port; an IP
// address in any spelling (ipHostRefusal); an empty host; one that is not ASCII; a trailing dot
// (dotReason); a port of digits; a character no label holds; an empty label; a label that opens
// or closes with a hyphen; a "*" other than one leading "*."; and a last label of digits alone.
// A host HostShapeOK accepts is refused only for what the grammar adds to it (beyondShape): more
// than 253 characters or a label of more than 63, a last label that is a number as the WHATWG URL
// parser's IPv4 reading takes one (0x and hex digits, 0x alone), and a label with "--" as its
// third and fourth characters other than an xn-- label the IDNA lookup profile reads back
// unchanged (weborigin.ReservedLabel). A capital letter is no reason: HostShapeOK accepts it and
// HostBound folds it, so a host in any case is read as its lower case (fold.ASCII).
//
// A refusal names the form to write when h is a host the grammar reads written another way - with
// a trailing dot, or as a Unicode name the IDNA lookup profile converts to punycode - and, for an
// IP address, the address in netaddr.Canonical's form. It repeats nothing else of h, since a
// rules file's text is any text.
func Refusal(h string) string {
	reason, advised := why(h)
	switch {
	case reason == "":
		return ""
	case advised:
		return reason
	}
	if form := canonicalForm(h); form != "" {
		return reason + "; write it as " + form
	}
	return reason + "; " + writeAHostName
}

// why is the reason h is refused with no form after it, or "" for a host the grammar reads.
// advised says the reason carries its own advice, which a scheme's and an IP address's do.
func why(h string) (reason string, advised bool) {
	if airlock.HostShapeOK(h) {
		return beyondShape(fold.ASCII(h)), false
	}
	if strings.Contains(h, "://") {
		return "holds a scheme, and a host is a name alone, so it binds nothing; write the service's host name with no scheme, path or port", true
	}
	if addr, ok := ipLiteral(h); ok {
		return ipHostRefusal(h, addr), true
	}
	if reason := shapeReason(fold.ASCII(h)); reason != "" {
		return reason, false
	}
	return noDNSName, false
}

// shapeReason names why airlock.HostShapeOK refuses h, written in lower case and holding no
// scheme and no IP address, or "" for a refusal no rule here names, which Refusal gives as no DNS
// name.
func shapeReason(h string) string {
	switch {
	case h == "":
		return "is empty, so it names no host and binds nothing"
	case !isASCII(h):
		return "is not ASCII, and a client sends an internationalized name as its punycode, so no request names this spelling and the host binds nothing"
	case strings.HasSuffix(h, "."):
		return dotReason(h)
	}
	// A port is digits: in https:/slack.com, user:pw@slack.com or mailto:ops@slack.com what
	// follows the colon is no port, and the colon is no DNS name's.
	if _, port, err := net.SplitHostPort(h); err == nil && port != "" && strings.Trim(port, "0123456789") == "" {
		return "holds a port, and the proxy compares a CONNECT's host without its port, so the host binds nothing"
	}
	name, wildcard := strings.CutPrefix(h, "*.")
	labels := strings.Split(name, ".")
	for _, label := range labels {
		if reason := labelReason(label); reason != "" {
			return reason
		}
	}
	if last := labels[len(labels)-1]; strings.Trim(last, "0123456789") == "" {
		return numberReason(wildcard)
	}
	return ""
}

// labelReason is why one label, in lower case, breaks airlock.HostShapeOK's label rule, or "" for
// one that keeps it. The first character outside a-z, 0-9 and a hyphen decides the reason, which
// names that character's kind and never the character.
func labelReason(label string) string {
	if label == "" {
		return "has an empty label, from a leading or doubled dot, so no request names it and the host binds nothing"
	}
	for i := range len(label) {
		switch c := label[i]; {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-':
		case c == '*':
			return "has a * other than one leading \"*.\", the only wildcard airlock.HostBound reads, so the host binds nothing"
		case c == '[' || c == ']':
			return "holds a bracket, which the proxy takes off a request's host, so no request names it and the host binds nothing"
		case c == '%':
			return "holds a %, which no host name holds, so the host binds nothing"
		case c <= ' ' || c == 0x7f:
			return "holds white space or a control character, which no host name holds, so the host binds nothing"
		case c == '_':
			return "holds an underscore, which is outside the LDH grammar public host names are written in: it binds when DNS answers for it, but it is no host in the one spelling a rules host is written in"
		case c == ':':
			return noDNSName
		default:
			return "holds a character other than a letter, a digit, a hyphen and a dot, which no host name holds, so the host binds nothing"
		}
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return "has a label that starts or ends with a hyphen, which a host name does not have"
	}
	return ""
}

// beyondShape is why a host airlock.HostShapeOK accepts, in lower case, is still no host in the
// grammar, or "" for one that is: what the grammar adds to HostShapeOK, and nothing HostShapeOK
// checks itself, so that HostShapeOK alone decides every host it refuses.
func beyondShape(h string) string {
	if len(h) > maxName {
		return "is longer than 253 characters, the longest name DNS carries, so the host binds nothing"
	}
	name, wildcard := strings.CutPrefix(h, "*.")
	labels := strings.Split(name, ".")
	for _, label := range labels {
		if len(label) > maxLabel {
			return "has a label longer than 63 characters, the longest DNS carries, so the host binds nothing"
		}
		if err := weborigin.ReservedLabel(label); err != nil {
			if strings.HasPrefix(label, "xn--") {
				return "has an xn-- label the IDNA lookup profile refuses, a Fake A-label (RFC 5890 section 2.3.1) that is the punycode of no internationalized name, so it is no host in the one spelling a rules host is written in"
			}
			return "has a label with \"--\" as its third and fourth characters, a reserved form (RFC 5890 section 2.3.1) public certificate authorities refuse to issue for, so it is no host in the one spelling a rules host is written in"
		}
	}
	if numericLabel(labels[len(labels)-1]) {
		return numberReason(wildcard)
	}
	return ""
}

// numberReason is the refusal of a host whose last label is a number, and of a wildcard over one.
func numberReason(wildcard bool) string {
	if wildcard {
		return "is a wildcard whose last label is a number, which no host name has, so airlock.HostBound would match it against addresses by their text"
	}
	return "ends in a number, which no host name does: the WHATWG URL parser reads a host whose last label is a number as an IPv4 address - in a form netip does not read when it has a leading zero, fewer than four parts or a hex part - or refuses it as no host, and clients read such a host differently, so it is no host in the one spelling a rules host is written in"
}

// dotReason is the refusal of a host, in lower case, that ends in a dot. Every trailing dot is
// taken off at once and the name before them read once, so the reading is linear however many
// dots the file writes. Two or more dots, and a name the resolver would not read with its dot,
// leave a name no CONNECT reaches the bound decision with. With one dot after a name the resolver
// reads, a CONNECT that writes the dot binds the entry when DNS answers for the name, and its
// client sends the name without the dot as its TLS server name, so the proxy decrypts it and
// airlock.Decide refuses every request it carries; the refusal then says, too, why the name
// without its dot is refused, when it is.
func dotReason(h string) string {
	const noName = "ends in a dot, and without it is no host name either, so no request is bound to it and the host binds nothing"
	const decrypted = "ends in a dot, which a client drops from the TLS server name it sends, so the proxy decrypts a CONNECT to it and airlock.Decide refuses every request it carries (\"sni mismatch\"): the entry carries no secret there"
	name := strings.TrimRight(h, ".")
	if len(h)-len(name) > 1 || !resolverReads(name) {
		return noName
	}
	if rest, _ := why(name); rest != "" {
		return decrypted + ", and without the dot it " + rest
	}
	return decrypted
}

// resolverReads reports whether Go's resolver reads name, in lower case and with no trailing dot,
// as a domain name (net's isDomainName): after an optional leading "*.", labels of 1 to 63
// letters, digits, hyphens and underscores that neither open nor close with a hyphen, at most
// 253 characters, and not digits alone. A wildcard's name is read as the names it matches are.
func resolverReads(name string) bool {
	name = strings.TrimPrefix(name, "*.")
	if name == "" || len(name) > maxName {
		return false
	}
	nonNumeric := false
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > maxLabel || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			switch c := label[i]; {
			case 'a' <= c && c <= 'z', c == '_', c == '-':
				nonNumeric = true
			case '0' <= c && c <= '9':
			default:
				return false
			}
		}
	}
	return nonNumeric
}

// numericLabel reports whether a label is a number as the WHATWG URL parser's IPv4 reading takes
// one: decimal, or 0x and hex digits, where 0x alone is the number 0. The last label of no host
// name is one; the WHATWG parser reads a host ending in one as an IPv4 address or refuses it.
func numericLabel(label string) bool {
	digits, hex := label, false
	if rest, ok := strings.CutPrefix(label, "0x"); ok {
		digits, hex = rest, true
	}
	if digits == "" {
		return hex
	}
	for i := range len(digits) {
		c := digits[i]
		if !('0' <= c && c <= '9' || hex && 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// isASCII reports whether s holds only ASCII bytes.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// ipLiteral is the address h writes in any spelling netip or the proxy reads one in: with a port,
// in brackets, after a trailing dot, with a zone, or IPv4-mapped.
func ipLiteral(h string) (netip.Addr, bool) {
	s := h
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"), "."))
	return addr, err == nil
}

// ipHostRefusal is the refusal of a host that is an IP address, in any spelling, with what that
// spelling would do served. The proxy reads the address a CONNECT names in netaddr.Canonical's
// form (netaddr.Host) and airlock.HostBound folds ASCII case, so the canonical spelling, and an
// IPv6 address in upper case, bind: a client sends no TLS server name for an address (RFC 6066
// section 3; Go's crypto/tls and curl send none), so the proxy decrypts a CONNECT to it and
// airlock.Decide refuses every request it carries. With a trailing dot it is a name of numbers
// alone, which the resolver refuses, so a CONNECT to it is refused before anything is bound. Any
// other spelling - a port, brackets, a zone, an IPv4-mapped or not-canonical IPv6 address - is no
// CONNECT's host, so it binds nothing and a CONNECT to the address is tunnelled unread. Either way
// the entry carries no secret. It names the address in netaddr.Canonical's form when h writes it
// another way, which holds no zone, so it repeats nothing of h.
func ipHostRefusal(h string, addr netip.Addr) string {
	const rename = "; name the service by its host name"
	canonical := netaddr.Canonical(addr).String()
	written := "is an IP address, and "
	if canonical != h {
		written = "is an IP address, " + canonical + " in its canonical form, and "
	}
	switch {
	case fold.ASCII(h) == canonical:
		return written + "a client sends no TLS server name for an address, so the proxy decrypts a CONNECT to it and airlock.Decide refuses every request it carries (\"sni mismatch\"): the entry carries no secret there" + rename
	case strings.HasSuffix(strings.TrimSuffix(h, "]"), "."):
		return written + "with a trailing dot it is a name of numbers alone, which the resolver refuses, so a CONNECT to it is refused before anything is bound: the entry carries no secret there" + rename
	}
	return written + "this spelling is no CONNECT's host, since the proxy reads the address a CONNECT names in its canonical form, so it binds nothing and a CONNECT to the address is tunnelled unread: the entry carries no secret there" + rename
}

// canonicalForm is the spelling of h the grammar reads, when h is that host written another way:
// with one trailing dot, or as a Unicode name the IDNA lookup profile converts to its punycode,
// in lower case either way; "" when there is no such spelling. It names only a host the grammar
// reads, so the only text of h it can repeat is a host name's letters, digits, hyphens and dots.
func canonicalForm(h string) string {
	prefix, name := "", h
	if rest, ok := strings.CutPrefix(h, "*."); ok {
		prefix, name = "*.", rest
	}
	if !isASCII(name) {
		ascii, err := idna.Lookup.ToASCII(name)
		if err != nil {
			return ""
		}
		name = ascii
	}
	form := prefix + strings.TrimSuffix(fold.ASCII(name), ".")
	if reason, _ := why(form); reason != "" {
		return ""
	}
	return form
}
