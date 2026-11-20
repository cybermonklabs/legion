package auth

import (
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).

type callbackErrorTwin struct {
	code        string
	description string
}

func (e *callbackErrorTwin) Error() string {
	return fmt.Sprintf("sign-in callback %s: %s", e.code, e.description)
}

type callbackErrorExported struct {
	Code        string
	Description string
}

func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	for _, h := range []redacttest.Holder{
		{
			Name:     "auth.CallbackError",
			Kind:     "error",
			Boxed:    &CallbackError{code: redact.NewRemoteText(redacttest.Code), description: redact.NewRemoteText(redacttest.Words)},
			Same:     &callbackErrorTwin{redacttest.Code, redacttest.Words},
			Exported: &callbackErrorExported{redacttest.Code, redacttest.Words},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
