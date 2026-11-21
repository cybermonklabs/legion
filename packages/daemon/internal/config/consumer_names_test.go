package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/consumernames"
)

// `consumer_names` in legion.yaml says how the daemon names its durable consumers: absent is the
// fixed names the daemon has always used, `fixed` is the same, and `fresh` is the selection
// records' fresh names. The listener's and Dispatch's ENVOY_CONSUMER_NAMES is no setting of the
// daemon's: set in the daemon's environment with no key in the file, it leaves the daemon on the
// fixed names, as every other daemon setting is the file's.
func TestTheConsumerNamingModeIsTheFilesConsumerNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
		env   map[string]string
		want  consumernames.Mode
	}{
		{name: "absent, the default", want: consumernames.ModeFixed},
		{name: "fixed", block: "consumer_names: fixed\n", want: consumernames.ModeFixed},
		{name: "fresh", block: "consumer_names: fresh\n", want: consumernames.ModeFresh},
		{name: "the environment's variable, which the daemon does not read", env: map[string]string{"ENVOY_CONSUMER_NAMES": "fresh"}, want: consumernames.ModeFixed},
		{name: "the file's fixed over the environment's fresh", block: "consumer_names: fixed\n", env: map[string]string{"ENVOY_CONSUMER_NAMES": "fresh"}, want: consumernames.ModeFixed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, minimalFile+tc.block), envMap(tc.env))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.ConsumerNames != tc.want {
				t.Fatalf("ConsumerNames = %q, want %q", cfg.ConsumerNames, tc.want)
			}
		})
	}
}

// A value that is neither mode refuses the load, naming the key and the value, at `legion start`
// and at `legion config check` (LoadForValidation) alike: a typo is never read as the default.
func TestAnUnknownConsumerNamingModeIsRefused(t *testing.T) {
	for _, value := range []string{"Fresh", `"fresh "`, "on", `""`} {
		t.Run(value, func(t *testing.T) {
			for _, load := range []struct {
				name string
				run  func(string, func(string) string) (Config, error)
			}{{"Load", Load}, {"LoadForValidation", LoadForValidation}} {
				_, err := load.run(writeConfigFile(t, minimalFile+"consumer_names: "+value+"\n"), envMap(nil))
				if err == nil || !strings.HasPrefix(err.Error(), "consumer_names must be 'fixed' or 'fresh' (got ") {
					t.Fatalf("%s with consumer_names: %s = %v, want the refusal naming the key and the value", load.name, value, err)
				}
			}
		})
	}
}
