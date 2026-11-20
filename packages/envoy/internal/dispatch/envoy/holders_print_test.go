package envoy

import (
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/listenerwire"
)

// The box print test of this package's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).

type refillingErrorTwin struct {
	RetryAfter string
	route      string
	reason     string
}

func (e *refillingErrorTwin) Error() string {
	return fmt.Sprintf("%s returned 503: %s", e.route, e.reason)
}

type refillingErrorExported struct {
	RetryAfter string
	Route      string
	Reason     string
}

func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	code, words := redact.NewRemoteText(redacttest.Code), redact.NewRemoteText(redacttest.Words)
	// The refusal Client.lapsedRoleHolder decodes, and its twin, the same type with each word a
	// string.
	refusal := listenerwire.RoleRefusalAs[redact.RemoteText, redact.RemoteText, holderState]{
		Error: words, Reason: code, Holder: code, HolderState: holderStateUnresponsive, ClientInstanceID: words,
	}
	refusalTwin := listenerwire.RoleRefusalAs[string, string, holderState]{
		Error: redacttest.Words, Reason: redacttest.Code, Holder: redacttest.Code, HolderState: holderStateUnresponsive,
		ClientInstanceID: redacttest.Words,
	}
	// The failure unavailableGetStatus, listenerResponseError and listFailure decode, and its twin,
	// the body as the listener writes it.
	failure := listenerwire.APIErrorAs[redact.RemoteText]{Code: code, Error: words, Expected: []redact.RemoteText{words}}
	failureTwin := listenerwire.APIError{Code: redacttest.Code, Error: redacttest.Words, Expected: []string{redacttest.Words}}
	for _, h := range []redacttest.Holder{
		{
			Name:     "envoy.RefillingError",
			Kind:     "error",
			Boxed:    &RefillingError{RetryAfter: "1", route: "/v1/sessions", reason: words},
			Same:     &refillingErrorTwin{"1", "/v1/sessions", redacttest.Words},
			Exported: &refillingErrorExported{"1", "/v1/sessions", redacttest.Words},
		},
		{
			Name:     "envoy.listenerwire.RoleRefusalAs",
			Kind:     "fields",
			Boxed:    refusal,
			Same:     refusalTwin,
			Exported: refusalTwin,
		},
		{
			Name:     "envoy.listenerwire.APIErrorAs",
			Kind:     "fields",
			Boxed:    failure,
			Same:     failureTwin,
			Exported: failureTwin,
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
