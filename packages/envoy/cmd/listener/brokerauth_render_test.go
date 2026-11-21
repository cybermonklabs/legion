package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderedListenerEnv is the environment of the container named container in the pod spec golden
// name, which envoy-infra renders under packages/envoy/infra/gcp/testdata: each variable with a
// plain value, as the container receives it. A variable drawn from a Secret or a field has no
// value there and is left out; a golden holding no such container fails the test.
func renderedListenerEnv(t *testing.T, name, container string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "infra", "gcp", "testdata", name))
	if err != nil {
		t.Fatalf("read the rendered pod %s: %v", name, err)
	}
	var pod struct {
		Spec struct {
			Containers []struct {
				Name string `json:"name"`
				Env  []struct {
					Name  string  `json:"name"`
					Value *string `json:"value"`
				} `json:"env"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		t.Fatalf("decode the rendered pod %s: %v", name, err)
	}
	for _, rendered := range pod.Spec.Containers {
		if rendered.Name != container {
			continue
		}
		env := make(map[string]string, len(rendered.Env))
		for _, variable := range rendered.Env {
			if variable.Value != nil {
				env[variable.Name] = *variable.Value
			}
		}
		if len(env) == 0 {
			t.Fatalf("the listener container of %s renders no environment", name)
		}
		return env
	}
	t.Fatalf("%s holds no container named %s", name, container)
	return nil
}

// TestTheRenderedListenerEnvResolves holds the listener envoy-infra renders to the listener's own
// rule for its broker settings: the environment of each golden a listener of a hub that may make
// a project private is rendered as is fed to resolveListenerBrokerConfig, which accepts it, in the
// token-file form, with the origin it renders. A render the listener refused would be a listener
// that does not start (main makes the refusal fatal), so this is the start's own check of what the
// component renders.
//
// Control: the private environment without ENVOY_PUBLIC_URL; the resolver refuses it, and the
// test fails naming the variable.
func TestTheRenderedListenerEnvResolves(t *testing.T) {
	for _, golden := range []struct{ name, container string }{
		{"clustered-hub-listener-pod-private.golden", "listener"},
		{"dispatch-listener-pod-private.golden", "envoy-listener"},
	} {
		t.Run(golden.name, func(t *testing.T) {
			env := renderedListenerEnv(t, golden.name, golden.container)
			config, err := resolveListenerBrokerConfig(func(name string) string { return env[name] })
			if err != nil {
				t.Fatalf("the listener refuses the environment %s renders: %v", golden.name, err)
			}
			if !config.configured() || config.AccountTokenFile == "" || config.GoogleAudience != "" || config.Origin != env[EnvPublicURL] {
				t.Fatalf("%s resolves to %+v, want the broker check in its token-file form with the origin it renders", golden.name, config)
			}

			delete(env, EnvPublicURL)
			if _, err := resolveListenerBrokerConfig(func(name string) string { return env[name] }); err == nil || !strings.Contains(err.Error(), EnvPublicURL) {
				t.Fatalf("%s without %s resolves (err = %v), want a refusal naming %s", golden.name, EnvPublicURL, err, EnvPublicURL)
			}
		})
	}
}
