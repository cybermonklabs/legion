package mcpbridge

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// busFor is a bus client of a NATS server of t's own, which a bridge publishes through.
func busFor(t *testing.T) *bus.Client {
	t.Helper()
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect to NATS: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// bridgeTo starts a bridge, publishing through client, whose one server is the MCP server at url,
// and answers what the log package writes from then until t ends. The bridge stops when t ends.
func bridgeTo(t *testing.T, client *bus.Client, url string) *logtest.Buffer {
	t.Helper()
	cfg := &Config{Servers: []ServerConfig{httpServerConfig(url)}}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate the bridge's config: %v", err)
	}
	logged := capturedLog(t)
	bridge := NewBridge(cfg, client)
	if err := bridge.Start(); err != nil {
		t.Fatalf("start the bridge: %v", err)
	}
	t.Cleanup(bridge.Stop)
	return logged
}

// notified is the bridge's line for a notification the MCP server mock sends that the resource
// uri changed, once the bridge has logged a line holding phrase: everything the bridge logged by
// then.
func notified(t *testing.T, mock *mockHTTPMCPServer, logged *logtest.Buffer, uri, phrase string) string {
	t.Helper()
	mock.notify(uri)
	testwait.Eventually(t, "the bridge's line "+phrase, func() bool { return strings.Contains(logged.String(), phrase) })
	return logged.String()
}

// TestANotificationsURIAndTopicPrintAsTheRuleReadsThem: an MCP server notifies the bridge of a
// resource whose URI quotes the probe's URL. The bridge's line for a URI its pattern does not match
// (unmatched), for a read the server refuses with words that quote the probe (read-failed), for an
// event it published on a topic whose capture is the probe's URL (published), and for a topic NATS
// refuses, since the capture holds spaces (publish-refused), each names the URI, the topic and the
// failure as the rule reads them, and none of the credentials. Captures
// mcpbridge.notification/<case>.
func TestANotificationsURIAndTopicPrintAsTheRuleReadsThem(t *testing.T) {
	client := busFor(t)
	const message = "whatsapp://messages/15551234567/"
	for _, row := range []struct{ name, uri, refusal, phrase, want string }{
		{"unmatched", redacttest.Words, "", "URI does not match pattern", "URI does not match pattern: " + redacttest.Shown},
		{"read-failed", message + redacttest.Code, redacttest.Words, "resources/read failed",
			"resources/read failed for " + redact.WordsNotReadable + ": rpc error -32002: " + redacttest.Shown},
		{"published", message + redacttest.Code, "", "published to", "published to notifications.whatsapp.15551234567." + redacttest.CodeShown},
		{"publish-refused", message + redacttest.Words, "", "publish failed", "publish failed: " + redact.WordsNotReadable},
	} {
		t.Run(row.name, func(t *testing.T) {
			mock := newMockHTTPMCPServer(func(m *mockHTTPMCPServer) { m.notifyAfterSubscribe, m.readRefusal = false, row.refusal })
			t.Cleanup(mock.close)
			text := notified(t, mock, bridgeTo(t, client, mock.url()), row.uri, row.phrase)
			redacttest.RequireNone(t, "mcpbridge.notification/"+row.name, text)
			if want := "mcp-bridge: whatsapp: " + row.want + "\n"; !strings.Contains(text, want) {
				t.Errorf("the bridge logged %q; want it to hold %q", text, want)
			}
		})
	}
}

// TestAnHTTPServersWordsPrintWithoutTheCredentialsItsStreamCarried: the configured URL of an MCP
// server over HTTP carries the probe's user and password, which net/http sends as the stream's
// Basic Authorization, and the server echoes the password bare, where no URL holds it: in its
// refusal of the handshake (rpc), in a notification's URI its pattern does not match (uri), and as
// the capture of a topic the bridge publishes on (topic). The server's words are held with the
// record of the stream's request (redact.StreamedWords), so each print of them holds none of what
// that request carried. Captures mcpbridge.echo/<case>.
func TestAnHTTPServersWordsPrintWithoutTheCredentialsItsStreamCarried(t *testing.T) {
	credentialed := func(m *mockHTTPMCPServer) string {
		return strings.Replace(m.url(), "://", "://"+redacttest.User+":"+redacttest.Password+"@", 1)
	}
	const echo = "the key " + redacttest.Password + " is not the one"
	const shown = "the key " + redact.Redacted + " is not the one"
	t.Run("rpc", func(t *testing.T) {
		mock := newMockHTTPMCPServer(func(m *mockHTTPMCPServer) { m.failInitialize, m.refusal = true, echo })
		t.Cleanup(mock.close)
		err := NewHTTPServer(httpServerConfig(credentialed(mock)), nil).Start()
		if err == nil {
			t.Fatal("Start succeeded against a server that refuses the handshake")
		}
		redacttest.RequireNone(t, "mcpbridge.echo/rpc", err.Error())
		if want := "initialize whatsapp: rpc error -32600: " + shown; err.Error() != want {
			t.Errorf("Start = %q; want %q", err, want)
		}
	})
	client := busFor(t)
	for _, row := range []struct{ name, uri, phrase, want string }{
		{"uri", echo, "URI does not match pattern", "URI does not match pattern: " + shown},
		{"topic", "whatsapp://messages/15551234567/" + redacttest.Password, "published to",
			"published to notifications.whatsapp.15551234567." + redact.Redacted + ".message"},
	} {
		t.Run(row.name, func(t *testing.T) {
			mock := newMockHTTPMCPServer(func(m *mockHTTPMCPServer) { m.notifyAfterSubscribe = false })
			t.Cleanup(mock.close)
			text := notified(t, mock, bridgeTo(t, client, credentialed(mock)), row.uri, row.phrase)
			redacttest.RequireNone(t, "mcpbridge.echo/"+row.name, text)
			if want := "mcp-bridge: whatsapp: " + row.want + "\n"; !strings.Contains(text, want) {
				t.Errorf("the bridge logged %q; want it to hold %q", text, want)
			}
		})
	}
}
