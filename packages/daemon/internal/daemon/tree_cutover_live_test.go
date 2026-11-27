//go:build e2e

// The live proof of the cut-over (k31 of dispatch://INFRA-4730's Verification; R21 and R22 of
// dispatch://INFRA-3320): the release before runs on a database, stops, and this release boots on
// the same database, is stopped after each step of its first boot's conversion and booted again,
// and booted once more, and every run, claim, session, session-name reservation, slot, cleanup
// reservation and production check the release before left reads as it did, or as R22 says it
// becomes. scripts/e2e/tree-cutover.sh builds this driver with `-tags e2e` from this checkout and
// the release before's driver (tree_release_before_live_test.go laid over the tree of the release
// before, which scripts/e2e/lib/tree-proof.sh finds) from that commit, whose test binary it names in
// LEGION_E2E_BEFORE_DRIVER; it owns the nats-server (LEGION_E2E_NATS_URL) and makes the oldest
// plugin contract's root (LEGION_E2E_OLD_TOOL_ROOT); the PostgreSQL is LEGION_TEST_PG_DSN's.
//
// The rig is the driver's own (treeRig): the real Dispatch server this checkout builds, on a
// database of its own, publishing its events on the script's broker, with its parent-status rule
// off, since production's cut-over runs before DISPATCH_PARENT_STATUS_RULE is turned on (k33) and
// the release before's lingering trees hand over children of a closed root, which the rule refuses;
// the daemon's one database, which both
// releases are given as it is; and the project with its repository. The release before runs first
// in a process of its own (TestTreeCutoverReleaseBefore names what it leaves), and this release then
// boots in this process on the same database, its fake runtime one of this process's.
//
// Dispatch's metrics have no route: they place an issue in a tree by the one rule the board reads
// (internal/dispatch/legion's TreeRoot), so the board's cards stand for both.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// treeBeforeDriverEnv names the release before's driver binary, which the script builds from the
// release before's tree with tree_release_before_live_test.go laid over it.
const treeBeforeDriverEnv = "LEGION_E2E_BEFORE_DRIVER"

// treeRig is the rig of a proof of the cut-over or the rollback: the broker, the checkout, the
// release before's driver, the scratch Dispatch, and the world file both drivers read.
type treeRig struct {
	t       *testing.T
	natsURL string
	before  string
	path    string
}

// newTreeRig takes the script's inputs, starts the scratch Dispatch from the checkout with its
// parent-status rule set to rule, makes the daemon's database and the project, and writes the
// world. With no broker named it skips, as every live proof of this package does.
func newTreeRig(t *testing.T, rule string) *treeRig {
	t.Helper()
	natsURL := os.Getenv(liveNATSURLEnv)
	if natsURL == "" {
		t.Skipf("%s is unset: scripts/e2e/tree-cutover.sh and tree-rollback.sh start a nats-server and set it", liveNATSURLEnv)
	}
	before := os.Getenv(treeBeforeDriverEnv)
	if before == "" {
		t.Fatalf("%s is unset: the script builds the release before's driver and names it there", treeBeforeDriverEnv)
	}
	root := os.Getenv(testbuild.RootEnv)
	if root == "" {
		t.Fatalf("%s is unset: the script names the checkout Dispatch is built from", testbuild.RootEnv)
	}
	var log *os.File
	if daemonLog := os.Getenv(liveDaemonLogEnv); daemonLog != "" {
		file, err := os.Create(filepath.Join(filepath.Dir(daemonLog), "dispatch.log"))
		if err != nil {
			t.Fatalf("create Dispatch's log: %v", err)
		}
		t.Cleanup(func() { _ = file.Close() })
		log = file
	}
	options := scratchDispatchOptions{natsURL: natsURL, root: root, env: []string{parentStatusRule + "=" + rule}}
	if log != nil {
		options.log = log
	}
	scratch := startScratchDispatchOn(t, options)
	world := &treeWorld{Project: "T" + randomSuffix(t)[:9], DSN: blockersDatabase(t), DispatchURL: scratch.base}
	dispatchCall(t, scratch.base, http.MethodPost, "/api/v1/projects", map[string]string{"key": world.Project, "name": "Tree proof"}, nil)
	// The project's default repository, through the route that holds it since #4457: Dispatch
	// writes no repository mapping of its own (GET /api/v1/settings/repo-projects is read only).
	dispatchCall(t, scratch.base, http.MethodPut, "/api/v1/projects/"+world.Project+"/default-repository",
		map[string]any{"repo": "acme/widgets"}, nil)
	path := filepath.Join(t.TempDir(), "world.json")
	world.save(t, path)
	return &treeRig{t: t, natsURL: natsURL, before: before, path: path}
}

// runBefore runs test of the release before's driver in a process of its own over the world, and
// fails unless it passed. Its output is this test's log.
func (g *treeRig) runBefore(test string) *treeWorld {
	t := g.t
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, g.before, "-test.run", "^"+test+"$", "-test.count=1", "-test.v", "-test.timeout=14m")
	command.WaitDelay = 10 * time.Second
	command.Env = append(os.Environ(), treeWorldEnv+"="+g.path)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	t.Logf("the release before's %s:\n%s", test, output.String())
	if err != nil || !strings.Contains(output.String(), "--- PASS: "+test+" ") {
		t.Fatalf("the release before's %s did not pass (%v); its output is above", test, err)
	}
	return readTreeWorld(t, g.path)
}

// head is this release's daemon over the world's rig, not booted. tune, when set, adjusts its
// test overrides at each boot.
func (g *treeRig) head(world *treeWorld) *treeDaemon {
	t := g.t
	t.Helper()
	pool := openTreePool(t, world.DSN)
	return &treeDaemon{t: t, world: world, cfg: treeConfig(t, world, g.natsURL), pool: pool, logs: &syncBuffer{}, registered: map[string]bool{}}
}

// bootStoppedAfter boots this release on the world's database and stops it after step of its first
// boot's conversion, as a crash after that step does: the boot returns the stop, having recorded
// the step.
func (r *treeDaemon) bootStoppedAfter(step int) {
	t := r.t
	t.Helper()
	rebindHeldPorts(t, &r.cfg)
	o := r.overrides()
	o.afterConversionStep = func(n int) error {
		if n == step {
			return errStoppedAfterStep
		}
		return nil
	}
	logs := &syncBuffer{}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	err := run(ctx, r.cfg, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), o)
	if path := os.Getenv(liveDaemonLogEnv); path != "" {
		appendTreeLog(t, path, fmt.Sprintf("this release, stopped after step %d", step), logs.String())
	}
	if err == nil || !strings.Contains(err.Error(), errStoppedAfterStep.Error()) {
		t.Fatalf("the boot stopped after step %d of the conversion ended with %v, want the stop", step, err)
	}
	var done int
	if err := r.pool.QueryRow(t.Context(), `select step from tree_conversions where project = $1`, r.world.Project).Scan(&done); err != nil || done != step {
		t.Fatalf("after the boot stopped after step %d the conversion records step %d (%v)", step, done, err)
	}
}

// treeKept is what the cut-over keeps of the release before: each claim with its session, each
// session-name reservation, each slot, each production check, and each run.
type treeKept struct {
	claims   map[string]string
	names    []string
	slots    map[string]string
	checks   []string
	runs     map[string]string
	deadline time.Time
}

// keptOf reads what the cut-over must keep. deadline is the linger deadline of root, the root of
// the lingering tree.
func keptOf(t *testing.T, pool *store.Pool, root string) treeKept {
	t.Helper()
	kept := treeKept{
		claims: treeRows(t, pool, `select token, concat_ws(' ', issue, role, session, session_file) from claims order by token`),
		names:  slices.Collect(maps.Values(treeRows(t, pool, `select concat_ws(' ', project, kind, name), concat_ws(' ', project, kind, name, claim) from claim_sessions`))),
		slots:  treeRows(t, pool, `select issue, concat_ws(' ', index, admitted_at) from slots`),
		runs:   treeRows(t, pool, `select key, concat_ws(' ', generation, phase) from issues`),
	}
	kept.checks = slices.Collect(maps.Values(treeRows(t, pool, `select issue, concat_ws(' ', issue, project, tree, repo, pr_number, merge_sha,
		opened_at, bound_at, role, looks, closed_at) from production_checks`)))
	slices.Sort(kept.names)
	slices.Sort(kept.checks)
	if err := pool.QueryRow(t.Context(), `select linger_until from issues where key = $1`, root).Scan(&kept.deadline); err != nil {
		t.Fatalf("read the linger deadline of %s: %v", root, err)
	}
	return kept
}

// treeRows is the two text columns of each row query answers, keyed by the first.
func treeRows(t *testing.T, pool *store.Pool, query string, args ...any) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), query, args...)
	if err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	defer rows.Close()
	answer := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatalf("read %q: %v", query, err)
		}
		answer[key] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return answer
}

// treeCount is the one count query answers.
func treeCount(t *testing.T, pool *store.Pool, query string, args ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return count
}

// treeStep runs one step of a tree proof between its `== name` and `ok name (<s>s)` lines, as the
// repository's live proofs print them (liveProof.step).
func treeStep(name string, body func()) {
	started := time.Now()
	fmt.Printf("== %s\n", name)
	body()
	fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
}

// treeNote prints what a step drove or read, under its heading.
func treeNote(format string, args ...any) {
	fmt.Printf("   "+format+"\n", args...)
}

// treeNewestEvent is the newest event of kind on key in Dispatch's event log, which must hold one.
func treeNewestEvent(t *testing.T, world *treeWorld, key, kind string) scratchEvent {
	t.Helper()
	var events []scratchEvent
	dispatchCallAs(t, world.DispatchURL, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, &events, asSession)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == kind {
			return events[i]
		}
	}
	t.Fatalf("the event log of %s holds no %s event: %+v", key, kind, events)
	return scratchEvent{}
}

// treeHandoff writes the handoff record of key's phase word on Dispatch under session, as a role's
// `dispatch handoff-write` does, and answers its id and version.
func treeHandoff(t *testing.T, world *treeWorld, key, word, session string) (string, int) {
	t.Helper()
	body := fmt.Sprintf(`{"schemaVersion": 1, "phase": %q, "issue": %q, "summary": "the %s of the tree proof", "proof": [{"criterion": "live", "observed": "written"}]}`,
		word, key, word)
	status, answer := dispatchAnswerWith(t, world.DispatchURL, http.MethodPost, "/api/v1/issues/"+key+"/handoffs/"+word,
		map[string]any{"actor": map[string]string{"kind": "session", "id": session}, "body": body}, bearing(scratchAgentToken))
	var written struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	if status != http.StatusOK && status != http.StatusCreated || json.Unmarshal(answer, &written) != nil || written.ID == "" {
		t.Fatalf("the %s handoff of %s as %s = %d: %s", word, key, session, status, answer)
	}
	return written.ID, written.Version
}

// treeBoard waits for Dispatch's sampler to read the daemon's state at or after since, and answers
// the tree each card of the project's Legion board names, by its issue.
func treeBoard(t *testing.T, world *treeWorld, since time.Time) map[string]string {
	t.Helper()
	var board struct {
		Header struct {
			ReadAt *time.Time `json:"read_at"`
			ReadOK bool       `json:"read_ok"`
		} `json:"header"`
		Columns []struct {
			Cards []struct {
				Key  string `json:"key"`
				Tree string `json:"tree"`
			} `json:"cards"`
		} `json:"columns"`
	}
	// The sampler reads each configured daemon once a minute (internal/dispatch/legion's
	// SampleInterval), so the first read after the daemon is named comes within a minute.
	if !testwait.Holds(testwait.Bounded(t, 3*time.Minute, 30*time.Second), time.Second, func() bool {
		dispatchCall(t, world.DispatchURL, http.MethodGet, "/api/v1/projects/"+world.Project+"/legion/board", nil, &board)
		return board.Header.ReadOK && board.Header.ReadAt != nil && !board.Header.ReadAt.Before(since)
	}) {
		t.Fatalf("Dispatch's sampler read no state of the daemon since %s: %+v", since, board.Header)
	}
	cards := map[string]string{}
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			cards[card.Key] = card.Tree
		}
	}
	return cards
}

// treeDump is every table the conversion writes, as text (conversionDump), left out of the outbox
// the publications of current state each boot queues and Dispatch takes, and the state lane's rows
// (record.LaneState): each boot queues a state_boot row of its own run, and what a state change
// reports of an issue's phase and tree and of its claims stands in the issues and claims tables
// the dump compares.
func treeDump(t *testing.T, pool *store.Pool) map[string][]string {
	t.Helper()
	dump := conversionDump(t, pool)
	dump["outbox"] = slices.DeleteFunc(dump["outbox"], func(row string) bool {
		kind, _, _ := strings.Cut(row, " ")
		return kind == string(record.OutboxKindRoleHoldersPublish) || kind == string(record.OutboxKindDesignGatesPublish) ||
			kind == string(record.OutboxKindGatedStepsPublish) || record.LaneOf(record.OutboxKind(kind)) == record.LaneState
	})
	return dump
}

// drained waits until the outbox has tried every row that has come due: the publications a boot
// queues taken by Dispatch, and every row that cannot be delivered (a notice, whose Envoy address
// answers nothing here) tried at least once. The state lane's rows are left out (notStateRows):
// the lane publishes them strictly in sequence (record.LaneState), so its oldest row, which that
// Envoy address never takes, is retried for ever, and every state row behind it is held in SQL,
// never leased, and so never tried.
func (r *treeDaemon) drained() {
	r.t.Helper()
	pool := store.NewPool(r.pool)
	testwait.Eventually(r.t, "the outbox to try every row that came due", func() bool {
		return treeCount(r.t, pool, `select count(*) from outbox where next_at <= now() and attempts = 0 and `+notStateRows) == 0
	})
}

// oldPluginOperations drives each operation of a deployed plugin of the oldest contract in the
// fleet against this release, through that plugin's own client (oldPluginCall), as the planner of
// issue that plugin registered with the release before: ready, activity, a credential (a grant),
// the planner's handoff record and its phase completion, which moves issue to implementing, the
// registration of the implementer this release then launches, and that implementer's exit.
func (r *treeDaemon) oldPluginOperations(issue string) {
	t := r.t
	t.Helper()
	planner := r.world.token(t, issue, claim.RolePlanner)
	held, ok := r.world.Claims[planner]
	if !ok || !held.OldPlugin {
		t.Fatalf("the oldest contract's plugin registered no planner of %s: %+v", issue, held)
	}
	call := func(method string, input map[string]any) json.RawMessage {
		t.Helper()
		answer := oldPluginCall(t, r.d, method, input)
		if !answer.OK {
			t.Fatalf("this release answered the oldest contract's %s with %d %s: %s", method, answer.Status, answer.Code, answer.Error)
		}
		treeNote("%s answered %s", method, answer.Answer)
		return answer.Answer
	}
	claimOf := func(token claim.Token, held treeClaim) map[string]any {
		return map[string]any{"claimToken": string(token), "sessionId": held.Session, "secret": held.Secret}
	}
	ready := claimOf(planner, held)
	ready["generation"] = held.Generation
	call("ready", ready)
	activity := claimOf(planner, held)
	activity["activity"] = map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano),
		"backgroundWork": map[string]any{"running": false, "kinds": []string{}}, "phase": map[string]any{"nextReminderAt": nil}}
	call("activity", activity)
	var grant struct {
		GrantID string `json:"grantId"`
	}
	if err := json.Unmarshal(call("grant", map[string]any{"sessionId": held.Session, "secret": held.Secret, "tree": held.Tree, "issue": held.Issue}), &grant); err != nil || grant.GrantID == "" {
		t.Fatalf("the oldest contract's grant answered no grant id (%v)", err)
	}
	id, version := treeHandoff(t, r.world, issue, "plan", held.Session)
	treeNote("the planner's handoff record %s, version %d", id, version)
	call("handoffComplete", map[string]any{"grantId": grant.GrantID, "summary": "planned", "verdict": "", "handoffId": id, "handoffVersion": version})
	settled(t, r.d, "move "+issue+" to implementing", func(s api.State) bool { return s.Issues[issue].Phase == phase.Implementing })
	implementer := r.world.token(t, issue, claim.RoleImplementer)
	registered := r.oldPluginRegister(r.awaitLaunch(implementer))
	treeNote("the implementer of %s registered under tree %s", issue, registered.Tree)
	exit := claimOf(implementer, r.world.Claims[implementer])
	exit["generation"] = registered.Generation
	exit["reason"] = "the agent ended its session"
	call("exit", exit)
}

// TestTheTreeCutoverLive is k31. Each step prints `== <step>`, what it observed and `ok <step>`.
func TestTheTreeCutoverLive(t *testing.T) {
	rig := newTreeRig(t, "off")
	step := treeStep
	var world *treeWorld
	step("the release before", func() { world = rig.runBefore("TestTreeCutoverReleaseBefore") })
	r := rig.head(world)
	pool := store.NewPool(r.pool)
	key := func(name string) string { return world.key(t, name) }
	a, a1, a2, a3, b, c, e, l, l1, l2, l3 := key("A"), key("A1"), key("A2"), key("A3"), key("B"), key("C"), key("E"), key("L"), key("L1"),
		key("L2"), key("L3")
	kept := keptOf(t, pool, l)
	landed := treeCount(t, pool, `select count(*) from handovers where issue = $1`, l1)
	if landed != 1 {
		t.Fatalf("the release before left %d hand-overs of %s, want 1", landed, l1)
	}

	var converted time.Time
	step("a boot stopped after each step of the conversion, and booted again", func() {
		for n := convertFences; n <= convertRecord; n++ {
			r.bootStoppedAfter(n)
			fmt.Printf("   the boot stopped after step %d, which the conversion records\n", n)
		}
		if err := r.pool.QueryRow(t.Context(), `select converted_at from tree_conversions where project = $1`, world.Project).
			Scan(&converted); err != nil {
			t.Fatalf("the conversion records no end after its last step: %v", err)
		}
		fmt.Printf("   the conversion ended at %s\n", converted.UTC().Format(time.RFC3339Nano))
	})

	var state api.State
	var body string
	step("this release booted once more", func() {
		rebindHeldPorts(t, &r.cfg)
		r.boot("this release, once converted")
		r.caughtUp()
		state, body = r.state()
		if state.Daemon.ConvertedAt == nil || !state.Daemon.ConvertedAt.Equal(converted) {
			t.Fatalf("the state's daemon.convertedAt is %v, want %s, the first finished conversion's\nstate %s", state.Daemon.ConvertedAt, converted, body)
		}
		fmt.Printf("   daemon.convertedAt %s\n", state.Daemon.ConvertedAt.UTC().Format(time.RFC3339Nano))
	})

	step("every run, claim, session, session-name reservation, slot and production check kept", func() {
		now := keptOf(t, pool, l)
		for token, held := range kept.claims {
			if now.claims[token] != held {
				t.Fatalf("the claim %s reads %q after the cut-over, want %q as the release before left it", token, now.claims[token], held)
			}
		}
		if !slices.Equal(now.names, kept.names) {
			t.Fatalf("the session-name reservations read %q after the cut-over, want %q", now.names, kept.names)
		}
		if !slices.Equal(now.checks, kept.checks) {
			t.Fatalf("the production checks read %q after the cut-over, want %q", now.checks, kept.checks)
		}
		for issue, slot := range kept.slots {
			if issue != l1 && now.slots[issue] != slot {
				t.Fatalf("the slot of %s reads %q after the cut-over, want %q", issue, now.slots[issue], slot)
			}
		}
		for issue, run := range kept.runs {
			if issue != l1 && now.runs[issue] != run {
				t.Fatalf("the run of %s reads %q after the cut-over, want %q", issue, now.runs[issue], run)
			}
		}
		if state.Issues[a3].Check == nil {
			t.Fatalf("the state shows no production check of %s\nstate %s", a3, body)
		}
		fmt.Printf("   %d claims with their sessions, %d session names, %d slots, %d runs and the production check of %s, as the release before left them\n",
			len(kept.claims), len(kept.names), len(kept.slots), len(kept.runs), a3)
	})

	step("the linger deadline is the retention deadline of each issue it covered", func() {
		for _, issue := range []string{l, l3} {
			var until time.Time
			if err := r.pool.QueryRow(t.Context(), `select retained_until from issues where key = $1`, issue).Scan(&until); err != nil || !until.Equal(kept.deadline) {
				t.Fatalf("%s is retained until %v (%v), want %s, the linger deadline", issue, until, err, kept.deadline)
			}
			if n := treeCount(t, pool, `select count(*) from outbox where kind = 'retention_end' and issue = $1 and next_at = $2`, issue, kept.deadline); n != 1 {
				t.Fatalf("%s owes %d retention_end rows at %s, want 1", issue, n, kept.deadline)
			}
		}
		for _, issue := range []string{l1, l2} {
			if n := treeCount(t, pool, `select count(*) from issues where key = $1 and retained_until is not null`, issue); n != 0 {
				t.Fatalf("%s, at work, is retained", issue)
			}
		}
		fmt.Printf("   %s and %s retained until %s, each with its retention_end row\n", l, l3, kept.deadline.UTC().Format(time.RFC3339))
	})

	step("the reserved cleanup is the reservation of each issue of its tree", func() {
		// Step 5 reserves each issue the tree's cleanup covers, L and L3; step 9 reserves L2, whose
		// hand-over's write never landed, and leaves L1, whose write landed, a root of its own whose
		// lifecycle is open (record.ReserveIssuesOfTreeCleanups, convertHandOvers).
		for issue, want := range map[string]int{l: 1, l2: 1, l3: 1, l1: 0} {
			if n := treeCount(t, pool, `select count(*) from issue_lifecycles where issue = $1 and cleanup_started`, issue); n != want {
				t.Fatalf("%s's own lifecycle holds %d reserved cleanups after the cut-over, want %d", issue, n, want)
			}
		}
		fmt.Printf("   the cleanups of %s, %s and %s are reserved on their own lifecycles; %s's is not\n", l, l2, l3, l1)
	})

	step("the operator's hold of a tree is a hold of each issue of it", func() {
		if n := treeCount(t, pool, `select count(*) from issue_lifecycles where issue = $1 and authority = 'operator'`, e); n != 1 {
			t.Fatalf("the operator's hold of %s's tree is no hold of %s", e, e)
		}
		fmt.Printf("   %s's lifecycle is the operator's\n", e)
	})

	step("the landed hand-over is a root of its own and its later event the daemon's own write", func() {
		var generation, carried uint64
		var holds, taken bool
		var status string
		if err := r.pool.QueryRow(t.Context(), `select generation, carried_generation, holds_gate, taken_back, status from issues where key = $1`, l1).
			Scan(&generation, &carried, &holds, &taken, &status); err != nil {
			t.Fatalf("read %s: %v", l1, err)
		}
		var before uint64
		if _, err := fmt.Sscanf(kept.runs[l1], "%d", &before); err != nil {
			t.Fatalf("read the run %s of %s: %v", kept.runs[l1], l1, err)
		}
		if carried != before+1 || generation != before+1 || !holds || !taken || status != "todo" {
			t.Fatalf("%s reads generation %d, carried %d, holds its gate %t, taken back %t, status %s; want a root of its own carrying run %d into %d",
				l1, generation, carried, holds, taken, status, before, before+1)
		}
		if n := treeCount(t, pool, `select count(*) from handovers`); n != 0 {
			t.Fatalf("%d hand-over rows are left after the cut-over", n)
		}
		event := treeNewestEvent(t, world, l1, "issue.updated")
		testwait.Eventually(t, "the daemon to apply the echo of "+l1+"'s hand-over write", func() bool {
			return treeCount(t, pool, `select count(*) from processed_events where source = 'dispatch' and event_id = $1`, event.factID()) == 1
		})
		if lines := loggedLines(t, r.logs.String(), "admission: queued a person's handover"); slices.ContainsFunc(lines, func(line map[string]any) bool {
			return line["issue"] == l1
		}) {
			t.Fatalf("the daemon read the echo of %s's hand-over write as a person's handover", l1)
		}
		fmt.Printf("   %s carries run %d into %d as a root of its own; the echo %s by %s changed nothing\n", l1, before, before+1, event.factID(), event.Actor.ID)
	})

	step("each issue in Legion's work has one place, and the root under a parent at work is told once", func() {
		state, body = r.state()
		for _, issue := range []string{a, a1, a2, b, c, e, l1, l2} {
			// An issue that holds a row and waits in the line stands in active and in waiting at
			// once (api.Admission.Waiting); held is neither of them.
			places := placed(state, issue)
			if len(places) == 0 || (slices.Contains(places, "held") && len(places) > 1) {
				t.Fatalf("%s is in Legion's work and in %v of the state's lists, want one place\nstate %s", issue, places, body)
			}
			fmt.Printf("   %s in %v under tree %s\n", issue, places, state.Issues[issue].Tree)
		}
		for issue, tree := range map[string]string{a1: a, a2: a2, b: b, l1: l1} {
			if got := state.Issues[issue].Tree; got != tree {
				t.Fatalf("the state's tree of %s is %s, want its gate root %s\nstate %s", issue, got, tree, body)
			}
		}
		told := treeCount(t, pool, `select count(*) from outbox where kind = 'notice' and payload->>'kind' = 'reparented' and payload::text like '%' || $1 || '%'`, b)
		if told != 2 {
			t.Fatalf("the daemon queued %d reparented notices of %s, want one to its own architect and one to %s's", told, b, a)
		}
	})

	// A2 is the shape the release before left INFRA-533 in: a child of A at work whose parent the
	// person took off, which that release kept in A's tree, under A's sign-off, with no gate of its
	// own (dispatch://INFRA-4730's k49 reads INFRA-533 after the deploy).
	step("an issue the release before kept in another tree with no Dispatch parent is a root of its own", func() {
		// It reads as a root: its gate root on the Dispatch chain is itself (record.Chains.StateTree).
		state, body = r.state()
		if got := state.Issues[a2].Tree; got != a2 || !slices.Contains(state.Admission.Trees, a2) {
			t.Fatalf("the state's tree of %s is %s and admission.trees is %v, want %s a root of its own\nstate %s", a2, got, state.Admission.Trees, a2, body)
		}
		treeNote("%s's tree is %s, admission.trees names it, and it stands in %v in %s", a2, a2, placed(state, a2), state.Issues[a2].Phase)

		// Its architect, which the release before launched under A, is relaunched in its own
		// session under A2 (supervision.relaunchUnderGateRoots, supervise.GateRootMoved), and its
		// registration answers A2 as its tree, which its plugin holds its register_gate to.
		architect := world.token(t, a2, claim.RoleArchitect)
		held, ok := world.Claims[architect]
		if !ok {
			t.Fatalf("the release before registered no architect of %s, which its first phase start launches", a2)
		}
		var session, sessionFile string
		if err := r.pool.QueryRow(t.Context(), `select session, session_file from claims where token = $1`, string(architect)).
			Scan(&session, &sessionFile); err != nil || session != held.Session {
			t.Fatalf("the claim %s holds the session %q (%v), want %q, the one the release before registered", architect, session, err, held.Session)
		}
		var relaunch runtime.SpawnSpec
		testwait.Eventually(t, "the daemon to relaunch "+string(architect)+" in its session under "+a2, func() bool {
			for _, call := range r.rt.Calls() {
				if call.Method == "Resume" && call.Spec.Claim == architect && call.Spec.Tree == a2 {
					relaunch = call.Spec
					return true
				}
			}
			return false
		})
		if relaunch.ResumeSessionFile != sessionFile && relaunch.ResumeSession != session {
			t.Fatalf("the relaunch of %s resumes the session %q (file %q), want its own, %q (file %q)", architect, relaunch.ResumeSession,
				relaunch.ResumeSessionFile, session, sessionFile)
		}
		registered := r.register(relaunch)
		if registered.Tree != a2 {
			t.Fatalf("the relaunched architect of %s registered under the tree %s, want %s", a2, registered.Tree, a2)
		}
		treeNote("%s, launched under %s by the release before, resumed its session %s with LEGION_TREE %s and registered under the tree %s",
			architect, held.Tree, session, relaunch.Tree, registered.Tree)

		// The gate the merger's hold reads is A2's own, and the state shows no approved version of
		// it. The hold (the outbox's mergeHold) reads the gate root on the Dispatch chain, as the
		// state's tree does. A2 holds its own gate and none of its rows has opened, so a step the
		// deployment gates that has no row holds its merger (classify.MergeHold).
		if gate := state.Issues[a2].DesignGate; gate != nil && gate.ApprovedVersion != nil {
			t.Fatalf("the state shows the design gate of %s approved at version %d, want no approved version: nobody designed %s as a root\nstate %s",
				a2, *gate.ApprovedVersion, a2, body)
		}
		issue, err := store.ReadOnly(t.Context(), pool, "read "+a2, func(ctx context.Context, tx pgx.Tx) (*record.Issue, error) {
			return record.NewStore().Issue(ctx, tx, a2)
		})
		if err != nil || issue == nil {
			t.Fatalf("read %s: %v (recorded %t)", a2, err, issue != nil)
		}
		hold := &outbox{pool: pool, records: record.NewStore(), gatedSteps: r.cfg.Gates.Steps()}
		gateRoot, step, stepRegistered, err := hold.mergeHold(t.Context(), *issue)
		if err != nil || gateRoot != a2 {
			t.Fatalf("the merger's hold of %s reads the design gate of %q (%v), want the gate of %s itself, not of %s", a2, gateRoot, err, a2, a)
		}
		treeNote("the state shows no approved version of %s's own gate, and the merger's hold reads the gate of %s (held by step %q, registered %t)", a2, gateRoot, step, stepRegistered)
		treeNote("gates.design is off on this rig. It gates no step, so that hold holds no merger here")
		treeNote("%s holds no row, and none of its steps has opened. Under root-issues its merger waits for the spec step, which has no row, until its architect registers that document and a person approves it", a2)

		// A's sign-off and close wait on nothing of A2's: none of the three readers of the hold on
		// them (workflow.Engine.acceptanceHeld) names it.
		type acceptanceReading struct {
			beneath []string
			resting []record.RestingRoot
			open    []record.OpenChild
		}
		reading, err := store.ReadOnly(t.Context(), pool, "read what holds the acceptance of "+a, func(ctx context.Context, tx pgx.Tx) (acceptanceReading, error) {
			records := record.NewStore()
			root, err := records.Issue(ctx, tx, a)
			if err != nil {
				return acceptanceReading{}, err
			}
			if root == nil {
				return acceptanceReading{}, fmt.Errorf("the daemon records no %s", a)
			}
			chains, err := record.ReadChains(ctx, tx, records, root.Project, []string{a})
			if err != nil {
				return acceptanceReading{}, err
			}
			resting, err := record.RestingRoots(ctx, tx, records, a)
			if err != nil {
				return acceptanceReading{}, err
			}
			waits, err := record.OpenChildWaits(ctx, tx, records, world.Project, []record.Issue{*root})
			return acceptanceReading{beneath: record.OpenBeneath(chains, a), resting: resting, open: waits[a].Open}, err
		})
		if err != nil {
			t.Fatalf("read what holds the acceptance of %s: %v", a, err)
		}
		var resting, open []string
		for _, root := range reading.resting {
			resting = append(resting, root.Key)
		}
		for _, child := range reading.open {
			open = append(open, child.Key)
		}
		if slices.Contains(reading.beneath, a2) || slices.Contains(resting, a2) || slices.Contains(open, a2) {
			t.Fatalf("the hold on the acceptance of %s names %s: beneath %v, resting roots %v, open children %v; want nothing of it", a, a2, reading.beneath, resting, open)
		}
		treeNote("the acceptance of %s waits on beneath %v, resting roots %v and open children %v: nothing of %s", a, reading.beneath, resting, open, a2)
	})

	step("the undelivered decision is named by the next catch-up", func() {
		r.ready(r.world.token(t, a, claim.RoleArchitect))
		var row int64
		testwait.Eventually(t, "a catch-up of "+a+"'s architect to name the decision kept on "+a1, func() bool {
			return r.pool.QueryRow(t.Context(), `select o.id from parked_decisions p join outbox o on o.id = p.claimed_by
				where p.issue = $1 and o.payload::text like '%' || $2 || '%'`, a1, treeDecision).Scan(&row) == nil
		})
		fmt.Printf("   outbox row %d names the decision kept on %s\n", row, a1)
	})

	step("the oldest contract's plugin is answered", func() {
		r.oldPluginOperations(a1)
	})

	step("Dispatch's Legion board groups each issue by its gate root", func() {
		named := time.Now().UTC()
		dispatchCall(t, world.DispatchURL, http.MethodPut, "/api/v1/projects/"+world.Project+"/legion/daemon",
			map[string]any{"state_url": r.d.base}, nil)
		cards := treeBoard(t, world, named)
		state, body = r.state()
		for _, issue := range []string{a1, b, l1} {
			card, ok := cards[issue]
			if !ok {
				t.Fatalf("Dispatch's Legion board holds no card of %s: %v", issue, cards)
			}
			if card != state.Issues[issue].Tree {
				t.Fatalf("Dispatch's Legion board puts %s in the tree of %s, want %s, its gate root", issue, card, state.Issues[issue].Tree)
			}
			fmt.Printf("   the card of %s names the tree %s\n", issue, card)
		}
	})

	step("the last boot changes nothing", func() {
		// The boot before stops once it has done the admission work it owes: it held roots for its
		// Dispatch consumer to catch up (admit's Reconcile), and its hold poll admits them at the
		// release it makes once the consumer has caught up, so a boot stopped before that release
		// leaves the last boot to admit them, which writes the issues, claims and outbox the dump
		// compares. The held set lives in admission's memory, so the boot's log says it: no hold, or
		// a release after the last hold, since a repository's recovery can hold again in one boot.
		testwait.Eventually(t, "this boot to release the roots it held for the Dispatch consumer", func() bool {
			logs := r.logs.String()
			held := strings.LastIndex(logs, holdingMessage)
			return held < 0 || held < strings.LastIndex(logs, releaseMessage)
		})
		r.caughtUp()
		r.drained()
		r.d.stop()
		before := treeDump(t, pool)
		rebindHeldPorts(t, &r.cfg)
		r.boot("this release, booted last")
		r.caughtUp()
		r.drained()
		r.d.stop()
		after := treeDump(t, pool)
		if !maps.EqualFunc(before, after, slices.Equal[[]string]) {
			for table := range before {
				if !slices.Equal(before[table], after[table]) {
					t.Errorf("the last boot changed %s:\nbefore %q\nafter  %q", table, before[table], after[table])
				}
			}
			t.FailNow()
		}
		var again time.Time
		if err := r.pool.QueryRow(t.Context(), `select converted_at from tree_conversions where project = $1`, world.Project).Scan(&again); err != nil || !again.Equal(converted) {
			t.Fatalf("the conversion's end reads %v (%v) after the later boots, want %s", again, err, converted)
		}
		fmt.Printf("   every table the conversion writes reads the same after the last boot; the conversion still ended at %s\n", converted.UTC().Format(time.RFC3339Nano))
	})
	fmt.Println("tree cutover: PASS")
}
