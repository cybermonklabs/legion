package workflow

// One delivery at each move of the design gate (dispatch://INFRA-2040 R8, D16 of its plan): every
// merger of the tree in merging gets the one delivery gateDelivery chooses, in the transaction of
// the fact that moved the gate (putGateTold, deliverGateMove). A live session gets the move's role
// message, and a merger's closing text holds the merge; a merger with no live session gets the
// gate's own start, unless a queued start that carries a task runs first; a merger that runs
// nothing gets nothing.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// gateDeliveryLine is the engine's line for each merger's delivery of a move of the gate.
const gateDeliveryLine = `msg="workflow: a held merger's gate delivery"`

// The choice of each merger's one delivery follows its claim and its queued starts (gateDelivery):
// nothing for a merger that runs nothing, the role message for a live session whose pod no
// release is taking, and for every other claim a start — a queued start that carries a task and
// runs first, else the gate's own. At the close only a queued start that names no phase counts,
// since the fence holds the phase's start; at the opening any start with a task counts. A taskless
// start and a task the claim holds and has not taken count for nothing.
func TestTheGateDeliveryOfAMergerFollowsItsClaimAndItsQueuedStarts(t *testing.T) {
	held := func(state supervise.ClaimState, by supervise.SuspendSource) *record.RoleClaim {
		return &record.RoleClaim{State: state, SuspendedBy: by}
	}
	queued := func(id int64, op record.SuperviseOp, p phase.Phase, task string) record.QueuedSupervise {
		return record.QueuedSupervise{ID: id, Request: record.SuperviseRequest{Op: op, Role: claim.RoleMerger, Phase: p, Task: task}}
	}
	released := held(supervise.StateSuspended, supervise.SuspendedByReleased)
	untaken := held(supervise.StateLaunching, "")
	untaken.Pending = &supervise.Delivery{ID: "outbox:3", Task: "The wake is a CI settlement on your pull request's head: head1"}
	for _, tc := range []struct {
		name      string
		run       record.RoleRun
		releasing bool
		// atClose and atOpening are the choice at each move, and row the queued start that is the
		// delivery where the choice is one.
		atClose, atOpening gateChoice
		row                int64
	}{
		{name: "no claim", atClose: gateNothing, atOpening: gateNothing},
		{name: "a failed claim", run: record.RoleRun{Claim: held(supervise.StateFailed, "")}, atClose: gateNothing, atOpening: gateNothing},
		{name: "a retired claim", run: record.RoleRun{Claim: held(supervise.StateRetired, "")}, atClose: gateNothing, atOpening: gateNothing},
		{name: "a claim an operator suspended", run: record.RoleRun{Claim: held(supervise.StateSuspended, supervise.SuspendedByOperator)},
			atClose: gateNothing, atOpening: gateNothing},
		{name: "a ready session", run: record.RoleRun{Claim: held(supervise.StateReady, "")}, atClose: gateRoleMessage, atOpening: gateRoleMessage},
		{name: "an idle session", run: record.RoleRun{Claim: held(supervise.StateIdle, "")}, atClose: gateRoleMessage, atOpening: gateRoleMessage},
		{name: "a working session", run: record.RoleRun{Claim: held(supervise.StateWorking, "")}, atClose: gateRoleMessage, atOpening: gateRoleMessage},
		{name: "a live session whose queue holds a wake's start", run: record.RoleRun{Claim: held(supervise.StateIdle, ""),
			Queued: []record.QueuedSupervise{queued(7, "start", "", "a wake's task")}}, atClose: gateRoleMessage, atOpening: gateRoleMessage},
		{name: "an idle session whose pod's release is in progress", run: record.RoleRun{Claim: held(supervise.StateIdle, "")}, releasing: true,
			atClose: gateOwnStart, atOpening: gateOwnStart},
		{name: "a claim the idle release took", run: record.RoleRun{Claim: released}, atClose: gateOwnStart, atOpening: gateOwnStart},
		{name: "a claim the daemon suspended", run: record.RoleRun{Claim: held(supervise.StateSuspended, supervise.SuspendedByDaemon)},
			atClose: gateOwnStart, atOpening: gateOwnStart},
		{name: "a claim its own exit suspended", run: record.RoleRun{Claim: held(supervise.StateSuspended, supervise.SuspendedByExit)},
			atClose: gateOwnStart, atOpening: gateOwnStart},
		{name: "a queued claim", run: record.RoleRun{Claim: held(supervise.StateQueued, "")}, atClose: gateOwnStart, atOpening: gateOwnStart},
		{name: "a launching relaunch", run: record.RoleRun{Claim: held(supervise.StateLaunching, "")}, atClose: gateOwnStart, atOpening: gateOwnStart},
		{name: "a relaunch whose launch is uncertain", run: record.RoleRun{Claim: held(supervise.StateLaunchUncertain, "")},
			atClose: gateOwnStart, atOpening: gateOwnStart},
		{name: "a connecting relaunch", run: record.RoleRun{Claim: held(supervise.StateShimConnected, "")}, atClose: gateOwnStart, atOpening: gateOwnStart},
		{name: "a registered relaunch", run: record.RoleRun{Claim: held(supervise.StateRegistered, "")}, atClose: gateOwnStart, atOpening: gateOwnStart},
		{name: "a relaunch holding a task it has not taken", run: record.RoleRun{Claim: untaken}, atClose: gateOwnStart, atOpening: gateOwnStart},
		{name: "a released claim whose queue holds a wake's start", run: record.RoleRun{Claim: released,
			Queued: []record.QueuedSupervise{queued(7, "start", "", "a wake's task")}}, atClose: gateQueuedStart, atOpening: gateQueuedStart, row: 7},
		{name: "a released claim whose queue holds a start of its phase", run: record.RoleRun{Claim: released,
			Queued: []record.QueuedSupervise{queued(8, "start", phase.Merging, "Merge the head.")}}, atClose: gateOwnStart, atOpening: gateQueuedStart, row: 8},
		{name: "a released claim whose queue holds the resume of a cancelled release", run: record.RoleRun{Claim: released,
			Queued: []record.QueuedSupervise{queued(9, "start", "", "")}}, atClose: gateOwnStart, atOpening: gateOwnStart},
		{name: "a released claim whose queue holds a suspend", run: record.RoleRun{Claim: released,
			Queued: []record.QueuedSupervise{queued(10, "suspend", "", "")}}, atClose: gateOwnStart, atOpening: gateOwnStart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, move := range []struct {
				open bool
				want gateChoice
			}{{open: false, want: tc.atClose}, {open: true, want: tc.atOpening}} {
				choice, row := gateDelivery(tc.run, tc.releasing, move.open)
				wantRow := int64(0)
				if move.want == gateQueuedStart {
					wantRow = tc.row
				}
				if choice != move.want || row != wantRow {
					t.Errorf("gateDelivery at the %s = %s, row %d; want %s, row %d", moveName(move.open), choice, row, move.want, wantRow)
				}
			}
		})
	}
}

// moveName is the name a move of the gate is logged under (deliverGateMove).
func moveName(open bool) string {
	if open {
		return "opened"
	}
	return "closed"
}

// The gate's own task carries the gate's whole message at its send (MergerGateMessage): the
// closing message while the gate holds the merger, which is the closed note every task of the hold
// carries, and the opening message once it stands open, naming the registered gated step whose
// approval stands newest, its document and its version. A task that carries no note — another
// role's, a tree whose gated step has no row — carries no message either.
func TestTheGatesOwnTaskCarriesTheClosingOrTheOpeningMessageAtItsSend(t *testing.T) {
	const root = "LEGION-208"
	merging := record.Issue{Key: "LEGION-209", Tree: root, Project: "LEGION", Phase: phase.Merging, Generation: 3, Status: "in_progress"}
	approved := func(step record.DesignStep, document string, version int, seq int64) record.DesignGate {
		return record.DesignGate{Issue: root, Step: step, ArtifactID: document, LatestVersion: version, ApprovedVersion: &version,
			ApprovedMark: record.EventMark{Seq: seq}, EverOpened: true}
	}
	for _, tc := range []struct {
		name string
		role claim.Role
		gate record.TreeGate
		want supervise.GateNote
	}{
		{name: "the gate closed", role: claim.RoleMerger, gate: closedSpecGate(root), want: supervise.GateNote{
			Text: "The design gate of LEGION-208 closed: version 2 of its spec document (spec-artifact) is with its reviewer. " +
				"Merge nothing, enqueue nothing, and take a pull request you queued back out of the merge queue.",
			State: "closed", Step: "spec", Version: 2}},
		{name: "the gate open", role: claim.RoleMerger, gate: openSpecGate(root), want: supervise.GateNote{
			Text: gateOpenedMessage(root, record.DesignStepSpec, "spec-artifact", 2), State: "open", Step: "spec", Version: 2}},
		{name: "the gate open over two steps, the second step's approval the newest", role: claim.RoleMerger,
			gate: record.TreeGate{Steps: twoSteps(), Rows: map[record.DesignStep]record.DesignGate{
				record.DesignStepCriteria: approved(record.DesignStepCriteria, "criteria-artifact", 5, 4),
				record.DesignStepSpec:     approved(record.DesignStepSpec, "spec-artifact", 3, 9),
			}},
			want: supervise.GateNote{Text: gateOpenedMessage(root, record.DesignStepSpec, "spec-artifact", 3), State: "open", Step: "spec", Version: 3}},
		{name: "a tree whose gated step has no row", role: claim.RoleMerger, gate: record.TreeGate{Steps: []record.DesignStep{record.DesignStepSpec}}},
		{name: "another role's task", role: claim.RoleImplementer, gate: closedSpecGate(root)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergerGateMessage(merging, tc.role, tc.gate); got != tc.want {
				t.Fatalf("MergerGateMessage = %+v, want %+v", got, tc.want)
			}
		})
	}
	opened := MergerGateMessage(merging, claim.RoleMerger, openSpecGate(root)).Text
	if want := "The design gate of LEGION-208 opened: a person approved version 2 of its spec document (spec-artifact), " +
		"and every gated step that someone registered now stands open"; !strings.HasPrefix(opened, want) {
		t.Fatalf("the opening message %q does not open with %q", opened, want)
	}
}

// gateTreeMembers are the members of the gate delivery tests' tree, each in merging, by the claim
// its merger holds: the root's live, two children's released by an idle release, one retired, one
// failed, one an operator suspended, and one with no claim.
const (
	liveMerger      = gatedRoot
	releasedMerger  = "LEGION-209"
	releasedMerger2 = "LEGION-210"
	retiredMerger   = "LEGION-211"
	failedMerger    = "LEGION-212"
	suspendedMerger = "LEGION-213"
	unclaimedMerger = "LEGION-214"
	testingMember   = "LEGION-215"
)

// seedGateDeliveryTree records the tree the gate delivery tests move the gate of: LEGION-208 and
// six children in merging, and one child in testing, under a deployment that gates the spec, the
// spec's version 1 approved; each merger's claim as gateTreeMembers says, and a backstop armed for
// the second released merger and for the merger with no claim. It answers the engine, over the
// store's own pod records, and the apply of a fact through it.
func seedGateDeliveryTree(t *testing.T) (*pgxpool.Pool, wakeRig) {
	t.Helper()
	pool := migratedPool(t)
	member := func(key string, at phase.Phase) record.Issue {
		issue := record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: gatedRoot,
			Project: "LEGION", Title: key, Phase: at, Generation: wakeIssueGeneration, Status: PhaseStatus(at), Rank: "U" + key,
			LastDispatchSeq: 1}
		if key != gatedRoot {
			issue.Parent = new(gatedRoot)
		}
		return issue
	}
	for _, key := range []string{liveMerger, releasedMerger, releasedMerger2, retiredMerger, failedMerger, suspendedMerger, unclaimedMerger} {
		seedIssue(t, pool, member(key, phase.Merging))
	}
	seedIssue(t, pool, member(testingMember, phase.Testing))
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: heldMergerSpec, LatestVersion: 1,
		ApprovedVersion: new(1), EverOpened: true})
	seedTreeLifecycle(t, pool, gatedRoot)
	for key, state := range map[string][2]string{
		liveMerger:      {string(supervise.StateIdle), ""},
		releasedMerger:  {string(supervise.StateSuspended), string(supervise.SuspendedByReleased)},
		releasedMerger2: {string(supervise.StateSuspended), string(supervise.SuspendedByReleased)},
		retiredMerger:   {string(supervise.StateRetired), ""},
		failedMerger:    {string(supervise.StateFailed), ""},
		suspendedMerger: {string(supervise.StateSuspended), string(supervise.SuspendedByOperator)},
	} {
		seedMergerClaim(t, pool, key, supervise.ClaimState(state[0]), supervise.SuspendSource(state[1]))
	}
	armMergerBackstop(t, pool, releasedMerger2)
	armMergerBackstop(t, pool, unclaimedMerger)
	return pool, wakeRigOf(t, pool, config.DesignGateRootIssues.Steps())
}

// mergerToken is the claim token of issue's merger, under the project as the claim tables spell it.
func mergerToken(t *testing.T, issue string) claim.Token {
	t.Helper()
	token, err := claim.NewToken(wakeProjectToken(t), issue, claim.RoleMerger)
	if err != nil {
		t.Fatalf("the merger token of %s: %v", issue, err)
	}
	return token
}

// seedMergerClaim records issue's merger claim in state, suspended by by when it is suspended, on a
// session of its own and serving the issue's run.
func seedMergerClaim(t *testing.T, pool *pgxpool.Pool, issue string, state supervise.ClaimState, by supervise.SuspendSource) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, suspended_by, serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, $3, $4, 'merger', 1, $5, '', $6, $7, $8, 0, 0, 0, 0)`,
		string(mergerToken(t, issue)), wakeProjectToken(t), gatedRoot, issue, "session-"+issue, string(state), string(by),
		int64(wakeIssueGeneration)); err != nil {
		t.Fatalf("seed the merger claim of %s: %v", issue, err)
	}
}

// armMergerBackstop arms the backstop of issue's merger, due before the engine's clock.
func armMergerBackstop(t *testing.T, pool *pgxpool.Pool, issue string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `insert into pod_wake_backstops (project, issue, role, release_id, due_at)
		values ($1, $2, 'merger', 0, $3)`, wakeProjectToken(t), issue, wakeAt.Add(-time.Minute)); err != nil {
		t.Fatalf("arm the backstop of %s's merger: %v", issue, err)
	}
}

// openMergerBackstops is every issue whose merger holds a backstop that has not fired, in order.
func openMergerBackstops(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	return queryStrings(t, pool, `select issue from pod_wake_backstops where role = 'merger' and not fired order by issue`)
}

// mergerResumes is every pod resume of a merger, as issue:kind, in order.
func mergerResumes(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	return queryStrings(t, pool, `select issue || ':' || kind from pod_resumes where role = 'merger' order by id`)
}

// queryStrings is every text the query answers, in its order.
func queryStrings(t *testing.T, pool *pgxpool.Pool, query string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return texts
}

// mergerStart is one supervise start row of a merger: its row id and its request.
type mergerStart struct {
	id      int64
	request record.SuperviseRequest
}

// mergerStartsOf is every supervise start row of issue's merger, oldest first.
func mergerStartsOf(t *testing.T, pool *pgxpool.Pool, issue string) []mergerStart {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, payload from outbox where kind = 'supervise' and issue = $1
		and payload->>'op' = 'start' and payload->>'role' = 'merger' order by id`, issue)
	if err != nil {
		t.Fatalf("read the merger starts of %s: %v", issue, err)
	}
	defer rows.Close()
	var starts []mergerStart
	for rows.Next() {
		var start mergerStart
		var payload []byte
		if err := rows.Scan(&start.id, &payload); err != nil {
			t.Fatalf("read a merger start of %s: %v", issue, err)
		}
		if err := json.Unmarshal(payload, &start.request); err != nil {
			t.Fatalf("decode a merger start of %s: %v", issue, err)
		}
		starts = append(starts, start)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the merger starts of %s: %v", issue, err)
	}
	return starts
}

// wantGateStart asserts issue's merger holds exactly one start, the gate's own: a start of the
// merger's run that names no phase and no entry, carries the gate's task (a wake's, behind the
// wake's preamble, when woke) and the mark that its send puts the gate's message in front of it.
func wantGateStart(t *testing.T, pool *pgxpool.Pool, issue string, woke bool) mergerStart {
	t.Helper()
	starts := mergerStartsOf(t, pool, issue)
	task := gateMovedTask(gatedRoot)
	if woke {
		task = WakeTask(pods.WakeDesignGate, task, StoppedByRelease)
	}
	if len(starts) != 1 {
		t.Fatalf("the merger starts of %s = %+v, want the gate's one start", issue, starts)
	}
	got := starts[0].request
	if !got.GateMessage || got.Phase != "" || got.Entry != 0 || got.Task != task || got.Generation != wakeIssueGeneration || got.Tree != gatedRoot {
		t.Fatalf("the merger start of %s = %+v, want the gate's own start: no phase, no entry, the gate's task %q and its mark", issue, got, task)
	}
	return starts[0]
}

// wantDeliveryLogged asserts the engine logged issue's merger's delivery of move once, with choice
// and the further fields given.
func wantDeliveryLogged(t *testing.T, rig wakeRig, issue string, open bool, choice gateChoice, fields ...string) {
	t.Helper()
	fragments := append([]string{gateDeliveryLine, "issue=" + issue, "move=" + moveName(open), `choice="` + choice.String() + `"`}, fields...)
	if choice == gateNothing {
		fragments[3] = "choice=nothing"
	}
	var found int
	for line := range strings.SplitSeq(rig.logged.String(), "\n") {
		holds := line != ""
		for _, fragment := range fragments {
			holds = holds && strings.Contains(line, fragment)
		}
		if holds {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("the delivery of the %s to %s's merger was logged %d times as %v, want once; the log is:\n%s",
			moveName(open), issue, found, fragments, rig.logged)
	}
}

// At each move of the gate every merger of the tree in merging gets its own one delivery, chosen
// by its claim (R8), in the fact that moved the gate. At the close: the root's live merger is sent
// the closing message, which names the closed gate, says to merge nothing and to take a queued
// pull request back out of the merge queue; each of the two released mergers gets the gate's own
// wake, which records its resume and cancels its backstop; the retired, the failed, the
// operator-suspended merger and the merger with no claim get nothing; and the tester in testing
// is sent the closing text every other phase holder gets. At the opening the live merger is sent
// the opening message, each released merger's closing start is still queued and carries a task,
// so it is that merger's delivery and no second start is written, and the backstop of every
// merger of the tree in merging is cancelled, whatever its delivery.
func TestEachMoveOfTheDesignGateGivesEveryMergerOfTheTreeItsOwnDelivery(t *testing.T) {
	pool, rig := seedGateDeliveryTree(t)
	away := []string{retiredMerger, failedMerger, suspendedMerger, unclaimedMerger}

	rig.apply("spec-version-2", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: heldMergerSpec, Kind: intake.DispatchArtifactVersion,
		Version: 2, Mark: record.EventMark{Seq: 4}})
	closedNote := mergerClosedNote(gatedRoot, record.DesignStepSpec, heldMergerSpec, 2)
	assertRoleMessages(t, pool, liveMerger, []record.RoleMessage{
		{Role: string(mergerToken(t, liveMerger)), Body: closedNote},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: gateMovedBody(record.DesignStepSpec, heldMergerSpec, 2, false)},
	})
	for _, says := range []string{"The design gate of LEGION-208 closed", "Merge nothing",
		"take a pull request you queued back out of the merge queue"} {
		if !strings.Contains(closedNote, says) {
			t.Fatalf("the live merger's closing message %q does not say %q", closedNote, says)
		}
	}
	assertRoleMessages(t, pool, testingMember, []record.RoleMessage{
		{Role: treeRoleToken(t, testingMember, claim.RoleTester), Body: gateMovedBody(record.DesignStepSpec, heldMergerSpec, 2, false)},
	})
	closingStarts := map[string]mergerStart{}
	for _, issue := range []string{releasedMerger, releasedMerger2} {
		assertRoleMessages(t, pool, issue, nil)
		closingStarts[issue] = wantGateStart(t, pool, issue, true)
		wantDeliveryLogged(t, rig, issue, false, gateOwnStart, "wake=true")
	}
	if resumes := mergerResumes(t, pool); len(resumes) != 2 || resumes[0] != releasedMerger+":"+pods.WakeDesignGate ||
		resumes[1] != releasedMerger2+":"+pods.WakeDesignGate {
		t.Fatalf("the merger resumes after the close = %v, want one wake of the gate for each released merger", resumes)
	}
	if open := openMergerBackstops(t, pool); len(open) != 1 || open[0] != unclaimedMerger {
		t.Fatalf("the merger backstops open after the close = %v, want the one the gate's wake of %s did not take", open, releasedMerger2)
	}
	wantDeliveryLogged(t, rig, liveMerger, false, gateRoleMessage)
	for _, issue := range away {
		assertRoleMessages(t, pool, issue, nil)
		if starts := mergerStartsOf(t, pool, issue); len(starts) != 0 {
			t.Fatalf("the merger starts of %s after the close = %+v, want none: its merger runs nothing now", issue, starts)
		}
		wantDeliveryLogged(t, rig, issue, false, gateNothing)
	}

	rig.apply("spec-approved-2", stepApproval(gatedRoot, heldMergerSpec, 2, record.EventMark{Seq: 5}))
	assertRoleMessages(t, pool, liveMerger, []record.RoleMessage{
		{Role: string(mergerToken(t, liveMerger)), Body: closedNote},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: gateMovedBody(record.DesignStepSpec, heldMergerSpec, 2, false)},
		{Role: string(mergerToken(t, liveMerger)), Body: gateMovedBody(record.DesignStepSpec, heldMergerSpec, 2, true)},
		{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: gateMovedBody(record.DesignStepSpec, heldMergerSpec, 2, true)},
	})
	wantDeliveryLogged(t, rig, liveMerger, true, gateRoleMessage)
	for _, issue := range []string{releasedMerger, releasedMerger2} {
		assertRoleMessages(t, pool, issue, nil)
		if starts := mergerStartsOf(t, pool, issue); len(starts) != 1 || starts[0].id != closingStarts[issue].id {
			t.Fatalf("the merger starts of %s after the opening = %+v, want the close's one start and no second", issue, starts)
		}
		wantDeliveryLogged(t, rig, issue, true, gateQueuedStart, "row="+itoa(closingStarts[issue].id))
	}
	for _, issue := range away {
		if starts := mergerStartsOf(t, pool, issue); len(starts) != 0 {
			t.Fatalf("the merger starts of %s after the opening = %+v, want none", issue, starts)
		}
		wantDeliveryLogged(t, rig, issue, true, gateNothing)
	}
	if open := openMergerBackstops(t, pool); len(open) != 0 {
		t.Fatalf("the merger backstops open after the opening = %v, want none: the opening cancels every merger's backstop", open)
	}
}

// itoa is id in decimal, as a log line writes it.
func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// A released merger whose queue holds a start that carries a task gets no second start (R8): that
// start runs first and its send carries the gate's state. At the close only a start that names no
// phase counts — a wake's — since the fence holds a start of the merger's phase until the opening,
// so a merger whose queue holds only its phase's start gets the gate's own start; at the opening
// the phase's start counts too. A start that carries no task, the resume of a cancelled idle
// release, counts for nothing at either move.
func TestAMergerWhoseQueueHoldsAStartWithATaskGetsNoSecondGateStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		// queued is the start of the released merger the outbox holds before the move.
		queued record.SuperviseRequest
		open   bool
		// counts is whether that start is the merger's delivery of the move.
		counts bool
	}{
		{name: "a wake's start at the close", queued: record.SuperviseRequest{Task: WakeTask(pods.WakeChecks, "head1 passed every check", StoppedByRelease)}, counts: true},
		{name: "a start of its phase at the close", queued: record.SuperviseRequest{Phase: phase.Merging, Task: "Merge the head.", Entry: 1}},
		{name: "a start of its phase at the opening", queued: record.SuperviseRequest{Phase: phase.Merging, Task: "Merge the head.", Entry: 1},
			open: true, counts: true},
		{name: "the resume of a cancelled idle release at the opening", open: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHeldMergerWakeRig(t)
			if !tc.open {
				approved := 2
				seedGate(t, rig.pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: heldMergerSpec,
					LatestVersion: 2, ApprovedVersion: &approved, EverOpened: true})
			}
			start := tc.queued
			start.Op, start.Tree, start.Role, start.Generation = "start", gatedRoot, claim.RoleMerger, wakeIssueGeneration
			seedRecord(t, rig.pool, func(tx pgx.Tx) error {
				row, err := record.NewOutboxRow(gatedRoot, start, wakeAt)
				if err != nil {
					return err
				}
				return record.NewStore().Enqueue(t.Context(), tx, row)
			})
			queued := mergerStartsOf(t, rig.pool, gatedRoot)

			if tc.open {
				rig.apply("spec-approved-2", stepApproval(gatedRoot, heldMergerSpec, 2, record.EventMark{Seq: 5}))
			} else {
				rig.apply("spec-version-3", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: heldMergerSpec,
					Kind: intake.DispatchArtifactVersion, Version: 3, Mark: record.EventMark{Seq: 6}})
			}
			starts := mergerStartsOf(t, rig.pool, gatedRoot)
			if tc.counts {
				if len(starts) != 1 || starts[0].id != queued[0].id {
					t.Fatalf("the merger's starts after the %s = %+v, want the queued start alone", moveName(tc.open), starts)
				}
				wantDeliveryLogged(t, rig, gatedRoot, tc.open, gateQueuedStart, "row="+itoa(queued[0].id))
				return
			}
			if len(starts) != 2 || starts[0].id != queued[0].id || !starts[1].request.GateMessage {
				t.Fatalf("the merger's starts after the %s = %+v, want the queued start and the gate's own after it", moveName(tc.open), starts)
			}
			wantDeliveryLogged(t, rig, gatedRoot, tc.open, gateOwnStart, "wake=true")
		})
	}
}
