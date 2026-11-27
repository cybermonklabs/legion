package proxy

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	code, words := redact.NewRemoteText(redacttest.Code), redact.NewRemoteText(redacttest.Words)
	// googleError's destination, as Answer.Decode fills it: Google's error, and its status and
	// message.
	type refusalError = struct {
		Status  redact.RemoteText `json:"status"`
		Message redact.RemoteText `json:"message"`
	}
	type refusalErrorTwin = struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	type refusalTwin = struct {
		Error refusalErrorTwin `json:"error"`
	}
	for _, h := range []redacttest.Holder{
		{
			Name: "proxy.googleError.refusal",
			Kind: "fields",
			Boxed: struct {
				Error refusalError `json:"error"`
			}{refusalError{code, words}},
			Same:     refusalTwin{refusalErrorTwin{redacttest.Code, redacttest.Words}},
			Exported: refusalTwin{refusalErrorTwin{redacttest.Code, redacttest.Words}},
		},
		{
			Name:     "proxy.googleError.refusal.error",
			Kind:     "fields",
			Boxed:    refusalError{code, words},
			Same:     refusalErrorTwin{redacttest.Code, redacttest.Words},
			Exported: refusalErrorTwin{redacttest.Code, redacttest.Words},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
