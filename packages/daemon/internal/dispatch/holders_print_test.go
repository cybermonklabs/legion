package dispatch

import (
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// The box print test of this package's holders (oauth2calltest.PrintTest): each holds the probe in
// every box field, and its twins have the same shape with the probe as a string there.
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	type errorTwin = struct {
		Status  int
		Code    string
		Message string
		Said    string
	}
	type answeredEventTwin = struct {
		eventHeader
		CreatedAt string
	}
	type callerTwin = struct {
		Session string
		Project string
		Owner   *string
	}
	for _, h := range []oauth2calltest.Holder{
		{
			Name:     "dispatch.Caller",
			Kind:     "fields",
			Boxed:    Caller{Session: oauth2calltest.Said(oauth2calltest.Words), Project: oauth2calltest.Said(oauth2calltest.Words)},
			Same:     callerTwin{Session: oauth2calltest.Words, Project: oauth2calltest.Words},
			Exported: callerTwin{Session: oauth2calltest.Words, Project: oauth2calltest.Words},
		},
		{
			Name:     "dispatch.Error",
			Kind:     "fields",
			Boxed:    Error{Code: oauth2calltest.Said(oauth2calltest.Words), Said: oauth2calltest.Said(oauth2calltest.Words)},
			Same:     errorTwin{Code: oauth2calltest.Words, Said: oauth2calltest.Words},
			Exported: errorTwin{Code: oauth2calltest.Words, Said: oauth2calltest.Words},
		},
		{
			Name:     "dispatch.answeredEvent",
			Kind:     "fields",
			Boxed:    answeredEvent{CreatedAt: oauth2calltest.Said(oauth2calltest.Words)},
			Same:     answeredEventTwin{CreatedAt: oauth2calltest.Words},
			Exported: answeredEventTwin{CreatedAt: oauth2calltest.Words},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { oauth2calltest.PrintTest(t, h) })
	}
}
