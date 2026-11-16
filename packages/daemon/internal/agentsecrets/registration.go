package agentsecrets

import (
	"context"
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

// KeepPodSession writes r's registration now and keeps it written until ctx ends, on r.After: again
// RegistrationRenewal after each write the broker takes, and after a write that failed or that the
// broker refused for a reason a retry can change (IsPermanent false: a 5xx, a 429, an expired pod
// token, POD_REGISTRAR_NOT_LISTED, a broker out of reach, a token the pod did not hand over) on a
// backoff from registrationRetryInitial doubling to registrationRetryMax. A permanent refusal ends
// it, and so does an answer the broker made for another pod than r.PodUID (PodMismatchError): no
// later write of the same registration could be taken for that pod. Each write's outcome is logged,
// naming the pod and the session and never a token. It returns once the first write is made, with
// that write's error, having scheduled the next; the caller ends the renewal by ending ctx, which
// stops the write scheduled next.
func (c *Client) KeepPodSession(ctx context.Context, r PodSessionRenewal) error {
	k := &podSessionKeeper{client: c, r: r, ctx: ctx}
	context.AfterFunc(ctx, k.stop)
	return k.write()
}

// podSessionKeeper is one KeepPodSession's state: the write scheduled next, and how many writes
// in a row have failed. Writes never overlap: each is scheduled by the one before it, after that
// one ended.
type podSessionKeeper struct {
	client *Client
	r      PodSessionRenewal
	ctx    context.Context

	mu      sync.Mutex
	pending func() bool
	failed  int
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
	return k.client.RegisterPodSession(ctx, k.r.PodUID, token, k.r.Session)
}

// schedule arms the next write after wait, unless ctx has ended.
func (k *podSessionKeeper) schedule(wait time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ctx.Err() != nil {
		return
	}
	k.pending = k.r.After(wait, func() { _ = k.write() })
}

// stop stops the write scheduled next; ctx's end calls it.
func (k *podSessionKeeper) stop() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pending != nil {
		k.pending()
	}
}
