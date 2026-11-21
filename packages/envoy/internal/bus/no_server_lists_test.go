package bus_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
)

// Every constructor refuses, before any dial, a server list that names no server or holds a blank
// entry: nats.go reads either as its default server, port 4222 on this machine. Each row connects
// in the background, so with the refusal gone it returns the client or connection it built and
// the row fails naming it, rather than waiting out a dial. Each row also holds the refusal to its
// own words, so a row the host check refuses in place of the blank check fails: a blank entry's
// URL prints as nothing, so only the blank check's words tell an operator which entry was blank.
//
// Controls, each on its own: drop refuseNoServer from newClient (the Connect and
// ConnectOwningStream rows fail), from Dial (the Dial rows fail), or its blank-entry check (the
// blank rows fail, refused by the host check in other words).
func TestEveryConnectRefusesAListNamingNoServer(t *testing.T) {
	noOptIn := func(string) (string, bool) { return "", false }
	requireEveryConnectRefuses(t, noOptIn, "nats.go would dial its default, nats://127.0.0.1:4222, in its place", map[string][]string{
		"nil":   nil,
		"empty": {},
	})
	requireEveryConnectRefuses(t, noOptIn, "is blank, which nats.go reads as port 4222 on this machine", map[string][]string{
		"blank":          {""},
		"spaces":         {"  "},
		"a blank second": {"nats://127.0.0.1:1", " "},
	})
}

// Every constructor refuses, before any dial, an entry that names no host, even with
// ENVOY_ALLOW_REMOTE_NATS=1: nats.go dials such an entry on this machine (nats:// as port 4222),
// and the opt-in, which admits another machine's server, names none here.
//
// Control: drop refuseNoServer's host check; with the opt-in, each row builds its client or
// connection, and fails naming it.
func TestEveryConnectRefusesAServerNamingNoHost(t *testing.T) {
	requireEveryConnectRefuses(t, allowRemote, "names no host, which nats.go dials on this machine", map[string][]string{
		"nats://":                 {"nats://"},
		"nats://:4222":            {"nats://:4222"},
		"a user and no host":      {"nats://example-user@:4222"},
		"a host-less second":      {"nats://127.0.0.1:1", "nats://:4222"},
		"no scheme, a port alone": {":4222"},
	})
}

// Every constructor refuses, before any dial, an entry that names the unspecified address, even
// with ENVOY_ALLOW_REMOTE_NATS=1: a connect to 0.0.0.0 or :: reaches this machine, since its
// network stack takes either for its own, so such an entry names no server the opt-in could
// allow. The first two rows are the tester's probe, which showed a client built for each reaching
// a server on this machine's loopback.
//
// Control: drop refuseNoServer's unspecified-address check; with the opt-in, each row builds its
// client or connection, which dials this machine, and fails naming it.
func TestEveryConnectRefusesTheUnspecifiedAddress(t *testing.T) {
	requireEveryConnectRefuses(t, allowRemote, "names the unspecified address, which a connect reaches on this machine", map[string][]string{
		"0.0.0.0":                 {"nats://0.0.0.0:4222"},
		"[::]":                    {"nats://[::]:4222"},
		"IPv4-mapped 0.0.0.0":     {"nats://[::ffff:0.0.0.0]:4222"},
		"[::] with a zone":        {"nats://[::%25lo]:4222"},
		"no scheme, 0.0.0.0":      {"0.0.0.0:4222"},
		"an unspecified second":   {"nats://127.0.0.1:1", "nats://0.0.0.0:4222"},
		"a user and 0.0.0.0 only": {"nats://example-user@0.0.0.0"},
	})
}

// Every constructor refuses, before any dial and without a lookup, an entry whose host is a
// numeric shorthand inet_aton reads as an IPv4 address but netip, which reads only the canonical
// form, does not, even with ENVOY_ALLOW_REMOTE_NATS=1: a system resolver (getaddrinfo, which Go's
// cgo resolver calls) reads 0 and 0x0 as 0.0.0.0, which reaches this machine, and 127.1 and
// 0177.0.0.1 as 127.0.0.1, where Go's own resolver would look the name up. The first two rows are
// the tester's probe, which showed a client built for each reaching a server on this machine's
// loopback under the cgo resolver.
//
// Control: drop refuseNoServer's numeric-shorthand check; with the opt-in, each row builds its
// client or connection, which under the cgo resolver (GODEBUG=netdns=cgo) dials this machine, and
// fails naming it.
func TestEveryConnectRefusesANumericShorthandHost(t *testing.T) {
	requireEveryConnectRefuses(t, allowRemote, "names a numeric host that is no address in canonical form", map[string][]string{
		"0":                    {"nats://0:4222"},
		"0x0":                  {"nats://0x0:4222"},
		"0X0":                  {"nats://0X0:4222"},
		"0, a trailing dot":    {"nats://0.:4222"},
		"0x0, a trailing dot":  {"nats://0x0.:4222"},
		"0.0":                  {"nats://0.0:4222"},
		"127.1":                {"nats://127.1:4222"},
		"0177.0.0.1, octal":    {"nats://0177.0.0.1:4222"},
		"0x7f.1, hexadecimal":  {"nats://0x7f.1:4222"},
		"no scheme, 0":         {"0:4222"},
		"a numeric second":     {"nats://127.0.0.1:1", "nats://0:4222"},
		"a user and 0x0 alone": {"nats://example-user@0x0"},
	})
}

// Every constructor still builds a client for a server an entry names in full, with or without a
// root dot: the refusals above leave a canonical address and a host name alone. Each row's server
// is a closed port on this machine's loopback, so the background dial is refused at once.
//
// Control: make refuseNoServer refuse every entry; each row fails naming the refusal.
func TestEveryConnectBuildsAClientForAServerItNames(t *testing.T) {
	connects := everyConnect(allowRemote)
	for connectName, connect := range connects {
		for listName, urls := range map[string][]string{
			"127.0.0.1":             {"nats://127.0.0.1:1"},
			"[::1]":                 {"nats://[::1]:1"},
			"localhost":             {"nats://localhost:1"},
			"127.0.0.1, a root dot": {"nats://127.0.0.1.:1"},
		} {
			t.Run(connectName+"/"+listName, func(t *testing.T) {
				closeIt, err := connect(urls)
				if err != nil {
					t.Fatalf("%s refused %q, a server it names in full: %v", connectName, urls, err)
				}
				closeIt()
			})
		}
	}
}

// allowRemote is a connect's environment with ENVOY_ALLOW_REMOTE_NATS=1 and nothing else set.
func allowRemote(name string) (string, bool) {
	if name == bus.AllowRemoteEnvVar {
		return "1", true
	}
	return "", false
}

// requireEveryConnectRefuses connects with Connect, ConnectOwningStream and Dial, each in the
// background and with environment, to each list of lists, and fails a row whose connect does not
// refuse it with bus.ErrNoNATSServer in words holding says.
func requireEveryConnectRefuses(t *testing.T, environment func(string) (string, bool), says string, lists map[string][]string) {
	t.Helper()
	for connectName, connect := range everyConnect(environment) {
		for listName, urls := range lists {
			t.Run(connectName+"/"+listName, func(t *testing.T) {
				closeIt, err := connect(urls)
				if err == nil {
					closeIt()
					t.Fatalf("%s built a connection for %q, which nats.go dials on this machine", connectName, urls)
				}
				if !errors.Is(err, bus.ErrNoNATSServer) {
					t.Fatalf("%s refused %q with %v, not bus.ErrNoNATSServer", connectName, urls, err)
				}
				if !strings.Contains(err.Error(), says) {
					t.Fatalf("%s refused %q as %q, which does not say %q", connectName, urls, err, says)
				}
			})
		}
	}
}

// everyConnect is Connect, ConnectOwningStream and Dial, each connecting in the background with
// environment and answering how to close what it built.
func everyConnect(environment func(string) (string, bool)) map[string]func(urls []string) (func(), error) {
	return map[string]func(urls []string) (func(), error){
		"Connect": func(urls []string) (func(), error) {
			client, err := bus.Connect(urls, bus.WithEnvironment(environment), bus.WithoutStreamSubjects(), bus.WithConnectInBackground())
			if err != nil {
				return nil, err
			}
			return client.Close, nil
		},
		"ConnectOwningStream": func(urls []string) (func(), error) {
			client, err := bus.ConnectOwningStream(urls, bus.WithEnvironment(environment), bus.WithConnectInBackground())
			if err != nil {
				return nil, err
			}
			return client.Close, nil
		},
		"Dial": func(urls []string) (func(), error) {
			conn, err := bus.Dial("no-server-test", urls, environment, bus.WithConnectInBackground())
			if err != nil {
				return nil, err
			}
			return conn.Close, nil
		},
	}
}
