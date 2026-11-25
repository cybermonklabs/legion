package daemon

// How a project release stops claims and decides that each has settled: the suspensions it asks
// and the refusals the machines answer (stopClaims, suspend), the wait until each claim stands
// settled both as its machine holds it and as the store holds it (awaitSettled, stopSettled), and
// each stopped claim's line of the result as the store then holds the claim (writeClaims).

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// stopClaims asks each of tokens the project release's suspension, bounded by the release's bound,
// and waits until each is stored settled (awaitSettled) or the stop's deadline has passed, on the
// wall clock (stopDeadline). It answers each claim not settled by then with what is known of why:
// the machine's refusal, and how the claim stands in memory or in the store. A suspension held for
// a turn is the machine's to end, at the turn's end or at the bound, so a held one is waited for
// like any other.
func (r *projectReleaser) stopClaims(ctx context.Context, run *releaseRun, tokens []claim.Token, reason string) map[claim.Token]string {
	refused := r.suspend(ctx, tokens, supervise.RequestSuspend{By: supervise.SuspendedByProjectRelease, Deadline: run.bound, Reason: reason},
		newRefusals())
	return r.awaitSettled(ctx, tokens, false, r.stopDeadline(run), refused)
}

// stopDeadline is when the release stops waiting for a stop it asks now to be stored: the stop
// timeout after the later of now and the bound. The machine holds a suspension that reaches a
// claim in a turn until the turn ends or until the deadline the release names, its bound
// (holdSuspension), so a role or the controller whose turn is still allowed to run is waited for,
// and the runtime's stop of the process takes up to the stop timeout after that.
func (r *projectReleaser) stopDeadline(run *releaseRun) time.Time {
	from := time.Now()
	if run.bound.After(from) {
		from = run.bound
	}
	return from.Add(r.stopTimeout)
}

// stopWaitRanOut is how a line says that the wait for a stop ran out (stopDeadline).
func (r *projectReleaser) stopWaitRanOut() string {
	return "not stored suspended within the stop timeout of " + r.stopTimeout.String() + " after the later of its stop and the bound"
}

// refusals is where the machines' refusals of the suspensions a release asked are written as they
// come, by claim.
type refusals struct {
	mu  sync.Mutex
	why map[claim.Token]string
}

func newRefusals() *refusals { return &refusals{why: map[claim.Token]string{}} }

func (f *refusals) set(token claim.Token, why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.why[token] = why
}

func (f *refusals) of(token claim.Token) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	why, ok := f.why[token]
	return why, ok
}

// suspend asks each of tokens request's suspension, each on a goroutine of its own under a store
// mark of its own, and writes each refusal to refused as it comes: a token the daemon supervises
// no machine for at once, and a machine's refusal once it answers (requestSuspend).
func (r *projectReleaser) suspend(ctx context.Context, tokens []claim.Token, request supervise.RequestSuspend, refused *refusals) *refusals {
	for _, token := range tokens {
		m, ok := r.machines.Machine(token)
		if !ok {
			refused.set(token, releaseNoMachine)
			continue
		}
		go requestSuspend(store.ForConcurrentWorker(ctx), m, token, request, refused)
	}
	return refused
}

// requestSuspend asks m request's suspension of token and writes a refusal of it to refused. A
// suspension already held for a turn (supervise.ErrSuspendHeld) is no refusal, since the machine
// ends it itself.
func requestSuspend(ctx context.Context, m *supervise.Machine, token claim.Token, request supervise.RequestSuspend, refused *refusals) {
	request.Claim = token
	if err := m.Handle(ctx, request); err != nil && !errors.Is(err, supervise.ErrSuspendHeld) {
		refused.set(token, err.Error())
	}
}

// awaitSettled waits, without asking anything, until each of tokens the daemon supervises is
// settled as a stop byOperator asks (stopSettled) both as its machine holds it and as the store
// holds it, or until the wall clock passes until. The stored row is what decides, since a pod is
// deleted only once each of its claims is stored suspended: a suspension whose write failed
// leaves the machine's claim suspended and the row as it was (supervise's owed write). A claim
// still stopping costs no read: the store is read only once every machine holds its claim
// settled, and at the last look, and a claim the store held settled is not read again while its
// machine holds it as it stood then. While every machine holds its claim settled and a stored row
// lags, the store is read every reread rather than every poll, so a write that lands late is
// noticed up to one reread after it. It answers each claim not settled by then with why — how its
// machine holds it, and how the store holds it where the machine holds it settled, after the
// machine's refusal where refused holds one — and each token with no machine with its refusal.
func (r *projectReleaser) awaitSettled(ctx context.Context, tokens []claim.Token, byOperator bool, until time.Time,
	refused *refusals) map[claim.Token]string {
	// stored holds, for each claim the store was read holding settled, how its machine stood at
	// that reading (claimStanding): while it stands so, the row the machine owes is written.
	stored := map[claim.Token]string{}
	for {
		last := !time.Now().Before(until) || ctx.Err() != nil
		pending := map[claim.Token]string{}
		var settled []supervise.Claim
		unsettled := map[claim.Token]supervise.Claim{}
		var unsupervised []claim.Token
		for _, token := range tokens {
			m, ok := r.machines.Machine(token)
			if !ok {
				unsupervised = append(unsupervised, token)
				continue
			}
			if v := m.View().Claim; stopSettled(v, byOperator) {
				settled = append(settled, v)
			} else {
				unsettled[token] = v
			}
		}
		if len(unsettled) == 0 || last {
			for _, c := range settled {
				standing := claimStanding(c)
				if stored[c.Token] == standing {
					continue
				}
				if why, confirmed := r.storedSettled(ctx, c, byOperator); confirmed {
					stored[c.Token] = standing
				} else {
					pending[c.Token] = why
				}
			}
		}
		if (len(unsettled) > 0 || len(pending) > 0) && !last {
			wait := r.poll
			if len(unsettled) == 0 {
				wait = r.reread
			}
			select {
			case <-ctx.Done():
			case <-time.After(min(wait, time.Until(until))):
			}
			continue
		}
		for token, c := range unsettled {
			pending[token] = "it stands " + claimStanding(c)
		}
		for token, stands := range pending {
			if why, ok := refused.of(token); ok {
				pending[token] = why + "; " + stands
			}
		}
		for _, token := range unsupervised {
			if why, ok := refused.of(token); ok {
				pending[token] = why
			}
		}
		return pending
	}
}

// storedSettled is whether c, which its machine holds settled as a stop byOperator asks, is stored
// settled too (stopSettled), and how the two stand when it is not.
func (r *projectReleaser) storedSettled(ctx context.Context, c supervise.Claim, byOperator bool) (string, bool) {
	row, found, err := r.store.ClaimByToken(ctx, c.Token)
	if err == nil && found && stopSettled(row, byOperator) {
		return "", true
	}
	stands := "it stands " + claimStanding(c)
	switch {
	case err != nil:
		return stands + ", and its stored record could not be read: " + err.Error(), false
	case !found:
		return stands + ", and the store holds no record of it", false
	}
	return stands + ", and the store holds it " + claimStanding(row), false
}

// claimStanding is how c stands, as a line names it: its state, and the source of its suspension
// when it is suspended.
func claimStanding(c supervise.Claim) string {
	if c.State == supervise.StateSuspended && c.SuspendedBy != "" {
		return string(c.State) + " by " + string(c.SuspendedBy)
	}
	return string(c.State)
}

// stopSettled is whether c stands as a stop asked leaves it, read of the machine's claim and of the
// stored row alike (awaitSettled). Suspended is settled: by an operator for an operator's hold,
// and by any source for the release's own, whose take-over of an idle release's is asked after
// (stopAndDelete). A claim that failed or retired is settled too: no stop changes it, it holds no
// process, so no deletion of its pod reads as its death, and the boot starts neither, so its line
// names how it ended.
func stopSettled(c supervise.Claim, byOperator bool) bool {
	switch c.State {
	case supervise.StateFailed, supervise.StateRetired:
		return true
	case supervise.StateSuspended:
		return !byOperator || c.SuspendedBy == supervise.SuspendedByOperator
	}
	return false
}

// releaseStopTokens is the tokens of claims, in their order.
func releaseStopTokens(claims []store.ProjectReleaseClaim) []claim.Token {
	tokens := make([]claim.Token, len(claims))
	for i, c := range claims {
		tokens[i] = c.Token
	}
	return tokens
}

// pendingWords is pending as one sentence, claim by claim in token order.
func pendingWords(pending map[claim.Token]string) string {
	tokens := slices.Sorted(maps.Keys(pending))
	words := make([]string, len(tokens))
	for i, token := range tokens {
		words[i] = string(token) + " (" + pending[token] + ")"
	}
	return strings.Join(words, ", ")
}

// writeClaims writes each stop-set claim's line from the claim's own record as the store holds it
// now: how it ended is its suspension's source when it is suspended, and its state otherwise, and
// only a claim held `project_release` is one the next boot starts. A claim the store holds no
// record of is api.ReleasedClaimUnrecorded. pending, and the run's detail, are why a claim was not
// stored suspended in time. A read of the store that fails is made again, retry later, up to
// projectReleaseTries reads in all (tryBounded), since one that failed would leave every line's
// end unwritten for the whole release; a store that cannot be read at any of them leaves each
// line's end unwritten, saying so, rather than naming one the release did not read. A line's
// detail is kept as the store keeps a far side's error text (storetext.FarSideError): it carries
// a machine's refusal of the stop, the store's answer to a read of the claim, or the read of the
// claims that failed here, whose text is cut once for every line.
func (r *projectReleaser) writeClaims(ctx context.Context, run *releaseRun, pending map[claim.Token]string) {
	var claims []supervise.Claim
	err := r.tryBounded(ctx, r.retry, "project release: read the claims it stopped; reading them again", func() error {
		var err error
		claims, err = r.store.Claims(ctx)
		return err
	}, "release", run.id)
	var unread string
	if err != nil {
		r.log.Error("project release: read the claims it stopped", "release", run.id, "error", err)
		unread = storetext.FarSideError("how it stands is not known: the release could not read the claims at its end: " + err.Error())
	}
	stored := map[claim.Token]supervise.Claim{}
	for _, c := range claims {
		stored[c.Token] = c
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	for i := range run.result.Claims {
		line := &run.result.Claims[i]
		if err != nil {
			line.Detail, line.Ended, line.BootStarts = unread, "", false
			continue
		}
		detail, explained := run.detail[line.Token]
		if why, ok := pending[line.Token]; ok {
			detail, explained = why, true
		}
		if explained {
			line.Detail = storetext.FarSideError(detail)
		}
		c, ok := stored[line.Token]
		if !ok {
			line.Ended, line.BootStarts = api.ReleasedClaimUnrecorded, false
			continue
		}
		line.Ended = api.ReleasedClaimEndOf(c)
		line.BootStarts = c.ProjectReleased()
	}
}

// podlessStops is every claim of stop on an issue that has no pod among pods, the controller's
// left out, which the release stops after the issue pods.
func podlessStops(stop []store.ProjectReleaseClaim, pods []*releasePod) []claim.Token {
	withPod := map[string]bool{}
	for _, pod := range pods {
		withPod[pod.sandbox.Issue] = true
	}
	var podless []claim.Token
	for _, c := range stop {
		if c.Issue != "" && !withPod[c.Issue] {
			podless = append(podless, c.Token)
		}
	}
	return podless
}
