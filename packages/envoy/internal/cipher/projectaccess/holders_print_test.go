package projectaccess

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holder: readPerson's answer, the one value here that holds
// the box, which holds the probe in its box field; its twins have the same shape with the probe as
// a string there (redacttest.PrintTest). The answer the client keeps of it (cachedPerson) holds
// the rule's reading and the kind of the closed set it came as, both words, so it is no holder.
// Dispatch's refusal code is dispatchread's holder (dispatchread.RefusalCode).
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	type personTwin = struct {
		MemberKind   string    `json:"member_kind"`
		Held         *bool     `json:"held"`
		AsOf         time.Time `json:"as_of"`
		NotHeldUntil time.Time `json:"not_held_until"`
	}
	for _, h := range []redacttest.Holder{
		{
			Name: "projectaccess.Client.readPerson.answer",
			Kind: "fields",
			Boxed: struct {
				MemberKind   redact.RemoteText `json:"member_kind"`
				Held         *bool             `json:"held"`
				AsOf         time.Time         `json:"as_of"`
				NotHeldUntil time.Time         `json:"not_held_until"`
			}{MemberKind: redact.NewRemoteText(redacttest.Words)},
			Same:     personTwin{MemberKind: redacttest.Words},
			Exported: personTwin{MemberKind: redacttest.Words},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
