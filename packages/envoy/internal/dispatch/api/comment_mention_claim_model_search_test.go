//go:build !race

package api

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The sender claim model's search (TestTheMentionSenderClaimModel; comment_mention_claim_model_test.go
// and the run, steps and checks files beside it hold the model itself): the key a run's state is
// known by, the walks that take every step of every state a variant reaches, each walk a run in a
// bubble of its own, the breadth-first reading of the graph they drew, and the test that runs the
// sender as built and its control.

// key is the run's state as the search keys on it: every claim in play named by its order among
// them and its age up to a lease and a slot, then the model's rows, the sender's goroutines in the
// order they started - with what each holds in its own variables, the lane it serves, the send it
// carries and an answer it has not acted on yet - their tries, the refresh, the sweep's hand-offs
// and the park timer, and the sender's maps. It is written into the run's own buffer, which the
// next key overwrites.
func (r *cmRun) key() []byte {
	r.lock.Mutex.Lock()
	defer r.lock.Mutex.Unlock()
	k := &r.k
	k.b, k.at, k.claims = k.b[:0], k.at[:0], k.claims[:0]
	k.int(r.slot)
	k.bool(r.published)
	k.bool(r.publishedBetween)
	k.int(len(r.took))
	for _, c := range r.took {
		k.int(c)
	}
	k.bool(r.sweptFirst)
	k.int(len(r.broke))
	for _, kind := range r.broke {
		k.str(kind)
	}
	k.int(r.readFailures)
	k.int(r.outages)
	for _, row := range r.rows {
		k.bool(row.committed)
		k.bool(row.pending)
		k.claim(row.created)
		k.claim(row.claim)
	}
	// A goroutine as it can still act: the refresh by the slot it started in, which holds time
	// back; a lane goroutine by its lane and its park's pending timer, and, away from next, by
	// whether the try of the send it carries answered and by that send. At next a lane goroutine
	// holds nothing else (serve's item is next's to hand it), so the key carries nothing else of
	// it there: whatever it last carried is history no later step reads, and two runs that differ
	// only in it are one state.
	k.int(len(r.gs))
	for _, g := range r.gs {
		at := "statement"
		if g.lock != nil {
			at = g.site
		}
		switch {
		case g.refresh:
			k.int(0)
			k.bool(g.born < r.slot)
			k.str(at)
		case g.atNext():
			k.int(1)
			k.str(at)
			k.lane(g.lane)
			k.bool(g.givenUp)
		default:
			k.int(2)
			k.str(at)
			k.lane(g.lane)
			k.bool(g.givenUp)
			k.bool(g.tried)
			k.send(g.carried)
		}
		k.bool(g.ended != nil)
		if g.ended != nil {
			k.send(g.ended.answer.item)
		}
		k.read(g.answered)
		if g.answered != nil {
			k.bool(g.readFailed)
		}
	}
	k.bool(r.refresh != nil)
	if f := r.refresh; f != nil {
		k.bool(f.named)
		k.read(f.read)
		k.bool(f.stmt != nil)
		if f.stmt != nil {
			k.named(f.stmt.queued)
			k.bool(f.stmt.wrote)
			k.named(f.stmt.written)
			k.bool(f.stmt.answered)
			k.bool(f.stmt.lost)
		}
	}
	for c := range 2 {
		try := r.tries[c]
		k.bool(try != nil)
		if try != nil {
			k.int(slices.Index(r.gs, try.g))
			k.bool(try.began < r.slot)
			k.read(try.read)
			k.send(try.item)
			k.bool(try.ran)
			k.bool(try.took)
			k.send(try.answer.item)
			k.bool(try.answer.waits)
		}
		sweep := r.sweeps[c]
		k.bool(sweep != nil)
		if sweep != nil {
			k.send(sweep.item)
		}
	}
	k.bool(r.timer != nil)
	if r.timer != nil {
		k.bool(r.timer.began < r.slot)
	}
	// Each attempt's lost count is keyed as how many re-reads in flight named it under another
	// count, which is all claimKnown and rereadUnsure compare. The sender holds only the two
	// comments' attempts, so each is keyed in the comments' order.
	reads := r.readsInFlight()
	k.int(len(r.sender.held))
	written := 0
	for c := range cmComments {
		state, ok := r.sender.held[cmItemKeys[c]]
		if !ok {
			continue
		}
		written++
		k.int(c)
		k.claim(state.claimed)
		k.bool(state.unsure)
		k.bool(state.trying)
		moved := 0
		for _, read := range reads {
			if began, named := read.lost[cmComments[c]]; named && began != state.lost {
				moved++
			}
		}
		k.int(moved)
	}
	if written != len(r.sender.held) {
		r.failModel("the sender holds an attempt of neither comment: %v", r.sender.held)
	}
	// The sender's lanes and waits in the order of their keys, so two runs that hold the same ones
	// write them the same.
	k.lanes = k.lanes[:0]
	for lane := range r.sender.lanes {
		k.lanes = append(k.lanes, lane)
	}
	slices.SortFunc(k.lanes, cmLaneOrder)
	k.int(len(k.lanes))
	for _, lane := range k.lanes {
		k.lane(lane)
		sends := r.sender.lanes[lane]
		k.int(len(sends))
		for _, s := range sends {
			k.send(s)
		}
	}
	k.lanes = k.lanes[:0]
	for lane := range r.sender.waits {
		k.lanes = append(k.lanes, lane)
	}
	slices.SortFunc(k.lanes, cmLaneOrder)
	k.int(len(k.lanes))
	for _, lane := range k.lanes {
		k.lane(lane)
		k.int(int(r.sender.waits[lane].by.EventID))
	}
	k.int(len(r.sender.ready))
	for _, lane := range r.sender.ready {
		k.lane(lane)
	}
	k.int(r.sender.serving)
	return k.done(r)
}

// cmKey is the buffer a run's key is written into: each field as bytes of its own, every list
// and string after its length, so no two states write one key, and each claim as two bytes that
// done fills in once every claim is written - its order among the claims the key writes and its
// age - so the key names the claims it writes, and only those, as the code compares them.
type cmKey struct {
	b []byte
	// at is where each claim written is in b, and claims each claim; order is done's scratch,
	// and lanes key's.
	at            []int
	claims, order []time.Time
	lanes         []laneKey
}

func (k *cmKey) int(v int) {
	k.b = binary.AppendVarint(k.b, int64(v))
}

func (k *cmKey) bool(v bool) {
	if v {
		k.b = append(k.b, 1)
	} else {
		k.b = append(k.b, 0)
	}
}

func (k *cmKey) str(s string) {
	k.int(len(s))
	k.b = append(k.b, s...)
}

// claim writes claim's two bytes: none, or the place done fills in.
func (k *cmKey) claim(claim time.Time) {
	if claim.IsZero() {
		k.b = append(k.b, 0xff, 0xff)
		return
	}
	k.at = append(k.at, len(k.b))
	k.claims = append(k.claims, claim)
	k.b = append(k.b, 0, 0)
}

// named writes claims, each by its comment, in the comments' order.
func (k *cmKey) named(claims []mentionClaim) {
	k.int(len(claims))
	for c, comment := range cmComments {
		for _, claim := range claims {
			if claim.key.comment == comment {
				k.int(c)
				k.claim(claim.claimedAt)
			}
		}
	}
}

// read writes a re-read: the claims it names, whether it ran, and what it found.
func (k *cmKey) read(read *cmRead) {
	k.bool(read != nil)
	if read != nil {
		k.named(read.named)
		k.bool(read.ran)
		k.named(read.found)
	}
}

// send writes a send as the sender reads it: its comment and claim, what it last waited behind,
// and whether it was stamped as waiting and warned of, which a late WARN reads (reportLate).
func (k *cmKey) send(s mentionSend) {
	k.str(s.comment.ID)
	k.claim(s.claimed)
	k.str(s.heldFor)
	k.int(int(s.heldBy.EventID))
	k.bool(!s.heldSince.IsZero())
	k.bool(s.warned)
}

func (k *cmKey) lane(lane laneKey) {
	k.str(lane.recipient)
	k.str(lane.owner)
	k.bool(lane.held)
}

// done fills in each claim's two bytes and answers the key.
func (k *cmKey) done(r *cmRun) []byte {
	k.order = append(k.order[:0], k.claims...)
	slices.SortFunc(k.order, time.Time.Compare)
	k.order = slices.CompactFunc(k.order, time.Time.Equal)
	if len(k.order) >= 0xff {
		r.failModel("%d claims in play, more than a key counts", len(k.order))
	}
	for i, at := range k.at {
		order, _ := slices.BinarySearchFunc(k.order, k.claims[i], time.Time.Compare)
		k.b[at], k.b[at+1] = byte(order), byte(min(r.age(k.claims[i]), cmLapseSlots+1))
	}
	return k.b
}

func cmLaneOrder(a, b laneKey) int {
	if c := strings.Compare(a.recipient, b.recipient); c != 0 {
		return c
	}
	if c := strings.Compare(a.owner, b.owner); c != 0 {
		return c
	}
	switch {
	case a.held == b.held:
		return 0
	case b.held:
		return -1
	}
	return 1
}

// cmResult is what one search found: the states it reached, the longest run, and, by check, how
// many states broke it first and the trace of the first of them, a shortest one.
type cmResult struct {
	variant  cmVariant
	periods  int
	states   int
	depth    int
	found    map[string]int
	first    map[string][]string
	duration time.Duration
}

func (r cmResult) String() string {
	return fmt.Sprintf("| %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %s |", r.variant.name, r.periods, r.states,
		r.depth, r.found["stale"], r.found["dropped"], r.found["order"], r.found["twice"], r.found["unnamed"],
		r.found["lapsed"], r.duration.Round(time.Millisecond))
}

// cmVisit is what one run came to: the SHA-256 of its state's key, the checks its last step broke,
// the steps it may take next and their digest (cmStepsDigest), the model's own fault, if any, and,
// from a run with its trace on, the run's trace.
type cmVisit struct {
	key          [sha256.Size]byte
	fault        string
	fresh, trace []string
	enabled      []cmStep
	digest       uint64
}

// visit reads the run's key before its fault, since the key can fail the model.
func (r *cmRun) visit() cmVisit {
	key := sha256.Sum256(r.key())
	enabled := r.enabled()
	return cmVisit{key: key, fault: r.fault, fresh: r.fresh, enabled: enabled, digest: cmStepsDigest(enabled), trace: r.trace}
}

// cmStepsDigest is a 64-bit FNV-1a digest of steps, each step's kind, comment and goroutine in
// turn. A state's node keeps it, so that every run at the state is held to the steps the first run
// there had, not only to as many: the search keeps a state's edges by their place in its list of
// steps, and two lists of one length with other steps in them would have one step taken twice
// and another never, with every edge filled and nothing failing.
func cmStepsDigest(steps []cmStep) uint64 {
	digest := uint64(14695981039346656037)
	for _, step := range steps {
		for _, field := range [3]uint64{uint64(step.kind), uint64(step.comment), uint64(step.g)} {
			for range 8 {
				digest = (digest ^ field&0xff) * 1099511628211
				field >>= 8
			}
		}
	}
	return digest
}

// cmVisitOf replays steps in a bubble of its own, writing the run's trace when tracing.
func cmVisitOf(t *testing.T, v cmVariant, periods int, steps []cmStep, tracing bool) (visit cmVisit) {
	synctest.Test(t, func(*testing.T) {
		r := newCmRun(v, periods, tracing)
		defer r.shutdown()
		for _, s := range steps {
			r.apply(s)
		}
		visit = r.visit()
	})
	return visit
}

// cmKinds is every check a step may break, each one bit of cmEdge.fresh: cmChecks, and the order
// after a lapsed claim that the table counts apart (deliver). cmExplore refuses more kinds than
// the bits hold.
var cmKinds = append(slices.Clip(cmChecks), "lapsed")

// cmKindBits is kinds as their bits in cmKinds. A kind cmKinds does not name panics, naming it,
// rather than count nothing.
func cmKindBits(kinds []string) uint8 {
	var bits uint8
	for _, kind := range kinds {
		at := slices.Index(cmKinds, kind)
		if at < 0 {
			panic("the claim model: a step broke the check " + kind + ", which cmKinds does not name")
		}
		bits |= 1 << at
	}
	return bits
}

// cmNode is one state the search reached: the state and its step that the shortest road a run has
// taken to it so far ends with, which every replay of this state retraces, and that road's length
// when it was taken, always more than its parent's, so the road ends at the start; where its steps'
// edges begin in cmSearch.edges, how many it has, how many of them runs have taken, and their
// digest (cmStepsDigest).
type cmNode struct {
	parent       int32
	step         cmStep
	edges        int32
	road         int32
	digest       uint64
	steps, taken uint8
}

// cmEdge is one step a state may take: the state it reaches, -1 until a run takes it, and the
// checks the step broke that its run had not broken before, by their bits in cmKinds.
type cmEdge struct {
	to    int32
	fresh uint8
}

// cmSearch is one variant's search under way: every state reached, by its key's SHA-256, with its
// steps; the states with a step no run has taken yet, oldest first; how many walks are under way
// and how many workers wait for a state to walk from; and what stopped the search early, if
// anything did, with the steps of the run that met it.
type cmSearch struct {
	t       *testing.T
	v       cmVariant
	periods int
	mu      sync.Mutex
	more    sync.Cond
	ids     map[[sha256.Size]byte]int32
	nodes   []cmNode
	edges   []cmEdge
	queue   []int32
	walking int
	idle    int
	stopped bool
	// failure is the model's own fault, which fault says, or a run that reached another state than
	// the one it retraced; key is the faulting run's.
	failure struct {
		message string
		fault   bool
		steps   []cmStep
		key     [sha256.Size]byte
	}
}

// stop ends the search at its first failure.
func (s *cmSearch) stop(message string, fault bool, steps []cmStep, key [sha256.Size]byte) {
	if !s.stopped {
		s.stopped = true
		s.failure.message, s.failure.fault, s.failure.steps, s.failure.key = message, fault, slices.Clone(steps), key
	}
	s.more.Broadcast()
}

// path is the steps from the start to state id that every replay of it takes.
func (s *cmSearch) path(id int32) []cmStep {
	var steps []cmStep
	for ; id != 0; id = s.nodes[id].parent {
		steps = append(steps, s.nodes[id].step)
	}
	slices.Reverse(steps)
	return steps
}

// reach is the state of visit, which a run reached from parent by step, by the steps of road: a
// state reached before must have the same steps (its digest, cmStepsDigest), and takes this road
// when it is shorter than the one it had; a new one joins the queue. Every replay of a state takes
// its road, and every road to a state reaches it, since the key is all that a run's future depends
// on (walk), so a shorter road is a shorter replay: a state a walk reached first is often far
// deeper on that walk's road than on another. A run that breaks either rule stops the search, with
// road for its traced replay, and reach records nothing of it.
func (s *cmSearch) reach(parent int32, step cmStep, visit cmVisit, road []cmStep) int32 {
	var length int32
	if parent >= 0 {
		length = s.nodes[parent].road + 1
	}
	if id, ok := s.ids[visit.key]; ok {
		switch node := &s.nodes[id]; {
		case int(node.steps) != len(visit.enabled) || node.digest != visit.digest:
			s.stop("the model is not deterministic: two runs at one state may take different steps", false,
				road, visit.key)
		case length < node.road:
			node.parent, node.step, node.road = parent, step, length
		}
		return id
	}
	if len(visit.enabled) > 255 {
		s.stop(fmt.Sprintf("a state has %d steps, more than the search counts", len(visit.enabled)), false,
			road, visit.key)
		return -1
	}
	id := int32(len(s.nodes))
	s.ids[visit.key] = id
	s.nodes = append(s.nodes, cmNode{parent: parent, step: step, edges: int32(len(s.edges)), road: length,
		digest: visit.digest, steps: uint8(len(visit.enabled))})
	for range visit.enabled {
		s.edges = append(s.edges, cmEdge{to: -1})
	}
	if len(visit.enabled) > 0 {
		s.queue = append(s.queue, id)
		if s.idle > 0 {
			s.more.Signal()
		}
	}
	return id
}

// take is a step of state id no run has taken yet, which the caller's run now takes.
func (s *cmSearch) take(id int32) (int, bool) {
	node := &s.nodes[id]
	if node.taken == node.steps {
		return 0, false
	}
	node.taken++
	return int(node.taken) - 1, true
}

// next is the oldest state with a step no run has taken, and that step. It waits while a walk
// under way may still reach one, and answers false once none can, or once the search stopped.
func (s *cmSearch) next() (int32, int, bool) {
	for !s.stopped {
		for len(s.queue) > 0 {
			id := s.queue[0]
			j, ok := s.take(id)
			if !ok || s.nodes[id].taken == s.nodes[id].steps {
				s.queue = s.queue[1:]
			}
			if ok {
				if len(s.queue) > 0 && s.idle > 0 {
					s.more.Signal()
				}
				return id, j, true
			}
		}
		if s.walking == 0 {
			return 0, 0, false
		}
		s.idle++
		s.more.Wait()
		s.idle--
	}
	return 0, 0, false
}

// work walks from the oldest state with a step no run has taken until no state has one. It holds
// the search's lock except while it walks, and takes no deferred unlock: a walk that panics, as a
// bubble that deadlocks does, would have it unlock a mutex it does not hold and hide the panic.
func (s *cmSearch) work() {
	s.mu.Lock()
	for {
		from, j, ok := s.next()
		if !ok {
			s.mu.Unlock()
			return
		}
		steps := s.path(from)
		s.walking++
		s.mu.Unlock()
		s.walk(from, steps, j)
		s.mu.Lock()
		s.walking--
		if s.walking == 0 {
			s.more.Broadcast()
		}
	}
}

// walk replays steps, state from's road, in a bubble of its own, which must reach that state again,
// and takes its step j; then, from each state it reaches, a step no run has taken yet, until it
// reaches a state whose steps runs have all taken. A run that reached a state by another road than
// the one a replay takes is as good a run there, since the key is all that a run's future depends
// on - which the search's merging of the two states already rests on, and which each replay
// checks.
func (s *cmSearch) walk(from int32, steps []cmStep, j int) {
	synctest.Test(s.t, func(*testing.T) {
		r := newCmRun(s.v, s.periods, false)
		defer r.shutdown()
		// road is every step this run took, which a failure replays with its trace on; steps is
		// this walk's own (path).
		road := steps
		for _, step := range steps {
			r.apply(step)
		}
		at := r.visit()
		s.mu.Lock()
		if id, ok := s.ids[at.key]; !ok || id != from || len(at.enabled) != int(s.nodes[from].steps) ||
			at.digest != s.nodes[from].digest {
			s.stop("the model is not deterministic: a replay of a road a run took to a state reached another", false,
				road, at.key)
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		enabled := at.enabled
		for cur := from; ; {
			step := enabled[j]
			r.apply(step)
			road = append(road, step)
			visit := r.visit()
			s.mu.Lock()
			if s.stopped {
				s.mu.Unlock()
				return
			}
			if visit.fault != "" {
				s.stop(visit.fault, true, road, visit.key)
				s.mu.Unlock()
				return
			}
			to := s.reach(cur, step, visit, road)
			if s.stopped {
				s.mu.Unlock()
				return
			}
			s.edges[s.nodes[cur].edges+int32(j)] = cmEdge{to: to, fresh: cmKindBits(visit.fresh)}
			next, ok := s.take(to)
			s.mu.Unlock()
			if !ok {
				return
			}
			cur, j, enabled = to, next, visit.enabled
		}
	})
}

// cmTracedPicks replays, with its trace on, the run that takes the picks-th step of each state in
// turn, and answers its visit.
func cmTracedPicks(t *testing.T, v cmVariant, periods int, picks []int) (visit cmVisit) {
	synctest.Test(t, func(*testing.T) {
		r := newCmRun(v, periods, true)
		defer r.shutdown()
		for _, pick := range picks {
			enabled := r.enabled()
			if pick >= len(enabled) {
				r.failModel("the model is not deterministic: a traced replay may take %d steps, not more than %d", len(enabled), pick)
				break
			}
			r.apply(enabled[pick])
		}
		visit = r.visit()
	})
	return visit
}

// cmExplore searches every state of v within periods. No run can be copied - the sender's
// goroutines are in it - so a state's every step needs a run at that state, and a run at a state
// is a replay of steps from the start: a search that replays a run for each step of each state,
// breadth first, takes about as many steps as the edges times their depth. Instead each run walks
// on from the state its step reaches as long as that state has a step no run took yet, and only a
// walk that reaches a state whose steps are all taken ends, so a replay is made for each such end
// rather than for each step, and it takes the shortest road the walks have found to its state
// (reach). At most 16 walk at once. The walks find every state and every step in an order of
// their own, so the graph they draw is then read breadth first, each state's steps in the order
// the model gives them, as one breadth-first search would have found them: the first trace of
// each check is a shortest one and the same on every run, and the states that broke a check first
// are counted where that search would count them. The search keeps a state's SHA-256 rather than
// its key, and replays with its trace on only the run that first breaks a check, or that fails
// the model; the replay must reach the same state.
func cmExplore(t *testing.T, v cmVariant, periods int) cmResult {
	began := time.Now()
	if len(cmKinds) > 8 {
		t.Fatalf("the model has %d kinds of check, and a step's edge keeps them in 8 bits (cmEdge.fresh)", len(cmKinds))
	}
	result := cmResult{variant: v, periods: periods, found: map[string]int{}, first: map[string][]string{}}
	root := cmVisitOf(t, v, periods, nil, false)
	if root.fault != "" {
		t.Fatalf("%s, at the start", cmVisitOf(t, v, periods, nil, true).fault)
	}
	s := &cmSearch{t: t, v: v, periods: periods, ids: map[[sha256.Size]byte]int32{}}
	s.more.L = &s.mu
	s.reach(-1, cmStep{}, root, nil)
	var workers sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), 16) {
		workers.Go(s.work)
	}
	workers.Wait()
	if f := s.failure; f.message != "" {
		visit := cmVisitOf(t, v, periods, f.steps, true)
		if f.fault && visit.key != f.key {
			t.Fatalf("the model is not deterministic: a replay with its trace on reached another state, after:\n\t%s",
				strings.Join(visit.trace, "\n\t"))
		}
		if f.fault {
			f.message = visit.fault
		}
		t.Fatalf("%s, after:\n\t%s", f.message, strings.Join(visit.trace, "\n\t"))
	}
	// The breadth-first reading: depth is each state's distance from the start, and via the state
	// and the step breadth-first order first reached it by.
	depth := make([]int32, len(s.nodes))
	via := make([][2]int32, len(s.nodes))
	for i := range depth {
		depth[i] = -1
	}
	depth[0] = 0
	firsts := map[string]int32{}
	count := func(to int32, fresh uint8) {
		for k, kind := range cmKinds {
			if fresh&(1<<k) != 0 {
				result.found[kind]++
				if _, ok := firsts[kind]; !ok {
					firsts[kind] = to
				}
			}
		}
	}
	count(0, cmKindBits(root.fresh))
	order := make([]int32, 1, len(s.nodes))
	for i := 0; i < len(order); i++ {
		id := order[i]
		node := s.nodes[id]
		for j := range int32(node.steps) {
			edge := s.edges[node.edges+j]
			if edge.to < 0 {
				t.Fatalf("the search left a step of a state untaken")
			}
			if depth[edge.to] >= 0 {
				continue
			}
			depth[edge.to] = depth[id] + 1
			via[edge.to] = [2]int32{id, j}
			result.depth = int(depth[edge.to])
			count(edge.to, edge.fresh)
			order = append(order, edge.to)
		}
	}
	result.states = len(order)
	for kind, id := range firsts {
		picks := make([]int, depth[id])
		for at := id; at != 0; at = via[at][0] {
			picks[depth[at]-1] = int(via[at][1])
		}
		visit := cmTracedPicks(t, v, periods, picks)
		if reached, ok := s.ids[visit.key]; !ok || reached != id || visit.fault != "" {
			message := "the model is not deterministic: a replay with its trace on reached another state"
			if visit.fault != "" {
				message = visit.fault
			}
			t.Fatalf("%s, after:\n\t%s", message, strings.Join(visit.trace, "\n\t"))
		}
		result.first[kind] = visit.trace
	}
	result.duration = time.Since(began)
	return result
}

// The sender's claim model: the sender as built must keep every claim it trusts in step with its
// row, never let a send it holds go with its attempt pending, never report a held send behind no
// event, and never let s1 take A-2 first or a comment twice - with any refresh's answer lost and
// two re-reads failed, and, in the full run, with up to two refreshes failed before they commit,
// so that a claim may lapse. Two controls show the model sees each failure: a sender whose re-reads
// learn nothing of a lost refresh must trust a stale claim, and, in the full run, a refresh that
// writes a claim even once the claim it names has lapsed and a take that counts the row's claim its
// own only while the claim it carries is live, under the same failures, must trust a stale claim,
// drop a send and let A-2 pass A-1. After a claim lapses the delivery sweep keeps no order between
// two comments, which the spec puts outside this change: an A-2 the sweep sends first, or one the
// lane sends after the sweep took A-1, is counted in its own column of the table, and fails
// nothing.
//
// CI runs three periods (the envoy-go-claim-model job), which is enough for a refresh to re-read
// and write a row between a try's answer and its park and for a send to park after a failed read;
// the full run (ENVOY_MENTION_ORDER_MODEL=full) adds the outage pair at four periods, enough for a
// claim to lapse and the sweep to hand its send back, and CI guards what only they reach with
// TestALostRefreshNeverRevivesALapsedClaim and TestAHandOffDuringASendsTryIsTriedAgainUnderItsClaim
// on the database and TestAHandOffAfterTheRetryCheckIsNotDropped on a sender with stub statements.
func TestTheMentionSenderClaimModel(t *testing.T) {
	// Not parallel: it swaps the default logger (slog.SetDefault).
	if model.DeliveryClaimLapse != cmLapsePeriods*mentionClaimRefresh {
		t.Fatalf("the model counts a claim's lease as %d refresh periods; model.DeliveryClaimLapse is %v and mentionClaimRefresh %v",
			cmLapsePeriods, model.DeliveryClaimLapse, mentionClaimRefresh)
	}
	// The sender logs every lost answer and failed read at ERROR, which the model makes by the
	// thousand. A handler that is never enabled drops each record before slog builds it; a text
	// handler writing to io.Discard formatted every one first.
	logs := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(logs) })
	type modelRun struct {
		v       cmVariant
		periods int
		// breaks is what a control must break; nil for the sender as built, which must break none.
		breaks []string
	}
	runs := []modelRun{
		{v: cmAsBuilt, periods: 3},
		{v: cmBlindRepair, periods: 3, breaks: []string{"stale"}},
	}
	if os.Getenv("ENVOY_MENTION_ORDER_MODEL") == "full" {
		// A claim lapses only after a lease with no committed refresh, and the sweep then takes
		// its attempt: four periods reach that, the control's refresh writing the lapsed claim
		// again, and a hand-off landing after a send's retry check.
		runs = append(runs,
			modelRun{v: cmOutage, periods: 4},
			modelRun{v: cmLapseBlindStatements, periods: 4, breaks: []string{"stale", "dropped", "order"}},
		)
	}
	t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
	t.Logf("| sender | periods | states | longest run | stale claims | dropped sends | A-2 before A-1 | sent twice | held behind event 0 | A-2 first after the sweep took a lapsed claim | time |")
	t.Logf("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	for _, run := range runs {
		result := cmExplore(t, run.v, run.periods)
		t.Logf("%s", result)
		if run.breaks == nil {
			for _, kind := range cmChecks {
				if trace := result.first[kind]; trace != nil {
					t.Errorf("%s: the %s check broke in %d states; first:\n\t%s", run.v.name, kind, result.found[kind],
						strings.Join(trace, "\n\t"))
				}
			}
			continue
		}
		for _, kind := range run.breaks {
			if result.found[kind] == 0 {
				t.Errorf("the model is blind: %s broke no %s check", run.v.name, kind)
			} else {
				t.Logf("%s, first %s:\n\t%s", run.v.name, kind, strings.Join(result.first[kind], "\n\t"))
			}
		}
	}
	t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
}
