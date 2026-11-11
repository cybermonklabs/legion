package main

// The store census end to end, from before the migration that made the store
// (dispatch://INFRA-1149's c-census). The test is TestTheStoreCensusReadsZeroDifferences below,
// and everything else in this file is what it drives.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// censusSigningKey is the one DISPATCH_SIGNING_KEY every process of this run carries: the older
// build and this one serve the same database here, and a process whose key a live peer does not
// share refuses to start (processes.Register).
const censusSigningKey = "store-census-signing-key"

// censusKinds are the kinds this child moves, as the census names them. Each must carry a
// population in this run: a census that checked no row of a kind proves nothing about it, so a
// kind that checks zero rows fails this test with that kind reported. The list is the test's
// own, so a kind the census stops checking is read by a person before this test agrees with it.
var censusKinds = []string{"comment", "block", "block_ref", "person", "session", "role", "page"}

// censusRole is the role this run addresses, and censusSession the session that writes one of
// its comments. The role is addressed through this build alone: the older build records the
// delivery its mention asks for and makes no node, and the node of a role is the publisher's to
// write (outbox.deliveredGraph).
const (
	censusRole    = "store-census-reader"
	censusSession = "019a1c0b-3333-7000-8000-000000000001"
)

// inventoryRow is one row of the inventory this run keeps: what the older build wrote, as that
// build's own schema holds it. Identity is the row's key, Address the node the store must hold
// for it, Author who wrote it, Digest the digest of its text and Parent what holds it.
type inventoryRow struct {
	Kind     string `json:"kind"`
	Identity string `json:"identity"`
	Address  string `json:"address"`
	Author   string `json:"author"`
	Digest   string `json:"digest"`
	Parent   string `json:"parent"`
}

// The store census reads zero differences, and the rows it reads are the rows that were there
// before the store existed.
//
// Two products of one transformation can agree after both lose the same row, so this run starts
// before the migration. The server of the commit this branch forked from is built and booted
// against a database of the run's own, so that build's own migrations run, and its own API
// writes the comments, the document blocks and the block references. An inventory of those rows
// is then read out of the database by this test's own SQL, over that build's own columns, and
// kept as a file: no code of this branch produces it.
//
// This build then boots over the same database, runs this child's migrations and its backfill,
// and is held to that inventory twice - once for the store and once for the projections. The two
// comparisons are reported apart, because a row the migration lost shows in the first and never
// in the second. Only then does `envoy-dispatch census --store` run, which is the
// store-to-projection comparison: it must read a population for every kind this child moves (a
// kind that checked zero rows fails this run, by name) and zero differences in both directions,
// over a database holding a deleted block, whose node is gone and whose row is not there.
//
// Last, the two builds answer the same reads over the same database, field for field, which is a
// different question from store-to-projection equality: both sides can agree while the answer a
// reader gets has changed.
func TestTheStoreCensusReadsZeroDifferences(t *testing.T) {
	ctx := context.Background()
	databaseURL := censusDatabase(t)
	older := baseBuild(t)

	// ───── before the migration: the older build writes the rows ─────
	before := startDispatchProcessWith(t, databaseURL, dispatchOptions{
		Binary: older, SigningKey: censusSigningKey,
	})
	before.waitHealthy(t)
	seeded := seedThroughTheOlderBuild(t, before)
	inventory := readInventory(t, ctx, databaseURL)
	keepInventory(t, inventory)
	requireInventoryCarriesEveryKind(t, inventory)
	before.Cmd.Process.Signal(os.Interrupt)
	<-before.Exited

	// ───── this build migrates the same database and backfills it ─────
	_, natsURL := testnats.Start(t)
	now := startDispatchProcessWith(t, databaseURL, dispatchOptions{
		NATSURL: natsURL, SigningKey: censusSigningKey,
	})
	now.waitHealthy(t)
	runBackfill(t, databaseURL)
	waitForTheBlockIndex(t, ctx, databaseURL)
	addressTheRole(t, now, seeded.issue)
	deleteABlockThroughThisBuild(t, ctx, now, databaseURL)

	// ───── the two comparisons, apart ─────
	storeMissing := storeAgainstInventory(t, ctx, databaseURL, inventory)
	projectionMissing := projectionsAgainstInventory(t, ctx, databaseURL, inventory)
	t.Logf("comparison 1, the store against the inventory: %d row(s) the inventory holds and the store lacks", len(storeMissing))
	for _, missing := range storeMissing {
		t.Errorf("comparison 1: the store lacks %s %s (%s)", missing.Kind, missing.Identity, missing.Address)
	}
	t.Logf("comparison 2, the projections against the inventory: %d row(s) the inventory holds and the projections lack", len(projectionMissing))
	for _, missing := range projectionMissing {
		t.Errorf("comparison 2: the projection lacks %s %s (%s)", missing.Kind, missing.Identity, missing.Address)
	}

	// ───── only then the store against its projections ─────
	requireTheCensusWritesNothing(t, ctx, databaseURL, func() {
		report := runStoreCensus(t, databaseURL)
		requireEveryKindCarriesAPopulation(t, report)
	})

	// ───── the old read beside the new read ─────
	compareTheOldReadWithTheNew(t, older, databaseURL, now, seeded)
}

// ───── the database of this run's own ─────────────────────────────────────

// censusDatabase creates an empty database on the test server for this run alone and answers its
// URL. It is empty and not storetest's clone of a migrated template, because the older build has
// to run its own migrations on it: a database already carrying this branch's schema would never
// prove that the migration kept what was there.
func censusDatabase(t *testing.T) string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(storetest.DatabaseURLEnv))
	if raw == "" {
		t.Skipf("%s is unset", storetest.DatabaseURLEnv)
	}
	server, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("read %s: %v", storetest.DatabaseURLEnv, err)
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("name the census database: %v", err)
	}
	name := "dispatch_census_" + hex.EncodeToString(suffix)
	admin := *server
	admin.Path = "/postgres"
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		t.Fatalf("connect to the test server: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create the census database: %v", err)
	}
	t.Cleanup(func() { dropCensusDatabase(t, admin.String(), name) })
	own := *server
	own.Path = "/" + name
	return own.String()
}

// dropCensusDatabase drops the run's database once every process on it has gone. Postgres tears a
// backend down after its client has closed the socket, so a drop can find a connection that is on
// its way out; it is tried again while Postgres answers object_in_use (55006), as storetest's own
// drop is.
func dropCensusDatabase(t *testing.T, adminURL, name string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Logf("drop the census database %s: connect: %v", name, err)
		return
	}
	defer conn.Close(ctx)
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := conn.Exec(ctx, "drop database if exists "+name)
		if err == nil {
			return
		}
		var postgres *pgconn.PgError
		if !errors.As(err, &postgres) || postgres.Code != "55006" || time.Now().After(deadline) {
			t.Logf("drop the census database %s: %v", name, err)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// censusConnect opens one connection to the run's database for a read of the test's own.
func censusConnect(t *testing.T, ctx context.Context, databaseURL string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the census database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// ───── the older build ────────────────────────────────────────────────────

// baseBuild builds `./cmd/dispatch` at the commit this branch forked from and answers the binary.
// The commit is `git merge-base origin/main HEAD` and the source is that commit's own tree, taken
// out of the repository with `git archive`, so the build is the fork point's and not this working
// copy's. Both live under the temporary directory, keyed by the commit, and a run that finds them
// there builds nothing: the extraction and the build are about a minute, and every run of this
// test would otherwise pay it.
func baseBuild(t *testing.T) string {
	t.Helper()
	root := gitOutput(t, ".", "rev-parse", "--show-toplevel")
	commit := gitOutput(t, root, "merge-base", "origin/main", "HEAD")
	if len(commit) < 12 {
		t.Fatalf("git merge-base origin/main HEAD answered %q", commit)
	}
	held := filepath.Join(os.TempDir(), "dispatch-census-base-"+commit[:12])
	binary := filepath.Join(held, "envoy-dispatch")
	if _, err := os.Stat(binary); err == nil {
		t.Logf("the older build is %s, from %s", binary, commit)
		return binary
	}
	building := held + ".building"
	if err := os.RemoveAll(building); err != nil {
		t.Fatalf("clear %s: %v", building, err)
	}
	source := filepath.Join(building, "src")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("make %s: %v", source, err)
	}
	archive := exec.Command("git", "-C", root, "archive", commit)
	extract := exec.Command("tar", "-x", "-C", source)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatalf("read the tree of %s: %v", commit, err)
	}
	// The archive's stdout is the pipe tar reads, so its own failure is read from its stderr
	// alone: asking for both, as CombinedOutput does, is `exec: Stdout already set` and the
	// build never runs. It surfaces only on the first build of a fork point, since a later run
	// finds the binary this one keeps.
	var problems strings.Builder
	archive.Stderr = &problems
	extract.Stdin = pipe
	if err := extract.Start(); err != nil {
		t.Fatalf("extract the tree of %s: %v", commit, err)
	}
	if err := archive.Start(); err != nil {
		t.Fatalf("read the tree of %s: %v", commit, err)
	}
	if err := archive.Wait(); err != nil {
		t.Fatalf("read the tree of %s: %v\n%s", commit, err, problems.String())
	}
	if err := extract.Wait(); err != nil {
		t.Fatalf("extract the tree of %s: %v", commit, err)
	}
	built := cmdtest.BuildCommand(t, cmdtest.Command{
		Name:   "envoy-dispatch-base",
		Dir:    filepath.Join(source, "packages", "envoy"),
		Target: "./cmd/dispatch",
		Env:    os.Environ(),
	})
	moved, err := os.ReadFile(built)
	if err != nil {
		t.Fatalf("read the older build: %v", err)
	}
	if err := os.WriteFile(filepath.Join(building, "envoy-dispatch"), moved, 0o755); err != nil {
		t.Fatalf("keep the older build: %v", err)
	}
	if err := os.Rename(building, held); err != nil {
		// Another run of this test built it first, which is the binary this one takes.
		if _, stat := os.Stat(binary); stat != nil {
			t.Fatalf("keep the older build at %s: %v", held, err)
		}
	}
	t.Logf("the older build is %s, from %s", binary, commit)
	return binary
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// ───── what the older build writes ────────────────────────────────────────

// seededRows is what the older build wrote, as this run addresses it afterwards.
type seededRows struct {
	project, issue, artifact string
	// mentioned is the issue the seeded document's blocks cite, which gives the block
	// references a population of their own.
	mentioned string
	comments  []string
}

// seedThroughTheOlderBuild drives the older build's own API: a project, two issues whose specs
// are documents of several blocks, a block that cites the other issue, and comments by a person
// and by a session. No write here names a role: the older build records the delivery and writes
// no node, so a role addressed here would be a row the backfill cannot answer for.
func seedThroughTheOlderBuild(t *testing.T, server *dispatchProcess) seededRows {
	t.Helper()
	const project = "CENSUS"
	server.call(t, http.MethodPost, "/api/v1/projects",
		`{"key":"`+project+`","name":"Store census"}`, http.StatusCreated)
	mentioned, _ := createCensusIssue(t, server, project, "The issue the blocks cite",
		"# Cited\n\nThe document every block of the other issue points at.\n")
	issue, artifact := createCensusIssue(t, server, project, "The issue the census reads",
		"# Spec\n\nThe first block of this document.\n\n## A nested heading\n\nA block that cites "+
			mentioned+", so the document holds a block reference.\n\n- a list item\n- another item\n")

	seeded := seededRows{project: project, issue: issue, artifact: artifact, mentioned: mentioned}
	seeded.comments = append(seeded.comments,
		server.mention(t, issue, "A comment a person wrote on "+issue+"."),
		server.mention(t, issue, "A second comment, which cites "+mentioned+"."))
	seeded.comments = append(seeded.comments, censusSessionComment(t, server, issue,
		"A comment a session wrote, so the store holds that session."))
	return seeded
}

// createCensusIssue creates one issue whose spec is markdown of several blocks and answers its
// key and its spec document's id. The spec is forced past the near-copy gate: these documents are
// written for their blocks, and what they say is beside the point.
func createCensusIssue(t *testing.T, server *dispatchProcess, project, title, spec string) (string, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"project": project, "title": title, "spec": spec, "force": true,
	})
	if err != nil {
		t.Fatalf("encode the issue: %v", err)
	}
	created := server.call(t, http.MethodPost, "/api/v1/issues", string(body), http.StatusCreated)
	var issue struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" || issue.PrimaryArtifactID == "" {
		t.Fatalf("create the issue %q: %v: %s", title, err, created)
	}
	return issue.Key, issue.PrimaryArtifactID
}

// censusSessionComment writes one comment as a session, on the shared agent bearer, so a session
// is the author a row records and the store holds a node for it.
func censusSessionComment(t *testing.T, server *dispatchProcess, issue, body string) string {
	t.Helper()
	input, err := json.Marshal(map[string]any{
		"body": body, "actor": map[string]string{"kind": "session", "id": censusSession},
	})
	if err != nil {
		t.Fatalf("encode the session's comment: %v", err)
	}
	header := http.Header{"Authorization": {"Bearer " + dispatchTestAgentToken}}
	answer := server.send(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", string(input), header)
	if answer.status != http.StatusCreated {
		t.Fatalf("the session's comment: %s", answer)
	}
	var comment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer.body, &comment); err != nil || comment.ID == "" {
		t.Fatalf("read the session's comment back: %v: %s", err, answer.body)
	}
	return comment.ID
}

// ───── the inventory ──────────────────────────────────────────────────────

// readInventory reads what the older build wrote out of the database, with this test's own SQL
// over that build's own columns: no code of this branch produces it, so a row the migration or
// the backfill loses is still held against what was there. A comment's digest is its body's, a
// block's is the text digest its row already holds, and a block reference's is its far end.
func readInventory(t *testing.T, ctx context.Context, databaseURL string) []inventoryRow {
	t.Helper()
	conn := censusConnect(t, ctx, databaseURL)
	rows, err := conn.Query(ctx, `
		select 'comment' as kind, c.id::text as identity, c.id::text as address,
		       coalesce(c.author->>'kind', '') || ':' ||
		       coalesce(c.author->>'id', c.author->>'owner', '') as author,
		       md5(c.body) as digest, coalesce(c.artifact_id::text, '') as parent
		from comments c
		union all
		select 'block', d.artifact_id::text || '#' || d.block_id, d.node_id, '',
		       d.text_sha256, d.artifact_id::text
		from document_blocks d
		union all
		select 'block_ref',
		       r.artifact_id::text || '#' || r.block_id || ' ' || r.kind || ' -> ' || r.to_kind || ':' || r.to_id,
		       r.from_node, '', md5(r.to_kind || ':' || r.to_id), r.artifact_id::text
		from block_refs r
		order by 1, 2
	`)
	if err != nil {
		t.Fatalf("read the inventory: %v", err)
	}
	defer rows.Close()
	var inventory []inventoryRow
	for rows.Next() {
		var row inventoryRow
		if err := rows.Scan(&row.Kind, &row.Identity, &row.Address, &row.Author, &row.Digest, &row.Parent); err != nil {
			t.Fatalf("scan an inventory row: %v", err)
		}
		inventory = append(inventory, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the inventory: %v", err)
	}
	return inventory
}

// keepInventory writes the inventory to a file of this run's own, under the temporary directory,
// and says where it is: the file is what the comparisons below are read against, and a failing
// run leaves it to be read by a person.
func keepInventory(t *testing.T, inventory []inventoryRow) {
	t.Helper()
	encoded, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		t.Fatalf("encode the inventory: %v", err)
	}
	file := filepath.Join(os.TempDir(), "store-census-inventory.json")
	if err := os.WriteFile(file, append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("keep the inventory: %v", err)
	}
	t.Logf("the inventory of what the older build wrote is %s: %d row(s)", file, len(inventory))
	for _, kind := range []string{"comment", "block", "block_ref"} {
		t.Logf("the inventory holds %d %s row(s)", countInventory(inventory, kind), kind)
	}
}

func countInventory(inventory []inventoryRow, kind string) int {
	count := 0
	for _, row := range inventory {
		if row.Kind == kind {
			count++
		}
	}
	return count
}

// requireInventoryCarriesEveryKind fails the run when the older build wrote no row of a kind this
// run goes on to compare: an inventory that holds nothing proves nothing either.
func requireInventoryCarriesEveryKind(t *testing.T, inventory []inventoryRow) {
	t.Helper()
	for _, kind := range []string{"comment", "block", "block_ref"} {
		if countInventory(inventory, kind) == 0 {
			t.Fatalf("the older build wrote no %s row, so the inventory proves nothing about that kind", kind)
		}
	}
}

// ───── this build's migration and backfill ────────────────────────────────

// runBackfill runs `envoy-dispatch backfill-graph-store` on this build, which is the command a
// deployment runs once the new binary serves.
func runBackfill(t *testing.T, databaseURL string) {
	t.Helper()
	code, out := runDispatchCommand(t, cmdtest.Build(t, "envoy-dispatch"), databaseURL, "backfill-graph-store")
	t.Logf("backfill-graph-store:\n%s", out)
	if code != 0 {
		t.Fatalf("backfill-graph-store exited %d", code)
	}
}

// waitForTheBlockIndex waits for the reconciliation to drain the mark the backfill left: a
// block's parent is the nesting of its rendered version, which no column holds, so the backfill
// marks each document it touched owed a block index and the reconciliation states the part_of
// tuple and writes parent_block_id. The census reads a difference on every backfilled block
// until that mark is drained, so this run waits for it rather than reading it as a difference.
func waitForTheBlockIndex(t *testing.T, ctx context.Context, databaseURL string) {
	t.Helper()
	conn := censusConnect(t, ctx, databaseURL)
	deadline := time.Now().Add(3 * time.Minute)
	ticks := 0
	for {
		var owed int
		if err := conn.QueryRow(ctx,
			`select count(*) from document_block_versions where version is null`).Scan(&owed); err != nil {
			t.Fatalf("read the documents owed a block index: %v", err)
		}
		if owed == 0 {
			t.Logf("the reconciliation drained the owed mark after %d read(s)", ticks)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d document(s) are still owed a block index", owed)
		}
		ticks++
		time.Sleep(500 * time.Millisecond)
	}
}

// addressTheRole routes an issue to a role through this build and writes a comment on it, so the
// publisher resolves a destination that names the role and writes the role's node and its page
// beside the row that addresses it. A role addressed through the older build would be a row with
// no node, which is exactly the difference the census exists to report.
func addressTheRole(t *testing.T, server *dispatchProcess, issue string) {
	t.Helper()
	server.call(t, http.MethodPatch, "/api/v1/issues/"+issue,
		`{"route":"role:`+censusRole+`"}`, http.StatusOK)
	server.mention(t, issue, "A comment on the routed issue, so the role is a destination.")
}

// deleteABlockThroughThisBuild writes a document of its own through this build and then removes
// a whole section of it, so the census is read over a node whose state is gone beside a
// projection row that is really deleted: no gone node holds a row, and every live node is
// projected by one. The document is written here and not by the older build, because the edit
// would otherwise move rows the inventory already holds.
//
// It waits for the block index to settle the edit and then reads the deletion back: a row of
// document_blocks is gone and its node stands, held gone.
func deleteABlockThroughThisBuild(t *testing.T, ctx context.Context, server *dispatchProcess, databaseURL string) {
	t.Helper()
	_, artifact := createCensusIssue(t, server, "CENSUS", "The issue whose block is deleted",
		"# Doomed\n\nThe block that stays.\n\n## Deleted\n\nThe block this run deletes.\n")
	waitForTheBlockIndex(t, ctx, databaseURL)
	conn := censusConnect(t, ctx, databaseURL)
	var before int
	var block string
	if err := conn.QueryRow(ctx, `
		select (select count(*) from document_blocks where artifact_id = $1::uuid),
		       (select block_id from document_blocks
		        where artifact_id = $1::uuid and excerpt like 'The block this run deletes%')
	`, artifact).Scan(&before, &block); err != nil {
		t.Fatalf("read the blocks of the document the run deletes from: %v", err)
	}
	// The edit names the block, which is what deletes it: a quote replaced by nothing empties
	// the block that held it and leaves its row where it was.
	server.call(t, http.MethodPost, "/api/v1/artifacts/"+artifact+"/edits",
		`{"ops":[{"op":"delete","block":"`+block+`"}]}`, http.StatusOK)
	waitForTheBlockIndex(t, ctx, databaseURL)
	deadline := time.Now().Add(time.Minute)
	for {
		var after, gone int
		if err := conn.QueryRow(ctx, `
			select (select count(*) from document_blocks where artifact_id = $1::uuid),
			       (select count(*) from graph_nodes where kind = 'block' and state = 'gone')
		`, artifact).Scan(&after, &gone); err != nil {
			t.Fatalf("read the deletion back: %v", err)
		}
		if after < before && gone > 0 {
			t.Logf("the edit deleted %d block row(s) of %s, and the store holds %d block node(s) gone",
				before-after, artifact, gone)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the edit left %d block rows (it found %d) and %d gone block node(s): the run needs a deleted block",
				after, before, gone)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ───── the two comparisons ────────────────────────────────────────────────

// storedNode is what the store says of one node, as this comparison reads it: who made it and
// the document it is in, each read out of the store alone.
type storedNode struct {
	state, author, parent string
}

// storeAgainstInventory holds the store to the inventory, row by row: every comment and every
// block of the inventory has a node of its own kind, the comment's node names the author the
// row recorded, and the node's own tuple reaches the document the row belongs to. It answers the
// rows the inventory holds and the store lacks, and reports a node that holds a different author
// or a different document as a difference of its own.
//
// The parent is read the way a current read reads it: the newest in_document tuple of a block
// and the newest owned_by of a comment, each kept while it asserts, resolved to the artifact the
// address names.
func storeAgainstInventory(t *testing.T, ctx context.Context, databaseURL string, inventory []inventoryRow) []inventoryRow {
	t.Helper()
	conn := censusConnect(t, ctx, databaseURL)
	rows, err := conn.Query(ctx, `
		select n.kind, n.id, n.state,
		       coalesce(n.made_by->>'kind', '') || ':' ||
		       coalesce(n.made_by->>'id', n.made_by->>'owner', '') as author,
		       coalesce((
		           select a.id::text from artifacts a where a.ref_key = (
		               select held.to_id from graph_tuples held
		               where held.from_kind = n.kind and held.from_id = n.id
		                 and held.kind in ('in_document', 'owned_by') and held.to_kind = 'artifact'
		               order by held.seq desc limit 1
		           )
		       ), '') as parent
		from graph_nodes n where n.kind in ('comment', 'block')
	`)
	if err != nil {
		t.Fatalf("read the nodes of the store: %v", err)
	}
	defer rows.Close()
	held := map[string]storedNode{}
	for rows.Next() {
		var kind, id string
		var node storedNode
		if err := rows.Scan(&kind, &id, &node.state, &node.author, &node.parent); err != nil {
			t.Fatalf("scan a node: %v", err)
		}
		held[kind+"\x00"+id] = node
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the nodes of the store: %v", err)
	}
	var missing []inventoryRow
	for _, row := range inventory {
		nodeKind := "comment"
		if row.Kind != "comment" {
			nodeKind = "block"
		}
		node, found := held[nodeKind+"\x00"+row.Address]
		if !found {
			missing = append(missing, row)
			continue
		}
		// A block's node is made by the version's writer, which no row of the inventory holds,
		// so the author is held only where the inventory recorded one.
		if row.Author != "" && node.author != row.Author {
			t.Errorf("comparison 1: the node of %s %s was made by %s, want the author the row recorded, %s",
				row.Kind, row.Identity, node.author, row.Author)
		}
		// A block reference's row names its block, whose document is the one the reference's own
		// row belongs to, so both kinds are held to the same parent.
		if node.parent != row.Parent {
			t.Errorf("comparison 1: the node of %s %s is in document %q, want the row's own %q",
				row.Kind, row.Identity, node.parent, row.Parent)
		}
	}
	return missing
}

// projectionsAgainstInventory holds each projection to the inventory the same way: every row the
// older build wrote is still in its own table, with the same identity, the same author, the same
// text digest and the same parent. A row the migration or the backfill dropped from both sides
// shows here and never in the census. Every row of the inventory is expected back: what this
// build deletes later, it deletes from a document of its own, which the inventory never held.
func projectionsAgainstInventory(t *testing.T, ctx context.Context, databaseURL string, inventory []inventoryRow) []inventoryRow {
	t.Helper()
	held := map[string]inventoryRow{}
	for _, row := range readInventory(t, ctx, databaseURL) {
		held[row.Kind+"\x00"+row.Identity] = row
	}
	var missing []inventoryRow
	for _, row := range inventory {
		reads, found := held[row.Kind+"\x00"+row.Identity]
		switch {
		case !found:
			missing = append(missing, row)
		case reads != row:
			t.Errorf("comparison 2: %s %s reads back as %+v, want the inventory's %+v",
				row.Kind, row.Identity, reads, row)
		}
	}
	return missing
}

// ───── the census ─────────────────────────────────────────────────────────

// kindCensus is one kind's line of the census report.
type kindCensus struct {
	kind             string
	nodes, rows      int64
	rowsWithoutANode int64
	nodesWithoutARow int64
}

// runStoreCensus runs `envoy-dispatch census --store` on this build and answers what it printed,
// one entry per kind. A census that refuses fails the run, with its whole report.
func runStoreCensus(t *testing.T, databaseURL string) []kindCensus {
	t.Helper()
	code, out := runDispatchCommand(t, cmdtest.Build(t, "envoy-dispatch"), databaseURL, "census", "--store")
	t.Logf("census --store:\n%s", out)
	if code != 0 {
		t.Fatalf("census --store exited %d, want 0: the store and its projections disagree", code)
	}
	var read []kindCensus
	for _, line := range strings.Split(out, "\n") {
		fields := censusFields(line)
		if fields == nil || fields["kind"] == "" || fields["nodes"] == "" {
			continue
		}
		read = append(read, kindCensus{
			kind:             fields["kind"],
			nodes:            censusCount(t, fields["nodes"]),
			rows:             censusCount(t, fields["rows"]),
			rowsWithoutANode: censusCount(t, fields["rows_without_a_node"]),
			nodesWithoutARow: censusCount(t, fields["nodes_without_a_row"]),
		})
	}
	if len(read) == 0 {
		t.Fatalf("the census printed no kind line")
	}
	return read
}

// censusFields reads one `census: store ` line as the name=value fields it is, and answers nil
// for a line that is not one.
func censusFields(line string) map[string]string {
	const prefix = "census: store "
	if !strings.HasPrefix(line, prefix) {
		return nil
	}
	fields := map[string]string{}
	for _, field := range strings.Fields(strings.TrimPrefix(line, prefix)) {
		name, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		fields[name] = value
	}
	return fields
}

// censusCount reads a count the report printed with thousands separators.
func censusCount(t *testing.T, text string) int64 {
	t.Helper()
	count, err := strconv.ParseInt(strings.ReplaceAll(text, ",", ""), 10, 64)
	if err != nil {
		t.Fatalf("read the count %q out of the census: %v", text, err)
	}
	return count
}

// requireEveryKindCarriesAPopulation holds the census to a population for every kind this child
// moves, and to zero differences in both directions. A kind that checked zero rows fails the run
// and is reported by name: two empty sides agree, and a census that read nothing would pass.
func requireEveryKindCarriesAPopulation(t *testing.T, report []kindCensus) {
	t.Helper()
	checked := map[string]kindCensus{}
	for _, kind := range report {
		checked[kind.kind] = kind
		t.Logf("census kind=%s nodes=%d rows=%d rows_without_a_node=%d nodes_without_a_row=%d",
			kind.kind, kind.nodes, kind.rows, kind.rowsWithoutANode, kind.nodesWithoutARow)
	}
	for _, want := range censusKinds {
		kind, found := checked[want]
		switch {
		case !found:
			t.Errorf("the census checked no kind %s at all", want)
		case kind.rows == 0:
			t.Errorf("the census checked zero rows of kind %s, so this run proves nothing about it", want)
		case kind.nodes == 0:
			t.Errorf("the census read zero nodes of kind %s", want)
		}
		if kind.rowsWithoutANode != 0 || kind.nodesWithoutARow != 0 {
			t.Errorf("kind %s reads %d row(s) without a node and %d node(s) without a row",
				want, kind.rowsWithoutANode, kind.nodesWithoutARow)
		}
	}
	for _, kind := range report {
		if !slices.Contains(censusKinds, kind.kind) {
			t.Errorf("the census checked kind %s, which this test's list does not hold", kind.kind)
		}
	}
}

// requireTheCensusWritesNothing runs the census and proves it wrote nothing: the log's highest
// sequence and both tables' row counts are the same afterwards.
func requireTheCensusWritesNothing(t *testing.T, ctx context.Context, databaseURL string, census func()) {
	t.Helper()
	conn := censusConnect(t, ctx, databaseURL)
	read := func() [3]int64 {
		var counted [3]int64
		if err := conn.QueryRow(ctx, `
			select coalesce(max(seq), 0), (select count(*) from graph_tuples), (select count(*) from graph_nodes)
			from graph_tuples
		`).Scan(&counted[0], &counted[1], &counted[2]); err != nil {
			t.Fatalf("read the store's marks: %v", err)
		}
		return counted
	}
	before := read()
	census()
	if after := read(); after != before {
		t.Errorf("the census moved the store: highest sequence, tuples and nodes were %v and are %v", before, after)
	}
}

// ───── the old read beside the new read ───────────────────────────────────

// compareTheOldReadWithTheNew runs the older build's answer beside this build's, over the same
// database, for each read this child touched, and compares the two field for field. Store-to-
// projection equality says nothing about this: both sides can agree and the answer a reader gets
// still change.
//
// The older build is started again over the migrated database. Its migration runner applies the
// versions schema_migrations does not record, and this branch's are all recorded by now, so it
// runs none and reads the schema it finds.
func compareTheOldReadWithTheNew(t *testing.T, older, databaseURL string, now *dispatchProcess, seeded seededRows) {
	t.Helper()
	before := startDispatchProcessWith(t, databaseURL, dispatchOptions{
		Binary: older, SigningKey: censusSigningKey,
	})
	before.waitHealthy(t)
	// The reads this child touched, each answered by both builds over the one database: the
	// issue and its comments, the backlinks a Referenced by panel shows, the document and its
	// blocks, the lists a project page reads, the asks and the inbox. A read of an artifact
	// that belongs to an issue is refused by its own route (ARTIFACT_LINKED), so the issue's
	// own comment read is the one here.
	reads := []string{
		"/api/v1/issues/" + seeded.issue,
		"/api/v1/issues/" + seeded.issue + "/comments",
		"/api/v1/issues/" + seeded.issue + "/references",
		"/api/v1/issues/" + seeded.issue + "/asks",
		"/api/v1/issues?project=" + seeded.project,
		"/api/v1/artifacts/" + seeded.artifact,
		"/api/v1/artifacts/" + seeded.artifact + "/text",
		"/api/v1/artifacts/" + seeded.artifact + "/blocks",
		"/api/v1/artifacts/" + seeded.artifact + "/references",
		"/api/v1/projects/" + seeded.project + "/artifacts",
		"/api/v1/asks/open?project=" + seeded.project,
		"/api/v1/inbox",
	}
	for _, read := range reads {
		was := decodeRead(t, before.call(t, http.MethodGet, read, "", http.StatusOK))
		is := decodeRead(t, now.call(t, http.MethodGet, read, "", http.StatusOK))
		if !reflect.DeepEqual(was, is) {
			t.Errorf("GET %s answers differently on the two builds:\nthe older build %s\nthis build     %s",
				read, mustEncode(t, was), mustEncode(t, is))
			continue
		}
		t.Logf("GET %s answers field for field on both builds", read)
	}
}

func decodeRead(t *testing.T, body []byte) any {
	t.Helper()
	var answer any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode a read's answer: %v: %s", err, body)
	}
	return withoutTheMomentOfTheRead(answer)
}

// withoutTheMomentOfTheRead drops `as_of` wherever an answer carries it. It is the clock of the
// read and no answer about the data: two reads of one database a millisecond apart carry
// different ones, and nothing of this child moves it. Every other field is compared.
func withoutTheMomentOfTheRead(answer any) any {
	switch held := answer.(type) {
	case map[string]any:
		delete(held, "as_of")
		for name, value := range held {
			held[name] = withoutTheMomentOfTheRead(value)
		}
		return held
	case []any:
		for index, value := range held {
			held[index] = withoutTheMomentOfTheRead(value)
		}
		return held
	default:
		return answer
	}
}

func mustEncode(t *testing.T, answer any) string {
	t.Helper()
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode a read's answer: %v", err)
	}
	return string(encoded)
}

// ───── running a subcommand of a build ────────────────────────────────────

// runDispatchCommand runs one subcommand of a build against databaseURL, in a home of its own and
// with nothing of the caller's environment, and answers its exit code and its whole output.
func runDispatchCommand(t *testing.T, binary, databaseURL string, args ...string) (int, string) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(binary, args...)
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"XDG_DATA_HOME=" + home,
		"DATABASE_URL=" + databaseURL,
		"DISPATCH_NATS_DISABLED=1",
	}
	sort.Strings(cmd.Env)
	out, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s %s: %v\n%s", filepath.Base(binary), strings.Join(args, " "), err, out)
	}
	return code, strings.TrimRight(string(out), "\n")
}
