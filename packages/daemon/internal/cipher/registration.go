package cipher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// RegistrationRenewal is how long after a pod-session registration's last write the daemon writes
// it again. The broker honours a registration oauthstore.PodSessionRetention (24 hours) past its
// last write, and every renewal of a session's broker token is a fresh exchange that checks it, so
// a quarter of that keeps a live session's registration written with 18 hours to spare: a broker
// or registrar outage shorter than that costs the session nothing.
const RegistrationRenewal = 6 * time.Hour

// registrationRetryInitial and registrationRetryMax bound the wait after a write that failed or
// that the broker refused for a reason a retry can change: the first wait is
// registrationRetryInitial, each one after it doubles, up to registrationRetryMax. A write the
// broker takes resets it. Package-level so tests can shrink them.
var (
	registrationRetryInitial = 15 * time.Second
	registrationRetryMax     = 5 * time.Minute
)

// PodSessionRenewal is one pod's registration to trade under a session, as KeepPodSession keeps it
// written.
type PodSessionRenewal struct {
	// PodUID names the pod in the log lines. The broker reads the pod from the token each write
	// presents, never from this.
	PodUID string
	// Session is the session name the pod may trade under.
	Session string
	// Thumbprint is the RFC 7638 thumbprint of the role's own key, which the role's shim hello
	// named: every write sends it, so the registration stays bound to that key alone.
	Thumbprint string
	// Generation is the launch generation of the process the registration is for, which every
	// write sends: the broker writes a registration only at a generation at least the one it holds.
	Generation int64
	// Latest answers the claim's current generation, read when an answer comes back at a later
	// generation than r.Generation: an answered generation above it is one no process of this claim
	// reached, so the registration is one this daemon never made (SupersededError.Foreign). Nil
	// reads every later generation as a later process of this claim's.
	Latest func() int64
	// Token answers the pod token a write presents: the pod's own projected ServiceAccount token
	// for the broker's audience. It is asked once per write, so a write hours after the pod
	// started presents the token the pod holds then.
	Token func(ctx context.Context) (string, error)
	// After runs f once d has passed and answers a stop, which reports whether it kept f from
	// running: the daemon's clock.
	After func(d time.Duration, f func()) (stop func() bool)
	// Bound is the longest one write may take, its token's read included; zero leaves it to ctx
	// and the client's own HTTP timeout.
	Bound time.Duration
}

// KeepPodSession writes r's registration now and keeps it written until stop is called or ctx
// ends, on r.After: again RegistrationRenewal after each write the broker takes, and after a write
// that failed or that the broker refused for a reason a retry can change (IsPermanent false: a
// 5xx, a 408, a 429, an expired pod token, POD_REGISTRAR_NOT_LISTED, a broker out of reach, a
// token the pod did not hand over) on a backoff from registrationRetryInitial doubling to
// registrationRetryMax. Every write sends r.Thumbprint and r.Generation. A permanent refusal ends
// it, and so do an answer the broker made for another pod or key than r's (AnswerMismatchError),
// an answer at a later generation than r's (SupersededError: a later process of this claim's
// registration, or, above the claim's current generation, one this daemon never made, which the
// error's own message names), and a renewal named with no thumbprint (MissingThumbprintError): no
// later write of the same registration could be taken. Each write's outcome is logged, naming the
// pod and the session and never a token; every permanent refusal is one Error line whose error
// says which it is.
//
// It returns once the first write is made, with that write's error, having scheduled the next,
// and a stop. The stop stops the write scheduled next and returns only once a write in flight, if
// any, has been answered: it does not cancel that write, which runs under r.Bound in ctx, so a
// caller that writes the same session's next registration after the stop knows this one's last
// write has landed before it. ctx's end cancels the write in flight and stops the renewal too.
func (c *Client) KeepPodSession(ctx context.Context, r PodSessionRenewal) (stop func(), err error) {
	k := &podSessionKeeper{client: c, r: r, ctx: ctx}
	unhook := context.AfterFunc(ctx, k.halt)
	stop = func() {
		unhook()
		k.halt()
		k.flying.Wait()
	}
	return stop, k.write()
}

// podSessionKeeper is one KeepPodSession's state: the write scheduled next, how many writes in a
// row have failed, whether the renewal has been stopped, and the scheduled write in flight.
// Writes never overlap: each is scheduled by the one before it, after that one ended.
type podSessionKeeper struct {
	client *Client
	r      PodSessionRenewal
	ctx    context.Context

	mu      sync.Mutex
	pending func() bool
	stopped bool
	failed  int
	// flying counts the scheduled write in flight. A write joins it under mu only while the
	// renewal is not stopped, so once halt has set stopped no write joins, and a Wait after halt
	// waits for exactly the write that had already started.
	flying sync.WaitGroup
}

// write makes one write and schedules the next, answering the write's error. A write made or
// ended after ctx ended schedules nothing and logs nothing: the renewal is over.
func (k *podSessionKeeper) write() error {
	if err := k.ctx.Err(); err != nil {
		return err
	}
	writing, cancel := k.ctx, context.CancelFunc(func() {})
	if k.r.Bound > 0 {
		writing, cancel = context.WithTimeout(k.ctx, k.r.Bound)
	}
	expires, err := k.attempt(writing)
	cancel()
	if k.ctx.Err() != nil {
		return err
	}
	log := k.client.log().With("pod", k.r.PodUID, "session", k.r.Session)
	var wait time.Duration
	switch {
	case err == nil:
		k.failed, wait = 0, RegistrationRenewal
		log.Info("agent-secrets: registered the pod to trade under its session",
			"expires_at", expires.UTC().Format(time.RFC3339), "next_write_in", wait.String())
	case IsPermanent(err):
		var superseded *SupersededError
		if errors.As(err, &superseded) && k.r.Latest != nil {
			superseded.Latest = k.r.Latest()
			superseded.Foreign = superseded.Answered > superseded.Latest
		}
		log.Error("agent-secrets: the pod's session registration cannot be written for this pod; not writing it again", "error", err)
		return err
	default:
		k.failed++
		wait = backoff{initial: registrationRetryInitial, most: registrationRetryMax, failed: k.failed}.wait()
		log.Warn("agent-secrets: the pod's session registration was not written; writing it again", "retry_in", wait.String(), "error", err)
	}
	k.schedule(wait)
	return err
}

// attempt is one write: the pod's token, then the registration.
func (k *podSessionKeeper) attempt(ctx context.Context) (time.Time, error) {
	token, err := k.r.Token(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the pod's token: %w", err)
	}
	return k.client.RegisterPodSession(ctx, k.r.PodUID, token, k.r.Session, k.r.Thumbprint, k.r.Generation)
}

// schedule arms the next write after wait, unless the renewal is stopped or ctx has ended.
func (k *podSessionKeeper) schedule(wait time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped || k.ctx.Err() != nil {
		return
	}
	k.pending = k.r.After(wait, k.fire)
}

// fire is a scheduled write: it joins flying and writes, unless the renewal was stopped first.
func (k *podSessionKeeper) fire() {
	k.mu.Lock()
	if k.stopped {
		k.mu.Unlock()
		return
	}
	k.flying.Add(1)
	k.mu.Unlock()
	defer k.flying.Done()
	_ = k.write()
}

// halt stops the renewal: the write scheduled next never runs, and a write in flight schedules
// nothing after it.
func (k *podSessionKeeper) halt() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stopped = true
	if k.pending != nil {
		k.pending()
	}
}
