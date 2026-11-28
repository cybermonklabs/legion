package main

import (
	"strings"
	"testing"
)

// A server's header identity reads X-Dispatch-User alone, so the driver refuses any other
// -identity-header value, naming it, rather than open rooms that refuse every peer; every
// spelling of X-Dispatch-User is accepted, and the peers name their person in X-Dispatch-User.
func TestTheDriverNamesItsPeersInX_Dispatch_UserAlone(t *testing.T) {
	flags := func(header string) []string {
		return []string{"-url", "http://127.0.0.1:8766", "-artifact", "a", "-identity-header", header}
	}
	for _, header := range []string{"Authorization", "Cookie", "X-Forwarded-User", ""} {
		_, err := parseFlags(flags(header))
		if err == nil || !strings.Contains(err.Error(), `"`+header+`"`) || !strings.Contains(err.Error(), "X-Dispatch-User") {
			t.Errorf("-identity-header %q: err = %v, want a refusal naming it and X-Dispatch-User", header, err)
		}
	}
	for _, header := range []string{"X-Dispatch-User", "x-dispatch-user", " X-DISPATCH-USER "} {
		if _, err := parseFlags(flags(header)); err != nil {
			t.Errorf("-identity-header %q: err %v, want it accepted", header, err)
		}
	}
}
