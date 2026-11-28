package bus

import (
	"errors"
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

// LastTryError answers the error a start's last try ended with, so cmd/listener's connectListener
// judges that error alone: a start whose last tries failed otherwise names its earlier refusal
// after that error (withLastRefusal), and the refusal, which errors.Is still finds in the whole
// error, decides nothing there. An error that names no earlier refusal is the one it is, a wrapper
// around the one that names it is judged by the error it carries, and the text keeps the refusal
// for the line that logs it.
func TestLastTryErrorIsTheErrorTheLastTryEndedWith(t *testing.T) {
	refusal := fmt.Errorf("read: %w: %w", ErrPublishDenied, nats.ErrPermissionViolation)
	last := fmt.Errorf("dial: %w", nats.ErrNkeysNotSupported)
	named := withLastRefusal(last, refusal)
	atBound := credentialWaitEnded(time.Minute, refusal)
	for _, row := range []struct {
		name string
		err  error
		want error
	}{
		{name: "no error", err: nil, want: nil},
		{name: "an error that names no refusal", err: last, want: last},
		{name: "a refusal alone", err: refusal, want: refusal},
		{name: "the end at the bound", err: atBound, want: atBound},
		{name: "the last error after a refusal", err: named, want: last},
		{name: "a wrapper around it", err: fmt.Errorf("bus: %w", named), want: last},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := LastTryError(row.err); got != row.want {
				t.Fatalf("LastTryError(%v) = %v, want %v", row.err, got, row.want)
			}
		})
	}
	if !errors.Is(named, ErrPublishDenied) || errors.Is(LastTryError(named), ErrPublishDenied) {
		t.Fatalf("the earlier refusal must be found in %v and not in %v", named, LastTryError(named))
	}
	if text := named.Error(); text != last.Error()+" (last refusal of the credential: "+refusal.Error()+")" {
		t.Fatalf("text = %q, want the last error and the refusal named after it", text)
	}
}
