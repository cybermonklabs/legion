package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnHTTPEdgeURLTheBrokerRefusesPrintsNoneOfIt: outside a local run, a plain-http
// BROKER_EDGE_URL (edge-url) or BROKER_EDGE_MACHINE_LOGIN_URL (machine-login) with redacttest's
// token in its path is refused naming the setting, and the refusal holds no run of the token. The
// refusal quoted the URL as the rule reads it (redact.URL), which prints a plain path as written.
// The twin shows each URL is still refused naming its setting, and that the rule's reading of it
// holds the token, so the row can see one printed. Captures cipherd.edge-https/<case>.
func TestAnHTTPEdgeURLTheBrokerRefusesPrintsNoneOfIt(t *testing.T) {
	const edgeURL, machineLoginURL = "https://edge.invalid", "https://dispatch.edge.invalid/credentials/machine"
	cases := []struct{ name, setting, edgeURL, machineLoginURL, plain string }{
		{"edge-url", "BROKER_EDGE_URL", "http://edge.invalid/" + redacttest.Token, machineLoginURL, "http://edge.invalid/" + redacttest.Token},
		{"machine-login", "BROKER_EDGE_MACHINE_LOGIN_URL", edgeURL, "http://dispatch.edge.invalid/" + redacttest.Token, "http://dispatch.edge.invalid/" + redacttest.Token},
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if shown := redact.URL(tc.plain); !strings.Contains(shown, redacttest.Token) {
				t.Errorf("%s: redact.URL(%q) = %q; want the token in it, so the row can see one printed", tc.name, tc.plain, shown)
			}
			if err := refuseHTTPEdgeURLsInProduction(tc.edgeURL, tc.machineLoginURL, ""); err == nil || !strings.HasPrefix(err.Error(), tc.setting+" ") {
				t.Errorf("%s: refuseHTTPEdgeURLsInProduction = %v; want a refusal naming %s", tc.name, err, tc.setting)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseHTTPEdgeURLsInProduction(tc.edgeURL, tc.machineLoginURL, "")
			if err == nil {
				t.Fatalf("refuseHTTPEdgeURLsInProduction took %s=%q; want it refused", tc.setting, tc.plain)
			}
			if !strings.HasPrefix(err.Error(), tc.setting+" ") {
				t.Errorf("refuseHTTPEdgeURLsInProduction = %v; want a refusal naming %s", err, tc.setting)
			}
			redacttest.RequireNoPiece(t, "cipherd.edge-https/"+tc.name, err.Error(), redacttest.Token)
		})
	}
}
