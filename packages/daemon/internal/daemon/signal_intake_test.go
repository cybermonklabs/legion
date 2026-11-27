package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// signalRig is the daemon's workflow runtime over an isolated pool, its admission built and
// registered with the live signals and the signal deadline's timer as openWorkflow registers them
// (admit.LiveSignals over the store's settings, admit.UseLiveSignals, workflowRuntime.signalTimer),
// the project's settings admission by signals on at limits, written as a person's changes before
// the rig's first fact. Each committed pass's kind is in passes.
type signalRig struct {
	t      *testing.T
	pool   *store.Pool
	w      *workflowRuntime
	live   *admit.LiveSignals
	kills  *memoryGuardKills
	mu     sync.Mutex
	clock  func() time.Time
	passes []signals.PassKind
	n      int
}

// signalSettings is admission by signals on, at limits.
type signalSettings map[string]float64

func (s signalSettings) SignalSwitch(context.Context, pgx.Tx, string) (admit.SignalSwitch, error) {
	return admit.SignalSwitch{On: true, Limits: s}, nil
}

func newSignalRig(t *testing.T, limits map[string]float64) *signalRig {
	t.Helper()
	r := &signalRig{t: t, pool: isolatedOutboxPool(t), kills: &memoryGuardKills{}, clock: time.Now}
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: killsProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	admission := admit.New(records, engine, 2, killsProject, quietLogger())
	admission.UseClock(func() time.Time {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.clock()
	})
	admission.UseModelRequests(6000, nil)
	r.w = &workflowRuntime{pool: r.pool, records: records, admission: admission, dispatchProject: killsProject, log: quietLogger(),
		signalDue: make(chan record.SignalDeadline, 1)}
	r.live = admit.NewLiveSignals(admission, admit.StoreSignalSettings{}, api.ModelRequestsContract())
	admission.UseSignals(r.live.Read)
	r.w.signalTimer = admit.NewDeadlineTimer(r.w.signalDeadlineDue)
	admission.UseLiveSignals(r.live, r.w.signalTimer.Arm)
	admission.UsePassObserver(func(view admit.PassView) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.passes = append(r.passes, view.Kind)
	})
	r.w.handlers = admit.Handlers(records, killsProject, engine, admission)
	r.kills.bind(r.pool, r.w.handlers)
	t.Cleanup(r.w.signalTimer.Stop)
	r.seed(limits)
	return r
}

// seed writes admission by signals on at limits as the operator's changes, before the rig's first
// fact, so the boot's pass already admits by them.
func (r *signalRig) seed(limits map[string]float64) {
	r.t.Helper()
	set := store.SignalSet{By: store.PauseActor{Kind: store.PauseByOperator}, At: time.Now().UTC(), Reason: "the rig's settings"}
	changes := []store.SignalSettingChange{{Setting: store.SignalSwitchSetting, Value: new(1.0), Set: set}}
	for series, limit := range limits {
		changes = append(changes, store.SignalSettingChange{Setting: series, Value: &limit, Set: set})
	}
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, change := range changes {
			if _, err := store.ChangeSignalSettingTx(ctx, tx, killsProject, change); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		r.t.Fatalf("write the rig's settings: %v", err)
	}
}

// at sets admission's clock to now.
func (r *signalRig) at(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clock = func() time.Time { return now }
}

// passCount is how many passes committed.
func (r *signalRig) passCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.passes)
}

// apply applies fact under source and id through the runtime's handlers.
func (r *signalRig) apply(source, id string, fact intake.Fact) {
	r.t.Helper()
	if _, err := intake.ApplyFact(context.Background(), r.pool, source, id, fact, r.w.handlers...); err != nil {
		r.t.Fatalf("apply %s/%s: %v", source, id, err)
	}
}

// boot is the daemon's boot at since and the boot's read of Dispatch, which names the project's
// default assignee, each as the boot applies it; the clock is now again after it.
func (r *signalRig) boot(since, now time.Time) {
	r.t.Helper()
	r.at(since)
	r.apply("daemon", "boot", intake.DaemonBooted{Boot: 1})
	r.apply("daemon", "boot-approvals", intake.ApprovalsBootRead{Read: record.ApprovalsRead{DefaultAssignee: "owner@example.invalid", DefaultAt: since}})
	r.at(now)
}

// dispatch decodes event, a Dispatch event of issue at seq of kind with payload, as the Dispatch
// consumer decodes one (intake.DecodeDispatchEvent), and applies its fact under the event's id.
func (r *signalRig) dispatch(issue string, seq int64, kind string, at time.Time, payload map[string]any) {
	r.t.Helper()
	raw, err := json.Marshal(map[string]any{"id": 9000 + seq, "issue_key": issue, "artifact_id": nil, "project": killsProject, "seq": seq, "type": kind,
		"actor": map[string]any{"kind": "user", "id": "owner@example.invalid"}, "notify": false, "created_at": at.Format(time.RFC3339Nano), "payload": payload})
	if err != nil {
		r.t.Fatal(err)
	}
	fact, _, err := intake.DecodeDispatchEvent(killsProject, issue, raw)
	if err != nil || fact == nil {
		r.t.Fatalf("decode the %s event of %s: %v, %v", kind, issue, fact, err)
	}
	r.apply("dispatch", fmt.Sprintf("dispatch:%s:%d", issue, seq), fact)
}

// ask is the payload of an event of ask id on issue: open, of kind, asked of nobody where
// askedOf is nil, with more as the event's own members.
func ask(id, issue, kind string, askedOf any, more map[string]any) map[string]any {
	payload := map[string]any{"id": id, "issue_key": issue, "author": map[string]any{"id": "ses-architect", "kind": "session"}, "kind": kind,
		"state": "open", "asked_of": askedOf}
	for key, value := range more {
		payload[key] = value
	}
	return payload
}

// holder records issue as a root holding a slot since admitted, its tree open, with its
// implementer's claim in state, its last turn begun at start and ended at end.
func (r *signalRig) holder(issue string, admitted time.Time) {
	r.t.Helper()
	project, err := claim.ProjectToken(killsProject)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		if err := records.PutIssue(ctx, tx, record.Issue{Key: issue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: issue,
			Project: killsProject, Title: issue, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "a", HandedOver: true}); err != nil {
			return err
		}
		if _, err := records.OpenTreeLifecycle(ctx, tx, project, issue, treelifecycle.AuthorityWorkflow); err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: issue, Index: 0, AdmittedAt: admitted})
	}); err != nil {
		r.t.Fatalf("record the holder %s: %v", issue, err)
	}
}

// claimed writes issue's implementer's claim in state with its last turn begun at start and ended
// at end, of a host that reports its model requests (api.ModelRequestsContract), so its turns leave
// the provider's minutes whole.
func (r *signalRig) claimed(issue string, state supervise.ClaimState, start, end time.Time) supervise.Claim {
	r.t.Helper()
	project, err := claim.ProjectToken(killsProject)
	if err != nil {
		r.t.Fatal(err)
	}
	c := supervise.Claim{Token: hostToken(r.t, issue, claim.RoleImplementer), Project: project, Tree: issue, Issue: issue, Role: claim.RoleImplementer,
		TreeEpoch: 1, Generation: 1, State: state, Session: "ses_" + issue, LastTurnStart: start, LastTurnEnd: end,
		PluginContract: api.ModelRequestsContract()}
	st, err := store.Open(context.Background(), r.pool.Config().ConnString())
	if err != nil {
		r.t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if err := st.PutClaim(context.Background(), c); err != nil {
		r.t.Fatalf("write the claim of %s: %v", issue, err)
	}
	return c
}

// turn writes issue's implementer's claim (claimed) and tells the runtime the edge the write made,
// a start where edgeStart says so and an end otherwise, as the supervisor tells it once the
// claim's machine wrote it (supervisor.OnTurnEdge, workflowRuntime.turnEdge).
func (r *signalRig) turn(issue string, state supervise.ClaimState, start, end time.Time, edgeStart bool) {
	r.t.Helper()
	c := r.claimed(issue, state, start, end)
	edge := end
	if edgeStart {
		edge = start
	}
	r.w.turnEdge(c, edgeStart, edge)
}

// reading is the live reading of series at the clock's moment.
func (r *signalRig) reading(series string) admit.SeriesReading {
	r.t.Helper()
	var reading admit.LiveReading
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reading, err = r.live.Reading(ctx, tx, r.clock())
		return err
	}); err != nil {
		r.t.Fatalf("read the live signals: %v", err)
	}
	for _, s := range reading.Series {
		if s.Name == series {
			return s
		}
	}
	r.t.Fatalf("no series %s", series)
	return admit.SeriesReading{}
}

// committed is the signal deadline the store holds.
func (r *signalRig) committed() record.SignalDeadline {
	r.t.Helper()
	var deadline record.SignalDeadline
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		deadline, err = record.NewStore().SignalDeadline(ctx, tx, killsProject)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return deadline
}

// requests applies one report of the host of token's launch: the requests, each of tokens uncached
// input tokens, ended at the moments given, as the claim route applies a report (api's
// modelRequests).
func (r *signalRig) requests(token claim.Token, id string, tokens int64, ended ...time.Time) {
	r.t.Helper()
	var requests []claim.ModelRequest
	for i, at := range ended {
		requests = append(requests, claim.ModelRequest{ID: fmt.Sprintf("%s-%d", id, i), Session: "ses_" + id, Model: "acme-model", EndedAt: at,
			UncachedInputTokens: &tokens})
	}
	r.apply("api", "requests:"+id, intake.ModelRequests{Claim: token, Generation: 1, Requests: requests})
}

// guardKill hands the kill holder the memory guard's kill of a process of an issue pod's tester, a
// minute before now, as the launcher session hands it one.
func (r *signalRig) guardKill(now time.Time) {
	r.t.Helper()
	if err := r.kills.killed(context.Background(), memoryGuardIssue, hostToken(r.t, memoryGuardIssue, claim.RoleTester), "pod-1",
		shimwire.LauncherMemoryKill{Guard: "guard-1", Sequence: 1, PID: 4242, Executable: "python3", LimitBytes: 4 << 30, At: now.Add(-time.Minute)}); err != nil {
		r.t.Fatalf("apply the kill: %v", err)
	}
}

// setting applies the operator's change of setting to value at now, for reason, as the
// operator's route applies it (api's changeSignals).
func (r *signalRig) setting(now time.Time, setting string, value *float64, reason string) {
	r.t.Helper()
	r.n++
	r.apply("api", fmt.Sprintf("signals:%d", r.n), intake.SignalSettingChanged{Change: store.SignalSettingChange{Setting: setting, Value: value,
		Set: store.SignalSet{By: store.PauseActor{Kind: store.PauseByOperator}, At: now, Reason: reason}}})
}

// writtenHold is whether the record's newest holds entry of series says it holds admission: what
// the last committed pass wrote of it.
func (r *signalRig) writtenHold(series string) bool {
	r.t.Helper()
	var newest map[string]store.SeriesEntries
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		newest, err = store.NewestSeriesEntriesTx(ctx, tx, killsProject)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	holds := newest[series].Holds
	return holds != nil && *holds.Value == 1
}

// hostToken is the claim token of issue's role, whose host reports its model requests.
func hostToken(t *testing.T, issue string, role claim.Role) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(killsProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// signalEvent is one source event of plan section 4's list: what drives it through the daemon,
// the series whose live value its committed pass reads, and that value; passes is how many
// committed passes it runs, by the pass rule and the series-moved pass rule for a report and a turn edge.
// holds, where set, is whether the series holds admission once the event's last pass committed,
// as the live signals read it and as that pass wrote it in the record of changes.
type signalEvent struct {
	name   string
	limits map[string]float64
	drive  func(r *signalRig, now time.Time)
	series string
	value  float64
	passes int
	holds  *bool
}

// signalEventSources is every source event of plan section 4's list that this release takes: a
// slot given or taken back is the pass's own move, which the pass that made it reads.
var signalEventSources = []string{"queue entry", "queue removal", "queue merge", "ask opened", "ask restored", "ask asked of",
	"ask edited", "ask handed back", "ask comment", "ask answered", "ask resolved", "issue assignee", "default assignee",
	"issue pod guard kill", "Oh My Pi report", "Claude Code report", "holder turn start", "holder turn end",
	"switch", "limit set", "limit clear"}

func signalEvents() []signalEvent {
	const issue = "LEGION-301"
	queue := func(id int64, n int, at time.Time, enters bool) intake.QueueTimeline {
		return intake.QueueTimeline{Repo: "acme/widgets", Number: n, Events: []signals.QueueEvent{{ID: id, Repo: "acme/widgets", PullRequest: n, At: at, Enters: enters}}}
	}
	opened := func(r *signalRig, now time.Time) {
		r.dispatch(issue, 1, "ask.opened", now.Add(-time.Minute), ask("A1", issue, "question", nil, nil))
	}
	askEvent := func(seq int64, kind string, payload map[string]any, value float64, name string) signalEvent {
		return signalEvent{name: name, series: signals.Approvals, value: value, passes: 2, drive: func(r *signalRig, now time.Time) {
			opened(r, now)
			r.dispatch(issue, seq, kind, now, payload)
		}}
	}
	return []signalEvent{
		{name: "queue entry", series: signals.Queue, value: 0.5, passes: 1, drive: func(r *signalRig, now time.Time) {
			r.apply("outbox", "queue-timeline:1", queue(10, 7, now.Add(-30*time.Minute), true))
		}},
		{name: "queue removal", series: signals.Queue, value: 0.25, passes: 2, drive: func(r *signalRig, now time.Time) {
			r.apply("outbox", "queue-timeline:1", queue(10, 7, now.Add(-30*time.Minute), true))
			r.apply("outbox", "queue-timeline:2", queue(11, 7, now.Add(-15*time.Minute), false))
		}},
		{name: "queue merge", series: signals.Queue, value: 0.5, passes: 2, drive: func(r *signalRig, now time.Time) {
			r.apply("outbox", "queue-timeline:1", queue(20, 8, now.Add(-time.Hour), true))
			r.apply("outbox", "queue-merged:2", queue(21, 8, now.Add(-30*time.Minute), false))
		}},
		{name: "ask opened", series: signals.Approvals, value: 1, passes: 1, drive: opened},
		askEvent(2, "ask.opened", ask("A2", issue, "question", nil, map[string]any{"restored": true}), 2, "ask restored"),
		askEvent(2, "ask.asked_of", ask("A1", issue, "question", "other@example.invalid", nil), 0, "ask asked of"),
		askEvent(2, "ask.edited", ask("A1", issue, "approval", nil, map[string]any{"approval": map[string]any{"artifact_id": "doc-1", "version": 2, "requested_version": 1}}), 0, "ask edited"),
		{name: "ask handed back", series: signals.Approvals, value: 1, passes: 3, drive: func(r *signalRig, now time.Time) {
			r.dispatch(issue, 1, "ask.opened", now.Add(-time.Minute), ask("A1", issue, "approval", nil, map[string]any{"approval": map[string]any{"artifact_id": "doc-1", "version": 1, "requested_version": 1}}))
			r.dispatch(issue, 2, "ask.edited", now.Add(-30*time.Second), ask("A1", issue, "approval", nil, map[string]any{"approval": map[string]any{"artifact_id": "doc-1", "version": 2, "requested_version": 1}}))
			r.dispatch(issue, 3, "ask.handed_back", now, ask("A1", issue, "approval", nil, map[string]any{"approval": map[string]any{"artifact_id": "doc-1", "version": 2, "requested_version": 2}}))
		}},
		askEvent(2, "comment.created", map[string]any{"id": "comment-1", "ask_id": "A1", "ask_state": "open", "ask_waiting_on": "agent", "body": "A reply"}, 0, "ask comment"),
		askEvent(2, "ask.answered", ask("A1", issue, "question", nil, map[string]any{"state": "answered"}), 0, "ask answered"),
		askEvent(2, "ask.resolved", ask("A1", issue, "question", nil, map[string]any{"state": "resolved"}), 0, "ask resolved"),
		{name: "issue assignee", series: signals.Approvals, value: 0, passes: 2, drive: func(r *signalRig, now time.Time) {
			opened(r, now)
			r.dispatch(issue, 2, "issue.updated", now, map[string]any{"key": issue, "status": "in_progress", "title": "Settle it", "rank": "a", "labels": []string{},
				"parent": nil, "claim": nil, "priority": 2, "resolved_repo": "acme/widgets", "blocked_by": []string{}, "assignee": "other@example.invalid"})
		}},
		{name: "default assignee", series: signals.Approvals, value: 0, passes: 2, drive: func(r *signalRig, now time.Time) {
			// The ask is asked of the default assignee by name, so it waits on that person, and no
			// longer on the project's default once it moves to another.
			r.dispatch(issue, 1, "ask.opened", now.Add(-time.Minute), ask("A1", issue, "question", "owner@example.invalid", nil))
			r.apply("dispatch", "settings:1", intake.DispatchDefaultAssignee{Assignee: "lead@example.invalid", Seq: 1, At: now})
		}},
		{name: "issue pod guard kill", series: signals.Kills, value: 1, passes: 1, drive: func(r *signalRig, now time.Time) { r.guardKill(now) }},
		// A report of either host moves the headroom: the pinned Oh My Pi host's, and the Claude
		// Code bridge's, whose answered requests are all it reports.
		{name: "Oh My Pi report", series: signals.Provider, value: 4000.0 / 6000, passes: 1, limits: map[string]float64{signals.Provider: 0.5}, drive: func(r *signalRig, now time.Time) {
			r.requests(hostToken(r.t, issue, claim.RoleImplementer), "omp", 4000, now.Truncate(time.Minute).Add(-30*time.Second))
		}},
		{name: "Claude Code report", series: signals.Provider, value: 3000.0 / 6000, passes: 1, limits: map[string]float64{signals.Provider: 0.5}, drive: func(r *signalRig, now time.Time) {
			r.requests(hostToken(r.t, issue, claim.RoleReviewer), "bridge", 3000, now.Truncate(time.Minute).Add(-30*time.Second))
		}},
		// A turn edge runs the pass only where a series moved against the last committed pass: the
		// holder's claim is written with no edge, a pass records the holder as it stands, and then
		// the edge comes, so each turn row counts that pass and the edge's.
		{name: "holder turn start", series: signals.Idle, value: 0, passes: 2, limits: map[string]float64{signals.Idle: 0.5}, drive: func(r *signalRig, now time.Time) {
			r.holder(issue, now.Add(-3*time.Hour))
			r.claimed(issue, supervise.StateIdle, now.Add(-3*time.Hour), now.Add(-2*time.Hour))
			r.apply("pause", "resume", intake.ProjectResumed{})
			r.turn(issue, supervise.StateWorking, now, now.Add(-2*time.Hour), true)
		}},
		{name: "holder turn end", series: signals.Idle, value: 0, passes: 1, limits: map[string]float64{signals.Idle: 0.5}, drive: func(r *signalRig, now time.Time) {
			r.holder(issue, now.Add(-3*time.Hour))
			r.claimed(issue, supervise.StateWorking, now.Add(-10*time.Minute), now.Add(-2*time.Hour))
			r.apply("pause", "resume", intake.ProjectResumed{})
			r.turn(issue, supervise.StateIdle, now.Add(-10*time.Minute), now, false)
		}},
		// A person's change of a setting, applied as the operator's and the controller's routes
		// apply it, runs its own pass, which admits by the change: the kill at the limit holds
		// until admission by signals is turned off, the limit set at the kill holds, and the limit
		// cleared under it holds no more.
		{name: "switch", series: signals.Kills, value: 1, passes: 2, limits: map[string]float64{signals.Kills: 1}, holds: new(false), drive: func(r *signalRig, now time.Time) {
			r.guardKill(now)
			r.setting(now, store.SignalSwitchSetting, new(0.0), "the owner turned admission by signals off")
		}},
		{name: "limit set", series: signals.Kills, value: 1, passes: 2, holds: new(true), drive: func(r *signalRig, now time.Time) {
			r.guardKill(now)
			r.setting(now, signals.Kills, new(1.0), "the owner set the kills' limit at one")
		}},
		{name: "limit clear", series: signals.Kills, value: 1, passes: 2, limits: map[string]float64{signals.Kills: 1}, holds: new(false), drive: func(r *signalRig, now time.Time) {
			r.guardKill(now)
			r.setting(now, signals.Kills, nil, "the owner cleared the kills' limit")
		}},
	}
}

// Each source event of plan section 4's list reaches admission's pass through the daemon's own
// path with no tick: the merge queue's timeline the outbox applies, each Dispatch event decoded
// as its consumer decodes it, a kill the launcher session hands the kill holder, a host's report
// of model requests, and a slot holder's turn edge the supervisor tells the runtime. Each commits
// the passes the pass rule and the series-moved pass rule give it, and its last committed pass reads the
// input it kept; a turn's end moves no series, so it runs none and sets the deadline at the hour
// its holder is idle. A kill delivered again commits no pass, nor does a fact whose transaction
// rolls back. It fails where a source event has no row.
func TestSignalEventPassInventory(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute).Add(20 * time.Second)
	rows := map[string]bool{}
	for _, event := range signalEvents() {
		rows[event.name] = true
		t.Run(event.name, func(t *testing.T) {
			limits := event.limits
			if limits == nil {
				limits = map[string]float64{signals.Kills: 5}
			}
			r := newSignalRig(t, limits)
			r.boot(now.Add(-7*time.Hour), now)
			before := r.passCount()
			event.drive(r, now)
			if got := r.passCount() - before; got != event.passes {
				t.Errorf("the event committed %d passes, want %d", got, event.passes)
			}
			if got := r.reading(event.series); !got.Value.Recorded || got.Value.Value != event.value {
				t.Errorf("the %s series reads %+v after the event, want %v", event.series, got.Value, event.value)
			}
			if r.passCount() > before && slices.Contains(r.passes[before:], signals.PassTick) {
				t.Errorf("a tick ran among the event's passes %v", r.passes[before:])
			}
			if event.name == "holder turn end" {
				if got := r.committed(); !got.Set || !got.Due.Equal(now.Add(time.Hour)) {
					t.Errorf("after the turn's end the committed deadline is %+v, want the hour the holder is idle, %s", got, now.Add(time.Hour))
				}
			}
			if event.holds != nil {
				if got := r.reading(event.series); got.Holds != *event.holds {
					t.Errorf("the %s series holds %t after the event, want %t", event.series, got.Holds, *event.holds)
				}
				if got := r.writtenHold(event.series); got != *event.holds {
					t.Errorf("the event's last pass wrote the %s series holding %t, want %t", event.series, got, *event.holds)
				}
			}
		})
	}
	for _, source := range signalEventSources {
		if !rows[source] {
			t.Errorf("the source event %q has no row", source)
		}
	}
	t.Run("a duplicate and a rollback", func(t *testing.T) {
		r := newSignalRig(t, map[string]float64{signals.Kills: 5})
		r.boot(now.Add(-7*time.Hour), now)
		report := shimwire.LauncherMemoryKill{Guard: "guard-1", Sequence: 1, PID: 4242, Executable: "python3", LimitBytes: 4 << 30, At: now.Add(-time.Minute)}
		token := hostToken(t, memoryGuardIssue, claim.RoleTester)
		before := r.passCount()
		for range 2 {
			if err := r.kills.killed(context.Background(), memoryGuardIssue, token, "pod-1", report); err != nil {
				t.Fatalf("apply the kill: %v", err)
			}
		}
		if got := r.passCount() - before; got != 1 {
			t.Errorf("a kill delivered twice committed %d passes, want 1: the second delivery is a duplicate", got)
		}
		failing := handlerOf(func(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
			return intake.Result{}, errors.New("the transaction fails after admission")
		})
		report.Sequence = 2
		if _, err := intake.ApplyFact(context.Background(), r.pool, "launcher", "kill-rolled-back", intake.MemoryGuardKill{Issue: memoryGuardIssue,
			Token: token, PodUID: "pod-1", Report: report}, append(slices.Clone(r.w.handlers), failing)...); err == nil {
			t.Fatalf("the kill's transaction committed, want it rolled back")
		}
		if got := r.passCount() - before; got != 1 {
			t.Errorf("a rolled-back kill committed a pass: %d passes since the kills, want 1", got)
		}
		if got := r.reading(signals.Kills); got.Value.Value != 1 {
			t.Errorf("the kills read %v after the rollback, want 1: the rolled-back kill is not kept", got.Value.Value)
		}
	})
}

// handlerOf is an intake.Handler of one function.
type handlerOf func(context.Context, pgx.Tx, intake.Fact) (intake.Result, error)

func (f handlerOf) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	return f(ctx, tx, fact)
}

// signalInput is one input of plan section 2 as the daemon keeps it: the facts that bring it, the
// last delivered twice under its own id and once more under another id with the same content, and
// what the store reads of it once kept, which the deliveries again leave as it stood.
type signalInput struct {
	name string
	keep func(r *signalRig, now time.Time, again bool)
	read func(r *signalRig, now time.Time) string
	want string
}

// signalInputRows is each input row of plan section 2, each host's report at each of its two
// flush points (the request ended 15 seconds before, and the turn's end).
var signalInputRows = []string{"queue event", "ask", "issue assignee", "default assignee", "guard kill", "kernel kill of no role",
	"Oh My Pi report mid-turn", "Oh My Pi report at the turn's end", "Claude Code report mid-turn", "Claude Code report at the turn's end"}

func signalInputs() []signalInput {
	const issue = "LEGION-302"
	id := func(again bool, base string) string {
		if again {
			return base + "-again"
		}
		return base
	}
	approvals := func(r *signalRig, now time.Time) string { return fmt.Sprint(r.reading(signals.Approvals).Value.Value) }
	kills := func(r *signalRig, now time.Time) string { return fmt.Sprint(r.reading(signals.Kills).Value.Value) }
	// Each host's report is read for its tokens of the minute before.
	report := func(name string, role claim.Role, ended time.Duration) signalInput {
		return signalInput{name: name, want: "300 tokens", keep: func(r *signalRig, now time.Time, again bool) {
			at := now.Truncate(time.Minute).Add(-time.Minute).Add(ended)
			tokens := int64(300)
			r.apply("api", id(again, "requests:"+name), intake.ModelRequests{Claim: hostToken(r.t, issue, role), Generation: 1,
				Requests: []claim.ModelRequest{{ID: name, Session: "ses_" + name, Model: "acme-model", EndedAt: at, UncachedInputTokens: &tokens}}})
		}, read: func(r *signalRig, now time.Time) string {
			var minutes record.ProviderMinutes
			if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				minutes, err = record.NewStore().ProviderMinutes(ctx, tx, killsProject, now.Truncate(time.Minute).Add(-time.Minute), now.Truncate(time.Minute), api.ModelRequestsContract())
				return err
			}); err != nil {
				r.t.Fatal(err)
			}
			minute, _ := minutes.Read(now.Truncate(time.Minute).Add(-time.Minute))
			return fmt.Sprintf("%d tokens", minute.Uncached)
		}}
	}
	return []signalInput{
		{name: "queue event", want: "0.5", keep: func(r *signalRig, now time.Time, again bool) {
			r.apply("outbox", id(again, "queue-timeline:1"), intake.QueueTimeline{Repo: "acme/widgets", Number: 7,
				Events: []signals.QueueEvent{{ID: 10, Repo: "acme/widgets", PullRequest: 7, At: now.Add(-30 * time.Minute), Enters: true}}})
		}, read: func(r *signalRig, now time.Time) string { return fmt.Sprint(r.reading(signals.Queue).Value.Value) }},
		{name: "ask", want: "1", keep: func(r *signalRig, now time.Time, again bool) {
			fact, _, err := intake.DecodeDispatchEvent(killsProject, issue, []byte(mustJSON(r.t, map[string]any{"id": 9001, "issue_key": issue, "artifact_id": nil,
				"project": killsProject, "seq": 1, "type": "ask.opened", "actor": map[string]any{"kind": "session", "id": "ses-architect"}, "notify": false,
				"created_at": now.Format(time.RFC3339Nano), "payload": ask("A1", issue, "question", nil, nil)})))
			if err != nil {
				r.t.Fatal(err)
			}
			r.apply("dispatch", id(again, "dispatch:ask"), fact)
		}, read: approvals},
		{name: "issue assignee", want: "0", keep: func(r *signalRig, now time.Time, again bool) {
			if !again {
				r.apply("dispatch", "dispatch:ask", intake.DispatchAskTurn{Key: issue, Mark: record.EventMark{Seq: 1},
					Turn: signals.AskEvent{Kind: signals.AskOpened, Ask: "A1", Issue: issue}})
			}
			r.apply("dispatch", id(again, "dispatch:assignee"), intake.ApprovalsBootRead{Read: record.ApprovalsRead{
				Assignees: []record.IssueAssignee{{Issue: issue, Assignee: "other@example.invalid", Seq: 2}}}})
		}, read: approvals},
		{name: "default assignee", want: "0", keep: func(r *signalRig, now time.Time, again bool) {
			// The ask is asked of the default assignee by name, so the move of the project's default
			// to another person leaves it waiting on no default.
			if !again {
				r.apply("dispatch", "dispatch:ask", intake.DispatchAskTurn{Key: issue, Mark: record.EventMark{Seq: 1},
					Turn: signals.AskEvent{Kind: signals.AskOpened, Ask: "A1", Issue: issue, AskedOfSet: true, AskedOf: "owner@example.invalid"}})
			}
			r.apply("dispatch", id(again, "settings:1"), intake.DispatchDefaultAssignee{Assignee: "lead@example.invalid", Seq: 1, At: now})
		}, read: approvals},
		{name: "guard kill", want: "1", keep: func(r *signalRig, now time.Time, again bool) {
			r.apply("launcher", id(again, "kill:1"), intake.MemoryGuardKill{Issue: memoryGuardIssue, Token: hostToken(r.t, memoryGuardIssue, claim.RoleTester),
				PodUID: "pod-1", Report: shimwire.LauncherMemoryKill{Guard: "guard-1", Sequence: 1, PID: 4242, Executable: "python3", LimitBytes: 4 << 30, At: now.Add(-time.Minute)}})
		}, read: kills},
		{name: "kernel kill of no role", want: "1", keep: func(r *signalRig, now time.Time, again bool) {
			r.apply("launcher", id(again, "kill:2"), intake.MemoryGuardKill{Issue: memoryGuardIssue, Token: hostToken(r.t, memoryGuardIssue, claim.RoleTester),
				PodUID: "pod-1", Report: shimwire.LauncherMemoryKill{Guard: "guard-1", Sequence: 2, PID: 4243, Executable: "postgres", LimitBytes: 4 << 30,
					At: now.Add(-time.Minute), Cause: shimwire.MemoryKillCauseKernel, NoRole: true}})
		}, read: kills},
		report("Oh My Pi report mid-turn", claim.RoleImplementer, 15*time.Second),
		report("Oh My Pi report at the turn's end", claim.RoleImplementer, 50*time.Second),
		report("Claude Code report mid-turn", claim.RoleReviewer, 15*time.Second),
		report("Claude Code report at the turn's end", claim.RoleReviewer, 50*time.Second),
	}
}

// Each input of plan section 2 is kept once, whichever way it arrives: delivered, delivered again
// under its own id (a duplicate the fact's mark refuses), and brought again under another id (a
// read of the same source by another path, which the row's own key keeps once); and each host's
// report at each of its two flush points, the request 15 seconds after it ended and at the turn's
// end, is kept, and read by the live signals, once: its tokens in the headroom's minute. It fails
// where an input has no row.
func TestSignalInputInventory(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute).Add(20 * time.Second)
	rows := map[string]bool{}
	for _, input := range signalInputs() {
		rows[input.name] = true
		t.Run(input.name, func(t *testing.T) {
			r := newSignalRig(t, map[string]float64{signals.Kills: 5})
			r.boot(now.Add(-7*time.Hour), now)
			input.keep(r, now, false)
			first := input.read(r, now)
			input.keep(r, now, false)
			input.keep(r, now, true)
			if again := input.read(r, now); first != input.want || again != input.want {
				t.Errorf("the input reads %s once kept and %s after it came again, want %s both times", first, again, input.want)
			}
		})
	}
	for _, input := range signalInputRows {
		if !rows[input] {
			t.Errorf("the input %q has no row", input)
		}
	}
}

// The daemon's timer of the signal deadline through its own runtime (runSignalDeadline): a kill
// that brings the kills to their limit commits the deadline of the moment it leaves its hour,
// two seconds on, and the timer the commit armed fires it on the wall clock, applying
// intake.SignalDeadline through the fact path; its one pass is a deadline's, which computes the
// deadline again, none being due. The daemon's stop ends the runtime and its timer.
func TestDaemonSignalDeadlineLive(t *testing.T) {
	r := newSignalRig(t, map[string]float64{signals.Kills: 1})
	r.boot(time.Now().Add(-7*time.Hour), time.Now())
	r.mu.Lock()
	r.clock = time.Now
	r.mu.Unlock()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.w.runSignalDeadline(store.ForConcurrentWorker(ctx))
	}()
	before := r.passCount()
	if err := r.kills.killed(context.Background(), memoryGuardIssue, hostToken(t, memoryGuardIssue, claim.RoleTester), "pod-1",
		shimwire.LauncherMemoryKill{Guard: "guard-1", Sequence: 1, PID: 4242, Executable: "python3", LimitBytes: 4 << 30,
			At: time.Now().Add(-signals.KillSpan + 2*time.Second)}); err != nil {
		t.Fatalf("apply the kill: %v", err)
	}
	if committed := r.committed(); !committed.Set {
		t.Fatalf("the kill committed no deadline: %+v", committed)
	}
	testwait.Eventually(t, "the deadline's pass", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return slices.Contains(r.passes[before:], signals.PassDeadline)
	})
	r.mu.Lock()
	deadlines := 0
	for _, kind := range r.passes[before:] {
		if kind == signals.PassDeadline {
			deadlines++
		}
	}
	r.mu.Unlock()
	if deadlines != 1 {
		t.Errorf("the timer ran %d deadline passes, want 1", deadlines)
	}
	if got := r.committed(); got.Set {
		t.Errorf("after the deadline's pass the committed deadline is %s, want none: no crossing is due", got.Due)
	}
	stop()
	testwait.Receive(t, "the signal deadline's runtime to end at the daemon's stop", done)
}
