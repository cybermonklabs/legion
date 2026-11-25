package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestTheReadyRefusalNamesTheServerWithNoCredentialItsURLCarries: -nats is taken unchecked, and a
// server that never answers ends run, once readyTimeout has passed, with an error that names the
// server by its host alone, so it prints no run of the probe's user, password or token, which the
// URL carries in its user information and its query. The twin shows the -nats value as written
// holds every marker. Captures calloutd.ready.
func TestTheReadyRefusalNamesTheServerWithNoCredentialItsURLCarries(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	dir := t.TempDir()
	for name, seed := range map[string]string{"service.seed": keys.ServiceSeed, "issuer.seed": keys.IssuerSeed, "xkey.seed": keys.XKeySeed} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(seed+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A port this test held and let go, so nothing answers there.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	server := "nats://" + redacttest.User + ":" + redacttest.Password + "@" + addr + "/?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		for _, marker := range redacttest.Secrets {
			if !strings.Contains(server, marker) {
				t.Errorf("the -nats value as written, %q, holds no %q, so the row cannot see it printed", server, marker)
			}
		}
	})
	t.Run("ready", func(t *testing.T) {
		err := run([]string{"-keys", dir, "-nats", server, "-introspect", "http://127.0.0.1:1/introspect"})
		if err == nil {
			t.Fatal("run returned no error with nothing answering at -nats; want the service never ready")
		}
		text := err.Error()
		redacttest.RequireNoPiece(t, "calloutd.ready", text, redacttest.User, redacttest.Password, redacttest.Token)
		if !strings.HasPrefix(text, "the service did not reach ") || !strings.Contains(text, addr) {
			t.Errorf("run's error %q does not name the server %s it could not reach", text, addr)
		}
	})
}
