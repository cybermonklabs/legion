package oidc

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	type revokeTwin = struct {
		Error string `json:"error"`
	}
	for _, h := range []redacttest.Holder{
		{
			// The body CodeFlow.Revoke decodes a refusal into.
			Name: "oidc.CodeFlow.Revoke.body",
			Kind: "fields",
			Boxed: struct {
				Error redact.RemoteText `json:"error"`
			}{redact.NewRemoteText(redacttest.Code)},
			Same:     revokeTwin{redacttest.Code},
			Exported: revokeTwin{redacttest.Code},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
