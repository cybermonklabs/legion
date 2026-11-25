package bus

import (
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// FinalTryRefused judges a connect by the error it ended on: a start that ended at its bound on a
// refusal (credentialWaitEnded) wraps that refusal alone, and is refused, while a start whose last
// ten tries failed otherwise names its earlier refusal after that last error (withLastRefusal), and
// is not, though errors.Is still finds the refusal there (AuthorizationRefused). A wrapper around
// either form is judged by the error it wraps, and an error that wraps several by its first.
func TestFinalTryRefusedJudgesTheErrorTheConnectEndedOn(t *testing.T) {
	refusal := fmt.Errorf("dial: %w", nats.ErrAuthorization)
	atBound := credentialWaitEnded(3*time.Minute, refusal)
	lastOther := withLastRefusal(nats.ErrMaxConnectionsExceeded, refusal)
	for _, row := range []struct {
		name    string
		err     error
		refused bool
	}{
		{name: "no error", err: nil, refused: false},
		{name: "a refusal", err: nats.ErrAuthRevoked, refused: true},
		{name: "the bound ended on a refusal", err: atBound, refused: true},
		{name: "a wrapped end at the bound", err: fmt.Errorf("connect: %w", atBound), refused: true},
		{name: "ten other failures after a refusal", err: lastOther, refused: false},
		{name: "a wrapped end after ten other failures", err: fmt.Errorf("bus: session consumers: %w", lastOther), refused: false},
		{name: "several errors, a refusal first", err: fmt.Errorf("%w (then: %w)", nats.ErrAuthExpired, nats.ErrMaxConnectionsExceeded), refused: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := FinalTryRefused(row.err); got != row.refused {
				t.Fatalf("FinalTryRefused(%v) = %t, want %t", row.err, got, row.refused)
			}
		})
	}
	if !AuthorizationRefused(lastOther) {
		t.Fatalf("AuthorizationRefused(%v) = false, want the earlier refusal found in the chain", lastOther)
	}
}
