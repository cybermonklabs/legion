package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestTheRenderedIntrospectionLimitIsTheBrokers: a private hub's session listener with tailnet
// renders its per-address introspection limit (ENVOY_AGENT_SECRETS_INTROSPECTIONS_PER_SECOND and
// ENVOY_AGENT_SECRETS_INTROSPECTION_BURST) as this broker's own per-address bound on
// POST /v1/oauth/introspect, DefaultAccountLimit: every tailnet session reaches the listener from a
// tailnet host's address, and the listener pod is one address to the broker, so a host's bucket of
// the broker's size refuses no session the broker would admit, and the broker's bucket stays the
// bound on a flood. envoy-infra cannot import this package, so this holds the numbers its goldens
// render to the broker's.
//
// Control: DefaultAccountLimit's burst changed to 300; the rendered burst no longer matches, and the
// test fails naming both.
func TestTheRenderedIntrospectionLimitIsTheBrokers(t *testing.T) {
	for _, golden := range []string{"clustered-hub-listener-pod-private.golden", "clustered-hub-tailnet-listener-pod-private.golden"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "gcp", "testdata", golden))
		if err != nil {
			t.Fatalf("read the rendered pod %s: %v", golden, err)
		}
		var pod struct {
			Spec struct {
				Containers []struct {
					Name string                         `json:"name"`
					Env  []struct{ Name, Value string } `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(raw, &pod); err != nil {
			t.Fatalf("decode the rendered pod %s: %v", golden, err)
		}
		env := map[string]string{}
		for _, container := range pod.Spec.Containers {
			if container.Name == "listener" {
				for _, variable := range container.Env {
					env[variable.Name] = variable.Value
				}
			}
		}
		perSecond, err := strconv.Atoi(env["ENVOY_AGENT_SECRETS_INTROSPECTIONS_PER_SECOND"])
		if err != nil || perSecond < 1 {
			t.Fatalf("%s renders ENVOY_AGENT_SECRETS_INTROSPECTIONS_PER_SECOND=%q, want a whole number", golden, env["ENVOY_AGENT_SECRETS_INTROSPECTIONS_PER_SECOND"])
		}
		burst, err := strconv.Atoi(env["ENVOY_AGENT_SECRETS_INTROSPECTION_BURST"])
		if err != nil {
			t.Fatalf("%s renders ENVOY_AGENT_SECRETS_INTROSPECTION_BURST=%q, want a whole number", golden, env["ENVOY_AGENT_SECRETS_INTROSPECTION_BURST"])
		}
		if every := time.Second / time.Duration(perSecond); every != DefaultAccountLimit.Every || burst != DefaultAccountLimit.Burst {
			t.Fatalf("%s renders %d a second (every %s) and a burst of %d, want the broker's per-address bound, every %s and a burst of %d",
				golden, perSecond, every, burst, DefaultAccountLimit.Every, DefaultAccountLimit.Burst)
		}
	}
}
