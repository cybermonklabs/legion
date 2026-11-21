// edge_limits_test.go holds the edge's own buckets to what a caller on the internet can spend:
// a person's bucket per address, an IPv6 caller's /64, and the one bucket every caller of a device
// authorization shares.
package api_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// edgeLogin posts a device authorization naming person at rig's edge from client and answers the
// status and the OAuth body; mismatched sends as dpop_jkt the thumbprint of another key than the
// request object's, a request the broker refuses before it writes anything.
func edgeLogin(t *testing.T, rig *ciphertest.Rig, client, person string, mismatched bool) (int, map[string]any) {
	t.Helper()
	form := edgeDeviceForm(t, rig.EdgeURL, person)
	if mismatched {
		other := newSigningKey(t)
		jkt, err := proof.Thumbprint(&other.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		form.Set("dpop_jkt", jkt)
	}
	return postEdge(t, rig.EdgeURL, "/v1/oauth/device_authorization", form, map[string]string{"X-Forwarded-For": forwardedFor("", client)})
}

// TestTheEdgeKeysAPersonsBucketOnTheCallersAddress: a caller on the internet can name any outside
// person, so at the edge a person's bucket is the person's at the caller's address, and only a
// login the broker opened spends it. Address A spends its burst of logins for the person and is
// then refused 429 RATE_LIMITED; address B still opens a login for the same person. Address C
// sends requests the broker refuses before it writes anything (a dpop_jkt that is not the request
// object's key), and then still opens its whole burst of logins for that person.
//
// The named reverts: key the person's bucket on the person alone, and B is refused 429; spend it
// before Deps.DeviceLogin, and C's last login is refused 429.
func TestTheEdgeKeysAPersonsBucketOnTheCallersAddress(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	burst := api.DefaultLauncherLimits.PerLogin.Burst
	const a, b, c = "203.0.113.160", "203.0.113.161", "203.0.113.162"
	for i := range burst {
		if status, body := edgeLogin(t, rig, a, outsidePerson, false); status != http.StatusOK {
			t.Fatalf("login %d of %d for %s from %s = %d %v, want 200", i+1, burst, outsidePerson, a, status, body)
		}
	}
	if status, body := edgeLogin(t, rig, a, outsidePerson, false); status != http.StatusTooManyRequests || body["code"] != "RATE_LIMITED" {
		t.Fatalf("login %d for %s from %s = %d %v, want 429 RATE_LIMITED", burst+1, outsidePerson, a, status, body)
	}
	if status, body := edgeLogin(t, rig, b, outsidePerson, false); status != http.StatusOK {
		t.Fatalf("a login for %s from %s, after %s spent its bucket for that person = %d %v, want 200", outsidePerson, b, a, status, body)
	}
	for i := range 4 {
		if status, body := edgeLogin(t, rig, c, outsidePerson, true); status != http.StatusBadRequest || body["code"] != "REQUEST_INVALID" {
			t.Fatalf("refused request %d for %s from %s = %d %v, want 400 REQUEST_INVALID", i+1, outsidePerson, c, status, body)
		}
	}
	for i := range burst {
		if status, body := edgeLogin(t, rig, c, outsidePerson, false); status != http.StatusOK {
			t.Fatalf("login %d of %d for %s from %s after four refused requests = %d %v, want 200: a refused request spent the person's bucket", i+1, burst, outsidePerson, c, status, body)
		}
	}
}

// TestTheEdgeKeysAnIPv6CallerOnItsSlash64: one host or one site holds a whole IPv6 /64, so the
// edge keys an IPv6 caller's bucket on its /64. Addresses of one /64, each a new one, spend one
// bucket: its burst is served and the next is refused 429 RATE_LIMITED; an address of the next
// /64 is served.
//
// The named revert: key an IPv6 caller on its own address, and every address of the /64 is
// served.
func TestTheEdgeKeysAnIPv6CallerOnItsSlash64(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	burst := api.DefaultLauncherLimits.PerAddress.Burst
	for i := range burst {
		client := fmt.Sprintf("2001:db8:1:2::%x", i+1)
		if status, body := edgeLogin(t, rig, client, fmt.Sprintf("v6-%d@outside.example", i), false); status != http.StatusOK {
			t.Fatalf("login %d of %d from %s = %d %v, want 200", i+1, burst, client, status, body)
		}
	}
	if status, body := edgeLogin(t, rig, "2001:db8:1:2:ffff::1", "v6-late@outside.example", false); status != http.StatusTooManyRequests || body["code"] != "RATE_LIMITED" {
		t.Fatalf("a login from another address of 2001:db8:1:2::/64 past its burst = %d %v, want 429 RATE_LIMITED", status, body)
	}
	if status, body := edgeLogin(t, rig, "2001:db8:1:3::1", "v6-next@outside.example", false); status != http.StatusOK {
		t.Fatalf("a login from 2001:db8:1:3::/64 = %d %v, want 200", status, body)
	}
}

// TestTheEdgeBoundsTheDeviceAuthorizationsOfEveryCallerTogether: beside each address's bucket the
// edge holds one bucket every caller of a device authorization shares, which bounds what callers
// from many addresses together can make the broker store. With that bucket at a burst of three
// that does not refill within the test, three logins from three addresses are served and write
// their rows, and the fourth, from a fourth address, is refused 429 RATE_LIMITED and writes
// nothing; the broker's own address still serves a login.
//
// The named revert: drop the shared bucket from the edge (launcherLimiter.overall), and the fourth
// login is served.
func TestTheEdgeBoundsTheDeviceAuthorizationsOfEveryCallerTogether(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdgeDeviceLimit(ratelimit.Limit{Every: time.Hour, Burst: 3}))
	for i := range 3 {
		client := fmt.Sprintf("198.51.100.%d", 170+i)
		if status, body := edgeLogin(t, rig, client, outsidePerson, false); status != http.StatusOK {
			t.Fatalf("login %d of 3 from %s = %d %v, want 200", i+1, client, status, body)
		}
	}
	before := loginRows(t, rig)
	status, body := edgeLogin(t, rig, "198.51.100.173", outsidePerson, false)
	if status != http.StatusTooManyRequests || body["code"] != "RATE_LIMITED" {
		t.Fatalf("a fourth login from a fourth address = %d %v, want 429 RATE_LIMITED", status, body)
	}
	if after := loginRows(t, rig); after != before {
		t.Fatalf("the refused fourth login wrote %d rows, want none", after-before)
	}
	m := newDeviceMachine(t, rig)
	if da := m.login(t); da.UserCode == "" {
		t.Fatal("the broker's own address opened no login after the edge's shared bucket ran out")
	}
}
