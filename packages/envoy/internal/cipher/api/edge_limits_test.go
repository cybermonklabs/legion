// edge_limits_test.go holds the edge's own buckets to what a caller on the internet can spend:
// a person's bucket per address, an IPv6 caller's /64, and the one bucket every caller of a device
// authorization shares.
package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestTheEdgeSpendsItsSharedBucketOnlyOnALoginItCanOpen: a device authorization the device login
// would refuse spends nothing of the bucket every caller shares. With that bucket at a burst of
// three that does not refill within the test, five addresses each send one such request: another
// client's id, a request object that is no JWS, one signed for the broker's own address rather
// than the edge, a dpop_jkt that is not the object's key (400 REQUEST_INVALID) and a service's
// login (400 SERVICE_LOGS_IN_WITH_TOKEN). Each is refused and writes nothing. A valid login from
// another address is then served, and so are two more; the fourth valid login is refused 429
// RATE_LIMITED, so valid logins still spend the bucket.
//
// The named reverts: spend the shared bucket before the request is judged, and the first three
// refused requests spend its three tokens, so the fourth, the dpop_jkt that is not the object's
// key, answers 429 where the test wants a refusal that is no 429; judge a request only by its
// object's signature and fields (record.VerifyRequestObject), and the dpop_jkt mismatch and the
// service's login spend two tokens, so the second valid login is refused 429.
func TestTheEdgeSpendsItsSharedBucketOnlyOnALoginItCanOpen(t *testing.T) {
	t.Parallel()
	rig := ciphertest.NewRig(t, ciphertest.WithEdgeDeviceLimit(ratelimit.Limit{Every: time.Hour, Burst: 3}))
	before := loginRows(t, rig)
	for i, row := range []struct {
		name string
		form func() url.Values
		// code is the broker's code the refusal names; "" is any 4xx refusal that is no 429.
		code string
	}{
		{"another client's id", func() url.Values {
			form := edgeDeviceForm(t, rig.EdgeURL, outsidePerson)
			form.Set("client_id", "junk")
			return form
		}, ""},
		{"a request object that is no JWS", func() url.Values {
			form := edgeDeviceForm(t, rig.EdgeURL, outsidePerson)
			form.Set("request", "junk")
			return form
		}, ""},
		{"a request object signed for the broker's own address", func() url.Values { return edgeDeviceForm(t, rig.URL, outsidePerson) }, ""},
		{"a dpop_jkt that is not the object's key", func() url.Values {
			form := edgeDeviceForm(t, rig.EdgeURL, outsidePerson)
			other := newSigningKey(t)
			jkt, err := proof.Thumbprint(&other.PublicKey)
			if err != nil {
				t.Fatal(err)
			}
			form.Set("dpop_jkt", jkt)
			return form
		}, "REQUEST_INVALID"},
		{"a service's login", func() url.Values {
			key := newSigningKey(t)
			jkt, err := proof.Thumbprint(&key.PublicKey)
			if err != nil {
				t.Fatal(err)
			}
			return url.Values{
				"client_id": {oauthwire.ClientID}, "scope": {oauthwire.Scope}, "dpop_jkt": {jkt},
				"request": {signMachineLoginRequest(t, key, rig.EdgeURL, "", "example-host-devbox", "example-service")},
			}
		}, "SERVICE_LOGS_IN_WITH_TOKEN"},
	} {
		client := fmt.Sprintf("198.51.100.%d", 180+i)
		status, body := postEdge(t, rig.EdgeURL, "/v1/oauth/device_authorization", row.form(), map[string]string{"X-Forwarded-For": forwardedFor("", client)})
		if status < 400 || status >= 500 || status == http.StatusTooManyRequests || (row.code != "" && body["code"] != row.code) {
			t.Fatalf("%s from %s = %d %v, want a 4xx refusal that is no 429, naming %q when set", row.name, client, status, body, row.code)
		}
	}
	if after := loginRows(t, rig); after != before {
		t.Fatalf("the refused device authorizations wrote %d rows, want none", after-before)
	}
	for i := range 3 {
		client := fmt.Sprintf("198.51.100.%d", 190+i)
		if status, body := edgeLogin(t, rig, client, outsidePerson, false); status != http.StatusOK {
			t.Fatalf("valid login %d of 3 from %s after the refused requests = %d %v, want 200: a refused request spent the shared bucket", i+1, client, status, body)
		}
	}
	if status, body := edgeLogin(t, rig, "198.51.100.199", outsidePerson, false); status != http.StatusTooManyRequests || body["code"] != "RATE_LIMITED" {
		t.Fatalf("a fourth valid login = %d %v, want 429 RATE_LIMITED from the shared bucket", status, body)
	}
}

// TestEveryLoginTheEdgeOpensSpendsItsSharedBucket: the edge's pre-check reads its own clock, so
// a login it refuses can still be opened, as one whose iat crosses a second between the two clock
// reads is; such a login spends the shared bucket once opened. With that bucket at a burst of one
// that does not refill within the test, one valid login spends it, and then a login the pre-check
// refuses (a dpop_jkt that is not the object's key) but the edge's device login opens (a wrapper
// that hands the real login the object's own key, which no clock decides) is refused 429
// RATE_LIMITED, and the login it opened is rolled back: it writes nothing.
//
// The named revert: spend the shared bucket only on the pre-check's word, and the login the
// pre-check refused is served with the bucket empty.
func TestEveryLoginTheEdgeOpensSpendsItsSharedBucket(t *testing.T) {
	t.Parallel()
	var rig *ciphertest.Rig
	rig = ciphertest.NewRig(t,
		ciphertest.WithEdgeDeviceLimit(ratelimit.Limit{Every: time.Hour, Burst: 1}),
		ciphertest.WithEdgeDeviceLogin(func(open api.DeviceLoginFunc) api.DeviceLoginFunc {
			return func(ctx context.Context, form url.Values, userCode string) error {
				obj, err := record.VerifyRequestObject(form.Get("request"), rig.EdgeURL, time.Minute, time.Now())
				if err != nil {
					return open(ctx, form, userCode)
				}
				opened := url.Values{}
				for k, v := range form {
					opened[k] = v
				}
				opened.Set("dpop_jkt", obj.Thumbprint)
				return open(ctx, opened, userCode)
			}
		}))
	if status, body := edgeLogin(t, rig, "198.51.100.201", outsidePerson, false); status != http.StatusOK {
		t.Fatalf("the valid login that spends the shared bucket = %d %v, want 200", status, body)
	}
	before := loginRows(t, rig)
	status, body := edgeLogin(t, rig, "198.51.100.202", outsidePerson, true)
	if status != http.StatusTooManyRequests || body["code"] != "RATE_LIMITED" {
		t.Fatalf("a login the pre-check refused and the edge opened, with the shared bucket empty = %d %v, want 429 RATE_LIMITED", status, body)
	}
	if after := loginRows(t, rig); after != before {
		t.Fatalf("the refused login wrote %d rows, want none: it was not rolled back", after-before)
	}
}
