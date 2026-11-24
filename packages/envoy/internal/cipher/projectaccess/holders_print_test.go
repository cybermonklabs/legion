package projectaccess

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holder: it holds the probe in its box field, and its twins
// have the same shape with the probe as a string there (redacttest.PrintTest). Dispatch's refusal
// code is dispatchread's holder (dispatchread.RefusalCode).
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	type memberKindTwin = struct {
		MemberKind string `json:"member_kind"`
	}
	for _, h := range []redacttest.Holder{
		{
			Name: "projectaccess.Client.readKind.answer",
			Kind: "fields",
			Boxed: struct {
				MemberKind redact.RemoteText `json:"member_kind"`
			}{redact.NewRemoteText(redacttest.Words)},
			Same:     memberKindTwin{redacttest.Words},
			Exported: memberKindTwin{redacttest.Words},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
