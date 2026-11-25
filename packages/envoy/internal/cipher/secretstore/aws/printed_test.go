package aws_test

import (
	"strings"
	"testing"
)

// wantNamedNotPrinted fails t unless text names each of named and holds none of secrets.
func wantNamedNotPrinted(t *testing.T, what, text string, named []string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Errorf("%s holds %q: %s", what, secret, text)
		}
	}
	for _, name := range named {
		if !strings.Contains(text, name) {
			t.Errorf("%s does not name %q: %s", what, name, text)
		}
	}
}
