package wakepace

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// paceClock is the pacer tests' clock. It moves only when a test moves it, and fires the timers
// that fall due on the way in the order of their moments, then of their arming, on the moving
// goroutine and outside its own lock, since a timer's callback is a pacer's turn, which reads the
// clock and arms the next timer.
type paceClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*paceTimer
	armed  int
}

type paceTimer struct {
	at    time.Time
	order int
	f     func()
	fired bool
}

func newPaceClock() *paceClock { return &paceClock{now: origin} }

func (c *paceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// At arms f for at. A moment the clock has passed fires at the clock's own time on the next move,
// which is the pacer's "at once".
func (c *paceClock) At(at time.Time, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed++
	c.timers = append(c.timers, &paceTimer{at: at, order: c.armed, f: f})
}

// advance moves the clock d forward, firing each timer that falls due at its own moment.
func (c *paceClock) advance(d time.Duration) { c.move(d, false) }

// late moves the clock d forward first and then fires what fell due, at the new time: a timer
// goroutine that ran late, which is how a turn comes to find the bucket refilled.
func (c *paceClock) late(d time.Duration) { c.move(d, true) }

func (c *paceClock) move(d time.Duration, late bool) {
	c.mu.Lock()
	until := c.now.Add(d)
	if late {
		c.now = until
	}
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *paceTimer
		for _, timer := range c.timers {
			if timer.fired || timer.at.After(until) {
				continue
			}
			if next == nil || timer.at.Before(next.at) || (timer.at.Equal(next.at) && timer.order < next.order) {
				next = timer
			}
		}
		if next == nil {
			c.now = until
			c.mu.Unlock()
			return
		}
		next.fired = true
		if next.at.After(c.now) {
			c.now = next.at
		}
		c.mu.Unlock()
		next.f()
	}
}

// waitingTimers is how many timers are armed and have not fired: the pacer stops none.
func (c *paceClock) waitingTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, timer := range c.timers {
		if !timer.fired {
			n++
		}
	}
	return n
}

// newPacer builds a pacer of perMinute turns a minute in bursts of burst, with no jitter, so that
// every turn falls on an exact interval, over a clock standing at origin.
func newPacer(perMinute, burst int) (*Pacer, *paceClock) {
	clock := newPaceClock()
	return NewPacer(New(perMinute, burst, nil), clock.Now, clock.At), clock
}

// turnAt is the moment k intervals after origin: the k-th turn of a pacer busy from origin on.
func turnAt(k int) time.Time { return origin.Add(time.Duration(k) * interval) }

func message(i int) string { return fmt.Sprintf("message %d", i) }

func other(i int) string { return fmt.Sprintf("other %d", i) }

// queueOf is the queue a test's wake of that name joined: First for a message, Second otherwise.
func queueOf(name string) Queue {
	if strings.HasPrefix(name, "message") {
		return First
	}
	return Second
}

// entry is one let-out a test saw: the wake's name and its Let.
type entry struct {
	name string
	let  Let
}

// out is the let-out of name from q at the k-th turn after origin, turns turns after its Join.
func out(name string, q Queue, k, turns int) entry {
	return entry{name, Let{At: turnAt(k), Queue: q, Turns: turns}}
}

func (e entry) String() string {
	return fmt.Sprintf("%s out of %s at +%s after %d turns", e.name, e.let.Queue, e.let.At.Sub(origin), e.let.Turns)
}

func sameLet(a, b Let) bool { return a.At.Equal(b.At) && a.Queue == b.Queue && a.Turns == b.Turns }

// letLog keeps every let-out a test's places report, in the order they came, whether at the Join
// or through a release.
type letLog struct {
	mu  sync.Mutex
	out []entry
}

func (g *letLog) add(name string, let Let) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.out = append(g.out, entry{name, let})
}

// join takes a place in q for the wake name and binds it to the log.
func (g *letLog) join(p *Pacer, q Queue, name string) *Place {
	pl := p.Join(q)
	if let, ok := pl.Bind(func(let Let) { g.add(name, let) }); ok {
		g.add(name, let)
	}
	return pl
}

func (g *letLog) entries() []entry {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.out)
}

// check fails the test unless the log holds exactly want, in order.
func (g *letLog) check(t *testing.T, want []entry) {
	t.Helper()
	got := g.entries()
	for i := range max(len(got), len(want)) {
		switch {
		case i >= len(got):
			t.Fatalf("let-out %d: none, want %s (%d let-outs, want %d)", i, want[i], len(got), len(want))
		case i >= len(want):
			t.Fatalf("let-out %d: %s, want none (%d let-outs, want %d)", i, got[i], len(got), len(want))
		case got[i].name != want[i].name || !sameLet(got[i].let, want[i].let):
			t.Fatalf("let-out %d: %s, want %s", i, got[i], want[i])
		}
	}
}

// A wake on an idle pacer runs at once and costs the bucket its token, so the wake after it waits
// one turn. A pacer that queued every wake behind a timer, or armed one for a wake it let out,
// fails here.
func TestAPlaceOnAnIdlePacerIsLetOutAtOnce(t *testing.T) {
	p, clock := newPacer(rate, 1)
	pl := p.Join(Second)
	want := Let{At: origin, Queue: Second}
	if let, ok := pl.Let(); !ok || !sameLet(let, want) {
		t.Fatalf("Let() of the first place on an idle pacer = %s, %t, want %s, true", entry{"it", let}, ok, entry{"it", want})
	}
	let, ok := pl.Bind(func(Let) { t.Error("the release of a place let out at its Join was called") })
	if !ok || !sameLet(let, want) {
		t.Fatalf("Bind of a place let out at its Join = %s, %t, want %s, true", entry{"it", let}, ok, entry{"it", want})
	}
	if at := pl.Estimate(); !at.Equal(origin) {
		t.Errorf("Estimate of a place let out at %s = %s", origin, at)
	}
	if n := clock.waitingTimers(); n != 0 {
		t.Fatalf("the pacer armed %d timers for a place it let out at once, want none", n)
	}

	next := p.Join(First)
	if let, ok := next.Let(); ok {
		t.Fatalf("the second place was let out at once (%s), want it to wait a turn", entry{"it", let})
	}
	clock.advance(interval)
	want = Let{At: turnAt(1), Queue: First, Turns: 1}
	if let, ok := next.Let(); !ok || !sameLet(let, want) {
		t.Fatalf("Let() of the second place = %s, %t, want %s, true", entry{"it", let}, ok, entry{"it", want})
	}
}

// A message wake raised behind twenty waiting others runs at the next turn, and the twenty run
// after it in their order, one turn each. A pacer of one queue, the one this package had, lets the
// message wake out last.
func TestAMessagePlaceGoesAheadOfTwentyWaitingOthers(t *testing.T) {
	p, clock := newPacer(rate, 1)
	var log letLog
	// The first place takes the idle bucket's token, so that the twenty wait.
	log.join(p, Second, "spent")
	for i := range 20 {
		log.join(p, Second, other(i))
	}
	log.join(p, First, message(0))
	clock.advance(21 * interval)

	want := []entry{out("spent", Second, 0, 0), out(message(0), First, 1, 1)}
	for i := range 20 {
		want = append(want, out(other(i), Second, i+2, i+2))
	}
	log.check(t, want)
}

// Eight message wakes behind twenty others run first in their order, the turn after each three of
// them going to the next other, and every wake runs one interval after the one before: the queues
// move wakes ahead and never add a turn. A strict priority runs the eight before any other, and a
// share of one in two runs an other after every second message.
func TestMessagePlacesKeepTheirOrderTheShareAndTheRate(t *testing.T) {
	p, clock := newPacer(rate, 1)
	var log letLog
	log.join(p, Second, "spent")
	for i := range 20 {
		log.join(p, Second, other(i))
	}
	for i := range 8 {
		log.join(p, First, message(i))
	}
	clock.advance(28 * interval)

	order := []string{
		message(0), message(1), message(2), other(0),
		message(3), message(4), message(5), other(1),
		message(6), message(7),
	}
	for i := 2; i < 20; i++ {
		order = append(order, other(i))
	}
	want := []entry{out("spent", Second, 0, 0)}
	for k, name := range order {
		want = append(want, out(name, queueOf(name), k+1, k+1))
	}
	log.check(t, want)

	got := log.entries()
	for i := 1; i < len(got); i++ {
		if gap := got[i].let.At.Sub(got[i-1].let.At); gap < interval {
			t.Errorf("%s came %s after the let-out before it, want at least %s", got[i], gap, interval)
		}
	}
	// Every message wake keeps the pacer's bound: with k message wakes ahead of it, it runs within
	// k+2+floor(k/3) turns of its Join.
	for _, e := range got {
		var k int
		if _, err := fmt.Sscanf(e.name, "message %d", &k); err != nil {
			continue
		}
		if bound := k + 2 + k/3; e.let.Turns > bound {
			t.Errorf("%s, want within %d turns", e, bound)
		}
	}
}

// When the other queue fills after ten message turns, it gets one turn and then waits out three
// message turns again: the run the share counted while it was empty buys it its next turn, and
// nothing more is owed. A share that built credit would give the others four turns in a row, and
// one that counted only while both queues waited would run three messages first.
func TestAnOtherQueueThatFillsAfterAMessageRunGetsOneTurnNotARow(t *testing.T) {
	p, clock := newPacer(rate, 1)
	var log letLog
	for i := range 20 {
		log.join(p, First, message(i))
	}
	clock.advance(10 * interval)
	for i := range 4 {
		log.join(p, Second, other(i))
	}
	clock.advance(13 * interval)

	want := []entry{out(message(0), First, 0, 0)}
	for i := 1; i <= 10; i++ {
		want = append(want, out(message(i), First, i, i))
	}
	order := []string{
		other(0), message(11), message(12), message(13),
		other(1), message(14), message(15), message(16),
		other(2), message(17), message(18), message(19),
		other(3),
	}
	for k, name := range order {
		// The messages joined at origin, the others ten turns later.
		turn, turns := 11+k, 11+k
		if queueOf(name) == Second {
			turns -= 10
		}
		want = append(want, out(name, queueOf(name), turn, turns))
	}
	log.check(t, want)
}

// A wake that leaves gives its turn to the next head and costs the bucket nothing, so the wakes
// after it keep one interval between them; the turn it left stands for a place that joins before
// it comes, and a turn nobody is left to take lets nothing out and is spent. A pacer that charged a
// token for each Join spaces the rest further apart, one that dropped a left turn makes the next
// place reserve another, and one that banked a spent turn lets the next place out at once.
func TestLeaveGivesItsTurnToTheNextHead(t *testing.T) {
	p, clock := newPacer(rate, 1)
	var log letLog
	log.join(p, Second, "spent")
	a := log.join(p, Second, "a")
	b := log.join(p, Second, "b")
	c := log.join(p, Second, "c")
	log.join(p, Second, "d")
	a.Leave() // The head, whose turn is pending.
	c.Leave()
	clock.advance(2 * interval)
	log.check(t, []entry{out("spent", Second, 0, 0), out("b", Second, 1, 1), out("d", Second, 2, 2)})

	for name, pl := range map[string]*Place{"a": a, "c": c} {
		pl.Leave()
		if let, ok := pl.Let(); ok {
			t.Errorf("Let() of %s, which left, = %s, true", name, entry{name, let})
		}
		if _, ok := pl.Bind(func(Let) { t.Errorf("the release of %s, which left, was called", name) }); ok {
			t.Errorf("Bind of %s, which left, = true", name)
		}
		if pl.Mark() {
			t.Errorf("Mark of %s, which left, = true", name)
		}
		if at := pl.Estimate(); !at.IsZero() {
			t.Errorf("Estimate of %s, which left, = %s, want the zero time", name, at)
		}
	}
	b.Leave()
	if _, ok := b.Let(); !ok {
		t.Fatalf("a Leave of b, let out already, undid its let-out")
	}

	// The turn e leaves stands for f, which joins before it comes.
	e := log.join(p, Second, "e")
	e.Leave()
	clock.advance(interval / 2)
	log.join(p, Second, "f")
	clock.advance(interval / 2)
	// The turn g leaves comes with nobody to take it, and h, joined after it, waits for the next.
	g := log.join(p, Second, "g")
	g.Leave()
	clock.advance(interval)
	log.join(p, Second, "h")
	clock.advance(interval)
	log.check(t, []entry{
		out("spent", Second, 0, 0), out("b", Second, 1, 1), out("d", Second, 2, 2),
		out("f", Second, 3, 1), out("h", Second, 5, 1),
	})
}

// A mark moves a waiting other to the back of the message queue, behind the messages already
// there and ahead of the others it waited with; it moves nothing else. A Mark that put the place
// at the front would run it before the messages, and one that left it where it was would run it
// last.
func TestMarkMovesAnOtherPlaceToTheBackOfTheMessageQueue(t *testing.T) {
	p, clock := newPacer(rate, 1)
	var log letLog
	log.join(p, Second, "spent")
	log.join(p, Second, other(0))
	log.join(p, Second, other(1))
	marked := log.join(p, Second, other(2))
	first := log.join(p, First, message(0))
	log.join(p, First, message(1))

	if !marked.Mark() {
		t.Fatalf("Mark of %s, waiting in %s, = false", other(2), Second)
	}
	if q := marked.Queue(); q != First {
		t.Fatalf("the marked place waits in %s, want %s", q, First)
	}
	if marked.Mark() {
		t.Errorf("a second Mark of %s = true, want false", other(2))
	}
	if first.Mark() {
		t.Errorf("Mark of %s, waiting in %s, = true, want false", message(0), First)
	}
	clock.advance(5 * interval)
	log.check(t, []entry{
		out("spent", Second, 0, 0),
		out(message(0), First, 1, 1), out(message(1), First, 2, 2), out(other(2), First, 3, 3),
		out(other(0), Second, 4, 4), out(other(1), Second, 5, 5),
	})
	if marked.Mark() {
		t.Errorf("Mark of %s, let out, = true, want false", other(2))
	}
}

// What Estimate says of each waiting place is the moment it is let out when nothing changes, and
// Before orders places as those moments do, from the start of a run and halfway through it. The
// first place is a message let out at once, so the share starts one message into its run: an
// estimate that read a place's own queue alone, ran a strict priority or forgot the run would be
// off for some of these places.
func TestEstimateIsTheMomentAWaitingPlaceIsLetOut(t *testing.T) {
	p, clock := newPacer(rate, 1)
	var log letLog
	log.join(p, First, "spent message")
	var names []string
	places := map[string]*Place{}
	messages, others := 0, 0
	for i := range 28 {
		name := other(others)
		if i%3 == 1 && messages < 8 {
			name, messages = message(messages), messages+1
		} else {
			others++
		}
		names = append(names, name)
		places[name] = log.join(p, queueOf(name), name)
	}

	estimates := map[string]time.Time{}
	for _, name := range names {
		estimates[name] = places[name].Estimate()
	}
	for _, a := range names {
		for _, b := range names {
			if got, want := places[a].Before(places[b]), estimates[a].Before(estimates[b]); got != want {
				t.Fatalf("%s.Before(%s) = %t, want %t from their estimates %s and %s",
					a, b, got, want, estimates[a].Sub(origin), estimates[b].Sub(origin))
			}
		}
	}

	clock.advance(5 * interval)
	for _, name := range names {
		pl := places[name]
		if _, ok := pl.Let(); ok {
			continue
		}
		if at := pl.Estimate(); !at.Equal(estimates[name]) {
			t.Errorf("halfway, Estimate of %s = +%s, want +%s as at the start", name, at.Sub(origin), estimates[name].Sub(origin))
		}
	}

	clock.advance(23 * interval)
	got := log.entries()
	if len(got) != 29 {
		t.Fatalf("%d let-outs, want 29", len(got))
	}
	for _, e := range got[1:] {
		if !e.let.At.Equal(estimates[e.name]) {
			t.Errorf("%s, estimated at +%s", e, estimates[e.name].Sub(origin))
		}
	}
}

// A place let out before its wake was bound — the caller took its place and the turn came first —
// reports its let-out to the Bind, and the release is never called. A pacer that held an unbound
// place back, or let it out into nothing, loses the wake.
func TestBindAfterTheLetOutReturnsTheLet(t *testing.T) {
	p, clock := newPacer(rate, 1)
	p.Join(Second)
	pl := p.Join(Second)
	clock.advance(interval)

	want := Let{At: turnAt(1), Queue: Second, Turns: 1}
	let, ok := pl.Bind(func(Let) { t.Error("the release of a place bound after its let-out was called") })
	if !ok || !sameLet(let, want) {
		t.Fatalf("Bind after the let-out = %s, %t, want %s, true", entry{"it", let}, ok, entry{"it", want})
	}
	if let, ok := pl.Let(); !ok || !sameLet(let, want) {
		t.Fatalf("Let() after the let-out = %s, %t, want %s, true", entry{"it", let}, ok, entry{"it", want})
	}
}

// A machine that detaches a waiting wake and takes it up again binds its place a second time: the
// newer release is the one called, once, and the earlier one never is. A place that left is never
// released, whatever is bound to it. A pacer that kept the first release, or called both, fails.
func TestBindAgainReplacesTheRelease(t *testing.T) {
	p, clock := newPacer(rate, 1)
	p.Join(Second)
	pl := p.Join(Second)
	if _, ok := pl.Bind(func(Let) { t.Error("the release a second Bind replaced was called") }); ok {
		t.Fatalf("the first Bind of a waiting place = true")
	}
	var got []Let
	if _, ok := pl.Bind(func(let Let) { got = append(got, let) }); ok {
		t.Fatalf("the second Bind of a waiting place = true")
	}
	clock.advance(interval)
	want := Let{At: turnAt(1), Queue: Second, Turns: 1}
	if len(got) != 1 || !sameLet(got[0], want) {
		t.Fatalf("the second release was called with %v, want once with %s", got, entry{"it", want})
	}

	left := p.Join(Second)
	left.Leave()
	if _, ok := left.Bind(func(Let) { t.Error("the release bound after a Leave was called") }); ok {
		t.Fatalf("Bind after a Leave = true")
	}
	clock.advance(interval)
}

// A burst of two lets two wakes out at once; from the third on the pace holds, one interval
// between one let-out and the next. A pacer that let every place out while the bucket said now
// for the first, or that queued even the burst, fails.
func TestABurstOfTwoLetsTwoOutAtOnceAndThenPaces(t *testing.T) {
	p, clock := newPacer(rate, 2)
	var log letLog
	for i := range 5 {
		log.join(p, Second, other(i))
	}
	clock.advance(3 * interval)
	log.check(t, []entry{
		out(other(0), Second, 0, 0), out(other(1), Second, 0, 0),
		out(other(2), Second, 1, 1), out(other(3), Second, 2, 2), out(other(4), Second, 3, 3),
	})
}

// A turn whose timer fires late finds the bucket refilled and lets out a place for each token it
// holds at the moment it fired, each at a turn of its own on a timer armed for that moment, and
// the pace holds after them. A pacer that let one place out a timer would leave the refilled
// tokens unspent, and one that let out every waiting place at once would break the rate.
func TestALateTurnLetsOutOnePlaceForEachTokenTheBucketRefilled(t *testing.T) {
	p, clock := newPacer(rate, 2)
	var log letLog
	for i := range 6 {
		log.join(p, Second, other(i))
	}
	clock.late(5 * interval)
	clock.advance(interval)
	log.check(t, []entry{
		out(other(0), Second, 0, 0), out(other(1), Second, 0, 0),
		out(other(2), Second, 5, 1), out(other(3), Second, 5, 2), out(other(4), Second, 5, 3),
		out(other(5), Second, 6, 4),
	})
}

// The places a late turn lets out are released each on a timer of its own, so the release of one,
// which for a machine is its whole launch decision, holds back none of the others: here a's
// release waits until b's has run. The clock runs each timer on a goroutine of its own, as
// time.AfterFunc does, and one armed for a moment that has come at once. A pacer that called
// every release of a late turn one after another on that turn's goroutine never releases b, and
// the test reports it after ten seconds rather than hang.
func TestALateTurnReleasesEachPlaceOnATimerOfItsOwn(t *testing.T) {
	var mu sync.Mutex
	now := origin
	var armed []func()
	at := func(at time.Time, f func()) {
		mu.Lock()
		defer mu.Unlock()
		if at.After(now) {
			armed = append(armed, f)
			return
		}
		go f()
	}
	p := NewPacer(New(rate, 1, nil), func() time.Time { mu.Lock(); defer mu.Unlock(); return now }, at)
	p.Join(Second) // the burst's one token lets it out at its join
	a, b := p.Join(Second), p.Join(Second)
	bReleased, aReleased := make(chan struct{}), make(chan struct{})
	if _, out := a.Bind(func(Let) { <-bReleased; close(aReleased) }); out {
		t.Fatal("a was let out at its join, want it waiting behind the burst's place")
	}
	if _, out := b.Bind(func(Let) { close(bReleased) }); out {
		t.Fatal("b was let out at its join, want it waiting behind a")
	}

	mu.Lock()
	now = origin.Add(3 * interval)
	turns := armed
	armed = nil
	mu.Unlock()
	if len(turns) != 1 {
		t.Fatalf("%d turns armed while a and b wait, want the one pending turn", len(turns))
	}
	go turns[0]()
	select {
	case <-aReleased:
	case <-time.After(10 * time.Second):
		t.Fatal("a's release still waited for b's after 10s: the late turn released its places one after another on one goroutine")
	}
}

// A rate of zero is the pacing off: every place, of either queue, is let out at its Join, and no
// timer is armed. A pacer that paced at some rate of its own when the limiter's was off fails.
func TestPacingOffLetsEveryPlaceOutAtOnce(t *testing.T) {
	p, clock := newPacer(0, 1)
	var log letLog
	var want []entry
	for i := range 40 {
		name := other(i)
		if i%3 == 0 {
			name = message(i)
		}
		log.join(p, queueOf(name), name)
		want = append(want, out(name, queueOf(name), 0, 0))
	}
	log.check(t, want)
	if n := clock.waitingTimers(); n != 0 {
		t.Fatalf("the pacing off armed %d timers, want none", n)
	}
}

// A release may call back into the pacer — take a place for a follow-up wake, give up another —
// since the pacer calls it outside its lock. A pacer that released under its lock deadlocks here,
// which the test reports after ten seconds rather than hang.
func TestAReleaseMayJoinAndLeave(t *testing.T) {
	p, clock := newPacer(rate, 1)
	var log letLog
	log.join(p, Second, "spent")
	a := p.Join(Second)
	log.join(p, Second, "b")
	c := log.join(p, Second, "c")
	a.Bind(func(let Let) {
		log.add("a", let)
		log.join(p, First, "a's follow-up")
		c.Leave()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		clock.advance(3 * interval)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the turns had not ended after 10s: a release that calls back into the pacer deadlocked it")
	}
	log.check(t, []entry{
		out("spent", Second, 0, 0), out("a", Second, 1, 1),
		out("a's follow-up", First, 2, 1), out("b", Second, 3, 3),
	})
}

// Places taken, marked and read from several goroutines at once get a schedule as orderly as one
// goroutine's: every place let out once, one turn each, one interval apart, while other goroutines
// read the places through the turns.
func TestConcurrentPlacesShareOneSchedule(t *testing.T) {
	const goroutines, each = 10, 5
	p, clock := newPacer(rate, 1)
	var log letLog
	places := make([]*Place, goroutines*each)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				pl := log.join(p, Queue(i%2), fmt.Sprintf("%d.%d", g, i))
				places[g*each+i] = pl
				if i%4 == 0 {
					pl.Mark()
				}
				pl.Estimate()
			}
		}()
	}
	wg.Wait()

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 2 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, pl := range places {
					pl.Estimate()
					pl.Before(places[0])
					pl.Queue()
					pl.Let()
				}
			}
		}()
	}
	clock.advance(goroutines * each * interval)
	close(stop)
	readers.Wait()

	got := log.entries()
	if len(got) != goroutines*each {
		t.Fatalf("%d let-outs, want %d", len(got), goroutines*each)
	}
	seen := map[string]bool{}
	for k, e := range got {
		if seen[e.name] {
			t.Fatalf("%s was let out twice", e.name)
		}
		seen[e.name] = true
		if !e.let.At.Equal(turnAt(k)) {
			t.Fatalf("let-out %d: %s, want it at +%s", k, e, turnAt(k).Sub(origin))
		}
	}
}
