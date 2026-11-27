package supervise

import (
	"fmt"
	"time"
)

// claimClock is what the reading knows of one claim's first wait: whether its turn lines show a
// turn in progress, whether a daemon's start waits on its worker's next act, whether a death
// waits on its next registration; whether the machine's view lines last showed its view of the
// claim's turns cut (cut); the claim's latest value write and whether a seen report since
// named it (sawLatest); the number it owes, nil for none; and whether the window has opened for
// it (enter). It holds the claim's turns a put-back recounts too: its latest turn naming a task,
// and the number whose put-back run's end line is still to come, 0 for none, which that line names
// and which counts as no cause.
type claimClock struct {
	inTurn, awaitsAct, awaitsRegistration bool
	cut                                   bool
	written                               TaskValue
	seen                                  bool
	owed                                  *owedNumber
	entered                               bool
	taskTurn                              costTurn
	putBackEnd                            uint64
}

// runs is whether the claim's clock runs now: it owes a number whose latest revision was
// reported seen and whose bound has not passed, and nothing stops the clock.
func (k *claimClock) runs() bool {
	o := k.owed
	return o != nil && k.seen && !o.stopped && o.passed.IsZero() && !k.inTurn && !k.awaitsAct && !k.awaitsRegistration && !k.cut
}

// sawLatest is whether the seen report l names the claim's latest value write: its number and
// revision, of its epoch where the line names one. A line that names no epoch is an older
// daemon's, matched by its revision alone, as the reading matched every line before the seen
// report carried its epoch. Another epoch's report is not of this write: the bucket was created
// again, and its revisions count from 1 once more.
func (k *claimClock) sawLatest(l costLine) bool {
	w := k.written
	return w.Number != 0 && *l.Task == w.Number && l.Revision == w.Revision && (l.Epoch == nil || *l.Epoch == w.Epoch)
}

// enter is the window's opening for the claim of k, at its first line inside the window, at a
// daemon's start inside it, or, for a claim with none, at the window's end (done): only clock time
// inside the window counts, so a clock that runs at From runs from From, as one whose seen report
// came there. No bound of the claim was judged before it (boundPassed), so none has passed. The
// window opens claim by claim, since only each claim's own lines are sure to be in the order they
// were written: one machine writes them, while two claims' lines can cross in the log.
func (c *costReading) enter(k *claimClock) {
	if k.entered {
		return
	}
	k.entered = true
	if o := k.owed; o != nil && !o.from.IsZero() && o.from.Before(c.cost.Args.From) {
		o.from = c.cost.Args.From
	}
}

// owe is the write of key's number written, at at (the reading's doc comment says what it owes),
// which is the claim's latest write until the next: no seen report has named it yet. A write of
// the number the claim owes already is a newer revision of it: the clock stands still until that
// revision is reported seen, and a refusal's or a take-back's stop ends. Otherwise the number the
// claim owed, if any, is replaced, which ends its time owed, and key's number is owed from here,
// unless an accepted take of it stands.
func (c *costReading) owe(k *claimClock, key costKey, written TaskValue, at time.Time) {
	k.written, k.seen = written, false
	if o := k.owed; o != nil && o.key == key {
		o.stopped, o.from = false, time.Time{}
		return
	}
	if o := k.owed; o != nil {
		how := ""
		if !o.passed.IsZero() {
			how = fmt.Sprintf("replaced by task %d", key.number)
		}
		c.endOwed(k, o.key.number, at, how)
	}
	if open, ok := c.open[key.claim]; ok && open.number == key.number {
		return
	}
	k.owed = &owedNumber{key: key}
}

// boundPassed records that the claim's clock passed its bound before at, where it did. It is
// asked at every line of the claim the reading decodes (costed), at a daemon's start, and at the
// window's end, and never before the window opened (enter), since only clock time inside it counts.
func (c *costReading) boundPassed(k *claimClock, at time.Time) {
	o := k.owed
	if !k.entered || o == nil || o.from.IsZero() || !o.passed.IsZero() {
		return
	}
	if bound := o.from.Add(c.cost.Args.Take); at.After(bound) {
		o.passed = bound
	}
}

// restart starts the claim's clock from at, the bound whole, where nothing stops it.
func (c *costReading) restart(k *claimClock, at time.Time) {
	if k.runs() {
		k.owed.from = at
	}
}

// stop stops the clock of the claim's number until that number's next write (owe) or the claim's
// next registration (registered), if it is the number the claim owes: a take refused or undecided
// at its revision, or a take-back of it.
func (c *costReading) stop(k *claimClock, number uint64) {
	if o := k.owed; o != nil && o.key.number == number {
		o.stopped, o.from = true, time.Time{}
	}
}

// takenBack is a take-back of key's number, which owes it again (the reading's doc comment): its
// clock stops until the number's next write or the claim's next registration. A number the claim
// no longer owes, since the take now taken back ended its time owed, is owed again from here, as a
// time owed of its own, where the lines hold the write that number was last written at: that is
// the revision the claim's next process reads and asks at, with no new write.
func (c *costReading) takenBack(k *claimClock, key costKey) {
	if k.owed == nil && k.written.Number == key.number {
		k.owed = &owedNumber{key: key}
	}
	c.stop(k, key.number)
}

// registered is a registration of the claim's agent, which a new process makes (register): the
// process is in no turn, a death's wait for a registration is over, and what the process before it
// saw and asked is forgotten, as the machine forgets it (decision 6). So the seen report goes, and
// every stop of the clock with it, and the clock starts again at the new process's own seen
// report, which the machine logs since the registration cleared the revision it had recorded seen.
func (c *costReading) registered(k *claimClock) {
	k.inTurn, k.awaitsRegistration, k.putBackEnd, k.seen = false, false, 0, false
	if o := k.owed; o != nil {
		o.stopped, o.from = false, time.Time{}
	}
}

// turnStarted is the start of a turn of the claim, which stops its clock. A put-back run's end
// that has not come by now never will.
func (c *costReading) turnStarted(k *claimClock) {
	k.inTurn, k.putBackEnd = true, 0
	if o := k.owed; o != nil {
		o.from = time.Time{}
	}
}

// turnEnded is the end of the claim's turn at at: its clock starts again from here, the bound
// whole.
func (c *costReading) turnEnded(k *claimClock, at time.Time) {
	k.inTurn = false
	c.restart(k, at)
}

// putBack is a report that named no task for the run the claim's take of key's number was given
// (notTheTakesTurnLine): that run was no turn of the number. Its start and its end, where the
// window counted them as the number's, count as a turn of no cause instead, and so does its end
// still to come, whose line names the number its start named (endTurn); and the take awaits its
// turn again, its second wait beginning here (secondWaitNow).
func (c *costReading) putBack(k *claimClock, key costKey, at time.Time) {
	run := k.taskTurn
	k.taskTurn = costTurn{}
	if run.number != key.number {
		// No line of the run was read, but its end may still come.
		run = costTurn{number: key.number}
	}
	if run.startCounted {
		c.cost.TurnsStarted.Task--
		c.cost.TurnsStarted.None++
		if c.turns[key]--; c.turns[key] == 0 {
			delete(c.turns, key)
		}
	}
	if run.endCounted {
		c.cost.TurnsEnded.Task--
		c.cost.TurnsEnded.None++
	}
	if !run.over {
		k.putBackEnd = key.number
	}
	if open, ok := c.open[key.claim]; ok && open.number == key.number {
		open.awaits, open.from = true, at
		c.open[key.claim] = open
	}
}

// secondWaitFrom begins the second wait of the claim's accepted take again at at, where that take
// awaits its turn: at the end of a turn that ran while it waited, where the machine's view of the
// claim's turns is whole again (viewWhole), and at the worker's first act after a daemon's start
// over a view no line shows cut (openTake).
func (c *costReading) secondWaitFrom(claim string, at time.Time) {
	if open, ok := c.open[claim]; ok && open.awaits {
		open.from = at
		c.open[claim] = open
	}
}

// viewCut is the machine's view of the claim's turns ceasing to be whole (viewCutLine): a close, a
// daemon's start that restored the claim with a process, or a reconnect hello over a view it found
// whole. The machine's bounds that judge the worker by its frames stand still from here, so the
// claim's clock stands still too, and no act of the worker starts it or begins a take's second
// wait again until the view is whole (viewWhole). The line judged the bound at its own time first
// (costed).
func (c *costReading) viewCut(k *claimClock) {
	k.cut = true
	if o := k.owed; o != nil {
		o.from = time.Time{}
	}
}

// viewWhole is the machine's view of the claim's turns whole again (viewWholeLine) at at: the
// machine arms each bound the view held still whole from here, so the claim's clock starts again
// here, the bound whole, where nothing else stops it, and the second wait of a take awaiting its
// turn begins again here (secondWaitNow). It ends a daemon start's wait for the worker's first act
// too (Machine.awaitsAct): every change to whole but a let-go comes with or after a shim's hello,
// which is that act and which no line the reading decodes names, but one — a sweep's ask of a
// claim restored working, answered before the decision that reads the hello — where the machine's
// waits start at the hello, after the reading's. A let-go makes the view whole too, for the next
// process, whose clock its registration and seen report start again.
func (c *costReading) viewWhole(k *claimClock, claim string, at time.Time) {
	k.cut, k.awaitsAct = false, false
	c.restart(k, at)
	c.secondWaitFrom(claim, at)
}

// daemonStarting is a daemon's starting line, the first line it writes (internal/daemon's run),
// before it reads its plugins or restores a claim: every line the reading decodes before it is the
// daemon before's, so every clock stops as of the last one the reading decoded (stopEveryClock),
// and no time between the two daemons counts. A daemon whose boot stopped short writes no start
// line, and the next daemon's starting line is a start of its own.
func (c *costReading) daemonStarting() {
	c.starting = true
	c.stopEveryClock()
}

// daemonStarted is a daemon's start line, which the daemon writes once its boot's steps are done,
// after it restored every claim. Where its starting line began that start (daemonStarting), every
// clock stopped there, and each line since is the started daemon's own, so this line changes
// nothing: a view the started daemon made whole before it — its shim's hello and its agent's
// answer decided while the boot's last steps ran — keeps the clock that line started. In a log
// with no starting line before it, an older daemon's, every clock stops here (stopEveryClock).
func (c *costReading) daemonStarted() {
	if c.starting {
		c.starting = false
		return
	}
	c.stopEveryClock()
}

// stopEveryClock is a daemon's start, as of the last line the reading decoded before it (alive):
// each clock's bound is judged there, since nothing says the daemon before the start lived any
// longer, and the machine the start builds runs none of its waits until the worker's first act
// and its view's whole line, so the time no daemon ran counts toward no bound. Each clock stands
// still until its claim's worker next acts in the log (workerActs), as the machine's waits wait
// for the worker's first act (Machine.awaitsAct), or, for a claim the start restored with a
// process, until its view's whole line (viewWhole). A start inside the window opens it for every
// claim first, so the time a clock ran before From is not judged.
func (c *costReading) stopEveryClock() {
	for _, k := range c.clocks {
		if c.counting {
			c.enter(k)
		}
		c.boundPassed(k, c.alive)
		k.awaitsAct = true
		if o := k.owed; o != nil {
			o.from = time.Time{}
		}
	}
}

// endOwed ends at at, as how says, the time the claim was owed number, if it owes that number, and
// keeps that time owed for the figure where its clock passed the bound. The line ending it has
// judged the bound already (costed).
func (c *costReading) endOwed(k *claimClock, number uint64, at time.Time, how string) {
	o := k.owed
	if o == nil || o.key.number != number {
		return
	}
	if !o.passed.IsZero() {
		o.ended = how + " " + lineTime(at)
		c.unreached = append(c.unreached, *o)
	}
	k.owed = nil
}
