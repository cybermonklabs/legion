package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEndpointIsNamedWithNoneOfItsQuery: an endpoint URL carries a credential in a query value
// under a name other than password (access_token). Every place that records or prints an endpoint
// names it as the URL rule names a URL a request asked (redactURL: scheme, host and path, no
// userinfo, no query), where it kept every query value but password's: meta.json's flags
// (recorded), each -nats server (nats), the NATS monitor's refusal (nats-mon-line), Dispatch's
// refusal (dispatch-line) and the dashboard's socket line (socket). The endpoint as given holds
// the credential (twin). The control: each still names its host and path. Captures
// loadtest.endpoint-query/<case>.
func TestAnEndpointIsNamedWithNoneOfItsQuery(t *testing.T) {
	query := "?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		if !strings.Contains("http://127.0.0.1:1/prefix"+query, redacttest.Token) {
			t.Fatal("the endpoint as given holds no credential, so the row cannot see one printed")
		}
	})
	t.Run("recorded", func(t *testing.T) {
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		var ef endpointFlags
		ef.register(fs)
		var args []string
		for _, f := range []string{"-url", "-listener", "-nats-mon", "-pprof"} {
			args = append(args, f, "http://127.0.0.1:1/prefix"+query)
		}
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		recorded := strings.Join(recordedArgs(fs), " ")
		redacttest.RequireNoPiece(t, "loadtest.endpoint-query/recorded", recorded)
		if want := "-url=http://127.0.0.1:1/prefix"; !strings.Contains(recorded, want) {
			t.Errorf("recorded %q; want %q in it", recorded, want)
		}
	})
	t.Run("nats", func(t *testing.T) {
		said := redactEndpoint("-nats", "nats://127.0.0.1:4222"+query+",127.0.0.1:4223"+query)
		redacttest.RequireNoPiece(t, "loadtest.endpoint-query/nats", said)
		if want := "nats://127.0.0.1:4222,127.0.0.1:4223"; said != want {
			t.Errorf("redactEndpoint(-nats) = %q; want %q", said, want)
		}
	})
	refusing := func(t *testing.T) string {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	t.Run("nats-mon-line", func(t *testing.T) {
		mon := refusing(t)
		err := checkNATSServerName(context.Background(), http.DefaultClient, mon+"/prefix"+query, "loadtest-nats")
		said := errorText(err)
		redacttest.RequireNoPiece(t, "loadtest.endpoint-query/nats-mon-line", said)
		if want := "GET " + mon + "/prefix/varz answered 403"; !strings.Contains(said, want) {
			t.Errorf("checkNATSServerName = %q; want %q in it", said, want)
		}
	})
	t.Run("dispatch-line", func(t *testing.T) {
		dispatch := refusing(t)
		err := checkStack(context.Background(), endpoints{dispatch: dispatch + "/prefix" + query, token: "agent-token"})
		said := errorText(err)
		redacttest.RequireNoPiece(t, "loadtest.endpoint-query/dispatch-line", said)
		if want := "GET " + dispatch + "/prefix/api/v1/projects with the stack's agent token answered 403"; !strings.Contains(said, want) {
			t.Errorf("checkStack = %q; want %q in it", said, want)
		}
	})
	t.Run("socket", func(t *testing.T) {
		said := redactURL("ws://127.0.0.1:1/prefix/ws/live" + query)
		redacttest.RequireNoPiece(t, "loadtest.endpoint-query/socket", said)
		if want := "ws://127.0.0.1:1/prefix/ws/live"; said != want {
			t.Errorf("redactURL(ws) = %q; want %q", said, want)
		}
	})
}
