package main

import (
	"bytes"
	"errors"
	"net/url"
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// withPassword is the NATS URL raw with the user name "user" and the password "s3cret" in it, as
// a URL an operator gives can carry them.
func withPassword(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	parsed.User = url.UserPassword("user", "s3cret")
	return parsed.String()
}

// withToken is the server hostPort as a NATS URL whose user info is token alone, which nats.go
// reads as a token.
func withToken(hostPort, token string) string {
	return (&url.URL{Scheme: "nats", User: url.User(token), Host: hostPort}).String()
}

// A URL that names a list of servers is refused, and nothing is provisioned. nats.go splits its
// URL on ',' into a pool it dials in random order, while the loopback check reads one host, the
// one after the authority's last '@'. So nats://x@<hub>,y@<loopback> passed that check, and the
// fixture could provision the hub. Here the list's loopback server is the test's own, which
// nats.go reaches when the other does not resolve, so a fixture that dialed the list provisions it.
func TestFixturebucketsRefusesAListOfServers(t *testing.T) {
	_, uri := testnats.Start(t)
	own := strings.TrimPrefix(uri, "nats://")
	list := withToken("db.example:4222", "x") + "," + strings.TrimPrefix(withToken(own, "y"), "nats://")
	var out bytes.Buffer
	err := run([]string{"-url", list}, &out)
	const want = "names a list of servers"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("fixturebuckets answered %v and printed %q for a list of servers, want a refusal that %s", err, out.String(), want)
	}
	conn := testnats.Connect(t, uri)
	defer conn.Close()
	js, jsErr := conn.JetStream()
	if jsErr != nil {
		t.Fatalf("jetstream: %v", jsErr)
	}
	if _, err := js.StreamInfo(bustest.MarkerStream); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("the list's loopback server holds %s (read: %v): the refused fixture provisioned it", bustest.MarkerStream, err)
	}
}

// fixturebuckets names the server it provisions by the host and port it checked was on loopback,
// and nothing else of the URL it was given: a rig can hand it a URL an operator gave (the
// skill-scenarios rig's SKILL_SCENARIOS_NATS_URL), from which nats.go reads a password or a token,
// and the rig keeps its output in a log and prints it on a failure. A refusal is fixed text that
// names what was wrong and prints no part of the URL it refused, not even a host read from it,
// since a rejected URL can hold credential text there. Its standard output is out here, and its
// standard error the error main prints.
func TestFixturebucketsNamesItsServerWithoutTheURLsUserInfo(t *testing.T) {
	// The parts of the URLs below that a message may not hold.
	rest := []string{"s3cret", "user", "nats://", "/a/path", "q=1", "frag", "db.example"}
	held := func(message string) []string {
		var found []string
		for _, part := range rest {
			if strings.Contains(message, part) {
				found = append(found, part)
			}
		}
		return found
	}
	t.Run("a server it provisions", func(t *testing.T) {
		_, uri := testnats.Start(t)
		server := strings.TrimPrefix(uri, "nats://")
		var out bytes.Buffer
		if err := run([]string{"-url", withPassword(t, uri)}, &out); err != nil {
			t.Fatalf("provision %s: %v", server, err)
		}
		if found := held(out.String()); len(found) > 0 || !strings.Contains(out.String(), "provisioned the server at "+server+":") {
			t.Fatalf("fixturebuckets printed %q, holding %q, want the server named as %s alone", out.String(), found, server)
		}
	})
	t.Run("a URL it refuses", func(t *testing.T) {
		// Off loopback with a password, path, query and fragment; a host that is credential text;
		// and a URL that does not parse: one fixed refusal for each, holding no part of any.
		for _, refused := range []string{
			withPassword(t, "nats://db.example:4222/a/path?q=1#frag"),
			"nats://s3cret:4222",
			// Built at run time, so no literal reads as a URL's user info to a scan of the source.
			"nats://user:s3cret" + string(rune(64)) + "[::1/a/path?q=1#frag",
		} {
			var out bytes.Buffer
			err := run([]string{"-url", refused}, &out)
			const want = "fixturebuckets: the URL names no server on this machine's loopback that fixturebuckets can read (127.0.0.1, localhost or ::1): a fixture provisions only its own server"
			if err == nil || "fixturebuckets: "+err.Error() != want || out.Len() > 0 {
				t.Fatalf("fixturebuckets answered %v and printed %q, want the refusal %q and nothing printed", err, out.String(), want)
			}
			if found := held(err.Error()); len(found) > 0 {
				t.Fatalf("fixturebuckets' refusal %q holds %q of the URL it refused", err, found)
			}
		}
	})
}

// fixturebuckets reads its server's URL from FIXTUREBUCKETS_URL when it is given no -url, so a rig
// whose URL can carry a credential keeps it out of the process list, where every user of the
// machine reads a process's arguments; a process's environment is its owner's alone.
func TestFixturebucketsReadsItsServersURLFromTheEnvironment(t *testing.T) {
	_, uri := testnats.Start(t)
	t.Setenv("FIXTUREBUCKETS_URL", withPassword(t, uri))
	var out bytes.Buffer
	if err := run(nil, &out); err != nil {
		t.Fatalf("provision the server FIXTUREBUCKETS_URL names: %v", err)
	}
	if server := strings.TrimPrefix(uri, "nats://"); !strings.Contains(out.String(), "provisioned the server at "+server+":") {
		t.Fatalf("fixturebuckets printed %q, want the server FIXTUREBUCKETS_URL names provisioned as %s", out.String(), server)
	}
}
