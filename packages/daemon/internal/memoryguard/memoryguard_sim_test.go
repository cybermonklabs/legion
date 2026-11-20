package memoryguard

import (
	"cmp"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"testing"
	"time"
)

// The seeded simulation of the latepool probe under check c-choice (INFRA-3531 R1 of spec version
// 43, the architect's addition of 2026-11-20, and INFRA-3368's architect's decision of 2026-11-20,
// dispatch://INFRA-3531/comment/b3d48079-f88b-4b86-838b-dd06ccb53837). It drives the guard with
// the fake clock and the fake pod, and puts each grower's steps at random instants, so that a step
// lands between two readings, between a reading and the guard's read of the grower at it, inside a
// read, and between a kill and the guard's next read. Over a fixed list of seeds it checks the
// three rules of R1 the probe has broken, from the guard's kills (the Sink's Kill values) and the
// processes' true sizes:
//
//   - first: the first kill of a rise ends a process that grew over the window, where one the guard
//     read at that reading did;
//   - largest: no rule=largest kill comes while a process the guard may end has grown since the
//     stay's first kill;
//   - go-on: at a reading after a kill that has not fallen below the reading at the kill, where the
//     killed processes have exited and the processes the guard may end hold the floor, a process it
//     may end that has grown since the kill is a kill at that reading.
//
// Growth since a kill is from a process's size as of the kill: its true size at the kill's reading,
// where the guard read it running its program before that reading; the least of the guard's reads
// of it at the kill's reading before the kill, where those are its first reads of its program; and
// 0 where every read of it before the kill is of another program, the read between its fork and its
// exec. A process the guard never read before the kill is left out, and so is one that the guard's
// last read of it by the end of the kill's reading found still running another program. Rule
// largest, at a kill of a process that never grew, counts from 0 a process the guard never read
// before the stay's first kill whose first read after that kill was not a fork's read, where no
// other case explains the kill: the order's third tier counts it so (INFRA-3531 R1 of spec version
// 43; INFRA-3368's architect's decision of 2026-11-20T08:13Z, item 2); rule go-on keeps that first
// read, as the decision does, and leaves the process out. Each failure names its seed, the rule,
// the kill it is about and its case:
//
//   - (a) the guard had read the process running its program before the kill's reading, and did not
//     read it at that reading before the kill (it read no smaps there, or the read failed): the
//     step it missed landed after the kill, before its next read;
//   - (b) every read of it before the kill was of another program, its parent's at its parent's
//     size, the guard having read it between its fork and its exec: the guard took its first read
//     after the kill, after the step;
//   - (c) the guard's read of it at the kill's reading, before the kill, came after a step that
//     landed after the reading: the reading rose since the kill's reading and its sizes did not;
//   - (d) the guard never read the process before the stay's first kill, and its first read after
//     that kill ran its own program, not its parent's: the guard took that first read as its size
//     as of the kill. At 28732f65, in `8g-latepool-r2`, the guard first read five of six growers
//     after the first kill, and its third kill ended the probe's parent by rule=largest while four
//     of them lived.
//
// At 83d6a910 seeds of each of the cases (a) to (c) fail: rule largest in case (b) is r40's shape,
// rule go-on in case (c) r31's. A guard that keeps as a size as of a kill the latest read finished
// before the kill, whatever program it read, passes the seeds of case (a) alone; one that keeps the
// size the decision of 2026-11-20 states passes every seed of those cases. At a28d75a8 three seeds
// fail, each in rule largest, case (d), and none in another rule or case: in seeds 113 and 156 a
// late grower (below) lived, and in seed 122 one of the six that fork in the band, which the guard
// too first read after that kill.
//
// The model: the agent, protected, holds 1 GiB; the probe's parent, pid 700, and its three sleeping
// forks hold the latepool block and never grow, the block in the reading once while any of them
// lives; and the rest of the pod holds what puts the reading 5 to 60 MiB below the mark, as in r40,
// where one grower of 69 MiB held the reading above the mark after the others ended. Six growers
// fork from the parent at random instants 400 to 700 ms after the guard's first reading, in the
// band, each first with its parent's program and its parent's block as its own, as a fork reports
// it under gVisor, and run the grow program after a random 1 to 120 ms, at 9 MiB; an exec lands at
// the end of the reading it falls in, so the guard reads each process's smaps of the program it
// listed. Each grower then steps 8 to 12 MiB at random gaps of 60 to 500 ms. A step is under way for
// 1 to 30 ms before it lands, and a read of the grower's smaps while it is under way fails. Each
// read of a smaps by the guard's loop takes 2 to 8 ms of the model's time, by the seed, as the
// pod's CPU throttle makes them, a read of a new process's identity 1 ms, a kill 1 ms and a read of
// the counter 3 ms; the guard's own clock stands still within a reading, so its budgets at the mark
// never pass. Readings come the interval apart, most of them 0 to 10 ms late, some 10 to 60 ms and
// some 60 to 220 ms, as a throttled pod's do. A killed process exits at its kill and frees what it
// holds. In half the seeds, by draws of their own (planLate), the last two to five of the six by
// their forks are late growers: they start instead at the stay's first kill or 1 to 150 ms after
// it, each running the grow program from its start, at 9 MiB, and step first 250 to 500 ms after
// it, the grow program's sleep after its first block, so the guard reads none of them before that
// kill (the architect's decision of 2026-11-20T08:13Z, item 2: the simulation missed r2's shape, so
// its model lacked them).
func TestMemoryGuardKeepsTheRulesOfR1WhereGrowersStepAtRandomInstants(t *testing.T) {
	failed := map[string][]uint64{}
	for seed := uint64(1); seed <= simSeeds; seed++ {
		var found []simFailure
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			found = newSimRun(t, seed).run().check()
			for i, f := range found {
				if i == simFailuresShown {
					t.Errorf("seed %d: %d more failures", seed, len(found)-i)
					break
				}
				t.Errorf("seed %d: %s", seed, f)
			}
		})
		for _, f := range found {
			key := "rule " + f.rule + ", case " + f.cause
			if seeds := failed[key]; len(seeds) == 0 || seeds[len(seeds)-1] != seed {
				failed[key] = append(seeds, seed)
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(failed)) {
		t.Logf("%s: %d seeds %v", key, len(failed[key]), failed[key])
	}
}

const (
	// simSeeds is how many seeds the simulation runs, from 1.
	simSeeds = 200
	// simReadings is the most readings of one seed's run.
	simReadings = 40
	// simFailuresShown is how many failures of one seed the test prints.
	simFailuresShown = 3
	simAgent         = gib
	simGrowerBase    = 9 * mib
)

// The model's time each of the guard's operations takes but a smaps read (simRun.op).
const (
	simIdentityCost = time.Millisecond
	simKillCost     = time.Millisecond
	simCounterCost  = 3 * time.Millisecond
)

// simProcess is one process of the model: its files as the fake pod writes them, and its state.
// Its smaps reads fp.anon: the block for the parent, a sleeping fork and a grower before its exec,
// and its own memory for a grower after it; and it cannot be read while a step is under way
// (fp.unreadableSmaps).
type simProcess struct {
	fp     fakeProcess
	grower bool
	// forkAt is when a grower forked, forked whether it has and execd whether it runs the grow
	// program.
	forkAt               time.Duration
	forked, execd, alive bool
}

// The kinds of a simEvent.
const (
	simFork = iota
	simExec
	simStepStart
	simStepEnd
	// simStart is a late grower's start: a child of the parent that runs the grow program from its
	// start (startLate).
	simStart
)

// simEvent is one change of a grower at an instant of the model's time.
type simEvent struct {
	at   time.Duration
	pid  int
	kind int
	by   uint64
}

// simReading is one reading of the run: when the guard's clock took it, the reading, each living
// process's smaps size when it was taken, the model's time at the end of the reading's work, and
// whether the guard gave the rise up there.
type simReading struct {
	now, end time.Duration
	reading  uint64
	size     map[int]uint64
	stopped  bool
}

// simRead is one read of a process's smaps by the guard's loop: the reading it was at, its instant
// in the model's time, whether it read, the size it read and whether the process ran its own
// program then.
type simRead struct {
	pid, reading int
	at           time.Duration
	ok, own      bool
	size         uint64
}

// simKill is one kill the guard sent: the reading, the instant in the model's time, and the rule
// its report carries.
type simKill struct {
	pid, reading int
	at           time.Duration
	rule         string
}

// simFailure is one break of a rule of R1 in a seed's run.
type simFailure struct {
	rule, cause, what string
}

func (f simFailure) String() string {
	return fmt.Sprintf("rule %s, case %s: %s", f.rule, f.cause, f.what)
}

// simRun is one seed's run of the model.
type simRun struct {
	t      *testing.T
	seed   uint64
	rng    *rand.Rand
	pod    *fakePod
	h      *harness
	procs  map[int]*simProcess
	events []simEvent
	next   int
	// deferred are the changes due within a reading that wait for its end (apply).
	deferred []simEvent
	lastFork time.Duration
	// rest is what the pod holds that no process the guard reads does, and readCost the model's
	// time a smaps read takes, both by the seed.
	rest     uint64
	readCost time.Duration
	vt       time.Duration
	readings []simReading
	reads    []simRead
	kills    []simKill
	// late are the late growers' changes, each at its time from their start (planLate), until
	// startLate puts them on the model's time.
	late []simEvent
	// byPID are the reads by pid, in order, indexed once the run is done (readsOf).
	byPID map[int][]simRead
}

var (
	simBlockHolders = []int{700, 701, 702, 703}
	simGrowers      = []int{800, 801, 802, 803, 804, 805}
	// simEndable are the processes the guard may end.
	simEndable = slices.Concat(simBlockHolders, simGrowers)
)

func newSimRun(t *testing.T, seed uint64) *simRun {
	t.Helper()
	s := &simRun{t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, 0x6c61746570)), procs: map[int]*simProcess{}}
	mark := share(podLimit, Defaults().ActPercent)
	s.rest = mark - simAgent - latepoolBlock - (5+s.rng.Uint64N(56))*mib
	s.readCost = s.uniform(2*time.Millisecond, 8*time.Millisecond)
	s.pod = newFakePod(t)
	omp := agent(simAgent)
	s.procs[omp.pid] = &simProcess{fp: omp, alive: true, execd: true}
	s.pod.add(omp)
	for i, pid := range simBlockHolders {
		fp := fakeProcess{pid: pid, ppid: 700, start: uint64(5000 + 100*i), exe: "/usr/bin/python3.13", args: latepoolArgs, anon: latepoolBlock}
		if i == 0 {
			fp.ppid = 100
		}
		s.procs[pid] = &simProcess{fp: fp, alive: true, execd: true}
		s.pod.add(fp)
	}
	s.schedule()
	s.planLate(seed)
	s.pod.setAnon(s.total())
	s.h = newHarness(t, s.pod, protect("tester", 100))
	s.h.g.smapsRead = func(pid int) {
		s.op(s.readCost)
		r := simRead{pid: pid, reading: len(s.readings) - 1, at: s.vt}
		if p := s.procs[pid]; p != nil {
			r.ok, r.own, r.size = p.alive && !p.fp.unreadableSmaps, !p.grower || p.execd, p.fp.anon
		}
		s.reads = append(s.reads, r)
	}
	s.h.g.identityRead = func(int) { s.op(simIdentityCost) }
	s.h.g.counterRead = func() { s.op(simCounterCost) }
	s.h.signal.onKill = func(pid int) bool {
		s.op(simKillCost)
		s.kills = append(s.kills, simKill{pid: pid, reading: len(s.readings) - 1, at: s.vt})
		if p := s.procs[pid]; p != nil && p.alive {
			p.alive = false
			s.pod.remove(pid)
		}
		if len(s.kills) == 1 {
			s.startLate(s.vt)
		}
		return true
	}
	return s
}

// schedule draws the growers' forks, execs and steps. The growers take their pids and start times
// in the order they fork, as gVisor and Linux allocate them.
func (s *simRun) schedule() {
	forks := make([]time.Duration, len(simGrowers))
	for i := range forks {
		forks[i] = 400*time.Millisecond + s.uniform(0, 300*time.Millisecond)
	}
	slices.Sort(forks)
	s.lastFork = forks[len(forks)-1]
	for i, fork := range forks {
		pid := simGrowers[i]
		s.procs[pid] = &simProcess{
			fp:     fakeProcess{pid: pid, ppid: 700, start: uint64(6000 + i), exe: "/usr/bin/python3.13", args: latepoolArgs},
			grower: true,
		}
		exec := fork + s.uniform(time.Millisecond, 120*time.Millisecond)
		s.events = append(s.events, simEvent{at: fork, pid: pid, kind: simFork}, simEvent{at: exec, pid: pid, kind: simExec})
		for at := exec + s.uniform(20*time.Millisecond, 200*time.Millisecond); at < 8*time.Second; at += s.uniform(60*time.Millisecond, 500*time.Millisecond) {
			start := max(exec, at-s.uniform(time.Millisecond, 30*time.Millisecond))
			by := (8 + s.rng.Uint64N(5)) * mib
			s.events = append(s.events, simEvent{at: start, pid: pid, kind: simStepStart}, simEvent{at: at, pid: pid, kind: simStepEnd, by: by})
		}
	}
	slices.SortStableFunc(s.events, func(x, y simEvent) int { return cmp.Compare(x.at, y.at) })
}

// planLate makes the run's late growers (INFRA-3368's architect's decision of 2026-11-20T08:13Z,
// item 2, on `8g-latepool-r2` at 28732f65, where five of six growers were first read after the
// stay's first kill), by a stream of draws of the seed's own, so that a seed without them runs as
// it did: in half the seeds, the last two to five of the six by their forks do not fork in the band
// but start at the stay's first kill or 1 to 150 ms after it (startLate). Each is a child of the
// parent that runs the grow program from its start, at 9 MiB, as r2's growers, subprocesses started
// after the band's last reading, did, so its first read is not a fork's read. The program sleeps
// 250 ms after its first block, so its first step lands 250 to 500 ms after its start, as r2's
// did, and it then steps as the others do.
func (s *simRun) planLate(seed uint64) {
	rng := rand.New(rand.NewPCG(seed, 0x6c617465))
	uniform := func(lo, hi time.Duration) time.Duration { return lo + time.Duration(rng.Int64N(int64(hi-lo))) }
	if rng.IntN(2) == 0 {
		return
	}
	late := simGrowers[len(simGrowers)-2-rng.IntN(4):]
	s.events = slices.DeleteFunc(s.events, func(e simEvent) bool { return slices.Contains(late, e.pid) })
	s.lastFork = 0
	for _, e := range s.events {
		if e.kind == simFork {
			s.lastFork = max(s.lastFork, e.at)
		}
	}
	starts := make([]time.Duration, len(late))
	for j := range starts {
		if rng.IntN(2) == 0 {
			starts[j] = uniform(time.Millisecond, 150*time.Millisecond)
		}
	}
	slices.Sort(starts)
	for j, pid := range late {
		p, start := s.procs[pid], starts[j]
		p.fp.args, p.fp.anon = growArgs, simGrowerBase
		s.late = append(s.late, simEvent{at: start, pid: pid, kind: simStart})
		for at := start + uniform(250*time.Millisecond, 500*time.Millisecond); at < 8*time.Second; at += uniform(60*time.Millisecond, 500*time.Millisecond) {
			step := max(start, at-uniform(time.Millisecond, 30*time.Millisecond))
			by := (8 + rng.Uint64N(5)) * mib
			s.late = append(s.late, simEvent{at: step, pid: pid, kind: simStepStart}, simEvent{at: at, pid: pid, kind: simStepEnd, by: by})
		}
	}
}

// startLate puts the late growers' changes on the model's time from the later of at, the instant
// of the stay's first kill, and the last fork of the growers that fork in the band, so that every
// grower takes its pid and start time in the order it starts; the guard has read none of them
// before that kill. A start due at once lands before the guard's next read, so the reads after the
// kill at its reading can list it, as they listed r2's 7177 to 7181.
func (s *simRun) startLate(at time.Duration) {
	from := max(at, s.lastFork)
	for _, e := range s.late {
		e.at += from
		if e.kind == simStart {
			s.lastFork = max(s.lastFork, e.at)
		}
		s.events = append(s.events, e)
	}
	s.late = nil
	slices.SortStableFunc(s.events[s.next:], func(x, y simEvent) int { return cmp.Compare(x.at, y.at) })
	s.apply(s.vt, false)
}

// uniform is a duration drawn evenly from [lo, hi).
func (s *simRun) uniform(lo, hi time.Duration) time.Duration {
	return lo + time.Duration(s.rng.Int64N(int64(hi-lo)))
}

// op is one of the guard's operations, which takes d of the model's time: every change of a grower
// due by its end lands before it, but an exec (apply).
func (s *simRun) op(d time.Duration) {
	s.vt += d
	s.apply(s.vt, false)
}

// apply makes each change due at or before upTo. Within a reading, between, false, an exec waits
// for the reading's end, and so does every later change of its grower: a reading reads each
// process's program once, at its listing or its read of a new process's identity, and the smaps the
// guard reads after it are of that program.
func (s *simRun) apply(upTo time.Duration, between bool) {
	if between {
		for _, e := range s.deferred {
			s.change(e)
		}
		s.deferred = s.deferred[:0]
	}
	for ; s.next < len(s.events) && s.events[s.next].at <= upTo; s.next++ {
		e := s.events[s.next]
		if !between && (e.kind == simExec || slices.ContainsFunc(s.deferred, func(d simEvent) bool { return d.pid == e.pid })) {
			s.deferred = append(s.deferred, e)
			continue
		}
		s.change(e)
	}
}

// change makes e.
func (s *simRun) change(e simEvent) {
	p := s.procs[e.pid]
	switch {
	case e.kind == simFork && s.procs[700].alive:
		p.forked, p.forkAt, p.alive, p.fp.anon = true, e.at, true, latepoolBlock
		s.pod.add(p.fp)
	case e.kind == simStart && s.procs[700].alive:
		p.forked, p.execd, p.forkAt, p.alive = true, true, e.at, true
		s.pod.add(p.fp)
	case e.kind == simFork || e.kind == simStart || !p.alive:
	case e.kind == simExec:
		p.execd = true
		p.fp.args, p.fp.anon = growArgs, simGrowerBase
		s.pod.add(p.fp)
	case e.kind == simStepStart:
		// A read of the smaps fails while the step is under way, as a directory's does. A step
		// rewrites the smaps alone: fakePod.add rewrites every file of the process, and costs the
		// simulation about 30% of its time.
		p.fp.unreadableSmaps = true
		smaps := s.h.g.smapsPath(p.fp.pid)
		if err := os.Remove(smaps); err != nil {
			s.t.Fatal(err)
		}
		if err := os.Mkdir(smaps, 0o755); err != nil {
			s.t.Fatal(err)
		}
	case e.kind == simStepEnd:
		p.fp.unreadableSmaps = false
		p.fp.anon += e.by
		smaps := s.h.g.smapsPath(p.fp.pid)
		if err := os.Remove(smaps); err != nil {
			s.t.Fatal(err)
		}
		s.pod.writeWhole(smaps, smapsOf(p.fp))
	}
}

// total is the pod's reading: the agent, what no process holds, the block once while a process that
// holds it lives, and each grower's own memory once it runs its program.
func (s *simRun) total() uint64 {
	total := simAgent + s.rest
	if slices.ContainsFunc(simBlockHolders, func(pid int) bool { return s.procs[pid].alive }) {
		total += latepoolBlock
	}
	for _, pid := range simGrowers {
		if p := s.procs[pid]; p.alive && p.execd {
			total += p.fp.anon
		}
	}
	return total
}

// lateBy is how late the next reading comes after the interval.
func (s *simRun) lateBy() time.Duration {
	switch f := s.rng.Float64(); {
	case f < 0.7:
		return s.uniform(0, 10*time.Millisecond)
	case f < 0.9:
		return s.uniform(10*time.Millisecond, 60*time.Millisecond)
	}
	return s.uniform(60*time.Millisecond, 220*time.Millisecond)
}

// snapshot starts the reading the guard's clock takes at now: every change due before it lands.
func (s *simRun) snapshot(now time.Duration) {
	s.apply(now-1, true)
	s.vt = max(s.vt, now)
	s.pod.setAnon(s.total())
	r := simReading{now: now, reading: s.pod.anon, size: map[int]uint64{}}
	for pid, p := range s.procs {
		if p.alive {
			r.size[pid] = p.fp.anon
		}
	}
	s.readings = append(s.readings, r)
}

// run takes the readings until every grower has forked and ended, three readings after that, or
// simReadings have passed, and gives each kill the rule of its report.
func (s *simRun) run() *simRun {
	stopped := 0
	finish := func() {
		r := &s.readings[len(s.readings)-1]
		r.end = s.vt
		if n := len(s.h.log.with("stopped")); n > stopped {
			stopped, r.stopped = n, true
		}
	}
	s.snapshot(0)
	s.h.run()
	finish()
	for after := 0; len(s.readings) < simReadings && after < 3; {
		late := s.lateBy()
		s.snapshot(s.h.clock.Now().Sub(testStart) + s.h.pending.d + late)
		s.h.tickAfter(late)
		finish()
		if s.vt > s.lastFork && !slices.ContainsFunc(simGrowers, func(pid int) bool { return s.procs[pid].alive }) {
			after++
		}
	}
	s.h.stop()
	s.byPID = map[int][]simRead{}
	for _, r := range s.reads {
		s.byPID[r.pid] = append(s.byPID[r.pid], r)
	}
	reported := s.h.killed()
	if len(reported) != len(s.kills) {
		s.t.Fatalf("seed %d: the guard sent %d kills and reported %d", s.seed, len(s.kills), len(reported))
	}
	for i, k := range reported {
		if k.PID != s.kills[i].pid {
			s.t.Fatalf("seed %d: report %d is of pid %d, the kill of pid %d", s.seed, i, k.PID, s.kills[i].pid)
		}
		s.kills[i].rule = k.Rule
	}
	return s
}

// killsAt are the kills sent at reading i, in order.
func (s *simRun) killsAt(i int) []simKill {
	var out []simKill
	for _, k := range s.kills {
		if k.reading == i {
			out = append(out, k)
		}
	}
	return out
}

// readsOf are the guard's reads of pid's smaps, in order.
func (s *simRun) readsOf(pid int) []simRead { return s.byPID[pid] }

// readAt is pid's read the guard decided on at reading i before the instant before: its last read
// at reading i before it, false where it has none or that read failed.
func (s *simRun) readAt(pid, i int, before time.Duration) (simRead, bool) {
	var last simRead
	found := false
	for _, r := range s.readsOf(pid) {
		if r.reading == i && r.at < before {
			last, found = r, true
		}
	}
	return last, found && last.ok
}

// kept is pid's read at reading i that the guard keeps in its window, its last read there that
// read.
func (s *simRun) kept(pid, i int) (simRead, bool) {
	var last simRead
	found := false
	for _, r := range s.readsOf(pid) {
		if r.reading == i && r.ok {
			last, found = r, true
		}
	}
	return last, found
}

// aliveAt is whether pid lives at the model's instant at: it has forked by then and no kill before
// it ended it.
func (s *simRun) aliveAt(pid int, at time.Duration) bool {
	if p := s.procs[pid]; p.grower && (!p.forked || p.forkAt > at) {
		return false
	}
	return !slices.ContainsFunc(s.kills, func(k simKill) bool { return k.pid == pid && k.at <= at })
}

// asOfKill is pid's size as of the kills of reading k, from which the rules measure growth since
// them, and the case of a guard that measured from a read made after the kill: false where pid is
// left out, never read before the kill or, at the guard's last read of it by the end of reading k,
// still running another program, the program it runs as far as the guard knows. Where fromNothing
// is set, a process never read before the kill whose first read after it ran its own program, not
// a fork's read, has 0 (case d): the order's third tier counts it from 0 from the stay's first kill
// (INFRA-3368's architect's decision of 2026-11-20T08:13Z, item 2).
func (s *simRun) asOfKill(pid, k int, fromNothing bool) (size uint64, cause string, ok bool) {
	first := s.killsAt(k)[0].at
	var last, latest, after *simRead
	var decision []simRead
	readBefore := false
	for _, r := range s.readsOf(pid) {
		if r.ok && r.reading <= k {
			latest = &r
		}
		switch {
		case !r.ok:
		case r.reading < k:
			last, readBefore = &r, true
		case r.reading == k && r.at < first:
			readBefore = true
			if r.own {
				decision = append(decision, r)
			}
		case after == nil:
			after = &r
		}
	}
	if !readBefore {
		if fromNothing && after != nil && after.own {
			return 0, "d", true
		}
		return 0, "", false
	}
	if latest == nil || !latest.own {
		return 0, "", false
	}
	switch {
	case last != nil && last.own && len(decision) > 0:
		return s.readings[k].size[pid], "c", true
	case last != nil && last.own:
		return s.readings[k].size[pid], "a", true
	case len(decision) > 0:
		return slices.MinFunc(decision, func(x, y simRead) int { return cmp.Compare(x.size, y.size) }).size, "other", true
	default:
		return 0, "b", true
	}
}

// grownSince is the process the guard may end that has grown the most since the kills of reading
// k, as the guard read it at reading i before the instant before, with how much, the size it read,
// its size as of those kills and the case, a process never read before those kills counted from 0
// where fromNothing is set (asOfKill).
func (s *simRun) grownSince(k, i int, before time.Duration, fromNothing bool) (pid int, grown, read, from uint64, cause string) {
	for _, q := range simEndable {
		if !s.aliveAt(q, before) {
			continue
		}
		r, ok := s.readAt(q, i, before)
		if !ok {
			continue
		}
		base, why, known := s.asOfKill(q, k, fromNothing)
		if !known || r.size < base+growthNoise || r.size-base <= grown {
			continue
		}
		pid, grown, read, from, cause = q, r.size-base, r.size, base, why
		// Case c is where the read the decision kept as the size as of the kill already held the
		// step; where it shows growth since that read, the guard missed something else.
		if d, kept := s.readAt(q, k, s.killsAt(k)[0].at); why == "c" && kept && r.size >= d.size+growthNoise {
			cause = "other"
		}
	}
	return pid, grown, read, from, cause
}

// check judges the run by the three rules, following the guard's phase in each rise and its stays
// in the band from the readings, the kills and the stopped lines.
func (s *simRun) check() []simFailure {
	d := Defaults()
	mark, band, floor := share(podLimit, d.ActPercent), bandStart(podLimit, d.ActPercent), share(podLimit, d.FloorPercent)
	var out []simFailure
	phase, stayFirst, rise := armed, -1, -1
	for i, r := range s.readings {
		kills := s.killsAt(i)
		if phase != armed && r.reading < mark {
			phase = armed
		}
		switch {
		case phase == armed && r.reading < band:
			stayFirst = -1
		case phase == armed && len(kills) > 0:
			out = append(out, s.checkFirst(i, kills[0])...)
			phase, rise = waiting, i
			if stayFirst < 0 {
				stayFirst = i
			}
		case phase == waiting && len(kills) > 0:
			rise = i
		case phase == waiting && r.reading >= s.readings[rise].reading:
			var sum uint64
			for _, q := range simEndable {
				if read, ok := s.readAt(q, i, r.end+1); ok && s.aliveAt(q, r.end) {
					sum += read.size
				}
			}
			if sum < floor {
				break
			}
			if pid, grown, read, from, cause := s.grownSince(rise, i, r.end+1, false); pid != 0 {
				k := s.killsAt(rise)[0]
				out = append(out, simFailure{rule: "go-on", cause: cause, what: fmt.Sprintf(
					"at the reading at %s, %d MiB, not fallen below the reading at the kill of pid %d (rule=%s) at %s, %d MiB, the guard ended nothing while pid %d had grown %d MiB since that kill (read %d MiB, %d MiB as of the kill)",
					simMS(r.now), r.reading/mib, k.pid, k.rule, simMS(s.readings[rise].now), s.readings[rise].reading/mib, pid, grown/mib, read/mib, from/mib)})
			}
		}
		if phase == waiting && stayFirst >= 0 && stayFirst < i {
			for _, k := range kills {
				if k.rule != ruleLargest {
					continue
				}
				pid, grown, read, from, cause := s.grownSince(stayFirst, i, k.at, false)
				if pid == 0 && !s.procs[k.pid].grower {
					// The parent and its forks never grow. Where no other case explains a kill of
					// one of them, case d, r2's, may.
					pid, grown, read, from, cause = s.grownSince(stayFirst, i, k.at, true)
				}
				if pid != 0 {
					first := s.killsAt(stayFirst)[0]
					out = append(out, simFailure{rule: "largest", cause: cause, what: fmt.Sprintf(
						"at the reading at %s, %d MiB, the guard ended pid %d by rule=largest while pid %d had grown %d MiB since the stay's first kill, of pid %d (rule=%s) at %s (read %d MiB, %d MiB as of that kill)",
						simMS(r.now), r.reading/mib, k.pid, pid, grown/mib, first.pid, first.rule, simMS(s.readings[stayFirst].now), read/mib, from/mib)})
				}
			}
		}
		if r.stopped {
			phase, stayFirst = stopped, -1
		}
	}
	return out
}

// checkFirst judges the first kill of a rise, at reading i: where a process the guard read at that
// reading before the kill grew over the window by its reads, the process the kill ended grew over
// the window too, by its reads at that reading or over the band's last window.
func (s *simRun) checkFirst(i int, first simKill) []simFailure {
	// grownBy is how much pid grew over the window to last, its read the decision took: last less
	// its oldest read kept within growthWindow of the reading at the mark, or of last's reading.
	grownBy := func(pid int, last simRead) uint64 {
		var most uint64
		for _, end := range []time.Duration{s.readings[i].now, s.readings[last.reading].now} {
			for j := range last.reading {
				if r, ok := s.kept(pid, j); ok && end-s.readings[j].now <= growthWindow {
					if last.size >= r.size+growthNoise {
						most = max(most, last.size-r.size)
					}
					break
				}
			}
		}
		return most
	}
	var grower int
	var grown uint64
	for _, pid := range simEndable {
		if last, ok := s.readAt(pid, i, first.at); ok {
			if g := grownBy(pid, last); g > grown {
				grower, grown = pid, g
			}
		}
	}
	if grower == 0 {
		return nil
	}
	var last simRead
	for _, r := range s.readsOf(first.pid) {
		if r.ok && r.at < first.at {
			last = r
		}
	}
	if grownBy(first.pid, last) > 0 {
		return nil
	}
	return []simFailure{{rule: "first", cause: "other", what: fmt.Sprintf(
		"at the reading at %s, %d MiB, the first kill of the rise ended pid %d (rule=%s), which did not grow over the window, while pid %d grew %d MiB over it",
		simMS(s.readings[i].now), s.readings[i].reading/mib, first.pid, first.rule, grower, grown/mib)}}
}

// simMS is d as the trace writes a reading's time: whole milliseconds after the guard's start.
func simMS(d time.Duration) string { return fmt.Sprintf("+%d ms", d.Milliseconds()) }
