package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/cipher/netaddr"
)

const (
	// previousTokenLogWindow is how long a caller that authenticated with a value after the first
	// is not logged again (MatchesSharedAgentToken).
	previousTokenLogWindow = 10 * time.Minute
	// previousTokenCallerBytes bounds the address, the User-Agent and the path the previous-token
	// log keeps and writes, since the caller writes all three.
	previousTokenCallerBytes = 256
	// previousTokenCallers bounds how many callers the previous-token log remembers. A caller past
	// it is logged on each request until a sweep makes room, rather than remembered.
	previousTokenCallers = 10_000
)

// BearerToken reads a request's bearer the one way Dispatch reads it: the Authorization header
// trimmed, then the token after "Bearer ". present is false when the header is absent or blank,
// so the caller authenticates the request another way. A header that is present but is not a
// bearer gives present true and an empty token, which no credential matches. The HTTP API and
// the document websocket both read their bearer through it.
func BearerToken(r *http.Request) (token string, present bool) {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if authorization == "" {
		return "", false
	}
	token, isBearer := strings.CutPrefix(authorization, "Bearer ")
	if !isBearer {
		return "", true
	}
	return token, true
}

// SharedAgentTokens is the deployment's shared agent token setting (DISPATCH_AGENT_TOKEN), parsed
// once at boot by ParseSharedAgentTokens: the values it accepts, the first being the current one
// and every later one a value on its way out, which a rotation keeps accepting while its consumers
// move. It also remembers which callers it has logged authenticating with a later value, so one
// value serves the HTTP API and the document websocket together. A nil list accepts nothing.
type SharedAgentTokens struct {
	values [][]byte

	mu sync.Mutex
	// logged is when each caller last authenticated with a later value and was logged; swept is
	// when logged was last cleared of callers whose window had passed.
	logged map[previousTokenCaller]time.Time
	swept  time.Time
}

// previousTokenCaller is a caller as the previous-token log tells callers apart.
type previousTokenCaller struct {
	address   string
	userAgent string
}

// ParseSharedAgentTokens parses the shared agent token setting: one value, or several separated
// by whitespace, the first the current value, refused as parseSettingEntries refuses it.
func ParseSharedAgentTokens(setting string) (*SharedAgentTokens, error) {
	entries, err := parseSettingEntries(setting)
	if err != nil {
		return nil, err
	}
	tokens := &SharedAgentTokens{
		values: make([][]byte, 0, len(entries)),
		logged: map[previousTokenCaller]time.Time{},
	}
	for _, entry := range entries {
		tokens.values = append(tokens.values, []byte(entry))
	}
	return tokens, nil
}

// parseSettingEntries parses a setting that holds one secret value or several separated by
// whitespace (DISPATCH_AGENT_TOKEN, DISPATCH_SIGNING_KEY), and returns them in order. Whitespace at
// either end is not a value. It refuses an empty entry (two whitespace characters in a row, a
// CR LF among them) and an entry that repeats an earlier one, naming each by its position, never
// its value, since the error reaches the boot log.
func parseSettingEntries(setting string) ([]string, error) {
	entries := splitAtEachSpace(strings.TrimSpace(setting))
	positions := make(map[string]int, len(entries))
	for index, entry := range entries {
		if entry == "" {
			return nil, fmt.Errorf("entry %d of %d is empty: separate the values with one space or one line feed", index+1, len(entries))
		}
		if earlier, repeated := positions[entry]; repeated {
			return nil, fmt.Errorf("entry %d of %d repeats entry %d", index+1, len(entries), earlier)
		}
		positions[entry] = index + 1
	}
	return entries, nil
}

// splitAtEachSpace splits s at every whitespace character, so two in a row enclose an empty
// entry, and an empty s is one empty entry.
func splitAtEachSpace(s string) []string {
	var entries []string
	start := 0
	for index := 0; index < len(s); {
		r, size := utf8.DecodeRuneInString(s[index:])
		if unicode.IsSpace(r) {
			entries = append(entries, s[start:index])
			start = index + size
		}
		index += size
	}
	return append(entries, s[start:])
}

// MatchesSharedAgentToken reports whether a request's bearer token is one of the deployment's
// shared agent token values (DISPATCH_AGENT_TOKEN). It compares the bearer with every value with
// subtle.ConstantTimeCompare and never stops early, so its timing gives away no byte of any
// value. As with a single value, a compare returns at once on a length mismatch, so the values'
// lengths are not hidden, and a match on a later value does the logging work below. A nil list
// matches nothing, and the empty bearer matches no value, since none is empty. The HTTP API and
// the document websocket both authenticate the shared token through it.
//
// A bearer matching a value after the first is logged with the request's caller address
// (ForwardedAddress: the one the middleware around the router resolved under the trusted proxy
// ranges, which the access log and the broker's bucket key on too), its User-Agent and its path,
// once per address and User-Agent every previousTokenLogWindow, so a rotation can see whether the
// value on its way out is still in use. The line names the value's position and never a token.
func MatchesSharedAgentToken(r *http.Request, token string, tokens *SharedAgentTokens) bool {
	if tokens == nil {
		return false
	}
	bearer := []byte(token)
	// entry is the 1-based position of the value the bearer matched, and 0 for none.
	entry := 0
	for index, value := range tokens.values {
		entry = subtle.ConstantTimeSelect(subtle.ConstantTimeCompare(bearer, value), index+1, entry)
	}
	if entry > 1 {
		tokens.notePreviousToken(r, entry)
	}
	return entry != 0
}

// notePreviousToken logs a request that authenticated with entry, a value after the first, unless
// its caller was logged within the window.
func (t *SharedAgentTokens) notePreviousToken(r *http.Request, entry int) {
	caller := previousTokenCaller{
		address:   clipCallerField(ForwardedAddress(r)),
		userAgent: clipCallerField(r.UserAgent()),
	}
	now := time.Now()
	t.mu.Lock()
	last, seen := t.logged[caller]
	if seen && now.Sub(last) < previousTokenLogWindow {
		t.mu.Unlock()
		return
	}
	// Callers whose window has passed are cleared once a window, so the map holds only the
	// callers of the last two windows however many come and go.
	swept := now.Sub(t.swept) >= previousTokenLogWindow
	if swept {
		for logged, at := range t.logged {
			if now.Sub(at) >= previousTokenLogWindow {
				delete(t.logged, logged)
			}
		}
		t.swept = now
	}
	// A stored key is a copy: the caller's fields are slices of its request's headers, which a key
	// holding them would keep whole. A caller already remembered is refreshed even when the map
	// is full, so it is not logged on every request until the next sweep. remembered does not need
	// a second lookup: reaching here with seen true means now.Sub(last) >= previousTokenLogWindow
	// (the early return above didn't fire), which is exactly the sweep's own deletion test above, so
	// a sweep this call (swept) always deletes caller's own stale entry; one that didn't run leaves
	// it exactly as seen found it.
	if remembered := seen && !swept; remembered || len(t.logged) < previousTokenCallers {
		t.logged[previousTokenCaller{
			address:   strings.Clone(caller.address),
			userAgent: strings.Clone(caller.userAgent),
		}] = now
	}
	t.mu.Unlock()
	slog.Warn("dispatch: request authenticated with a previous shared agent token",
		"entry", entry, "address", caller.address, "user_agent", caller.userAgent, "path", clipCallerField(r.URL.Path))
}

// clipCallerField cuts a caller-written field to previousTokenCallerBytes, dropping a rune the cut
// splits.
func clipCallerField(field string) string {
	if len(field) <= previousTokenCallerBytes {
		return field
	}
	return strings.ToValidUTF8(field[:previousTokenCallerBytes], "")
}

// ForwardedAddress is the caller's address as Dispatch keys it: the access log's address
// (accesslog.Handler), the previous-token line's, and the per-address bucket the secrets broker's
// token check keys on (cmd/dispatch's brokerTokenVerifier). Nothing admits a caller by it.
//
// A request the middleware around the router resolved carries its answer (WithForwardedAddress),
// and ForwardedAddress answers it: ResolveForwardedAddress's reading under the trusted proxy
// ranges (DISPATCH_TRUSTED_PROXY_RANGES), so every reader of the address on one request reads the
// one answer. A request that carries none, one no router served, is read as
// ResolveForwardedAddress reads it with no range configured: the rightmost X-Forwarded-For entry,
// the address the load balancer in front of Dispatch saw, or the connection's own address without
// its port when the header names none, the reading the access log and the broker's bucket have
// always had.
func ForwardedAddress(r *http.Request) string {
	if address, resolved := r.Context().Value(forwardedAddressKey{}).(string); resolved {
		return address
	}
	// With no range configured no peer is a trusted proxy's, so the reading refuses nothing.
	address, _ := ResolveForwardedAddress(r, nil)
	return address
}

// ResolveForwardedAddress is the caller's address as Dispatch keys it once the proxies
// DISPATCH_TRUSTED_PROXY_RANGES names (configured) are trusted, which the one middleware around
// the router resolves for each request and hands its readers (WithForwardedAddress), and the error
// that refuses a request whose X-Forwarded-For such a proxy could not have written.
//
// A peer in configured, such as an internal Application Load Balancer's proxy-only subnet, appends
// to X-Forwarded-For as Google's balancers do: the client it saw, then its forwarding rule's
// address, the same for every caller. So it is read as the listener's edge reads Google's
// balancer, by netaddr.ForwardedClient: the second-to-last entry of the one line, an IPv4 address
// or the /64 an IPv6 one is in, never an entry to its left, which the caller wrote.
// ForwardedClient's error (other than one line, fewer than two entries, or a second-to-last entry
// that is no IP address) is the answer then, since no such proxy wrote that header, and reading
// anything from it would let the caller pick its key.
//
// A peer in no range keeps the rule Dispatch has always read: the rightmost entry, or the
// connection's own address when the header names none. That entry is text a caller that reaches
// Dispatch directly writes itself, so a forged line of two entries never makes the one it wrote
// second from the right its key, and its header is never refused.
func ResolveForwardedAddress(r *http.Request, configured []netip.Prefix) (string, error) {
	peer := peerAddress(r)
	forwarded := r.Header.Values("X-Forwarded-For")
	if inRanges(peer, configured) {
		return netaddr.ForwardedClient(forwarded)
	}
	if len(forwarded) > 0 {
		last := forwarded[len(forwarded)-1]
		if comma := strings.LastIndexByte(last, ','); comma >= 0 {
			last = last[comma+1:]
		}
		if address := strings.TrimSpace(last); address != "" {
			return address, nil
		}
	}
	return peer, nil
}

// forwardedAddressKey is the context key the address the middleware around the router resolved
// for a request's caller is carried under (WithForwardedAddress).
type forwardedAddressKey struct{}

// WithForwardedAddress is ctx carrying address, the caller's address ResolveForwardedAddress
// answered for the request ctx is of, which ForwardedAddress then answers. The middleware around
// the router sets it once, where it refuses a request ResolveForwardedAddress reads no address
// from, so the address that decides the refusal is the one every reader keys on: the access log,
// the previous-token line and the broker token check's bucket.
func WithForwardedAddress(ctx context.Context, address string) context.Context {
	return context.WithValue(ctx, forwardedAddressKey{}, address)
}

// peerAddress is the connection's own address without its port, or RemoteAddr as it stands when
// it holds no port.
func peerAddress(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// inRanges reports whether address, an IP address written as text, is in one of ranges, read as
// netaddr.Canonical reads it: an IPv4 address written as IPv6 is the IPv4 address it holds, and a
// zone is dropped, either of which netip.Prefix.Contains would otherwise find in no IPv4 range.
// Text that is no IP address is in none.
func inRanges(address string, ranges []netip.Prefix) bool {
	if len(ranges) == 0 {
		return false
	}
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	addr = netaddr.Canonical(addr)
	for _, prefix := range ranges {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
