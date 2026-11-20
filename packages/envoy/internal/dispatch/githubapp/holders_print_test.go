package githubapp

import (
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).

type statusErrorTwin struct {
	Status  int
	body    string
	message string
}

func (e *statusErrorTwin) Error() string { return fmt.Sprintf("status %d: %s", e.Status, e.body) }

type statusErrorExported struct {
	Status  int
	Body    string
	Message string
}

func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	body := `{"message":"` + redacttest.Words + `"}`
	for _, h := range []redacttest.Holder{
		{
			Name:     "githubapp.StatusError",
			Kind:     "error",
			Boxed:    &StatusError{Status: 422, body: redact.NewRemoteText(body), message: redact.NewRemoteText(redacttest.Words)},
			Same:     &statusErrorTwin{422, body, redacttest.Words},
			Exported: &statusErrorExported{422, body, redacttest.Words},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
