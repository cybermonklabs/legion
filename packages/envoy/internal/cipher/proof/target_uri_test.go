package proof

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/urljoin"
)

// TestAudienceIsTheBrokersOwnURL: a request object's audience is the address's scheme, host, port
// and path (TargetURI), so an address with user information, a query and a fragment names the URL
// the broker verifies the object for; an address that is no absolute URL with a host is
// urljoin.ErrBase, which quotes none of it.
func TestAudienceIsTheBrokersOwnURL(t *testing.T) {
	for _, address := range []string{"https://broker.example:8443/broker", "https://" + "ada:pass-7Qz4k@broker.example:8443/broker?sig=k3y-7Qz4k#pane"} {
		if got, err := Audience(cipheraddr.New(address)); err != nil || got != "https://broker.example:8443/broker" {
			t.Errorf("Audience(%q) = %q, %v; want the broker's own URL", address, got, err)
		}
	}
	if got, err := Audience(cipheraddr.New("not-a-secret-user:not-a-secret-token@broker.example")); !errors.Is(err, urljoin.ErrBase) || got != "" {
		t.Errorf("Audience of an address with no host = %q, %v; want urljoin.ErrBase", got, err)
	}
}

// TestTargetURIIsTheURLTheBrokerChecks: a proof signed over TargetURI of a request's URL verifies
// against the broker's public URL followed by the path the request reached it at, which is what
// the broker checks (internal/cipher/api's proofSubject), for an address with a path, a port, a
// query, a fragment and user information alike; each of the three the target leaves out would
// fail that check, so the twin signs the URL whole and is refused.
func TestTargetURIIsTheURLTheBrokerChecks(t *testing.T) {
	const checked = "https://secrets.test:8443/broker/v1/requests/req-1"
	for _, c := range []struct{ name, request string }{
		{"plain", "https://secrets.test:8443/broker/v1/requests/req-1"},
		{"query", "https://secrets.test:8443/broker/v1/requests/req-1?key=v&wait=5"},
		{"fragment", "https://secrets.test:8443/broker/v1/requests/req-1#pane"},
		{"user information", "https://ada:pass@secrets.test:8443/broker/v1/requests/req-1"},
		{"all three", "https://ada:pass@secrets.test:8443/broker/v1/requests/req-1?key=v#pane"},
	} {
		t.Run(c.name, func(t *testing.T) {
			request, err := url.Parse(c.request)
			if err != nil {
				t.Fatal(err)
			}
			if got := TargetURI(request); got != checked {
				t.Fatalf("TargetURI(%s) = %q, want %q", c.request, got, checked)
			}
			v, key := launcherVerifier(t)
			signed, err := SignLauncher(key, "cred-1", "GET", TargetURI(request), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := v.Verify(context.Background(), signed, "GET", checked, time.Now()); err != nil {
				t.Errorf("a proof over TargetURI(%s) = %v at the broker; want it verified", c.request, err)
			}
			if c.request == checked {
				return
			}
			whole, err := SignLauncher(key, "cred-1", "GET", c.request, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := v.Verify(context.Background(), whole, "GET", checked, time.Now()); err == nil {
				t.Errorf("a proof over %s whole verified at the broker; want it refused, which is what TargetURI exists for", c.request)
			}
		})
	}
}

// TestTargetURIKeepsAnEscapedPath: an element written escaped (%2F) stays escaped, as the request
// sends it.
func TestTargetURIKeepsAnEscapedPath(t *testing.T) {
	request, err := url.Parse("https://secrets.test/v1/secrets/a%2Fb/reread?x=1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := TargetURI(request), "https://secrets.test/v1/secrets/a%2Fb/reread"; got != want {
		t.Fatalf("TargetURI = %q, want %q", got, want)
	}
}

// launcherVerifier is a Verifier that knows one launcher credential, cred-1, with the key it
// answers.
func launcherVerifier(t *testing.T) (*Verifier, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	return &Verifier{
		Skew: time.Minute,
		LookupLauncher: func(_ context.Context, id string) (string, bool, error) {
			return thumbprint, id == "cred-1", nil
		},
		Replay: func(_ context.Context, jti string, _ time.Time) (bool, error) {
			fresh := !seen[jti]
			seen[jti] = true
			return fresh, nil
		},
	}, key
}
