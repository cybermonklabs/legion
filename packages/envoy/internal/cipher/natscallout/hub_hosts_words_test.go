package natscallout

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestAConnectErrorNamesTheHubWithNoCredentialItsURLsCarry: Start's error when the connection to
// the hub cannot be made names each server by its host alone, and an entry that does not parse,
// or whose host could be a piece of its credential, by BROKER_NATS_URLS alone, so none prints a
// run of the probe's user, password or token, which an entry carries in its user information and
// its query (calloutd hands its -nats flag to Start unchecked). The two ways a list is refused are
// both read: a nats:// entry beside a ws:// one, which nats.go refuses in words of its own, and an
// entry whose port is no number, which net/url cannot parse and the bus names by its class alone.
// In at-authority, a "?", a "/" and a "#" written into an entry's user information end its
// authority there, so the host url.Parse reads is the probe's user, password or token, and an "@"
// stands past the authority. The bus refuses such a list before it dials, naming the first such
// entry by its position alone (natsurl.Check), and the connect error names each of the three by
// BROKER_NATS_URLS and the clean entry by its host. The twin shows each list as written holds
// every marker. Captures natscallout.hub-hosts/<case>.
func TestAConnectErrorNamesTheHubWithNoCredentialItsURLsCarry(t *testing.T) {
	credentialed := func(hostport string) string {
		return "nats://" + redacttest.User + ":" + redacttest.Password + "@" + hostport + "/?access" + "_token=" + redacttest.Token
	}
	cases := []struct {
		name  string
		urls  []string
		names []string
	}{
		{"mixed", []string{credentialed("127.0.0.1:4"), "ws://127.0.0.1:5"}, []string{"127.0.0.1:4", "127.0.0.1:5", "mixing of websocket"}},
		{"unparsed", []string{credentialed("127.0.0.1:port")}, []string{"BROKER_NATS_URLS"}},
		{"at-authority", []string{
			"nats://" + redacttest.User + ":4222?access" + "_token=x@127.0.0.1:4",
			"nats://" + redacttest.Password + "/x@127.0.0.1:5",
			"nats://" + redacttest.Token + "#x@127.0.0.1:6",
			"ws://127.0.0.1:7",
		}, []string{"connect to BROKER_NATS_URLS,BROKER_NATS_URLS,BROKER_NATS_URLS,127.0.0.1:7: ", `bus: NATS server URL 1 of 4 cannot be read: an "@" stands after its host`}},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			if held := redacttest.Held(strings.Join(c.urls, ",")); !slices.Equal(held, redacttest.Secrets) {
				t.Errorf("%s: the URLs as written, %q, hold %q, want %q, so the row can see each printed", c.name, c.urls, held, redacttest.Secrets)
			}
		}
	})
	keys := testnats.NewCalloutKeys(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			settings := serviceSettings(t, "", keys)
			settings.URLs = c.urls
			service, err := Start(t.Context(), settings, Deps{})
			if err == nil {
				service.Close()
				t.Fatalf("Start with %q connected; want nats.go to refuse the list", c.urls)
			}
			text := err.Error()
			redacttest.RequireNoPiece(t, "natscallout.hub-hosts/"+c.name, text, redacttest.User, redacttest.Password, redacttest.Token)
			if !strings.HasPrefix(text, "natscallout: connect to ") {
				t.Errorf("Start's error %q does not say it is the connect to the hub", text)
			}
			for _, name := range c.names {
				if !strings.Contains(text, name) {
					t.Errorf("Start's error %q does not name %q", text, name)
				}
			}
		})
	}
}
