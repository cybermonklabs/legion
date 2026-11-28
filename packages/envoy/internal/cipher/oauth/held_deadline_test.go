package oauth_test

import (
	"context"
	"testing"
	"time"
)

// TestIntrospectionCarriesTheGatesDeadline (K11): introspection answers a live token naming a
// person whom the gate serves until a deadline with that deadline, as Unix seconds rounded down,
// so a resource server keeps the answer no later than the gate would serve it
// (oauthwire.Introspection.NotHeldUntil). A person served with no deadline, as everyone outside
// IAP, is answered with none; a held person is held and answered with none. Once the gate's clock
// reaches the deadline, ada's token is still answered active and not held, with a not_held_until
// that every reader's clock is past (it must be no later than the gate's own clock), so each reader
// refuses it as a deadline passed, not as a hold it cannot read.
//
// Controls: withHold leaving NotHeldUntil unset; ada's token is answered with no deadline, and the
// test fails naming her. withHold answering a passed deadline *oauth.HoldUncheckableError, as it
// did; the last row fails.
func TestIntrospectionCarriesTheGatesDeadline(t *testing.T) {
	d := newHoldDispatch(t, nil, person, servedBob, otherPerson)
	f := newExchangeFixture(t, d.configure)
	// The deadline is set before the tokens are minted, since each machine login's redemption asks
	// the gate and the answer it gets is kept for the client's thirty seconds; the hold comes after
	// them, since a held person's redemption mints nothing, and moves past that cache.
	deadline := d.now().Add(time.Hour).Truncate(time.Second).Add(700 * time.Millisecond)
	d.standIn.SetNotHeldUntil(person, deadline)
	adaToken := f.devBoxToken(t, newKey(t), person)
	bobToken := f.devBoxToken(t, newKey(t), servedBob)
	heldToken := f.devBoxToken(t, newKey(t), otherPerson)
	d.hold(otherPerson)
	ctx := context.Background()

	answer, err := f.provider.Introspect(ctx, adaToken)
	if err != nil || !answer.Active || answer.Held || answer.NotHeldUntil != deadline.Unix() {
		t.Fatalf("Introspect of %s's token, served until %v = %+v, %v; want active, not held, not_held_until %d (the deadline rounded down to its second)",
			person, deadline, answer, err, deadline.Unix())
	}
	answer, err = f.provider.Introspect(ctx, bobToken)
	if err != nil || !answer.Active || answer.Held || answer.NotHeldUntil != 0 {
		t.Fatalf("Introspect of %s's token, served with no deadline = %+v, %v; want active, not held, and no not_held_until", servedBob, answer, err)
	}
	answer, err = f.provider.Introspect(ctx, heldToken)
	if err != nil || !answer.Active || !answer.Held || answer.NotHeldUntil != 0 {
		t.Fatalf("Introspect of %s's token, held = %+v, %v; want active, held, and no not_held_until", otherPerson, answer, err)
	}

	d.ahead.Add(int64(time.Hour))
	answer, err = f.provider.Introspect(ctx, adaToken)
	if err != nil || !answer.Active || answer.Held || answer.NotHeldUntil <= 0 || answer.NotHeldUntil > d.now().Unix() {
		t.Fatalf("Introspect of %s's token once the gate's clock passed her deadline = %+v, %v; want active, not held, and a not_held_until in the past (at most %d)",
			person, answer, err, d.now().Unix())
	}
}
