package natsurl

import (
	"strings"
	"testing"
)

// Check answers each rule's reason at the position of the URL that breaks it, and lets through
// what nats.go reads as written. The URLs carry no credential: what a refusal prints is held by
// the callers' tests (internal/bus credential_printed_test.go, the daemon's natsauth
// printed_test.go), which put made-up credentials in these places.
func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name     string
		urls     []string
		position int
		reason   string
	}{
		// Check reads each URL alone and models no mixing of websocket and other schemes, which
		// nats.go refuses for a list (ErrMixingWebsocketSchemes), so the rows keep each list to one
		// kind.
		{"a nats and tls list whose URLs each pass", []string{" nats://127.0.0.1:4222 ", "tls://legion@host:4223"}, 0, ""},
		{"a websocket URL with a path and a query that pass", []string{"ws://host/nats?x=1"}, 0, ""},
		{"a first URL with a user name and no scheme", []string{"legion@host:4222"}, 0, ""},
		{"a URL that does not parse", []string{"nats://host:4222", "nats://user:head/tail@host:4222"}, 2, UnparsedReason},
		{"an @ after the host", []string{"nats://head/tail@host:4222"}, 1, UserinfoPastHostReason},
		{"a later URL with a user name and no scheme", []string{"nats://user:8472", "tail@host:4222"}, 2, TailReason},
		{"a later URL with a user name and a scheme no NATS server is written with", []string{"nats://head", "x://tail@host:4222"}, 2, TailReason},
		{"a websocket path that stops parsing once decoded", []string{"ws://host/%25zz?x=1"}, 1, HandshakeReason},
		// nats.go gives a later URL with no scheme the pool's, ws or wss once the first URL names
		// it, and then runs the websocket handshake on it; in a nats list the same URL is no
		// websocket URL and passes.
		{"a ws list's later URL with no scheme whose path stops parsing once decoded", []string{"ws://host:4222", "host:4223/%25zz?x=1"}, 2, HandshakeReason},
		{"a wss list's later URL with no scheme whose path stops parsing once decoded", []string{"WSS://host:4222", "host:4223/%25zz?x=1"}, 2, HandshakeReason},
		{"a nats list's later URL with no scheme and the same path", []string{"nats://host:4222", "host:4223/%25zz?x=1"}, 0, ""},
		// nats.go drops an empty entry before it builds its pool (processUrlString), so the pool's
		// scheme is the first entry that is not empty, and a position is counted in the caller's
		// list, empty entries included.
		{"an empty first entry, then a ws list's later URL with no scheme whose path stops parsing", []string{"", "ws://host:4222", "host:4223/%25zz?x=1"}, 3, HandshakeReason},
		{"a blank first entry, then a ws URL and a later URL with a user name and no scheme", []string{" ", "ws://host:4222", "tail@host:4223"}, 3, TailReason},
		{"empty entries only", []string{"", " "}, 0, ""},
		// nats.go reads the first URL's scheme as url.Parse does: one that begins `ws:` with its
		// `://` further on is a ws URL, so a later scheme-less URL is dialled as ws.
		{"an opaque ws first URL, then a later URL with no scheme whose path stops parsing", []string{"ws:opaque://x", "host:4223/%25zz?x=1"}, 2, HandshakeReason},
		// A ws host that stops parsing once decoded is named as the host, not the path.
		{"a websocket host that stops parsing once decoded", []string{"ws://host%25zz:4222/nats"}, 1, HandshakeHostReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialled, position, reason := Check(tc.urls)
			if position != tc.position || reason != tc.reason {
				t.Fatalf("Check = %d, %q; want %d, %q", position, reason, tc.position, tc.reason)
			}
			for _, server := range dialled {
				if server == "" || server != strings.TrimSpace(server) {
					t.Fatalf("Check returned %q, an entry nats.go would not dial as written", server)
				}
			}
		})
	}
	if dialled, _, _ := Check([]string{" nats://host:4222\t", "", "nats://host:4223"}); len(dialled) != 2 || dialled[0] != "nats://host:4222" {
		t.Fatalf("Check = %q, want the two URLs trimmed and the empty entry dropped", dialled)
	}
}

// CheckEntries is Check without the tail rule: a later entry with a user name and another scheme,
// or none, is dialled; the pool's scheme and the per-URL rules still hold.
func TestCheckEntries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		urls     []string
		position int
		reason   string
	}{
		{"a later entry with a user name and another scheme", []string{"nats://head", "x://tail@host:4222"}, 0, ""},
		{"a later entry with a user name and no scheme", []string{"nats://user:8472", "tail@host:4222"}, 0, ""},
		{"a ws list's later scheme-less entry whose path stops parsing", []string{"", "ws://host:4222", "host:4223/%25zz?x=1"}, 3, HandshakeReason},
		{"an @ after the host", []string{"nats://head/tail@host:4222"}, 1, UserinfoPastHostReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, position, reason := CheckEntries(tc.urls); position != tc.position || reason != tc.reason {
				t.Fatalf("CheckEntries = %d, %q; want %d, %q", position, reason, tc.position, tc.reason)
			}
		})
	}
}

// EntryRefusal refuses a comma, which nats.go reads as a list of servers once its caller joins
// the entries, before Refusal's rules, and is Refusal otherwise.
func TestEntryRefusal(t *testing.T) {
	for _, tc := range []struct{ server, reason string }{
		{"nats://host:4222", ""},
		{"nats://host:4222,nats://host:4223", CommaReason},
		{"nats://" + "user" + ":" + "head,tail@host:4222", CommaReason},
		{"nats://head/tail@host:4222", UserinfoPastHostReason},
		{"ws://host/%25zz", HandshakeReason},
	} {
		if got := EntryRefusal(tc.server); got != tc.reason {
			t.Errorf("EntryRefusal(%q) = %q, want %q", tc.server, got, tc.reason)
		}
	}
}
