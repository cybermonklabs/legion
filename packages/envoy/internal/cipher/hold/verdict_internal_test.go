package hold

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
)

// TestVerdictReadsEachCheckInTheOneOrder: every error the check answers reads as one Kind, and each
// Kind is refused as a route refuses it. A held answer the broker could not keep, which matches
// ErrHeld and ErrUncheckable at once in either order of the join, is HeldUnkept and a 503, never
// Held and a 403; a passed deadline, which matches ErrUncheckable too, is DeadlinePassed; an error
// matching no sentinel decides nothing. No refusal repeats the error behind it, which for a store
// carries the database's user, name and address.
//
// The faults: kindOf testing ErrHeld before ErrUncheckable (the unkept hold reads Held and a 403),
// or ErrUncheckable before ErrDeadlinePassed (the passed deadline reads Uncheckable); each fails a
// row by name.
func TestVerdictReadsEachCheckInTheOneOrder(t *testing.T) {
	const ada = "ada@example.com"
	stored := errors.New(`failed to connect to user=broker database=broker_db host=10.0.0.7: connection refused`)
	heldWords := ada + "'s credentials are held by Dispatch; they work again when " + ada + " signs in to Dispatch, or when Dispatch can confirm " + ada + " with Google again"
	cannot := func(reason string) string { return "whether Dispatch holds " + ada + " cannot be read: " + reason }
	for _, c := range []struct {
		name   string
		err    error
		kind   Kind
		status int
		code   string
		words  string
	}{
		{"a person the gate serves", nil, Served, 0, "", ""},
		{"a held answer the broker keeps", fmt.Errorf("%w: %s", errHeld, ada), Held, http.StatusForbidden, "MEMBERSHIP_HELD", heldWords},
		{"a held answer the broker could not keep", heldUnwritten(ada, stored), HeldUnkept,
			http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", cannot("the broker could not keep Dispatch's answer that holds them")},
		{"a held answer the broker could not keep, joined the other way round",
			errors.Join(fmt.Errorf("%w: %s", errHeld, ada), fmt.Errorf("%w: %s: %w", errUncheckable, ada, stored)), HeldUnkept,
			http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", cannot("the broker could not keep Dispatch's answer that holds them")},
		{"a not-held answer past its deadline", fmt.Errorf("%w: %s: %w, %s", errUncheckable, ada, errDeadlinePassed, "2026-11-16T00:05:00Z"), DeadlinePassed,
			http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", cannot("Dispatch's answer that they are not held has passed its deadline, or Dispatch has never confirmed them")},
		{"Dispatch refusing the broker's own account", fmt.Errorf("%w: %s: %w", errUncheckable, ada, fmt.Errorf("%w: dispatch answered 403", dispatchread.ErrBrokerRefused)), BrokerRefused,
			http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", cannot("Dispatch refused the broker's own account")},
		{"a not-held answer naming no deadline where the broker requires one", fmt.Errorf("%w: %s: %w", errUncheckable, ada, errNoDeadline), Uncheckable,
			http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", cannot("Dispatch's answer that they are not held names no deadline, which this broker requires")},
		{"no answer kept where the broker requires one", fmt.Errorf("%w: %s: %w", errUncheckable, ada, errNoAnswer), Uncheckable,
			http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", cannot("the broker could not ask Dispatch, and keeps no answer Dispatch gave for them")},
		{"an error matching no sentinel", errors.New("hold: a person's hold is checked, and none was named"), Uncheckable,
			http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", cannot("the broker could not ask Dispatch, nor read the last answer Dispatch gave")},
	} {
		kind, words := decode(c.err)
		v := Verdict{Kind: kind, Person: ada, Err: c.err, words: words}
		if v.Kind != c.kind {
			t.Errorf("the Kind of %s = %d, want %d", c.name, v.Kind, c.kind)
		}
		status, code, words := v.Refusal()
		if status != c.status || code != c.code || words != c.words {
			t.Errorf("the refusal of %s = %d %q %q, want %d %q %q", c.name, status, code, words, c.status, c.code, c.words)
		}
		if strings.Contains(words, "user=") || strings.Contains(words, "10.0.0.7") {
			t.Errorf("the refusal of %s says %q, which repeats the error behind it", c.name, words)
		}
	}
}

// TestDecideOfANilGateServes: a broker with no Dispatch to ask builds no gate, and holds nobody.
func TestDecideOfANilGateServes(t *testing.T) {
	var g *Gate
	if v := g.Decide(context.Background(), "ada@example.com"); v.Kind != Served || v.Err != nil {
		t.Fatalf("a nil gate's Decide = %+v, want Served with no error", v)
	}
}
