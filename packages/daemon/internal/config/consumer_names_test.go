package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/consumernames"
)

// The daemon reads how it names its durable consumers from ENVOY_CONSUMER_NAMES in its own
// environment, once, when `legion start` loads its configuration: unset is the fixed names the
// daemon has always used, "fixed" is the same, and "fresh" is the selection records' fresh names.
// The variable is spelled out here rather than through consumernames.ModeEnvVar, so a rename of the
// constant fails this row instead of leaving every deployment that sets the old name on fixed names.
func TestTheConsumerNamingModeIsReadFromTheDaemonsEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want consumernames.Mode
	}{
		{name: "unset, the default", want: consumernames.ModeFixed},
		{name: "empty", env: map[string]string{"ENVOY_CONSUMER_NAMES": ""}, want: consumernames.ModeFixed},
		{name: "fixed", env: map[string]string{"ENVOY_CONSUMER_NAMES": "fixed"}, want: consumernames.ModeFixed},
		{name: "fresh", env: map[string]string{"ENVOY_CONSUMER_NAMES": "fresh"}, want: consumernames.ModeFresh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, minimalFile), envMap(tc.env))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.ConsumerNames != tc.want {
				t.Fatalf("ConsumerNames = %q, want %q", cfg.ConsumerNames, tc.want)
			}
		})
	}
}

// A value that is neither mode refuses the load, naming the variable and the value, so a daemon
// given one never starts, on either mode: a typo is never read as the default. `legion config check`
// loads the same way, so it refuses the value too.
func TestAnUnknownConsumerNamingModeRefusesTheStart(t *testing.T) {
	for _, value := range []string{"Fresh", "fresh ", "on", "both"} {
		t.Run(value, func(t *testing.T) {
			for _, load := range []struct {
				name string
				run  func(string, func(string) string) (Config, error)
			}{{"Load", Load}, {"LoadForValidation", LoadForValidation}} {
				_, err := load.run(writeConfigFile(t, minimalFile), envMap(map[string]string{"ENVOY_CONSUMER_NAMES": value}))
				if err == nil || !strings.Contains(err.Error(), "ENVOY_CONSUMER_NAMES") || !strings.Contains(err.Error(), `"`+value+`"`) {
					t.Fatalf("%s with ENVOY_CONSUMER_NAMES=%q = %v, want a refusal naming the variable and the value", load.name, value, err)
				}
			}
		})
	}
}
