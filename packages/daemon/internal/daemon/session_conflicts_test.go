package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"testing"
	"time"

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
// carries it as a SQL literal, its quotes doubled.
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
	const twice = "ses_twice'; update claim_sessions set claim = 'x'; --"

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
	settle := "update claim_sessions set claim = '<token>' where project = '" + project +
		"' and kind = 'session' and name = 'ses_twice''; update claim_sessions set claim = ''x''; --'"
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
	if got := len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume")); got != launches {
		t.Errorf("the queued claim's launch reached the runtime %d times, want none: it was refused before anything started", got-launches)
	}
}

// A restored machine arms its timers as restore builds it, and a timer decides on its own
// goroutine the moment it fires, so the boot holds every recorded name no row holds before
// restore builds the first machine (start, review round 9 of #4220). Two claims record one session
// no row holds, as a daemon rolled back to a release before the hold leaves them: one registered,
// whose restored registration deadline relaunches it, and one idle. A transaction of the test holds
// an uncommitted row of a third claim's name, the first the boot's backfill inserts (a file-id,
// the kind it takes first, store.HoldRecordedSessions), so the backfill waits before it has
// inserted any name of the other two; while it waits the registered claim's deadline is fired, if
// one is armed, and its relaunch given time to reach the runtime, and then the row is let go. No
// machine exists to fire, the backfill holds the session for no claim, both claims are suspended
// by the operator's suspend, and nothing resumes the session.
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
	if _, err := row.Exec(ctx, `insert into claim_sessions (project, kind, name, claim) values ($1, 'file-id', 'a-waiter', $2)`,
		project, string(waiter)); err != nil {
		t.Fatalf("hold the waiting claim's row uncommitted: %v", err)
	}

	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	clock := &heldClock{}
	o.clock = clock
	deadline := cfg.WorkerBootTimeout * time.Duration(cfg.WorkerBootRegistrationDeadlineIntervals)
	fired := make(chan int, 1)
	go func() {
		// The boot waits on the row; fire the registered claim's deadline while it does, wait for
		// what it launches to reach the runtime, then let the row go. Three seconds is past the few
		// milliseconds restore takes, should it run first.
		n := 0
		for until := time.Now().Add(3 * time.Second); n == 0 && time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
			n = clock.fire(deadline)
		}
		for until := time.Now().Add(3 * time.Second); n > 0 && len(rt.CallsOf("Resume")) == 0 && time.Now().Before(until); {
			time.Sleep(10 * time.Millisecond)
		}
		_ = row.Rollback(ctx)
		fired <- n
	}()
	d := startDaemon(t, cfg, o)
	whileWaiting := <-fired
	t.Logf("deadlines fired while the backfill waited: %d; the runtime was asked Suspend %d, Resume %d and Spawn %d times; %s is %s",
		whileWaiting, len(rt.CallsOf("Suspend")), len(rt.CallsOf("Resume")), len(rt.CallsOf("Spawn")), registered, d.claim(registered).State)

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
