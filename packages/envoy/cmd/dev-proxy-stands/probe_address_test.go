package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABoundProbeAddressThatDoesNotParsePrintsItsClass: AIRLOCK_S_URL is whatever the environment
// holds, and a value with user information whose password holds an escape net/url cannot read does
// not parse. net/url's error quotes that value whole, user name and password (the twin);
// probe-bound prints the failure by its class alone. Captures
// dev-proxy-stands.probe-address/unparsed.
func TestABoundProbeAddressThatDoesNotParsePrintsItsClass(t *testing.T) {
	address := "https://" + redacttest.User + ":" + redacttest.Password + "%zz@bound.example"
	t.Run("twin", func(t *testing.T) {
		_, err := http.NewRequest(http.MethodGet, address+"/go", nil)
		if err == nil || !strings.Contains(err.Error(), redacttest.Password) {
			t.Fatalf("net/url's own error holds no password, so the row cannot see one: %v", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		t.Setenv("AIRLOCK_S_URL", address)
		t.Setenv("AIRLOCK_DEMO_TOKEN", "unused")
		var stdout, stderr strings.Builder
		if code := run(context.Background(), []string{"probe-bound"}, &stdout, &stderr); code != 0 {
			t.Fatalf("dev-airlock-stands probe-bound exited %d; stderr: %s", code, stderr.String())
		}
		redacttest.RequireNone(t, "dev-proxy-stands.probe-address/unparsed", stdout.String())
		if want := "error " + redact.ClassNotAURL + "\n"; stdout.String() != want {
			t.Errorf("probe-bound printed %q, want %q", stdout.String(), want)
		}
	})
}
