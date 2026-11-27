package bus

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// printProbeParam is the query parameter a probe URL carries redacttest.Token under, in two parts
// so no line of this file reads as a credential to a scanner.
const printProbeParam = "access" + "_token"

// printProbeURL is a configured NATS server URL at host carrying the probe's credentials:
// redacttest.User and redacttest.Password as its userinfo and redacttest.Token in its query. A NATS
// server reads no query, and one with no authorization configured (testnats.Start) ignores a
// userinfo, so a connection on such a URL connects.
func printProbeURL(host string) string {
	return "nats://" + redacttest.User + ":" + redacttest.Password + "@" + host + "?" + printProbeParam + "=" + redacttest.Token
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
// position alone (natsconn.ServerPosition), and the configured servers by their count: the
// connected and reconnected lines name the server nats.go reached (ConnectedUrl, the configured
// URL's String with its userinfo and query) by its position, the refusal of the credential and the
// failed reconnect name the configured servers by their count, and the refusal of another
// machine's server names that server by its position. A URL that carries the probe's userinfo and
// a token in its query prints no piece of either on any of them.
func TestAConfiguredNATSServerPrintsByItsPositionAlone(t *testing.T) {
	_, uri := testnats.Start(t)
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse the test server's URL %q: %v", uri, err)
	}
	configured := printProbeURL(server.Host)
	nc, err := nats.Connect(configured)
	if err != nil {
		t.Fatalf("connect to the test server on the probe URL: %v", err)
	}
	t.Cleanup(nc.Close)
	const remoteHost = "nats.example.test:4222"
	remote := printProbeURL(remoteHost)

	t.Run("twin", func(t *testing.T) {
		for what, written := range map[string]string{
			"the configured URL":                   configured,
			"the URL nats.go reports it connected": nc.ConnectedUrl(),
			"another machine's URL":                remote,
		} {
			if held := redacttest.Held(written); len(held) != len(redacttest.Secrets) {
				t.Errorf("%s %q holds %q, want every one of %q", what, written, held, redacttest.Secrets)
			}
		}
	})

	t.Run("connected", func(t *testing.T) {
		logs := captureLogs()
		var background nats.Options
		if err := connectInBackground(logs.logger, "print-connected", []string{configured}, newReconnectErrThrottle(nil), nil)(&background); err != nil {
			t.Fatalf("apply connectInBackground: %v", err)
		}
		background.ConnectedCB(nc)
		line, record := printRecord(t, logs, "print-connected", "envoy nats connected")
		redacttest.RequireNoPiece(t, "bus.connected-line", line, redacttest.User, redacttest.Password, redacttest.Token)
		if record["server"] != "1 of 1" {
			t.Errorf("the connected line's server = %v, want %q: %s", record["server"], "1 of 1", line)
		}
	})

	t.Run("reconnected", func(t *testing.T) {
		logs := captureLogs()
		options(logs.logger, "print-reconnected", []string{configured}, nil, nil, nil, nil, nil).ReconnectedCB(nc)
		line, record := printRecord(t, logs, "print-reconnected", "envoy nats reconnected")
		redacttest.RequireNoPiece(t, "bus.reconnected-line", line, redacttest.User, redacttest.Password, redacttest.Token)
		if record["server"] != "1 of 1" {
			t.Errorf("the reconnected line's server = %v, want %q: %s", record["server"], "1 of 1", line)
		}
	})

	t.Run("refusal", func(t *testing.T) {
		logs := captureLogs()
		var background nats.Options
		if err := connectInBackground(logs.logger, "print-refusal", []string{configured}, newReconnectErrThrottle(nil), nil)(&background); err != nil {
			t.Fatalf("apply connectInBackground: %v", err)
		}
		background.AsyncErrorCB(nc, nil, nats.ErrAuthorization)
		line, record := printRecord(t, logs, "print-refusal", "envoy nats authorization refused")
		redacttest.RequireNoPiece(t, "bus.refusal-line", line, redacttest.User, redacttest.Password, redacttest.Token)
		if record["servers"] != float64(1) {
			t.Errorf("the refusal line's servers = %v, want 1: %s", record["servers"], line)
		}
	})

	t.Run("reconnect-failed", func(t *testing.T) {
		logs := captureLogs()
		options(logs.logger, "print-reconnect-failed", []string{configured}, nil, nil, nil, nil, nil).ReconnectErrCB(nil, errors.New("dial refused"))
		line, record := printRecord(t, logs, "print-reconnect-failed", "envoy nats reconnect attempt failed")
		redacttest.RequireNoPiece(t, "bus.reconnect-failed-line", line, redacttest.User, redacttest.Password, redacttest.Token)
		if record["servers"] != float64(1) {
			t.Errorf("the failed reconnect line's servers = %v, want 1: %s", record["servers"], line)
		}
	})

	t.Run("reach", func(t *testing.T) {
		_, err := Dial("print-reach", []string{remote}, printLookup(nil))
		if !errors.Is(err, ErrRemoteNATS) {
			t.Fatalf("dial %s = %v, want a %v", remoteHost, err, ErrRemoteNATS)
		}
		text := err.Error()
		redacttest.RequireNoPiece(t, "bus.reach-refusal", text, redacttest.User, redacttest.Password, redacttest.Token)
		if want := "NATS server URL 1 of 1 names another machine"; !strings.Contains(text, want) || !strings.Contains(text, AllowRemoteEnvVar) || strings.Contains(text, remoteHost) {
			t.Errorf("refusal %q does not name both the server's position (%q) and the override %s, or names the server %s", text, want, AllowRemoteEnvVar, remoteHost)
		}
	})
}
