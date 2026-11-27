package bus

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The failed-reconnect WARN names how many servers the client was configured with and the
// failure's class, and the refusal of a remote server names that server by its position; both are
// logged, so neither carries a URL's userinfo or host.
func TestLoggedNATSURLsCarryNoUserinfo(t *testing.T) {
	t.Parallel()
	logs := captureLogs()
	urls := []string{
		"nats://user-marker-0:pw-marker-1@127.0.0.1:4222",
		"nats://tok-marker-2@127.0.0.1:4223",
		"u-marker-5:pw-marker-3@127.0.0.1:4224",
	}
	secrets := []string{"user-marker-0", "pw-marker-1", "tok-marker-2", "u-marker-5", "pw-marker-3", "127.0.0.1"}

	options(logs.logger, "log-url-test", urls, nil, nil, nil, nil, nil).ReconnectErrCB(nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED})

	// Only records that name the client count, so the WARN must carry its client's name.
	var mine []map[string]any
	for _, record := range logs.records(t) {
		if record["name"] == "log-url-test" {
			mine = append(mine, record)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("%d records logged, want 1: %s", len(mine), logs)
	}
	record := mine[0]
	if record["level"] != "WARN" || record["msg"] != "envoy nats reconnect attempt failed" {
		t.Fatalf("record = %v, want the failed reconnect WARN", record)
	}
	if record["servers"] != float64(3) || record["error"] != "connection refused" {
		t.Errorf("servers = %v and error = %v, want 3 and connection refused", record["servers"], record["error"])
	}
	for _, secret := range secrets {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the failed reconnect WARN holds %q: %s", secret, logs)
		}
	}

	unset := func(string) (string, bool) { return "", false }
	err := refuseRemoteNATS([]string{"nats://127.0.0.1:4222", " ", "nats://user-marker-6:" + "pw-marker-4@nats.example.test:4222"}, unset)
	if !errors.Is(err, ErrRemoteNATS) {
		t.Fatalf("refuseRemoteNATS = %v, want a %v", err, ErrRemoteNATS)
	}
	if !strings.Contains(err.Error(), "NATS server URL 3 of 3 names another machine") {
		t.Errorf("refusal %q does not name the server by its position", err)
	}
	for _, secret := range []string{"user-marker-6", "pw-marker-4", "nats.example.test"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("refusal %q holds %q", err, secret)
		}
	}
}

// The reconnected INFO names the server the connection reached by its position, never by the URL
// nats.go renders with its userinfo (ConnectedUrl); testnats.URL is a user:password URL, so the
// line must hold neither part, nor the host.
func TestTheReconnectedLineCarriesNoUserinfo(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	configured, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %s: %v", uri, err)
	}
	if configured.User == nil {
		t.Fatalf("testnats.URL %s carries no userinfo, so this test proves nothing", uri)
	}
	nc, err := nats.Connect(uri)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	logs := captureLogs()

	options(logs.logger, "reconnected-url-test", []string{"nats://127.0.0.1:1", uri}, nil, nil, nil, nil, nil).ReconnectedCB(nc)

	var logged string
	var record map[string]any
	for _, candidate := range logs.records(t) {
		if candidate["name"] == "reconnected-url-test" && candidate["msg"] == "envoy nats reconnected" {
			record = candidate
			logged, _ = candidate["server"].(string)
		}
	}
	if logged != "2 of 2" {
		t.Fatalf("the reconnected record names the server %q, want 2 of 2: %s", logged, logs)
	}
	password, _ := configured.User.Password()
	for _, part := range []string{configured.User.Username() + ":" + password, configured.Host} {
		if line := fmt.Sprint(record); strings.Contains(line, part) {
			t.Errorf("the reconnected line holds %q: %s", part, logs)
		}
	}
}
