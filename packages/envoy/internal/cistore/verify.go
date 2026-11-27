package cistore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// Verify-before-announce (ENVOY_CI_VERIFY=on, WithVerify).
//
// A head's record cannot tell "no more checks" from "their webhooks never reached me": the listener
// answers GitHub 503 for a webhook it could not write, it misses every delivery made while it was
// down, and GitHub does not redeliver on its own. So with verify on, the summary loop does not
// publish a settlement the moment it holds the claim. It writes a verify request for the head
// (verifyKey) and keeps the revision that write got (verifyWait.requested). Dispatch's CI reconcile
// lists the requests (Writer.VerifyRequests), and for each one, after reading the request's
// revision, reads the head from GitHub whole, writes what it read into the record by the record's
// own rules, and then writes that revision as the record's VerifiedRev (Writer.MarkVerified). The
// loop publishes once the record shows a VerifiedRev at or past its request, the record is still
// ready to settle on the merged runs, and its claim is still exactly the one it wrote the request
// for (ClaimStillHeld, the authorization point). A run GitHub held before the request is then in
// the record, whichever listener or webhook lost it.
//
// Revisions order everything, never clocks: within one stream of the bucket a KV revision only
// grows, an expired or deleted key never gets an old one back, and a revision read before the
// request is below it. A revision compares only with revisions of its own stream: a bucket deleted
// and created again numbers from 1 again, so an answer carries the identity of the stream it read
// its request in (VerifiedStream), and an answer of another stream answers nothing. An answer for
// a claim that has gone authorizes nothing: a new claim writes a new request, whose revision no
// earlier answer reaches. Two listeners asking for one head share a read: an answer at a newer
// request's revision is past every older one.
//
// A wait ends. A head unanswered after verifyMaxAsks requests, ten minutes, is given up: the loop
// logs one ERROR naming it, deletes its request, lets the claim go and marks the record
// (VerifyStopped, VerifyStops, VerifyRetryAt). No listener claims the head again until the give-up's
// backoff runs out (verifyBackoffAfter: 5 minutes, doubled at each give-up of the same snapshot up
// to 30) or a new observation moves its snapshot, and then a new episode asks again, so a head
// given up in an outage is published once Dispatch answers, with no new observation of it
// (verifyStopped). A head given up once its last observation is the bucket's TTL old is given up
// for good. Dispatch backs off its own reads of a head it fails to read, doubling, and gives an
// episode up at once only for a failure no later read mends (cireconcile's verifyRetryAfter and
// permanentVerifyFailure).
const (
	// verifyKeyPrefix begins every verify request's key: verifyKeyPrefix, then the head's record
	// key. A record key always has four tokens (Key), so a request key has five, and no record of
	// an owner named "verify" is ever taken for one (isVerifyKey, verifyRequestKeys).
	verifyKeyPrefix = "verify"
	// verifyRequestKeys is the watch filter of every verify request and of nothing else.
	verifyRequestKeys = verifyKeyPrefix + ".*.*.*.*"

	// verifyTimeout is how long the summary loop waits for an answer before it writes the
	// request again, with a new revision that replaces the old one, and refreshes its claim. Two
	// minutes is many times what an answer takes while Dispatch serves (the reconcile lists the
	// requests every cireconcile.VerifyInterval, five seconds, and reads a head in a few
	// requests), so a request written again is one Dispatch did not answer: each rewrite logs a
	// WARN.
	verifyTimeout = 2 * time.Minute
	// verifyMaxAsks is how many requests of one settlement episode the summary loop writes
	// before it gives the episode up: five, one every verifyTimeout, is ten minutes with no
	// answer, which is Dispatch's reconcile down or a head it cannot read. The give-up logs one
	// ERROR, and the head is asked again after a backoff (verifyBackoffAfter).
	verifyMaxAsks = 5
	// verifyBackoff is how long no listener claims a head after the first give-up of its snapshot:
	// the next episode asks 15 minutes after the first request, so an outage of Dispatch, NATS or
	// GitHub that ends between the give-up and then delays the head by at most 5 minutes past
	// its end, and each give-up costs the head one episode: a claim, verifyMaxAsks requests,
	// four claim refreshes, four WARN lines, one ERROR and the stop, about a dozen KV writes.
	verifyBackoff = 5 * time.Minute
	// verifyBackoffMax is the longest backoff, reached at the fourth give-up in a row (5, 10, 20,
	// then 30 minutes): it bounds how long past the end of an outage of any length a head waits
	// to be asked again, and holds a head no one answers to one episode each 40 minutes, about
	// 36 a day, until its last observation is the bucket's TTL old (stopVerify).
	verifyBackoffMax = 30 * time.Minute
	// verifyClaimWindow is how long a claim marked Verify lives before another replica may reclaim
	// it (claimWindow): two verifyTimeouts, so a holder that refreshes its claim at each rewrite
	// keeps it while it lives, and one that died gives it up within four minutes.
	verifyClaimWindow = 2 * verifyTimeout
	// claimMargin is how much of its life a verify claim must have left for its answer to
	// authorize anything (claimLive): a publication that passed the authorization point
	// (ClaimStillHeld) is made well before any replica may reclaim the claim. Thirty seconds is
	// far longer than the read and the publish take, and far shorter than the two minutes a live
	// holder's claim always has left, since it refreshes the claim at every verifyTimeout of its
	// four-minute window.
	claimMargin = 30 * time.Second
	// authorizationRetry is how soon the summary loop looks again at a wait whose authorization
	// read or publication failed, with its claim still held: a NATS that did not answer one read
	// answers the next, or the claim runs out and the wait ends (claimLive).
	authorizationRetry = time.Second
)

// verifyKey is the key of the verify request of one head.
func verifyKey(owner, repo, number, sha string) string {
	return verifyKeyPrefix + "." + Key(owner, repo, number, sha)
}

// isVerifyKey reports whether key is a verify request's: verifyKeyPrefix and the four tokens of a
// record key. A record's key has four tokens, whatever its owner is called.
func isVerifyKey(key string) bool {
	return strings.HasPrefix(key, verifyKeyPrefix+".") && strings.Count(key, ".") == 4
}

// claimWindow is how long claim lives before another replica may reclaim it: twice the debounce,
// as every claim always had, and verifyClaimWindow for a claim that waits for a GitHub read.
func claimWindow(claim *SettlementClaim, debounce time.Duration) time.Duration {
	window := 2 * debounce
	if claim.Verify {
		window = max(window, verifyClaimWindow)
	}
	return window
}

// claimLive reports whether claim, a claim waiting for a GitHub read, has more than claimMargin of
// its life (claimWindow) left at now.
func claimLive(claim *SettlementClaim, now int64, debounce time.Duration) bool {
	return claim != nil && claim.ClaimedAt+(claimWindow(claim, debounce)-claimMargin).Milliseconds() > now
}

// verifyStopped reports whether st's head is given up at now (giveUp): the record still holds the
// snapshot the summary loop gave up (VerifyStopped), no observation having changed it since, and
// the give-up's backoff has not run out (VerifyRetryAt), or the give-up is for good (VerifyRetryAt
// zero).
func verifyStopped(st State, now int64) bool {
	return st.VerifyStopped != "" && st.VerifyStopped == st.Hash() && (st.VerifyRetryAt == 0 || now < st.VerifyRetryAt)
}

// verifyBackoffAfter is how long no listener claims a head after the stops-th give-up in a row of
// one snapshot: verifyBackoff, doubled at each give-up after the first, up to verifyBackoffMax.
func verifyBackoffAfter(stops int) time.Duration {
	backoff := verifyBackoff
	for i := 1; i < stops && backoff < verifyBackoffMax; i++ {
		backoff *= 2
	}
	return min(backoff, verifyBackoffMax)
}

// clock is the time a claim is stamped by (Store.now).
func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// verifyRequest is the value of a verify request: the head to read, under the owner and name
// GitHub gives the repository, which is what the record is keyed by, and the settlement episode it
// asks for, the generation of the claim it was written for. Kind names it, so a value read alone
// says what it is; a listener of an earlier release decodes it as a record stored under another
// key than its own, and keeps none of it (decodeState).
type verifyRequest struct {
	Kind       string `json:"kind"`
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	Number     string `json:"number"`
	SHA        string `json:"sha"`
	Generation uint64 `json:"generation"`
}

// requestVerify writes the verify request of st's head for the episode of generation and answers
// the revision the write got. It is a put, never a compare-and-swap: every write gets a revision
// past every earlier one, which is all a request needs, and a request another listener wrote for
// the head is served by the same read.
func (s *Store) requestVerify(ctx context.Context, st State, generation uint64) (uint64, error) {
	buf, err := json.Marshal(verifyRequest{Kind: verifyKeyPrefix, Owner: st.Owner, Repo: st.Repo, Number: st.Number, SHA: st.SHA, Generation: generation})
	if err != nil {
		return 0, err
	}
	kv := s.kv()
	key := verifyKey(st.Owner, st.Repo, st.Number, st.SHA)
	return kvCall(ctx, s.logger, s.nextRewatch(), func() (uint64, error) { return kv.Put(key, buf) })
}

// dropRequest deletes the verify request of the record under key where the bucket still holds it
// at revision, the summary loop's own: a request written since is another listener's, or a later
// one, and stays.
func (s *Store) dropRequest(key string, revision uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), recordWait)
	defer cancel()
	kv := s.kv()
	_, err := kvCall(ctx, s.logger, s.nextRewatch(), func() (struct{}, error) {
		return struct{}{}, kv.Delete(verifyKeyPrefix+"."+key, nats.LastRevision(revision))
	})
	if err != nil && !bus.IsCASConflict(err) && !errors.Is(err, nats.ErrKeyNotFound) {
		s.logger.Warn("checks verify request not deleted", slog.String("key", key), slog.Uint64("request", revision), slog.String("error", err.Error()))
	}
}

// refreshClaim moves the claim of generation and hash to now, so no replica reclaims a claim whose
// holder is alive and still waiting for its answer (claimWindow). It answers false when the record
// no longer carries that claim.
func (s *Store) refreshClaim(key string, generation uint64, hash string) (bool, error) {
	_, refreshed, err := s.casState(key, func(state *State) (bool, error) {
		if state.Claim == nil || state.Claim.Generation != generation || state.Claim.Hash != hash || state.Generation != generation {
			return false, nil
		}
		state.Claim.ClaimedAt = s.clock().UnixMilli()
		return true, nil
	})
	return refreshed, err
}

// stopVerify gives up, at now, the settlement episode of the claim of generation and hash: it lets
// the claim go and marks the record with the snapshot given up (VerifyStopped), how many episodes of
// that snapshot have been given up in a row (VerifyStops), and when its backoff runs out
// (VerifyRetryAt, verifyBackoffAfter), so no replica claims the head again before then unless an
// observation changes it (verifyStopped). Where the head's last observation is the store's TTL old
// at now, the give-up is for good (VerifyRetryAt zero): restarts would keep writing a record no one
// observes, which the bucket would otherwise have expired. It answers the record as written, and
// false when the record no longer carries that claim.
func (s *Store) stopVerify(key string, generation uint64, hash string, now int64) (State, bool, error) {
	return s.casState(key, func(state *State) (bool, error) {
		if state.Claim == nil || state.Claim.Generation != generation || state.Claim.Hash != hash || state.Generation != generation {
			return false, nil
		}
		state.Claim = nil
		bumpGeneration(state)
		if state.VerifyStopped == hash {
			state.VerifyStops++
		} else {
			state.VerifyStopped, state.VerifyStops = hash, 1
		}
		state.VerifyRetryAt = 0
		if s.ttl <= 0 || now-state.LastEventAt < s.ttl.Milliseconds() {
			state.VerifyRetryAt = now + verifyBackoffAfter(state.VerifyStops).Milliseconds()
		}
		return true, nil
	})
}

// streamIdentity is the identity of the stream kv's bucket is held in: its creation time in Unix
// nanoseconds, which a bucket deleted and created again does not keep (kvwatch compares streams
// the same way).
func streamIdentity(ctx context.Context, kv bus.KeyValue) (int64, error) {
	state, err := kv.ReadStreamState(ctx)
	if err != nil {
		return 0, err
	}
	return state.Created.UnixNano(), nil
}

// VerifyRequests lists every verify request the bucket holds, each with the revision it holds it
// at, the episode it asks for, and the identity of the stream it was read in: what an answer to it
// carries (MarkVerified). It reads the stream's identity, then the requests in one watch, within
// ctx and recordWait, and a watch that ends before it has delivered every request is an error.
func (w *Writer) VerifyRequests(ctx context.Context) ([]contracts.CIVerifyRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, recordWait)
	defer cancel()
	kv := w.store.kv()
	stream, err := streamIdentity(ctx, kv)
	if err != nil {
		return nil, err
	}
	watcher, err := kv.WatchKeys(ctx, verifyRequestKeys, jetstream.IgnoreDeletes())
	if err != nil {
		return nil, err
	}
	var requests []contracts.CIVerifyRequest
	for entry := range watcher.Updates() {
		if entry == nil {
			_ = watcher.Stop()
			go bus.DrainWatcher(watcher)
			return requests, nil
		}
		var request verifyRequest
		if err := json.Unmarshal(entry.Value(), &request); err != nil ||
			entry.Key() != verifyKey(request.Owner, request.Repo, request.Number, request.SHA) {
			// A value that does not name the head of its own key asks for no read.
			continue
		}
		requests = append(requests, contracts.CIVerifyRequest{
			Head:     contracts.CIHead{Owner: request.Owner, Repo: request.Repo, Number: request.Number, SHA: request.SHA},
			Revision: entry.Revision(),
			Episode:  request.Generation,
			Stream:   stream,
		})
	}
	return nil, fmt.Errorf("cistore: the watch of the verify requests ended before it delivered them all: %w", ctx.Err())
}

// errStreamChanged is MarkVerified's refusal of an answer read in a stream the bucket is no longer
// held in.
var errStreamChanged = errors.New("cistore: the CI bucket was created again since the verify request was read")

// MarkVerified writes revision, read in the stream stream (VerifyRequests), as the VerifiedRev and
// VerifiedStream of head's record: the answer to the verify request the caller read at that
// revision, once it has read the head from GitHub whole and written what it read. revision is the
// caller's, read before its GitHub read, and stays the same through every compare-and-swap the
// write retries, whatever the request's key holds by then: a request written after the caller read
// it is past revision, and this answer does not reach it.
//
// It refuses (errStreamChanged) when the bucket is held in another stream than stream: the bucket
// was deleted and created again since the request was read, and revision is of the old numbering.
// Within one stream the record's VerifiedRev only grows; an answer of another stream than the one
// the record's VerifiedStream names replaces it whatever its revision, since the two do not
// compare, and the listener takes only an answer of the stream it reads (ClaimStillHeld). A record
// the bucket no longer holds is not made again for it. It answers whether the write changed the
// record.
func (w *Writer) MarkVerified(ctx context.Context, head contracts.CIHead, revision uint64, stream int64) (bool, error) {
	read, cancel := context.WithTimeout(ctx, recordWait)
	current, err := streamIdentity(read, w.store.kv())
	cancel()
	if err != nil {
		return false, err
	}
	if current != stream {
		return false, errStreamChanged
	}
	return w.store.update(ctx, head.Owner, head.Repo, head.Number, head.SHA, "", func(st *State) bool {
		if len(st.Checks) == 0 && len(st.Suites) == 0 && len(st.Statuses) == 0 {
			// The bucket holds no record of the head (write hands a missing one over as its
			// identity alone), so no claim waits on this answer.
			return false
		}
		if st.VerifiedStream == stream && revision <= st.VerifiedRev {
			return false
		}
		st.VerifiedRev, st.VerifiedStream = revision, stream
		return true
	})
}

// DoneVerifying deletes head's verify request if the bucket still holds it at revision, the one
// the caller answered or gave up. A request written since is a newer one, which the delete leaves
// for the next read, and a request already gone is no error.
func (w *Writer) DoneVerifying(ctx context.Context, head contracts.CIHead, revision uint64) error {
	kv := w.store.kv()
	key := verifyKey(head.Owner, head.Repo, head.Number, head.SHA)
	_, err := kvCall(ctx, w.store.logger, w.store.nextRewatch(), func() (struct{}, error) {
		return struct{}{}, kv.Delete(key, nats.LastRevision(revision))
	})
	if err == nil || bus.IsCASConflict(err) || errors.Is(err, nats.ErrKeyNotFound) {
		return nil
	}
	return err
}

// verifyWait is a settlement the summary loop has claimed and asked Dispatch to verify: the claim's
// generation and snapshot, which must still be the record's claim at the authorization point; the
// revision of the request's last write, zero until a write lands; when that write was made or last
// tried; when the wait's first request was written, which says how long it has gone unanswered;
// how many requests of the episode it has written (verifyMaxAsks); and, after an authorization
// read or a publication that failed, when it looks again (authorizationRetry), zero otherwise.
type verifyWait struct {
	generation uint64
	hash       string
	requested  uint64
	askedAt    int64
	since      int64
	asks       int
	retryAt    int64
}

// due reports whether the summary loop has to look at the wait at now with nothing changed: its
// request is not written yet, a failed authorization is to be tried again, or the request has gone
// unanswered for timeout.
func (w verifyWait) due(now int64, timeout time.Duration) bool {
	return w.requested == 0 || now >= w.dueAt(timeout)
}

// dueAt is the millisecond the wait is next due with nothing changed.
func (w verifyWait) dueAt(timeout time.Duration) int64 {
	if w.requested == 0 {
		return w.askedAt + 1
	}
	at := w.askedAt + timeout.Milliseconds()
	if w.retryAt != 0 && w.retryAt < at {
		at = w.retryAt
	}
	return at
}

// claimHeldFor reports whether st carries exactly the claim wait was asked for, unsettled.
func claimHeldFor(st State, wait verifyWait) bool {
	return !st.SettledEmitted &&
		st.Generation == wait.generation &&
		st.Claim != nil &&
		st.Claim.Generation == wait.generation &&
		st.Claim.Hash == wait.hash &&
		st.Hash() == wait.hash
}

// ask writes the verify request of the claim wait stands for and keeps the wait, with the revision
// the write got as the one an answer must reach, and one more request counted. A write that fails
// keeps the wait with no revision, and the next pass writes it again; the claim holds the head
// meanwhile.
func (z *summarizer) ask(logger *logging.Logger, key string, st State, now int64, wait verifyWait) {
	ctx, cancel := context.WithTimeout(context.Background(), recordWait)
	defer cancel()
	revision, err := z.store.requestVerify(ctx, st, wait.generation)
	if err != nil {
		logger.Warn("checks verify request not written",
			slog.String("sha", st.SHA), slog.String("number", st.Number), slog.String("error", err.Error()))
		revision = 0
	} else {
		wait.asks++
	}
	wait.requested, wait.askedAt, wait.retryAt = revision, now, 0
	z.keepWait(key, wait)
}

// checkVerify looks at one wait at now. A wait whose claim the record no longer carries is
// dropped: the answer to it authorizes nothing, and the change that took the claim files the record
// afresh, whose next claim asks again. So is a wait whose claim has run out its life, or is within
// claimMargin of it (claimLive), whether or not a replica has reclaimed it yet: an answer reaches
// only a claim that is live, and the record's next claim asks again. A wait whose request is not
// written yet writes it. A wait the cached record answers -- VerifiedRev at or past its request,
// the record still ready on what the answer merged -- publishes (publish, which reads the record
// fresh once more: ClaimStillHeld); one whose authorization read or publication failed looks again
// after authorizationRetry with its claim held, and one the fresh read refuses waits on as for no
// answer. A wait unanswered for verifyTimeout refreshes its claim and writes its request again,
// with a WARN, until it has written verifyMaxAsks of them; then the episode is given up (giveUp),
// and the head is claimed again once the give-up's backoff runs out. A dropped wait deletes its own
// request (dropWait).
func (z *summarizer) checkVerify(pub Publisher, logger *logging.Logger, key string, wait verifyWait, now int64) {
	st, cached := z.store.cachedState(key)
	if !cached || !claimHeldFor(st, wait) || !claimLive(st.Claim, now, z.debounce) {
		z.dropWait(key, wait)
		return
	}
	if wait.requested == 0 {
		z.ask(logger, key, st, now, wait)
		return
	}
	if !settlementReady(st) {
		// The record overflowed under the claim, which moves no snapshot: it never settles, so
		// the claim is let go rather than asked about for ever.
		z.dropWait(key, wait)
		if _, err := z.store.ReleaseClaim(key, wait.generation); err != nil {
			logger.Warn("checks release failed", slog.String("error", err.Error()), slog.String("sha", st.SHA))
		}
		return
	}
	wait.retryAt = 0
	if st.VerifiedRev >= wait.requested {
		switch z.publish(pub, logger, key, st, wait.requested, now) {
		case publishEnded:
			z.dropWait(key, wait)
			return
		case publishUnread:
			wait.retryAt = now + authorizationRetry.Milliseconds()
			z.keepWait(key, wait)
			return
		}
		// publishRefused: the bucket answers no request of this claim, whatever the cache said
		// (an answer of another stream of the bucket); the wait goes on as for no answer.
	}
	if now < wait.dueAt(z.verifyTimeout) {
		z.keepWait(key, wait)
		return
	}
	if wait.asks >= z.verifyMaxAsks {
		z.giveUp(logger, key, st, wait, now)
		return
	}
	waited := time.Duration(now-wait.since) * time.Millisecond
	logger.Warn("checks verify request unanswered; the head is not announced until Dispatch answers",
		slog.String("owner", st.Owner),
		slog.String("repo", st.Repo),
		slog.String("number", st.Number),
		slog.String("sha", st.SHA),
		slog.Uint64("request", wait.requested),
		slog.Uint64("verified", st.VerifiedRev),
		slog.Int("requests", wait.asks),
		slog.String("waited", waited.String()))
	refreshed, err := z.store.refreshClaim(key, wait.generation, wait.hash)
	if err != nil {
		logger.Warn("checks verify claim not refreshed", slog.String("sha", st.SHA), slog.String("error", err.Error()))
		wait.askedAt = now
		z.keepWait(key, wait)
		return
	}
	if !refreshed {
		z.dropWait(key, wait)
		return
	}
	z.ask(logger, key, st, now, wait)
}

// giveUp ends the settlement episode of wait, unanswered after verifyMaxAsks requests: it lets the
// claim go and marks the record (stopVerify), deletes the wait's request, and logs one ERROR naming
// the head, why, how many episodes of the snapshot are given up in a row and when the next one
// asks. The head is claimed again once the backoff runs out, or at once when a new observation of
// it changes the record. A mark that did not land is tried again after authorizationRetry, with
// the claim held.
func (z *summarizer) giveUp(logger *logging.Logger, key string, st State, wait verifyWait, now int64) {
	stopped, marked, err := z.store.stopVerify(key, wait.generation, wait.hash, now)
	if err != nil {
		logger.Warn("checks verify give-up not written", slog.String("sha", st.SHA), slog.String("error", err.Error()))
		wait.retryAt = now + authorizationRetry.Milliseconds()
		z.keepWait(key, wait)
		return
	}
	z.dropWait(key, wait)
	if !marked {
		return
	}
	next := "at its next observation"
	if stopped.VerifyRetryAt != 0 {
		next = "in " + (time.Duration(stopped.VerifyRetryAt-now) * time.Millisecond).String() + " or at its next observation"
	}
	logger.Error("checks verify gave up; the head is not announced until Dispatch answers a later episode",
		slog.String("owner", st.Owner),
		slog.String("repo", st.Repo),
		slog.String("number", st.Number),
		slog.String("sha", st.SHA),
		slog.String("reason", fmt.Sprintf("Dispatch answered none of %d verify requests", wait.asks)),
		slog.Uint64("request", wait.requested),
		slog.Uint64("verified", st.VerifiedRev),
		slog.String("waited", (time.Duration(now-wait.since)*time.Millisecond).String()),
		slog.Int("given_up", stopped.VerifyStops),
		slog.String("next_episode", next))
}

// keepWait stores wait as key's wait.
func (z *summarizer) keepWait(key string, wait verifyWait) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.verifying[key] = wait
}

// dropWait forgets key's wait, unless a wait of another claim has taken its place, and deletes the
// wait's own request where the bucket still holds it at that revision (dropRequest): a request
// left behind would be read for nothing until its TTL.
func (z *summarizer) dropWait(key string, wait verifyWait) {
	z.mu.Lock()
	if current, held := z.verifying[key]; held && current.generation == wait.generation && current.hash == wait.hash {
		delete(z.verifying, key)
	}
	z.mu.Unlock()
	if wait.requested != 0 {
		z.store.dropRequest(key, wait.requested)
	}
}
