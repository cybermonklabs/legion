package googleidentity

import (
	"context"
	"strings"
	"testing"
)

// GCE_METADATA_HOST names a host with an optional port, and nothing else: a value with a user part,
// a path or a query is refused by its length, before any request, so no URL this package asks, and
// no error that names one, holds a part of it. The control is a host and a port, which is asked and
// named. The user part's values are made for this test.
func TestAMetadataHostThatIsNoHostIsRefusedByItsLength(t *testing.T) {
	const (
		user     = "made-up-user-6d2b"
		password = "not-a-secret-91c4a7"
	)
	for _, host := range []string{
		user + ":" + password + "@127.0.0.1:1",
		password + "@127.0.0.1:1",
		"127.0.0.1:1/" + password,
		"127.0.0.1:1?" + password,
	} {
		t.Setenv(MetadataHostVar, host)
		texts := map[string]string{}
		if _, err := Probe(context.Background()); err == nil {
			t.Fatalf("host %q: Probe passed", host)
		} else {
			texts["Probe"] = err.Error()
		}
		if _, err := Token(context.Background(), "made-up-audience"); err == nil {
			t.Fatalf("host %q: Token passed", host)
		} else {
			texts["Token"] = err.Error()
		}
		if _, err := AccessTokenSource().Token(); err == nil {
			t.Fatalf("host %q: the access token passed", host)
		} else {
			texts["the access token"] = err.Error()
		}
		for name, text := range texts {
			if !strings.Contains(text, MetadataHostVar) || !strings.Contains(text, "characters") {
				t.Errorf("host %q, %s: %q, want the refusal that names %s by its length", host, name, text, MetadataHostVar)
			}
			if strings.Contains(text, password) || strings.Contains(text, user) {
				t.Errorf("host %q, %s: %q holds a part of the value", host, name, text)
			}
		}
	}
	t.Setenv(MetadataHostVar, "127.0.0.1:1")
	if _, err := Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("the control 127.0.0.1:1: Probe err = %v, want the host asked and named", err)
	}
}
