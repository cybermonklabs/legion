package redact_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTheBoxPrintsNoSecretByAnyPrinterAtAnyPlacement is the box print test of the box itself:
// redact.RemoteText holding the probe, by every printer at every placement, beside its twin, the
// probe as a plain string.
func TestTheBoxPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	for name, words := range map[string]string{"words": redacttest.Words, "fragment": redacttest.FragmentWords, "unreadable": redacttest.Unreadable} {
		t.Run(name, func(t *testing.T) {
			redacttest.PrintTest(t, redacttest.Holder{
				Name:     "redact.RemoteText/" + name,
				Kind:     "text",
				Boxed:    redact.NewRemoteText(words),
				Same:     words,
				Exported: words,
			})
		})
	}
}
