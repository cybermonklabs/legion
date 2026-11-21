package daemon

import (
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// The box print test of this package's holder (oauth2calltest.PrintTest): githubText holds the
// probe in its box field, and its twin has the same shape with the probe as a string there.
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	type textTwin = struct {
		Name string
		Said string
	}
	for _, h := range []oauth2calltest.Holder{
		{
			Name:     "daemon.githubText",
			Kind:     "fields",
			Boxed:    githubText{Name: "lint", Said: oauth2calltest.Said(oauth2calltest.Words)},
			Same:     textTwin{Name: "lint", Said: oauth2calltest.Words},
			Exported: textTwin{Name: "lint", Said: oauth2calltest.Words},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { oauth2calltest.PrintTest(t, h) })
	}
}
