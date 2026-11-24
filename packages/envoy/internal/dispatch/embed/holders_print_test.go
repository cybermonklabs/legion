package embed

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	code := redact.NewRemoteText(redacttest.Code)
	type bodyTwin = struct {
		Code string `json:"code"`
	}
	type errorTwin = struct {
		Error bodyTwin `json:"error"`
	}
	type refusalTwin = struct {
		Error string `json:"error"`
	}
	for _, h := range []redacttest.Holder{
		{
			Name:     "embed.azureErrorResponse",
			Kind:     "fields",
			Boxed:    azureErrorResponse{Error: azureErrorBody{Code: code}},
			Same:     errorTwin{bodyTwin{redacttest.Code}},
			Exported: errorTwin{bodyTwin{redacttest.Code}},
		},
		{
			Name:     "embed.azureErrorBody",
			Kind:     "fields",
			Boxed:    azureErrorBody{Code: code},
			Same:     bodyTwin{redacttest.Code},
			Exported: bodyTwin{redacttest.Code},
		},
		{
			Name: "embed.EntraTokenSource.exchange.refusal",
			Kind: "fields",
			Boxed: struct {
				Error redact.RemoteText `json:"error"`
			}{code},
			Same:     refusalTwin{redacttest.Code},
			Exported: refusalTwin{redacttest.Code},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
