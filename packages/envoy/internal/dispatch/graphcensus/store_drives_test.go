package graphcensus

//ci:builds packages/envoy ./cmd/dispatch-seed seedRunDrives cmdtest.BuildCommand . in ../../../cmd/dispatch-seed, whose binary TestTheSizingCountsEveryTupleTheWritersAppend runs as the seed's three store writers

import (
	"context"
	"encoding/json"
	"maps"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// StoredKind is one kind of tuple a writer of the store appends: the kind of the node the tuple is
// out of, and the tuple's own kind.
type StoredKind struct{ From, Kind string }

// StoreDrive is one run of writers of the store whose tuples TestTheSizingCountsEveryTupleTheWritersAppend
// reads off graph_tuples (writeTheStore). It is exported, with its fields, for this package's
// external test (imported_drives_test.go), which drives the sites of the API and the document
// service.
type StoreDrive struct {
	// Name is the drive as a failure names it: the site it drives, and what it drives that site
	// over where the test drives one site twice.
	Name string
	// Sites are the sites of the register of the store's writers (lockstep.StoreWriteCallSites)
	// whose real code the drive runs: the site Name names, and every listed site that one runs in
	// turn.
	Sites []string
	// Database is the database the drive writes, which its tuples are read off.
	Database *store.Store
	// Ready writes what the drive needs and waits for everything it wrote to land, before the
	// highest tuple is read, so nothing it writes is read as the drive's. Nil readies nothing.
	Ready func(*testing.T)
	// Drive runs the writers, and answers once everything they write has committed.
	Drive func(*testing.T)
	// Counts is, for each kind of tuple the drive appends, the terms of the sizing that count it.
	// Every kind the drive appends must be here, and every kind here must be one it appended.
	Counts map[StoredKind][]string
}

// ImportedDrives builds the drives this package's external test package runs on one rig: the
// API's and the document service's. Its value is set by this package's external test package as
// it starts (imported_drives_test.go), and writeTheStore runs the drives it builds beside its own;
// while it is nil, every site only those drives run is listed and not driven, and the test fails
// naming it.
var ImportedDrives func(*testing.T) []StoreDrive

// requireEveryListedSiteDriven fails on every site the register of the store's writers lists that
// no drive runs, naming it, so a site added to the register is a site this test drives before it
// passes; and on every site a drive says it runs that the register does not list.
func requireEveryListedSiteDriven(t *testing.T, drives []StoreDrive) {
	t.Helper()
	listed := lockstep.StoreWriteCallSites()
	driven := map[string][]string{}
	for _, drive := range drives {
		for _, site := range drive.Sites {
			driven[site] = append(driven[site], drive.Name)
		}
	}
	for _, site := range slices.Sorted(maps.Keys(listed)) {
		if len(driven[site]) == 0 {
			t.Errorf("the register of the store's writers (lockstep.StoreWriteCallSites) lists %s, and no drive of this test runs it: drive its real code here and read off graph_tuples what it appends, so a kind it appends cannot go uncounted",
				site)
		}
	}
	for _, site := range slices.Sorted(maps.Keys(driven)) {
		if _, ok := listed[site]; !ok {
			t.Errorf("%s says it runs %s, which the register of the store's writers (lockstep.StoreWriteCallSites) does not list",
				strings.Join(driven[site], " and "), site)
		}
	}
}

// The drives of graphstore's own writers, each run on its own over an input of this test's: the
// queued forms of the writers of the people and agents Dispatch records (QueueAuthored,
// QueueReached, QueueSessionRecorded), and the cause of a node write held for it (Caused over
// QueueNodes). Each runs as a batch of its own (store.RunWrite) in a transaction of its own, as a
// writer with no write phase runs it. QueueReached and QueueSessionRecorded take their cause in
// hand, as the settle hands them the receipt it appended before them (drivenReceipt); the write
// QueueNodes held for its cause is caused at the sentinel (graphstore.Caused), as a write no event
// recorded is.
const (
	authoredDrive        = "graphstore/people.go#QueueAuthored"
	reachedDrive         = "graphstore/people.go#QueueReached"
	sessionRecordedDrive = "graphstore/people.go#QueueSessionRecorded"
	heldNodeDrive        = "graphstore/write.go#Caused, over a node QueueNodes held for its cause"
)

// drivenReceipt is the event the reached and the recorded drives state their session under, as
// the receipt a settle appends before the session it records. No row reads it back: a drive is
// read for the kinds of tuple it appends.
const drivenReceipt = int64(9700001)

// graphstoreDrives are the drives of graphstore's own writers run on their own, over a database
// of their own: who wrote a comment a build before the store wrote, as "Dana", whose canonical
// login is dana, so the write states the recorded_as alias beside the authored_by; a sent
// attempt's session; a session a write records; and a person's node held for its cause.
func graphstoreDrives(t *testing.T) []StoreDrive {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	inTransaction := func(t *testing.T, what string, write func(tx pgx.Tx) error) {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %s: %v", what, err)
		}
		defer tx.Rollback(ctx)
		if err := write(tx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %s: %v", what, err)
		}
	}
	dana := model.Actor{Kind: "user", ID: "Dana"}
	var comment string
	var commented time.Time
	return []StoreDrive{{
		Name: authoredDrive, Sites: []string{"graphstore/people.go#QueueAuthored"},
		Database: database,
		Ready: func(t *testing.T) {
			t.Helper()
			if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'dana')`); err != nil {
				t.Fatalf("seed project CORE: %v", err)
			}
			if err := database.Pool.QueryRow(ctx, `
				with issue as (
					insert into issues (key, project_key, number, title, created_by, rank)
					values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"Dana"}', 'U')
					returning key
				)
				insert into comments (issue_key, author, body) select key, '{"kind":"user","id":"Dana"}', 'A remark.' from issue
				returning id::text, created_at
			`).Scan(&comment, &commented); err != nil {
				t.Fatalf("write the comment Dana's write is recorded for: %v", err)
			}
		},
		Drive: func(t *testing.T) {
			inTransaction(t, "record who wrote the comment", func(tx pgx.Tx) error {
				return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
					return graphstore.QueueAuthored(b, graphstore.KindComment, comment, dana, commented, graphstore.NoEvent)
				})
			})
		},
		Counts: map[StoredKind][]string{
			{graphstore.KindComment, graphstore.TupleAuthoredBy}:    {graphstore.TupleAuthoredBy},
			{graphstore.KindPerson, graphstore.AttributeVisibility}: {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:      {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.TupleRecordedAs}:     {"author_alias"},
		},
	}, {
		Name: reachedDrive, Database: database,
		Sites: []string{"graphstore/people.go#QueueReached", "graphstore/people.go#QueueSessionRecorded"},
		Drive: func(t *testing.T) {
			inTransaction(t, "record the session a sent attempt reached", func(tx pgx.Tx) error {
				return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
					return graphstore.QueueReached(b, model.DeliveryStateSent, "ses_reached", dana, time.Now(), drivenReceipt)
				})
			})
		},
		Counts: map[StoredKind][]string{
			{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
		},
	}, {
		Name: sessionRecordedDrive, Database: database,
		Sites: []string{"graphstore/people.go#QueueSessionRecorded"},
		Drive: func(t *testing.T) {
			inTransaction(t, "record a session a write names", func(tx pgx.Tx) error {
				return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
					return graphstore.QueueSessionRecorded(b, "ses_recorded", dana, time.Now(), drivenReceipt)
				})
			})
		},
		Counts: map[StoredKind][]string{
			{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
		},
	}, {
		Name: heldNodeDrive, Database: database,
		Sites: []string{"graphstore/write.go#Caused"},
		Drive: func(t *testing.T) {
			inTransaction(t, "write a person's node held for its cause", func(tx pgx.Tx) error {
				write := graphstore.PlanUncaused(
					[]graphstore.Node{graphstore.PersonNode("erin", dana, time.Now(), graphstore.NoEvent)}, nil)
				if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
					graphstore.QueueNodes(b, write)
					return nil
				}); err != nil {
					return err
				}
				return graphstore.Caused(ctx, tx, write, graphstore.NoEvent)
			})
		},
		Counts: map[StoredKind][]string{
			{graphstore.KindPerson, graphstore.AttributeVisibility}: {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:      {graphstore.KindPerson},
		},
	}}
}

// The drives of the lens catalogue's writer, of the backfill's passes over what a process of the
// release before leaves of a run and of a prevented_by, and of the block-index tick's sweep of the
// lenses such a process leaves stale when it removes a document, each a site no drive over the
// seeded history or the API runs.
const (
	addLensKindDrive      = "graphstore/lens_catalogue.go#AddLensKind"
	owedRunDrive          = "graphcensus/backfill_runs.go#writeOwedRun"
	runFramesDrive        = "graphcensus/backfill_runs.go#writeRunFrames"
	goneRunDrive          = "graphcensus/backfill_runs.go#writeGoneRun"
	endedPreventionsDrive = "graphcensus/backfill_preventions.go#writeEndedPreventions"
	staleLensSweepDrive   = "graphcensus/orphans.go#StateStaleLensesGone"
)

// runAndCatalogueDrives are those drives, over a database of their own, one after another: a kind
// added to the lens catalogue as `envoy-dispatch lens-kind-add` adds one, its lens_kind node and
// the constraint node of its source, which the term lens_kind counts; then the backfill's passes
// over the rows a process of the release before writes of a run during a roll (olderPodRun): the
// node of a run it recorded with no node, written through graphstore.WriteRun with a frame value of
// its one frame, which the terms interaction and frame count, and the node of its asker, the first
// write of alice in this database, which the term person counts; the value of a frame it appended
// to a run this build gave its node, which the term frame counts; and the gone state of a run whose
// row it deleted, which the term interaction_gone counts. Then the backfill's retraction of the
// prevented_by at a check of a document that process retired and at a check its version dropped
// (seedEndedPreventions), which the term risk counts. Last, the block-index tick's sweep of the
// stale lenses (StateStaleLensesGone) over one lens on a document that process removed, stating
// nothing (seedOrphan): the lens's gone state, which the term lens counts.
func runAndCatalogueDrives(t *testing.T) []StoreDrive {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	pass := func(backfill func(context.Context, *store.Store) (KindBackfill, error)) func(*testing.T) {
		return func(t *testing.T) {
			t.Helper()
			if done, err := backfill(ctx, database); err != nil {
				t.Fatalf("backfill the %s kind: %v", done.Kind, err)
			} else if done.Rows == 0 {
				t.Fatalf("the backfill of the %s kind wrote nothing, where the drive readied a row it owes", done.Kind)
			}
		}
	}
	return []StoreDrive{{
		Name: addLensKindDrive, Sites: []string{"graphstore/lens_catalogue.go#AddLensKind"},
		Database: database,
		Drive: func(t *testing.T) {
			if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
				_, err := graphstore.AddLensKind(ctx, tx, graphstore.LensKind{
					ID: "sizing-kind", Words: "Weigh what the store holds against what it costs.",
					Source: "The sizing of the store", Reference: "its tuples a day",
				}, time.Now().UTC())
				return err
			}); err != nil {
				t.Fatalf("add a kind to the lens catalogue: %v", err)
			}
		},
		Counts: map[StoredKind][]string{
			{graphstore.KindLensKind, graphstore.AttributeVisibility}:   {graphstore.KindLensKind},
			{graphstore.KindLensKind, graphstore.AttributeState}:        {graphstore.KindLensKind},
			{graphstore.KindLensKind, graphstore.AttributeWords}:        {graphstore.KindLensKind},
			{graphstore.KindLensKind, graphstore.TupleConstrainedBy}:    {graphstore.KindLensKind},
			{graphstore.KindConstraint, graphstore.AttributeVisibility}: {graphstore.KindLensKind},
			{graphstore.KindConstraint, graphstore.AttributeState}:      {graphstore.KindLensKind},
			{graphstore.KindConstraint, graphstore.AttributeStandard}:   {graphstore.KindLensKind},
			{graphstore.KindConstraint, graphstore.AttributeReference}:  {graphstore.KindLensKind},
		},
	}, {
		Name: owedRunDrive, Sites: []string{owedRunDrive, "graphstore/interaction.go#WriteRun"},
		Database: database,
		Ready:    func(t *testing.T) { olderPodRun(t, database, alice, "CORE", "who answers this?", "Half an answer.") },
		Drive:    pass(backfillRuns),
		Counts: map[StoredKind][]string{
			{graphstore.KindInteraction, graphstore.AttributeVisibility}: {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeState}:      {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeProject}:    {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeWords}:      {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeEndsAt}:     {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeSource}:     {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.TupleAuthoredBy}:     {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeFrame}:      {graphstore.AttributeFrame},
			{graphstore.KindPerson, graphstore.AttributeVisibility}:      {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:           {graphstore.KindPerson},
		},
	}, {
		Name: runFramesDrive, Sites: []string{runFramesDrive},
		Database: database,
		Ready: func(t *testing.T) {
			framed := olderPodRun(t, database, alice, "", "and this one?", "One ")
			giveRunItsNode(t, database, framed)
			appendOlderPodFrame(t, database, framed, 2, "two.")
		},
		Drive:  pass(backfillRunFrames),
		Counts: map[StoredKind][]string{{graphstore.KindInteraction, graphstore.AttributeFrame}: {graphstore.AttributeFrame}},
	}, {
		Name: goneRunDrive, Sites: []string{goneRunDrive},
		Database: database,
		Ready: func(t *testing.T) {
			deleted := olderPodRun(t, database, alice, "", "asked and deleted an hour on", "Gone ")
			giveRunItsNode(t, database, deleted)
			if _, err := database.Pool.Exec(ctx, `delete from going_requests where id = $1::uuid`, deleted); err != nil {
				t.Fatalf("delete run %s as an older pod's sweep does: %v", deleted, err)
			}
		},
		Drive:  pass(backfillGoneRuns),
		Counts: map[StoredKind][]string{{graphstore.KindInteraction, graphstore.AttributeState}: {"interaction_gone"}},
	}, {
		Name: endedPreventionsDrive, Sites: []string{endedPreventionsDrive},
		Database: database,
		Ready:    func(t *testing.T) { seedEndedPreventions(t, database) },
		Drive:    pass(backfillEndedPreventions),
		Counts:   map[StoredKind][]string{{graphstore.KindRisk, graphstore.TuplePreventedBy}: {graphstore.KindRisk}},
	}, {
		Name: staleLensSweepDrive, Sites: []string{"graphcensus/orphans.go#StateStaleLensesGone", "graphstore/write.go#WriteState"},
		Database: database,
		Ready: func(t *testing.T) {
			seedOrphan(t, database, "lensed-md", "00000000-0000-4000-8000-0000000005a1")
		},
		Drive: func(t *testing.T) {
			if stated, err := StateStaleLensesGone(ctx, database); err != nil || stated != 1 {
				t.Fatalf("the sweep of the stale lenses stated %d nodes gone (%v), want the one stale lens", stated, err)
			}
		},
		Counts: map[StoredKind][]string{{graphstore.KindLens, graphstore.AttributeState}: {graphstore.KindLens}},
	}}
}

// The drives of the boot step's ask passes (backfillAsks, backfillAskFollowers), each over the
// rows a build before the store wrote.
const (
	askBatchDrive         = "graphcensus/backfill_asks.go#writeAskBatch"
	askFollowerBatchDrive = "graphcensus/backfill_asks.go#writeAskFollowerBatch"
)

// askBackfillDrives are the drives of the boot step's two ask passes, over a database of their
// own: an ask of ASKS-1 that a session asked and the person bob answered, written by a build
// before the store, whose node the object pass writes first (backfillObjectNodes), as the boot
// step's first phase runs it (BackfillAsks), and then a follow of it that build recorded. The asks
// pass states who asked it, its answer and its answerer's node; the follower pass the follower's
// node and its followed_by.
func askBackfillDrives(t *testing.T) []StoreDrive {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	var ask string
	return []StoreDrive{{
		Name: askBatchDrive, Sites: []string{askBatchDrive, "graphstore/write.go#Write"},
		Database: database,
		Ready: func(t *testing.T) {
			t.Helper()
			text := "The node store."
			answer, err := json.Marshal(model.AskAnswer{User: "bob", Selected: []string{}, Text: &text, At: time.Now().UTC()})
			if err != nil {
				t.Fatalf("encode bob's answer: %v", err)
			}
			if err := database.Pool.QueryRow(ctx, `
				with project as (
					insert into projects (key, name, default_assignee) values ('ASKS', 'Asks', 'bob') returning key
				), issue as (
					insert into issues (key, project_key, number, title, created_by, rank)
					select 'ASKS-1', key, 1, 'Asked', '{"kind":"user","id":"bob"}', 'U' from project
					returning key
				)
				insert into asks (issue_key, author, question, state, answer)
				select key, '{"kind":"session","id":"ses_asked"}', 'Which store?', 'answered', $1::jsonb from issue
				returning id::text
			`, answer).Scan(&ask); err != nil {
				t.Fatalf("write the answered ask a build before the store wrote: %v", err)
			}
			if _, err := backfillObjectNodes(ctx, database); err != nil {
				t.Fatalf("write the nodes of ASKS-1 and its ask: %v", err)
			}
		},
		Drive: func(t *testing.T) {
			if done, err := backfillAsks(ctx, database); err != nil || done.Rows != 1 {
				t.Fatalf("backfill the ask (%d rows): %v", done.Rows, err)
			}
		},
		// Who asked the ask, which ask.opened states, the answer bob gave, and the nodes of the
		// session that asked and of bob, each first recorded by this write.
		Counts: map[StoredKind][]string{
			{graphstore.KindAsk, graphstore.TupleAuthoredBy}:         {"ask_opened"},
			{graphstore.KindAsk, graphstore.TupleAnswerState}:        {"answer"},
			{graphstore.KindAsk, graphstore.TupleAnswer}:             {"answer"},
			{graphstore.KindAsk, graphstore.TupleSelected}:           {"answer"},
			{graphstore.KindAsk, graphstore.TupleAnsweredAt}:         {"answer"},
			{graphstore.KindAsk, graphstore.TupleAnsweredBy}:         {"answer"},
			{graphstore.KindAsk, graphstore.TupleDecidedBy}:          {"answer"},
			{graphstore.KindPerson, graphstore.AttributeVisibility}:  {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:       {graphstore.KindPerson},
			{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
		},
	}, {
		Name: askFollowerBatchDrive, Sites: []string{askFollowerBatchDrive, "graphstore/write.go#Write"},
		Database: database,
		Ready: func(t *testing.T) {
			t.Helper()
			if _, err := database.Pool.Exec(ctx, `insert into ask_followers (ask_id, session_id) values ($1::uuid, 'ses_follows')`, ask); err != nil {
				t.Fatalf("write the follow a build before the store recorded: %v", err)
			}
		},
		Drive: func(t *testing.T) {
			if done, err := backfillAskFollowers(ctx, database); err != nil || done.Rows != 1 {
				t.Fatalf("backfill the follower (%d rows): %v", done.Rows, err)
			}
		},
		Counts: map[StoredKind][]string{
			{graphstore.KindAsk, graphstore.TupleFollowedBy}:         {"ask_follower"},
			{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
		},
	}}
}

// The seed's three sites, each a drive of its own over one run of the seed.
const (
	seedDocumentsDrive = "cmd/dispatch-seed/write_docs.go#writeDocument"
	seedFollowersDrive = "cmd/dispatch-seed/write_items.go#writeFollowerNodes"
	seedCommentsDrive  = "cmd/dispatch-seed/write_items.go#writeComments"
)

// seedScale is the scale the seed runs at: the smallest of those tried at which it runs whole,
// writing four issues, eight documents, sixteen asks with their followers and seventy-four
// comments, the asks and the comments each in one batch (cmd/dispatch-seed's batchItems). Every
// count of its data set is at least one at any scale (its scaled), but below this the seed fails:
// at 0.000175, 0.00015 and 0.0001 its report reads a parent with no children and exits 1, and at
// 0.000001, with one issue, its issue links index past the issues it has.
const seedScale = "0.0002"

// seedWait bounds each wait on the seed: for it to queue behind a hold, and for it to exit.
const seedWait = 2 * time.Minute

// seedStatementSpace is a run of whitespace in a statement, which the seed's waits fold.
var seedStatementSpace = regexp.MustCompile(`\s+`)

// seedRunDrives are the seed's three sites, each a drive of its own over one run of the seed on
// an empty database of its own. The seed is package main, which no package can import, so the
// test builds its binary (cmdtest.BuildCommand) and runs it as `go run ./cmd/dispatch-seed` would,
// at seedScale with --no-migrate, storetest's database being migrated already.
//
// One run writes all three, in this order and each in transactions of its own: its documents
// (writeDocument), then its asks with the session nodes of their followers (writeFollowerNodes),
// then its comments (writeComments). Before the run starts the test holds the asks and the
// comments tables in share mode, which no read and no row lock conflicts with and every insert
// does, so the seed writes its documents and queues at its first insert into asks: what it
// appended by then is writeDocument's. Let go, it writes its asks and their followers' nodes and
// queues at its first insert into comments, whose batch's store write comes before that insert
// and has not committed: what it appended by then is writeFollowerNodes'. Let go of that hold, it
// writes its comments and the rest of its data set, which no other listed site writes, and exits:
// what it appended since is writeComments'. A scale too small to reach one of the three would
// leave that drive appending none of what its Counts say it does, and the test fails on it.
func seedRunDrives(t *testing.T) []StoreDrive {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	var process *cmdtest.Process
	var binary string
	var asks, comments pgx.Tx
	hold := func(t *testing.T, table string) pgx.Tx {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the hold on %s: %v", table, err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(ctx, `lock table `+pgx.Identifier{table}.Sanitize()+` in share mode`); err != nil {
			t.Fatalf("hold %s in share mode: %v", table, err)
		}
		return tx
	}
	// queuedAt waits for the seed to queue behind holder, and fails if it exits first or waits at
	// any statement but one that starts as statement does.
	queuedAt := func(t *testing.T, holder pgx.Tx, table, statement string) {
		t.Helper()
		deadline := time.Now().Add(seedWait)
		for storetest.WaitForLockWaiters(t, holder, 1, 100*time.Millisecond) < 1 {
			select {
			case <-process.Exited:
				t.Fatalf("the seed exited before it queued behind the hold on %s; it wrote:\n%s", table, process.Output.String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("the seed never queued behind the hold on %s within %s; it wrote:\n%s", table, seedWait, process.Output.String())
			}
		}
		var waited string
		if err := database.Pool.QueryRow(ctx, `
			select query from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&waited); err != nil {
			t.Fatalf("read the statement the seed waits at behind the hold on %s: %v", table, err)
		}
		if folded := strings.TrimSpace(seedStatementSpace.ReplaceAllString(strings.ToLower(waited), " ")); !strings.HasPrefix(folded, statement) {
			t.Fatalf("the seed waits behind the hold on %s at %q, not at its first %s: what it appended so far is not the sites before it alone",
				table, folded, statement)
		}
	}
	return []StoreDrive{{
		Name: seedDocumentsDrive, Database: database,
		Sites: []string{"cmd/dispatch-seed/write_docs.go#writeDocument", "cmd/dispatch-seed/write_core.go#writeIssues",
			"refs/blocks.go#Queue", "graphstore/write.go#Caused"},
		Ready: func(t *testing.T) {
			t.Helper()
			binary = cmdtest.BuildCommand(t, cmdtest.Command{Name: "dispatch-seed", Dir: "../../../cmd/dispatch-seed"})
			asks, comments = hold(t, "asks"), hold(t, "comments")
		},
		Drive: func(t *testing.T) {
			t.Helper()
			process = cmdtest.Start(t, "dispatch-seed", exec.Command(binary,
				"--database-url", database.Pool.Config().ConnString(), "--scale", seedScale, "--no-migrate"))
			queuedAt(t, asks, "asks", "insert into asks")
		},
		// The issues' nodes, which the seed writes before their documents, with a child's child_of
		// its parent, each document's node with its part_of its issue and its references, and each
		// document's latest version, indexed as its first: the version's node, and the blocks it
		// states afresh with their values and the references they hold. Every node of the seed is
		// in its project.
		Counts: map[StoredKind][]string{
			{graphstore.KindIssue, graphstore.AttributeVisibility}:    {graphstore.KindIssue},
			{graphstore.KindIssue, graphstore.AttributeState}:         {graphstore.KindIssue},
			{graphstore.KindIssue, graphstore.AttributeProject}:       {graphstore.KindIssue},
			{graphstore.KindIssue, graphstore.TupleChildOf}:           {"issue_relation"},
			{graphstore.KindArtifact, graphstore.AttributeVisibility}: {graphstore.KindArtifact},
			{graphstore.KindArtifact, graphstore.AttributeState}:      {graphstore.KindArtifact},
			{graphstore.KindArtifact, graphstore.AttributeProject}:    {graphstore.KindArtifact},
			{graphstore.KindArtifact, graphstore.TuplePartOf}:         {"document_part_of"},
			{graphstore.KindArtifact, graphstore.TupleMentions}:       {"document_ref"},
			{graphstore.KindVersion, graphstore.AttributeVisibility}:  {graphstore.KindVersion},
			{graphstore.KindVersion, graphstore.AttributeState}:       {graphstore.KindVersion},
			{graphstore.KindVersion, graphstore.AttributeProject}:     {graphstore.KindVersion},
			{graphstore.KindVersion, graphstore.TuplePartOf}:          {graphstore.KindVersion},
			{graphstore.KindVersion, graphstore.AttributeNumber}:      {graphstore.KindVersion},
			{graphstore.KindVersion, graphstore.AttributeCreatedAt}:   {graphstore.KindVersion},
			{refs.BlockNodeKind, graphstore.AttributeVisibility}:      {"block"},
			{refs.BlockNodeKind, graphstore.AttributeState}:           {"block"},
			{refs.BlockNodeKind, graphstore.AttributeProject}:         {"block"},
			{refs.BlockNodeKind, refs.TupleInDocument}:                {"block"},
			{refs.BlockNodeKind, graphstore.TuplePartOf}:              {"block"},
			{refs.BlockNodeKind, refs.AttributeRank}:                  {"block_values"},
			{refs.BlockNodeKind, refs.AttributePlacement}:             {"block_values"},
			{refs.BlockNodeKind, refs.TupleTextParent}:                {"block_values"},
			{refs.BlockNodeKind, refs.AttributeTextRank}:              {"block_values"},
			{refs.BlockNodeKind, graphstore.AttributeText}:            {"block_text"},
			{refs.BlockNodeKind, "mentions"}:                          {"block_ref"},
		},
	}, {
		Name: seedFollowersDrive, Database: database,
		Sites: []string{"cmd/dispatch-seed/write_items.go#writeFollowerNodes", "cmd/dispatch-seed/write_items.go#writeItemNodes",
			"graphstore/write.go#Write"},
		Drive: func(t *testing.T) {
			t.Helper()
			if err := asks.Rollback(ctx); err != nil {
				t.Fatalf("let go of the hold on asks: %v", err)
			}
			queuedAt(t, comments, "comments", "insert into comments")
		},
		// The asks' nodes, which their batch writes before its rows, and the session nodes of
		// their followers.
		Counts: map[StoredKind][]string{
			{graphstore.KindAsk, graphstore.AttributeVisibility}:     {graphstore.KindAsk},
			{graphstore.KindAsk, graphstore.AttributeState}:          {graphstore.KindAsk},
			{graphstore.KindAsk, graphstore.AttributeProject}:        {graphstore.KindAsk},
			{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
		},
	}, {
		Name: seedCommentsDrive, Database: database,
		Sites: []string{"cmd/dispatch-seed/write_items.go#writeComments", "cmd/dispatch-seed/write_items.go#writeItemNodes",
			"graphstore/write.go#Write"},
		Drive: func(t *testing.T) {
			t.Helper()
			if err := comments.Rollback(ctx); err != nil {
				t.Fatalf("let go of the hold on comments: %v", err)
			}
			select {
			case <-process.Exited:
			case <-time.After(seedWait):
				t.Fatalf("the seed was still running %s after it was let go; it wrote:\n%s", seedWait, process.Output.String())
			}
			if code := process.Cmd.ProcessState.ExitCode(); code != 0 {
				t.Fatalf("the seed exited %d; it wrote:\n%s", code, process.Output.String())
			}
		},
		// The comments, as the comment path states one, in their project, each anchored one's
		// anchored_in its anchor's block, their one author's node, and the messages' nodes, which
		// the seed writes after its comments.
		Counts: map[StoredKind][]string{
			{graphstore.KindComment, graphstore.AttributeVisibility}: {"comment"},
			{graphstore.KindComment, graphstore.AttributeState}:      {"comment"},
			{graphstore.KindComment, graphstore.AttributeProject}:    {"comment"},
			{graphstore.KindComment, graphstore.TupleAuthoredBy}:     {graphstore.TupleAuthoredBy},
			{graphstore.KindComment, graphstore.TupleAnchoredTo}:     {graphstore.TupleAnchoredTo},
			{graphstore.KindComment, graphstore.TupleAnchoredIn}:     {"anchor_block"},
			{graphstore.KindComment, graphstore.TupleOwnedBy}:        {graphstore.TupleOwnedBy},
			{graphstore.KindMessage, graphstore.AttributeVisibility}: {graphstore.KindMessage},
			{graphstore.KindMessage, graphstore.AttributeState}:      {graphstore.KindMessage},
			{graphstore.KindMessage, graphstore.AttributeProject}:    {graphstore.KindMessage},
			{graphstore.KindPerson, graphstore.AttributeVisibility}:  {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:       {graphstore.KindPerson},
		},
	}}
}
