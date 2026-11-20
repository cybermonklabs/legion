package main

import (
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this command's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).

type apiErrorTwin struct {
	Status int
	base   brokeraddr.Address
	code   string
	reason string
}

func (e *apiErrorTwin) Error() string {
	return fmt.Sprintf("%s: %s (HTTP %d)", e.code, e.reason, e.Status)
}

type apiErrorExported struct {
	Status int
	Code   string
	Reason string
}

func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	code, words := redact.NewRemoteText(redacttest.Code), redact.NewRemoteText(redacttest.Words)
	// A clean address, beside which the refusal prints its words: the state that prints most.
	base := brokeraddr.New("http://127.0.0.1:1")
	type doTwin = struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	for _, h := range []redacttest.Holder{
		{
			Name:     "devrelay.apiError",
			Kind:     "error",
			Boxed:    &apiError{Status: 403, base: base, code: code, reason: words},
			Same:     &apiErrorTwin{403, base, redacttest.Code, redacttest.Words},
			Exported: &apiErrorExported{403, redacttest.Code, redacttest.Words},
		},
		{
			// The envelope apiClient.do decodes a refusal into.
			Name: "devrelay.apiClient.do.envelope",
			Kind: "fields",
			Boxed: struct {
				Code  redact.RemoteText `json:"code"`
				Error redact.RemoteText `json:"error"`
			}{code, words},
			Same:     doTwin{redacttest.Code, redacttest.Words},
			Exported: doTwin{redacttest.Code, redacttest.Words},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
