package githubrest

import (
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// The box print test of this package's holders (oauth2calltest.PrintTest): an entry of a GraphQL
// answer's errors list as the answer decodes it (graphQLAnswered), a step of its path
// (graphQLStep), and the list GraphQL decodes, each holding the probe in every box it reaches, and
// each twin the same shape with the probe as a string in each of those places.
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	type stepTwin struct {
		Named bool
		Name  string
		Index int
	}
	type answeredTwin struct {
		Type    string     `json:"type"`
		Message string     `json:"message"`
		Path    []stepTwin `json:"path"`
	}
	type listTwin struct {
		Errors []answeredTwin `json:"errors"`
	}
	words := oauth2calltest.Said(oauth2calltest.Words)
	step, stepT := graphQLStep{Named: true, Name: words}, stepTwin{Named: true, Name: oauth2calltest.Words}
	entry := graphQLAnswered{Type: words, Message: words, Path: []graphQLStep{step, {Index: 2}}}
	entryT := answeredTwin{Type: oauth2calltest.Words, Message: oauth2calltest.Words, Path: []stepTwin{stepT, {Index: 2}}}
	list := struct {
		Errors []graphQLAnswered `json:"errors"`
	}{Errors: []graphQLAnswered{entry}}
	listT := listTwin{Errors: []answeredTwin{entryT}}
	for _, h := range []oauth2calltest.Holder{
		{Name: "githubrest.graphQLStep", Kind: "fields", Boxed: step, Same: stepT, Exported: stepT},
		{Name: "githubrest.graphQLAnswered", Kind: "fields", Boxed: entry, Same: entryT, Exported: entryT},
		{Name: "githubrest.Client.GraphQL.answered", Kind: "fields", Boxed: list, Same: listT, Exported: listT},
	} {
		t.Run(h.Name, func(t *testing.T) { oauth2calltest.PrintTest(t, h) })
	}
}
