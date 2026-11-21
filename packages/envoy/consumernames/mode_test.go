package consumernames

import (
	"strings"
	"testing"
)

func TestTheModeDefaultsToFixed(t *testing.T) {
	t.Setenv(ModeEnvVar, "")
	mode, err := ModeFromEnv()
	if err != nil || mode != ModeFixed {
		t.Fatalf("ModeFromEnv with %s unset = %q, %v; want %q", ModeEnvVar, mode, err, ModeFixed)
	}
}

func TestEachModeValueParses(t *testing.T) {
	for value, want := range map[string]Mode{"fixed": ModeFixed, "fresh": ModeFresh} {
		t.Setenv(ModeEnvVar, value)
		if mode, err := ModeFromEnv(); err != nil || mode != want {
			t.Errorf("%s=%s: %q, %v; want %q", ModeEnvVar, value, mode, err, want)
		}
	}
}

func TestAnUnknownModeRefusesNamingTheVariableAndTheValue(t *testing.T) {
	for _, value := range []string{"Fresh", "fresh ", "on", "fixed,fresh"} {
		t.Setenv(ModeEnvVar, value)
		mode, err := ModeFromEnv()
		if err == nil || !strings.Contains(err.Error(), ModeEnvVar) || !strings.Contains(err.Error(), `"`+value+`"`) {
			t.Errorf("%s=%q: %q, %v; want a refusal naming the variable and the value", ModeEnvVar, value, mode, err)
		}
	}
}
