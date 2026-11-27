package oidc

import (
	"context"
	"sync/atomic"
)

// beforeWaitKey is the context key WithBeforeWait stores its hook under.
type beforeWaitKey struct{}

// beforeWaitHook is a hook WithBeforeWait carries, and whether it has run.
type beforeWaitHook struct {
	run func()
	ran atomic.Bool
}

// WithBeforeWait returns ctx carrying hook, which a verification under ctx runs at the point it
// has decided to wait on the issuer and before it blocks: Keys.Key where it reads the key set or
// joins a read under way (fetch, missRead, settleMiss), and LazyVerifier where it reads the
// issuer's discovery document or joins that read (discovery.discover). It runs on the goroutine
// that called Verify or Key, with no lock of this package held, at most once however many waits
// the verification makes (the discovery and then the key set), and never for a verification
// answered from memory.
//
// A caller gives up what it holds for the wait there - the Dispatch API gives back the request's
// admission unit (admission.GiveBack) - so the decision to wait and the give-back are one step:
// no clock reading, no read another caller starts or ends, comes between them. A question asked
// before the call ("would this read?") could be answered from a set still fresh, or a miss window
// still closed, and the call made a moment later then read and waited holding the unit.
func WithBeforeWait(ctx context.Context, hook func()) context.Context {
	return context.WithValue(ctx, beforeWaitKey{}, &beforeWaitHook{run: hook})
}

// beforeWait runs the hook ctx carries (WithBeforeWait), unless it has run already.
func beforeWait(ctx context.Context) {
	if hook, ok := ctx.Value(beforeWaitKey{}).(*beforeWaitHook); ok && hook.ran.CompareAndSwap(false, true) {
		hook.run()
	}
}
