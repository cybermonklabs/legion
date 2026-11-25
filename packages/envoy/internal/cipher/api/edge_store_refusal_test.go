package api_test

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
)

// TestALoginHintTheStoreCannotHoldIsRefusedNotAServerError: a device authorization whose request
// object's login_hint holds a character the broker's store cannot write, U+0000, is refused 400
// REQUEST_INVALID before any row is written, at the edge and on the broker's own address alike. It
// is never a 500 server_error: at the edge a caller anywhere on the internet writes the object, and
// a 500 there is an alert any caller can raise, and a refusal the client cannot read as its own.
//
// The named revert: verify a request object without holding login_hint to what the store takes,
// and each row answers 500 server_error.
func TestALoginHintTheStoreCannotHoldIsRefusedNotAServerError(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	for _, hint := range []string{"nul@outside.example\x00", "a\x00@outside.example"} {
		status, body := postEdge(t, rig.EdgeURL, "/v1/oauth/device_authorization", edgeDeviceForm(t, rig.EdgeURL, hint),
			map[string]string{"X-Forwarded-For": forwardedFor("", "203.0.113.90")})
		if status != http.StatusBadRequest || body["code"] != "REQUEST_INVALID" {
			t.Errorf("an edge login naming %q = %d %v, want 400 REQUEST_INVALID", hint, status, body)
		}
		status, body = postEdge(t, rig.URL, "/v1/oauth/device_authorization", edgeDeviceForm(t, rig.URL, hint), nil)
		if status != http.StatusBadRequest || body["code"] != "REQUEST_INVALID" {
			t.Errorf("a login on the broker's own address naming %q = %d %v, want 400 REQUEST_INVALID", hint, status, body)
		}
	}
	if rows := loginRows(t, rig); rows != 0 {
		t.Fatalf("the refused logins wrote %d rows, want none", rows)
	}
}
