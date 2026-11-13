package daemon

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// cutoverRelease is the directory of what this test keeps of legion-v19.57.3, the release the
// deployments ran when the cutover began: migrations.txt, the name of every migration the tag
// embeds (jj file list -r legion-v19.57.3 packages/daemon/internal/store/migrations), and the
// tag's own text of the four migrations of that list whose text main has changed since
// (cutoverReleaseText).
const cutoverRelease = "testdata/legion-v19.57.3"

// cutoverReleaseText names the four migrations legion-v19.57.3 shares with this head whose text
// this head has changed, which the record is built from at the tag's own text (jj file show -r
// legion-v19.57.3 <path>). Two of them changed only in their comments (0067_phase_assignment and
// 20261109191929_outbox_write_mark), and they are kept at the tag's text all the same, so the
// record is built from the tag's own text of every migration it embeds. Every other migration of
// the tag is applied from the embedded set, whose text for those versions is the tag's.
var cutoverReleaseText = []string{"0067_phase_assignment", "20261109191929_outbox_write_mark",
	"20261110225246_backward_moves_and_cap_told", "20261110235320_design_gates_publish"}

// The daemon's own lines this test reads: the repair's one record per boot (workflow's
// cutoverMergerRepair, and the daemon's repairCutoverMergers for a boot that finds the repair
// already ran), and its line for each merger it owed a start. cutoverTold is a phrase of the
// Dispatch message the repair posts on each such issue (workflow's cutoverMergerTold), which no
// other message of the daemon's says.
const (
	cutoverRepairLine = "cutover merger repair:"
	cutoverOwedLine   = "workflow: the cutover owes a stopped merger its merging task"
	cutoverTold       = "gave the merger its merging task"
)

// cutoverOldContract is the plugin contract the claims legion-v19.57.3 left record: one this
// daemon does not speak, so a live claim on it is relaunched at the boot (refreshStalePlugins).
const cutoverOldContract = 68

// cutoverStoredSeq is the Dispatch sequence each issue of the old record stores. liveDispatch's
// listing answers every issue at sequence 1, which is below it, so admission's boot reconcile
// neither applies a listed issue (applySummary passes over a summary older than the record) nor
// holds one for the Dispatch consumer (it holds only a summary ahead of the record). A held key
// would stop every admission pass until that consumer caught up, and the consumers start only
// after the repair.
const cutoverStoredSeq = 5

// cutoverWait bounds each wait of this test on the daemon's own work: a boot, a launch, a
// registration, an outbox row's tries, an idle pass. The longest of those is a merger's start
// that a closed design gate held: once the gate opens the test makes the row due at once, and the
// outbox's sweep, every outboxLease (30 seconds), wakes the row's lane to run it. A try of that
// row that read the gate closed just before the test opened it can still record its retry after
// the test made the row due, which puts the row back on its backoff, up to the outbox's 64 seconds
// (retryBackoff), so the bound stays above that. It is the bound a fault is reported at, so a slow
// machine is no failure, and a start that never comes fails here rather than at the package's
// deadline. cutoverPoll is how often a wait asks: most questions are a read of the daemon's claims
// over its API or of its database.
const (
	cutoverWait = 90 * time.Second
	cutoverPoll = 100 * time.Millisecond
)

// cutoverIdleTick is how often this test's daemon decides every pod (overrides.idleTick, the
// production minute): the idle release of the second half is read from the claim rows the test
// back-dates (releaseAgain), so the tick buys nothing but the wait for it.
const cutoverIdleTick = 250 * time.Millisecond

// TestTheCutoverStartsTheMergersTheOldBuildLeftStopped holds that the first boot of this release on
// a record legion-v19.57.3 left gives a merging task to each merger nothing else would start
// again, once, and that no later boot does it again (dispatch://INFRA-3274/spec R2). Under
// legion-v19.57.3 a merger's READY moved its issue to `awaiting_merge`, where the daemon merged and
// no role worked, so the idle release stopped the merger's pod there and armed no backstop for it.
// Migration 20261029120700 moves every such issue to `merging`, where the merger now merges, and
// writes it no task. Without the repair its pull request waits open with nobody to merge it.
//
// The record is built as legion-v19.57.3 leaves it, not as this head would write it. The test
// applies by hand exactly the migrations the tag embeds (cutoverRelease's migrations.txt), in
// version order: the four whose text main has changed since at the tag's own text, and the rest
// from the embedded set. A cut of the embedded set at the tag's newest version would be wrong:
// this head embeds migrations below that version which the tag does not, because a version is the
// moment its migration was written and not the moment it merged. The list internal/store's
// upgrade test applies is no release's either. On that schema the test writes the issues with
// plain SQL in that release's columns, the phase assignment among them (0067_phase_assignment):
// each merger's phases row carries the token that release minted when it started the merger on
// merging, and the merger's claim carries the same token as the one its worker took. Then
// Store.Migrate applies the rest of this head's migrations, as a deployment's roll does:
// 20261029120700 moves the issues to merging, and the fill of 20261111225552 puts each merger at
// entry 1, given and served, with no task recorded for that entry, which is where a merger that
// took its merging task stands after the upgrade. The whole daemon then boots on the store
// (startDaemonWithin).
//
// The four issues, each a tree root of its own, all in `awaiting_merge` with their merger's READY
// head and `completed_at` on its phase row, and none holding a slot, as that release kept none for
// the phase:
//
//   - A: an open pull request and a live, idle merger on plugin contract 68. The boot relaunches it
//     onto this daemon's contract (pluginRelaunchable), and the relaunch's ready gives it its
//     merging task (workflow's readyWorkerTask). This is the merger something else starts.
//   - G: an open pull request, and a merger the idle release suspended with no backstop row, its
//     implementer, tester and reviewer released too. Only the repair starts it.
//   - E: like G, with the root's design gate closed by a newer version of the spec than the one
//     approved. The repair owes its start, and the gate holds the start as it holds any merger's.
//   - D: a merged pull request, and a merger released as G's is, but D is a root with one open
//     child in `implementing`, which holds the slot that release kept for an issue at work and has
//     no claim and no queued start, so the merge does not close D: a merger with nothing left to
//     do, which the repair passes over.
//
// The daemon runs `runtime: kubernetes` over releaseRuntime (idle_release_test.go), the fake
// runtime that also answers what the idle release asks of a pod, since the boot turns the idle
// release on only for such a runtime (attachIdleRelease) and the second half needs it. Its
// Dispatch is liveDispatch, which records every message the daemon posts and lists every issue
// at a sequence below the one the record stores (cutoverStoredSeq). A claim's process is
// cutoverAgent, which says hello on the worker stream with the launch's boot token, registers and
// says ready over the claim routes as the plugin does, and records every prompt and ends its turn
// at once.
//
// The first boot:
//
//   - The repair writes one record naming 2 mergers, G and E: the selection of
//     cutoverMergerRepair, read on the four kinds of merger the old record holds.
//   - Its line for each of them names the slot mark of the start it owed, and that mark is the one
//     workflow.StartOrOwe writes for a start of `merging`, read here from a start StartOrOwe
//     writes for a control issue in `merging` inside a transaction the test rolls back. The start
//     takes the mark the slot rule gives `merging` and no mark of the repair's own, so it waits
//     for a slot exactly where any start of `merging` waits (decision D9 of dispatch://INFRA-3274's
//     plan).
//   - E's merger is not started while its gate is closed, and gets exactly one task once the test
//     opens the gate.
//   - A's merger is relaunched and gets exactly one task, the merging task; G's merger claim is
//     resumed and gets exactly one task, the same merging task. That is the architect's case,
//     decision D8 of the plan: no start the boot makes gives either a second merging task. The
//     count is read after the boot's resume by rule and admission's pass on DaemonBooted have
//     run, after the relaunch's ready and the repair have each given their task, and once nothing
//     is left in the outbox to start either merger, and it is read again at the end of the first
//     daemon's life.
//   - D's merger gets no task, and no role but the merger starts on G or E: their implementer,
//     tester and reviewer claims stay released.
//
// The second boot: G's merger completes with no merge, which records a READY head on its phase
// row again, the idle release suspends it again, and the test deletes the backstop row that
// release armed. G then matches the repair's selection again. The daemon boots again on the same
// store, and the repair starts nothing: G's merger gets no start, and the boot's one record of the
// repair names 0 mergers. A repair keyed on anything that changes at each boot would start G
// again here. Once the outbox holds no Dispatch message, Dispatch has received exactly one
// cutover message on G and one on E over both boots, and none on A or D.
func TestTheCutoverStartsTheMergersTheOldBuildLeftStopped(t *testing.T) {
	rig := newCutoverRig(t)
	ctx := context.Background()

	// The mark StartOrOwe writes for a start of merging, read before the daemon ever runs, on a
	// control issue no daemon reads: the transaction that writes it is rolled back.
	mark := rig.startOrOweMark(t)

	rig.boot(t)
	first := rig.logs

	// A and G: each merger the boot starts is started, and gets one merging task, whatever else of
	// the boot ran. Nothing but the repair starts G's merger, so a boot with no repair fails here.
	rig.connect(t, rig.a)
	rig.connect(t, rig.g)
	if resumed := rig.launches(rig.g.merger, 0); len(resumed) != 1 || resumed[0].Method != "Resume" {
		t.Fatalf("G's merger was launched by %v, want one Resume: the start resumes the claim the release kept", cutoverMethods(resumed))
	}
	rig.await(t, "the boot's own fact (DaemonBooted), with its resume by rule and admission's pass, to be applied", func() bool {
		return rig.bootsApplied(t) == 1
	})
	rig.settled(t, rig.a)
	rig.settled(t, rig.g)
	rig.toldOnce(t, rig.a)
	rig.toldOnce(t, rig.g)

	// The repair's own record of what it did: G and E, each start with StartOrOwe's mark.
	repairs := cutoverLogged(t, first, cutoverRepairLine)
	if len(repairs) != 1 {
		t.Fatalf("the first boot wrote %d records starting %q, want one: %v", len(repairs), cutoverRepairLine, repairs)
	}
	if count, issues := repairs[0]["count"], cutoverIssues(repairs[0]["issues"]); count != float64(2) ||
		!slices.Equal(issues, []string{rig.g.key, rig.e.key}) {
		t.Fatalf("the first boot's repair record %v names %v mergers, %v; want 2, %s and %s: G's and E's mergers are the ones the old build left released with no backstop",
			repairs[0]["msg"], count, issues, rig.g.key, rig.e.key)
	}
	owed := cutoverLogged(t, first, cutoverOwedLine)
	if len(owed) != 2 {
		t.Fatalf("the repair wrote %d lines %q, want one for each of G and E: %v", len(owed), cutoverOwedLine, owed)
	}
	for _, line := range owed {
		if line["needsSlot"] != mark {
			t.Fatalf("the repair owed %v a start whose slot mark is %v, want %v, the mark StartOrOwe writes for a start of merging",
				line["issue"], line["needsSlot"], mark)
		}
	}

	// E: the gate holds the start the repair owed, and the start goes on once the gate opens.
	rig.await(t, "E's merger start held by its tree's closed design gate", func() bool {
		return strings.Contains(rig.startWaits(t, rig.e.key), "design gate")
	})
	if launched := rig.launches(rig.e.merger, 0); len(launched) != 0 {
		t.Fatalf("E's merger was launched by %v while its tree's design gate was closed, want no start", cutoverMethods(launched))
	}
	approved := 2
	putOutboxGate(t, rig.pool, rig.records, record.DesignGate{Issue: rig.e.key, Step: record.DesignStepSpec, ArtifactID: "spec-" + rig.e.key,
		LatestVersion: 2, ApprovedVersion: &approved, EverOpened: true})
	// The held start waits on its row's backoff, up to the outbox's 64 seconds (retryBackoff):
	// made due now, it runs at the outbox's next sweep.
	if _, err := rig.pool.Exec(ctx, "update outbox set next_at = now() where issue = $1 and kind = 'supervise'", rig.e.key); err != nil {
		t.Fatalf("let the backoff of E's merger start go: %v", err)
	}
	rig.connect(t, rig.e)
	rig.settled(t, rig.e)
	rig.toldOnce(t, rig.e)

	// D's merger, and every role of G and E but the merger: nothing starts them.
	rig.notStarted(t, rig.d.merger, "D's merger has nothing to do: its pull request merged")
	claims := rig.daemon.claims()
	if held := listedClaim(t, claims, rig.d.merger); held.State != string(supervise.StateSuspended) || held.SuspendedBy != string(supervise.SuspendedByReleased) {
		t.Fatalf("D's merger stands %s by %q, want suspended by released: no start reached it", held.State, held.SuspendedBy)
	}
	for _, issue := range []cutoverIssue{rig.g, rig.e} {
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer} {
			token := rig.token(t, issue.key, role)
			rig.notStarted(t, token, "the repair starts the merger and no other role")
			if held := listedClaim(t, claims, token); held.State != string(supervise.StateSuspended) || held.SuspendedBy != string(supervise.SuspendedByReleased) {
				t.Fatalf("%s stands %s by %q, want suspended by released: the repair starts the merger and no other role",
					token, held.State, held.SuspendedBy)
			}
		}
	}

	// G's merger ends its task without a merge: GitHub has not merged the pull request yet. Its
	// completion records a READY head on its phase row again, which the repair had cleared.
	applyFact(t, rig.pool, "api", "handoff:"+rig.g.key+":cutover", intake.HandoffComplete{Issue: rig.g.key, Role: claim.RoleMerger,
		Claim: rig.g.merger, Generation: 1, Commit: rig.g.head, Summary: "The pull request waits in the merge queue."}, startedAgainEngine(rig.cfg))
	rig.releaseAgain(t, rig.g)
	rig.matchesTheSelection(t, rig.g)
	// The whole first boot gave each merger one task: the release that stopped G's merger, and
	// everything this daemon did after the counts above, gave none.
	rig.toldOnce(t, rig.a)
	rig.toldOnce(t, rig.g)
	rig.toldOnce(t, rig.e)
	rig.daemon.stop()

	// The second boot: the repair ran at the first, so it starts G's merger no more, and says so.
	calls := len(rig.rt.Calls())
	rebindHeldPorts(t, &rig.cfg)
	rig.boot(t)
	rig.await(t, "the second boot's own fact (DaemonBooted), with its resume by rule and admission's pass, to be applied", func() bool {
		return rig.bootsApplied(t) == 2
	})
	if queued := rig.startsQueued(t, rig.g.key); queued != 0 {
		t.Fatalf("the second boot queued %d starts of G's merger, want none: the repair ran at the first boot", queued)
	}
	if launched := rig.launches(rig.g.merger, calls); len(launched) != 0 {
		t.Fatalf("the second boot launched G's merger by %v, want no start: the repair ran at the first boot", cutoverMethods(launched))
	}
	repairs = cutoverLogged(t, rig.logs, cutoverRepairLine)
	if len(repairs) != 1 || repairs[0]["count"] != float64(0) || len(cutoverIssues(repairs[0]["issues"])) != 0 ||
		!strings.Contains(fmt.Sprint(repairs[0]["msg"]), "0 mergers") {
		t.Fatalf("the second boot wrote the records %v starting %q, want one naming 0 mergers", repairs, cutoverRepairLine)
	}

	// Dispatch: one cutover message on each issue whose merger the repair started, over both boots.
	rig.await(t, "the outbox to hold no Dispatch message", func() bool {
		var waiting int
		if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where kind = $1`, string(record.OutboxKindDispatchMessage)).Scan(&waiting); err != nil {
			t.Fatalf("count the Dispatch messages the outbox holds: %v", err)
		}
		return waiting == 0
	})
	for _, want := range []struct {
		issue cutoverIssue
		told  int
	}{{rig.a, 0}, {rig.g, 1}, {rig.e, 1}, {rig.d, 0}} {
		if told := rig.cutoverMessages(want.issue.key); told != want.told {
			t.Errorf("Dispatch received %d cutover messages on %s over both boots, want %d", told, want.issue.key, want.told)
		}
	}
}

// cutoverRig is the daemon this test boots twice on the old record, and the stand-ins it reads
// back from: Dispatch, GitHub, the runtime the claims launch in, every claim's process, and each
// boot's log.
type cutoverRig struct {
	cfg     config.Config
	project string
	pool    *pgxpool.Pool
	records *record.Postgres
	rt      *releaseRuntime
	built   *built
	board   *liveDispatch
	o       overrides
	// daemon is the running boot, and logs is its log alone, so each boot's records are read
	// apart.
	daemon *daemon
	logs   *syncBuffer
	// a, g, e and d are the four issues of the doc comment, and child is d's open child.
	a, g, e, d cutoverIssue
	child      string
	mu         sync.Mutex
	agents     map[claim.Token][]*cutoverAgent
}

// cutoverIssue is one root of the old record: its key, its pull request's number and head, and
// its merger's claim.
type cutoverIssue struct {
	key    string
	number int
	head   string
	merger claim.Token
}

// newCutoverRig builds the record legion-v19.57.3 leaves (cutoverReleaseStore, seed), brings it to
// this head's schema with Store.Migrate, and readies the configuration and every stand-in the
// daemon boots on. It boots nothing.
func newCutoverRig(t *testing.T) *cutoverRig {
	t.Helper()
	ctx := context.Background()
	st, dsn := cutoverReleaseStore(t)
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.PostgresDSN = dsn
	cfg.Runtime = kubernetesConfig(t, "https://127.0.0.1:1").Runtime
	// A pod quiet for ten minutes is released, and no backstop of one comes due while the test
	// runs: the test deletes the one the release of G arms.
	cfg.Runtime.Kubernetes.IdleRelease = config.IdleReleaseConfig{IdleMinutes: 10, BackstopMinutes: 24 * 60}
	// Room for every issue, so the cap holds no start back.
	cfg.AdmissionCap = 16
	listener := httptest.NewServer(newNoticeListener())
	t.Cleanup(listener.Close)
	cfg.EnvoyURL = listener.URL
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	rig := &cutoverRig{cfg: cfg, project: project, pool: st.Pool(), records: record.NewStore(), built: &built{},
		board: newLiveDispatch(t, cfg.Project), agents: map[claim.Token][]*cutoverAgent{}}
	rig.board.lists = true
	rig.cfg.DispatchURL = rig.board.url
	for i, issue := range []*cutoverIssue{&rig.a, &rig.g, &rig.e, &rig.d} {
		key := cfg.Project + "-" + strconv.Itoa(i+1)
		*issue = cutoverIssue{key: key, number: 400 + i + 1, head: fmt.Sprintf("%040x", 0xa11ce00000+i+1), merger: rig.token(t, key, claim.RoleMerger)}
		rig.board.show(key, workflow.PhaseStatus(phase.Merging))
	}
	rig.child = cfg.Project + "-5"
	rig.board.place(rig.child, workflow.PhaseStatus(phase.Implementing), rig.d.key, false)
	rig.seed(t)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the record legion-v19.57.3 left to this head's schema: %v", err)
	}
	// The record the daemon boots on is the upgraded one: the fill put every merger at entry 1,
	// given and served, with no task recorded for it. A merger left behind its entry would be sent
	// the same resume task by another rule (workflow's sendOwedEntry, for an entry with no task),
	// so the prompts the test counts would not tell the two records apart.
	for _, issue := range []cutoverIssue{rig.a, rig.g, rig.e, rig.d} {
		var entry, given, served int64
		var task string
		if err := st.Pool().QueryRow(ctx, `select p.entry, p.entry_task, c.given_entry, c.served_entry from phases p
			join claims c on c.token = $2 where p.issue = $1 and p.role = 'merger'`, issue.key, string(issue.merger)).Scan(
			&entry, &task, &given, &served); err != nil {
			t.Fatalf("read the entry of %s's merger after the upgrade: %v", issue.key, err)
		}
		if entry != 1 || task != "" || given != 1 || served != 1 {
			t.Fatalf("%s's merger stands at entry %d with task %q, given %d and served %d after the upgrade; want entry 1 with no task, given 1 and served 1, where the fill puts a merger that took its merging task",
				issue.key, entry, task, given, served)
		}
	}

	rig.rt = &releaseRuntime{Runtime: fake.NewRuntime(), onImage: true, sandboxName: "legion-" + project + "-cutover"}
	rig.rt.InPod = true
	rig.o = fakeRuntime(rig.rt, rig.built)
	rig.o.workflowTokens = &workflowTokenRecorder{}
	rig.o.githubAPI = rig.github(t)
	rig.o.idleTick = cutoverIdleTick
	relaunchable := true
	rig.o.pluginRelaunchable = &relaunchable
	return rig
}

// cutoverReleaseStore is a database of this test's own carrying exactly the migrations
// legion-v19.57.3 embeds, applied by hand in version order (applyMigrationsByHand): the four of
// cutoverReleaseText at the tag's own text, and every other one from the embedded set. Each name
// the list holds must be the embedded migration of its version, so a migration renamed or
// renumbered since the tag is named here rather than applied as something else.
func cutoverReleaseStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := testdb.Database(t, "legion_cutover")
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the store legion-v19.57.3 leaves: %v", err)
	}
	t.Cleanup(st.Close)
	listed, err := os.ReadFile(filepath.Join(cutoverRelease, "migrations.txt"))
	if err != nil {
		t.Fatalf("read legion-v19.57.3's migration list: %v", err)
	}
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	embedded := make(map[int]migrations.Migration, len(all))
	for _, migration := range all {
		embedded[migration.Version] = migration
	}
	var release []migrations.Migration
	for _, name := range strings.Fields(string(listed)) {
		digits, _, _ := strings.Cut(name, "_")
		version, err := strconv.Atoi(digits)
		if err != nil {
			t.Fatalf("legion-v19.57.3's migration %q names no version: %v", name, err)
		}
		migration, ok := embedded[version]
		if !ok || migration.Name != name+".up.sql" {
			t.Fatalf("legion-v19.57.3 embeds %s, and this head embeds %q at version %d", name, migration.Name, version)
		}
		if slices.Contains(cutoverReleaseText, name) {
			text, err := os.ReadFile(filepath.Join(cutoverRelease, migration.Name))
			if err != nil {
				t.Fatalf("read legion-v19.57.3's text of %s: %v", migration.Name, err)
			}
			migration.SQL = string(text)
		}
		release = append(release, migration)
	}
	slices.SortFunc(release, func(x, y migrations.Migration) int { return cmp.Compare(x.Version, y.Version) })
	applyMigrationsByHand(t, st, release)
	return st, dsn
}

// seed writes the old record with plain SQL in legion-v19.57.3's columns, as that release's
// daemon left it (the doc comment of the test): the four roots and D's child, each tree's
// lifecycle open under the workflow, the pull requests, each merger's READY phase row, the
// claims, E's design gate closed by a newer spec version, and a ready report of the Envoy
// listener, without which the idle release releases nothing.
//
// Each merger's phases row holds the assignment that release minted when it started the merger
// on merging (0067_phase_assignment's phases.assignment), and the merger's claim holds the same
// token as the one its worker took (claims.serving_assignment), since each of those mergers took
// its merging task and completed it with a READY. That is what the fill of 20261111225552 reads:
// it puts each merger at entry 1, given and served, and takes no task for that entry, since no
// start of it is still owed. No other role has a phases row here, and its claim holds no token.
func (r *cutoverRig) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := r.pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("write %s in legion-v19.57.3's columns: %v", what, err)
		}
	}
	status := workflow.PhaseStatus(phase.Merging)
	for _, issue := range []cutoverIssue{r.a, r.g, r.e, r.d} {
		exec("the root "+issue.key, `insert into issues (key, tree, project, title, phase, generation, status, rank,
			last_dispatch_seq, handed_over, dispatch_status, repo, default_branch, github_repo)
			values ($1, $1, $2, $1, 'awaiting_merge', 1, $3, $1, $4, true, $3, $5, 'main', $5)`,
			issue.key, r.cfg.Project, status, cutoverStoredSeq, startedAgainRepo)
		exec("the tree of "+issue.key, `insert into tree_lifecycles (project, tree, epoch, authority) values ($1, $2, 1, 'workflow')`,
			r.project, issue.key)
		state, mergeSHA := string(record.PullRequestOpen), ""
		if issue == r.d {
			state, mergeSHA = string(record.PullRequestMerged), fmt.Sprintf("%040x", 0xd0e0000000+issue.number)
		}
		exec("the pull request of "+issue.key, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
			failing, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, state, mergeability, merge_sha)
			values ($1, $2, $3, $4, $5, now() - interval '3 hours', '[]', 0, 0, '[]', 1, '', '', $6, 'MERGEABLE', $7)`,
			issue.key, startedAgainRepo, issue.number, claim.IssueBranch(issue.key), issue.head, state, mergeSHA)
		exec("the merger's READY on "+issue.key, `insert into phases (issue, role, claim, handoff_commit, rounds, verdict, summary, completed_at,
			assignment) values ($1, 'merger', $2, $3, 1, '', 'READY: the reviewed head is ready to merge.', now() - interval '2 hours', $4)`,
			issue.key, string(issue.merger), issue.head, cutoverAssignment(issue.key))
	}
	exec("the open child of "+r.d.key, `insert into issues (key, tree, project, title, parent, phase, generation, status, rank,
		last_dispatch_seq, handed_over, dispatch_status, repo, default_branch, github_repo)
		values ($1, $2, $3, $1, $2, 'implementing', 1, $4, $1, $5, false, $4, $6, 'main', $6)`,
		r.child, r.d.key, r.cfg.Project, workflow.PhaseStatus(phase.Implementing), cutoverStoredSeq, startedAgainRepo)
	exec("the slot of "+r.child, `insert into slots (issue, index, admitted_at) values ($1, 1, now() - interval '1 day')`, r.child)

	locator, err := json.Marshal(runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: r.a.merger, Incarnation: "4242:1",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}})
	if err != nil {
		t.Fatal(err)
	}
	claimRow := `insert into claims (token, project, tree, issue, role, generation, session, session_file, locator, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak, boot_token_hash, serving_generation, tree_epoch,
		suspended_by, plugin_contract, serving_assignment)
		values ($1, $2, $3, $3, $4, 1, $5, $6, $7::jsonb, $8, 0, 0, 0, 0, $9, 1, 1, $10, $11, $12)`
	session := func(issue string, role claim.Role) (string, string) {
		return "ses_" + string(role) + "_" + issue, filepath.Join(r.cfg.StateDir, issue+"-"+string(role)+".jsonl")
	}
	name, file := session(r.a.key, claim.RoleMerger)
	exec("A's idle merger", claimRow, string(r.a.merger), r.project, r.a.key, string(claim.RoleMerger), name, file, string(locator),
		string(supervise.StateIdle), supervise.HashBootToken("cutover-a-"+randomSuffix(t)), "", cutoverOldContract,
		cutoverAssignment(r.a.key))
	released := func(issue string, role claim.Role) {
		name, file := session(issue, role)
		served := ""
		if role == claim.RoleMerger {
			served = cutoverAssignment(issue)
		}
		exec(issue+"'s released "+string(role), claimRow, string(r.token(t, issue, role)), r.project, issue, string(role), name, file, nil,
			string(supervise.StateSuspended), nil, string(supervise.SuspendedByReleased), cutoverOldContract, served)
	}
	for _, issue := range []cutoverIssue{r.g, r.e} {
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
			released(issue.key, role)
		}
	}
	released(r.d.key, claim.RoleMerger)

	exec("the closed design gate of "+r.e.key, `insert into design_gates (issue, artifact_id, latest_version, approved_version, ever_opened, step)
		values ($1, $2, 2, 1, true, 'spec')`, r.e.key, "spec-"+r.e.key)
	exec("the listener's ready report", `insert into listener_reports (project, ready_at) values ($1, now())`, r.project)
}

// cutoverAssignment is the token legion-v19.57.3 minted onto issue's merger phases row when it
// started the merger on merging (0067_phase_assignment), which the merger's claim then served.
// That release minted a random one; any token both sides carry is the same record.
func cutoverAssignment(issue string) string {
	return "merging-" + issue
}

// startOrOweMark is the slot mark workflow.StartOrOwe writes on a start of merging: the
// NeedsSlot of the start it queues for the merger of a control issue standing in merging, read
// back as the repair reads its own (record.Store.RoleRun), in a transaction the test rolls back.
func (r *cutoverRig) startOrOweMark(t *testing.T) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	control := startedAgainIssue(r.cfg, r.cfg.Project+"-99", r.cfg.Project+"-99", phase.Merging)
	if err := r.records.PutIssue(ctx, tx, control); err != nil {
		t.Fatalf("record the control issue: %v", err)
	}
	if err := workflow.StartOrOwe(ctx, tx, r.records, control.Key, record.SuperviseRequest{Op: "start", Tree: control.Tree,
		Role: claim.RoleMerger, Generation: control.Generation, Phase: phase.Merging, Task: workflow.ResumePhaseTask(control),
		ResumeTask: true, RuleResume: true}, time.Now()); err != nil {
		t.Fatalf("owe the control issue's merger a start: %v", err)
	}
	run, err := r.records.RoleRun(ctx, tx, r.token(t, control.Key, claim.RoleMerger), control.Key, claim.RoleMerger, control.Generation)
	if err != nil {
		t.Fatalf("read the control issue's queued start: %v", err)
	}
	if len(run.Queued) != 1 {
		t.Fatalf("StartOrOwe queued %v for the control issue's merger, want one start", run.Queued)
	}
	return run.Queued[0].Request.NeedsSlot
}

// boot runs the daemon on the rig's store until it is stopped or the test ends, with a log of its
// own, and returns once it answers /healthz. The repair is a boot step before the API serves, so
// its records are in that log by then. A failed test prints the end of the log of each boot.
func (r *cutoverRig) boot(t *testing.T) {
	t.Helper()
	logs := &syncBuffer{}
	r.logs = logs
	t.Cleanup(func() {
		if t.Failed() {
			log := logs.String()
			t.Logf("the end of a boot's log:\n%s", log[max(0, len(log)-idleRigLogTailBytes):])
		}
	})
	r.daemon = startDaemonWithin(t, r.cfg, r.o, slog.New(slog.NewJSONHandler(logs, nil)), cutoverWait)
}

// await waits for condition within cutoverWait, or what is left of the test binary's own deadline
// less a second, asking it every cutoverPoll.
func (r *cutoverRig) await(t *testing.T, what string, condition func() bool) {
	t.Helper()
	testwait.WithinPolling(t, testwait.Bounded(t, cutoverWait, time.Second), cutoverPoll, what, condition)
}

// token is role's claim token on issue.
func (r *cutoverRig) token(t *testing.T, issue string, role claim.Role) claim.Token {
	t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// launches is every launch of token the runtime was asked for from its call since on: a spawn of
// a new process or a resume of the session the claim kept.
func (r *cutoverRig) launches(token claim.Token, since int) []fake.Call {
	calls := r.rt.Calls()
	var launched []fake.Call
	for _, call := range calls[min(since, len(calls)):] {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
			launched = append(launched, call)
		}
	}
	return launched
}

// notStarted holds that token was never launched, why saying why it must not be.
func (r *cutoverRig) notStarted(t *testing.T, token claim.Token, why string) {
	t.Helper()
	if launched := r.launches(token, 0); len(launched) != 0 {
		t.Fatalf("%s was launched by %v, want no start: %s", token, cutoverMethods(launched), why)
	}
}

// connect is the process of issue's merger coming up in its latest launch: it says hello with
// that launch's boot token, and once the machine has the hello it registers in the session the
// claim recorded and says ready, as the plugin does. The ready is what gives a relaunched merger
// its task (workflow's readyWorkerTask), and what a task already pending waits for.
func (r *cutoverRig) connect(t *testing.T, issue cutoverIssue) {
	t.Helper()
	r.await(t, "a launch of "+issue.key+"'s merger", func() bool {
		return len(r.launches(issue.merger, 0)) != 0
	})
	launch := lastLaunch(t, r.rt.Runtime, issue.merger)
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	agent := dialCutoverAgent(t, address, launch.BootToken)
	r.mu.Lock()
	r.agents[issue.merger] = append(r.agents[issue.merger], agent)
	r.mu.Unlock()
	r.await(t, "the hello of "+issue.key+"'s merger to reach its machine", func() bool {
		return r.daemon.claim(issue.merger).State == string(supervise.StateShimConnected)
	})
	held := r.daemon.claim(issue.merger)
	status, body := r.daemon.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{BootToken: launch.BootToken,
		SessionID: held.Session, OmpSessionFile: held.SessionFile, AgentID: "agent-" + issue.key, PluginContract: api.DaemonAPIVersion}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s's merger = %d; body %s", issue.key, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s's merger: %v", issue.key, err)
	}
	if status, body := r.daemon.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{ClaimToken: issue.merger,
		SessionID: held.Session, Secret: registered.Secret, Generation: registered.Generation}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s's merger = %d; body %s", issue.key, status, body)
	}
}

// settled waits until issue's merger has been told something and nothing more is coming for it:
// no start of it left in the outbox, and its claim idle with no task pending. A start that would
// give it a second task is one of those two until its prompt is sent, and the agent ends every
// turn at once, so a task delivered after this shows as a second prompt or a pending task.
func (r *cutoverRig) settled(t *testing.T, issue cutoverIssue) {
	t.Helper()
	r.await(t, issue.key+"'s merger to be told its task and hold nothing more", func() bool {
		if len(r.prompts(issue.merger)) == 0 || r.startsQueued(t, issue.key) != 0 {
			return false
		}
		held := r.daemon.claim(issue.merger)
		return held.State == string(supervise.StateIdle) && held.Pending == nil
	})
}

// toldOnce holds that issue's merger was given exactly one prompt over its whole life in this
// test, and that it is the merging task: the resume task of the phase it holds
// (workflow.ResumePhaseTask), read from the issue as the record holds it now.
func (r *cutoverRig) toldOnce(t *testing.T, issue cutoverIssue) {
	t.Helper()
	stands := readIssue(t, r.pool, issue.key)
	if stands == nil {
		t.Fatalf("read %s: the record does not hold it", issue.key)
	}
	want := workflow.ResumePhaseTask(*stands)
	if told := r.prompts(issue.merger); len(told) != 1 || told[0] != want {
		t.Fatalf("%s's merger was prompted %q, want once with its merging task %q", issue.key, told, want)
	}
}

// prompts is every prompt token's processes were given, oldest first.
func (r *cutoverRig) prompts(token claim.Token) []string {
	r.mu.Lock()
	agents := slices.Clone(r.agents[token])
	r.mu.Unlock()
	var told []string
	for _, agent := range agents {
		told = append(told, agent.told()...)
	}
	return told
}

// startsQueued is how many starts of issue's merger the outbox holds. A finished row is deleted
// (record.Postgres.FinishOutbox).
func (r *cutoverRig) startsQueued(t *testing.T, issue string) int {
	t.Helper()
	var queued int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = $2
		and payload->>'op' = 'start' and payload->>'role' = $3`, issue, string(record.OutboxKindSupervise), string(claim.RoleMerger)).Scan(&queued); err != nil {
		t.Fatalf("count the starts of %s's merger: %v", issue, err)
	}
	return queued
}

// startWaits is what issue's queued merger start last waited on, "" for none.
func (r *cutoverRig) startWaits(t *testing.T, issue string) string {
	t.Helper()
	var waits string
	if err := r.pool.QueryRow(context.Background(), `select coalesce(max(last_error), '') from outbox where issue = $1 and kind = $2
		and payload->>'op' = 'start' and payload->>'role' = $3`, issue, string(record.OutboxKindSupervise), string(claim.RoleMerger)).Scan(&waits); err != nil {
		t.Fatalf("read the wait of %s's merger start: %v", issue, err)
	}
	return waits
}

// bootsApplied is how many boots' own facts (intake.DaemonBooted, applied as the daemon's
// workflowRuntime.booted keys it) the record holds as applied: each is the boot's resume by rule
// and, through admission's handler beside it, that boot's admission pass.
func (r *cutoverRig) bootsApplied(t *testing.T) int {
	t.Helper()
	var applied int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from processed_events where source = 'daemon' and event_id like $1`,
		"boot:"+r.cfg.Project+":%").Scan(&applied); err != nil {
		t.Fatalf("count the boots applied: %v", err)
	}
	return applied
}

// releaseAgain has the idle release take issue's pod again, and then deletes the backstop row
// that release armed for the merger. The pass takes a pod whose live claims are idle, have
// reported since their last turn that they run no background work, and have been quiet for the
// idle period (workflow.IdleRelease). cutoverAgent makes no activity report of its own, so the
// test writes that report on the issue's claim rows and back-dates the rows past the period, as
// idle_release_test.go's rig does, and refreshes the listener's ready report, without which the
// pass releases nothing. It does both at every poll, since a write of the claim's own in between
// moves the rows forward again.
func (r *cutoverRig) releaseAgain(t *testing.T, issue cutoverIssue) {
	t.Helper()
	ctx := context.Background()
	r.await(t, "the idle release to suspend "+issue.key+"'s merger and arm its backstop", func() bool {
		if _, err := r.pool.Exec(ctx, `update claims set updated_at = now() - interval '30 minutes',
			activity_at = now() - interval '25 minutes', activity_background = false, activity_kinds = '',
			activity_next_reminder_at = null where issue = $1`, issue.key); err != nil {
			t.Fatalf("back-date the claims of %s: %v", issue.key, err)
		}
		if _, err := r.pool.Exec(ctx, `insert into listener_reports (project, ready_at) values ($1, now())
			on conflict (project) do update set ready_at = excluded.ready_at`, r.project); err != nil {
			t.Fatalf("refresh the listener's ready report: %v", err)
		}
		held := r.daemon.claim(issue.merger)
		if held.State != string(supervise.StateSuspended) || held.SuspendedBy != string(supervise.SuspendedByReleased) {
			return false
		}
		var releasing, armed bool
		if err := r.pool.QueryRow(ctx, `select exists (select 1 from issue_releases where issue = $1),
			exists (select 1 from pod_wake_backstops where issue = $1 and role = $2 and not fired)`, issue.key, string(claim.RoleMerger)).Scan(&releasing, &armed); err != nil {
			t.Fatalf("read the release of %s: %v", issue.key, err)
		}
		return !releasing && armed
	})
	tag, err := r.pool.Exec(ctx, `delete from pod_wake_backstops where issue = $1 and role = $2`, issue.key, string(claim.RoleMerger))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete the backstop the release armed for %s's merger = %v rows, %v; want the one", issue.key, tag.RowsAffected(), err)
	}
}

// matchesTheSelection holds that issue's merger is again one the repair would select, as the
// repair reads one (workflow's cutoverMergerRepair): the issue in merging on an open pull request,
// the merger's phase row holding a READY head, its claim suspended by `released`, and no open
// backstop of it. Without this, a second boot that starts nothing would prove nothing.
func (r *cutoverRig) matchesTheSelection(t *testing.T, issue cutoverIssue) {
	t.Helper()
	var stands, pull, head, state, by string
	var armed bool
	if err := r.pool.QueryRow(context.Background(), `select i.phase, p.state, ph.handoff_commit, c.state, c.suspended_by,
		exists (select 1 from pod_wake_backstops b where b.issue = i.key and b.role = c.role and not b.fired)
		from issues i join pull_requests p on p.issue = i.key join phases ph on ph.issue = i.key and ph.role = $2
		join claims c on c.token = $3 where i.key = $1`, issue.key, string(claim.RoleMerger), string(issue.merger)).Scan(
		&stands, &pull, &head, &state, &by, &armed); err != nil {
		t.Fatalf("read %s as the repair selects it: %v", issue.key, err)
	}
	if stands != string(phase.Merging) || pull != string(record.PullRequestOpen) || head != issue.head ||
		state != string(supervise.StateSuspended) || by != string(supervise.SuspendedByReleased) || armed {
		t.Fatalf("%s stands in %s on a %s pull request, its merger's READY head %q, its claim %s by %q, a backstop open %t; want it as the repair selects one again",
			issue.key, stands, pull, head, state, by, armed)
	}
}

// cutoverMessages is how many messages the daemon posted on issue that are the cutover's.
func (r *cutoverRig) cutoverMessages(issue string) int {
	told := 0
	for _, write := range r.board.writes() {
		if write.Issue == issue && strings.Contains(write.Body, cutoverTold) {
			told++
		}
	}
	return told
}

// github is GitHub's REST API for acme/widgets as this test's daemon reads it: each root's pull
// request, open at its head and mergeable, or merged for D; no reviews and no open pull request in
// the listing; the repository, with main its default branch, and that branch's ref; and every
// create of an issue branch answered as GitHub answers a branch that is there already. Anything
// else is answered 404 and logged, so a read the test did not foresee is named in its output
// rather than reaching github.com.
func (r *cutoverRig) github(t *testing.T) string {
	t.Helper()
	pulls := map[string]cutoverIssue{}
	for _, issue := range []cutoverIssue{r.a, r.g, r.e, r.d} {
		pulls[strconv.Itoa(issue.number)] = issue
	}
	repo := "/repos/" + startedAgainRepo
	return githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		write := func(value any) {
			_ = json.NewEncoder(w).Encode(value)
		}
		number, tail, _ := strings.Cut(strings.TrimPrefix(path, repo+"/pulls/"), "/")
		issue, isPull := pulls[number]
		isPull = isPull && strings.HasPrefix(path, repo+"/pulls/")
		switch {
		case req.Method == http.MethodPost && path == repo+"/git/refs":
			status, answer := branchExists()
			w.WriteHeader(status)
			_, _ = io.WriteString(w, answer)
			return
		case req.Method != http.MethodGet:
		case path == repo:
			write(map[string]string{"full_name": startedAgainRepo, "default_branch": "main"})
			return
		case path == repo+"/pulls":
			write([]any{})
			return
		case path == repo+"/git/ref/heads/main":
			write(map[string]any{"ref": "refs/heads/main", "object": map[string]string{"sha": branchBaseCommit, "type": "commit"}})
			return
		case isPull && tail == "reviews":
			write([]any{})
			return
		case isPull && tail == "":
			answer := map[string]any{"number": issue.number, "state": "open", "merged": false,
				"html_url": fmt.Sprintf("https://github.com/%s/pull/%d", startedAgainRepo, issue.number), "mergeable": true,
				"mergeable_state": "clean", "base": map[string]string{"ref": "main"},
				"head": map[string]string{"ref": claim.IssueBranch(issue.key), "sha": issue.head}}
			if issue == r.d {
				answer["state"], answer["merged"], answer["merge_commit_sha"] = "closed", true, fmt.Sprintf("%040x", 0xd0e0000000+issue.number)
			}
			write(answer)
			return
		}
		t.Logf("the GitHub stand-in has no answer for %s %s", req.Method, path)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}})
}

// cutoverLogged is every record of a boot's log whose message starts with prefix, oldest first.
func cutoverLogged(t *testing.T, logs *syncBuffer, prefix string) []map[string]any {
	t.Helper()
	return slices.DeleteFunc(loggedLines(t, logs.String(), prefix), func(entry map[string]any) bool {
		msg, _ := entry["msg"].(string)
		return !strings.HasPrefix(msg, prefix)
	})
}

// cutoverIssues is a record's `issues` attribute as the issue keys it names, in key order.
func cutoverIssues(value any) []string {
	listed, _ := value.([]any)
	keys := make([]string, 0, len(listed))
	for _, key := range listed {
		keys = append(keys, fmt.Sprint(key))
	}
	slices.Sort(keys)
	return keys
}

// cutoverMethods is how each launch was made, for a failure's text.
func cutoverMethods(calls []fake.Call) []string {
	methods := make([]string, 0, len(calls))
	for _, call := range calls {
		methods = append(methods, call.Method)
	}
	return methods
}

// cutoverAgent is one launch of a claim's process on the daemon's worker stream: the shim and the
// Oh My Pi behind it, as far as this test needs them. It says hello with the launch's boot token,
// answers the protocol negotiation and every question about whether a turn is running, records
// each prompt, and takes the prompt's turn to its end at once, so a task the daemon gives it is
// over before the next one could arrive and every task it is ever given is a prompt it recorded.
// It is the shape of daemon_test.go's shim and wake_wave_refresh_test.go's waveTurn, serving on a
// goroutine of its own because the daemon prompts it whenever a task reaches its claim.
type cutoverAgent struct {
	conn  net.Conn
	lines *bufio.Reader
	// mu orders the serving goroutine's writes and its record of prompts against the test's reads.
	mu      sync.Mutex
	writer  *shimwire.Writer
	prompts []string
}

// dialCutoverAgent connects a process to the worker stream at address and returns once the
// daemon has acknowledged its hello with bootToken.
func dialCutoverAgent(t *testing.T, address, bootToken string) *cutoverAgent {
	t.Helper()
	network, socket := "tcp", strings.TrimPrefix(address, "tcp://")
	if path, isUnix := strings.CutPrefix(address, "unix://"); isUnix {
		network, socket = "unix", path
	}
	conn, err := net.Dial(network, socket)
	if err != nil {
		t.Fatalf("dial the worker stream %s: %v", address, err)
	}
	t.Cleanup(func() { conn.Close() })
	a := &cutoverAgent{conn: conn, lines: bufio.NewReader(conn), writer: shimwire.NewWriter(conn)}
	a.send(shimwire.Hello2{BootToken: bootToken})
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	line, err := a.lines.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read the daemon's answer to the hello: %v", err)
	}
	frame, err := shimwire.Decode(line)
	if err != nil || frame.FrameType() != shimwire.TypeHelloAck {
		t.Fatalf("the daemon answered the hello with %q (%v), want hello_ack", line, err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear the read deadline: %v", err)
	}
	go a.serve()
	return a
}

// serve answers the frames the daemon sends until the connection closes: a shutdown is the
// process's stop, after which it says nothing more.
func (a *cutoverAgent) serve() {
	for {
		line, err := a.lines.ReadBytes('\n')
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			continue
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			a.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.GetState:
			streaming := false
			data, err := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			if err != nil {
				return
			}
			a.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
		case shimwire.Prompt:
			a.mu.Lock()
			a.prompts = append(a.prompts, f.Message)
			a.mu.Unlock()
			a.send(shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true})
			a.send(shimwire.AgentStart{})
			a.send(shimwire.AgentEnd{})
			a.send(shimwire.PromptResult{ID: f.ID, AgentInvoked: true})
		case shimwire.Shutdown:
			_ = a.conn.Close()
			return
		}
	}
}

// send writes one frame. A write that fails is left to the assertions that follow it, since this
// runs on the serving goroutine and the test may already be over.
func (a *cutoverAgent) send(frame shimwire.Frame) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.writer.WriteFrame(frame)
}

// told is every prompt this process was given, oldest first.
func (a *cutoverAgent) told() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.prompts)
}
