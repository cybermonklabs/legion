package bus

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// printProbeParam is the query parameter a probe URL carries redacttest.Token under, in two parts
// so no line of this file reads as a credential to a scanner.
const printProbeParam = "access" + "_token"

// printProbeURL is a configured NATS server URL at host, path after it, carrying the probe's
// credentials: redacttest.User and redacttest.Password as its userinfo and redacttest.Token in its
// query. A NATS server reads no query, and one with no authorization configured (testnats.Start)
// ignores a userinfo, so a connection on such a URL connects.
func printProbeURL(host, path string) string {
	return "nats://" + redacttest.User + ":" + redacttest.Password + "@" + host + path + "?" + printProbeParam + "=" + redacttest.Token
}

// printLookup answers as os.LookupEnv would in an environment holding values alone.
func printLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, set := values[key]
		return value, set
	}
}

// printRecord is the one line logs holds from the connection named name with msg, as it was
// written, and that line decoded.
func printRecord(t *testing.T, logs *capturedLogs, name, msg string) (string, map[string]any) {
	t.Helper()
	var line string
	var record map[string]any
	for _, written := range strings.Split(logs.String(), "\n") {
		var decoded map[string]any
		if json.Unmarshal([]byte(written), &decoded) != nil || decoded["name"] != name || decoded["msg"] != msg {
			continue
		}
		if line != "" {
			t.Fatalf("more than one %q line from %s: %s", msg, name, logs)
		}
		line, record = written, decoded
	}
	if line == "" {
		t.Fatalf("no %q line from %s: %s", msg, name, logs)
	}
	return line, record
}

// Every line and logged error of the bus that names a configured NATS server names it by its
// scheme and host alone (natsServerName): the connected and reconnected lines name the server
// nats.go reached (ConnectedUrl, the configured URL's String with its userinfo and query), the
// refusal of the credential and the failed reconnect name the configured servers (serverList), and
// the refusal of another machine's server names that server. A URL the refusal cannot read is
// named as redact.NotPrinted.
func TestAConfiguredNATSServerPrintsByItsSchemeAndHostAlone(t *testing.T) {
	_, uri := testnats.Start(t)
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse the test server's URL %q: %v", uri, err)
	}
	configured := printProbeURL(server.Host, "")
	nc, err := nats.Connect(configured)
	if err != nil {
		t.Fatalf("connect to the test server on the probe URL: %v", err)
	}
	t.Cleanup(nc.Close)
	named := "nats://" + server.Host
	const remoteHost = "nats.example.test:4222"
	remote := printProbeURL(remoteHost, "")
	unparsed := printProbeURL(remoteHost, "/%zz")

	t.Run("twin", func(t *testing.T) {
		for what, written := range map[string]string{
			"the configured URL":                   configured,
			"the URL nats.go reports it connected": nc.ConnectedUrl(),
			"another machine's URL":                remote,
			"the URL that does not parse":          unparsed,
		} {
			if held := redacttest.Held(written); len(held) != len(redacttest.Secrets) {
				t.Errorf("%s %q holds %q, want every one of %q", what, written, held, redacttest.Secrets)
			}
		}
		if _, err := url.Parse(unparsed); err == nil {
			t.Errorf("%q parses, so the reach-unparsed row reads a URL that does", unparsed)
		}
	})

	t.Run("connected", func(t *testing.T) {
		logs := captureLogs(t)
		var background nats.Options
		if err := connectInBackground("print-connected", nil, newReconnectErrThrottle(nil), nil)(&background); err != nil {
			t.Fatalf("apply connectInBackground: %v", err)
		}
		background.ConnectedCB(nc)
		line, record := printRecord(t, logs, "print-connected", "envoy nats connected")
		redacttest.RequireNoPiece(t, "bus.connected-line", line, redacttest.User, redacttest.Password, redacttest.Token)
		if record["url"] != named {
			t.Errorf("the connected line's url = %v, want %q: %s", record["url"], named, line)
		}
	})

	t.Run("reconnected", func(t *testing.T) {
		logs := captureLogs(t)
		options("print-reconnected", nil, nil, nil, nil, nil, nil).ReconnectedCB(nc)
		line, record := printRecord(t, logs, "print-reconnected", "envoy nats reconnected")
		redacttest.RequireNoPiece(t, "bus.reconnected-line", line, redacttest.User, redacttest.Password, redacttest.Token)
		if record["url"] != named {
			t.Errorf("the reconnected line's url = %v, want %q: %s", record["url"], named, line)
		}
	})

	t.Run("refusal", func(t *testing.T) {
		logs := captureLogs(t)
		var background nats.Options
		if err := connectInBackground("print-refusal", []string{configured}, newReconnectErrThrottle(nil), nil)(&background); err != nil {
			t.Fatalf("apply connectInBackground: %v", err)
		}
		background.AsyncErrorCB(nc, nil, nats.ErrAuthorization)
		line, record := printRecord(t, logs, "print-refusal", "envoy nats authorization refused")
		redacttest.RequireNoPiece(t, "bus.refusal-line", line, redacttest.User, redacttest.Password, redacttest.Token)
		if record["servers"] != named {
			t.Errorf("the refusal line's servers = %v, want %q: %s", record["servers"], named, line)
		}
	})

	t.Run("reconnect-failed", func(t *testing.T) {
		logs := captureLogs(t)
		options("print-reconnect-failed", []string{configured}, nil, nil, nil, nil, nil).ReconnectErrCB(nil, errors.New("dial refused"))
		line, record := printRecord(t, logs, "print-reconnect-failed", "envoy nats reconnect attempt failed")
		redacttest.RequireNoPiece(t, "bus.reconnect-failed-line", line, redacttest.User, redacttest.Password, redacttest.Token)
		if record["servers"] != named {
			t.Errorf("the failed reconnect line's servers = %v, want %q: %s", record["servers"], named, line)
		}
	})

	t.Run("reach", func(t *testing.T) {
		_, err := Dial("print-reach", []string{remote}, printLookup(nil))
		if !errors.Is(err, ErrRemoteNATS) {
			t.Fatalf("dial %s = %v, want a %v", remoteHost, err, ErrRemoteNATS)
		}
		text := err.Error()
		redacttest.RequireNoPiece(t, "bus.reach-refusal", text, redacttest.User, redacttest.Password, redacttest.Token)
		if want := "nats://" + remoteHost; !strings.Contains(text, want) || !strings.Contains(text, AllowRemoteEnvVar) {
			t.Errorf("refusal %q does not name both the server %s and the override %s", text, want, AllowRemoteEnvVar)
		}
	})

	t.Run("reach-unparsed", func(t *testing.T) {
		_, err := Dial("print-reach-unparsed", []string{unparsed}, printLookup(nil))
		if !errors.Is(err, ErrRemoteNATS) {
			t.Fatalf("dial a URL that does not parse = %v, want a %v", err, ErrRemoteNATS)
		}
		text := err.Error()
		redacttest.RequireNoPiece(t, "bus.reach-refusal/unparsed", text, redacttest.User, redacttest.Password, redacttest.Token)
		if !strings.Contains(text, redact.NotPrinted) || !strings.Contains(text, AllowRemoteEnvVar) {
			t.Errorf("refusal %q does not name both the server as %q and the override %s", text, redact.NotPrinted, AllowRemoteEnvVar)
		}
	})
}

// A NATS server URL nats.go cannot read fails its connect with url.Parse's *url.Error, which quotes
// the URL whole: the connect returns it by its class alone, through a SOCKS5 proxy (whose failure
// names the setting and the proxy) and without one (the error the listener's start logs and exits
// on). Each attempt fails the same way, so each row waits out the connect's ten attempts.
func TestANATSServerURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	t.Parallel()
	unparsed := printProbeURL("127.0.0.1:4222", "/%zz")

	t.Run("twin", func(t *testing.T) {
		t.Parallel()
		_, err := url.Parse(unparsed)
		if err == nil {
			t.Fatalf("%q parses, so no row here reaches nats.go's parse failure", unparsed)
		}
		if held := redacttest.Held(err.Error()); len(held) != len(redacttest.Secrets) {
			t.Errorf("url.Parse's error %q holds %q, want every one of %q", err, held, redacttest.Secrets)
		}
	})

	const proxy = "127.0.0.1:1080"
	for _, row := range []struct {
		name, capture string
		environment   map[string]string
		names         []string
	}{
		{"proxy", "bus.connect-unparsed/proxy", map[string]string{AllowRemoteEnvVar: "1", SOCKS5ProxyEnvVar: proxy}, []string{SOCKS5ProxyEnvVar + "=" + proxy, "NATS server URL", redact.ClassNotAURL}},
		{"direct", "bus.connect-unparsed/direct", map[string]string{AllowRemoteEnvVar: "1"}, []string{"NATS server URL", redact.ClassNotAURL}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			conn, err := Dial("print-unparsed-"+row.name, []string{unparsed}, printLookup(row.environment))
			if err == nil {
				conn.Close()
				t.Fatalf("dial a URL that does not parse succeeded")
			}
			text := err.Error()
			redacttest.RequireNoPiece(t, row.capture, text, redacttest.User, redacttest.Password, redacttest.Token)
			for _, name := range row.names {
				if !strings.Contains(text, name) {
					t.Errorf("the connect's failure %q does not name %q", text, name)
				}
			}
			var parseErr *url.Error
			if !errors.As(err, &parseErr) {
				t.Errorf("the connect's failure %q no longer holds url.Parse's error, so errors.As cannot classify it", text)
			}
		})
	}
}
