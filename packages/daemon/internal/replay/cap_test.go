package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A run's own cap (RunConfig.Cap) replaces the cap the record shows for the whole window: the
// question what the same window would have done with more slots. firstStartsWindow ran under a
// cap of 1, where ACME-2, handed to Legion at 09:01, waits for ACME-1's slot until ACME-1's
// accepting at 10:15. Replayed under main at the record's cap ACME-2's first slot is 10:15; at a
// cap of 2 the pass that records ACME-2 at 09:01 has a free slot and gives it, and run.json names
// the cap the run served under.
func TestARunsOwnCapReplacesTheRecordsCap(t *testing.T) {
	t.Parallel()
	window := firstStartsWindow(t)
	for _, c := range []struct {
		cap   int
		first time.Time
	}{{0, clock(10, 15)}, {2, clock(9, 1)}} {
		out := t.TempDir()
		result, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
			PGDSN: replayDSN(t), Out: out, Cap: c.cap})
		if err != nil {
			t.Fatalf("replay the window at the cap %d: %v", c.cap, err)
		}
		if first, ok := result.Scorecard.FirstSlot["ACME-2"]; !ok || !first.Truncate(time.Minute).Equal(c.first) {
			t.Errorf("at the cap %d ACME-2's first slot is %s, want %s", c.cap, stamp(first), stamp(c.first))
		}
		var ran runRecord
		if err := readJSON(filepath.Join(out, runFile), &ran); err != nil {
			t.Fatal(err)
		}
		if ran.Cap != c.cap {
			t.Errorf("run.json names the cap %d, want %d", ran.Cap, c.cap)
		}
	}
}

// capSpansWindow is a window from 09:00 to 14:00 under main whose daemon serves three caps, one
// span of each: 1 from its boot at 09:00, 73 from its restart at 11:00 and 2 from its restart at
// 12:30, each restart's first slot line naming its cap. ACME-1, filed and promoted at 09:00, works
// 15 minutes in each phase to its accepting at 10:15, where it gives its slot to ACME-2, filed at
// 09:01, which holds it to the window's end. ACME-3, ACME-4 and ACME-5, filed at 09:02, 09:03 and
// 09:04, wait for the restart at 11:00, take a slot each at 11:03 and work to their accepting at
// 12:18. ACME-6, ACME-7 and ACME-8, filed at 12:40, find one slot free under the cap of 2, which
// ACME-6 takes. The record gives out 2 slots under the cap of 1, 3 under 73 and 1 under 2.
func capSpansWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(14, 0))
	b.capped(1)
	b.boot(clock(9, 0))
	filed := func(at time.Time, key string, rank int) {
		b.file(at, key, filing{Title: "Ship the " + key + " change", Status: "todo", Rank: fmt.Sprintf("a%d", rank), Labels: handedToLegion})
	}
	filed(clock(9, 0), "ACME-1", 1)
	filed(clock(9, 1), "ACME-2", 2)
	trio := []string{"ACME-3", "ACME-4", "ACME-5"}
	for i, key := range trio {
		filed(clock(9, 2+i), key, 3+i)
	}
	b.rootThroughReview(clock(9, 0), "ACME-1")
	b.segmentEnds(clock(10, 15), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.rootPromoted(clock(10, 15), "ACME-2")
	b.turnEnds(clock(10, 30), "ACME-1", claim.RoleArchitect)
	b.turnEnds(clock(10, 45), "ACME-2", claim.RoleArchitect)
	b.stopped(clock(10, 58))
	b.boot(clock(11, 0))
	b.window.Caps = append(b.window.Caps, CapAt{At: clock(11, 3), Cap: 73})
	each := func(step func(key string)) {
		for _, key := range trio {
			step(key)
		}
	}
	each(func(key string) { b.rootPromoted(clock(11, 3), key) })
	each(func(key string) { b.rootDesigned(clock(11, 18), key) })
	each(func(key string) {
		b.segmentEnds(clock(11, 33), key, phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	})
	each(func(key string) {
		b.segmentEnds(clock(11, 48), key, phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	})
	each(func(key string) {
		b.segmentEnds(clock(12, 3), key, phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	})
	each(func(key string) {
		b.segmentEnds(clock(12, 18), key, phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	})
	each(func(key string) { b.turnEnds(clock(12, 25), key, claim.RoleArchitect) })
	b.stopped(clock(12, 28))
	b.boot(clock(12, 30))
	b.window.Caps = append(b.window.Caps, CapAt{At: clock(12, 40), Cap: 2})
	for i, key := range []string{"ACME-6", "ACME-7", "ACME-8"} {
		filed(clock(12, 40), key, 6+i)
	}
	b.rootPromoted(clock(12, 40), "ACME-6")
	return b.build()
}

// unboundCapWindow is a window from 09:00 to 12:00 under main whose daemon serves three caps, one
// span of each: 5 from its boot at 09:00, 73 from its restart at 10:00 and 6 from its restart at
// 11:00. ACME-1, ACME-2 and ACME-3, one filed in each span, at 09:10, 10:10 and 11:10, each take a
// slot at their filing: no cap ever holds an issue back, so the record gives out one slot in each
// span, and so does a replay at any cap.
func unboundCapWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.capped(5)
	b.boot(clock(9, 0))
	start := func(at time.Time, key, rank string) {
		b.file(at, key, filing{Title: "Ship the " + key + " change", Status: "todo", Rank: rank, Labels: handedToLegion})
		b.rootPromoted(at, key)
		b.turnEnds(at.Add(20*time.Minute), key, claim.RoleArchitect)
	}
	start(clock(9, 10), "ACME-1", "a1")
	b.stopped(clock(9, 58))
	b.boot(clock(10, 0))
	b.window.Caps = append(b.window.Caps, CapAt{At: clock(10, 10), Cap: 73})
	start(clock(10, 10), "ACME-2", "a2")
	b.stopped(clock(10, 58))
	b.boot(clock(11, 0))
	b.window.Caps = append(b.window.Caps, CapAt{At: clock(11, 10), Cap: 6})
	start(clock(11, 10), "ACME-3", "a3")
	return b.build()
}

// emptySpansWindow is a window from 09:00 to 13:00 under main whose daemon serves four caps, one
// span of each: 3 from its boot at 09:00, 2 from its restart at 10:00, 5 from its restart at 11:00
// and 6 from its restart at 12:00. ACME-1 and ACME-2, filed at 09:10 and 09:20, each take a slot at
// their filing and hold it to the window's end. ACME-3, filed at 10:10, finds both slots of the cap
// of 2 taken, waits for the restart at 11:00 and takes a slot at 11:03. Nothing is filed after it.
// The record gives out 2 slots under the cap of 3, none under 2, 1 under 5 and none under 6.
func emptySpansWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.capped(3)
	b.boot(clock(9, 0))
	filed := func(at time.Time, key, rank string) {
		b.file(at, key, filing{Title: "Ship the " + key + " change", Status: "todo", Rank: rank, Labels: handedToLegion})
	}
	takes := func(at time.Time, key string) {
		b.rootPromoted(at, key)
		b.turnEnds(at.Add(20*time.Minute), key, claim.RoleArchitect)
	}
	filed(clock(9, 10), "ACME-1", "a1")
	takes(clock(9, 10), "ACME-1")
	filed(clock(9, 20), "ACME-2", "a2")
	takes(clock(9, 20), "ACME-2")
	b.stopped(clock(9, 58))
	b.boot(clock(10, 0))
	b.window.Caps = append(b.window.Caps, CapAt{At: clock(10, 0), Cap: 2})
	filed(clock(10, 10), "ACME-3", "a3")
	b.stopped(clock(10, 58))
	b.boot(clock(11, 0))
	b.window.Caps = append(b.window.Caps, CapAt{At: clock(11, 3), Cap: 5})
	takes(clock(11, 3), "ACME-3")
	b.stopped(clock(11, 58))
	b.boot(clock(12, 0))
	b.window.Caps = append(b.window.Caps, CapAt{At: clock(12, 0), Cap: 6})
	return b.build()
}

// capBacktestJSON is a validation's cap backtest as its JSON gives it, and capSpanJSON one span of
// one cap of it.
type capBacktestJSON struct {
	CapBacktest *struct {
		Cap   int           `json:"cap"`
		Spans []capSpanJSON `json:"spans"`
		Pass  bool          `json:"pass"`
	} `json:"capBacktest"`
}

type capSpanJSON struct {
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
	Cap            int       `json:"cap"`
	Recorded       int       `json:"recorded"`
	AtRecordedCaps float64   `json:"atRecordedCaps"`
	AtFlatCap      float64   `json:"atFlatCap"`
	RecordedPass   bool      `json:"recordedPass"`
	FlatRead       bool      `json:"flatRead"`
	FlatPass       bool      `json:"flatPass"`
}

// The cap backtest (spec R5): validate --cap-backtest replays a window under the rule its daemon ran
// on the main branch at f 1 once at the recorded caps and once at one flat cap, --cap (RunConfig.Cap,
// which replaces the window's caps from its start to its end and which run.json names), and reads
// the slots given out in each span of one cap the daemon served against the record. Under the
// recorded caps each span stands within 10% of the record; under the flat cap each span whose cap is
// not the flat one misses it by more than 10%, and a span at the flat cap is not read. A span's count
// can be so small that one slot is more than 10% of it, and the result says so. capSpansWindow's
// three spans, of the caps 1, 73 and 2, give out 2, 3 and 1 slots in the record and the same at the
// recorded caps; at a flat cap of 73 every issue waiting under the cap of 1 starts at its filing, so
// its span gives out 5 and the span of 73 none, and ACME-6, ACME-7 and ACME-8 all start at 12:40, 3
// in the span of 2: the backtest passes. unboundCapWindow's caps never hold an issue back, so the
// flat cap gives out each span's one slot as the record did: the backtest fails, and the result says
// that the tool cannot tell a change of the cap. A span the flat cap reads in which the record gave
// out no slot fails where the flat cap gives out none there either, and the result names it as such
// a span. emptySpansWindow's four spans, of the caps 3, 2, 5 and 6, give out 2, 0, 1 and 0 slots in
// the record and the same at the recorded caps; at the flat cap ACME-3 starts at its filing, so the
// span of 3 gives out 2, the span of 2 one and the spans of 5 and 6 none. The spans of 2 and 5 pass,
// the span of 2 though the record gave out no slot in it; the spans of 3 and 6 fail the backtest,
// and the result names the span of 6 alone as one in which the record gave out no slot and the flat
// cap none. Neither other window's result names such a span.
func TestCapBacktestReadsEachSpanOfOneCap(t *testing.T) {
	t.Parallel()
	const flat = 73
	backtest := func(t *testing.T, name string, window Window) (capBacktestJSON, string) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), name)
		if err := window.Write(dir); err != nil {
			t.Fatal(err)
		}
		out := t.TempDir()
		cfg := ValidateConfig{Window: dir, Rule: mainRule(t), PGDSN: replayDSN(t), Parallel: 2, Out: out,
			plan: &runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 1}}
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"capBacktest": true, "cap": %d}`, flat)), &cfg); err != nil {
			t.Fatal(err)
		}
		v, err := RunValidate(context.Background(), cfg)
		if err != nil {
			t.Fatalf("the cap backtest of %s: %v", name, err)
		}
		var read capBacktestJSON
		asJSON(t, v, &read)
		var printedOut bytes.Buffer
		if err := v.Write(&printedOut, 1, nil); err != nil {
			t.Fatal(err)
		}
		var ran struct {
			Cap int `json:"cap"`
		}
		path := filepath.Join(runDir(filepath.Join(out, fmt.Sprintf("cap-%d", flat)), name, mainRule(t).Name, MainBranchAt, 1), runFile)
		if err := readJSON(path, &ran); err != nil {
			t.Errorf("the run at the flat cap: %v", err)
		} else if ran.Cap != flat {
			t.Errorf("the run at the flat cap names the cap %d in its run.json, want %d", ran.Cap, flat)
		}
		return read, printedOut.String()
	}
	spansRead := func(t *testing.T, got, want []capSpanJSON) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("the backtest reads %d spans of one cap, want %d: %+v", len(got), len(want), got)
		}
		for i := range want {
			g, w := got[i], want[i]
			if !g.From.Equal(w.From) || !g.To.Equal(w.To) || g.Cap != w.Cap || g.Recorded != w.Recorded || g.AtRecordedCaps != w.AtRecordedCaps ||
				g.AtFlatCap != w.AtFlatCap || g.RecordedPass != w.RecordedPass || g.FlatRead != w.FlatRead || g.FlatPass != w.FlatPass {
				t.Errorf("span %d reads %+v, want %+v", i+1, g, w)
			}
		}
	}
	// lineOf is the first line of printed that holds part, without its line end, or "" for none.
	lineOf := func(printed, part string) string {
		for line := range strings.Lines(printed) {
			if strings.Contains(line, part) {
				return strings.TrimSuffix(line, "\n")
			}
		}
		return ""
	}
	const noSlot = "the record gave out no slot"

	t.Run("spans the flat cap tells apart", func(t *testing.T) {
		t.Parallel()
		read, printed := backtest(t, "cap-spans", capSpansWindow(t))
		if read.CapBacktest == nil {
			t.Fatalf("the validation gives no cap backtest:\n%s", printed)
		}
		spansRead(t, read.CapBacktest.Spans, []capSpanJSON{
			{From: clock(9, 0), To: clock(11, 0), Cap: 1, Recorded: 2, AtRecordedCaps: 2, AtFlatCap: 5, RecordedPass: true, FlatRead: true, FlatPass: true},
			{From: clock(11, 0), To: clock(12, 30), Cap: 73, Recorded: 3, AtRecordedCaps: 3, AtFlatCap: 0, RecordedPass: true},
			{From: clock(12, 30), To: clock(14, 0), Cap: 2, Recorded: 1, AtRecordedCaps: 1, AtFlatCap: 3, RecordedPass: true, FlatRead: true, FlatPass: true},
		})
		if read.CapBacktest.Cap != flat || !read.CapBacktest.Pass {
			t.Errorf("the backtest at the flat cap %d passes %t, want a pass at %d", read.CapBacktest.Cap, read.CapBacktest.Pass, flat)
		}
		if !strings.Contains(printed, "a miss of one slot fails it") {
			t.Errorf("the backtest does not say that one slot is more than 10%% of a span's count:\n%s", printed)
		}
		if line := lineOf(printed, noSlot); line != "" {
			t.Errorf("a backtest whose record gives out a slot in each span names a span with none: %s", line)
		}
		if strings.Contains(printed, "the tool cannot tell a change of the cap") {
			t.Errorf("a backtest that passes says that the tool cannot tell a change of the cap:\n%s", printed)
		}
	})

	t.Run("caps that hold nothing back", func(t *testing.T) {
		t.Parallel()
		read, printed := backtest(t, "unbound-caps", unboundCapWindow(t))
		if read.CapBacktest == nil {
			t.Fatalf("the validation gives no cap backtest:\n%s", printed)
		}
		if spans := read.CapBacktest.Spans; len(spans) != 3 || spans[0].FlatPass || spans[2].FlatPass || spans[1].FlatRead {
			t.Errorf("the spans of caps that hold nothing back read %+v, want three, the flat cap's missing neither span it reads", spans)
		}
		if read.CapBacktest.Pass {
			t.Errorf("a backtest whose flat cap gives out every span's slots as the record did passes")
		}
		if !strings.Contains(printed, "the tool cannot tell a change of the cap") {
			t.Errorf("a backtest that fails does not say that the tool cannot tell a change of the cap:\n%s", printed)
		}
		if line := lineOf(printed, noSlot); line != "" {
			t.Errorf("a backtest whose record gives out a slot in each span names a span with none: %s", line)
		}
	})

	t.Run("spans in which the record gave out no slot", func(t *testing.T) {
		t.Parallel()
		read, printed := backtest(t, "empty-spans", emptySpansWindow(t))
		if read.CapBacktest == nil {
			t.Fatalf("the validation gives no cap backtest:\n%s", printed)
		}
		spansRead(t, read.CapBacktest.Spans, []capSpanJSON{
			{From: clock(9, 0), To: clock(10, 0), Cap: 3, Recorded: 2, AtRecordedCaps: 2, AtFlatCap: 2, RecordedPass: true, FlatRead: true},
			{From: clock(10, 0), To: clock(11, 0), Cap: 2, Recorded: 0, AtRecordedCaps: 0, AtFlatCap: 1, RecordedPass: true, FlatRead: true, FlatPass: true},
			{From: clock(11, 0), To: clock(12, 0), Cap: 5, Recorded: 1, AtRecordedCaps: 1, AtFlatCap: 0, RecordedPass: true, FlatRead: true, FlatPass: true},
			{From: clock(12, 0), To: clock(13, 0), Cap: 6, Recorded: 0, AtRecordedCaps: 0, AtFlatCap: 0, RecordedPass: true, FlatRead: true},
		})
		if read.CapBacktest.Pass {
			t.Errorf("a backtest whose flat cap fails the spans of 3 and 6 passes")
		}
		span := func(from, to time.Time) string { return spanStamp(from) + " to " + spanStamp(to) }
		ofThree, ofTwo, ofSix := span(clock(9, 0), clock(10, 0)), span(clock(10, 0), clock(11, 0)), span(clock(12, 0), clock(13, 0))
		if want := fmt.Sprintf("The backtest fails on the spans %s, %s: the tool cannot tell a change of the cap.", ofThree, ofSix); lineOf(printed, "The backtest fails") != want {
			t.Errorf("the backtest does not say %q:\n%s", want, printed)
		}
		if want := fmt.Sprintf("In 1 of the 4 spans the record gave out no slot, and the cap of %d gave out none there either, so it did not miss the record by more than 10%%: %s.", flat, ofSix); lineOf(printed, noSlot) != want {
			t.Errorf("the backtest does not say %q, naming the span of 6 and not the span of 2, %s, in which the flat cap gave out a slot:\n%s", want, ofTwo, printed)
		}
	})
}
