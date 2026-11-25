package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// daemonThreadSigningKey is the one cookie key every process of the lifecycle test takes, as every
// process of a deployment does, so no process waits out a live window of the one before it
// (processes.Register).
const daemonThreadSigningKey = "daemon-thread-key"

// daemonThreadStartedMatch is the line the step logs as its run after the listen starts
// (docs.DaemonThreadStarted), as a reader of the process's output matches it.
const daemonThreadStartedMatch = `msg="` + docs.DaemonThreadStarted + `"`

// daemonThreadFixtureSpec is the spec of the fixture's issue: two criteria and a requirement, each
// a block a parent pointer can place.
const daemonThreadFixtureSpec = ":::criterion{#crit}\nI want the urgent asks first.\n:::\n\n" +
	":::requirement{#req}\nAn urgent ask comes first.\n:::\n\n:::criterion{#late}\nI want it quick.\n:::\n"

// daemonThreadFixture is an issue a process of this build wrote and then left: its key, the reply
// a person wrote in the thread the Legion daemon's session began on it, the node of that reply's
// first block, and the node address of each block of its spec, by block id.
type daemonThreadFixture struct {
	issue             string
	reply, replyBlock string
	blocks            map[string]string
}

// seedDaemonThreadFixture starts a process of this build over the database, has it write the
// fixture's issue - its spec, which alice approves, a message the session legion-daemon:CORE
// posts and alice's reply to it - and stops it, so nothing serves the database when it returns.
// It writes no placement under the thread: the caller writes those once no process runs.
func seedDaemonThreadFixture(t *testing.T, database *store.Store, databaseURL string) daemonThreadFixture {
	t.Helper()
	ctx := context.Background()
	seeder := startDispatchProcessWith(t, databaseURL, dispatchOptions{SigningKey: daemonThreadSigningKey})
	seeder.waitHealthy(t)
	seeder.createProject(t, "CORE", "Core")
	key, spec := seeder.createIssueIn(t, "CORE", "The daemon's thread", daemonThreadFixtureSpec)
	seeder.call(t, http.MethodPost, "/api/v1/artifacts/"+spec+"/reviews", `{"state":"approved"}`, http.StatusCreated)
	posted := func(what string, answer httpAnswer) string {
		t.Helper()
		var message struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(answer.body, &message); answer.status != http.StatusCreated || err != nil || message.ID == "" {
			t.Fatalf("%s: status %d, %v: %s", what, answer.status, err, answer.body)
		}
		return message.ID
	}
	messages := "/api/v1/issues/" + key + "/messages"
	root := posted("the daemon's message", seeder.send(t, http.MethodPost, messages, encodedBody(t, map[string]any{
		"body": "READY: the issue is claimed.", "actor": map[string]string{"kind": "session", "id": "legion-daemon:CORE"},
	}), agentBearer()))
	fixture := daemonThreadFixture{issue: key, blocks: map[string]string{}}
	fixture.reply = posted("alice's reply in the daemon's thread", seeder.send(t, http.MethodPost, messages, encodedBody(t, map[string]any{
		"body": "Hold it until Friday.", "in_reply_to": root,
	}), asUser(dispatchTestLogin)))
	seeder.Terminate(t)
	seeder.WaitExit(t, "SIGTERM")

	if err := database.Pool.QueryRow(ctx, `
		select node_id from message_blocks where message_id = $1::uuid and position is not null order by position limit 1
	`, fixture.reply).Scan(&fixture.replyBlock); err != nil {
		t.Fatalf("read the first block of the reply %s: %v", fixture.reply, err)
	}
	for _, id := range []string{"crit", "req", "late"} {
		var address string
		if err := database.Pool.QueryRow(ctx, `
			select d.node_address from document_blocks d join artifacts a on a.id = d.artifact_id
			where a.issue_key = $1 and d.block_id = $2 and d.node_address is not null`, key, id).Scan(&address); err != nil {
			t.Fatalf("read the node of block %s of %s: %v", id, key, err)
		}
		fixture.blocks[id] = address
	}
	return fixture
}

// daemonThreadPlacement is the current part_of of the block node, failing the test where it holds
// no one part_of.
func daemonThreadPlacement(t *testing.T, database *store.Store, node string) graphstore.Tuple {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, refs.BlockNodeKind, node, []string{graphstore.TuplePartOf})
	if err != nil || len(held) != 1 {
		t.Fatalf("the block %s stands on %v (%v), want one parent", node, held, err)
	}
	return held[0]
}

// placeUnderDaemonThread moves the block node under the node toKind and toID name, through the
// store's writer, as a parent pointer an agent's session wrote before R2's rule of the daemon
// placed it: the retraction of its part_of and one to that node, dated by the database's clock.
func placeUnderDaemonThread(t *testing.T, database *store.Store, node, toKind, toID string) {
	t.Helper()
	ctx := context.Background()
	retracted := daemonThreadPlacement(t, database, node)
	if err := database.Pool.QueryRow(ctx, `select clock_timestamp()`).Scan(&retracted.At); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	retracted.Asserted, retracted.Who, retracted.SourceEvent = false, model.Actor{Kind: "session", ID: "ses_architect"}, graphstore.NoEvent
	placed := retracted
	placed.ToKind, placed.ToID, placed.Asserted = toKind, toID, true
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, nil, []graphstore.Tuple{retracted, placed})
	}); err != nil {
		t.Fatalf("place %s under %s %s: %v", node, toKind, toID, err)
	}
}

// daemonThreadParents is how many placements the census kind daemon_thread_parent counts in the
// database now.
func daemonThreadParents(t *testing.T, database *store.Store) int {
	t.Helper()
	census, err := graphcensus.ReadDaemonThreadCensus(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("read the census kind %s: %v", graphcensus.DaemonThreadParentKind, err)
	}
	return census.Count()
}

// daemonThreadWritten is what a census must leave as it found it: how many tuples the store
// holds and the newest of them, how many versions and reviews, and the approval requests as they
// stand.
type daemonThreadWritten struct {
	tuples, newestTuple int64
	versions, reviews   int64
	requests            string
}

func readDaemonThreadWritten(t *testing.T, database *store.Store) daemonThreadWritten {
	t.Helper()
	var written daemonThreadWritten
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from graph_tuples), (select coalesce(max(seq), 0) from graph_tuples),
		       (select count(*) from artifact_versions), (select count(*) from artifact_reviews),
		       (select coalesce(md5(string_agg(a.id::text || a.state || coalesce(a.approval::text, ''), ',' order by a.id)), '')
		        from asks a where a.kind = 'approval')
	`).Scan(&written.tuples, &written.newestTuple, &written.versions, &written.reviews, &written.requests); err != nil {
		t.Fatalf("read the tuples, versions and approvals of the database: %v", err)
	}
	return written
}

// TestDaemonThreadReplacementRunsAfterListenAndOnReconciliation holds R2's daemon-thread step
// (docs.DaemonThreadStep) to when this issue's spec R2 runs it: once after the server listens, and
// then on each tick of the block-index reconciliation; and its dry run to the pre-deploy census,
// which writes nothing.
//
// First the dry run, on a fixture of its own that no server of the release under test starts on. A
// process of this build writes an issue with a thread the Legion daemon's session began and a
// person's reply in it, and stops; the test then places a criterion under that reply and a
// requirement under a block of it, as a parent pointer written before R2's rule placed them. The
// pre-deploy command, `envoy-dispatch census`, exits 0, prints `daemon_thread_parent=2` and the
// addresses of exactly those two nodes, and leaves the store's tuples, the versions, the reviews
// and the approval requests as it found them.
//
// Then the lifecycle, on a second fixture written the same way. A real server starts over it. Its
// output holds the line that says it listens and, after it, the line the step logs as its first
// run starts: a step started before the listener was ready fails here as "daemon-thread
// replacement started before the server listened". That run moves the two placements, each under
// the issue by a part_of the step wrote, so the census kind reads zero. The test then places a
// third node under the reply, with the server running, and the census kind reads zero again within
// three intervals of the block-index reconciliation, with no restart: a server whose
// reconciliation does not call the step fails here as "daemon_thread_parent=1 after
// reconciliation tick, want 0".
func TestDaemonThreadReplacementRunsAfterListenAndOnReconciliation(t *testing.T) {
	t.Parallel()

	censused := storetest.Open(t)
	censusedURL := censused.Pool.Config().ConnString()
	dryRun := seedDaemonThreadFixture(t, censused, censusedURL)
	placeUnderDaemonThread(t, censused, dryRun.blocks["crit"], string(refs.ItemMessage), dryRun.reply)
	placeUnderDaemonThread(t, censused, dryRun.blocks["req"], refs.ItemMessage.BlockKind(), dryRun.replyBlock)
	before := readDaemonThreadWritten(t, censused)
	code, out := runDispatchCommand(t, cmdtest.Build(t, "envoy-dispatch"), censusedURL, "census")
	if code != 0 {
		t.Fatalf("envoy-dispatch census exited %d over a migrated database that holds two such placements, want 0: the dry run refuses nothing:\n%s", code, out)
	}
	var counted string
	var addresses []string
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\n")
		if quoted, found := strings.CutPrefix(line, graphcensus.DaemonThreadNodeLine); found {
			prefix, err := strconv.QuotedPrefix(quoted)
			if err != nil {
				t.Fatalf("the census line %q quotes no node address: %v", line, err)
			}
			address, err := strconv.Unquote(prefix)
			if err != nil {
				t.Fatalf("the census line %q quotes no node address: %v", line, err)
			}
			addresses = append(addresses, address)
			continue
		}
		if rest, found := strings.CutPrefix(line, "census: "+graphcensus.DaemonThreadParentKind+"="); found {
			counted, _, _ = strings.Cut(rest, " ")
		}
	}
	want := []string{dryRun.blocks["crit"], dryRun.blocks["req"]}
	slices.Sort(want)
	if counted != "2" || !slices.Equal(addresses, want) {
		t.Errorf("envoy-dispatch census printed daemon_thread_parent=%s and the nodes %v, want daemon_thread_parent=2 and %v:\n%s", counted, addresses, want, out)
	}
	if after := readDaemonThreadWritten(t, censused); after != before {
		t.Errorf("envoy-dispatch census changed the store: tuples, versions and approvals read %+v after it and %+v before", after, before)
	}
	if owed := daemonThreadParents(t, censused); owed != 2 {
		t.Errorf("the census kind reads daemon_thread_parent=%d after the dry run, want the two placements still there", owed)
	}

	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	fixture := seedDaemonThreadFixture(t, database, databaseURL)
	placeUnderDaemonThread(t, database, fixture.blocks["crit"], string(refs.ItemMessage), fixture.reply)
	placeUnderDaemonThread(t, database, fixture.blocks["req"], refs.ItemMessage.BlockKind(), fixture.replyBlock)
	if owed := daemonThreadParents(t, database); owed != 2 {
		t.Fatalf("the census kind reads daemon_thread_parent=%d over the fixture before the server starts, want 2", owed)
	}
	process := startDispatchProcessWith(t, databaseURL, dispatchOptions{SigningKey: daemonThreadSigningKey})
	process.waitHealthy(t)
	process.waitForLog(t, time.Minute, daemonThreadStartedMatch)
	output := process.Output.String()
	listening, started := strings.Index(output, process.listeningLine()), strings.Index(output, daemonThreadStartedMatch)
	if listening < 0 || started < listening {
		t.Fatalf("daemon-thread replacement started before the server listened: the step's first run starts at byte %d of the output and the listening line at %d:\n%s",
			started, listening, output)
	}
	settled := func(within time.Duration) int {
		t.Helper()
		deadline := time.Now().Add(within)
		for {
			owed := daemonThreadParents(t, database)
			if owed == 0 || time.Now().After(deadline) {
				return owed
			}
			select {
			case <-process.Exited:
				t.Fatalf("the server exited while the test waited for the step:\n%s", process.Output.String())
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	if owed := settled(time.Minute); owed != 0 {
		t.Fatalf("daemon_thread_parent=%d after the run that follows the listen, want 0:\n%s", owed, process.Output.String())
	}
	for _, id := range []string{"crit", "req"} {
		now := daemonThreadPlacement(t, database, fixture.blocks[id])
		if now.ToKind != graphstore.KindIssue || now.ToID != fixture.issue || now.Who.ID != docs.DaemonThreadStepActor.ID {
			t.Errorf("the block %s stands under %s %s by a part_of %s %s wrote, want it under the issue %s by the step's",
				fixture.blocks[id], now.ToKind, now.ToID, now.Who.Kind, now.Who.ID, fixture.issue)
		}
	}

	placeUnderDaemonThread(t, database, fixture.blocks["late"], string(refs.ItemMessage), fixture.reply)
	if owed := settled(3 * docs.BlockIndexReconcileInterval); owed != 0 {
		t.Fatalf("daemon_thread_parent=%d after reconciliation tick, want 0: the server placed nothing in %s, three intervals of the block-index reconciliation:\n%s",
			owed, 3*docs.BlockIndexReconcileInterval, process.Output.String())
	}
	if now := daemonThreadPlacement(t, database, fixture.blocks["late"]); now.ToKind != graphstore.KindIssue || now.ToID != fixture.issue || now.Who.ID != docs.DaemonThreadStepActor.ID {
		t.Errorf("the block %s stands under %s %s by a part_of %s %s wrote, want it under the issue %s by the step's",
			fixture.blocks["late"], now.ToKind, now.ToID, now.Who.Kind, now.Who.ID, fixture.issue)
	}
	if starts := process.logLines(daemonThreadStartedMatch); len(starts) != 1 {
		t.Errorf("the server logged %d first runs of the step, want the one after its listen: the tick placed the third node with no restart", len(starts))
	}
	process.Terminate(t)
	process.WaitExit(t, "SIGTERM")
}
