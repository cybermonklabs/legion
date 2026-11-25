package natsgrant_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/natsgrant"
)

// TestTheBusSpellsThePrivacySuffixAsThisPackageDoes: internal/bus keeps PrivacyRequiredSuffix last
// in a connection's name, with the build before it, and spells the suffix itself, since this
// package imports internal/bus and it cannot import this one back. A suffix the two spell apart
// would put the build after the one the auth service reads.
func TestTheBusSpellsThePrivacySuffixAsThisPackageDoes(t *testing.T) {
	if bus.PrivacyRequiredSuffix != natsgrant.PrivacyRequiredSuffix {
		t.Fatalf("internal/bus spells the privacy suffix %q, natsgrant %q", bus.PrivacyRequiredSuffix, natsgrant.PrivacyRequiredSuffix)
	}
}
