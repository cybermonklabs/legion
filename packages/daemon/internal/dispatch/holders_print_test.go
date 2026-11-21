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
	for _, h := range []oauth2calltest.Holder{
		{
			Name:     "dispatch.Error",
			Kind:     "fields",
			Boxed:    Error{Code: oauth2calltest.Said(oauth2calltest.Words), Said: oauth2calltest.Said(oauth2calltest.Words)},
			Same:     errorTwin{Code: oauth2calltest.Words, Said: oauth2calltest.Words},
			Exported: errorTwin{Code: oauth2calltest.Words, Said: oauth2calltest.Words},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { oauth2calltest.PrintTest(t, h) })
	}
}
