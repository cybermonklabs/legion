package hold

import (
	"context"
	"time"
)

// Check is the check's error, which the package's tests read by its sentinels. Every point that
// serves a credential reads Decide's Verdict instead, which reads this error in the one order.
func (g *Gate) Check(ctx context.Context, person string) error {
	_, err := g.checkUntil(ctx, person)
	return err
}

// CheckUntil is Check with the deadline of the not-held answer a served person was served on.
func (g *Gate) CheckUntil(ctx context.Context, person string) (time.Time, error) {
	return g.checkUntil(ctx, person)
}

// HeldWords is what Decide's Verdict says of person when it holds them (Verdict.HeldWords).
func (g *Gate) HeldWords(person string) string {
	return Verdict{Person: person, requireDeadline: g != nil && g.requireDeadline}.HeldWords()
}
