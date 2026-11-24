package githubapp

import (
	"fmt"
	"testing"
	"time"

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

// rateLimitTwin is RateLimitError's shape had it kept GitHub's text as it came: its Body the body
// whole, where answeredLimit sets the rule's reading, and its message the probe as a string.
type rateLimitTwin struct {
	Status  int
	Wait    time.Duration
	Body    string
	message string
}

func (e *rateLimitTwin) Error() string { return withBody(limitAnswered(e.Status, e.Wait), e.Body) }

type rateLimitExported struct {
	Status  int
	Wait    time.Duration
	Body    string
	Message string
}

func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	body := `{"message":"` + redacttest.Words + `"}`
	words := redact.NewRemoteText(redacttest.Words)
	type messageTwin = struct {
		Message string `json:"message"`
	}
	type entryTwin = struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	type limitTwin = struct {
		Message string      `json:"message"`
		Errors  []entryTwin `json:"errors"`
	}
	for _, h := range []redacttest.Holder{
		{
			Name:     "githubapp.StatusError",
			Kind:     "error",
			Boxed:    &StatusError{Status: 422, body: redact.NewRemoteText(body), message: redact.NewRemoteText(redacttest.Words)},
			Same:     &statusErrorTwin{422, body, redacttest.Words},
			Exported: &statusErrorExported{422, body, redacttest.Words},
		},
		{
			Name: "githubapp.answerMessage",
			Kind: "fields",
			Boxed: struct {
				Message redact.RemoteText `json:"message"`
			}{words},
			Same:     messageTwin{redacttest.Words},
			Exported: messageTwin{redacttest.Words},
		},
		{
			Name:     "githubapp.limitWords",
			Kind:     "fields",
			Boxed:    limitWords{Message: words, Errors: []limitEntryWords{{Type: words, Message: words}}},
			Same:     limitTwin{redacttest.Words, []entryTwin{{redacttest.Words, redacttest.Words}}},
			Exported: limitTwin{redacttest.Words, []entryTwin{{redacttest.Words, redacttest.Words}}},
		},
		{
			Name:     "githubapp.limitEntryWords",
			Kind:     "fields",
			Boxed:    limitEntryWords{Type: words, Message: words},
			Same:     entryTwin{redacttest.Words, redacttest.Words},
			Exported: entryTwin{redacttest.Words, redacttest.Words},
		},
		{
			Name:     "githubapp.RateLimitError",
			Kind:     "error",
			Boxed:    &RateLimitError{Status: 429, Wait: time.Minute, Body: redact.NewRemoteText(body).String(), message: words},
			Same:     &rateLimitTwin{429, time.Minute, body, redacttest.Words},
			Exported: &rateLimitExported{429, time.Minute, body, redacttest.Words},
		},
		// failedAnswer holds the box only behind its limit pointer, which fmt prints as an address
		// and encoding/json does not read, and its Error prints the limit's Error, which prints no
		// box: it has no same-shape twin a printer reaches, so its rows use the kind's own.
		{
			Name:  "githubapp.failedAnswer",
			Kind:  "error",
			Boxed: limitNoted("GET", "https://api.github.com/repos/o/r/pulls", &RateLimitError{Status: 429, Wait: time.Minute, Body: redact.NewRemoteText(body).String(), message: words}),
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
