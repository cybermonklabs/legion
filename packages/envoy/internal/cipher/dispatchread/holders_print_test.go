package dispatchread

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holder, the refusal RefusalCode decodes: it holds the probe
// in its box field, and its twins have the same shape with the probe as a string there
// (redacttest.PrintTest).
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	type refusalTwin = struct {
		Code string `json:"code"`
	}
	for _, h := range []redacttest.Holder{
		{
			Name: "dispatchread.RefusalCode.refusal",
			Kind: "fields",
			Boxed: struct {
				Code redact.RemoteText `json:"code"`
			}{redact.NewRemoteText(redacttest.Code)},
			Same:     refusalTwin{redacttest.Code},
			Exported: refusalTwin{redacttest.Code},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
