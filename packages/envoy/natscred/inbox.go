package natscred

import (
	"strings"

	"github.com/nats-io/nats.go"
)

// inboxPrefixBase begins every reply prefix InboxPrefix makes: nats.go's own prefix, "_INBOX.",
// with its dot written "_", so the token differs from nats.go's "_INBOX" and no prefix reaches
// another's.
var inboxPrefixBase = strings.TrimSuffix(nats.InboxPrefix, ".") + "_"

// InboxPrefix is the reply prefix of the client whose credential names identity (Identity): "_INBOX_"
// and identity with each byte outside [A-Za-z0-9-], '_' included, written '_' and its two lowercase
// hex digits. '_' always opens an escape, so distinct identities have distinct prefixes, and the
// prefix is one subject token holding no '.', wildcard, whitespace or control character. A service
// connection sets it with nats.CustomInboxPrefix, and the NATS auth service grants that connection
// InboxPrefix(identity)+".>" in place of _INBOX.>, so no other client's replies reach it. It lives
// here, beside the credential, because a client package that the grant table itself imports
// (internal/bus) sets it; natsgrant.InboxPrefix is this function.
func InboxPrefix(identity string) string {
	const digits = "0123456789abcdef"
	var out strings.Builder
	out.Grow(len(inboxPrefixBase) + 3*len(identity))
	out.WriteString(inboxPrefixBase)
	for i := range len(identity) {
		c := identity[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' {
			out.WriteByte(c)
		} else {
			out.WriteByte('_')
			out.WriteByte(digits[c>>4])
			out.WriteByte(digits[c&15])
		}
	}
	return out.String()
}
