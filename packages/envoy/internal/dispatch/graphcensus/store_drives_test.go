package graphcensus

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
// external test (imported_drives_test.go), which drives the sites whose packages import this one.
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

// ImportedDrives builds the drives of the sites whose packages import this one, and so cannot be
// imported by its internal tests: the API's, the document service's and the erase's, since docs
// imports graphcensus (the block-index tick queues CommentWrite), api imports docs and grapherase
// imports api. Its value is set by this package's external test package as it starts
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

// The drives of graphstore's own writers that no site of another package runs: Authored, Reached,
// SessionRecorded and WriteUncaused. Each runs in a transaction of its own, as a writer with no
// write phase calls it; a write held for its cause (Reached, SessionRecorded, WriteUncaused) is
// then caused at the sentinel (graphstore.Caused), as a write no event recorded is.
const (
	authoredDrive        = "graphstore/people.go#Authored"
	reachedDrive         = "graphstore/people.go#Reached"
	sessionRecordedDrive = "graphstore/people.go#SessionRecorded"
	writeUncausedDrive   = "graphstore/write.go#WriteUncaused"
)

// graphstoreDrives are the drives of graphstore's own writers that no other site runs, over a
// database of their own: who wrote a comment a build before the store wrote, as "Dana", whose
// canonical login is dana, so the write states the recorded_as alias beside the authored_by; a
// sent attempt's session; a session a write records; and a person's node held for its cause.
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
		Name: authoredDrive, Sites: []string{"graphstore/people.go#Authored", "graphstore/people.go#QueueAuthored"},
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
				return graphstore.Authored(ctx, tx, graphstore.KindComment, comment, dana, commented, graphstore.NoEvent)
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
		Sites: []string{"graphstore/people.go#Reached", "graphstore/people.go#QueueReached",
			"graphstore/people.go#QueueSessionRecorded", "graphstore/write.go#Caused"},
		Drive: func(t *testing.T) {
			inTransaction(t, "record the session a sent attempt reached", func(tx pgx.Tx) error {
				write, err := graphstore.Reached(ctx, tx, model.DeliveryStateSent, "ses_reached", dana, time.Now())
				if err != nil {
					return err
				}
				return graphstore.Caused(ctx, tx, write, graphstore.NoEvent)
			})
		},
		Counts: map[StoredKind][]string{
			{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
		},
	}, {
		Name: sessionRecordedDrive, Database: database,
		Sites: []string{"graphstore/people.go#SessionRecorded", "graphstore/people.go#QueueSessionRecorded",
			"graphstore/write.go#Caused"},
		Drive: func(t *testing.T) {
			inTransaction(t, "record a session a write names", func(tx pgx.Tx) error {
				write, err := graphstore.SessionRecorded(ctx, tx, "ses_recorded", dana, time.Now(), graphstore.NoEvent)
				if err != nil {
					return err
				}
				return graphstore.Caused(ctx, tx, write, graphstore.NoEvent)
			})
		},
		Counts: map[StoredKind][]string{
			{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
		},
	}, {
		Name: writeUncausedDrive, Database: database,
		Sites: []string{"graphstore/write.go#WriteUncaused", "graphstore/write.go#Caused"},
		Drive: func(t *testing.T) {
			inTransaction(t, "write a person's node held for its cause", func(tx pgx.Tx) error {
				write, err := graphstore.WriteUncaused(ctx, tx,
					[]graphstore.Node{graphstore.PersonNode("erin", dana, time.Now(), graphstore.NoEvent)}, nil)
				if err != nil {
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

// The drives of the boot step's ask passes (backfillAsks, backfillAskFollowers), each over the
// rows a build before the store wrote.
const (
	askBatchDrive         = "graphcensus/backfill.go#writeAskBatch"
	askFollowerBatchDrive = "graphcensus/backfill.go#writeAskFollowerBatch"
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
		Sites: []string{"cmd/dispatch-seed/write_items.go#writeFollowerNodes", "cmd/dispatch-seed/write_items.go#writeItemNodes",
			"graphstore/write.go#Write"},
		Drive: func(t *testing.T) {
			t.Helper()
			if err := asks.Rollback(ctx); err != nil {
				t.Fatalf("let go of the hold on asks: %v", err)
			}
			queuedAt(t, comments, "comments", "insert into comments")
		},
		// The asks' nodes, which their batch writes before its rows, and the session nodes of their
		// followers; the questions' body blocks with their links and the asks' citations; and the
		// part the person an ask reaches takes in it with the read mark it stands at, whose first
		// tuple makes that person's node (graphstore.ToPerson).
		Counts: map[StoredKind][]string{
			{graphstore.KindAsk, graphstore.AttributeVisibility}:     {graphstore.KindAsk},
			{graphstore.KindAsk, graphstore.AttributeState}:          {graphstore.KindAsk},
			{graphstore.KindAsk, graphstore.AttributeProject}:        {graphstore.KindAsk},
			{graphstore.KindAsk, graphstore.TupleMentions}:           {"citation"},
			{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
			{graphstore.KindPerson, graphstore.AttributeVisibility}:  {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:       {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.TupleParticipatesIn}:  {"participation"},
			{graphstore.KindPerson, graphstore.TupleReadThrough}:     {"read_mark"},
			{refs.BlockNodeKind, graphstore.AttributeVisibility}:     {"body_block"},
			{refs.BlockNodeKind, graphstore.AttributeState}:          {"body_block"},
			{refs.BlockNodeKind, graphstore.AttributeProject}:        {"body_block"},
			{refs.BlockNodeKind, graphstore.TuplePartOf}:             {"body_block"},
			{refs.BlockNodeKind, graphstore.TupleMentions}:           {"body_block_ref"},
		},
	}, {
		Name: seedCommentsDrive, Database: database,
		Sites: []string{"cmd/dispatch-seed/write_items.go#writeComments", "cmd/dispatch-seed/write_items.go#writeItemNodes",
			"cmd/dispatch-seed/write_items.go#stateAtSentinel", "cmd/dispatch-seed/write_items.go#writeMessages",
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
		// The comments, as the comment path states one, in their project, their meaning tuples and
		// citations, and the messages, which the seed writes after its comments, with their author,
		// parent, meaning tuples and citations; the body blocks of both with their links, and each
		// author's part in the thread written to with the read mark the write moves.
		Counts: map[StoredKind][]string{
			{graphstore.KindComment, graphstore.AttributeVisibility}: {"comment"},
			{graphstore.KindComment, graphstore.AttributeState}:      {"comment"},
			{graphstore.KindComment, graphstore.AttributeProject}:    {"comment"},
			{graphstore.KindComment, graphstore.TupleAuthoredBy}:     {graphstore.TupleAuthoredBy},
			{graphstore.KindComment, graphstore.TupleAnchoredTo}:     {graphstore.TupleAnchoredTo},
			{graphstore.KindComment, graphstore.TupleOwnedBy}:        {graphstore.TupleOwnedBy},
			{graphstore.KindComment, graphstore.TupleAnswers}:        {"reply"},
			{graphstore.KindComment, graphstore.TupleRepliesTo}:      {"reply"},
			{graphstore.KindComment, graphstore.TuplePartOf}:         {"reply"},
			{graphstore.KindComment, graphstore.TupleMentions}:       {"citation"},
			{graphstore.KindMessage, graphstore.AttributeVisibility}: {graphstore.KindMessage},
			{graphstore.KindMessage, graphstore.AttributeState}:      {graphstore.KindMessage},
			{graphstore.KindMessage, graphstore.AttributeProject}:    {graphstore.KindMessage},
			{graphstore.KindMessage, graphstore.TupleAuthoredBy}:     {"message_relation"},
			{graphstore.KindMessage, graphstore.TuplePartOf}:         {"message_relation", "reply"},
			{graphstore.KindMessage, graphstore.TupleRepliesTo}:      {"reply"},
			{graphstore.KindMessage, graphstore.TupleMentions}:       {"citation"},
			{refs.BlockNodeKind, graphstore.AttributeVisibility}:     {"body_block"},
			{refs.BlockNodeKind, graphstore.AttributeState}:          {"body_block"},
			{refs.BlockNodeKind, graphstore.AttributeProject}:        {"body_block"},
			{refs.BlockNodeKind, graphstore.TuplePartOf}:             {"body_block"},
			{refs.BlockNodeKind, graphstore.TupleMentions}:           {"body_block_ref"},
			{graphstore.KindPerson, graphstore.TupleParticipatesIn}:  {"participation"},
			{graphstore.KindPerson, graphstore.TupleReadThrough}:     {"read_mark"},
		},
	}}
}
