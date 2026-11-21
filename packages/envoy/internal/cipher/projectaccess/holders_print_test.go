package projectaccess

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	type refusalTwin = struct {
		Code string `json:"code"`
	}
	type memberKindTwin = struct {
		MemberKind string `json:"member_kind"`
	}
	for _, h := range []redacttest.Holder{
		{
			Name: "projectaccess.refusalCode",
			Kind: "fields",
			Boxed: struct {
				Code redact.RemoteText `json:"code"`
			}{redact.NewRemoteText(redacttest.Code)},
			Same:     refusalTwin{redacttest.Code},
			Exported: refusalTwin{redacttest.Code},
		},
		{
			Name: "projectaccess.Client.MemberKind.answer",
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
