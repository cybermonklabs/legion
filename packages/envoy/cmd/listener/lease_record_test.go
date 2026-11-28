package main

import (
	"encoding/json"
	"testing"
)

// Every rewrite keeps the JSON keys of the record this build does not know.
func TestLeaseRewriteKeepsUnknownFields(t *testing.T) {
	t.Parallel()
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	putLease(t, kv, `{"holder":"pod-new-build","boot":"0011223344556677","released":true,"future":{"k":[1,2]}}`)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	b.step()
	if !b.Holding() {
		t.Fatal("standby did not take the released record")
	}
	clock.advance(leaseRenewEvery)
	b.step()
	_, entry := readLease(t, kv)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entry.Value(), &fields); err != nil {
		t.Fatalf("decode %s: %v", entry.Value(), err)
	}
	if got := string(fields["future"]); got != `{"k":[1,2]}` {
		t.Fatalf("record %s: future = %s, want the newer build's field kept", entry.Value(), got)
	}
	if got := string(fields["holder"]); got != `"pod-b"` {
		t.Fatalf("record %s: holder = %s, want pod-b", entry.Value(), got)
	}
}
