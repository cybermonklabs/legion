package githubapp

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/answertest"
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
// whole as a string, where answeredLimit holds the box, and its message the probe as a string.
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

// failedAnswerTwin is failedAnswer's shape had it kept the body send read as a string.
type failedAnswerTwin struct {
	method, shown string
	status        int
	detail        string
}

func (f *failedAnswerTwin) Error() string {
	return f.method + " " + f.shown + ": " + StatusText(f.status, f.detail)
}

type failedAnswerExported struct {
	Method, Shown string
	Status        int
	Detail        string
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
		// RateLimitError and failedAnswer are each built as the client builds them, from an answer
		// whose body holds the probe (answeredLimit, send), not from a text already printed.
		{
			Name:     "githubapp.RateLimitError",
			Kind:     "error",
			Boxed:    answeredLimit(http.StatusTooManyRequests, http.Header{}, answertest.Of(http.StatusTooManyRequests, body)),
			Same:     &rateLimitTwin{http.StatusTooManyRequests, time.Minute, body, redacttest.Words},
			Exported: &rateLimitExported{http.StatusTooManyRequests, time.Minute, body, redacttest.Words},
		},
		{
			Name:     "githubapp.failedAnswer",
			Kind:     "error",
			Boxed:    &failedAnswer{method: http.MethodGet, shown: "https://api.github.com/repos/o/r/pulls", status: http.StatusBadGateway, detail: answertest.Of(http.StatusBadGateway, body).Words()},
			Same:     &failedAnswerTwin{http.MethodGet, "https://api.github.com/repos/o/r/pulls", http.StatusBadGateway, body},
			Exported: &failedAnswerExported{http.MethodGet, "https://api.github.com/repos/o/r/pulls", http.StatusBadGateway, body},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
