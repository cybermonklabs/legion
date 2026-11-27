package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The daemon's live signals (admit.LiveSignals) and the replay's record of a window
// (SignalRecord) read one definition of each series (package signals): fed the same facts, the
// daemon as the facts it keeps and the replay as the lines of a window collected for them, they
// read one value of each series and one next crossing of its limit at every moment.

// liveReplaySeries is one series as the replay reads it: its value at a moment and its next
// crossing of a limit, none for a series that moves only at events.
type liveReplaySeries struct {
	name string
	at   func(r *SignalRecord, at time.Time) signals.Value
	next func(r *SignalRecord, at time.Time, limit float64) (time.Time, bool)
}

// liveNotComputed is each signal of the tool's definitions (signals.Names()) that the live daemon
// does not compute, with why. The replay's historical refusal measurements (SignalRecord.Refusals,
// SignalRecord.NextRefusals) stay in the tool, and the live daemon has no refusal series: its
// settings, its state and its record of changes carry none. A signal of the tool's that is in
// neither this table nor the daemon's series fails TestLiveAndReplaySignalInventory until someone
// classes it.
var liveNotComputed = map[string]string{
	signals.Refusals: "the provider's refusals are a historical measurement of the replay: the live daemon reads no model request's refusal",
}

// liveReplayRows is the replay's reader of each series the live daemon computes: each definition
// of SignalDefinitions, taken as data, that liveNotComputed does not class as one it does not.
func liveReplayRows() []liveReplaySeries {
	var rows []liveReplaySeries
	for _, def := range SignalDefinitions() {
		if _, notLive := liveNotComputed[def.Name]; !notLive {
			rows = append(rows, liveReplaySeries{name: def.Name, at: def.At, next: def.Next})
		}
	}
	return rows
}

// The replay's readers are matched to the five series the daemon reads (store.LiveSeries()), each
// once, and every signal of the tool's definitions (signals.Names() and SignalDefinitions) is
// either one of them or classed as one the live daemon does not compute (liveNotComputed). It
// fails where a series of the daemon's has no reader of the replay's, where a reader names no
// series of the daemon's, where a signal of the tool's is neither, and where a class names a
// signal the daemon does compute or the tool does not define.
func TestLiveAndReplaySignalInventory(t *testing.T) {
	t.Parallel()
	rows := map[string]int{}
	for _, row := range liveReplayRows() {
		rows[row.name]++
	}
	live := store.LiveSeries()
	for _, name := range live {
		if rows[name] != 1 {
			t.Errorf("the series %s has %d readers of the replay's, want 1", name, rows[name])
		}
		delete(rows, name)
	}
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		t.Errorf("the replay's reader %s names no series of the daemon's", name)
	}
	defined := signals.Names()
	for _, def := range SignalDefinitions() {
		if !slices.Contains(defined, def.Name) {
			t.Errorf("the definition %s of SignalDefinitions is no signal of the tool's (signals.Names())", def.Name)
		}
	}
	for _, name := range defined {
		_, notLive := liveNotComputed[name]
		switch {
		case notLive && slices.Contains(live, name):
			t.Errorf("the signal %s is classed as one the live daemon does not compute, and it computes it", name)
		case !notLive && !slices.Contains(live, name):
			t.Errorf("the signal %s is neither a series of the live daemon nor classed as one it does not compute (liveNotComputed)", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(liveNotComputed)) {
		if !slices.Contains(defined, name) {
			t.Errorf("liveNotComputed names %s, which the tool does not define", name)
		}
	}
}

// The limits each series is compared at.
var liveReplayLimits = map[string]float64{
	signals.Idle: 0.5, signals.Queue: 1, signals.Provider: 0.5, signals.Kills: 2, signals.Approvals: 1,
}

// liveReplayEnd is where the spans of the window's record end, past every moment compared: an edge
// the daemon's record, which goes on, has no counterpart of, so a crossing the replay finds there
// is none.
var liveReplayEnd = clock(23, 0)

// liveDaemon is the daemon's admission over a pool of its own, registered with the live signals
// as the daemon registers them, its clock at now.
type liveDaemon struct {
	t         *testing.T
	pool      *pgxpool.Pool
	engine    *workflow.Engine
	admission *admit.Admission
	live      *admit.LiveSignals
	claims    *store.Store
	now       time.Time
	applied   int
}

func newLiveDaemon(t *testing.T) *liveDaemon {
	t.Helper()
	d := &liveDaemon{t: t, pool: testpg.ClonePool(t, "legion_replay_test")}
	claims, err := store.Open(context.Background(), d.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(claims.Close)
	d.claims = claims
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	d.engine = workflow.New(record.NewStore(), workflow.Config{Project: builtProject, Clock: func() time.Time { return d.now }}, quiet)
	d.admission = admit.New(record.NewStore(), d.engine, 4, builtProject, quiet)
	d.admission.UseClock(func() time.Time { return d.now })
	d.admission.UseModelRequests(6000, nil)
	d.live = admit.NewLiveSignals(d.admission, liveReplaySettings{}, 0)
	d.admission.UseSignals(d.live.Read)
	d.admission.UseLiveSignals(d.live, func(record.SignalDeadline) {})
	return d
}

// liveReplaySettings is admission by signals on, at liveReplayLimits.
type liveReplaySettings struct{}

func (liveReplaySettings) SignalSwitch(context.Context, pgx.Tx, string) (admit.SignalSwitch, error) {
	return admit.SignalSwitch{On: true, Limits: liveReplayLimits}, nil
}

// apply applies fact at at through the daemon's chain of handlers.
func (d *liveDaemon) apply(at time.Time, fact intake.Fact) {
	d.t.Helper()
	d.now = at
	d.applied++
	if _, err := intake.ApplyFact(context.Background(), d.pool, "test", fmt.Sprintf("fact-%d", d.applied), fact,
		admit.Handlers(record.NewStore(), builtProject, d.engine, d.admission)...); err != nil {
		d.t.Fatalf("apply %T at %s: %v", fact, at.Format(time.RFC3339), err)
	}
}

// holder records key as a root holding a slot it took at admitted, its tree opened under the
// workflow and its architect's claim in a turn begun then: the root the replay's promotion of it
// makes (promotedRoot), which a pass keeps its slot, where a root of no open tree gives it back.
func (d *liveDaemon) holder(key string, admitted time.Time) {
	d.t.Helper()
	project, err := claim.ProjectToken(builtProject)
	if err != nil {
		d.t.Fatal(err)
	}
	if err := store.Transact(context.Background(), d.pool, func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		if err := records.PutIssue(ctx, tx, record.Issue{Key: key, Repo: builtRepository, DefaultBranch: "main", GitHubRepo: builtRepository,
			Project: builtProject, Title: key, Tree: key, Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "a1", HandedOver: true}); err != nil {
			return err
		}
		if _, err := records.OpenTreeLifecycle(ctx, tx, project, key, treelifecycle.AuthorityWorkflow); err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: 0, AdmittedAt: admitted})
	}); err != nil {
		d.t.Fatalf("record the root %s: %v", key, err)
	}
	d.claim(key, supervise.StateWorking, admitted, time.Time{})
}

// claim writes key's architect's claim in state, its last turn begun at start and ended at end.
func (d *liveDaemon) claim(key string, state supervise.ClaimState, start, end time.Time) {
	d.t.Helper()
	project, err := claim.ProjectToken(builtProject)
	if err != nil {
		d.t.Fatal(err)
	}
	if err := d.claims.PutClaim(context.Background(), supervise.Claim{Token: builtToken(d.t, key, claim.RoleArchitect), Project: project, Tree: key, Issue: key,
		Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: state, Session: "ses_architect_" + key, LastTurnStart: start,
		LastTurnEnd: end}); err != nil {
		d.t.Fatalf("record the architect's claim of %s: %v", key, err)
	}
}

// reading is the daemon's live reading at at.
func (d *liveDaemon) reading(at time.Time) admit.LiveReading {
	d.t.Helper()
	var reading admit.LiveReading
	d.now = at
	if err := store.Transact(context.Background(), d.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reading, err = d.live.Reading(ctx, tx, at)
		return err
	}); err != nil {
		d.t.Fatalf("read the live signals at %s: %v", at.Format(time.RFC3339), err)
	}
	return reading
}

// liveReplayFact is one fact of the underlying record at its moment: written into a window by
// window, and given to the daemon by live, the facts of Dispatch's events aside, which the daemon
// takes from the window's own events (liveReplayAt).
type liveReplayFact struct {
	at     time.Time
	what   string
	window func(b *windowBuilder)
	live   func(d *liveDaemon)
}

// requestAt is the provider's requests of the record: each one's moment and uncached input
// tokens.
type requestAt struct {
	at     time.Time
	tokens int64
}

// liveReplayRecord is the record both read, from the daemon's boot at 05:00: ACME-1 takes a slot
// at 07:00 and its architect's turn ends at 07:40; the memory guard ends two processes of its pod
// at 07:10 and 07:20; pull request 1 waits in the merge queue from 07:30 to its merge at 08:15, and
// pull request 2 from 09:00 on; a question on ACME-2 opens at 08:00 and waits on the default
// assignee, and one on ACME-3 opens at 08:30 and is asked of another person at 08:40; and the
// provider's minute at 08:10 spends 4,000 uncached input tokens of the project's 6,000, its minute
// at 08:20 3,000 and its minute at 08:25 1,500.
func liveReplayRecord(requests *[]requestAt) []liveReplayFact {
	kill := func(at time.Time, pid int, sequence uint64) liveReplayFact {
		return liveReplayFact{at: at, what: fmt.Sprintf("the kill of pid %d", pid),
			window: func(b *windowBuilder) {
				b.window.Launcher = append(b.window.Launcher, LauncherLine{Time: at, Pod: "legion-acme-acme-1", Text: guardKillText(pid, "architect")})
			},
			live: func(d *liveDaemon) {
				d.apply(at, intake.MemoryGuardKill{Issue: "ACME-1", Token: builtToken(d.t, "ACME-1", claim.RoleArchitect), PodUID: "pod-1",
					Report: shimwire.LauncherMemoryKill{Guard: "guard-1", Sequence: sequence, PID: pid, Executable: "python3", LimitBytes: 8 << 30, At: at}})
			}}
	}
	queue := func(at time.Time, id, n int, kind string) liveReplayFact {
		return liveReplayFact{at: at, what: fmt.Sprintf("%s of pull request %d", kind, n),
			window: func(b *windowBuilder) {
				if b.window.Queue == nil {
					b.window.Queue = map[string][]json.RawMessage{}
				}
				b.window.Queue[builtRepository] = append(b.window.Queue[builtRepository], queueLine(id, n, kind, at))
			},
			live: func(d *liveDaemon) {
				d.apply(at, intake.QueueTimeline{Repo: builtRepository, Number: n, Events: []signals.QueueEvent{
					{ID: int64(id), Repo: builtRepository, PullRequest: n, At: at, Enters: kind == intake.QueueAdded}}})
			}}
	}
	request := func(id string, r requestAt) liveReplayFact {
		return liveReplayFact{at: r.at, what: "the model request " + id,
			window: func(*windowBuilder) { *requests = append(*requests, r) },
			live: func(d *liveDaemon) {
				tokens := r.tokens
				d.apply(r.at, intake.ModelRequests{Claim: builtToken(d.t, "ACME-1", claim.RoleArchitect), Generation: 1, Requests: []claim.ModelRequest{
					{ID: id, Session: "ses_1", Model: "acme-model", EndedAt: r.at, UncachedInputTokens: &tokens}}})
			}}
	}
	ask := func(at time.Time, key, id, kind string, payload map[string]any) liveReplayFact {
		return liveReplayFact{at: at, what: kind + " of " + id, window: func(b *windowBuilder) {
			body := map[string]any{"id": id, "issue_key": key, "author": b.session(key, claim.RoleArchitect), "kind": record.AskQuestion, "state": "open", "asked_of": nil}
			maps.Copy(body, payload)
			actor := b.session(key, claim.RoleArchitect)
			if kind == "ask.asked_of" {
				actor = builtPerson
			}
			b.event(at, key, kind, actor, body)
		}}
	}
	return []liveReplayFact{
		{at: clock(7, 0), what: "ACME-1 takes a slot", window: func(b *windowBuilder) { promotedRoot(b, clock(7, 0), "ACME-1", "a1") },
			live: func(d *liveDaemon) { d.holder("ACME-1", clock(7, 0)) }},
		kill(clock(7, 10), 100, 1),
		kill(clock(7, 20), 101, 2),
		queue(clock(7, 30), 1, 1, intake.QueueAdded),
		{at: clock(7, 40), what: "ACME-1's architect ends its turn", window: func(b *windowBuilder) { b.turnEnds(clock(7, 40), "ACME-1", claim.RoleArchitect) },
			live: func(d *liveDaemon) { d.claim("ACME-1", supervise.StateIdle, clock(7, 0), clock(7, 40)) }},
		ask(clock(8, 0), "ACME-2", "A1", "ask.opened", nil),
		request("r1", requestAt{at: clock(8, 10).Add(30 * time.Second), tokens: 4000}),
		queue(clock(8, 15), 2, 1, intake.QueueMerged),
		request("r2", requestAt{at: clock(8, 20).Add(10 * time.Second), tokens: 3000}),
		request("r3", requestAt{at: clock(8, 25), tokens: 1500}),
		ask(clock(8, 30), "ACME-3", "A2", "ask.opened", nil),
		ask(clock(8, 40), "ACME-3", "A2", "ask.asked_of", map[string]any{"asked_of": acmeOther, "asked_of_by": acmeOwner}),
		queue(clock(9, 0), 3, 2, intake.QueueAdded),
	}
}

// liveReplayAt is the record up to at as each reads it: the window collected of the facts at or
// before at, every span of its record from the boot's reach to liveReplayEnd (the merge queue's
// from 6 hours after the boot, the kills' from an hour after, as the daemon's own record begins),
// and its provider's every minute from the boot on, each holding the requests that ended in it by
// at; and the daemon given the same facts, Dispatch's events among them read from the window's own
// lines through the daemon's decoder (intake.DecodeDispatchEvent).
func liveReplayAt(t *testing.T, at time.Time) (*SignalRecord, *liveDaemon) {
	t.Helper()
	boot := clock(5, 0)
	b := newWindowBuilder(t, clock(6, 0), clock(12, 0))
	b.reaches(time.Hour)
	b.boot(boot)
	b.window.DefaultAssignee = acmeOwner
	b.window.Launcher = []LauncherLine{{Time: boot, Pod: "legion-acme-acme-1",
		Text: "launcher: memory guard: started act_percent=85 floor_percent=10 wait=5s interval=100ms limit=8589934592 reading=23052288"}}
	for i, key := range []string{"ACME-2", "ACME-3"} {
		b.file(clock(6, 30), key, filing{Title: "Settle the question of " + key, Status: "in_progress", Rank: fmt.Sprintf("b%d", i+1)})
	}
	d := newLiveDaemon(t)
	d.apply(boot, intake.DaemonBooted{Boot: 1})
	d.apply(boot, intake.ApprovalsBootRead{Read: record.ApprovalsRead{DefaultAssignee: acmeOwner, DefaultAt: boot}})
	var requests []requestAt
	for _, fact := range liveReplayRecord(&requests) {
		if fact.at.After(at) {
			break
		}
		fact.window(b)
		if fact.live != nil {
			fact.live(d)
		}
	}
	limit, project, source := int64(6000), projectLimitScope, "dispatch"
	for minute := boot; minute.Before(liveReplayEnd); minute = minute.Add(time.Minute) {
		line := ProviderMinute{Minute: minute, Limit: &limit, LimitSource: &source, LimitScope: &project}
		for _, r := range requests {
			if r.at.Truncate(time.Minute).Equal(minute) {
				line.Uncached += r.tokens
			}
		}
		b.window.Provider = append(b.window.Provider, line)
	}
	b.window.Signals = map[string][]Span{
		SignalIdle:      {{From: boot, To: liveReplayEnd}},
		SignalProvider:  {{From: boot, To: liveReplayEnd}},
		SignalKills:     {{From: boot.Add(signals.KillSpan), To: liveReplayEnd}},
		SignalApprovals: {{From: boot, To: liveReplayEnd}},
	}
	// A window collected before the first merge-queue event holds no queue file, and so names no
	// record of the queue, which the daemon reads as not recorded until 6 hours after its boot too.
	if len(b.window.Queue) > 0 {
		b.window.Signals[SignalQueue] = []Span{{From: boot.Add(signals.QueueExitSpan), To: liveReplayEnd}}
	}
	w := b.build()
	type event struct {
		at   time.Time
		fact intake.Fact
	}
	var events []event
	for _, key := range slices.Sorted(maps.Keys(w.Events)) {
		for _, raw := range w.Events[key] {
			fact, _, err := intake.DecodeDispatchEvent(builtProject, key, raw)
			if err != nil {
				t.Fatalf("decode an event of %s: %v", key, err)
			}
			if _, _, ok := intake.AskTurnOf(fact); !ok {
				continue
			}
			var head struct {
				CreatedAt time.Time `json:"created_at"`
			}
			if err := json.Unmarshal(raw, &head); err != nil {
				t.Fatal(err)
			}
			events = append(events, event{at: head.CreatedAt, fact: fact})
		}
	}
	slices.SortStableFunc(events, func(x, y event) int { return x.at.Compare(y.at) })
	for _, e := range events {
		d.apply(e.at, e.fact)
	}
	return signalRecordOf(t, w, signals.DefaultIdleAfter), d
}

// The live signals and the replay give one value of each series and one next crossing of its
// limit, from the same facts: before the first fact, at each fact, after the last, and on both
// sides of each next crossing the daemon finds; the provider's headroom, the edges where the
// kills' hour and the queue's 6 hours become whole, and the approvals, which move only at events.
// The rule of admission by signals decides one start alike on either's snapshot, for a pass with
// admission by signals off, at an event, at the tick, at a deadline, under a full cap, and where a
// signal holds the start.
func TestLiveAndReplaySignals(t *testing.T) {
	moments := []time.Time{clock(6, 59)}
	var none []requestAt
	for _, fact := range liveReplayRecord(&none) {
		moments = append(moments, fact.at)
	}
	moments = append(moments, clock(11, 30))
	compared := map[time.Time]bool{}
	verdicts := map[signals.Verdict]bool{}
	var compare func(at time.Time, crossings bool)
	compare = func(at time.Time, crossings bool) {
		if compared[at] {
			return
		}
		compared[at] = true
		r, d := liveReplayAt(t, at)
		reading := d.reading(at)
		live := map[string]admit.SeriesReading{}
		for _, series := range reading.Series {
			live[series.Name] = series
		}
		if rows := liveReplayRows(); len(live) != len(rows) {
			t.Errorf("at %s the daemon reads %d series, want the %d the replay reads", at.Format(time.TimeOnly), len(live), len(rows))
		}
		replaySnapshot := signals.Snapshot{Mode: signals.Ceiling, At: at, Readings: map[string]signals.Reading{}}
		for _, row := range liveReplayRows() {
			got, want := live[row.name], row.at(r, at)
			limit := liveReplayLimits[row.name]
			replaySnapshot.Readings[row.name] = signals.Reading{Value: want, Limit: &limit}
			if got.Value != want {
				t.Errorf("%s at %s: the daemon reads %s, the replay %s", row.name, at.Format(time.TimeOnly), describeValue(got.Value), describeValue(want))
			}
			var next time.Time
			var due bool
			if row.next != nil {
				next, due = row.next(r, at, limit)
				if due && next.Equal(liveReplayEnd) {
					due = false
				}
			}
			if got.Due != due || due && !got.Next.Equal(next) {
				t.Errorf("%s at %s: the daemon's next crossing of %v is %s, the replay's %s", row.name, at.Format(time.TimeOnly), limit,
					describeCrossing(got.Next, got.Due), describeCrossing(next, due))
			}
		}
		liveSnapshot := reading.Snapshot()
		for _, decision := range []struct {
			name            string
			mode            signals.Mode
			kind            signals.PassKind
			capped, ceiling int
		}{
			{"off", signals.Off, signals.PassEvent, 1, 4},
			{"an event", signals.Ceiling, signals.PassEvent, 1, 4},
			{"the tick", signals.Ceiling, signals.PassTick, 1, 4},
			{"a deadline", signals.Ceiling, signals.PassDeadline, 1, 4},
			{"a full cap", signals.Ceiling, signals.PassEvent, 4, 4},
		} {
			ofLive, ofReplay := liveSnapshot, replaySnapshot
			ofLive.Mode, ofReplay.Mode = decision.mode, decision.mode
			got := record.MainRule.SignalsLet(decision.kind, ofLive, decision.capped, decision.ceiling)
			want := record.MainRule.SignalsLet(decision.kind, ofReplay, decision.capped, decision.ceiling)
			verdicts[got] = true
			if got != want {
				t.Errorf("at %s, for %s, the rule decides %s on the daemon's snapshot and %s on the replay's", at.Format(time.TimeOnly), decision.name, got, want)
			}
		}
		if !crossings {
			return
		}
		for _, series := range reading.Series {
			if series.Due {
				compare(series.Next.Add(-time.Second), false)
				compare(series.Next.Add(time.Second), false)
			}
		}
	}
	for _, at := range moments {
		compare(at, true)
	}
	for _, verdict := range []signals.Verdict{signals.Starts, signals.HeldByTick, signals.HeldByCap, signals.HeldBySignal} {
		if !verdicts[verdict] {
			t.Errorf("no moment compared gave the verdict %s: the record must exercise each decision", verdict)
		}
	}
}
