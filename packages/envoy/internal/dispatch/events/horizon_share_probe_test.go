//go:build horizonprobe

package events

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// probeHorizonDB counts observations, holds observation 1 until release closes, and has
// observation number abandon wait for its runner's context to end and then send on that ended
// context: the observation fails with its runner's cancellation, so it is abandoned.
type probeHorizonDB struct {
	HorizonDB
	batches   atomic.Int32
	entered   chan struct{}
	release   chan struct{}
	abandon   int32
	abandoned chan struct{}
}

func newProbeHorizonDB(db HorizonDB, abandon int32) *probeHorizonDB {
	return &probeHorizonDB{HorizonDB: db, entered: make(chan struct{}), release: make(chan struct{}), abandon: abandon, abandoned: make(chan struct{})}
}

func (db *probeHorizonDB) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	n := db.batches.Add(1)
	switch {
	case n == 1:
		close(db.entered)
		<-db.release
	case n == db.abandon:
		close(db.abandoned)
		<-ctx.Done()
	}
	return db.HorizonDB.SendBatch(ctx, batch)
}

type probeRun struct {
	t   *testing.T
	ctx context.Context
	h   *Horizon
	db  *probeHorizonDB
}

func (p probeRun) callInto(ctx context.Context, done chan<- error) {
	go func() {
		_, err := p.h.Safe(ctx)
		done <- err
	}()
}

func (p probeRun) call(ctx context.Context) <-chan error {
	done := make(chan error, 1)
	p.callInto(ctx, done)
	return done
}

func (p probeRun) await(what string, ch <-chan struct{}) {
	p.t.Helper()
	select {
	case <-ch:
	case <-p.ctx.Done():
		p.t.Fatalf("%s: %v", what, p.ctx.Err())
	}
}

func (p probeRun) answer(which string, done <-chan error, wantErr bool) {
	p.t.Helper()
	select {
	case err := <-done:
		if (err != nil) != wantErr {
			p.t.Fatalf("the %s Safe call: err=%v, want an error: %v", which, err, wantErr)
		}
	case <-p.ctx.Done():
		p.t.Fatalf("the %s Safe call has no answer: %v", which, p.ctx.Err())
	}
}

// abandonNext makes one Safe call run the next observation and cancels that call's context once
// the observation has reached the database, so the observation is abandoned and the call fails.
func (p probeRun) abandonNext() {
	p.t.Helper()
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	done := p.call(ctx)
	p.await("the observation to abandon never reached the database", p.db.abandoned)
	cancel()
	p.answer("abandoning", done, true)
}

func (p probeRun) want(observations int32, why string) {
	p.t.Helper()
	if n := p.db.batches.Load(); n != observations {
		p.t.Fatalf("ran %d observations, want %d: %s", n, observations, why)
	}
}

func newProbe(t *testing.T, abandon int32) (probeRun, context.CancelFunc) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	db := newProbeHorizonDB(database.Pool, abandon)
	return probeRun{t: t, ctx: ctx, h: NewHorizon(db), db: db}, cancel
}

// A caller refuses observation 1 and is held after it ends. Observation 2 then runs and succeeds:
// it started after the caller's call, so the caller may use it. Observation 3 runs and is abandoned
// by its runner's context. The held caller then looks again: a fourth observation means it
// started one of its own while observation 2 was there to use.
func TestProbeParkedRefuserUsesTheNextObservationWhenANewerOneWasAbandoned(t *testing.T) {
	p, cancel := newProbe(t, 3)
	defer cancel()
	parked := make(chan struct{}, 16)
	p.h.afterObservationParked = func() { parked <- struct{}{} }
	ended := make(chan struct{}, 16)
	resume := make(chan struct{})
	release, unhold := sync.OnceFunc(func() { close(p.db.release) }), sync.OnceFunc(func() { close(resume) })
	defer release()
	defer unhold()
	var ends atomic.Int32
	p.h.afterParkedObservationEnded = func() {
		if ends.Add(1) == 1 {
			ended <- struct{}{}
			<-resume
		}
	}
	first := p.call(p.ctx)
	p.await("observation 1 never reached the database", p.db.entered)
	refuser := p.call(p.ctx)
	p.await("the refuser never parked on observation 1", parked)
	release()
	p.answer("first", first, false)
	p.await("the refuser never saw observation 1 end", ended)
	p.answer("second", p.call(p.ctx), false)
	p.abandonNext()
	unhold()
	p.answer("refusing", refuser, false)
	p.want(3, "the refused one, the one after it (which the refuser may use) and the abandoned one")
}

// The same, with the refusing caller held right after it reads its call boundary, while
// observation 1 is in flight, instead of parked on it.
func TestProbeRefuserHeldAtItsBoundaryUsesTheNextObservationWhenANewerOneWasAbandoned(t *testing.T) {
	p, cancel := newProbe(t, 3)
	defer cancel()
	atBoundary := make(chan struct{}, 1)
	resume := make(chan struct{})
	release, unhold := sync.OnceFunc(func() { close(p.db.release) }), sync.OnceFunc(func() { close(resume) })
	defer release()
	defer unhold()
	var boundaries atomic.Int32
	p.h.afterObservationBoundary = func() {
		if boundaries.Add(1) == 2 {
			atBoundary <- struct{}{}
			<-resume
		}
	}
	first := p.call(p.ctx)
	p.await("observation 1 never reached the database", p.db.entered)
	refuser := p.call(p.ctx)
	p.await("the refuser never reached its boundary", atBoundary)
	release()
	p.answer("first", first, false)
	p.answer("second", p.call(p.ctx), false)
	p.abandonNext()
	unhold()
	p.answer("refusing", refuser, false)
	p.want(3, "the refused one, the one after it (which the refuser may use) and the abandoned one")
}

// Two callers refuse observation 1 and are held after it ends. Observation 2, which started after
// both calls, is abandoned by its runner's context, so neither may return it. They then look again
// one after the other: the first runs observation 3, and the second, which looks only after
// observation 3 has ended, may use it. A fourth observation means the second started one of its own.
func TestProbeParkedRefusersShareOneObservationAfterTheirNextWasAbandoned(t *testing.T) {
	p, cancel := newProbe(t, 2)
	defer cancel()
	parked := make(chan struct{}, 16)
	p.h.afterObservationParked = func() { parked <- struct{}{} }
	ended := make(chan struct{}, 16)
	resumes := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	release := sync.OnceFunc(func() { close(p.db.release) })
	unhold1, unhold2 := sync.OnceFunc(func() { close(resumes[0]) }), sync.OnceFunc(func() { close(resumes[1]) })
	defer release()
	defer unhold1()
	defer unhold2()
	var ends atomic.Int32
	p.h.afterParkedObservationEnded = func() {
		if n := ends.Add(1); n <= 2 {
			ended <- struct{}{}
			<-resumes[n-1]
		}
	}
	first := p.call(p.ctx)
	p.await("observation 1 never reached the database", p.db.entered)
	refusers := make(chan error, 2)
	p.callInto(p.ctx, refusers)
	p.await("the first refuser never parked on observation 1", parked)
	p.callInto(p.ctx, refusers)
	p.await("the second refuser never parked on observation 1", parked)
	release()
	p.answer("first", first, false)
	p.await("a refuser never saw observation 1 end", ended)
	p.await("the other refuser never saw observation 1 end", ended)
	p.abandonNext()
	unhold1()
	p.answer("first refusing", refusers, false)
	unhold2()
	p.answer("second refusing", refusers, false)
	p.want(3, "the refused one, the abandoned one after it, and one the two refusers share")
}

// The same, with both refusing callers held right after they read their call boundaries, while
// observation 1 is in flight.
func TestProbeRefusersHeldAtTheirBoundariesShareOneObservationAfterTheirNextWasAbandoned(t *testing.T) {
	p, cancel := newProbe(t, 2)
	defer cancel()
	atBoundary := make(chan struct{}, 2)
	resumes := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	release := sync.OnceFunc(func() { close(p.db.release) })
	unhold1, unhold2 := sync.OnceFunc(func() { close(resumes[0]) }), sync.OnceFunc(func() { close(resumes[1]) })
	defer release()
	defer unhold1()
	defer unhold2()
	var boundaries atomic.Int32
	p.h.afterObservationBoundary = func() {
		if n := boundaries.Add(1); n == 2 || n == 3 {
			atBoundary <- struct{}{}
			<-resumes[n-2]
		}
	}
	first := p.call(p.ctx)
	p.await("observation 1 never reached the database", p.db.entered)
	r1 := p.call(p.ctx)
	p.await("the first refuser never reached its boundary", atBoundary)
	r2 := p.call(p.ctx)
	p.await("the second refuser never reached its boundary", atBoundary)
	release()
	p.answer("first", first, false)
	p.abandonNext()
	unhold1()
	p.answer("first refusing", r1, false)
	unhold2()
	p.answer("second refusing", r2, false)
	p.want(3, "the refused one, the abandoned one after it, and one the two refusers share")
}

// A caller reads its boundary with no observation in flight and is held there. Another caller then
// runs observation 1, which started after the held caller's call. The held caller then looks: a
// second observation means it started one of its own while observation 1 was there to use. This
// caller refused nothing.
func TestProbeCallerHeldAtAnIdleBoundaryUsesTheObservationThatStartedAfterIt(t *testing.T) {
	p, cancel := newProbe(t, 0)
	defer cancel()
	close(p.db.release)
	atBoundary := make(chan struct{}, 1)
	resume := make(chan struct{})
	unhold := sync.OnceFunc(func() { close(resume) })
	defer unhold()
	var boundaries atomic.Int32
	p.h.afterObservationBoundary = func() {
		if boundaries.Add(1) == 1 {
			atBoundary <- struct{}{}
			<-resume
		}
	}
	held := p.call(p.ctx)
	p.await("the held caller never reached its boundary", atBoundary)
	p.answer("second", p.call(p.ctx), false)
	unhold()
	p.answer("held", held, false)
	p.want(1, "observation 1, which started after the held caller's call")
}
