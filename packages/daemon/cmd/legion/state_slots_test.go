package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// slotsProject is the Dispatch project of TestTheSlotsLineTheJSONAndTheTickAgree's daemon.
const slotsProject = "ACME"

// slotsStateSource is the state route's source for that test's daemon: the daemon's own
// projection (projection.Project) over the test's database, read in the transaction the route
// opens, with the admission cap the daemon's own source writes beside it. No claim of the test's
// project is stopped, so it hands the projection no stranded claims.
type slotsStateSource struct {
	store *store.Store
	steps []record.DesignStep
	cap   int
	now   time.Time
}

func (s slotsStateSource) State(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	claims, err := s.store.ClaimsTx(ctx, tx)
	if err != nil {
		return api.State{}, nil, err
	}
	state, err := projection.Project(ctx, projection.Inputs{Tx: tx, Store: record.NewStore(), Project: slotsProject, Steps: s.steps,
		Claims: claims, Now: s.now, Cap: s.cap})
	if err != nil {
		return api.State{}, nil, err
	}
	state.Daemon = api.DaemonInfo{Project: slotsProject, SchemaVersion: 1, Boots: 1}
	state.Admission.Cap = s.cap
	return state, claims, nil
}

// slotsLinePattern is the slots line as the spec writes it (R6), with a group for every number
// and every oldest time it prints. A count with no known time prints no oldest, and the last
// sentence names the first issue that waits for a slot or says that none waits.
var slotsLinePattern = regexp.MustCompile(`^slots: (\d+) of (\d+) in use: ` +
	`(\d+) working(?: \(oldest ([^)]+)\))?, (\d+) stopped(?: \(oldest ([^)]+)\))?, (\d+) person, (\d+) issue, ` +
	`(\d+) idle(?: \(oldest ([^)]+)\))?\. ` +
	`Started work without a slot: (\d+) wait only for a slot(?: \(oldest ([^)]+)\))?, (\d+) in all(?: \(oldest ([^)]+)\))?\. ` +
	`(?:(\d+) holders? ranks? below (\S+), which waits for a slot\.|No issue waits for a slot\.)$`)

// slotsReadingJSON is a slots reading as JSON, its line left out where withLine is false: what
// the comparisons below read, so a difference prints as the wire carries it.
func slotsReadingJSON(t *testing.T, reading record.SlotsReading, withLine bool) string {
	t.Helper()
	if !withLine {
		reading.Line = ""
	}
	encoded, err := json.Marshal(reading)
	if err != nil {
		t.Fatalf("marshal the slots reading: %v", err)
	}
	return string(encoded)
}

// slotsLineMinute is an oldest time as the slots line prints it, "" for none.
func slotsLineMinute(at *time.Time) string {
	if at == nil {
		return ""
	}
	return at.UTC().Format("2006-01-02T15:04Z")
}

// R6's last clause, through every surface that reads the slots: one project's records, a tick
// applied to them through the daemon's own handler chain (intake.ApplyFact over admit.Handlers),
// and then the state the daemon's own projection reads of the same records, served by the real
// state route. The text line `legion state` prints, the `admission.slots` of `legion state
// --json` and the `slots` of the tick notice the tick queued give the same number and the same
// oldest time for each of the five states, for both readings of started work without a slot, and
// for the holders below the first issue that waits for one; and the JSON gives the numbers worked
// out by hand from the records below.
//
// The records, at 2026-11-16T09:00Z under a cap of four, all four slots held: ACME-1 at work, its
// implementer in a turn for fifty minutes; ACME-2 held from implementing; ACME-3 waiting on a
// person's answer to an ask for two hours; ACME-4 resting, its implementer's last turn ended
// twenty minutes ago. In the line: ACME-7, released for a wait 150 minutes ago and still waiting
// on a link, so it is in reading (b) and holds nothing back; ACME-6, released for a wait 100
// minutes ago whose link has finished, so it is in both readings and is the first issue that waits
// for a slot; and ACME-5, a root waiting for its tree to be opened. ACME-6 ranks below ACME-1 and
// above the other three holders.
func TestTheSlotsLineTheJSONAndTheTickAgree(t *testing.T) {
	ctx := context.Background()
	st := testpg.CloneStore(t, "legion_cmd_slots")
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	before := func(d time.Duration) time.Time { return now.Add(-d) }
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	steps := config.DesignGate("").Steps()
	const slotCap = 4
	projectToken, err := legionclaim.ProjectToken(slotsProject)
	if err != nil {
		t.Fatal(err)
	}
	records := record.NewStore()
	approved := 1
	slotIndex := 0
	seed := func(what string, fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := st.Tx(ctx, fn); err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	root := func(key, status string, stands phase.Phase, rank string) record.Issue {
		return record.Issue{Key: key, Tree: key, Project: slotsProject, Title: key, Phase: stands, Generation: 1,
			Status: status, DispatchStatus: status, Rank: rank, HandedOver: true,
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	}
	// running records issue as a root whose tree runs, its lifecycle open under the workflow,
	// with its design gate approved where gateOpen says so, holding a slot taken three hours ago
	// where slotted says so.
	running := func(tx pgx.Tx, issue record.Issue, gateOpen, slotted bool) error {
		if err := records.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		if _, err := records.OpenTreeLifecycle(ctx, tx, projectToken, issue.Key, treelifecycle.AuthorityWorkflow); err != nil {
			return err
		}
		if gateOpen {
			if err := records.PutGate(ctx, tx, record.DesignGate{Issue: issue.Key, ArtifactID: "spec-" + issue.Key, LatestVersion: 1,
				ApprovedVersion: &approved, EverOpened: true}); err != nil {
				return err
			}
		}
		if !slotted {
			return nil
		}
		slotIndex++
		return records.PutSlot(ctx, tx, record.Slot{Issue: issue.Key, Index: slotIndex - 1, AdmittedAt: before(3 * time.Hour)})
	}
	mirror := func(tx pgx.Tx, key, status string, waits ...string) error {
		return records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: key, Project: slotsProject, Seq: 1,
			Status: record.Carry(status), Parent: record.Carry(""), Blockers: record.Carry(waits)})
	}
	claimOf := func(key string, role legionclaim.Role, state supervise.ClaimState, start, end time.Time) {
		t.Helper()
		token, err := legionclaim.NewToken(projectToken, key, role)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: projectToken, Tree: key, Issue: key, Role: role,
			TreeEpoch: 1, Generation: 1, State: state, Session: "ses_" + string(token), LastTurnStart: start, LastTurnEnd: end}); err != nil {
			t.Fatalf("record the %s claim of %s: %v", role, key, err)
		}
	}

	seed("the four holders", func(tx pgx.Tx) error {
		if err := running(tx, root("ACME-1", "in_progress", phase.Implementing, "a"), true, true); err != nil {
			return err
		}
		held := root("ACME-2", "in_progress", phase.Held, "c")
		held.Hold = &record.Hold{From: phase.Implementing}
		if err := running(tx, held, true, true); err != nil {
			return err
		}
		// No gate row stands for ACME-3, so the pass's reconciliation answers nothing of its wait.
		asking := root("ACME-3", "in_progress", phase.AwaitingPerson, "d")
		asking.Awaiting = &record.Awaiting{Ask: "ask-3", Since: record.EventMark{Seq: 5, At: before(2 * time.Hour)}}
		if err := running(tx, asking, false, true); err != nil {
			return err
		}
		return running(tx, root("ACME-4", "in_progress", phase.Implementing, "e"), true, true)
	})
	claimOf("ACME-1", legionclaim.RoleImplementer, supervise.StateWorking, before(50*time.Minute), time.Time{})
	claimOf("ACME-4", legionclaim.RoleImplementer, supervise.StateIdle, before(40*time.Minute), before(20*time.Minute))
	seed("the line", func(tx pgx.Tx) error {
		// Each released root took a slot and gave it back for its wait, which is the one write of
		// the mark (record.Store.ReleaseSlotForWait).
		for _, released := range []struct {
			key, rank, blocker, status string
			at                         time.Time
		}{
			{key: "ACME-7", rank: "f", blocker: "ACME-8", status: "in_progress", at: before(150 * time.Minute)},
			{key: "ACME-6", rank: "b", blocker: "ACME-9", status: "done", at: before(100 * time.Minute)},
		} {
			if err := running(tx, root(released.key, "in_progress", phase.Implementing, released.rank), true, true); err != nil {
				return err
			}
			if err := records.ReleaseSlotForWait(ctx, tx, released.key, released.at); err != nil {
				return err
			}
			slotIndex--
			if err := mirror(tx, released.key, "in_progress", released.blocker); err != nil {
				return err
			}
			if err := mirror(tx, released.blocker, released.status); err != nil {
				return err
			}
		}
		return records.PutIssue(ctx, tx, root("ACME-5", "todo", phase.Admitted, "g"))
	})
	generation, err := st.MintController(ctx, projectToken, []byte("capability"))
	if err != nil {
		t.Fatalf("mint the controller capability: %v", err)
	}
	if ok, err := st.RegisterController(ctx, projectToken, generation, "ses-controller", []byte("secret"), now, 0); err != nil || !ok {
		t.Fatalf("register the controller = %t, %v", ok, err)
	}

	// The tick, through the daemon's own chain.
	engine := workflow.New(record.NewStore(), workflow.Config{Project: slotsProject, Steps: steps, Clock: func() time.Time { return now }}, quiet)
	admission := admit.New(record.NewStore(), engine, slotCap, slotsProject, quiet)
	admission.UseClock(func() time.Time { return now })
	if _, err := intake.ApplyFact(ctx, st.Pool(), "controller", "tick-slots", intake.ControllerTick{},
		admit.Handlers(record.NewStore(), slotsProject, engine, admission)...); err != nil {
		t.Fatalf("apply the tick: %v", err)
	}
	var row record.OutboxRow
	if err := st.Pool().QueryRow(ctx, `select id, kind, issue, payload from outbox where kind = $1 and issue = $2 and payload->>'kind' = $3`,
		string(record.OutboxKindControllerNotice), slotsProject, string(record.TickNotice)).Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
		t.Fatalf("read the tick notice: %v", err)
	}
	decoded, err := record.DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("the tick notice does not pass the validator: %v", err)
	}
	notice, ok := decoded.(record.ControllerNotice)
	if !ok {
		t.Fatalf("the tick notice decoded to %T", decoded)
	}

	// The state, through the real route, as `legion state` and `legion state --json` read it.
	server := httptest.NewServer(api.NewServer("127.0.0.1", 0, api.Options{
		State: slotsStateSource{store: st, steps: steps, cap: slotCap, now: now}, StateTransactions: st,
		Project: projectToken, DispatchProject: slotsProject, OperatorToken: "operator-bearer-for-slots-tests", Log: quiet,
	}).Handler)
	t.Cleanup(server.Close)
	port := strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port)
	legionState := func(args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		if code := run(ctx, append(append([]string{"legion", "state"}, args...), "--port", port), &out, &errb); code != 0 || errb.Len() != 0 {
			t.Fatalf("legion state %v = %d, stderr %q", args, code, errb.String())
		}
		return out.String()
	}
	text := legionState()
	var served struct {
		Admission struct {
			Slots *record.SlotsReading `json:"slots"`
		} `json:"admission"`
	}
	if err := json.Unmarshal([]byte(legionState("--json")), &served); err != nil {
		t.Fatalf("read legion state --json: %v", err)
	}

	// Each surface carries the reading.
	jsonReading, tickReading := served.Admission.Slots, notice.Slots
	lines := stateLines(text, "slots: ")
	if jsonReading == nil {
		t.Errorf("legion state --json carries no admission.slots")
	}
	if tickReading == nil {
		t.Errorf("the tick notice carries no slots: %s", row.Payload)
	}
	if len(lines) != 1 {
		t.Errorf("legion state printed %d slots lines, want one:\n%s", len(lines), text)
	}
	if t.Failed() {
		t.FailNow()
	}

	// The JSON gives the numbers the records give.
	want := record.SlotsReading{
		InUse: 4, Cap: slotCap,
		Working:            record.StateCount{Count: 1, Oldest: new(before(50 * time.Minute))},
		Stopped:            record.StateCount{Count: 1},
		Person:             record.StateCount{Count: 1, Oldest: new(before(2 * time.Hour))},
		Idle:               record.StateCount{Count: 1, Oldest: new(before(20 * time.Minute))},
		WaitOnlyForSlot:    record.StateCount{Count: 1, Oldest: new(before(100 * time.Minute))},
		StartedWithoutSlot: record.StateCount{Count: 2, Oldest: new(before(150 * time.Minute))},
		BelowFirstWaiting:  record.BelowFirstWaiting{Count: 3, First: "ACME-6"},
	}
	if got, wanted := slotsReadingJSON(t, *jsonReading, false), slotsReadingJSON(t, want, false); got != wanted {
		t.Errorf("legion state --json's admission.slots = %s, want %s", got, wanted)
	}
	// The tick notice gives the same reading, its line included.
	if got, stated := slotsReadingJSON(t, *tickReading, true), slotsReadingJSON(t, *jsonReading, true); got != stated {
		t.Errorf("the tick notice's slots = %s and legion state --json's admission.slots = %s: one reading of one state", got, stated)
	}

	// The text is the JSON's line, printed as the first line after the admission count line, and
	// every number and oldest time in it is the JSON's.
	line := lines[0]
	if line != jsonReading.Line {
		t.Errorf("legion state printed the slots line %q and legion state --json's line is %q", line, jsonReading.Line)
	}
	printed := strings.Split(text, "\n")
	for i, each := range printed {
		if strings.HasPrefix(each, "admission: ") && (i+1 >= len(printed) || printed[i+1] != line) {
			t.Errorf("legion state printed the slots line elsewhere than right after its admission count line:\n%s", text)
		}
	}
	parts := slotsLinePattern.FindStringSubmatch(line)
	if parts == nil {
		t.Fatalf("legion state's slots line %q is not the line the spec writes", line)
	}
	number := func(group int) int {
		n, err := strconv.Atoi(parts[group])
		if err != nil && parts[group] != "" {
			t.Fatalf("the slots line's number %q: %v", parts[group], err)
		}
		return n
	}
	r := jsonReading
	for _, check := range []struct {
		what        string
		count, n    int
		oldest      *time.Time
		printedTime string
		timed       bool
	}{
		{what: "in use", count: r.InUse, n: number(1)},
		{what: "the cap", count: r.Cap, n: number(2)},
		{what: "working", count: r.Working.Count, n: number(3), oldest: r.Working.Oldest, printedTime: parts[4], timed: true},
		{what: "stopped", count: r.Stopped.Count, n: number(5), oldest: r.Stopped.Oldest, printedTime: parts[6], timed: true},
		{what: "person", count: r.Person.Count, n: number(7)},
		{what: "issue", count: r.Issue.Count, n: number(8)},
		{what: "idle", count: r.Idle.Count, n: number(9), oldest: r.Idle.Oldest, printedTime: parts[10], timed: true},
		{what: "wait only for a slot", count: r.WaitOnlyForSlot.Count, n: number(11), oldest: r.WaitOnlyForSlot.Oldest, printedTime: parts[12], timed: true},
		{what: "started without a slot in all", count: r.StartedWithoutSlot.Count, n: number(13), oldest: r.StartedWithoutSlot.Oldest,
			printedTime: parts[14], timed: true},
		{what: "holders below the first waiting issue", count: r.BelowFirstWaiting.Count, n: number(15)},
	} {
		if check.n != check.count {
			t.Errorf("the slots line gives %s as %d and the JSON as %d: %q", check.what, check.n, check.count, line)
		}
		if check.timed && check.printedTime != slotsLineMinute(check.oldest) {
			t.Errorf("the slots line gives the oldest of %s as %q and the JSON as %q: %q", check.what, check.printedTime,
				slotsLineMinute(check.oldest), line)
		}
	}
	if parts[16] != r.BelowFirstWaiting.First {
		t.Errorf("the slots line names %q as the first issue that waits for a slot and the JSON %q: %q", parts[16], r.BelowFirstWaiting.First, line)
	}
}
