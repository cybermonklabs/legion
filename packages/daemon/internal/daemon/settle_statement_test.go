package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// settleLine boots a daemon of cfg over rows and holds (seedConflicts), with the tree of each
// claim in open open, so its relaunch reaches its hold (treelifecycle.CheckWork), and returns it,
// its runtime, and the settle statement its boot logs for the name held for no claim.
func settleLine(t *testing.T, cfg config.Config, project, name string, rows []conflictRow, open []claim.Token, holds string, args ...any) (*daemon, *fake.Runtime, string) {
	t.Helper()
	rt := fake.NewRuntime()
	d, settle, _ := settleLineOn(t, cfg, rt, project, name, rows, open, holds, args...)
	return d, rt, settle
}

// settleLineOn is settleLine on the runtime rt, which a test may set to refuse the boot's stops,
// returning the daemon, the settle statement and the boot's log.
func settleLineOn(t *testing.T, cfg config.Config, rt *fake.Runtime, project, name string, rows []conflictRow, open []claim.Token, holds string, args ...any) (*daemon, string, *syncBuffer) {
	t.Helper()
	seedConflicts(t, cfg.PostgresDSN, project, rows, holds, args...)
	if len(open) > 0 {
		ctx := context.Background()
		st, err := store.Open(ctx, cfg.PostgresDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		for _, row := range rows {
			if !slices.Contains(open, row.token) {
				continue
			}
			if _, err := st.Pool().Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority) values ($1, $2, 1, 'workflow')`, project, row.issue); err != nil {
				t.Fatalf("open the tree of %s: %v", row.token, err)
			}
			if _, err := st.Pool().Exec(ctx, `update claims set tree_epoch = 1 where token = $1`, string(row.token)); err != nil {
				t.Fatalf("bind %s to its tree: %v", row.token, err)
			}
		}
	}
	logs := &syncBuffer{}
	d := startDaemonLogging(t, cfg, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)))
	for _, line := range loggedLines(t, logs.String(), "recorded by claims that do not hold it") {
		if line["name"] == name {
			settle, _ := line["settle"].(string)
			return d, settle, logs
		}
	}
	t.Fatalf("the boot logged no settle statement for %q:\n%s", name, logs.String())
	return nil, "", nil
}

// operatorSQL runs statement as an operator's session does, with standard_conforming_strings as
// given ("" leaves the server's default), and answers how many rows it changed.
func operatorSQL(t *testing.T, dsn, statement, conforming string) int64 {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if conforming != "" {
		if _, err := tx.Exec(ctx, "set local standard_conforming_strings = "+conforming); err != nil {
			t.Fatal(err)
		}
	}
	tag, err := tx.Exec(ctx, statement)
	if err != nil {
		t.Fatalf("run %q with standard_conforming_strings %q: %v", statement, conforming, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return tag.RowsAffected()
}

// The settle statement the boot prints for a name held for no claim moves that name alone,
// whatever standard_conforming_strings the operator's session sets (supervise.SQLLiteral, review
// of round 11 of #4220, F1). A name is an agent's own text, and this one, `\'` before its quote,
// ends a plain literal with the setting off and comments the statement's guard out after it.
//
// The control (the pull request records the run): SQLLiteral writing a plain '…' literal with only
// its quotes doubled, as before; with the setting off the statement moves every row of the
// table, the session another claim holds included.
func TestTheSettleStatementMovesOnlyItsNameUnderEitherStringSetting(t *testing.T) {
	cfg := testConfig(t)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)
	first, second, own := token("LEGION-931", claim.RoleImplementer), token("LEGION-932", claim.RoleImplementer), token("LEGION-933", claim.RoleImplementer)
	const shared = `ses_bs\' or true --`
	_, _, settle := settleLine(t, cfg, project, shared, []conflictRow{
		{first, "LEGION-931", shared, "/sessions/a/931.jsonl", supervise.StateIdle},
		{second, "LEGION-932", shared, "/sessions/b/932.jsonl", supervise.StateIdle},
		{own, "LEGION-933", "ses_own", "/sessions/933.jsonl", supervise.StateIdle},
	}, nil, `($1, 'session', $2, ''), ($1, 'session', 'ses_own', $3)`, project, shared, string(own))
	if moved := operatorSQL(t, cfg.PostgresDSN, strings.ReplaceAll(settle, "<token>", string(first)), "off"); moved != 1 {
		t.Errorf("the settle statement run with standard_conforming_strings off moved %d rows, want 1: %s", moved, settle)
	}
	if moved := operatorSQL(t, cfg.PostgresDSN, strings.ReplaceAll(settle, "<token>", string(second)), "on"); moved != 0 {
		t.Errorf("a second run with standard_conforming_strings on moved %d rows, want 0: the name is held", moved)
	}
	if holder, ok := heldFor(t, cfg.PostgresDSN, project, supervise.HoldSessionID, shared); !ok || holder != first {
		t.Errorf("%q is held for %q, want %s, the claim the first run recorded", shared, holder, first)
	}
	if holder, ok := heldFor(t, cfg.PostgresDSN, project, supervise.HoldSessionID, "ses_own"); !ok || holder != own {
		t.Errorf("ses_own, which %s holds, is held for %q after the settle: the statement moved a name it does not name", own, holder)
	}
}

// A first settle run that names the wrong one of the line's claims is undone by the steps the
// docs give (docs/kubernetes.md, review of round 11 of #4220, F2): suspend the claim the first
// run named, forget its session, move the name to the rightful claim, and resume both. The forget
// comes before the move, while the wrong claim still holds the name, so it asks the listener
// nothing, and it empties that claim's machine of the hold it was answered with: the wrong claim
// starts fresh, and only the rightful claim resumes the session.
//
// The control (the pull request records the run): the undo without its forget; the wrong claim's
// machine still answers the hold it cached, and it resumes the session the rightful claim
// resumes too.
func TestAWrongSettleRunIsUndoneByTheDocumentedSteps(t *testing.T) {
	cfg := testConfig(t)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := projectTokens(t, project)
	rightful, wrong := token("LEGION-941", claim.RoleImplementer), token("LEGION-942", claim.RoleImplementer)
	const shared = "ses_undo"
	files := map[claim.Token]string{rightful: "/sessions/a/941.jsonl", wrong: "/sessions/b/942.jsonl"}
	d, rt, settle := settleLine(t, cfg, project, shared, []conflictRow{
		{rightful, "LEGION-941", shared, files[rightful], supervise.StateIdle},
		{wrong, "LEGION-942", shared, files[wrong], supervise.StateIdle},
	}, []claim.Token{rightful, wrong}, `($1, 'session', $2, '')`, project, shared)
	operator := func(target claim.Token, route string, body any) {
		t.Helper()
		if status, answer := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(target)+"/"+route, body, true); status != http.StatusOK {
			t.Fatalf("the operator's %s of %s = %d %s, want 200", route, target, status, answer)
		}
	}
	resumes := func(target claim.Token) int {
		n := 0
		for _, call := range rt.CallsOf("Resume") {
			if call.Spec.Claim == target && call.Spec.ResumeSessionFile == files[target] {
				n++
			}
		}
		return n
	}

	// The first run names the wrong claim, which is resumed and comes up under the session: its
	// machine is answered that it holds the name, and keeps that answer.
	if moved := operatorSQL(t, cfg.PostgresDSN, strings.ReplaceAll(settle, "<token>", string(wrong)), ""); moved != 1 {
		t.Fatalf("the wrong first run moved %d rows, want 1", moved)
	}
	operator(wrong, "resume", nil)
	testwait.Eventually(t, "the wrong claim to resume the session", func() bool { return resumes(wrong) == 1 })
	readyUnder(t, d, rt, wrong, shared, files[wrong])

	// The undo, as the docs give it.
	operator(wrong, "suspend", nil)
	operator(wrong, "forget-session", api.ForgetSessionRequest{Session: shared})
	undo := "update claim_sessions set claim = '" + string(rightful) + "' where project = '" + project +
		"' and kind = 'session' and name = '" + shared + "' and claim = '" + string(wrong) + "'"
	if moved := operatorSQL(t, cfg.PostgresDSN, undo, ""); moved != 1 {
		t.Fatalf("the undo's move answered %d rows, want 1", moved)
	}
	operator(rightful, "resume", nil)
	operator(wrong, "resume", nil)
	testwait.Eventually(t, "the rightful claim to resume the session, and the wrong one to start", func() bool {
		return resumes(rightful) == 1 && len(launchesOf(rt, wrong)) == 2
	})
	if holder, ok := heldFor(t, cfg.PostgresDSN, project, supervise.HoldSessionID, shared); !ok || holder != rightful {
		t.Errorf("%s is held for %q after the undo, want %s", shared, holder, rightful)
	}
	if n := resumes(wrong); n != 1 {
		t.Errorf("the wrong claim resumed %s %d times, want once, before the undo: after it, it starts fresh", shared, n)
	}
	if c := d.claim(wrong); c.Session == shared {
		t.Errorf("the wrong claim records %s after the undo, want a new session", shared)
	}
}

// readyUnder registers the claim's latest launch under session and file and reports it ready, as
// its agent would.
func readyUnder(t *testing.T, d *daemon, rt *fake.Runtime, token claim.Token, session, file string) {
	t.Helper()
	launch := lastLaunch(t, rt, token)
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: file,
		AgentID: "agent", PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s under %s = %d; body %s", token, session, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration: %v", err)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d; body %s", token, status, body)
	}
}
