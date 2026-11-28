package bus

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"
)

// A clustered stream answers a publish whose message id an earlier publish still has in process
// with nats-server's code 10158 and no sequence (JSStreamDuplicateMessageConflict): the earlier
// copy is not stored yet, so nothing names the copy this publish duplicates. storedRefusal, the
// mapping sendStored makes of a failed publish on the stored path, names that answer
// ErrDuplicateInProcess, which cmd/listener's publishPastInProcess retries for the publishes that
// take it (its doc names them, and the ones that do not, the webhook routes' among them), and the
// server's own error still matches beside it. It is no ErrRefused, since the same publish sent
// again is answered once the earlier one is stored. A full stream's refusal is named ErrStreamFull
// alone, and every other answer passes through as it was. The test calls storedRefusal itself, so
// a change to the mapping sendStored makes fails it. The answer is the server's own APIError
// value; a cluster whose routes are frozen gives the code itself, which cmd/listener's in-process
// route test drives.
func TestAPublishWhoseMessageIDIsStillInProcessIsNamedErrDuplicateInProcess(t *testing.T) {
	t.Parallel()
	inProcess := &nats.APIError{Code: 409, ErrorCode: 10158, Description: "duplicate message id is in process"}
	full := &nats.APIError{Code: 503, ErrorCode: jsStreamStoreFailed, Description: maxBytesExceeded}
	for _, row := range []struct {
		name string
		err  error
		// inProcess is whether the answer is named ErrDuplicateInProcess, and full whether it is
		// named ErrStreamFull.
		inProcess, full bool
	}{
		{name: "the server's refusal of an id still in process", err: inProcess, inProcess: true},
		{name: "that refusal wrapped on its way up", err: fmt.Errorf("publish the kept copy: %w", inProcess), inProcess: true},
		{name: "a full stream's refusal", err: full, full: true},
		{name: "a write under a last sequence that moved", err: &nats.APIError{Code: 400, ErrorCode: nats.JSErrCodeStreamWrongLastSequence, Description: "wrong last sequence: 3"}},
		{name: "a refusal under the code before it", err: &nats.APIError{Code: 400, ErrorCode: 10157, Description: "pedantic mode: refused"}},
		{name: "an acknowledgement that did not come", err: nats.ErrTimeout},
	} {
		t.Run(row.name, func(t *testing.T) {
			named := storedRefusal(row.err)
			if got := errors.Is(named, ErrDuplicateInProcess); got != row.inProcess {
				t.Fatalf("%v names ErrDuplicateInProcess: %t, want %t", named, got, row.inProcess)
			}
			if got := errors.Is(named, ErrStreamFull); got != row.full {
				t.Fatalf("%v names ErrStreamFull: %t, want %t", named, got, row.full)
			}
			if !errors.Is(named, row.err) {
				t.Fatalf("%v no longer matches the error it named, %v", named, row.err)
			}
			var apiErr *nats.APIError
			if errors.As(row.err, &apiErr) {
				var still *nats.APIError
				if !errors.As(named, &still) || still.ErrorCode != apiErr.ErrorCode {
					t.Fatalf("%v no longer carries the server's *nats.APIError with code %d", named, apiErr.ErrorCode)
				}
			}
			if errors.Is(named, ErrRefused) {
				t.Fatalf("%v is an ErrRefused, which no retry passes: the same publish is answered once the earlier one is stored", named)
			}
			if !row.inProcess && !row.full && named != row.err {
				t.Fatalf("%v came back as %v, want it as it was", row.err, named)
			}
		})
	}
}
