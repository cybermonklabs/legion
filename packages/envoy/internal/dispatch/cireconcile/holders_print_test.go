package cireconcile

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	words := redact.NewRemoteText(redacttest.Words)
	type errorTwin = struct {
		Type    string        `json:"type"`
		Path    []pathSegment `json:"path"`
		Message string        `json:"message"`
	}
	type answerTwin = struct {
		Data   struct{}    `json:"data"`
		Errors []errorTwin `json:"errors"`
	}
	entry := graphQLError{Type: words, Message: words}
	twin := errorTwin{Type: redacttest.Words, Message: redacttest.Words}
	for _, h := range []redacttest.Holder{
		{Name: "cireconcile.graphQLError", Kind: "fields", Boxed: entry, Same: twin, Exported: twin},
		{
			Name:     "cireconcile.graphQLAnswer",
			Kind:     "fields",
			Boxed:    graphQLAnswer{Errors: []graphQLError{entry}},
			Same:     answerTwin{Errors: []errorTwin{twin}},
			Exported: answerTwin{Errors: []errorTwin{twin}},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
