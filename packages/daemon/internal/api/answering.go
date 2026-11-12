package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// startRefusal is what every request meets while the daemon starts: an uncoded 503, the shape and
// status the daemon's stopping refusal already has (server.stopped), so every caller that retries
// a restart retries this too and nothing a plugin parses changes.
const startRefusal = "refused: the daemon is starting and has not served its routes yet; ask again once it is up"

// startRefusalCap is how many refused launches Answering remembers. A deployment refuses one
// registration per launch in flight while it boots, which is bounded by its admission cap and the
// panes a restart finds alive; the cap is well above that, and what it bounds is the memory a
// caller that retries with a fresh boot token every time could otherwise grow without end. At the
// cap the oldest entry goes: the launch that has waited longest for its answer is the one whose
// phase has most likely ended already.
const startRefusalCap = 256

// Answering is this process's HTTP API on one listener: one *http.Server for the whole life of
// that socket, whose handler starts as the start handler — an uncoded 503 for every route — and
// becomes the daemon's own routes once it is ready (Routes).
//
// There is one server per listener because http.Server.Shutdown closes the listener it serves: a
// first server that stood down would destroy the very socket R5 exists to keep answering
// (dispatch://INFRA-2883). Every refusal between the listener and the daemon's routes stops this
// server (Stop) rather than closing the listener by hand, so a daemon that binds its own listener
// leaves a caller a refused connection, and an inherited socket its holder keeps stays listening
// for the next process.
//
// It also remembers every launch this process refused without answering it — what its start
// handler refused, and what a route refused because its own bound passed first
// (RefusedRegistration, RefusedReady) — and the moment the real routes first served
// (FirstServed): a launch the daemon could not answer is a launch it must not charge (Refused).
type Answering struct {
	listener net.Listener
	server   *http.Server
	log      *slog.Logger
	// routes is the daemon's own mux once Routes has swapped it in, and nil while the start
	// handler answers. It is read on every request and written once, so it is atomic rather than
	// under mu.
	routes atomic.Pointer[http.Handler]
	// done is closed once the serve goroutine has returned, and served is what it returned
	// (http.ErrServerClosed is no failure).
	done   chan struct{}
	served error

	mu sync.Mutex
	// refusals is one entry per launch the start handler refused, and order the keys in the order
	// they were written, oldest first, so the cap drops the oldest.
	refusals map[refusedLaunch]struct{}
	order    []refusedLaunch
	// firstServed is when Routes first swapped the daemon's routes in; zero until then.
	firstServed time.Time
}

// refusedLaunch names one launch the start handler refused. A registration carries a boot token
// and no claim, so it is remembered by that token's hash alone (supervise.HashBootToken, hex) —
// the start handler reads no store and cannot resolve the launch it belongs to. A ready carries no
// boot token, so it is remembered by the claim and generation it names.
type refusedLaunch struct {
	// hash is the hex of the registration's boot token hash, and empty for a ready.
	hash string
	// token and generation are the ready's claim and generation, and zero for a registration.
	token      claim.Token
	generation uint64
}

// NewAnswering serves listener from this moment, in a goroutine of its own, with the start
// handler. It reads no configuration, which is what lets `legion start` serve an inherited socket
// before it has read legion.yaml and the daemon serve a listener it has just bound.
func NewAnswering(listener net.Listener, log *slog.Logger) *Answering {
	if log == nil {
		log = slog.Default()
	}
	a := &Answering{
		listener: listener,
		log:      log,
		done:     make(chan struct{}),
		refusals: map[refusedLaunch]struct{}{},
	}
	a.server = &http.Server{Handler: a, ReadHeaderTimeout: readHeaderTimeout}
	go func() {
		defer close(a.done)
		if err := a.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.served = err
		}
	}()
	return a
}

// Addr is the address the listener serves on.
func (a *Answering) Addr() net.Addr { return a.listener.Addr() }

// ServeHTTP runs the daemon's routes once it has them, and the start handler until then.
func (a *Answering) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if routes := a.routes.Load(); routes != nil {
		(*routes).ServeHTTP(w, r)
		return
	}
	a.refuseStarting(w, r)
}

// Routes is the daemon ready: every request from here runs its own mux, and the moment of the
// swap is the moment this process first served a route (FirstServed).
func (a *Answering) Routes(routes http.Handler) {
	a.mu.Lock()
	if a.firstServed.IsZero() {
		a.firstServed = time.Now().UTC()
	}
	a.mu.Unlock()
	a.routes.Store(&routes)
}

// Drain stops the server, letting the requests it has run to their end, until ctx is done: the
// daemon's stop calls it, and it closes the listener as every shutdown of an http.Server does.
func (a *Answering) Drain(ctx context.Context) error {
	return a.server.Shutdown(ctx)
}

// Stop ends the server now, and with it this process's copy of the socket: every refusal between
// the listener and the daemon's routes calls it in place of closing the listener by hand, so the
// server that is serving the start handler on it stands down with it. It is harmless twice, as the
// close of a listener was.
func (a *Answering) Stop() {
	if err := a.server.Close(); err != nil {
		a.log.Debug("api: close the API server", "error", err)
	}
}

// Wait is the serve goroutine's error, once the server has stood down: nil for the ordinary close
// of a Drain or a Stop (http.ErrServerClosed), which the daemon's own stop is.
func (a *Answering) Wait() error {
	<-a.done
	if a.served != nil {
		return fmt.Errorf("serve the API on %s: %w", a.listener.Addr(), a.served)
	}
	return nil
}

// FirstServed is when the daemon's own routes first answered on this listener, and false for a
// process that never got that far: a launch whose end this daemon observed before then is a launch
// it never had the chance to answer.
func (a *Answering) FirstServed() (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.firstServed, !a.firstServed.IsZero()
}

// Refused is whether this process's start refused one launch of a claim: its registration, found
// by the hash of the boot token that launch was minted with (supervise.Claim.BootTokenHash), or
// its ready, found by the claim and generation the request named.
func (a *Answering) Refused(token claim.Token, generation uint64, bootTokenHash []byte) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(bootTokenHash) > 0 {
		if _, refused := a.refusals[refusedLaunch{hash: hex.EncodeToString(bootTokenHash)}]; refused {
			return true
		}
	}
	_, refused := a.refusals[refusedLaunch{token: token, generation: generation}]
	return refused
}

// Ended forgets what the start refused of one launch: its generation has ended, so nothing is left
// to waive. bootTokenHash is that generation's (supervise.Claim.BootTokenHash); a caller that
// holds none passes nil, and the ready's own entry still goes.
func (a *Answering) Ended(token claim.Token, generation uint64, bootTokenHash []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(bootTokenHash) > 0 {
		a.forget(refusedLaunch{hash: hex.EncodeToString(bootTokenHash)})
	}
	a.forget(refusedLaunch{token: token, generation: generation})
}

// Superseded forgets what the start refused of every earlier launch of token: generation is the
// launch the claim holds now, and it answers for the claim from here. A registration's entry is
// held by its boot token's hash alone, which names no claim, so it goes with its generation
// (Ended) or at the cap; it matches no later launch either way, since a claim's row carries only
// the hash of the launch it holds now.
func (a *Answering) Superseded(token claim.Token, generation uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, key := range slices.Clone(a.order) {
		if key.token == token && key.generation < generation {
			a.forget(key)
		}
	}
}

// RefusedRegistration remembers one registration this process refused without answering it: the
// retryable refusal a route gives a caller it could not answer inside its bound
// (server.refuseRegistrationUnanswered), beside the ones the start handler gave. It is the same
// memory and the same key — the hash of the boot token the launch was minted with — because it
// says the same thing: this launch asked and got no answer from this daemon, so it must not be
// charged for one (Refused). bootToken is the token the request carried, hashed here and never
// kept.
//
// It is called at the refusal itself, never inferred from a status: the daemon's own stopping
// refusal (server.stopped) wears the same 503 and writes nothing here, because the stop's
// relaunch on the next boot answers what it refused.
func (a *Answering) RefusedRegistration(bootToken string) {
	if bootToken == "" {
		return
	}
	a.refused(refusedLaunch{hash: hex.EncodeToString(supervise.HashBootToken(bootToken))})
}

// RefusedReady remembers one ready this process refused without answering it, by the claim and
// generation the request named: a ready carries no boot token, and this is the key the start
// handler's own memory of a ready uses (refuseStarting, Refused). RefusedRegistration states the
// rest of the rule.
func (a *Answering) RefusedReady(token claim.Token, generation uint64) {
	if token == "" || generation == 0 {
		return
	}
	a.refused(refusedLaunch{token: token, generation: generation})
}

// refuseStarting answers one request while the daemon starts, and remembers the launch it refused:
// a registration by the hash of its boot token, a ready by the claim and generation it names.
// Nothing is inferred from the status; this writes the memory where the start handler refuses, as
// the routes write it where their own bound refuses (RefusedRegistration, RefusedReady), and the
// daemon's own stopping refusal (server.stopped) writes nothing, because the stop's relaunch on
// the next boot answers what it refused.
//
// One line per refusal, at Debug: every pane's plugin retries its registration while a slow boot
// probes its image and restores its claims, and an Info line each would drown the boot's own
// stages in the log an operator reads. An operator looking into a slow boot turns Debug on.
func (a *Answering) refuseStarting(w http.ResponseWriter, r *http.Request) {
	a.remember(w, r)
	a.log.Debug("api: refused a request while the daemon starts", "method", r.Method, "route", r.URL.Path)
	writeJSON(w, http.StatusServiceUnavailable, errorBody(startRefusal))
}

// remember records the launch a refused request belongs to, for the two routes that name one. A
// body it cannot read names no launch, and is refused like any other request.
func (a *Answering) remember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		return
	}
	switch r.URL.Path {
	case "/legion/v1/claims/register":
		var request claim.RegisterRequest
		if !decodeRefused(w, r, &request) || request.BootToken == "" {
			return
		}
		a.refused(refusedLaunch{hash: hex.EncodeToString(supervise.HashBootToken(request.BootToken))})
	case "/legion/v1/claims/ready":
		var request claim.ReadyRequest
		if !decodeRefused(w, r, &request) || request.ClaimToken == "" || request.Generation == 0 {
			return
		}
		a.refused(refusedLaunch{token: request.ClaimToken, generation: request.Generation})
	}
}

// decodeRefused reads the body of a request the start refused, under the same bound every route
// reads a body under. It is lenient about the fields it does not know, as the registration route
// is: what it needs is the launch the request names.
func decodeRefused(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		return false
	}
	return json.Unmarshal(body, into) == nil
}

// refused writes one entry, dropping the oldest at the cap. A launch refused twice — a plugin that
// retries its registration — keeps the entry it already has, and its place in the order.
func (a *Answering) refused(key refusedLaunch) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, already := a.refusals[key]; already {
		return
	}
	if len(a.order) >= startRefusalCap {
		a.forget(a.order[0])
	}
	a.refusals[key] = struct{}{}
	a.order = append(a.order, key)
}

// forget drops one entry; the caller holds mu.
func (a *Answering) forget(key refusedLaunch) {
	if _, held := a.refusals[key]; !held {
		return
	}
	delete(a.refusals, key)
	a.order = slices.DeleteFunc(a.order, func(held refusedLaunch) bool { return held == key })
}
