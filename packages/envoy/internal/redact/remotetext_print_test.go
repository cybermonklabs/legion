package redact_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTheBoxPrintsNoSecretByAnyPrinterAtAnyPlacement is the box print test of the box itself:
// redact.RemoteText holding the probe, by every printer at every placement, beside its twin, the
// probe as a plain string. The carried rows hold a box that keeps the credentials its request
// carried, which its words echo: one held with them (WithoutTokens) and one Answer.Words reads
// from each way a request carries one (keptCases); neither its words nor what it keeps may print.
func TestTheBoxPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	type row struct {
		boxed redact.RemoteText
		words string
	}
	carried := "refused the token " + redacttest.Token
	boxes := map[string]row{
		"words":      {redact.NewRemoteText(redacttest.Words), redacttest.Words},
		"fragment":   {redact.NewRemoteText(redacttest.FragmentWords), redacttest.FragmentWords},
		"unreadable": {redact.NewRemoteText(redacttest.Unreadable), redacttest.Unreadable},
		"carried":    {redact.NewRemoteText(carried).WithoutTokens(redacttest.Token), carried},
	}
	for _, c := range keptCases {
		boxes["carried-"+c.name] = row{keptAnswer(t, c.request, c.echo).Words(), c.echo}
	}
	for name, box := range boxes {
		t.Run(name, func(t *testing.T) {
			redacttest.PrintTest(t, redacttest.Holder{
				Name:     "redact.RemoteText/" + name,
				Kind:     "text",
				Boxed:    box.boxed,
				Same:     box.words,
				Exported: box.words,
			})
		})
	}
}
