package main

import (
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this command's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).

type apiErrorTwin struct {
	Status   int
	code     string
	said     string
	answerID string
	withheld bool
}

func (e *apiErrorTwin) Error() string {
	return fmt.Sprintf("%s (%s) (answer %s)", e.said, e.code, e.answerID)
}

type apiErrorExported struct {
	Status   int
	Code     string
	Said     string
	AnswerID string
	Withheld bool
}

func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	code, words := redact.NewRemoteText(redacttest.Code), redact.NewRemoteText(redacttest.Words)
	answerID := redact.NewRemoteText(redacttest.FragmentWords)
	type doStatusTwin = struct {
		Code     string `json:"code"`
		Error    string `json:"error"`
		AnswerID string `json:"answer_id"`
	}
	for _, h := range []redacttest.Holder{
		{
			Name:     "agent-secrets.apiError",
			Kind:     "error",
			Boxed:    newAPIError(403, code, words, answerID, false),
			Same:     &apiErrorTwin{403, redacttest.Code, redacttest.Words, redacttest.FragmentWords, false},
			Exported: &apiErrorExported{403, redacttest.Code, redacttest.Words, redacttest.FragmentWords, false},
		},
		{
			// The envelope client.doStatus decodes a refusal into.
			Name: "agent-secrets.client.doStatus.envelope",
			Kind: "fields",
			Boxed: struct {
				Code     redact.RemoteText `json:"code"`
				Error    redact.RemoteText `json:"error"`
				AnswerID redact.RemoteText `json:"answer_id"`
			}{code, words, answerID},
			Same:     doStatusTwin{redacttest.Code, redacttest.Words, redacttest.FragmentWords},
			Exported: doStatusTwin{redacttest.Code, redacttest.Words, redacttest.FragmentWords},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
