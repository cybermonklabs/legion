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

// The sender claim model's search (TestTheMentionSenderClaimModel,
// comment_mention_claim_model_test.go holds the model itself): the key a run's state is known by,
// the breadth-first walk of every state a variant reaches, each run replayed in a bubble of its
// own, and the test that runs the sender as built and its control.

// key is the run's state as the search keys on it: every claim in play named by its order among
// them and its age up to a lease and a slot, then the model's rows, tries, timers and refresh,
// and the sender's maps.
func (r *cmRun) key() string {
	r.sender.mu.Lock()
	defer r.sender.mu.Unlock()
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
	for _, parking := range r.parking {
		add(parking.try.item.claimed)
	}
	if r.refresh != nil {
		addAll(r.refresh.queued)
		addAll(r.refresh.written)
		if r.refresh.read != nil {
			addAll(r.refresh.read.named)
			addAll(r.refresh.read.found)
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
	send := func(s mentionSend) string {
		return fmt.Sprintf("%s:%s:%s", s.comment.ID, canon(s.claimed), s.heldFor)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "s%d pub%t/%t took%v broke%v failed%d out%d|", r.slot, r.published, r.publishedBetween, r.took,
		r.broke, r.readFailures, r.outages)
	for _, row := range r.rows {
		fmt.Fprintf(&b, "row %t %t %s %s|", row.committed, row.pending, canon(row.created), canon(row.claim))
	}
	if f := r.refresh; f != nil {
		fmt.Fprintf(&b, "refresh %t %s %v %t %v|", f.began < r.slot, read(f.read), canonAll(f.queued), f.wrote,
			canonAll(f.written))
	}
	for c := range 2 {
		if try := r.tries[c]; try != nil {
			fmt.Fprintf(&b, "try %d %v %t %s %t %s %t %t %s %t|", c, try.lane, try.began < r.slot, read(try.read),
				try.trying, send(try.item), try.ran, try.took, send(try.answer.item), try.answer.waits)
		}
	}
	var timers []string
	for lane, timer := range r.timers {
		timers = append(timers, fmt.Sprintf("%v:%t", lane, timer.began < r.slot))
	}
	slices.Sort(timers)
	fmt.Fprintf(&b, "timers %v|", timers)
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
	var parking []string
	for c, p := range r.parking {
		parking = append(parking, fmt.Sprintf("%d %v %t %s", c, p.try.lane, p.try.began < r.slot, send(p.try.item)))
	}
	slices.Sort(parking)
	fmt.Fprintf(&b, "lanes %v waits %v parking %v ready %v serving %d", lanes, waits, parking, r.sender.ready,
		r.sender.serving)
	return b.String()
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
		visit = cmVisit{key: sha256.Sum256([]byte(r.key())), fault: r.fault, fresh: r.fresh, enabled: r.enabled(),
			trace: r.trace}
	})
	return visit
}

// cmExplore searches every state of v within periods breadth first, each run replayed from the
// start in a bubble of its own and the runs of one depth spread over at most 16 workers, then
// taken in order, so the first trace of each check is a shortest one and the same on every run.
// The search keeps a state's SHA-256 rather than its key, and replays with its trace on only the
// run that first breaks a check, or that fails the model; the replay must reach the same state.
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
	frontier := [][]cmStep{nil}
	for depth := 0; len(frontier) > 0; depth++ {
		visits := make([]cmVisit, len(frontier))
		work := make(chan int)
		var workers sync.WaitGroup
		for range min(runtime.GOMAXPROCS(0), 16) {
			workers.Go(func() {
				for i := range work {
					visits[i] = cmVisitOf(t, v, periods, frontier[i], false)
				}
			})
		}
		for i := range frontier {
			work <- i
		}
		close(work)
		workers.Wait()
		var next [][]cmStep
		for i, visit := range visits {
			if visit.fault != "" {
				visit = traced(frontier[i], visit.key)
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
					result.first[kind] = traced(frontier[i], visit.key).trace
				}
			}
			for _, s := range visit.enabled {
				next = append(next, append(slices.Clip(frontier[i]), s))
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
// learn nothing of a lost refresh must trust a stale claim, and, in the full run, the refresh and
// take before this change, under the same failures, must trust a stale claim, drop a send and let
// A-2 pass A-1. After a claim lapses the delivery sweep keeps no order between two comments, which
// the spec puts outside this change: the table counts those runs in their own column, and they
// fail nothing.
//
// CI runs three periods, about two and a half minutes on two cores, which is enough for a refresh
// to land between a try and its park and for a send to park after a failed read; the full run
// (ENVOY_MENTION_ORDER_MODEL=full) runs five periods and the outage runs, and CI guards what only
// they reach with TestALostRefreshNeverRevivesALapsedClaim and
// TestAHandOffDuringASendsTryIsTriedAgainUnderItsClaim on the database.
func TestTheMentionSenderClaimModel(t *testing.T) {
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
		// A claim lapses only after a lease with no committed refresh, and the refresh before
		// this change then wrote it again: that takes a claim a lease and a slot old, so five
		// periods at least.
		runs = []modelRun{
			{v: cmAsBuilt, periods: 5},
			{v: cmBlindRepair, periods: 5, breaks: []string{"stale"}},
			{v: cmOutage, periods: 5},
			{v: cmOldStatements, periods: 5, breaks: []string{"stale", "dropped", "order"}},
		}
	}
	t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
	t.Logf("| sender | periods | states | longest run | stale claims | dropped sends | A-2 before A-1 | sent twice | held behind event 0 | A-2 first by the sweep after its claim lapsed | time |")
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
