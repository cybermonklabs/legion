package clocktest

import "time"

// HeldOn is Held on the wall clock wall, so a test of Held names each moment that clock reads
// rather than reading the real one.
func HeldOn(wall func() time.Time) *Fake { return &Fake{held: true, wall: wall} }
