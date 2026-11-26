package memoryguard

import (
	"cmp"
	"fmt"
	"maps"
	"math"
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
// processes' true sizes, a fourth, orphan, and a fifth, urgent:
//
//   - first: the first kill of a rise ends a process that grew over the window, where one the guard
//     read at that reading did;
//   - largest: no rule=largest kill comes while a process the guard may end has grown since the
//     stay's first kill;
//   - go-on: at a reading after a kill that has not fallen below the reading at the kill, where the
//     killed processes have exited and the processes the guard may end hold the floor, a process it
//     may end that has grown since the kill is a kill at that reading;
//   - orphan: no sleeping fork whose parent the guard has ended is ended by rule=grew-since-kill. It
//     still runs its parent's program and holds what it held, and a process read as a fork stays
//     one, its parent gone or not, until its executable or arguments differ from what that read
//     found (INFRA-3368's architect's answer of 2026-11-20T09:24Z on its decision of 08:13Z);
//   - urgent: at a reading at or above the urgent share where the decision holds a process the
//     guard may end that is not new to the stay, the guard ends each process it may end that started
//     during the stay and holds memory of its own, and each that grew since the stay's first kill by
//     more than growthNoise, also where no other reason to act holds in the wait after a kill; and
//     by the urgent rule it ends neither a sleeping fork that ranks at nothing nor the parent, flat
//     since before the stay's first kill (INFRA-3531 R1 of spec version 45; INFRA-3368's
//     architect's decision of 2026-11-20T13:55Z on `4g-r15latepool-r206` at 00405eb3, and decisions
//     of 14:00Z and 14:06Z: a process started during the stay is one the stay's first table did not
//     list). Its cases are started, since, fork and parent (checkUrgent).
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
//     of them lived;
//   - (e) the guard ended a sleeping fork's parent, and at a reading whose table no longer lists
//     the parent counted the fork, which still ran its parent's program, as no fork: its size as
//     of the stay's first kill, from a fork's read, became 0, and all it holds counted as growth.
//
// At 83d6a910 seeds of each of the cases (a) to (c) fail: rule largest in case (b) is r40's shape,
// rule go-on in case (c) r31's. A guard that keeps as a size as of a kill the latest read finished
// before the kill, whatever program it read, passes the seeds of case (a) alone; one that keeps the
// size the decision of 2026-11-20 states passes every seed of those cases. At a28d75a8 three seeds
// fail, each in rule largest, case (d), and none in another rule or case: in seeds 113 and 156 a
// late grower (below) lived, and in seed 122 one of the six that fork in the band, which the guard
// too first read after that kill. With the protected rise and the runs to three readings past the
// last kill (below), seeds 146 and 176 fail so at a28d75a8 too. The guard then ends the parent
// while its sleeping forks live in 74 of the 200 seeds at this change; a guard that tests a fork
// at the measuring reading by its parent alone fails 48 of them, each in rule orphan, case (e),
// and none in another rule or case; one that keeps a fork while it runs the program of its fork's
// read passes them.
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
// their forks are late growers: they start instead at the later of the stay's first kill and the
// last fork in the band, or 1 to 150 ms after it (startLate), each running the grow program from
// its start, at 9 MiB, and step first 250 to 500 ms after it, the grow program's sleep after its
// first block, so the guard reads none of them before that kill (the architect's decision of
// 2026-11-20T08:13Z, item 2: the simulation missed r2's shape, so its model lacked them). In a
// quarter of the seeds, by draws of their own (planRise), the agent grows 0 to 300 ms after that
// later instant by what keeps the reading 1 to 30 MiB at or above the mark once every grower has
// ended, as a rise of Oh My Pi's own does: the guard then ends the probe's parent while its
// sleeping forks live, and they live on as orphans. A run ends once every grower has ended and
// three readings have passed since then and since the guard's last kill, so a kill after the
// growers have ended is judged at the readings after it. Where every seed runs, the test fails if
// no seed ends the parent so, since rule orphan would then check nothing.
//
// Those are the first simSeeds seeds, which run as they did before the burst seeds. The
// simBurstSeeds seeds after them are burst seeds (planBurst), r206's shape, by draws of their own
// (INFRA-3368's architect's decision of 2026-11-20T13:55Z: "a simulation case of a burst of growers
// that start together inside the band and step together after an urgent reading"): the parent
// fills its block over the band's first readings, its three sleeping forks start during the stay,
// and the six growers start together, in the band or after its last reading, and step 64 MiB
// together. At 00405eb3, which ends a grower read at more than one reading of the stay only where
// it grew over the window or since the previous kill, 8 of the 100 burst seeds fail, each in rule
// urgent, case started: in seeds 202, 206, 223, 236, 257, 259, 269 and 270 the guard left, at an
// urgent reading, growers that started during the stay and were not new to it, and that grew
// neither over the window nor, by its sizes, since the previous kill, as r206's guard left four at
// 4109 ms. No seed fails in another rule or case there, and the first 200 pass. The guard ends the
// parent while its sleeping forks live in 14 of the burst seeds, each by rule=largest below the
// urgent share. Run alone, the 300 seeds took 39.9 s at 00405eb3, and the first 200 26.8 s before
// the burst seeds were added.
func TestMemoryGuardKeepsTheRulesOfR1WhereGrowersStepAtRandomInstants(t *testing.T) {
	failed := map[string][]uint64{}
	ran, orphaned := 0, 0
	for seed := uint64(1); seed <= simSeeds+simBurstSeeds; seed++ {
		var found []simFailure
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			run := newSimRun(t, seed).run()
			found = run.check()
			ran++
			if run.orphaned() {
				orphaned++
			}
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
	t.Logf("the guard ended the probe's parent while its sleeping forks lived in %d of %d seeds", orphaned, ran)
	if ran == simSeeds+simBurstSeeds && orphaned == 0 {
		t.Errorf("no seed ended the probe's parent while its sleeping forks lived, so rule orphan checked nothing")
	}
}

const (
	// simSeeds is how many seeds the simulation runs from 1 with the six growers of the model
	// above, and simBurstSeeds how many it runs after them with a burst (planBurst).
	simSeeds      = 200
	simBurstSeeds = 100
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
	// born is a sleeping fork of a burst seed, which forks from the parent during the run, as a
	// grower does, rather than living from its start (planBurst).
	born bool
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
	// simRise is the agent's own rise by: what it holds grows, and the guard may not end it
	// (planRise).
	simRise
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
	// parentFull is when the probe's parent's block was last filled, during the band of a burst
	// seed (planBurst), and 0 in every other seed, whose parent holds its block from the start.
	parentFull time.Duration
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
	if seed > simSeeds {
		s.planBurst(seed)
	} else {
		s.schedule()
		s.planLate(seed)
		s.planRise(seed)
	}
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
		s.events = appendSteps(s.events, s.rng, pid, exec, s.uniform(20*time.Millisecond, 200*time.Millisecond))
	}
	slices.SortStableFunc(s.events, func(x, y simEvent) int { return cmp.Compare(x.at, y.at) })
}

// planLate makes the run's late growers (INFRA-3368's architect's decision of 2026-11-20T08:13Z,
// item 2, on `8g-latepool-r2` at 28732f65, where five of six growers were first read after the
// stay's first kill), by a stream of draws of the seed's own, so that a seed without them runs as
// it did: in half the seeds, the last two to five of the six by their forks do not fork in the band
// but start at the later of the stay's first kill and the last fork in the band, or 1 to 150 ms
// after it (startLate). Each is a child of the parent that runs the grow program from its start, at
// 9 MiB, as r2's growers, subprocesses started after the band's last reading, did, so its first
// read is not a fork's read. The program sleeps 250 ms after its first block, so its first step
// lands 250 to 500 ms after its start, as r2's did, and it then steps as the others do.
func (s *simRun) planLate(seed uint64) {
	rng := rand.New(rand.NewPCG(seed, 0x6c617465))
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
			starts[j] = uniformIn(rng, time.Millisecond, 150*time.Millisecond)
		}
	}
	slices.Sort(starts)
	for j, pid := range late {
		p, start := s.procs[pid], starts[j]
		p.fp.args, p.fp.anon = growArgs, simGrowerBase
		s.late = append(s.late, simEvent{at: start, pid: pid, kind: simStart})
		s.late = appendSteps(s.late, rng, pid, start, uniformIn(rng, 250*time.Millisecond, 500*time.Millisecond))
	}
}

// startLate puts the late growers' changes and the agent's rise (planRise) on the model's time from
// the later of at, the instant of the stay's first kill, and the last fork of the growers that fork
// in the band, so that every grower takes its pid and start time in the order it starts; the guard
// has read none of them before that kill. A start due at once lands before the guard's next read,
// so the reads after the kill at its reading can list it, as they listed r2's 7177 to 7181.
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

// planRise makes the run's protected rise (INFRA-3368's architect's answer of 2026-11-20T09:24Z:
// a process read as a fork stays one, its parent gone or not, until its executable or arguments
// differ from what that read found), by a stream of draws of the seed's own, so that it draws
// nothing from the run's other draws: in a quarter of the seeds, the agent, which the guard may not
// end, grows 0 to 300 ms after the later of the stay's first kill and the last fork in the band
// (startLate) by what keeps the reading 1 to 30 MiB at or above the mark once every grower has
// ended. The guard then ends the probe's parent while its sleeping forks live, and they live on as
// orphans that still run the parent's program.
func (s *simRun) planRise(seed uint64) {
	rng := rand.New(rand.NewPCG(seed, 0x6f727068))
	if rng.IntN(4) != 0 {
		return
	}
	below := share(podLimit, Defaults().ActPercent) - simAgent - latepoolBlock - s.rest
	at := time.Duration(rng.Int64N(int64(300 * time.Millisecond)))
	s.late = append(s.late, simEvent{at: at, pid: 100, kind: simRise, by: below + (1+rng.Uint64N(30))*mib})
}

// planBurst makes a burst seed's run, the shape of `4g-r15latepool-r206` at 00405eb3 (INFRA-3368's
// architect's decision of 2026-11-20T13:55Z: "a simulation case of a burst of growers that start
// together inside the band and step together after an urgent reading"), by a stream of draws of
// the seed's own. At the guard's first reading, in the band, the parent holds 60 MiB or more less
// than its block, and it fills the block in three equal steps 30 to 400 ms into the run, as r206's
// parent filled its 2.4 GB block over the band's first readings; the pod then reads 40 to 260 MiB
// below the mark. Its three sleeping forks fork from it 50 to 400 ms after that, during the stay,
// each with its program and its block, as r206's 7151 to 7153 did at 1773 ms. 200 to 700 ms after
// the last of them the six growers start together, within 20 ms, each a child of the parent that
// runs the grow program from its start, at 9 MiB: in half the seeds the first 64 MiB block of each
// lands 5 to 40 ms after its start, so that the band's last reading came before them and the
// reading at the mark is the first to read them at their block, as in r206; in the other half it
// lands 150 to 350 ms after it, so that the band reads them first. Each then steps 64 MiB every 270
// to 450 ms, the grow program's sleep of 250 ms and its allocation, so that the growers step
// together, as r206's four did within one 131 ms gap after the urgent reading at 4109 ms.
func (s *simRun) planBurst(seed uint64) {
	rng := rand.New(rand.NewPCG(seed, 0x6275727374))
	below := 40 + rng.Uint64N(221)
	fill := 3 * (20 + rng.Uint64N((400-below)/3-19)) * mib
	s.rest = share(podLimit, Defaults().ActPercent) - simAgent - latepoolBlock - below*mib
	parent := s.procs[simBlockHolders[0]]
	parent.fp.anon = latepoolBlock - fill
	s.pod.add(parent.fp)
	fills := make([]time.Duration, 3)
	for i := range fills {
		fills[i] = uniformIn(rng, 30*time.Millisecond, 400*time.Millisecond)
	}
	slices.Sort(fills)
	for _, at := range fills {
		s.events = append(s.events, simEvent{at: at, pid: parent.fp.pid, kind: simStepEnd, by: fill / 3})
	}
	s.parentFull = fills[len(fills)-1]
	forks := make([]time.Duration, len(simBlockHolders)-1)
	for i := range forks {
		forks[i] = s.parentFull + uniformIn(rng, 50*time.Millisecond, 400*time.Millisecond)
	}
	slices.Sort(forks)
	for i, pid := range simBlockHolders[1:] {
		p := s.procs[pid]
		p.alive, p.born = false, true
		s.pod.remove(pid)
		s.events = append(s.events, simEvent{at: forks[i], pid: pid, kind: simFork})
	}
	burst := forks[len(forks)-1] + uniformIn(rng, 200*time.Millisecond, 700*time.Millisecond)
	first, last := 5*time.Millisecond, 40*time.Millisecond
	if rng.IntN(2) == 0 {
		first, last = 150*time.Millisecond, 350*time.Millisecond
	}
	starts := make([]time.Duration, len(simGrowers))
	for i := range starts {
		starts[i] = burst + uniformIn(rng, 0, 20*time.Millisecond)
	}
	slices.Sort(starts)
	for i, pid := range simGrowers {
		s.procs[pid] = &simProcess{
			fp:     fakeProcess{pid: pid, ppid: 700, start: uint64(6000 + i), exe: "/usr/bin/python3.13", args: growArgs, anon: simGrowerBase},
			grower: true,
		}
		s.events = append(s.events, simEvent{at: starts[i], pid: pid, kind: simStart})
		s.events = appendBurstSteps(s.events, rng, pid, starts[i], starts[i]+uniformIn(rng, first, last))
	}
	s.lastFork = starts[len(starts)-1]
	slices.SortStableFunc(s.events, func(x, y simEvent) int { return cmp.Compare(x.at, y.at) })
}

// appendBurstSteps appends to events the 64 MiB steps of the burst grower pid that started at from
// until 8 s of the model's time: the first lands at first, and each next 270 to 450 ms after the
// one before; each is under way 1 to 30 ms before it lands, never before from (planBurst).
func appendBurstSteps(events []simEvent, rng *rand.Rand, pid int, from, first time.Duration) []simEvent {
	for at := first; at < 8*time.Second; at += 250*time.Millisecond + uniformIn(rng, 20*time.Millisecond, 200*time.Millisecond) {
		start := max(from, at-uniformIn(rng, time.Millisecond, 30*time.Millisecond))
		events = append(events, simEvent{at: start, pid: pid, kind: simStepStart}, simEvent{at: at, pid: pid, kind: simStepEnd, by: 64 * mib})
	}
	return events
}

// uniform is a duration drawn evenly from [lo, hi) by the run's draws.
func (s *simRun) uniform(lo, hi time.Duration) time.Duration { return uniformIn(s.rng, lo, hi) }

// uniformIn is a duration drawn evenly from [lo, hi) by rng.
func uniformIn(rng *rand.Rand, lo, hi time.Duration) time.Duration {
	return lo + time.Duration(rng.Int64N(int64(hi-lo)))
}

// appendSteps appends to events the steps of the grower pid until 8 s of the model's time: the
// first lands firstGap after from, and each next 60 to 500 ms after the one before; each is under
// way 1 to 30 ms before it lands, never before from, and grows 8 to 12 MiB. It draws them from rng
// in that order, for schedule from the run's draws and for planLate from the late growers' own.
func appendSteps(events []simEvent, rng *rand.Rand, pid int, from, firstGap time.Duration) []simEvent {
	for at := from + firstGap; at < 8*time.Second; at += uniformIn(rng, 60*time.Millisecond, 500*time.Millisecond) {
		start := max(from, at-uniformIn(rng, time.Millisecond, 30*time.Millisecond))
		by := (8 + rng.Uint64N(5)) * mib
		events = append(events, simEvent{at: start, pid: pid, kind: simStepStart}, simEvent{at: at, pid: pid, kind: simStepEnd, by: by})
	}
	return events
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
	case e.kind == simRise:
		p.fp.anon += e.by
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

// total is the pod's reading: the agent, at what it holds after any rise of its own, what no
// process holds, the block once while a process that holds it lives, at what its largest holder
// holds, short of the block while a burst seed's parent fills it (planBurst), and each grower's own
// memory once it runs its program.
func (s *simRun) total() uint64 {
	total := s.procs[100].fp.anon + s.rest
	var block uint64
	for _, pid := range simBlockHolders {
		if p := s.procs[pid]; p.alive {
			block = max(block, p.fp.anon)
		}
	}
	total += block
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

// run takes the readings until every grower has forked and ended and three readings have passed
// since then and since the guard's last kill, or simReadings have passed, and gives each kill the
// rule of its report.
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
		switch {
		case len(s.killsAt(len(s.readings)-1)) > 0:
			after = 0
		case s.vt > s.lastFork && !slices.ContainsFunc(simGrowers, func(pid int) bool { return s.procs[pid].alive }):
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

// aliveAt is whether pid lives at the model's instant at: it has forked by then, where it is a
// grower or a burst seed's sleeping fork, and no kill before it ended it.
func (s *simRun) aliveAt(pid int, at time.Duration) bool {
	if p := s.procs[pid]; (p.grower || p.born) && (!p.forked || p.forkAt > at) {
		return false
	}
	return !slices.ContainsFunc(s.kills, func(k simKill) bool { return k.pid == pid && k.at <= at })
}

// asOfKill is pid's size as of the kills of reading k, from which the rules measure growth since
// them, and the case of a guard that measured from a read made after the kill: false where pid is
// left out, never read before the kill or, at the guard's last read of it by the end of reading k,
// still running another program, the program it runs as far as the guard knows. Where fromNothing
// is set, a process never read before the kill whose first read after it ran its own program has 0
// (case d): an exec lands at the end of the reading it falls in, so none of its reads at that
// reading is a fork's read, its parent read there before or after it, and the order's third tier
// counts it from 0 from the stay's first kill (firstReadSince; INFRA-3368's architect's decision of
// 2026-11-20T08:13Z, item 2).
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

// check judges the run by the five rules, following the guard's phase in each rise and its stays
// in the band from the readings, the kills and the stopped lines.
func (s *simRun) check() []simFailure {
	d := Defaults()
	mark, band, floor := share(podLimit, d.ActPercent), bandStart(podLimit, d.ActPercent), share(podLimit, d.FloorPercent)
	urgent := urgentShare(podLimit, d.ActPercent)
	var out []simFailure
	// stay is the stay's first reading, -1 outside a stay.
	phase, stayFirst, rise, stay := armed, -1, -1, -1
	for i, r := range s.readings {
		kills := s.killsAt(i)
		if phase != armed && r.reading < mark {
			phase = armed
		}
		if phase == armed && r.reading < band {
			stay = -1
		} else if stay < 0 && phase != stopped && r.reading >= band {
			stay = i
		}
		// acting is whether the guard decides at the reading, which it does not once it has given
		// the rise up, and held whether it decides there on the band's held reads, as the first
		// reading at the mark does.
		acting, held := phase != stopped, phase == armed
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
		if acting && r.reading >= urgent {
			out = append(out, s.checkUrgent(i, stay, stayFirst, held)...)
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
		for _, k := range kills {
			if k.rule == ruleGrewSinceKill && slices.Contains(simBlockHolders[1:], k.pid) && !s.aliveAt(simBlockHolders[0], k.at) {
				out = append(out, simFailure{rule: "orphan", cause: "e", what: fmt.Sprintf(
					"at the reading at %s, %d MiB, the guard ended pid %d, a sleeping fork that still ran its parent's program, by rule=grew-since-kill after it had ended its parent, pid %d",
					simMS(r.now), r.reading/mib, k.pid, simBlockHolders[0])})
			}
		}
		if r.stopped {
			phase, stayFirst, stay = stopped, -1, -1
		}
	}
	return out
}

// checkUrgent judges reading i, at or above the urgent share and one the guard decides at, by R1's
// urgent clause of spec version 45 (INFRA-3368's architect's decision of 2026-11-20T13:55Z on r206
// and decisions of 14:00Z and 14:06Z), in the stay whose first reading is stay and whose first kill
// was at reading stayFirst, -1 before it; held is whether the guard decides at i on the band's held
// reads. Where the decision holds a process the guard may end that is not new to the stay, the
// guard ends at that reading, by any rule, each process it may end that started during the stay and
// holds memory of its own (case started) and each that grew since the stay's first kill by more
// than growthNoise (case since); at a reading of the wait after a kill each is a reason to act, as
// a process new to the stay is. By the urgent rule it ends neither a sleeping fork that ranks at
// nothing beside its living parent (case fork) nor the parent where it has been flat since before
// the stay's first kill and over the window and is not new to the stay (case parent).
//
// It judges by what the guard can know at i, so that it asks nothing the guard may not know:
//   - the decision holds a process that is not new where a block holder, whose reads never fail
//     and which the processes it may end then hold the floor by, was read at the two readings before
//     i and at i;
//   - a process counts only where it lives at the reading, every read of it at i read and, where
//     held, a read of it at the reading before read, so that the decision did not leave it out; its
//     size is the least of those reads, and it runs its own program where all of them did;
//   - it started during the stay where it forked after the end of the stay's first reading, whose
//     table so cannot list it, and it holds memory of its own where it is a grower that runs the
//     grow program, which is no fork of its parent: a process that forked during that first
//     reading, which its table may list, is not judged by case started;
//   - its growth since the stay's first kill is from its size as of that kill (asOfKill), 0 where
//     the guard never read it before that kill and its first read after it ran its own program, as
//     the order's third tier counts it; that size is never below the guard's own.
func (s *simRun) checkUrgent(i, stay, stayFirst int, held bool) []simFailure {
	if stay < 0 || i < stay+2 {
		return nil
	}
	r := s.readings[i]
	kills := s.killsAt(i)
	parent := simBlockHolders[0]
	var out []simFailure
	for _, k := range kills {
		if k.rule != ruleUrgent {
			continue
		}
		_, before := s.kept(parent, i-1)
		_, at := s.kept(parent, i)
		switch {
		case slices.Contains(simBlockHolders[1:], k.pid) && s.aliveAt(parent, r.now) && (before || at):
			out = append(out, simFailure{rule: "urgent", cause: "fork", what: fmt.Sprintf(
				"at the reading at %s, %d MiB, the guard ended pid %d, a sleeping fork that ranks at nothing beside its living parent, by rule=urgent",
				simMS(r.now), r.reading/mib, k.pid)})
		case k.pid == parent && s.parentSpared(i, stay, stayFirst):
			out = append(out, simFailure{rule: "urgent", cause: "parent", what: fmt.Sprintf(
				"at the reading at %s, %d MiB, the guard ended pid %d, the probe's parent, flat since %s, before the stay's first kill and the window, and not new to the stay, by rule=urgent",
				simMS(r.now), r.reading/mib, k.pid, simMS(s.parentFull))})
		}
	}
	history := slices.ContainsFunc(simBlockHolders, func(q int) bool {
		for j := i - 2; j <= i; j++ {
			if _, ok := s.kept(q, j); !ok {
				return false
			}
		}
		return true
	})
	if !history {
		return out
	}
	for _, q := range simEndable {
		if !s.aliveAt(q, r.now) || slices.ContainsFunc(kills, func(k simKill) bool { return k.pid == q }) {
			continue
		}
		size, own, ok := s.decided(q, i, held)
		if !ok {
			continue
		}
		p := s.procs[q]
		if p.grower && own && p.forkAt > s.readings[stay].end {
			out = append(out, simFailure{rule: "urgent", cause: "started", what: fmt.Sprintf(
				"at the reading at %s, %d MiB, at or above the urgent share in a stay with history, the guard did not end pid %d, which started at %s, during the stay that began at %s, and holds memory of its own (read %d MiB)",
				simMS(r.now), r.reading/mib, q, simMS(p.forkAt), simMS(s.readings[stay].now), size/mib)})
			continue
		}
		if stayFirst < 0 || stayFirst >= i {
			continue
		}
		if from, _, known := s.asOfKill(q, stayFirst, true); known && size > from+growthNoise {
			first := s.killsAt(stayFirst)[0]
			out = append(out, simFailure{rule: "urgent", cause: "since", what: fmt.Sprintf(
				"at the reading at %s, %d MiB, at or above the urgent share in a stay with history, the guard did not end pid %d, which had grown %d MiB since the stay's first kill, of pid %d (rule=%s) at %s (read %d MiB, %d MiB as of that kill)",
				simMS(r.now), r.reading/mib, q, (size-from)/mib, first.pid, first.rule, simMS(s.readings[stayFirst].now), size/mib, from/mib)})
		}
	}
	return out
}

// decided is pid's size as the decision at reading i may have taken it, the least of its reads at
// i and, where held, at the reading before, and whether each of those ran its own program; false
// where it has no read at i, where a read of it at i failed, or where held and every read of it at
// the reading before failed, so that the decision may have left it out (checkUrgent).
func (s *simRun) decided(pid, i int, held bool) (size uint64, own, ok bool) {
	atI, before, beforeOK := false, false, false
	size, own = math.MaxUint64, true
	for _, r := range s.readsOf(pid) {
		switch {
		case r.reading == i && !r.ok:
			return 0, false, false
		case r.reading == i:
			atI = true
		case held && r.reading == i-1:
			before = true
			if !r.ok {
				continue
			}
			beforeOK = true
		default:
			continue
		}
		size, own = min(size, r.size), own && r.own
	}
	if !atI || before && !beforeOK {
		return 0, false, false
	}
	return size, own, true
}

// parentSpared is whether the urgent rule may not end the probe's parent at reading i of the stay
// whose first reading is stay and whose first kill was at reading stayFirst, -1 before it: the two
// readings before i read it, so it is not new to the stay, and its block was last filled
// (parentFull) more than growthWindow before i, so it did not grow over the window, and before the
// reading before the stay's first kill, so it has not grown since that kill (checkUrgent). The
// stay's first table lists it, since it lives from the run's start.
func (s *simRun) parentSpared(i, stay, stayFirst int) bool {
	if i < stay+2 {
		return false
	}
	for j := i - 2; j < i; j++ {
		if _, ok := s.kept(simBlockHolders[0], j); !ok {
			return false
		}
	}
	flat := s.readings[i].now - growthWindow
	if stayFirst >= 0 && stayFirst < i {
		flat = min(flat, s.readings[max(stay, stayFirst-1)].now)
	}
	return s.parentFull < flat
}

// orphaned is whether the guard ended the probe's parent while one of its sleeping forks lived, so
// that rule orphan judged the run.
func (s *simRun) orphaned() bool {
	for _, k := range s.kills {
		if k.pid == simBlockHolders[0] {
			return slices.ContainsFunc(simBlockHolders[1:], func(pid int) bool { return s.aliveAt(pid, k.at) })
		}
	}
	return false
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
