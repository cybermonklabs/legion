package shim_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/shim"
)

// TestAConnectRefusalNamesNoneOfTheValue: ParseAddress refuses a --connect with user information
// or a query, which is where the worker stream's credential would be, naming the flag alone. The
// value printed as it came: written without "://" (no-scheme), it passes the daemon's output rule
// (redact.URLCredentials) whole, and with it (tcp) the rule is all that stood between the
// refusal and the value. The twin shows each value holds its markers. Captures
// shim.connect/<case>.
func TestAConnectRefusalNamesNoneOfTheValue(t *testing.T) {
	const (
		user     = "not-a-secret-user"
		password = "not-a-secret-password"
	)
	query := "?access" + "_token=" + oauth2calltest.Token
	values := map[string]string{
		"no-scheme": user + ":" + password + "@127.0.0.1:13371" + query,
		"tcp":       "tcp://" + user + ":" + password + "@127.0.0.1:13371" + query,
	}
	t.Run("twin", func(t *testing.T) {
		for name, value := range values {
			for _, marker := range []string{user, password, oauth2calltest.Token} {
				if !strings.Contains(value, marker) {
					t.Errorf("%s: %q holds no %q, so the row cannot see it printed", name, value, marker)
				}
			}
		}
	})
	for _, name := range []string{"no-scheme", "tcp"} {
		t.Run(name, func(t *testing.T) {
			_, _, err := shim.ParseAddress(values[name])
			if err == nil {
				t.Fatalf("ParseAddress took %q; want it refused", values[name])
			}
			oauth2calltest.RequireNoPiece(t, "shim.connect/"+name, err.Error(), user, password, oauth2calltest.Token)
			if !strings.Contains(err.Error(), "--connect") {
				t.Errorf("the refusal is %q; want it to name --connect", err)
			}
		})
	}
}
