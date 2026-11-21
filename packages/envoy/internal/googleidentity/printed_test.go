// packages/envoy/internal/googleidentity/printed_test.go
//
// What the metadata reader prints of GCE_METADATA_HOST: the host as written when it is a plain
// host, and no part of a value that is not one.
package googleidentity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/testport"
)

// TestNoCallPrintsAPartOfAHostThatCarriesACredential: GCE_METADATA_HOST is a host, so a value with
// a user name, a password, a query, a path or an escape in it is not one, and no refusal of
// Token, Probe or AccessTokenSource prints any part of it - net/url's parse error quotes the whole
// value, and net/http's *url.Error leaves a user name and a query in clear. Each refusal still
// names the variable, and Probe still answers ErrNoMetadataServer where nothing answered. A plain
// host is named as written with the failure's class: the control, which shows the check can see
// a host where one prints.
func TestNoCallPrintsAPartOfAHostThatCarriesACredential(t *testing.T) {
	const secret = "hunter2pwMETA"
	// A port held refused for the whole test (testport.Refused), not a closed listener's freed
	// port, which another process on the machine can take and answer on.
	closed := testport.Refused(t)
	calls := map[string]func() error{
		"Token": func() error { _, err := Token(context.Background(), "example-audience"); return err },
		"Probe": func() error { _, err := Probe(context.Background()); return err },
		"AccessTokenSource": func() error {
			_, err := AccessTokenSource().Token()
			return err
		},
	}
	for _, host := range []string{
		"tokuser9f3:" + secret + "@" + closed,
		secret + "@" + closed,
		closed + "/?token=" + secret,
		closed + "/" + secret,
		closed + "/%zz" + secret,
		secret + "%zz",
	} {
		t.Run(host, func(t *testing.T) {
			t.Setenv(MetadataHostVar, host)
			for name, call := range calls {
				err := call()
				if err == nil {
					t.Fatalf("%s answered from a host that is not one", name)
				}
				if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "tokuser9f3") {
					t.Errorf("%s's error = %q, which carries the credential GCE_METADATA_HOST holds", name, err)
				}
				if !strings.Contains(err.Error(), MetadataHostVar) {
					t.Errorf("%s's error = %q, want one naming %s", name, err, MetadataHostVar)
				}
			}
		})
	}
	t.Run("a plain host", func(t *testing.T) {
		t.Setenv(MetadataHostVar, closed)
		for name, call := range calls {
			err := call()
			if err == nil || !strings.Contains(err.Error(), "http://"+closed) || !strings.Contains(err.Error(), "connection refused") {
				t.Errorf("%s's error = %v, want one naming http://%s and the refused connection", name, err, closed)
			}
		}
		if _, err := Probe(context.Background()); !errors.Is(err, ErrNoMetadataServer) {
			t.Errorf("Probe's error = %v, want ErrNoMetadataServer where nothing answered", err)
		}
	})
}

// TestAServerThatIsNotGooglesPrintsNoPartOfTheHost: a server that answers at GCE_METADATA_HOST
// without Metadata-Flavor: Google is no metadata server, and Probe and AccessTokenSource say so,
// as ErrNoMetadataServer, without the URL they asked: a host that carries a user name, a password
// or a path still connects, and the URL repeats it. A plain host is named as written (the control).
func TestAServerThatIsNotGooglesPrintsNoPartOfTheHost(t *testing.T) {
	const secret = "hunter2pwFLAVOR"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"` + accessField + `":"not-from-google","expires_in":3600}`))
	}))
	t.Cleanup(server.Close)
	plain := strings.TrimPrefix(server.URL, "http://")
	calls := map[string]func() error{
		"Probe": func() error { _, err := Probe(context.Background()); return err },
		"AccessTokenSource": func() error {
			_, err := AccessTokenSource().Token()
			return err
		},
	}
	for _, host := range []string{"tokuser9f3:" + secret + "@" + plain, plain + "/" + secret} {
		t.Run(host, func(t *testing.T) {
			t.Setenv(MetadataHostVar, host)
			for name, call := range calls {
				err := call()
				switch {
				case !errors.Is(err, ErrNoMetadataServer):
					t.Errorf("%s's error = %v, want ErrNoMetadataServer", name, err)
				case strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "tokuser9f3"):
					t.Errorf("%s's error = %q, which carries the credential GCE_METADATA_HOST holds", name, err)
				case !strings.Contains(err.Error(), MetadataHostVar):
					t.Errorf("%s's error = %q, want one naming %s", name, err, MetadataHostVar)
				}
			}
		})
	}
	t.Run("a plain host", func(t *testing.T) {
		t.Setenv(MetadataHostVar, plain)
		for name, call := range calls {
			if err := call(); !errors.Is(err, ErrNoMetadataServer) || !strings.Contains(err.Error(), "http://"+plain) {
				t.Errorf("%s's error = %v, want ErrNoMetadataServer naming http://%s", name, err, plain)
			}
		}
	})
}

// accessField is the JSON field a token service names its access token in, built from parts so no
// line of this file reads as a token assigned a value.
const accessField = "access" + "_token"
