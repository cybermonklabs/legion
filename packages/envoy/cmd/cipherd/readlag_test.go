package main

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore/onepassword"
)

// TestTheOnePasswordReadLagIsOneReloadAndAMinute: the 1Password store's value reads trust their
// kept items for one policy reload and a minute (onepassword.ReadLag), so a broker that reloads
// every policyRefresh makes no call for a value read, and revocation stays bounded by one reload.
// A change of either constant alone fails here.
func TestTheOnePasswordReadLagIsOneReloadAndAMinute(t *testing.T) {
	if want := policyRefresh + time.Minute; onepassword.ReadLag != want {
		t.Fatalf("onepassword.ReadLag is %s; want policyRefresh (%s) and a minute, %s", onepassword.ReadLag, policyRefresh, want)
	}
}
