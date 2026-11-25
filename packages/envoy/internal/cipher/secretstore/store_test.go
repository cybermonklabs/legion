package secretstore_test

import (
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// TestNotFoundPrintsTheClientsFailure: NotFound of a client's failure matches ErrNotFound and
// prints as the client worded it, and unwraps to it.
func TestNotFoundPrintsTheClientsFailure(t *testing.T) {
	worded := errors.New("DescribeSecret: the store holds no such secret")
	err := secretstore.NotFound(worded)
	if !errors.Is(err, secretstore.ErrNotFound) {
		t.Errorf("NotFound(%v) does not match ErrNotFound", worded)
	}
	if !errors.Is(err, worded) {
		t.Errorf("NotFound(%v) does not unwrap to the client's failure", worded)
	}
	if err.Error() != worded.Error() {
		t.Errorf("NotFound prints %q, want the client's %q", err, worded)
	}
}
