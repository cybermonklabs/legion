// packages/envoy/cmd/cipher/client_transport_test.go
//
// What a failed call whose path holds an id a person typed prints of that id, whatever wraps the
// failure.
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
)

// TestATypedIDIsLeftOutOfAFailedCallEvenWhereALayerWrapsIt: withoutTypedID leaves the id a person
// typed out of the URL a failed call prints, whether the failure is err itself or a layer wraps
// it, so the id never prints, and the result still classifies as the failure it holds
// (errors.As, retryable). Control: the failure on its own still prints the route it was given.
func TestATypedIDIsLeftOutOfAFailedCallEvenWhereALayerWrapsIt(t *testing.T) {
	typed := "hunter2pw-" + "typedroute-" + "8a0c"
	const base = "http://127.0.0.1:1"
	const route = "/v1/operator/machines/(the machine id, not printed)/revoke"
	failure := func() *unreachableError {
		return &unreachableError{"the broker did not answer", cipheraddr.New(base), http.MethodPost, base + "/v1/operator/machines/" + typed + "/revoke", io.ErrUnexpectedEOF, ""}
	}
	for _, row := range []struct {
		name string
		err  error
	}{
		{"control: the failure itself", failure()},
		{"a failure a layer wraps", fmt.Errorf("revoke the machine login: %w", failure())},
		{"a failure two layers wrap", fmt.Errorf("machine revoke: %w", fmt.Errorf("revoke the machine login: %w", failure()))},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := withoutTypedID(row.err, route)
			wantNamedNotPrinted(t, "the error", got.Error(), []string{"POST " + base + route}, typed)
			var unreachable *unreachableError
			if !errors.As(got, &unreachable) {
				t.Errorf("withoutTypedID(%v) holds no unreachableError", row.err)
			}
			if retryable(got) != retryable(row.err) {
				t.Errorf("retryable = %v, want %v as before", retryable(got), retryable(row.err))
			}
		})
	}
}
