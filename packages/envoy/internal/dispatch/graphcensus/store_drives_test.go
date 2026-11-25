package graphcensus

//ci:builds packages/envoy ./cmd/dispatch-seed seedRunDrives cmdtest.BuildCommand . in ../../../cmd/dispatch-seed, whose binary TestTheSizingCountsEveryTupleTheWritersAppend runs as the seed's three store writers

import (
	"context"
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
	"github.com/sjawhar/envoy/internal/testwait"
)

// StoredKind is one kind of tuple a writer of the store appends: the kind of the node the tuple is
// out of, and the tuple's own kind.
type StoredKind struct{ From, Kind string }

// StoreDrive is one run of writers of the store whose tuples TestTheSizingCountsEveryTupleTheWritersAppend
// reads off graph_tuples (writeTheStore). It is exported, with its fields, for this package's
// external test (imported_drives_test.go), which drives the sites of the API, the document
// service and the eval's seed.
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

// ImportedDrives builds the drives this package's external test package runs: the API's and the
// document service's on one rig, and the eval's seed's, each over an empty database of its own.
// Its value is set by this package's external test package as it starts
// (imported_drives_test.go), and writeTheStore runs the drives it builds beside its own; while it
// is nil, every site only those drives run is listed and not driven, and the test fails naming it.
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

// seedWait bounds the wait for the seed to exit once it is let go; its waits to queue behind a
// hold end on the queue itself (queuedAt).
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
		testwait.UntilPolling(t, 10*time.Millisecond, "the seed to queue behind the hold on "+table+" (the backends queued last read)", func() (int, bool) {
			select {
			case <-process.Exited:
				t.Fatalf("the seed exited before it queued behind the hold on %s; it wrote:\n%s", table, process.Output.String())
			default:
			}
			waiters, err := storetest.LockWaiters(ctx, holder)
			if err != nil {
				t.Fatalf("count the backends queued behind the hold on %s: %v", table, err)
			}
			return waiters, waiters >= 1
		})
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
		// The issues' nodes, which the seed writes before their documents, each document's node,
		// and each document's latest version, indexed as its first: the blocks it states afresh and
		// the references they hold. Every node of the seed is in its project.
		Counts: map[StoredKind][]string{
			{graphstore.KindIssue, graphstore.AttributeVisibility}:    {graphstore.KindIssue},
			{graphstore.KindIssue, graphstore.AttributeState}:         {graphstore.KindIssue},
			{graphstore.KindIssue, graphstore.AttributeProject}:       {graphstore.KindIssue},
			{graphstore.KindArtifact, graphstore.AttributeVisibility}: {graphstore.KindArtifact},
			{graphstore.KindArtifact, graphstore.AttributeState}:      {graphstore.KindArtifact},
			{graphstore.KindArtifact, graphstore.AttributeProject}:    {graphstore.KindArtifact},
			{refs.BlockNodeKind, graphstore.AttributeVisibility}:      {"block"},
			{refs.BlockNodeKind, graphstore.AttributeState}:           {"block"},
			{refs.BlockNodeKind, graphstore.AttributeProject}:         {"block"},
			{refs.BlockNodeKind, refs.TupleInDocument}:                {"block"},
			{refs.BlockNodeKind, graphstore.TuplePartOf}:              {"block"},
			{refs.BlockNodeKind, "mentions"}:                          {"block_ref"},
		},
	}, {
		Name: seedFollowersDrive, Database: database,
		Sites: []string{"cmd/dispatch-seed/write_items.go#writeFollowerNodes", "cmd/dispatch-seed/write_items.go#writeAsks",
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
		Sites: []string{"cmd/dispatch-seed/write_items.go#writeComments", "cmd/dispatch-seed/write_items.go#writeMessages",
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
		// The comments, as the comment path states one, in their project, their one author's
		// node, and the messages' nodes, which the seed writes after its comments.
		Counts: map[StoredKind][]string{
			{graphstore.KindComment, graphstore.AttributeVisibility}: {"comment"},
			{graphstore.KindComment, graphstore.AttributeState}:      {"comment"},
			{graphstore.KindComment, graphstore.AttributeProject}:    {"comment"},
			{graphstore.KindComment, graphstore.TupleAuthoredBy}:     {graphstore.TupleAuthoredBy},
			{graphstore.KindComment, graphstore.TupleAnchoredTo}:     {graphstore.TupleAnchoredTo},
			{graphstore.KindComment, graphstore.TupleOwnedBy}:        {graphstore.TupleOwnedBy},
			{graphstore.KindMessage, graphstore.AttributeVisibility}: {graphstore.KindMessage},
			{graphstore.KindMessage, graphstore.AttributeState}:      {graphstore.KindMessage},
			{graphstore.KindMessage, graphstore.AttributeProject}:    {graphstore.KindMessage},
			{graphstore.KindPerson, graphstore.AttributeVisibility}:  {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:       {graphstore.KindPerson},
		},
	}}
}
