package redact_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement is the box print test of this package's
// holders: a RemoteNumber, which embeds the box and prints by its methods, holding the probe.
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	for _, h := range []redacttest.Holder{
		{
			Name:     "redact.RemoteNumber",
			Kind:     "text",
			Boxed:    redact.RemoteNumber{RemoteText: redact.NewRemoteText(redacttest.Words)},
			Same:     redacttest.Words,
			Exported: redacttest.Words,
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
