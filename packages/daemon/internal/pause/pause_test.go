package pause

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// pausedAt is where the tests' clock starts, and when their first pause begins.
var pausedAt = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

// The two claims seedClaims writes that a pause asks to stop, in token order.
const (
	implementer claim.Token = "legion-legion-legion-208-implementer"
	tester      claim.Token = "legion-legion-legion-209-tester"
)

var (
	controller = store.PauseActor{Kind: store.PauseByController, Session: "ses-controller"}
	operator   = store.PauseActor{Kind: store.PauseByOperator}
)

// testClock is the daemon's clock, moved by the test alone.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// pausedCall is one call of Effects.Paused, with the flag and the log as the call found them.
type pausedCall struct {
	pause  store.Pause
	asked  []store.PauseStopClaim
	flag   int64
	logged bool
}

// resumedCall is one call of Effects.Resumed, with the flag and the log as the call found them.
type resumedCall struct {
	ended   store.Pause
	flagged bool
	logged  bool
}

// fakeEffects stands in for the daemon's after-commit steps and records each call. While hold is
// set, Paused marks entered and waits for hold to close.
type fakeEffects struct {
	svc *Service
	log *testlog.Buffer

	mu      sync.Mutex
	paused  []pausedCall
	resumed []resumedCall

	entered atomic.Bool
	hold    chan struct{}
}

func (f *fakeEffects) Paused(_ context.Context, pause store.Pause, asked []store.PauseStopClaim) {
	flag, _ := f.svc.Paused()
	f.mu.Lock()
	f.paused = append(f.paused, pausedCall{pause: pause, asked: asked, flag: flag,
		logged: strings.Contains(f.log.String(), "pause: the project is paused")})
	f.mu.Unlock()
	if f.hold != nil {
		f.entered.Store(true)
		<-f.hold
	}
}

func (f *fakeEffects) Resumed(_ context.Context, ended store.Pause) {
	_, flagged := f.svc.Paused()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, resumedCall{ended: ended, flagged: flagged,
		logged: strings.Contains(f.log.String(), "pause: the project is resumed")})
}

func (f *fakeEffects) pausedCalls() []pausedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paused)
}

func (f *fakeEffects) resumedCalls() []resumedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.resumed)
}

// rig is one daemon's pause service over a database of its own, with a clock, a fake of the
// after-commit steps and a JSON log the test reads.
type rig struct {
	t       *testing.T
	st      *store.Store
	clock   *testClock
	effects *fakeEffects
	log     *testlog.Buffer
	svc     *Service
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return rigOn(t, testpg.CloneStore(t, "legion_pause"), &testClock{at: pausedAt}, capable())
}

// rigOn is a daemon's pause service over st, built with cfg.
func rigOn(t *testing.T, st *store.Store, clock *testClock, cfg config.Config) *rig {
	t.Helper()
	r := &rig{t: t, st: st, clock: clock, log: &testlog.Buffer{}}
	r.effects = &fakeEffects{log: r.log}
	svc, err := New(Options{Store: st, Records: record.NewStore(), Project: "LEGION", Config: cfg, Effects: r.effects,
		Log: slog.New(slog.NewJSONHandler(r.log, nil)), Now: clock.Now})
	if err != nil {
		t.Fatalf("build the pause service: %v", err)
	}
	r.svc, r.effects.svc = svc, svc
	return r
}

// capable is a configuration that can hold a pause: runtime: kubernetes with the idle release on
// and a wake pacing of 8 a minute, as every production project runs.
func capable() config.Config {
	return config.Config{Runtime: config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{IdleRelease: config.DefaultIdleRelease()}},
		WorkerWakesPerMinute: 8}
}

// seedClaims is project legion as a pause finds it, as the store's own test seeds it
// (store's seedPauseClaims): an idle implementer of LEGION-208 and a working tester of LEGION-209,
// which the pause stops; an architect an idle release left released, a failed reviewer and a
// retired merger, which run nothing to stop; the project's controller, which the pause never
// stops; and an idle implementer of another project on the same database.
func seedClaims(t *testing.T, st *store.Store) {
	t.Helper()
	for _, c := range []supervise.Claim{
		{Token: implementer, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
			Role: claim.RoleImplementer, Generation: 1, State: supervise.StateIdle},
		{Token: tester, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209",
			Role: claim.RoleTester, Generation: 2, State: supervise.StateWorking},
		{Token: "legion-legion-legion-208-architect", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
			Role: claim.RoleArchitect, Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased},
		{Token: "legion-legion-legion-210-reviewer", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-210",
			Role: claim.RoleReviewer, Generation: 1, State: supervise.StateFailed},
		{Token: "legion-legion-legion-210-merger", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-210",
			Role: claim.RoleMerger, Generation: 1, State: supervise.StateRetired},
		{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController, State: supervise.StateIdle},
		{Token: "legion-infra-infra-5-implementer", Project: "infra", Tree: "INFRA-5", Issue: "INFRA-5",
			Role: claim.RoleImplementer, Generation: 1, State: supervise.StateIdle},
	} {
		if err := st.PutClaim(context.Background(), c); err != nil {
			t.Fatalf("seed claim %s: %v", c.Token, err)
		}
	}
}

// registerController registers session ses-controller as project legion's controller, as
// `legion controller start` and the controller's registration record it.
func registerController(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	generation, err := st.MintController(ctx, "legion", []byte("capability"))
	if err != nil {
		t.Fatalf("mint the controller's capability: %v", err)
	}
	if registered, err := st.RegisterController(ctx, "legion", generation, controller.Session, []byte("secret"), pausedAt, 1); err != nil || !registered {
		t.Fatalf("register the controller = %t, %v", registered, err)
	}
}

// seedHeld queues, due at the pause, two rows the pause holds: a start of LEGION-208's
// implementer, which names its claim, and a notice for LEGION-208's architect, which names none
// (record.PauseHeldRow). It answers their ids in that order.
func seedHeld(t *testing.T, st *store.Store) (start, notice int64) {
	t.Helper()
	records := record.NewStore()
	startRow, err := record.NewOutboxRow("LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208",
		Role: claim.RoleImplementer, Generation: 1}, pausedAt)
	if err != nil {
		t.Fatal(err)
	}
	noticeRow, err := record.NewOutboxRow("LEGION-208", record.Notice{Kind: "child-closed", Role: claim.RoleArchitect,
		Reason: "LEGION-210 is done"}, pausedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(context.Background(), st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if start, err = records.EnqueueID(context.Background(), tx, startRow); err != nil {
			return err
		}
		notice, err = records.EnqueueID(context.Background(), tx, noticeRow)
		return err
	}); err != nil {
		t.Fatalf("queue the held rows: %v", err)
	}
	return start, notice
}

// count is the one number sql counts.
func count(t *testing.T, st *store.Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// actors is who paused pause and who ended it, as project_pauses holds them.
func actors(t *testing.T, st *store.Store, pause int64) (by, ended store.PauseActor) {
	t.Helper()
	var byKind, endedKind string
	if err := st.Pool().QueryRow(context.Background(), `select by_kind, by_session, coalesce(ended_by_kind, ''),
		coalesce(ended_by_session, '') from project_pauses where id = $1`, pause).Scan(&byKind, &by.Session, &endedKind, &ended.Session); err != nil {
		t.Fatalf("read pause %d: %v", pause, err)
	}
	by.Kind, ended.Kind = store.PauseActorKind(byKind), store.PauseActorKind(endedKind)
	return by, ended
}

// controllerNotices is every controller notice in the outbox, in the order it was queued, with
// the issue each is queued on.
func controllerNotices(t *testing.T, st *store.Store) ([]string, []record.ControllerNotice) {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `select issue, payload from outbox where kind = $1 order by id`,
		string(record.OutboxKindControllerNotice))
	if err != nil {
		t.Fatalf("read the controller notices: %v", err)
	}
	defer rows.Close()
	var issues []string
	var notices []record.ControllerNotice
	for rows.Next() {
		row := record.OutboxRow{Kind: record.OutboxKindControllerNotice}
		if err := rows.Scan(&row.Issue, &row.Payload); err != nil {
			t.Fatal(err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode the controller notice on %s: %v", row.Issue, err)
		}
		issues, notices = append(issues, row.Issue), append(notices, payload.(record.ControllerNotice))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return issues, notices
}

// lines is every JSON line of buf whose message is msg.
func lines(t *testing.T, buf *testlog.Buffer, msg string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, text := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if text == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatalf("log line %q is no JSON: %v", text, err)
		}
		if line["msg"] == msg {
			found = append(found, line)
		}
	}
	return found
}

func tokens(stops []store.PauseStopClaim) []claim.Token {
	out := make([]claim.Token, len(stops))
	for i, stop := range stops {
		out[i] = stop.Claim
	}
	return out
}

// The controller pauses and resumes the project. The pause's transaction writes one valid
// pause_stop row for each live role but the controller's; the flag holds the pause from its commit
// until the resume's; the daemon's after-commit steps get the pause with the claims it asked to
// stop, after the flag is set and after the pause's line, which its commit writes, and then the
// ended pause, after the flag is cleared and after the resume's line; and the store keeps the
// controller's session as who paused and who resumed. The controller is a registered one, and no
// notice tells it of its own acts.
func TestTheControllersPauseAndResumeSetAndClearTheFlag(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	seedClaims(t, r.st)
	registerController(t, r.st)

	if id, paused := r.svc.Paused(); paused || id != 0 {
		t.Fatalf("Paused before any pause = %d, %t; want none", id, paused)
	}
	const reason = "the model provider refuses every call"
	pause, err := r.svc.Pause(ctx, controller, reason)
	if err != nil {
		t.Fatalf("pause as the controller: %v", err)
	}
	if pause.ID == 0 || pause.Project != "LEGION" || pause.By != controller || pause.Reason != reason || !pause.Since.Equal(pausedAt) || pause.Ended() {
		t.Fatalf("pause = %+v, want an open pause of LEGION by the controller's session at %s", pause, pausedAt)
	}
	if id, paused := r.svc.Paused(); !paused || id != pause.ID {
		t.Fatalf("Paused after the pause = %d, %t; want pause %d", id, paused, pause.ID)
	}
	if by, _ := actors(t, r.st, pause.ID); by != controller {
		t.Fatalf("project_pauses says pause %d was made by %+v, want the controller's session", pause.ID, by)
	}

	rows, err := r.st.Pool().Query(ctx, `select issue, payload from outbox where kind = 'supervise' order by id`)
	if err != nil {
		t.Fatalf("read the stop rows: %v", err)
	}
	var stops []record.OutboxRow
	for rows.Next() {
		row := record.OutboxRow{Kind: record.OutboxKindSupervise}
		if err := rows.Scan(&row.Issue, &row.Payload); err != nil {
			t.Fatal(err)
		}
		stops = append(stops, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stop rows: %v", err)
	}
	if len(stops) != 2 {
		t.Fatalf("supervise rows = %d, want one pause_stop row for each of the two live roles", len(stops))
	}
	for i, want := range []struct {
		issue      string
		role       claim.Role
		generation uint64
	}{{"LEGION-208", claim.RoleImplementer, 1}, {"LEGION-209", claim.RoleTester, 2}} {
		payload, err := record.DecodeOutboxPayload(stops[i])
		if err != nil {
			t.Fatalf("decode the stop row of %s: %v", stops[i].Issue, err)
		}
		request := payload.(record.SuperviseRequest)
		if stops[i].Issue != want.issue || request.Op != "pause_stop" || request.Pause != pause.ID || request.Role != want.role ||
			request.Tree != "LEGION-208" || request.Generation != want.generation || !strings.Contains(request.Reason, fmt.Sprintf("(pause %d): %s", pause.ID, reason)) {
			t.Fatalf("stop row %d = %s %+v, want the pause_stop of %s's %s at generation %d naming pause %d", i, stops[i].Issue, request,
				want.issue, want.role, want.generation, pause.ID)
		}
	}

	calls := r.effects.pausedCalls()
	if len(calls) != 1 {
		t.Fatalf("Effects.Paused ran %d times, want once", len(calls))
	}
	asked := tokens(calls[0].asked)
	if calls[0].pause.ID != pause.ID || !slices.Equal(asked, []claim.Token{implementer, tester}) {
		t.Fatalf("Effects.Paused got pause %d asking %v, want pause %d asking the implementer and the tester", calls[0].pause.ID, asked, pause.ID)
	}
	if calls[0].flag != pause.ID || !calls[0].logged {
		t.Fatalf("Effects.Paused found the flag at %d and the pause logged %t, want the flag set and the pause logged at its commit", calls[0].flag, calls[0].logged)
	}

	r.clock.Advance(30 * time.Minute)
	ended, _, err := r.svc.Resume(ctx, controller)
	if err != nil {
		t.Fatalf("resume as the controller: %v", err)
	}
	if ended.ID != pause.ID || ended.EndedBy != controller || !ended.EndedAt.Equal(pausedAt.Add(30*time.Minute)) {
		t.Fatalf("ended pause = %+v, want pause %d ended by the controller's session 30 minutes on", ended, pause.ID)
	}
	if id, paused := r.svc.Paused(); paused || id != 0 {
		t.Fatalf("Paused after the resume = %d, %t; want none", id, paused)
	}
	if _, endedBy := actors(t, r.st, pause.ID); endedBy != controller {
		t.Fatalf("project_pauses says pause %d was ended by %+v, want the controller's session", pause.ID, endedBy)
	}
	resumes := r.effects.resumedCalls()
	if len(resumes) != 1 || resumes[0].ended.ID != pause.ID || resumes[0].flagged || !resumes[0].logged {
		t.Fatalf("Effects.Resumed calls = %+v, want one for pause %d with the flag cleared and the resume logged at its commit", resumes, pause.ID)
	}
	if _, notices := controllerNotices(t, r.st); len(notices) != 0 {
		t.Fatalf("controller notices = %+v, want none: the controller's own acts tell it nothing", notices)
	}
}

// The operator's pause and resume each queue one notice to the controller naming who, when and
// why, and only while a controller is registered: the rule of every wake of the controller
// (admit.Admission.wakeController). The store keeps the operator, with no session, as who paused
// and who resumed.
func TestTheOperatorsActsTellTheControllerOnlyWhileOneIsRegistered(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	seedClaims(t, r.st)

	first, err := r.svc.Pause(ctx, operator, "an outage, with no controller running")
	if err != nil {
		t.Fatalf("pause as the operator with no controller registered: %v", err)
	}
	r.clock.Advance(time.Minute)
	if _, _, err := r.svc.Resume(ctx, operator); err != nil {
		t.Fatalf("resume as the operator with no controller registered: %v", err)
	}
	if _, notices := controllerNotices(t, r.st); len(notices) != 0 {
		t.Fatalf("controller notices with no controller registered = %+v, want none", notices)
	}
	if by, ended := actors(t, r.st, first.ID); by != operator || ended != operator {
		t.Fatalf("project_pauses says pause %d was made by %+v and ended by %+v, want the operator both times", first.ID, by, ended)
	}

	registerController(t, r.st)
	r.clock.Advance(time.Minute)
	const reason = "the model provider is down"
	pause, err := r.svc.Pause(ctx, operator, reason)
	if err != nil {
		t.Fatalf("pause as the operator: %v", err)
	}
	r.clock.Advance(20 * time.Minute)
	ended, _, err := r.svc.Resume(ctx, operator)
	if err != nil {
		t.Fatalf("resume as the operator: %v", err)
	}
	issues, notices := controllerNotices(t, r.st)
	if len(notices) != 2 {
		t.Fatalf("controller notices = %+v, want the pause's and the resume's", notices)
	}
	paused, resumed := notices[0], notices[1]
	if issues[0] != "LEGION" || paused.Kind != record.ProjectPausedNotice || paused.Pause == nil || paused.Pause.ID != pause.ID ||
		paused.Pause.By != (record.PauseActor{Kind: record.PauseByOperator}) || !paused.Pause.At.Equal(pause.Since) || paused.Pause.Reason != reason {
		t.Fatalf("the pause's notice on %s = %+v %+v, want project-paused on LEGION naming pause %d, the operator, %s and %q",
			issues[0], paused, paused.Pause, pause.ID, pause.Since, reason)
	}
	if issues[1] != "LEGION" || resumed.Kind != record.ProjectResumedNotice || resumed.Pause == nil || resumed.Pause.ID != pause.ID ||
		resumed.Pause.By != (record.PauseActor{Kind: record.PauseByOperator}) || !resumed.Pause.At.Equal(ended.EndedAt) || resumed.Pause.Reason != "" {
		t.Fatalf("the resume's notice on %s = %+v %+v, want project-resumed on LEGION naming pause %d, the operator and %s",
			issues[1], resumed, resumed.Pause, pause.ID, ended.EndedAt)
	}
}

// A second pause is refused with the store's own *store.AlreadyPausedError naming the pause that
// stands, and changes nothing: no pause row, no stop, no after-commit step, no log line, and the
// flag still holds the first pause.
func TestASecondPauseIsRefusedWithThePauseThatStands(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	seedClaims(t, r.st)
	first, err := r.svc.Pause(ctx, operator, "the model provider is down")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}

	r.clock.Advance(time.Minute)
	_, err = r.svc.Pause(ctx, controller, "again")
	var standing *store.AlreadyPausedError
	if !errors.As(err, &standing) || standing.Standing.ID != first.ID || standing.Standing.Reason != first.Reason || standing.Standing.By != operator {
		t.Fatalf("a second pause = %v, want an AlreadyPausedError naming pause %d", err, first.ID)
	}
	t.Logf("a second pause is refused: %v", err)
	if got := count(t, r.st, `select count(*) from project_pauses`); got != 1 {
		t.Fatalf("pauses = %d after the refusal, want the one that stands", got)
	}
	if got := count(t, r.st, `select count(*) from project_pause_stops`); got != 2 {
		t.Fatalf("stops = %d after the refusal, want the first pause's two", got)
	}
	if calls := r.effects.pausedCalls(); len(calls) != 1 {
		t.Fatalf("Effects.Paused ran %d times, want once, for the first pause", len(calls))
	}
	if got := lines(t, r.log, "pause: the project is paused"); len(got) != 1 {
		t.Fatalf("pause lines = %d, want the first pause's alone", len(got))
	}
	if id, paused := r.svc.Paused(); !paused || id != first.ID {
		t.Fatalf("Paused after the refusal = %d, %t; want pause %d", id, paused, first.ID)
	}
}

// A live claim of another project's issue — one the operator's spawn makes under this project's
// token — is a session this daemon runs, so the pause stops it as it stops every other live role:
// Effects.Paused is handed it with the rest, its stop row names the pause's own project, and its
// `pause_stop` row (StopRow) is keyed by the project, whose outbox runs it, naming the claim's
// issue in its payload. A row of the project's own issue is keyed by that issue and names none.
func TestAPauseStopsALiveClaimOfAnotherProjectsIssueUnderTheProjectsKey(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	seedClaims(t, r.st)
	const foreign claim.Token = "legion-legion-infra-5-implementer"
	if err := r.st.PutClaim(ctx, supervise.Claim{Token: foreign, Project: "legion", Tree: "INFRA-5", Issue: "INFRA-5",
		Role: claim.RoleImplementer, Generation: 1, State: supervise.StateIdle}); err != nil {
		t.Fatalf("seed the claim of INFRA-5: %v", err)
	}

	pause, err := r.svc.Pause(ctx, operator, "the model provider is down")
	if err != nil {
		t.Fatalf("pause with a live claim of another project's issue: %v", err)
	}
	calls := r.effects.pausedCalls()
	if want := []claim.Token{foreign, implementer, tester}; len(calls) != 1 || !slices.Equal(tokens(calls[0].asked), want) {
		t.Fatalf("Effects.Paused calls = %+v, want one asking %v", calls, want)
	}
	var project string
	if err := r.st.Pool().QueryRow(ctx, `select project from project_pause_stops where pause_id = $1 and claim = $2`,
		pause.ID, string(foreign)).Scan(&project); err != nil || project != "LEGION" {
		t.Fatalf("the stop of %s names project %q, %v; want the pause's own, LEGION", foreign, project, err)
	}
	if got := count(t, r.st, `select count(*) from outbox where issue = 'INFRA-5'`); got != 0 {
		t.Fatalf("outbox rows keyed by INFRA-5 = %d, want none: the outbox runs only LEGION's keys", got)
	}
	row := record.OutboxRow{Kind: record.OutboxKindSupervise}
	if err := r.st.Pool().QueryRow(ctx, `select issue, payload from outbox where kind = 'supervise' and issue = 'LEGION'`).
		Scan(&row.Issue, &row.Payload); err != nil {
		t.Fatalf("read the stop row keyed by LEGION: %v", err)
	}
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("decode the stop row of %s: %v", foreign, err)
	}
	if request := payload.(record.SuperviseRequest); request.Op != "pause_stop" || request.Issue != "INFRA-5" || request.Tree != "INFRA-5" ||
		request.Role != claim.RoleImplementer || request.Pause != pause.ID {
		t.Fatalf("the stop row keyed by LEGION = %+v, want pause %d's stop of INFRA-5's implementer", request, pause.ID)
	}
	for issue, key := range map[string]string{"LEGION-208": "LEGION-208", "INFRA-5": "LEGION"} {
		if got := StopKey(pause, issue); got != key {
			t.Fatalf("StopKey of %s = %q, want %q", issue, got, key)
		}
	}
	if got := count(t, r.st, `select count(*) from outbox where kind = 'supervise' and issue = 'LEGION-208' and not payload ? 'issue'`); got != 1 {
		t.Fatalf("stop rows of LEGION-208 keyed by their issue and naming none = %d, want the implementer's", got)
	}
}

// A resume of a project that runs is refused with the store's own *store.NotPausedError, before
// any pause and after one has ended, and runs no after-commit step and logs nothing.
func TestAResumeOfARunningProjectIsRefused(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	seedClaims(t, r.st)

	var notPaused *store.NotPausedError
	if _, _, err := r.svc.Resume(ctx, operator); !errors.As(err, &notPaused) || notPaused.Project != "LEGION" {
		t.Fatalf("resuming a project never paused = %v, want a NotPausedError naming LEGION", err)
	}
	if _, err := r.svc.Pause(ctx, operator, "the model provider is down"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, _, err := r.svc.Resume(ctx, controller); err != nil {
		t.Fatalf("resume: %v", err)
	}
	_, _, err := r.svc.Resume(ctx, controller)
	if !errors.As(err, &notPaused) {
		t.Fatalf("a second resume = %v, want a NotPausedError", err)
	}
	t.Logf("a resume of a running project is refused: %v", err)
	if calls := r.effects.resumedCalls(); len(calls) != 1 {
		t.Fatalf("Effects.Resumed ran %d times, want once, for the one resume that ended a pause", len(calls))
	}
	if got := lines(t, r.log, "pause: the project is resumed"); len(got) != 1 {
		t.Fatalf("resume lines = %d, want the one resume's alone", len(got))
	}
	if _, paused := r.svc.Paused(); paused {
		t.Fatal("Paused after the refusals = true, want none")
	}
}

// A pause with an empty or blank reason is refused with ErrReasonRequired and writes nothing; the
// same service then pauses with a reason.
func TestAPauseWithNoReasonIsRefused(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	seedClaims(t, r.st)
	registerController(t, r.st)

	for _, reason := range []string{"", " ", "\t\n "} {
		if _, err := r.svc.Pause(ctx, operator, reason); !errors.Is(err, ErrReasonRequired) {
			t.Fatalf("a pause with the reason %q = %v, want ErrReasonRequired", reason, err)
		}
	}
	if got := count(t, r.st, `select count(*) from project_pauses`); got != 0 {
		t.Fatalf("pauses = %d after the refusals, want none", got)
	}
	if got := count(t, r.st, `select count(*) from outbox`); got != 0 {
		t.Fatalf("outbox rows = %d after the refusals, want none", got)
	}
	if calls := r.effects.pausedCalls(); len(calls) != 0 {
		t.Fatalf("Effects.Paused ran %d times after the refusals, want never", len(calls))
	}
	if _, paused := r.svc.Paused(); paused {
		t.Fatal("Paused after the refusals = true, want none")
	}
	if _, err := r.svc.Pause(ctx, operator, "the model provider is down"); err != nil {
		t.Fatalf("a pause with a reason: %v", err)
	}
	if got := count(t, r.st, `select count(*) from project_pauses`); got != 1 {
		t.Fatalf("pauses = %d after a pause with a reason, want 1", got)
	}
}

// A daemon under runtime: tmux, with the idle release off or with the wake pacing off refuses a
// pause with an *UnavailableError naming the setting as legion.yaml spells it, and writes nothing.
// Such a daemon still ends a pause another daemon of the project made.
func TestADaemonThatCannotHoldAPauseRefusesItNamingTheSetting(t *testing.T) {
	ctx := context.Background()
	st := testpg.CloneStore(t, "legion_pause")
	clock := &testClock{at: pausedAt}
	seedClaims(t, st)

	idleOff := capable()
	idleOff.Runtime.Kubernetes = &config.Kubernetes{IdleRelease: config.IdleReleaseConfig{IdleMinutes: 0, BackstopMinutes: 60}}
	pacingOff := capable()
	pacingOff.WorkerWakesPerMinute = 0
	var refusing []*rig
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want Setting
		yaml string
	}{
		{"tmux", config.Config{Runtime: config.Runtime{Name: "tmux"}, WorkerWakesPerMinute: 8}, SettingTmux, "runtime: tmux"},
		{"the idle release off", idleOff, SettingIdleReleaseOff, "runtime.kubernetes.idle_release.idle_minutes: 0"},
		{"the wake pacing off", pacingOff, SettingWakePacingOff, "worker_wakes_per_minute: 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if setting, unavailable := Unavailable(tc.cfg); !unavailable || setting != tc.want || string(setting) != tc.yaml {
				t.Fatalf("Unavailable = %q, %t; want %q", setting, unavailable, tc.yaml)
			}
			r := rigOn(t, st, clock, tc.cfg)
			_, err := r.svc.Pause(ctx, operator, "the model provider is down")
			var refused *UnavailableError
			if !errors.As(err, &refused) || refused.Setting != tc.want || !strings.Contains(err.Error(), tc.yaml) {
				t.Fatalf("a pause under %s = %v, want an UnavailableError naming %q", tc.name, err, tc.yaml)
			}
			t.Logf("refused: %v", err)
			if calls := r.effects.pausedCalls(); len(calls) != 0 {
				t.Fatalf("Effects.Paused ran %d times, want never", len(calls))
			}
			refusing = append(refusing, r)
		})
	}
	if got := count(t, st, `select count(*) from project_pauses`); got != 0 {
		t.Fatalf("pauses = %d after the refusals, want none", got)
	}
	if setting, unavailable := Unavailable(capable()); unavailable {
		t.Fatalf("Unavailable of a capable daemon = %q, want none", setting)
	}

	capableDaemon := rigOn(t, st, clock, capable())
	pause, err := capableDaemon.svc.Pause(ctx, operator, "the model provider is down")
	if err != nil {
		t.Fatalf("a capable daemon's pause: %v", err)
	}
	if len(refusing) != 3 {
		t.Fatalf("refusing daemons = %d, want 3", len(refusing))
	}
	ended, _, err := refusing[0].svc.Resume(ctx, operator)
	if err != nil || ended.ID != pause.ID {
		t.Fatalf("the tmux daemon's resume of pause %d = %+v, %v; want it ended", pause.ID, ended, err)
	}
}

// Load at boot and Refresh at each sweep set the flag from the store, so a pause made or ended by
// another daemon on the same database (a rolling restart) reaches this daemon's flag; until this
// daemon reads it, its flag holds what it last read. A refusal reads the store too: a pause
// refused because one stands sets the flag to it, and a resume refused because none is open clears
// it.
func TestLoadAndRefreshFollowAPauseAnotherDaemonMade(t *testing.T) {
	ctx := context.Background()
	url := testpg.Clone(t, "legion_pause")
	open := func() *store.Store {
		st, err := store.Open(ctx, url)
		if err != nil {
			t.Fatalf("open the database: %v", err)
		}
		t.Cleanup(st.Close)
		return st
	}
	clock := &testClock{at: pausedAt}
	first, second := rigOn(t, open(), clock, capable()), rigOn(t, open(), clock, capable())
	seedClaims(t, first.st)
	flag := func(r *rig) int64 {
		id, _ := r.svc.Paused()
		return id
	}

	if _, found, err := second.svc.Load(ctx); err != nil || found || flag(second) != 0 {
		t.Fatalf("Load of a running project = %t, %v, flag %d; want none", found, err, flag(second))
	}
	pause, err := first.svc.Pause(ctx, operator, "the model provider is down")
	if err != nil {
		t.Fatalf("the first daemon's pause: %v", err)
	}
	if flag(second) != 0 {
		t.Fatalf("the second daemon's flag = %d before it read the store, want none yet", flag(second))
	}
	if err := second.svc.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if id, paused := second.svc.Paused(); !paused || id != pause.ID {
		t.Fatalf("the second daemon's flag after Refresh = %d, %t; want pause %d", id, paused, pause.ID)
	}
	booted := rigOn(t, open(), clock, capable())
	loaded, found, err := booted.svc.Load(ctx)
	if err != nil || !found || loaded.ID != pause.ID || loaded.Reason != pause.Reason || flag(booted) != pause.ID {
		t.Fatalf("Load at a boot during the pause = %+v, %t, %v, flag %d; want pause %d", loaded, found, err, flag(booted), pause.ID)
	}
	if read, found, err := booted.svc.OpenPause(ctx); err != nil || !found || read.ID != pause.ID {
		t.Fatalf("OpenPause = %+v, %t, %v; want pause %d", read, found, err, pause.ID)
	}

	clock.Advance(10 * time.Minute)
	if _, _, err := first.svc.Resume(ctx, operator); err != nil {
		t.Fatalf("the first daemon's resume: %v", err)
	}
	if flag(second) != pause.ID {
		t.Fatalf("the second daemon's flag = %d before it read the store, want pause %d still", flag(second), pause.ID)
	}
	if err := second.svc.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, paused := second.svc.Paused(); paused {
		t.Fatal("the second daemon's flag after Refresh = paused, want none")
	}
	if last, found, err := second.svc.LastEndedPause(ctx); err != nil || !found || last.ID != pause.ID || !last.Ended() {
		t.Fatalf("LastEndedPause = %+v, %t, %v; want pause %d ended", last, found, err, pause.ID)
	}

	clock.Advance(10 * time.Minute)
	again, err := first.svc.Pause(ctx, controller, "the provider is down again")
	if err != nil {
		t.Fatalf("the first daemon's second pause: %v", err)
	}
	var standing *store.AlreadyPausedError
	if _, err := second.svc.Pause(ctx, operator, "the operator's pause"); !errors.As(err, &standing) {
		t.Fatalf("the second daemon's pause = %v, want an AlreadyPausedError", err)
	}
	if flag(second) != again.ID {
		t.Fatalf("the second daemon's flag after its refused pause = %d, want the pause that stands, %d", flag(second), again.ID)
	}
	if _, _, err := first.svc.Resume(ctx, controller); err != nil {
		t.Fatalf("the first daemon's second resume: %v", err)
	}
	var notPaused *store.NotPausedError
	if _, _, err := second.svc.Resume(ctx, operator); !errors.As(err, &notPaused) {
		t.Fatalf("the second daemon's resume = %v, want a NotPausedError", err)
	}
	if flag(second) != 0 {
		t.Fatalf("the second daemon's flag after its refused resume = %d, want none", flag(second))
	}
}

// A pause and a resume another daemon made are no acts of this one. Its Refresh, and its own acts
// the store refuses for them, move its flag and run none of its Effects: the daemon that made each
// act runs its own, and this daemon's machines meet the flag at their next decision.
// Only an act of this daemon's own whose outcome the store could not say is owed its Effects
// (TestAPauseWhoseCommitReplyIsLostKeepsItsSuccessWork).
func TestARefreshOfAnotherDaemonsPauseAndResumeRunsNoEffects(t *testing.T) {
	ctx := context.Background()
	url := testpg.Clone(t, "legion_pause")
	open := func() *store.Store {
		st, err := store.Open(ctx, url)
		if err != nil {
			t.Fatalf("open the database: %v", err)
		}
		t.Cleanup(st.Close)
		return st
	}
	clock := &testClock{at: pausedAt}
	first, second := rigOn(t, open(), clock, capable()), rigOn(t, open(), clock, capable())
	seedClaims(t, first.st)
	none := func(when string) {
		t.Helper()
		if paused, resumed := second.effects.pausedCalls(), second.effects.resumedCalls(); len(paused) != 0 || len(resumed) != 0 {
			t.Fatalf("%s: the second daemon's Effects.Paused ran %d times and Effects.Resumed %d, want none", when, len(paused), len(resumed))
		}
	}

	pause, err := first.svc.Pause(ctx, operator, "the model provider is down")
	if err != nil {
		t.Fatalf("the first daemon's pause: %v", err)
	}
	if err := second.svc.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if id, paused := second.svc.Paused(); !paused || id != pause.ID {
		t.Fatalf("the second daemon's flag after Refresh = %d, %t; want pause %d", id, paused, pause.ID)
	}
	none("after a Refresh read the first daemon's pause")

	clock.Advance(10 * time.Minute)
	if _, _, err := first.svc.Resume(ctx, operator); err != nil {
		t.Fatalf("the first daemon's resume: %v", err)
	}
	if err := second.svc.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, paused := second.svc.Paused(); paused {
		t.Fatal("the second daemon's flag after Refresh = paused, want none")
	}
	none("after a Refresh read the first daemon's resume")

	clock.Advance(10 * time.Minute)
	again, err := first.svc.Pause(ctx, controller, "the provider is down again")
	if err != nil {
		t.Fatalf("the first daemon's second pause: %v", err)
	}
	var standing *store.AlreadyPausedError
	if _, err := second.svc.Pause(ctx, operator, "the operator's pause"); !errors.As(err, &standing) {
		t.Fatalf("the second daemon's pause = %v, want an AlreadyPausedError", err)
	}
	if id, _ := second.svc.Paused(); id != again.ID {
		t.Fatalf("the second daemon's flag after its refused pause = %d, want pause %d", id, again.ID)
	}
	none("after the second daemon's pause was refused")
	if _, _, err := first.svc.Resume(ctx, controller); err != nil {
		t.Fatalf("the first daemon's second resume: %v", err)
	}
	var notPaused *store.NotPausedError
	if _, _, err := second.svc.Resume(ctx, operator); !errors.As(err, &notPaused) {
		t.Fatalf("the second daemon's resume = %v, want a NotPausedError", err)
	}
	if _, paused := second.svc.Paused(); paused {
		t.Fatal("the second daemon's flag after its refused resume = paused, want none")
	}
	none("after the second daemon's resume was refused")
	if got := len(first.effects.pausedCalls()) + len(first.effects.resumedCalls()); got != 4 {
		t.Fatalf("the first daemon's Effects ran %d times, want 4: one for each of its own acts", got)
	}
}

// The pause logs one line naming the pause, who made it, why, and how many roles and which claims
// it asked to stop; the resume logs one line naming the pause, who ended it, who made it and how
// many rows it held, then one line for each row it held, naming the row, its kind, its issue and
// its claim, which is empty for a row that names none.
func TestThePauseAndTheResumeLogTheirLines(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	seedClaims(t, r.st)
	registerController(t, r.st)

	const reason = "the model provider is down"
	pause, err := r.svc.Pause(ctx, operator, reason)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	start, notice := seedHeld(t, r.st)
	r.clock.Advance(30 * time.Minute)
	_, held, err := r.svc.Resume(ctx, controller)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(held) != 2 || held[0].ID != start || held[1].ID != notice {
		t.Fatalf("held rows = %+v, want the start %d and the notice %d", held, start, notice)
	}

	paused := lines(t, r.log, "pause: the project is paused")
	if len(paused) != 1 {
		t.Fatalf("pause lines = %d, want 1:\n%s", len(paused), r.log.String())
	}
	line := paused[0]
	claims, _ := line["claims"].([]any)
	if line["level"] != "INFO" || line["pause"] != float64(pause.ID) || line["by"] != "operator" || line["by_session"] != "" ||
		line["reason"] != reason || line["stops"] != float64(2) ||
		!slices.Equal(claims, []any{string(implementer), string(tester)}) || line["told_controller"] != true {
		t.Fatalf("pause line = %v, want pause %d by the operator for %q, asking %s and %s to stop, with the controller told",
			line, pause.ID, reason, implementer, tester)
	}

	resumed := lines(t, r.log, "pause: the project is resumed")
	if len(resumed) != 1 {
		t.Fatalf("resume lines = %d, want 1:\n%s", len(resumed), r.log.String())
	}
	line = resumed[0]
	if line["level"] != "INFO" || line["pause"] != float64(pause.ID) || line["by"] != "controller" || line["by_session"] != controller.Session ||
		line["paused_by"] != "operator" || line["paused_by_session"] != "" || line["held"] != float64(2) || line["told_controller"] != false {
		t.Fatalf("resume line = %v, want pause %d ended by the controller's session, made by the operator, holding 2 rows", line, pause.ID)
	}

	released := lines(t, r.log, HeldRowReleasedMessage)
	if len(released) != 2 {
		t.Fatalf("released-row lines = %d, want one for each of the 2 held rows:\n%s", len(released), r.log.String())
	}
	for i, want := range []struct {
		row   int64
		kind  record.OutboxKind
		claim claim.Token
	}{{start, record.OutboxKindSupervise, implementer}, {notice, record.OutboxKindNotice, ""}} {
		line := released[i]
		if line["pause"] != float64(pause.ID) || line["row"] != float64(want.row) || line["kind"] != string(want.kind) ||
			line["issue"] != "LEGION-208" || line["claim"] != string(want.claim) {
			t.Fatalf("released-row line %d = %v, want row %d, a %s of LEGION-208, claim %q", i, line, want.row, want.kind, want.claim)
		}
	}
}

// Refresh waits for a pause under way, its after-commit steps included, so it never writes a
// reading taken before the pause's commit over the flag the pause set; the flag itself is read
// without waiting, and already holds the pause while those steps run.
func TestARefreshWaitsForAPauseUnderWay(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	seedClaims(t, r.st)
	r.effects.hold = make(chan struct{})
	// The pause's after-commit steps wait on hold, and so do the pause and the refresh behind
	// them: a test that fails before it lets them go still lets them go when it ends.
	release := sync.OnceFunc(func() { close(r.effects.hold) })
	t.Cleanup(release)

	var pause store.Pause
	var pauseErr, refreshErr error
	var paused, refreshed atomic.Bool
	go func() {
		pause, pauseErr = r.svc.Pause(ctx, operator, "the model provider is down")
		paused.Store(true)
	}()
	testwait.Eventually(t, "the pause's after-commit steps to start", r.effects.entered.Load)
	id, flagged := r.svc.Paused()
	if !flagged || id == 0 {
		t.Fatalf("Paused while the pause's after-commit steps run = %d, %t; want the pause", id, flagged)
	}
	go func() {
		refreshErr = r.svc.Refresh(ctx)
		refreshed.Store(true)
	}()
	if testwait.Holds(200*time.Millisecond, testwait.Poll, refreshed.Load) {
		t.Fatal("Refresh returned while the pause's after-commit steps still ran")
	}
	release()
	testwait.Eventually(t, "the pause and the refresh to return", func() bool { return paused.Load() && refreshed.Load() })
	if pauseErr != nil || refreshErr != nil {
		t.Fatalf("pause = %v, refresh = %v", pauseErr, refreshErr)
	}
	if got, ok := r.svc.Paused(); !ok || got != pause.ID || got != id {
		t.Fatalf("Paused after both = %d, %t; want pause %d", got, ok, pause.ID)
	}
}
