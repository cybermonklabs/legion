package hold_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/hold"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
)

// TestReasonNamesEachFailureInFixedWords: each kind of check that decides nothing is named in its
// own fixed words, and none of them repeats the error behind it, which for a store the broker
// could not read or write carries the database's user, name and address. The words of a not-held
// answer past its deadline say too that Dispatch may never have confirmed the person: an IAP
// Dispatch answers a person with no row as it answers one whose confirmation lapsed, so the broker
// cannot tell the two apart, and words naming only a deadline would be false of the first.
func TestReasonNamesEachFailureInFixedWords(t *testing.T) {
	store := errors.New(`failed to connect to user=broker database=broker_db host=10.0.0.7: connection refused`)
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"Dispatch refusing the broker's own account",
			fmt.Errorf("%w: %s: %w", hold.ErrUncheckable, ada, fmt.Errorf("%w: dispatch answered 403", projectaccess.ErrBrokerRefused)),
			"Dispatch refused the broker's own account"},
		{"a held answer the broker could not keep",
			errors.Join(fmt.Errorf("%w: %s: %w", hold.ErrUncheckable, ada, store), fmt.Errorf("%w: %s", hold.ErrHeld, ada)),
			"the broker could not keep Dispatch's answer that holds them"},
		{"Dispatch out of reach and the last answer unreadable",
			fmt.Errorf("%w: %s: Dispatch could not be asked (%w), and its last answer could not be read: %w", hold.ErrUncheckable, ada, errors.New("dial tcp: refused"), store),
			"the broker could not ask Dispatch, nor read the last answer Dispatch gave"},
		{"a not-held answer past its deadline, or one for a person Dispatch never confirmed",
			fmt.Errorf("%w: %s: %w, %s", hold.ErrUncheckable, ada, hold.ErrDeadlinePassed, "2026-11-16T00:05:00Z"),
			"Dispatch's answer that they are not held has passed its deadline, or Dispatch has never confirmed them"},
	} {
		got := hold.Reason(c.err)
		if got != c.want {
			t.Errorf("Reason of %s = %q, want %q", c.name, got, c.want)
		}
		if strings.Contains(got, "user=") || strings.Contains(got, "10.0.0.7") {
			t.Errorf("Reason of %s = %q, which repeats the error behind it", c.name, got)
		}
	}
}
