package boot_test

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/boot"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
)

// TestConfigKindsAreRegistered: the kind config.ParseAccount gives an account of each form is the
// kind of the provider that verifies such an account's token, and that provider is one this build
// registers, so an API caller or a NATS service config.Load reads is never one no verifier claims.
func TestConfigKindsAreRegistered(t *testing.T) {
	for _, c := range []struct {
		account string
		want    identity.Kind
	}{
		{"system:serviceaccount:example-ns:example-worker", kubernetes.Kind},
		{"google:example-fleet@example-project.iam.gserviceaccount.com", google.Kind},
	} {
		got, err := config.ParseAccount("BROKER_API_ACCOUNTS", "listener="+c.account, c.account)
		if err != nil {
			t.Fatalf("ParseAccount(%s): %v", c.account, err)
		}
		if got.Kind != c.want {
			t.Errorf("ParseAccount(%s).Kind = %q, want %q", c.account, got.Kind, c.want)
		}
		if !slices.Contains(boot.Kinds(), got.Kind) {
			t.Errorf("ParseAccount(%s).Kind = %q, which boot.Kinds() %v does not hold", c.account, got.Kind, boot.Kinds())
		}
	}
}
