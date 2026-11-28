package duty

import (
	"context"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
)

// duty's tests live in package duty_test, because internal/dispatch/store imports duty - it gives
// the migration lock's key back through FreeSessionLock - and those tests open a database through
// store and storetest, which an in-package test could no longer do without an import cycle. The
// names below are what they reach for across that boundary, and nothing more: they are part of no
// package's surface but the test binary's.

const (
	// LeaseNamespace, TakeLease and FreeLease let a test hold and give back a loop's lease the
	// way Exclusive does, from a session of its own.
	LeaseNamespace = leaseNamespace
	TakeLease      = takeLease
	FreeLease      = freeLease
	// LeaseCallTimeout is the bound every statement on a lease's own connection gets.
	LeaseCallTimeout = leaseCallTimeout
	// RenewHold is a hold's renewal statement, so a test can pick it out among the statements
	// a hold sends.
	RenewHold = renewHold
)

// ErrLockNotHeld is what FreeSessionLock answers when the unlock reports the session never held
// the lock.
var ErrLockNotHeld = errLockNotHeld

// PeriodBoundary is the aligned start of the period a due time falls in.
var PeriodBoundary = periodBoundary

// JitterN points at the draw every jittered wait makes, so a test can fix it and put it back.
var JitterN = &jitterN

// ExclusiveOn lets a test set the clock for Exclusive's lease calls.
var ExclusiveOn = exclusiveOn

// RunLeasedUnderGate runs the fixed schedule on clock.Real(), with a test's gate and no lease
// pool. Inside a synctest bubble, the ticker runs on the bubble's clock.
func RunLeasedUnderGate(ctx context.Context, name string, interval time.Duration, first FirstTick, exclusive ExclusiveFunc, pass func(context.Context)) {
	runLeased(ctx, clock.Real(), nil, nil, name, interval, first, exclusive, pass)
}

// Run is WokenLoop.run, the loop with an Exclusive of the test's own in place of the real one.
func (l WokenLoop) Run(ctx context.Context, exclusive ExclusiveFunc) { l.run(ctx, exclusive) }
