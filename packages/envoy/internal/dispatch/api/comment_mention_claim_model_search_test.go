//go:build !race

package api

import (
	"crypto/sha256"
	"fmt"
	"io"
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
// known by, the breadth-first walk of every state a variant reaches, each run replayed in a bubble
// of its own or visited one step on in its parent's, and the test that runs the sender as built and
// its control.

// key is the run's state as the search keys on it: every claim in play named by its order among
// them and its age up to a lease and a slot, then the model's rows, the sender's goroutines in the
// order they started - with what each holds in its own variables, the lane it serves, the send it
// carries and an answer it has not acted on yet - their tries, the refresh, the sweep's hand-offs
// and the retry timer, and the sender's maps.
func (r *cmRun) key() string {
	r.lock.Mutex.Lock()
	defer r.lock.Mutex.Unlock()
	var claims []time.Time
	add := func(claim time.Time) {
		if !claim.IsZero() {
			claims = append(claims, claim)
		}
	}
	addAll := func(named []mentionClaim) {
		for _, claim := range named {
			add(claim.claimedAt)
		}
	}
	for _, row := range r.rows {
		add(row.created)
		add(row.claim)
	}
	for _, state := range r.sender.held {
		add(state.claimed)
	}
	for _, sends := range r.sender.lanes {
		for _, send := range sends {
			add(send.claimed)
		}
	}
	for _, try := range r.tries {
		add(try.item.claimed)
		add(try.answer.item.claimed)
		if try.read != nil {
			addAll(try.read.named)
			addAll(try.read.found)
		}
	}
	for _, g := range r.gs {
		if g.ended != nil {
			add(g.ended.answer.item.claimed)
		}
		add(g.carried.claimed)
		if g.answered != nil {
			addAll(g.answered.named)
			addAll(g.answered.found)
		}
	}
	for _, sweep := range r.sweeps {
		add(sweep.item.claimed)
	}
	if f := r.refresh; f != nil {
		if f.read != nil {
			addAll(f.read.named)
			addAll(f.read.found)
		}
		if f.stmt != nil {
			addAll(f.stmt.queued)
			addAll(f.stmt.written)
		}
	}
	slices.SortFunc(claims, time.Time.Compare)
	claims = slices.CompactFunc(claims, time.Time.Equal)
	canon := func(claim time.Time) string {
		if claim.IsZero() {
			return "-"
		}
		i, _ := slices.BinarySearchFunc(claims, claim, time.Time.Compare)
		return fmt.Sprintf("%d/%d", i, min(r.age(claim), cmLapseSlots+1))
	}
	canonAll := func(named []mentionClaim) []string {
		var out []string
		for _, claim := range named {
			out = append(out, claim.key.comment+":"+canon(claim.claimedAt))
		}
		slices.Sort(out)
		return out
	}
	read := func(read *cmRead) string {
		if read == nil {
			return "-"
		}
		return fmt.Sprintf("%v %t %v", canonAll(read.named), read.ran, canonAll(read.found))
	}
	// A send as the sender reads it: its claim, what it last waited behind, and whether it was
	// stamped as waiting and warned of, which a late WARN reads (reportLate).
	send := func(s mentionSend) string {
		return fmt.Sprintf("%s:%s:%s:%d:%t:%t", s.comment.ID, canon(s.claimed), s.heldFor, s.heldBy.EventID,
			!s.heldSince.IsZero(), s.warned)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "s%d pub%t/%t took%v swept%t broke%v failed%d out%d|", r.slot, r.published, r.publishedBetween, r.took,
		r.sweptFirst, r.broke, r.readFailures, r.outages)
	for _, row := range r.rows {
		fmt.Fprintf(&b, "row %t %t %s %s|", row.committed, row.pending, canon(row.created), canon(row.claim))
	}
	// A goroutine as it can still act: the refresh by the slot it started in, which holds time
	// back; a lane goroutine by its lane and its retry's pending timer, and at next by the send it
	// last finished, if any (claimedDuring released it), and elsewhere by the send it carries,
	// which a lane goroutine at next no longer reads.
	for i, g := range r.gs {
		at := "statement"
		if g.lock != nil {
			at = g.site
		}
		if g.refresh {
			fmt.Fprintf(&b, "g%d refresh %t %s", i, g.born < r.slot, at)
		} else if strings.HasPrefix(at, "next:") {
			finished := -1
			if g.done {
				finished = g.last
			}
			fmt.Fprintf(&b, "g%d %s %v %t finished %d", i, at, g.lane, g.givenUp, finished)
		} else {
			fmt.Fprintf(&b, "g%d %s %v %t %t %s", i, at, g.lane, g.givenUp, g.tried, send(g.carried))
		}
		if g.ended != nil {
			fmt.Fprintf(&b, " ended %s", send(g.ended.answer.item))
		}
		if g.answered != nil {
			fmt.Fprintf(&b, " answered %s %t", read(g.answered), g.readFailed)
		}
		b.WriteString("|")
	}
	if f := r.refresh; f != nil {
		fmt.Fprintf(&b, "refresh %t %s", f.named, read(f.read))
		if f.stmt != nil {
			fmt.Fprintf(&b, " %v %t %v %t %t", canonAll(f.stmt.queued), f.stmt.wrote, canonAll(f.stmt.written),
				f.stmt.answered, f.stmt.lost)
		}
		b.WriteString("|")
	}
	for c := range 2 {
		if try := r.tries[c]; try != nil {
			fmt.Fprintf(&b, "try %d g%d %t %s %s %t %t %s %t|", c, slices.Index(r.gs, try.g), try.began < r.slot,
				read(try.read), send(try.item), try.ran, try.took, send(try.answer.item), try.answer.waits)
		}
		if sweep := r.sweeps[c]; sweep != nil {
			fmt.Fprintf(&b, "sweep %s|", send(sweep.item))
		}
	}
	if r.timer != nil {
		fmt.Fprintf(&b, "timer %t|", r.timer.began < r.slot)
	}
	// Each attempt's lost count is keyed as whether it moved since a re-read in flight named it,
	// which is all claimKnown and rereadUnsure compare.
	var held []string
	for key, state := range r.sender.held {
		entry := key.comment + ":" + canon(state.claimed)
		if state.unsure {
			entry += ":unsure"
		}
		if state.trying {
			entry += ":trying"
		}
		for _, read := range r.readsInFlight() {
			if began, named := read.lost[key.comment]; named && began != state.lost {
				entry += ":lostSinceRead"
			}
		}
		held = append(held, entry)
	}
	slices.Sort(held)
	fmt.Fprintf(&b, "held %v|", held)
	var lanes []string
	for lane, sends := range r.sender.lanes {
		var names []string
		for _, s := range sends {
			names = append(names, send(s))
		}
		lanes = append(lanes, fmt.Sprintf("%v=%v", lane, names))
	}
	slices.Sort(lanes)
	var waits []string
	for lane, wait := range r.sender.waits {
		waits = append(waits, fmt.Sprintf("%v:%d", lane, wait.by.EventID))
	}
	slices.Sort(waits)
	fmt.Fprintf(&b, "lanes %v waits %v ready %v serving %d", lanes, waits, r.sender.ready, r.sender.serving)
	return b.String()
}

// cmResult is what one search found: the states it reached, the longest run, by check how many
// states broke it first and the trace of the first of them, a shortest one, and how many visits it
// carried from a parent's replay and how many of those it checked against a replay from the start.
type cmResult struct {
	variant          cmVariant
	periods          int
	states           int
	depth            int
	found            map[string]int
	first            map[string][]string
	carried, checked int
	duration         time.Duration
}

func (r cmResult) String() string {
	return fmt.Sprintf("| %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %s |", r.variant.name, r.periods, r.states,
		r.depth, r.found["stale"], r.found["dropped"], r.found["order"], r.found["twice"], r.found["unnamed"],
		r.found["lapsed"], r.duration.Round(time.Millisecond))
}

// cmVisit is what replaying one run's steps came to: the SHA-256 of its state's key, the checks
// its last step broke, the steps it may take next, the model's own fault, if any, and, from a
// replay with its trace on, the run's trace.
type cmVisit struct {
	key          [sha256.Size]byte
	fault        string
	fresh, trace []string
	enabled      []cmStep
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

// visit is what the run has come to.
func (r *cmRun) visit() cmVisit {
	return cmVisit{key: sha256.Sum256([]byte(r.key())), fault: r.fault, fresh: r.fresh, enabled: r.enabled(),
		trace: r.trace}
}

// same is whether two visits of one run came to the same thing: state, fault, checks broken and
// next steps.
func (a cmVisit) same(b cmVisit) bool {
	return a.key == b.key && a.fault == b.fault && slices.Equal(a.fresh, b.fresh) && slices.Equal(a.enabled, b.enabled)
}

// cmVisitWithFirstChildOf replays steps as cmVisitOf does with its trace off, then takes, in the
// same run, the first step the run may take next - the step of the first child the search makes
// of this run's state - and visits that run too. Reading a run's key and its next steps changes
// nothing in it, so the child's visit is the one a replay of its steps from the start would reach;
// cmExplore replays every cmCarriedCheckEvery-th such visit from the start to hold it to that. A
// run that fails the model or may take no step has no first child.
func cmVisitWithFirstChildOf(t *testing.T, v cmVariant, periods int, steps []cmStep) (visit cmVisit, child *cmVisit) {
	synctest.Test(t, func(*testing.T) {
		r := newCmRun(v, periods, false)
		defer r.shutdown()
		for _, s := range steps {
			r.apply(s)
		}
		visit = r.visit()
		if visit.fault == "" && len(visit.enabled) > 0 {
			r.apply(visit.enabled[0])
			first := r.visit()
			child = &first
		}
	})
	return visit, child
}

// cmEntry is a run the search has still to visit: its steps from the start and, when it is the
// first child of a state whose replay went on to it (cmVisitWithFirstChildOf), its visit.
type cmEntry struct {
	steps   []cmStep
	visited *cmVisit
}

// cmCarriedCheckEvery is how often, among the visits a search carries from a parent's replay, one
// is replayed from the start as well and must come to the same thing: every 64th, in the search's
// order, about 1% more replays. Without it a carried visit given to the wrong run would go unseen:
// the run it should have been is never replayed, so its state's subtree drops out, and no check
// fails. With each carried visit given to the state's last child instead of its first, the sender
// as built reached 1,837,683 states instead of 4,231,211, and the test passed.
const cmCarriedCheckEvery = 64

// cmExplore searches every state of v within periods breadth first, the runs of one depth spread
// over at most 16 workers, then taken in order, so the first trace of each check is a shortest one
// and the same on every run. A run's state lives in its goroutines, so a run cannot be copied: the
// search replays a run from the start in a bubble of its own, and that replay goes on one step to
// the run's first child, whose visit then needs no replay of its own (for the sender as built at
// three periods, 4,231,211 states over 11,913,487 runs: 8.2 million replays instead of 11.9, 387
// million replayed steps instead of 567, before the check below adds 58,512 replays). Every
// cmCarriedCheckEvery-th carried visit is replayed too and must match. The search keeps a state's
// SHA-256 rather than its key, and replays with its trace on only the run that first breaks a
// check, that fails the model, or whose carried visit does not match; the first two replays must
// reach the same state.
func cmExplore(t *testing.T, v cmVariant, periods int) cmResult {
	began := time.Now()
	result := cmResult{variant: v, periods: periods, found: map[string]int{}, first: map[string][]string{}}
	seen := map[[sha256.Size]byte]struct{}{}
	traced := func(steps []cmStep, key [sha256.Size]byte) cmVisit {
		visit := cmVisitOf(t, v, periods, steps, true)
		if visit.key != key {
			t.Fatalf("the model is not deterministic: a replay with its trace on reached another state, after:\n\t%s",
				strings.Join(visit.trace, "\n\t"))
		}
		return visit
	}
	frontier := []cmEntry{{}}
	for depth := 0; len(frontier) > 0; depth++ {
		visits := make([]cmVisit, len(frontier))
		children := make([]*cmVisit, len(frontier))
		// replayed holds, for each carried visit the search checks, the visit of its run replayed
		// from the start.
		replayed := make([]*cmVisit, len(frontier))
		work := make(chan int)
		var workers sync.WaitGroup
		for range min(runtime.GOMAXPROCS(0), 16) {
			workers.Go(func() {
				for i := range work {
					if frontier[i].visited != nil {
						visit := cmVisitOf(t, v, periods, frontier[i].steps, false)
						replayed[i] = &visit
						continue
					}
					visits[i], children[i] = cmVisitWithFirstChildOf(t, v, periods, frontier[i].steps)
				}
			})
		}
		for i, entry := range frontier {
			if entry.visited != nil {
				visits[i] = *entry.visited
				if result.carried++; result.carried%cmCarriedCheckEvery != 0 {
					continue
				}
				result.checked++
			}
			work <- i
		}
		close(work)
		workers.Wait()
		var next []cmEntry
		for i, visit := range visits {
			steps := frontier[i].steps
			if replayed[i] != nil && !replayed[i].same(visit) {
				t.Fatalf("the visit carried from its parent's replay is not what a replay of its run from the start comes to, after:\n\t%s",
					strings.Join(cmVisitOf(t, v, periods, steps, true).trace, "\n\t"))
			}
			if visit.fault != "" {
				visit = traced(steps, visit.key)
				t.Fatalf("%s, after:\n\t%s", visit.fault, strings.Join(visit.trace, "\n\t"))
			}
			if _, ok := seen[visit.key]; ok {
				continue
			}
			seen[visit.key] = struct{}{}
			result.states++
			result.depth = depth
			for _, kind := range visit.fresh {
				result.found[kind]++
				if result.first[kind] == nil {
					result.first[kind] = traced(steps, visit.key).trace
				}
			}
			for j, s := range visit.enabled {
				child := cmEntry{steps: append(slices.Clip(steps), s)}
				if j == 0 {
					child.visited = children[i]
				}
				next = append(next, child)
			}
		}
		frontier = next
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
// and write a row between a try's answer and its retry schedule and for a send to wait for a retry after a failed read;
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
	// thousand.
	logs := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	// The table's rows go together; what each search carried and checked follows it.
	var carried []string
	for _, run := range runs {
		result := cmExplore(t, run.v, run.periods)
		t.Logf("%s", result)
		carried = append(carried, fmt.Sprintf("%s: %d visits carried from a parent's replay, %d of them replayed from the start too, each the same",
			run.v.name, result.carried, result.checked))
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
	for _, line := range carried {
		t.Logf("%s", line)
	}
	t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
}
