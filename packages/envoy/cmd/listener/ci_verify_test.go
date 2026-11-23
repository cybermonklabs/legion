package main

import (
	"strings"
	"testing"
)

// ENVOY_CI_VERIFY is on or off, off when unset; any other value refuses the start naming the
// variable, since a misspelt value would otherwise turn the check off without a word.
func TestCIVerifySettingIsOnOrOffAndRefusesAnythingElse(t *testing.T) {
	for _, row := range []struct {
		value string
		on    bool
	}{
		{value: "", on: false},
		{value: "off", on: false},
		{value: "on", on: true},
	} {
		on, err := ciVerifySetting(row.value)
		if err != nil || on != row.on {
			t.Errorf("ciVerifySetting(%q) = %v, %v; want %v", row.value, on, err, row.on)
		}
	}
	for _, value := range []string{"1", "true", "ON", "yes"} {
		if on, err := ciVerifySetting(value); err == nil || on || !strings.Contains(err.Error(), ciVerifyEnv) {
			t.Errorf("ciVerifySetting(%q) = %v, %v; want a refusal naming %s", value, on, err, ciVerifyEnv)
		}
	}
}
