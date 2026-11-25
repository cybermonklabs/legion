// packages/envoy/internal/cipherclient/detect.go
//
// The identity a machine can trade at the broker when nothing tells it which one it holds.
// MachineIdentity reads a deployment's own choice (IdentityVar); DetectMachineIdentity asks each
// provider's detector (identity/providers), in rank order, for the field of the broker's
// GET /v1/settings answer that detector reads, so a machine reaches the broker as itself with
// nothing configured. A provider left out of this build takes its detector with it.

package cipherclient

import (
	"context"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/providers"
)

// Skipped is one place a detector looked and found nothing it could trade, and why, in words a
// refusal prints as they stand.
type Skipped = identity.Skipped

// DetectMachineIdentity is the first identity this machine holds, with every place looked before
// it and why each had nothing. fields are the broker's own settings answer, by JSON name: a
// detector runs only where the field it reads carries a value, since that value is the audience
// the token it finds must be minted for. It answers nil and no error where no detector holds
// one, which is a machine with no identity of its own rather than a fault; a place that is
// present but broken is an error, and nothing after it is tried. Nothing here verifies a token:
// the broker does that.
func DetectMachineIdentity(ctx context.Context, fields map[string]string) (Source, []Skipped, error) {
	var skipped []Skipped
	for _, detector := range providers.Detectors() {
		audience := fields[detector.SettingsField]
		if audience == "" {
			continue
		}
		found, looked, err := detector.Detect(ctx, audience, HTTPClient())
		skipped = append(skipped, looked...)
		if err != nil || found != nil {
			return found, skipped, err
		}
	}
	return nil, skipped, nil
}
