package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/listenerwire"
)

// TestNoRoleHolderIsTheListenersWords: Dispatch answers a role whose holder its caller may not see
// as a role no session claims, in words it keeps itself (dispatchenvoy.NoRoleHolder). They must be
// the words this listener answers an unclaimed role with, or that answer would tell the caller the
// role has a holder it is not shown.
func TestNoRoleHolderIsTheListenersWords(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeRoleHolderError(recorder, "walked-role", roles.Resolution{State: roles.Unclaimed})
	var refusal listenerwire.RoleRefusal
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the listener's refusal %s: %v", recorder.Body.Bytes(), err)
	}
	if want := dispatchenvoy.NoRoleHolder("walked-role").Error(); refusal.Error != want {
		t.Fatalf("the listener answers an unclaimed role %q, and Dispatch %q", refusal.Error, want)
	}
}
