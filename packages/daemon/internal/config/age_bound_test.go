package config

import (
	"strings"
	"testing"
)

// No key of the daemon's configuration bounds how long an issue pod may run on its old image and
// settings (dispatch://INFRA-4754 R4): a pod whose drift only drains keeps working until a release
// of it, however old. A top-level key whose name holds a drain or a maximum age is such a bound.
func TestNoKeySetsAnAgeBound(t *testing.T) {
	if len(topLevelKeys) == 0 {
		t.Fatal("the key table holds no top-level key, so this test reads nothing")
	}
	for _, key := range topLevelKeys {
		if strings.Contains(key.Name, "drain") || strings.Contains(key.Name, "max_age") {
			t.Errorf("the top-level key %q bounds a pod's age, which no key may", key.Name)
		}
	}
}
