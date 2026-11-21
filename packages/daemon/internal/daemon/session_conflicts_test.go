package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/listenerwire"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// conflictRow is one claim an earlier boot left, for the session-conflict boot tests to seed.
type conflictRow struct {
	token                claim.Token
	issue, session, file string
	state                supervise.ClaimState
}

// seedConflicts migrates the database at dsn and records rows as an earlier boot left them, then
// holds holds as the claim_sessions table would (`insert ... values` rows of project, kind, name,
// claim).
func seedConflicts(t *testing.T, dsn, project string, rows []conflictRow, holds string, args ...any) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		c := earlierBootClaim(project, row.token, row.issue, row.session, row.state, i+1)
		c.SessionFile = row.file
		if row.state == supervise.StateQueued {
			c.Locator = nil
		}
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatalf("record %s: %v", row.token, err)
		}
	}
	if holds != "" {
		if _, err := st.Pool().Exec(ctx, `insert into claim_sessions (project, kind, name, claim) values `+holds, args...); err != nil {
			t.Fatalf("record the holds: %v", err)
		}
	}
}

// heldFor reads the claim the table holds a name of project for, and whether a row holds it.
func heldFor(t *testing.T, dsn, project string, kind supervise.HoldKind, name string) (claim.Token, bool) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var holder string
	err = st.Pool().QueryRow(ctx, `select claim from claim_sessions where project = $1 and kind = $2 and name = $3`,
		project, string(kind), name).Scan(&holder)
	if err != nil {
		return "", false
	}
	return claim.Token(holder), true
}

// projectTokens is the daemon's project token and a minter of its claims' tokens.
func projectTokens(t *testing.T, project string) func(issue string, role claim.Role) claim.Token {
	return func(issue string, role claim.Role) claim.Token {
		t.Helper()
		tok, err := claim.NewToken(project, issue, role)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
}

// A session name two claims recorded before the daemon held sessions is held for neither (the
// claim_sessions migration), so each is refused its registration, its resume and its enrollment
// under it; relaunched, either would be refused again and again. So the boot suspends each through
// the operator's suspend, once whatever number of names it shares, before anything relaunches
// them, and names each name, its claims and the statement that settles it at Error; a claim of the
// same boot holding its own session is left as it was, and no launch is made for any claim of the
// names. A claim in a turn holds the suspension for that turn's end (it stops the process at the
// operator's stop timeout), which is no failure of the boot. A queued claim recording one is in no
// state a suspension reaches: it is left queued, and its launch is refused before anything starts,
// so it ends failed within its launch budget. A name is an agent's own text, so the statement
// carries it as an escape string literal, its backslashes and quotes doubled.
//
// The control: drop the boot step (start's call of suspendSessionConflicts); the claims are
// re-adopted live, each a relaunch away from the refusal, and the test fails naming them (the pull
// request records the run).
func TestAClaimWhoseIdIsHeldTwiceIsSuspendedAtBoot(t *testing.T) {
	cfg := testConfig(t)
	cfg.LaunchFailureLimit = 1
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)
	first, second, busy := token("LEGION-901", claim.RoleImplementer), token("LEGION-902", claim.RoleImplementer), token("LEGION-903", claim.RoleImplementer)
	// queued is its own tree's architect, so the operator's spawn opens the tree it launches in.
	queued, own := token("LEGION-904", claim.RoleArchitect), token("LEGION-901", claim.RoleTester)
	// A name is an agent's own text; this one would end its literal and run a second statement,
	// scoped to a project that does not exist, should the statement fail to quote it.
	const twice = "ses_twice'; update claim_sessions set claim = 'x' where project = 'no-such-project'; --"

	seedConflicts(t, cfg.PostgresDSN, project, []conflictRow{
		// first and second share two names, the session id and the file's base name.
		{first, "LEGION-901", twice, "/sessions/a/shared.jsonl", supervise.StateIdle},
		{second, "LEGION-902", twice, "/sessions/b/shared.jsonl", supervise.StateIdle},
		{busy, "LEGION-903", twice, "/sessions/3.jsonl", supervise.StateWorking},
		{queued, "LEGION-904", twice, "/sessions/5.jsonl", supervise.StateQueued},
		{own, "LEGION-901", "ses_own", "/sessions/4.jsonl", supervise.StateIdle},
	}, // What the migration leaves of the names several claims record, and of the one recording its own.
		`($1, 'session', $2, ''), ($1, 'file-name', 'shared.jsonl', ''), ($1, 'session', 'ses_own', $3)`,
		project, twice, string(own))

	rt := fake.NewRuntime()
	logs := &syncBuffer{}
	d := startDaemonLogging(t, cfg, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)))

	for _, tok := range []claim.Token{first, second} {
		if got := d.claim(tok); got.State != string(supervise.StateSuspended) || got.SuspendedBy != string(supervise.SuspendedByOperator) {
			t.Errorf("%s, whose session is held for no claim, is %s suspended by %q after the boot; want it suspended by the operator's suspend",
				tok, got.State, got.SuspendedBy)
		}
	}
	if got := d.claim(busy); got.State != string(supervise.StateWorking) || !got.SuspensionHeld {
		t.Errorf("%s, in a turn with its session held for no claim, is %s holding a suspension %t after the boot; want the boot to have served with its suspension held for the turn",
			busy, got.State, got.SuspensionHeld)
	}
	if got := d.claim(queued); got.State != string(supervise.StateQueued) {
		t.Errorf("%s, queued with its session held for no claim, is %s after the boot; want it left queued, in no state a suspension reaches", queued, got.State)
	}
	if got := d.claim(own); got.State != string(supervise.StateIdle) {
		t.Errorf("%s, which holds its own session, is %s after the boot, want it idle as it was", own, got.State)
	}
	for _, call := range rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim != own {
			t.Errorf("the boot launched %s (%s), whose session is held for no claim", call.Spec.Claim, call.Method)
		}
	}
	stops := map[claim.Token]int{}
	for _, call := range rt.CallsOf("Suspend") {
		stops[call.Locator.Claim]++
	}
	if stops[first] != 1 || stops[second] != 1 || stops[own] != 0 {
		t.Errorf("the boot stopped the processes %v, want %s's and %s's once each, though they share two names, and not %s's", stops, first, second, own)
	}
	// Each suspension's reason names the name it is for, which the claim row does not keep, so the
	// log line is where an operator reads it (acceptance of #4220, row 8a): one line for each of
	// the two claims suspended at once, each naming one of the names they share.
	var reasons []string
	for _, l := range loggedLines(t, logs.String(), "supervise: suspended") {
		if reason, _ := l["reason"].(string); strings.HasPrefix(reason, "session name not its own: ") {
			reasons = append(reasons, reason)
		}
	}
	named := map[string]bool{"session name not its own: session " + twice: true, "session name not its own: file-name shared.jsonl": true}
	if len(reasons) != 2 || !named[reasons[0]] || !named[reasons[1]] {
		t.Errorf("the boot's suspensions gave the reasons %q, want one for each of %s and %s, naming a name they share", reasons, first, second)
	}

	lines := loggedLines(t, logs.String(), "recorded by claims that do not hold it")
	if len(lines) != 2 {
		t.Fatalf("the boot logged %d lines naming a name held for none, want 2: %v", len(lines), lines)
	}
	byName := map[string]map[string]any{}
	for _, line := range lines {
		name, _ := line["name"].(string)
		byName[name] = line
	}
	line := byName[twice]
	claims, _ := line["claims"].([]any)
	// The literals are spelled out here rather than built by supervise.SQLLiteral, so a change of
	// its quoting fails this compare as well as the run below.
	settle := "update claim_sessions set claim = '<token>' where project = E'" + project +
		"' and kind = E'session' and name = E'" + strings.ReplaceAll(twice, "'", "''") + "' and claim = ''" +
		" and '<token>' in (E'" + string(first) + "', E'" + string(second) + "', E'" + string(busy) + "', E'" + string(queued) + "')"
	if line["level"] != "ERROR" || line["kind"] != "session" || line["holder"] != "none" || len(claims) != 4 ||
		claims[0] != string(first) || claims[1] != string(second) || claims[2] != string(busy) || claims[3] != string(queued) || line["settle"] != settle {
		t.Errorf("the boot logged %v, want an error naming the session %q held for none, %s, %s, %s and %s, and the statement that settles it, %q",
			line, twice, first, second, busy, queued, settle)
	}
	if line := byName["shared.jsonl"]; line["kind"] != "file-name" || len(line["claims"].([]any)) != 2 {
		t.Errorf("the boot logged %v for the shared file name, want the file-name kind and %s and %s", line, first, second)
	}

	// The queued claim's launch is refused before anything starts, and its one try is its budget.
	launches := len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume"))
	status, body := d.request(http.MethodPost, "/legion/v1/operator/claims", api.SpawnRequest{Tree: "LEGION-904", Issue: "LEGION-904", Role: claim.RoleArchitect}, true)
	if got := d.claim(queued); got.State != string(supervise.StateFailed) {
		t.Errorf("%s, launched with its session held for no claim, is %s (the spawn answered %d %s); want it failed at its launch budget", queued, got.State, status, body)
	}
	// Its failure names the refusal, so an operator reads why it cannot start (acceptance of #4220,
	// row 9).
	if got := d.claim(queued).LastStartFailure; !strings.Contains(got, "is recorded for another claim of this daemon") {
		t.Errorf("%s's last start failure is %q, want the hold's refusal", queued, got)
	}
	if got := len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume")); got != launches {
		t.Errorf("the queued claim's launch reached the runtime %d times, want none: it was refused before anything started", got-launches)
	}

	// The statement the line gives, run as an operator runs it, records the rightful holder of a
	// name held for none, and moves nothing once the name names a claim: a second run with another
	// token answers UPDATE 0, so the first claim keeps the name its daemon was answered with.
	st, err := store.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logged, _ := line["settle"].(string)
	for _, run := range []struct {
		token claim.Token
		moved int64
	}{{own, 0}, {first, 1}, {second, 0}} {
		tag, err := st.Pool().Exec(context.Background(), strings.ReplaceAll(logged, "<token>", string(run.token)))
		if err != nil || tag.RowsAffected() != run.moved {
			t.Errorf("the settle statement run for %s moved %d rows (%v), want %d", run.token, tag.RowsAffected(), err, run.moved)
		}
	}
	if holder, ok := heldFor(t, cfg.PostgresDSN, project, supervise.HoldSessionID, twice); !ok || holder != first {
		t.Errorf("after both runs of the settle statement %q is held for %q, want %s, the claim the first run recorded", twice, holder, first)
	}
}

// A claim a daemon from before the hold recorded under a name no row can hold — a session id past
// claim.MaxSessionNameBytes, or a session file that is not a clean path — is named at boot and
// suspended by the operator's suspend, as a claim whose name is held for no claim is, but with no
// statement to settle it: no row can hold the name, so the claim is given a new session
// (store.SessionConflict.Unholdable, review round 11 of #4220). A claim of the same boot holding
// its own session is left as it was.
//
// The control (the pull request records the run): SessionConflicts without its unholdable names;
// both claims are re-adopted live, and the test fails naming them.
func TestABootSuspendsAClaimRecordingANameNoRowCanHold(t *testing.T) {
	cfg := testConfig(t)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)
	longID, unclean, own := token("LEGION-921", claim.RoleImplementer), token("LEGION-922", claim.RoleImplementer), token("LEGION-923", claim.RoleImplementer)
	long := strings.Repeat("s", claim.MaxSessionNameBytes+45)
	const uncleanFile = "/sessions/2026-11-16T00-00-00-000Z_ses_922.jsonl/.."
	seedConflicts(t, cfg.PostgresDSN, project, []conflictRow{
		{longID, "LEGION-921", long, "/sessions/921.jsonl", supervise.StateIdle},
		{unclean, "LEGION-922", "ses_922", uncleanFile, supervise.StateIdle},
		{own, "LEGION-923", "ses_own", "/sessions/923.jsonl", supervise.StateIdle},
	}, "")

	rt := fake.NewRuntime()
	logs := &syncBuffer{}
	d := startDaemonLogging(t, cfg, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)))

	for _, tok := range []claim.Token{longID, unclean} {
		if got := d.claim(tok); got.State != string(supervise.StateSuspended) || got.SuspendedBy != string(supervise.SuspendedByOperator) {
			t.Errorf("%s, which records a name no row can hold, is %s suspended by %q after the boot; want it suspended by the operator's suspend",
				tok, got.State, got.SuspendedBy)
		}
	}
	if got := d.claim(own); got.State != string(supervise.StateIdle) {
		t.Errorf("%s, which holds its own session, is %s after the boot, want it idle as it was", own, got.State)
	}
	lines := loggedLines(t, logs.String(), "a session name this daemon cannot hold")
	named := map[string]bool{}
	for _, line := range lines {
		claims, _ := line["claims"].([]any)
		if line["level"] != "ERROR" || line["holder"] != "none" || line["settle"] != nil || len(claims) != 1 {
			t.Errorf("the boot logged %v, want an error naming one claim, held for none, with no statement to settle it", line)
			continue
		}
		named[claims[0].(string)+" "+line["kind"].(string)] = true
	}
	if !named[string(longID)+" session"] || !named[string(unclean)+" file-name"] || len(named) != 2 {
		t.Errorf("the boot named %v, want %s's session id and %s's session file", named, longID, unclean)
	}
}

// A suspension that fails at boot for a reason no state explains — here a stop the runtime refuses
// for every claim — is logged at Error naming the claim, and the boot goes on: the claim is left as
// it was, its hold refuses every relaunch of it, and the daemon serves, so the operator's steps,
// which need a serving daemon, can run (review round 11 of #4220). Nothing is launched for it.
//
// The control (the pull request records the run): the boot failing on that error, as before; the
// daemon never answers, and the test fails at its start.
func TestABootThatCannotStopAClaimStillServes(t *testing.T) {
	cfg := testConfig(t)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)
	first, second := token("LEGION-931", claim.RoleImplementer), token("LEGION-932", claim.RoleImplementer)
	const twice = "ses_twice_931"
	seedConflicts(t, cfg.PostgresDSN, project, []conflictRow{
		{first, "LEGION-931", twice, "/sessions/931.jsonl", supervise.StateIdle},
		{second, "LEGION-932", twice, "/sessions/932.jsonl", supervise.StateIdle},
	}, `($1, 'session', $2, '')`, project, twice)

	rt := fake.NewRuntime()
	rt.FailSuspend(errors.New("the pod's container cannot be reached"))
	logs := &syncBuffer{}
	d := startDaemonLogging(t, cfg, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)))

	failed := map[string]bool{}
	for _, line := range loggedLines(t, logs.String(), "does not hold failed; it is left as it is") {
		if line["level"] == "ERROR" {
			failed[line["claim"].(string)] = true
		}
	}
	for _, tok := range []claim.Token{first, second} {
		if !failed[string(tok)] {
			t.Errorf("the boot logged no Error naming %s, whose stop the runtime refused", tok)
		}
		if got := d.claim(tok); got.State != string(supervise.StateIdle) {
			t.Errorf("%s, whose stop the runtime refused, is %s after the boot, want it left idle as it was", tok, got.State)
		}
	}
	for _, call := range rt.Calls() {
		if call.Method == "Spawn" || call.Method == "Resume" {
			t.Errorf("the boot launched %s (%s), whose session is held for no claim", call.Spec.Claim, call.Method)
		}
	}
}

// A restored machine arms its timers as restore builds it, and a timer decides on its own
// goroutine the moment it fires, so the boot holds every recorded name no row holds before
// restore builds the first machine (start, review round 9 of #4220). Two claims record one session
// no row holds, as a daemon rolled back to a release before the hold leaves them: one registered,
// whose restored registration deadline relaunches it, and one idle. A transaction of the test holds
// an uncommitted row of a third claim's name, the first the boot's backfill inserts (a file-id,
// the kind it takes first, store.HoldRecordedSessions), so the backfill waits before it has
// inserted any name of the other two. Once PostgreSQL shows a statement of the boot waiting on
// that row (pg_blocking_pids), the registered claim's deadline is fired if one is armed, its
// relaunch given time to reach the runtime, and then the row is let go. No machine exists to fire,
// the backfill holds the session for no claim, both claims are suspended by the operator's
// suspend, and nothing resumes the session. A boot that never waits on the row fails the test,
// since then it could not see the order it checks.
//
// The control: today's order, the hold after restore (start calling holdRecordedSessions after
// restore). The deadline fires while the backfill waits, the relaunch holds the session for the
// registered claim and resumes it, the backfill keeps that claim's row, and the test fails naming
// the resume and the holder (the pull request records the run).
func TestARestoredClaimTakesNoSessionBeforeTheBootHoldsIt(t *testing.T) {
	cfg := testConfig(t)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)
	registered, idle, waiter := token("LEGION-911", claim.RoleImplementer), token("LEGION-912", claim.RoleImplementer), token("LEGION-913", claim.RoleImplementer)
	const shared = "ses_shared"
	seedConflicts(t, cfg.PostgresDSN, project, []conflictRow{
		{registered, "LEGION-911", shared, "/sessions/911.jsonl", supervise.StateRegistered},
		{idle, "LEGION-912", shared, "/sessions/912.jsonl", supervise.StateIdle},
		{waiter, "LEGION-913", "ses_waiter", "/sessions/2026-11-16T00-00-00-000Z_a-waiter.jsonl", supervise.StateIdle},
	}, "")

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The registered claim's tree is open, so its relaunch reaches its hold (treelifecycle.CheckWork).
	if _, err := st.Pool().Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority) values ($1, 'LEGION-911', 1, 'workflow')`, project); err != nil {
		t.Fatalf("open the registered claim's tree: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `update claims set tree_epoch = 1 where token = $1`, string(registered)); err != nil {
		t.Fatalf("bind the registered claim to its tree's epoch: %v", err)
	}
	row, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Every path out of the test lets the row go, so a failure reports rather than leaving the
	// boot waiting on it until the package's timeout (a second rollback answers ErrTxClosed).
	defer func() { _ = row.Rollback(ctx) }()
	if _, err := row.Exec(ctx, `insert into claim_sessions (project, kind, name, claim) values ($1, 'file-id', 'a-waiter', $2)`,
		project, string(waiter)); err != nil {
		t.Fatalf("hold the waiting claim's row uncommitted: %v", err)
	}
	var holderPID int
	if err := row.QueryRow(ctx, `select pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("read the holding transaction's backend: %v", err)
	}

	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	clock := &heldClock{}
	o.clock = clock
	deadline := cfg.WorkerBootTimeout * time.Duration(cfg.WorkerBootRegistrationDeadlineIntervals)
	type waited struct {
		blocked bool
		probe   error
		armed   int
		fired   int
	}
	result := make(chan waited, 1)
	go func() {
		// Wait for a statement of the boot to wait on the row: the backfill has reached its first
		// name. Then count every registration deadline armed so far, fire the registered claim's,
		// wait for what it launches to reach the runtime, and let the row go.
		var w waited
		for until := time.Now().Add(time.Minute); !w.blocked && time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
			var waiting int
			w.probe = st.Pool().QueryRow(ctx, `select count(*) from pg_stat_activity where $1 = any(pg_blocking_pids(pid))`, holderPID).Scan(&waiting)
			w.blocked = w.probe == nil && waiting > 0
		}
		if w.blocked {
			_, w.armed = clock.armed(deadline)
			w.fired = clock.fire(deadline)
			for until := time.Now().Add(3 * time.Second); w.fired > 0 && len(rt.CallsOf("Resume")) == 0 && time.Now().Before(until); {
				time.Sleep(10 * time.Millisecond)
			}
		}
		_ = row.Rollback(ctx)
		result <- w
	}()
	d := startDaemon(t, cfg, o)
	w := <-result
	if !w.blocked {
		t.Fatalf("no statement of the boot waited on the held row within a minute (the last probe's error: %v), so the test could not see whether a restored timer runs before the backfill", w.probe)
	}
	whileWaiting := w.fired
	t.Logf("deadlines armed while the backfill waited: %d, of which fired: %d; the runtime was asked Suspend %d, Resume %d and Spawn %d times; %s is %s",
		w.armed, whileWaiting, len(rt.CallsOf("Suspend")), len(rt.CallsOf("Resume")), len(rt.CallsOf("Spawn")), registered, d.claim(registered).State)
	if w.armed != 0 {
		t.Errorf("%d registration deadlines were armed while the backfill waited on its first name, want none: no machine may exist before the recorded names are held", w.armed)
	}
	if _, total := clock.armed(deadline); total < 1 {
		t.Errorf("restore armed no registration deadline for %s, so the control that fires it could not reach the order this test checks", registered)
	}

	if resumes := rt.CallsOf("Resume"); len(resumes) != 0 {
		t.Errorf("the boot resumed %d processes (a deadline fired %d times while the backfill waited), want none: %s's session is %s, which %s records too",
			len(resumes), whileWaiting, registered, shared, idle)
	}
	if holder, ok := heldFor(t, cfg.PostgresDSN, project, supervise.HoldSessionID, shared); !ok || holder != "" {
		t.Errorf("%s is held for %q (a row holds it: %t) after the boot, want it held for no claim: two claims record it", shared, holder, ok)
	}
	for _, tok := range []claim.Token{registered, idle} {
		if got := d.claim(tok); got.State != string(supervise.StateSuspended) || got.SuspendedBy != string(supervise.SuspendedByOperator) {
			t.Errorf("%s, whose session %s two claims record, is %s suspended by %q after the boot; want it suspended by the operator's suspend",
				tok, shared, got.State, got.SuspendedBy)
		}
	}
}

// A daemon rolled back to a release before the hold runs on the claim_sessions table without
// writing it, and the migration's backfill does not run again on the roll forward: so the boot
// holds every name a claim records that no row holds (store.HoldRecordedSessions), for the one claim
// recording it, and suspends a claim whose current session records a name another claim holds,
// which the rolled-back daemon let it register (store.SessionConflicts). The claim it took the name
// from, and a claim whose name was merely left unheld, are left as they were. So is a claim that
// lost a conflict and was given a new session: the lost name is in its abandoned session now, which
// nothing resumes or enrolls, so the boot logs it at Warn and suspends nothing (review R2-2). The
// line for a name held for another claim gives no settle statement, since that name stays its
// holder's (review N6).
//
// The controls (the pull request records each run): the boot without HoldRecordedSessions leaves
// the window's name unheld, so the next claim to register it takes it; the boot without the
// conflict read leaves the taker idle on the other claim's session; counting the abandoned
// session's names as the current session's suspends the claim that lost the conflict again.
func TestABootHoldsWhatARolledBackDaemonRecorded(t *testing.T) {
	cfg := testConfig(t)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)
	window, holder, taker := token("LEGION-905", claim.RoleImplementer), token("LEGION-906", claim.RoleImplementer), token("LEGION-907", claim.RoleImplementer)
	loser := token("LEGION-908", claim.RoleImplementer)

	seedConflicts(t, cfg.PostgresDSN, project, []conflictRow{
		// Registered while the rolled-back daemon ran: nothing holds ses_window.
		{window, "LEGION-905", "ses_window", "/sessions/6.jsonl", supervise.StateIdle},
		// holder recorded ses_taken under the hold; taker registered it while the rolled-back daemon
		// ran.
		{holder, "LEGION-906", "ses_taken", "/sessions/7.jsonl", supervise.StateIdle},
		{taker, "LEGION-907", "ses_taken", "/sessions/8.jsonl", supervise.StateIdle},
		// loser lost ses_lost to holder, and an operator gave it a new session.
		{loser, "LEGION-908", "ses_loser_fresh", "/sessions/9.jsonl", supervise.StateIdle},
	}, `($1, 'session', 'ses_taken', $2), ($1, 'session', 'ses_lost', $2)`, project, string(holder))
	abandonSession(t, cfg.PostgresDSN, loser, "ses_lost")

	rt := fake.NewRuntime()
	logs := &syncBuffer{}
	d := startDaemonLogging(t, cfg, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)))

	if got, held := heldFor(t, cfg.PostgresDSN, project, supervise.HoldSessionID, "ses_window"); !held || got != window {
		t.Errorf("ses_window is held for %q (a row %t) after the boot; want it held for %s, the one claim recording it", got, held, window)
	}
	if got, held := heldFor(t, cfg.PostgresDSN, project, supervise.HoldFileName, "8.jsonl"); !held || got != taker {
		t.Errorf("taker's file is held for %q (a row %t) after the boot; want it held for %s", got, held, taker)
	}
	if got, _ := heldFor(t, cfg.PostgresDSN, project, supervise.HoldSessionID, "ses_taken"); got != holder {
		t.Errorf("ses_taken is held for %q after the boot; want it still %s's", got, holder)
	}
	if got := d.claim(taker); got.State != string(supervise.StateSuspended) || got.SuspendedBy != string(supervise.SuspendedByOperator) {
		t.Errorf("%s, which records %s's session, is %s suspended by %q after the boot; want it suspended by the operator's suspend", taker, holder, got.State, got.SuspendedBy)
	}
	for _, tok := range []claim.Token{window, holder, loser} {
		if got := d.claim(tok); got.State != string(supervise.StateIdle) {
			t.Errorf("%s is %s after the boot, want it idle as it was", tok, got.State)
		}
	}
	lines := loggedLines(t, logs.String(), "recorded by claims that do not hold it")
	if len(lines) != 1 || lines[0]["name"] != "ses_taken" || lines[0]["holder"] != string(holder) {
		t.Errorf("the boot logged %v, want one error naming ses_taken, held by %s", lines, holder)
	} else if settle, ok := lines[0]["settle"]; ok {
		t.Errorf("the boot's line for ses_taken, held for %s, gives the statement %q; want none for a name held for another claim", holder, settle)
	}
	warned := loggedLines(t, logs.String(), "only an abandoned session records")
	if len(warned) != 1 || warned[0]["level"] != "WARN" || warned[0]["name"] != "ses_lost" {
		t.Errorf("the boot logged %v, want one warning naming ses_lost, which only %s's abandoned session records", warned, loser)
	} else if abandoned, _ := warned[0]["abandoned"].([]any); len(abandoned) != 1 || abandoned[0] != string(loser) {
		t.Errorf("the warning names %v, want %s", warned[0]["abandoned"], loser)
	}
	if held := loggedLines(t, logs.String(), "held session names claims recorded"); len(held) != 1 {
		t.Errorf("the boot logged %d lines of the names it held, want 1", len(held))
	}
}

// abandonSession records session as the one token's claim last abandoned, as forget-session leaves
// it.
func abandonSession(t *testing.T, dsn string, token claim.Token, session string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Pool().Exec(ctx, `update claims set abandoned_session = jsonb_build_object(
		'session', $2::text, 'why', 'an operator gave the claim a new session', 'at', '2026-11-16T00:00:00Z')
		where token = $1`, string(token), session); err != nil {
		t.Fatalf("record %s's abandoned session: %v", token, err)
	}
}

// earlierBootClaim is a claim an earlier boot left in state on issue, recording session, with the
// tmux process its locator names and this daemon's plugin contract: the boot re-adopts it rather
// than launching it.
func earlierBootClaim(project string, token claim.Token, issue, session string, state supervise.ClaimState, n int) supervise.Claim {
	role, _ := token.Role()
	pane := strconv.Itoa(n)
	return supervise.Claim{
		Token: token, Project: project, Tree: issue, Issue: issue, Role: role, Generation: 1,
		Session: session, SessionFile: "/sessions/" + strconv.Itoa(n) + ".jsonl",
		Locator: &runtime.Locator{
			Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "3100" + pane + ":90000" + pane,
			Tmux: &runtime.TmuxLocator{Window: "@" + pane, Pane: "%" + pane},
		},
		State:          state,
		BootTokenHash:  supervise.HashBootToken("boot-" + string(token)),
		CapabilityHash: []byte{byte(n)},
		PluginContract: api.DaemonAPIVersion,
	}
}

// withholdCall is one request of the Envoy listener's role-withhold route as withholdListener
// received it: its method, the role and session its body named, and the bearer it carried.
type withholdCall struct {
	method, role, session, bearer string
}

// withholdListener stands in for the Envoy listener: it records each PUT and DELETE of
// /v1/role-withholds and answers it what answer gives for its method (204 with no body where
// answer is nil), and answers every other request 404 naming no holder, as a listener that knows
// no session and no role claim does.
type withholdListener struct {
	url    string
	answer func(method string) (int, string)
	mu     sync.Mutex
	seen   []withholdCall
}

func newWithholdListener(t *testing.T, answer func(method string) (int, string)) *withholdListener {
	t.Helper()
	l := &withholdListener{answer: answer}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/role-withholds" || (r.Method != http.MethodPut && r.Method != http.MethodDelete) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found","reason":"unclaimed"}`)
			return
		}
		var body listenerwire.RoleWithholdRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("the daemon sent %s /v1/role-withholds a body that does not decode: %v", r.Method, err)
		}
		l.mu.Lock()
		l.seen = append(l.seen, withholdCall{method: r.Method, role: body.Role, session: body.Session, bearer: r.Header.Get("Authorization")})
		l.mu.Unlock()
		status, answered := http.StatusNoContent, ""
		if l.answer != nil {
			status, answered = l.answer(r.Method)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answered)
	}))
	t.Cleanup(server.Close)
	l.url = server.URL
	return l
}

// calls is every withhold and lift the listener has received, oldest first.
func (l *withholdListener) calls() []withholdCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.seen)
}

// asked reports whether the listener received a request of method for role and session.
func (l *withholdListener) asked(method string, role claim.Token, session string) bool {
	return slices.ContainsFunc(l.calls(), func(c withholdCall) bool {
		return c.method == method && c.role == string(role) && c.session == session
	})
}

// envoyTokenFile writes token as the daemon's listener bearer (`envoy_token_file`) and returns its
// path.
func envoyTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "envoy-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Envoy token: %v", err)
	}
	return path
}

// A claim the boot could not stop runs on under a session it does not hold, and its process still
// claims the role its session claimed. So the boot asks the Envoy listener to withhold that role
// from that session (PUT /v1/role-withholds), over the bearer it presents the listener for every
// other call: the listener frees the session's claim of the role and refuses each claim of it the
// session makes, so no message to the role reaches that process, from any sender. Here the runtime
// refuses both claims' stops, so both roles are withheld from the session they share, and neither
// is lifted while neither holds it. Once the runtime answers, the operator stops both, records the
// rightful holder with the boot's settle statement and resumes it: that claim's role is lifted
// (DELETE /v1/role-withholds), and the other claim's stays withheld.
//
// The controls (the pull request records each run): suspendSessionConflicts without its
// withhold; the listener is asked none, and the test fails naming the role. The resume route
// without its lift; the test fails naming the rightful claim's role.
func TestARefusedBootStopWithholdsTheRoleUntilTheClaimHoldsItsSession(t *testing.T) {
	cfg := testConfig(t)
	listener := newWithholdListener(t, nil)
	cfg.EnvoyURL = listener.url
	cfg.EnvoyTokenFile = envoyTokenFile(t, "envoy-bearer")
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)
	rightful, contained := token("LEGION-951", claim.RoleImplementer), token("LEGION-952", claim.RoleImplementer)
	const shared = "ses_withheld_951"
	rt := fake.NewRuntime()
	rt.FailSuspend(errors.New("the pod's container cannot be reached"))
	d, settle, _ := settleLineOn(t, cfg, rt, project, shared, []conflictRow{
		{rightful, "LEGION-951", shared, "/sessions/a/951.jsonl", supervise.StateIdle},
		{contained, "LEGION-952", shared, "/sessions/b/952.jsonl", supervise.StateIdle},
	}, []claim.Token{rightful, contained}, `($1, 'session', $2, '')`, project, shared)

	for _, tok := range []claim.Token{rightful, contained} {
		if !listener.asked(http.MethodPut, tok, shared) {
			t.Fatalf("the boot asked the listener no withhold of role %s from session %s, though the runtime refused its stop; the listener received %v",
				tok, shared, listener.calls())
		}
	}
	for _, call := range listener.calls() {
		if call.method != http.MethodPut {
			t.Errorf("the boot sent the listener %+v, while neither claim holds %s; want withholds alone", call, shared)
		}
		if call.bearer != "Bearer envoy-bearer" {
			t.Errorf("the boot's %s of role %s carried %q, want the bearer it presents the listener", call.method, call.role, call.bearer)
		}
	}

	rt.FailSuspend(nil)
	operator := func(target claim.Token, route string) {
		t.Helper()
		if status, answer := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(target)+"/"+route, nil, true); status != http.StatusOK {
			t.Fatalf("the operator's %s of %s = %d %s, want 200", route, target, status, answer)
		}
	}
	operator(rightful, "suspend")
	operator(contained, "suspend")
	if moved := operatorSQL(t, cfg.PostgresDSN, strings.ReplaceAll(settle, "<token>", string(rightful)), ""); moved != 1 {
		t.Fatalf("the settle statement moved %d rows, want 1", moved)
	}
	operator(rightful, "resume")

	if !listener.asked(http.MethodDelete, rightful, shared) {
		t.Fatalf("the operator's resume of %s, which holds session %s again, lifted no withhold of role %s; the listener received %v",
			rightful, shared, rightful, listener.calls())
	}
	if listener.asked(http.MethodDelete, contained, shared) {
		t.Errorf("role %s was lifted from session %s, which %s does not hold", contained, shared, contained)
	}
}

// The boot lifts the withhold of a claim that holds its session again. An earlier boot withheld
// both claims' roles from the session they shared, and the operator recorded the rightful holder
// with the settle statement while the daemon was down. This boot finds the other claim still
// recording the name the rightful one holds: it suspends that claim, and since the runtime refuses
// the stop, withholds its role again; and it lifts the rightful claim's role from its session. The
// other claim's role is not lifted, and the rightful one's is not withheld. The rightful claim is
// idle, not suspended, so the operator's resume of it is refused 409, and lifts its role all the
// same: its process is the session's rightful one.
//
// The controls (the pull request records each run): suspendSessionConflicts without its lift;
// the listener is asked none, and the test fails naming the rightful claim's role. The lift
// asked after the machine's answer, and only on a resume it took; the refused resume lifts
// nothing, and the test fails naming the role.
func TestABootLiftsTheWithholdOfAClaimThatHoldsItsSessionAgain(t *testing.T) {
	cfg := testConfig(t)
	listener := newWithholdListener(t, nil)
	cfg.EnvoyURL = listener.url
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)
	rightful, contained := token("LEGION-961", claim.RoleImplementer), token("LEGION-962", claim.RoleImplementer)
	const shared = "ses_settled_961"
	seedConflicts(t, cfg.PostgresDSN, project, []conflictRow{
		{rightful, "LEGION-961", shared, "/sessions/a/961.jsonl", supervise.StateIdle},
		{contained, "LEGION-962", shared, "/sessions/b/962.jsonl", supervise.StateIdle},
	}, `($1, 'session', $2, $3)`, project, shared, string(rightful))

	rt := fake.NewRuntime()
	rt.FailSuspend(errors.New("the pod's container cannot be reached"))
	d := startDaemon(t, cfg, fakeRuntime(rt, &built{}))

	if !listener.asked(http.MethodDelete, rightful, shared) {
		t.Fatalf("the boot lifted no withhold of role %s from session %s, which it holds again; the listener received %v",
			rightful, shared, listener.calls())
	}
	if !listener.asked(http.MethodPut, contained, shared) {
		t.Errorf("the boot withheld no role %s from session %s, which %s does not hold and whose stop the runtime refused", contained, shared, contained)
	}
	if listener.asked(http.MethodDelete, contained, shared) || listener.asked(http.MethodPut, rightful, shared) {
		t.Errorf("the boot sent %v; want %s's role withheld and %s's lifted, and nothing else", listener.calls(), contained, rightful)
	}

	status, answer := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(rightful)+"/resume", nil, true)
	if status != http.StatusConflict {
		t.Fatalf("the operator's resume of %s, which is idle, answered %d %s; want 409, since it is not suspended", rightful, status, answer)
	}
	lifts := 0
	for _, call := range listener.calls() {
		if call.method == http.MethodDelete && call.role == string(rightful) && call.session == shared {
			lifts++
		}
	}
	if lifts != 2 {
		t.Errorf("role %s was lifted from %s %d times, want twice, by the boot and by the operator's refused resume; the listener received %v",
			rightful, shared, lifts, listener.calls())
	}
}

// A withhold the listener refuses, here the 403 it gives every caller that is not the daemon's
// own broker token, is logged at Error naming the role, the session and the status, and the boot
// goes on: the claim is left as it was, contained, and the daemon serves.
//
// The control (the pull request records the run): the refusal logged at Warn; the test fails
// naming the role.
func TestARefusedWithholdIsLoggedAndTheBootGoesOn(t *testing.T) {
	cfg := testConfig(t)
	listener := newWithholdListener(t, func(string) (int, string) {
		return http.StatusForbidden, `{"error":"role withholds are a Legion daemon's own"}`
	})
	cfg.EnvoyURL = listener.url
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)
	first, second := token("LEGION-971", claim.RoleImplementer), token("LEGION-972", claim.RoleImplementer)
	const twice = "ses_twice_971"
	seedConflicts(t, cfg.PostgresDSN, project, []conflictRow{
		{first, "LEGION-971", twice, "/sessions/971.jsonl", supervise.StateIdle},
		{second, "LEGION-972", twice, "/sessions/972.jsonl", supervise.StateIdle},
	}, `($1, 'session', $2, '')`, project, twice)

	rt := fake.NewRuntime()
	rt.FailSuspend(errors.New("the pod's container cannot be reached"))
	logs := &syncBuffer{}
	d := startDaemonLogging(t, cfg, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)))

	refused := map[string]map[string]any{}
	for _, line := range loggedLines(t, logs.String(), "the Envoy listener refused to withhold") {
		role, _ := line["role"].(string)
		refused[role] = line
	}
	for _, tok := range []claim.Token{first, second} {
		line := refused[string(tok)]
		reason, _ := line["error"].(string)
		if line["level"] != "ERROR" || line["session"] != twice || !strings.Contains(reason, "403") {
			t.Errorf("the boot logged %v for the refused withhold of role %s, want an Error naming the role, session %s and the status 403", line, tok, twice)
		}
		if got := d.claim(tok); got.State != string(supervise.StateIdle) {
			t.Errorf("%s, whose stop the runtime and whose withhold the listener refused, is %s after the boot, want it left idle as it was", tok, got.State)
		}
	}
}
