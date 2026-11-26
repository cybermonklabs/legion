package admit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// The live signals (LiveSignals) as the daemon registers them: their reader is the pass's gate
// (UseSignals) and the admission keeps beside each pass what they read (UseLiveSignals). Every
// input below reaches the store through the fact the daemon applies for it, through the daemon's
// own chain of handlers, as signal_rule_test.go applies its facts; only the moments a test needs
// before the fixed clock (a slot taken, a turn ended) are seeded.

// settingsOf is SignalSettings with one answer, which a test may change between facts as a
// person's change of a setting would.
type settingsOf struct {
	mu sync.Mutex
	sw SignalSwitch
}

func (s *settingsOf) SignalSwitch(context.Context, pgx.Tx, string) (SignalSwitch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sw, nil
}

func (s *settingsOf) set(sw SignalSwitch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sw = sw
}

// on is admission by signals on with limits, by series name.
func on(limits map[string]float64) SignalSwitch { return SignalSwitch{On: true, Limits: limits} }

// liveTest is an admission of cap over acmeProject whose passes read the live signals under
// settings, registered as the daemon registers them, logging to logged, with each deadline it
// arms once a fact commits in armed.
type liveTest struct {
	admission *Admission
	live      *LiveSignals
	settings  *settingsOf
	logged    *testlog.Buffer
	mu        sync.Mutex
	armed     []record.SignalDeadline
	passes    []signals.PassKind
}

// newLiveTest is a liveTest under the daemon's own rule, whose issues read the priorities named
// (prioritized), and whose model provider has a token limit of 6,000 a minute.
func newLiveTest(t *testing.T, cap int, priorities map[string]record.Priority, sw SignalSwitch) *liveTest {
	t.Helper()
	lt := &liveTest{settings: &settingsOf{sw: sw}, logged: &testlog.Buffer{}}
	lt.admission = ruledAdmission(t, cap, record.MainRule, slog.New(slog.NewJSONHandler(lt.logged, nil)))
	lt.admission.store = prioritized{Store: lt.admission.store, of: priorities}
	lt.admission.UseModelRequests(6000, nil)
	lt.live = NewLiveSignals(lt.admission, lt.settings, 0)
	lt.admission.UseSignals(lt.live.Read)
	lt.admission.UseLiveSignals(lt.live, func(deadline record.SignalDeadline) {
		lt.mu.Lock()
		defer lt.mu.Unlock()
		lt.armed = append(lt.armed, deadline)
	})
	lt.admission.UsePassObserver(func(view PassView) {
		lt.mu.Lock()
		defer lt.mu.Unlock()
		lt.passes = append(lt.passes, view.Kind)
	})
	return lt
}

// at sets the admission's clock.
func (lt *liveTest) at(now time.Time) { lt.admission.now = func() time.Time { return now } }

// passCount is how many passes committed so far.
func (lt *liveTest) passCount() int {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return len(lt.passes)
}

// lastArmed is the deadline armed last, and whether any was.
func (lt *liveTest) lastArmed() (record.SignalDeadline, bool) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if len(lt.armed) == 0 {
		return record.SignalDeadline{}, false
	}
	return lt.armed[len(lt.armed)-1], true
}

// boot is the daemon's boot at since, which records that it keeps every input from then on, and
// the boot's read of Dispatch, which gives defaultAssignee ("" for none) as the project's default
// assignee and asks, each open and waiting on a person, on its own issue with no assignee. The
// clock is the fixed clock again after it.
func (lt *liveTest) boot(t *testing.T, pool *pgxpool.Pool, since time.Time, defaultAssignee string, asks ...string) {
	t.Helper()
	lt.at(since)
	applyThrough(t, pool, lt.admission, "daemon", "boot", intake.DaemonBooted{Boot: 1})
	read := record.ApprovalsRead{DefaultAssignee: defaultAssignee, DefaultAt: since}
	for _, id := range asks {
		read.Asks = append(read.Asks, record.ApprovalAsk{Ask: signals.Ask{ID: id, Issue: "ACME-9" + id, Open: true, Human: true}, Seq: 1})
	}
	applyThrough(t, pool, lt.admission, "daemon", "boot-approvals", intake.ApprovalsBootRead{Read: read})
	lt.at(fixedNow)
}

// kill applies the memory guard's kill of sequence at at in ACME-9's pod.
func (lt *liveTest) kill(t *testing.T, pool *pgxpool.Pool, sequence uint64, at time.Time) {
	t.Helper()
	fact := guardKill(t, sequence)
	fact.Report.At = at
	applyThrough(t, pool, lt.admission, "launcher", fmt.Sprintf("kill-%d", sequence), fact)
}

// queued applies GitHub's merge-queue timeline of pull request number: an entry at entered and,
// where left is not zero, an exit then.
func (lt *liveTest) queued(t *testing.T, pool *pgxpool.Pool, number int, entered, left time.Time) {
	t.Helper()
	events := []signals.QueueEvent{{ID: int64(number) * 10, Repo: "acme/widgets", PullRequest: number, At: entered, Enters: true}}
	if !left.IsZero() {
		events = append(events, signals.QueueEvent{ID: int64(number)*10 + 1, Repo: "acme/widgets", PullRequest: number, At: left})
	}
	applyThrough(t, pool, lt.admission, "outbox", fmt.Sprintf("queue-%d", number), intake.QueueTimeline{Repo: "acme/widgets", Number: number, Events: events})
}

// request applies one report of a host's model request id, ended at ended, of tokens uncached
// input tokens, refused by refusal ("" for none), the host one that reports each 429 its transport
// retries, as the Claude Code bridge does.
func (lt *liveTest) request(t *testing.T, pool *pgxpool.Pool, id string, ended time.Time, tokens int64, refusal claim.RefusalClass) {
	t.Helper()
	lt.report(t, pool, id, ended, tokens, refusal, true)
}

// report applies one report of a host's model request as request does, the batch saying retried
// of whether its host reports each 429 its transport retries: the pinned Oh My Pi host says it
// does not.
func (lt *liveTest) report(t *testing.T, pool *pgxpool.Pool, id string, ended time.Time, tokens int64, refusal claim.RefusalClass, retried bool) {
	t.Helper()
	applyThrough(t, pool, lt.admission, "api", "requests-"+id, intake.ModelRequests{Claim: acmeToken(t, "ACME-9", claim.RoleImplementer), Generation: 1,
		Requests:                []claim.ModelRequest{{ID: id, Session: "ses_9", Model: "acme-model", EndedAt: ended, UncachedInputTokens: &tokens, Refusal: refusal}},
		RetriedRefusalsReported: retried})
}

// heldLines is each line the passes logged of a start a signal held, as "<issue> <signal>/<part>
// <value> at <limit>", in the order logged.
func (lt *liveTest) heldLines(t *testing.T) []string {
	t.Helper()
	var lines []string
	for _, raw := range strings.Split(strings.TrimSpace(lt.logged.String()), "\n") {
		var line struct {
			Msg    string   `json:"msg"`
			Issue  string   `json:"issue"`
			Signal string   `json:"signal"`
			Part   *string  `json:"part"`
			Value  float64  `json:"value"`
			Limit  *float64 `json:"limit"`
		}
		if raw == "" || json.Unmarshal([]byte(raw), &line) != nil || line.Msg != SignalHeldStartLine {
			continue
		}
		part, limit := "", "none"
		if line.Part != nil {
			part = *line.Part
		}
		if line.Limit != nil {
			limit = fmt.Sprint(*line.Limit)
		}
		lines = append(lines, fmt.Sprintf("%s %s/%s %v at %s", line.Issue, line.Signal, part, line.Value, limit))
	}
	return lines
}

// resume is the pass of an event (the resume's fact), under an id of its own.
func (lt *liveTest) resume(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	applyThrough(t, pool, lt.admission, "pause", id, intake.ProjectResumed{})
}

// holderAt records key as a running root holding a slot it took at admitted, with its
// implementer's claim, which is in a turn begun at the turn's start where working, and otherwise
// idle since its last turn ended at end.
func holderAt(t *testing.T, pool *pgxpool.Pool, key string, admitted, end time.Time, working bool) {
	t.Helper()
	acmeRoot(t, pool, key, phase.Implementing, "in_progress", "a"+key, nil, nil)
	takeSlotAt(t, pool, key, admitted)
	idleClaim(t, pool, key, key, claim.RoleImplementer, end.Add(-time.Hour), end)
	if working {
		c := supervise.Claim{Token: acmeToken(t, key, claim.RoleImplementer), Project: acmeProjectToken(t), Tree: key, Issue: key,
			Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, State: supervise.StateWorking, Session: "ses_implementer_" + key,
			LastTurnStart: end, LastTurnEnd: end.Add(-time.Hour)}
		if err := claimStore(t, pool).PutClaim(context.Background(), c); err != nil {
			t.Fatalf("record the working claim of %s: %v", key, err)
		}
	}
}

// The queued root every row of the gate's tests offers a slot to, below P0, and the root that
// holds one, which no signal takes back.
const (
	gateQueued = "ACME-301"
	gateHolder = "ACME-302"
)

// seriesRow is one series of the gate's table: the inputs that make it read value, at which each
// row sets its limit, a legal limit above value under which it holds nothing, and each way the
// series can read as not recorded, the inputs under which it does, with value's limit set.
type seriesRow struct {
	name    string
	value   float64
	under   float64
	arrange func(t *testing.T, pool *pgxpool.Pool, lt *liveTest)
	absent  []absentCase
}

// absentCase is one cause for which a series reads as not recorded, and inputs under which it
// does.
type absentCase struct {
	cause   string
	arrange func(t *testing.T, pool *pgxpool.Pool, lt *liveTest)
}

// providerCoverage is each cause, by the provider's part, for which the gate's table holds that
// part not recorded: the headroom with no token limit configured, and the refusals over an hour
// the daemon did not wholly keep and while a request of a host that cannot report the 429s its
// transport retried is in the hour.
var providerCoverage = []string{"provider/headroom: no token limit", "provider/refusals: an hour not wholly kept",
	"provider/refusals: a host that cannot report its retried 429s"}

// seriesRows is one row for each of the six series (signals.Names()), the provider's headroom and
// its refusals each a row of its own.
func seriesRows() []seriesRow {
	booted := func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
		t.Helper()
		lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
	}
	refusals := func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
		t.Helper()
		lt.request(t, pool, "r1", fixedNow.Add(-50*time.Minute), 0, claim.RefusalOwn)
		lt.request(t, pool, "r2", fixedNow.Add(-40*time.Minute), 0, claim.RefusalOwn)
		lt.request(t, pool, "r3", fixedNow.Add(-30*time.Minute), 0, claim.RefusalUnknown)
		lt.request(t, pool, "r4", fixedNow.Add(-20*time.Minute), 0, claim.RefusalProvider)
	}
	return []seriesRow{
		{name: signals.Idle, value: 0.5, under: 0.75, arrange: func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
			t.Helper()
			booted(t, pool, lt)
			// The holder idle since 10:00 is idle for two hours, and the one in a turn works: half the
			// slots in use are idle for an hour.
			holderAt(t, pool, "ACME-303", fixedNow.Add(-3*time.Hour), fixedNow.Add(-2*time.Hour), false)
			holderAt(t, pool, gateHolder, fixedNow.Add(-3*time.Hour), fixedNow.Add(-10*time.Minute), true)
		}},
		{name: signals.Queue, value: 3, under: 4, arrange: func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
			t.Helper()
			booted(t, pool, lt)
			lt.queued(t, pool, 7, fixedNow.Add(-3*time.Hour), time.Time{})
		}, absent: []absentCase{{cause: "the boot's read of the merge queue failed", arrange: func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
			t.Helper()
			booted(t, pool, lt)
			lt.queued(t, pool, 7, fixedNow.Add(-3*time.Hour), time.Time{})
			applyThrough(t, pool, lt.admission, "daemon", "queue-read", intake.QueueBootStatus{ReadError: "GitHub answered 502"})
		}}}},
		// The headroom counts the tokens of a host that cannot report the 429s its transport
		// retries as any host's: such a host leaves only the refusals unrecorded.
		{name: signals.Provider, value: 0.5, under: 0.75, arrange: func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
			t.Helper()
			booted(t, pool, lt)
			lt.report(t, pool, "r1", fixedNow.Add(-30*time.Second), 3000, "", false)
		}, absent: []absentCase{{cause: "no token limit", arrange: func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
			t.Helper()
			lt.admission.UseModelRequests(0, nil)
			booted(t, pool, lt)
			lt.request(t, pool, "r1", fixedNow.Add(-30*time.Second), 3000, "")
		}}}},
		{name: signals.Refusals, value: 4, under: 5, arrange: func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
			t.Helper()
			booted(t, pool, lt)
			refusals(t, pool, lt)
		}, absent: []absentCase{{cause: "an hour not wholly kept", arrange: func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
			t.Helper()
			// The daemon began keeping the requests 30 minutes ago, so the hour before holds
			// minutes it did not keep.
			lt.boot(t, pool, fixedNow.Add(-30*time.Minute), "acme-owner@example.invalid")
			for i := range 5 {
				lt.request(t, pool, fmt.Sprintf("r%d", i), fixedNow.Add(-time.Duration(20-i)*time.Minute), 0, claim.RefusalOwn)
			}
		}}, {cause: "a host that cannot report its retried 429s", arrange: func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
			t.Helper()
			// The four refusals stand, and an answered request of a host that cannot report the
			// 429s its transport retried ended 50 minutes ago, in the hour the refusals read.
			booted(t, pool, lt)
			refusals(t, pool, lt)
			lt.report(t, pool, "unreported", fixedNow.Add(-50*time.Minute), 100, "", false)
		}}}},
		{name: signals.Kills, value: 2, under: 3, arrange: func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
			t.Helper()
			booted(t, pool, lt)
			lt.kill(t, pool, 1, fixedNow.Add(-30*time.Minute))
			lt.kill(t, pool, 2, fixedNow.Add(-10*time.Minute))
		}, absent: []absentCase{{cause: "an hour not wholly kept", arrange: func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
			t.Helper()
			// The daemon began keeping the kills 30 minutes ago, so their hour is not whole.
			lt.boot(t, pool, fixedNow.Add(-30*time.Minute), "acme-owner@example.invalid")
			lt.kill(t, pool, 1, fixedNow.Add(-20*time.Minute))
			lt.kill(t, pool, 2, fixedNow.Add(-10*time.Minute))
		}}}},
		{name: signals.Approvals, value: 2, under: 3, arrange: func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
			t.Helper()
			lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid", "1", "2")
		}, absent: []absentCase{{cause: "no default assignee", arrange: func(t *testing.T, pool *pgxpool.Pool, lt *liveTest) {
			t.Helper()
			lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "", "1", "2")
		}}}},
	}
}

// lineRow is one hold of the line the gate's tests keep with admission by signals on: arrange
// seeds the issues, and keys are the ones whose start each run reads; held is the key that must
// not start, "" for a row of the line's order and the cap.
type lineRow struct {
	hold    string
	cap     int
	keys    []string
	held    string
	arrange func(t *testing.T, pool *pgxpool.Pool, a *Admission)
	prio    map[string]record.Priority
}

// The holds of the line every row of lineRows names (plan section 4, TestLiveSignalGate/line), and
// the line's order and the cap beside them.
var gateLineHolds = []string{"line order with spare cap", "full cap", "priority", "legion label off", "not todo", "open blocker",
	"closed design gate", "ancestor's hold", "lingering tree", "paused project", "boot catch-up"}

func lineRows() []lineRow {
	return []lineRow{
		{hold: "line order with spare cap", cap: 1, keys: []string{"ACME-311", "ACME-312"}, arrange: func(t *testing.T, pool *pgxpool.Pool, _ *Admission) {
			acmeQueuedRoot(t, pool, "ACME-311", "a")
			acmeQueuedRoot(t, pool, "ACME-312", "b")
		}},
		{hold: "full cap", cap: 1, keys: []string{"ACME-313"}, held: "ACME-313", arrange: func(t *testing.T, pool *pgxpool.Pool, _ *Admission) {
			acmeRoot(t, pool, "ACME-314", phase.Implementing, "in_progress", "a", nil, nil)
			takeSlot(t, pool, "ACME-314")
			acmeQueuedRoot(t, pool, "ACME-313", "b")
		}},
		{hold: "priority", cap: 1, keys: []string{"ACME-315", "ACME-316"}, held: "ACME-315",
			prio: map[string]record.Priority{"ACME-315": record.P2, "ACME-316": record.P1},
			arrange: func(t *testing.T, pool *pgxpool.Pool, _ *Admission) {
				acmeQueuedRoot(t, pool, "ACME-315", "a")
				acmeQueuedRoot(t, pool, "ACME-316", "b")
			}},
		{hold: "legion label off", cap: 2, keys: []string{"ACME-317"}, held: "ACME-317", arrange: func(t *testing.T, pool *pgxpool.Pool, _ *Admission) {
			putIssue(t, pool, record.Issue{Key: "ACME-317", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: acmeProject,
				Title: "ACME-317", Tree: "ACME-317", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "a"})
			seedMirrorIn(t, pool, acmeProject, "ACME-317", "todo")
		}},
		{hold: "not todo", cap: 2, keys: []string{"ACME-318"}, held: "ACME-318", arrange: func(t *testing.T, pool *pgxpool.Pool, _ *Admission) {
			putIssue(t, pool, record.Issue{Key: "ACME-318", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: acmeProject,
				Title: "ACME-318", Tree: "ACME-318", Phase: phase.Admitted, Generation: 1, Status: "backlog", Rank: "a", HandedOver: true})
			seedMirrorIn(t, pool, acmeProject, "ACME-318", "backlog")
		}},
		{hold: "open blocker", cap: 2, keys: []string{"ACME-319"}, held: "ACME-319", arrange: func(t *testing.T, pool *pgxpool.Pool, _ *Admission) {
			seedWaitingForProject(t, pool, acmeProject, "ACME-319", "a")
			seedMirrorIn(t, pool, acmeProject, "ACME-319", "todo", "ACME-320")
			seedMirrorIn(t, pool, acmeProject, "ACME-320", "in_progress")
		}},
		{hold: "closed design gate", cap: 3, keys: []string{"ACME-322"}, held: "ACME-322", arrange: func(t *testing.T, pool *pgxpool.Pool, _ *Admission) {
			acmeRoot(t, pool, "ACME-321", phase.Admitted, "in_progress", "a", nil, nil)
			takeSlot(t, pool, "ACME-321")
			acmeChild(t, pool, "ACME-322", "ACME-321", phase.Admitted, "todo", "b")
		}},
		{hold: "ancestor's hold", cap: 3, keys: []string{"ACME-324"}, held: "ACME-324", arrange: func(t *testing.T, pool *pgxpool.Pool, _ *Admission) {
			acmeRoot(t, pool, "ACME-323", phase.Implementing, "in_progress", "a", nil, nil)
			takeSlot(t, pool, "ACME-323")
			putIssue(t, pool, record.Issue{Key: "ACME-324", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: acmeProject,
				Title: "ACME-324", Tree: "ACME-324", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "b", HandedOver: true,
				Ancestry: []string{"ACME-323"}})
			seedMirrorIn(t, pool, acmeProject, "ACME-324", "todo")
		}},
		{hold: "lingering tree", cap: 3, keys: []string{"ACME-326"}, held: "ACME-326", arrange: func(t *testing.T, pool *pgxpool.Pool, _ *Admission) {
			until := fixedNow.Add(time.Hour)
			putIssue(t, pool, record.Issue{Key: "ACME-325", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "ACME-325",
				Project: acmeProject, Title: "ACME-325", Phase: phase.Done, Generation: 1, Status: "done", Rank: "a", HandedOver: true,
				LingerUntil: &until, LastDispatchSeq: 1})
			seedMirrorIn(t, pool, acmeProject, "ACME-325", "done")
			acmeChild(t, pool, "ACME-326", "ACME-325", phase.Admitted, "todo", "b")
		}},
		{hold: "paused project", cap: 2, keys: []string{"ACME-327"}, held: "ACME-327", arrange: func(t *testing.T, pool *pgxpool.Pool, _ *Admission) {
			acmeQueuedRoot(t, pool, "ACME-327", "a")
			if _, err := pool.Exec(context.Background(), `insert into project_pauses (project, began_at, by_kind, reason)
				values ($1, $2, 'operator', 'the model provider is down')`, acmeProject, fixedNow.Add(-time.Hour)); err != nil {
				t.Fatalf("pause %s: %v", acmeProject, err)
			}
		}},
		{hold: "boot catch-up", cap: 2, keys: []string{"ACME-328"}, held: "ACME-328", arrange: func(t *testing.T, pool *pgxpool.Pool, a *Admission) {
			acmeQueuedRoot(t, pool, "ACME-328", "a")
			a.HoldForCatchUp("ACME-328")
		}},
	}
}

// startedOf is which of keys started, in their order: a root whose tree stands open, or a member
// whose first phase began.
func startedOf(t *testing.T, pool *pgxpool.Pool, keys []string) []string {
	t.Helper()
	var started []string
	for _, key := range keys {
		got := issue(t, pool, key)
		if (got.Parent == nil && treeOpenForProject(t, pool, acmeProject, key)) || (got.Parent != nil && got.Phase != phase.Admitted) {
			started = append(started, key)
		}
	}
	return started
}

// The gate's table has a row for each of the six series, the provider's headroom and its refusals
// each named by its own part, for each cause for which a part of the provider reads as not
// recorded (providerCoverage), and for each hold of the line plan section 4 keeps with admission
// by signals on. It fails where one lacks a row.
func TestSignalGateInventory(t *testing.T) {
	series := map[string]bool{}
	parts := map[string]bool{}
	for _, row := range seriesRows() {
		series[row.name] = true
		signal, part := store.SignalPart(row.name)
		parts[signal+"/"+part] = true
		for _, absent := range row.absent {
			parts[signal+"/"+part+": "+absent.cause] = true
		}
	}
	for _, name := range signals.Names() {
		if !series[name] {
			t.Errorf("the series %s has no row of TestLiveSignalGate/series", name)
		}
	}
	for _, part := range append([]string{"provider/headroom", "provider/refusals"}, providerCoverage...) {
		if !parts[part] {
			t.Errorf("the provider's part %s has no row of TestLiveSignalGate/series", part)
		}
	}
	holds := map[string]bool{}
	for _, row := range lineRows() {
		holds[row.hold] = true
	}
	for _, hold := range gateLineHolds {
		if !holds[hold] {
			t.Errorf("the hold %q has no row of TestLiveSignalGate/line", hold)
		}
	}
}

// With admission by signals on, the live signals decide each start below P0 at an event's pass, by
// the six series as the store keeps their inputs, and with it off nothing changes (plan section 4,
// the table's rows of TestLiveSignalGate).
func TestLiveSignalGate(t *testing.T) {
	// off: admission by signals off, with a limit set and the kills over it, starts the waiting root
	// at the tick's pass as the daemon always did, and logs no hold.
	t.Run("off", func(t *testing.T) {
		pool := migratedPool(t)
		lt := newLiveTest(t, 3, nil, SignalSwitch{Limits: map[string]float64{signals.Kills: 1}})
		lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
		lt.kill(t, pool, 1, fixedNow.Add(-10*time.Minute))
		acmeQueuedRoot(t, pool, gateQueued, "b")
		tickThrough(t, pool, lt.admission, "tick")
		if got, want := openedWord(t, pool, gateQueued), "the trees open of [ACME-301] are [ACME-301]"; got != want {
			t.Errorf("after the tick with admission by signals off: %s, want %s", got, want)
		}
		if held := lt.heldLines(t); len(held) != 0 {
			t.Errorf("the passes logged starts a signal held %q, want none while admission by signals is off", held)
		}
	})
	// series: each of the six series at its limit holds the start below P0 and logs it with the
	// series, its value and its limit, the holder keeps its slot, and a P0 root still starts; the
	// series under its limit, with no limit, or not recorded holds nothing.
	t.Run("series", func(t *testing.T) {
		for _, row := range seriesRows() {
			signal, part := store.SignalPart(row.name)
			run := func(t *testing.T, limits map[string]float64, arrange func(*testing.T, *pgxpool.Pool, *liveTest), starts bool) {
				t.Helper()
				pool := migratedPool(t)
				const urgent = "ACME-304"
				lt := newLiveTest(t, 4, map[string]record.Priority{urgent: record.P0}, on(limits))
				arrange(t, pool, lt)
				if !holdsSlot(t, pool, gateHolder) {
					acmeRoot(t, pool, gateHolder, phase.Implementing, "in_progress", "a"+gateHolder, nil, nil)
					takeSlotAt(t, pool, gateHolder, fixedNow.Add(-3*time.Hour))
					idleClaim(t, pool, gateHolder, gateHolder, claim.RoleImplementer, fixedNow.Add(-20*time.Minute), fixedNow.Add(-5*time.Minute))
				}
				acmeQueuedRoot(t, pool, gateQueued, "y")
				acmeQueuedRoot(t, pool, urgent, "z")
				lt.resume(t, pool, "resume")
				want := "the trees open of [ACME-301 ACME-304] are [ACME-304]"
				if starts {
					want = "the trees open of [ACME-301 ACME-304] are [ACME-301 ACME-304]"
				}
				if got := openedWord(t, pool, gateQueued, urgent); got != want {
					t.Errorf("after the event's pass: %s, want %s", got, want)
				}
				if !holdsSlot(t, pool, gateHolder) {
					t.Errorf("%s holds no slot after the pass, want the holder's slot kept whatever a signal reads", gateHolder)
				}
				wantLines := []string(nil)
				if !starts {
					wantLines = []string{fmt.Sprintf("%s %s/%s %v at %v", gateQueued, signal, part, row.value, limits[row.name])}
				}
				if got := lt.heldLines(t); !slices.Equal(got, wantLines) {
					t.Errorf("the starts a signal held, as logged = %q, want %q", got, wantLines)
				}
			}
			t.Run(row.name+"/at its limit", func(t *testing.T) { run(t, map[string]float64{row.name: row.value}, row.arrange, false) })
			t.Run(row.name+"/under its limit", func(t *testing.T) { run(t, map[string]float64{row.name: row.under}, row.arrange, true) })
			t.Run(row.name+"/with no limit", func(t *testing.T) { run(t, nil, row.arrange, true) })
			for _, absent := range row.absent {
				t.Run(row.name+"/not recorded: "+absent.cause, func(t *testing.T) { run(t, map[string]float64{row.name: row.value}, absent.arrange, true) })
			}
		}
	})
	// line: with admission by signals on and every series under its limit, the line's order, the
	// cap and each hold decide each start as they do with it off.
	t.Run("line", func(t *testing.T) {
		for _, row := range lineRows() {
			t.Run(row.hold, func(t *testing.T) {
				outcome := func(sw SignalSwitch) []string {
					t.Helper()
					pool := migratedPool(t)
					lt := newLiveTest(t, row.cap, row.prio, sw)
					lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
					row.arrange(t, pool, lt.admission)
					lt.resume(t, pool, "resume")
					return startedOf(t, pool, row.keys)
				}
				off := outcome(SignalSwitch{})
				clear := outcome(on(map[string]float64{signals.Kills: 3, signals.Approvals: 5, signals.Queue: 1}))
				if !slices.Equal(clear, off) {
					t.Errorf("of %v, %v started with admission by signals on and every series under its limit, want %v as with it off", row.keys, clear, off)
				}
				if row.held != "" && slices.Contains(clear, row.held) {
					t.Errorf("%s: %s started, want it held", row.hold, row.held)
				}
			})
		}
	})
	// tick: with admission by signals on and every series under its limit, a tick starts no issue
	// below P0, and gives the waiter's slot back and starts the P0 root as the tick does with it off.
	t.Run("tick", func(t *testing.T) {
		outcome := func(sw SignalSwitch) string {
			t.Helper()
			pool := migratedPool(t)
			const waiter, blocker, ranked, urgent = "ACME-330", "ACME-331", "ACME-332", "ACME-333"
			lt := newLiveTest(t, 3, map[string]record.Priority{ranked: record.P1, urgent: record.P0}, sw)
			lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
			registerControllerFor(t, pool, acmeProject)
			acmeRoot(t, pool, waiter, phase.Implementing, "in_progress", "a", []string{blocker}, nil)
			takeSlot(t, pool, waiter)
			seedMirrorIn(t, pool, acmeProject, blocker, "in_progress")
			acmeQueuedRoot(t, pool, ranked, "b")
			acmeQueuedRoot(t, pool, urgent, "c")
			tickThrough(t, pool, lt.admission, "tick")
			var notices int
			if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'controller_notice' and payload->>'kind' = 'tick'`).Scan(&notices); err != nil {
				t.Fatalf("count the tick notices: %v", err)
			}
			return fmt.Sprintf("%s; %s; %d tick notice", slotWord(t, pool, waiter), openedWord(t, pool, ranked, urgent), notices)
		}
		if got, want := outcome(SignalSwitch{}), "ACME-330 holds no slot; the trees open of [ACME-332 ACME-333] are [ACME-332 ACME-333]; 1 tick notice"; got != want {
			t.Errorf("the tick with admission by signals off: %s\nwant %s", got, want)
		}
		if got, want := outcome(on(map[string]float64{signals.Kills: 3})), "ACME-330 holds no slot; the trees open of [ACME-332 ACME-333] are [ACME-333]; 1 tick notice"; got != want {
			t.Errorf("the tick with admission by signals on: %s\nwant %s", got, want)
		}
	})
}
