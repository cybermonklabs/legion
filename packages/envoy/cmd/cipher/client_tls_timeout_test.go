package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/tlstimeout"
	"github.com/sjawhar/envoy/tlstimeout/tlstimeouttest"
)

// A broker whose listener takes the connection and never answers TLS ends agent-secrets' call in
// net/http's own TLS handshake timeout, after http.DefaultTransport's 10 s, and the client
// records it (brokerTLSTimeouts): one line naming agent-secrets, the broker's host and port alone,
// nothing of the call's path or query, and the handshake timeout as its event. The call's error is
// the one agent-secrets gave before the record existed, an unreachableError that a later call may
// cure: `the broker at <host> did not answer in time: GET <URL without its query>
// (AGENT_SECRETS_URL names <base>): net/http: TLS handshake timeout`.
func TestABrokerThatNeverAnswersTLSIsRecordedAndRefusedAsBefore(t *testing.T) {
	// Not parallel: it sets the package variable brokerTLSTimeouts.
	var stderr logtest.Buffer
	dir := t.TempDir()
	previous := brokerTLSTimeouts
	brokerTLSTimeouts = &tlstimeout.Recorder{
		Client: "agent-secrets", Stderr: &stderr, Bound: time.Second,
		StampDir: filepath.Join(dir, "stamps"), Curl: filepath.Join(dir, "no-curl"),
	}
	t.Cleanup(func() { brokerTLSTimeouts = previous })
	host := tlstimeouttest.Silent(t)
	base := cipheraddr.New("https://" + host)
	_, err := newClient(base).do(context.Background(), http.MethodGet, "https://"+host+"/v1/requests/req-1?wait=30", nil, nil)
	want := "the broker at " + brokerHost(base) + " did not answer in time: GET https://" + host + "/v1/requests/req-1 (" +
		brokerURLVar + " names https://" + host + "): net/http: TLS handshake timeout"
	var unreached *unreachableError
	if err == nil || err.Error() != want || !errors.As(err, &unreached) || !retryable(err) {
		t.Errorf("the call answered %v, want the retryable unreachableError %q", err, want)
	}
	lines := stderr.Lines("tls handshake timeout recorded")
	if strings.Count(lines, "\n") != 1 || !strings.HasPrefix(lines, "agent-secrets: tls handshake timeout recorded: host="+host+" event=handshake_timeout ") {
		t.Errorf("the call wrote %q, want one record line naming agent-secrets, host=%s and event=handshake_timeout", stderr.String(), host)
	}
	for _, word := range []string{"/v1", "req-1", "wait"} {
		if strings.Contains(stderr.String(), word) {
			t.Errorf("the record holds %q of the call: %q", word, stderr.String())
		}
	}
}
